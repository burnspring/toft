package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
)

// The access log: one structured line per request, naming who made it.
//
// This is the third of the three pieces ADR §46 describes, and
// the one that answers "who made *this* call" for a request that changed
// nothing — a 500, a 429, a 404. `audit_log` answers it for mutations and
// `session.*` for sign-ins; before this, everything else named nobody.
//
// **Why it can be outermost and still know the caller.** chi's
// `middleware.Logger` cannot: it is mounted above the authentication
// middlewares, and a principal put into the *inner* request's context is
// invisible to the outer one — `r.WithContext` returns a copy. The way out is
// the pattern `internal/telemetry` already uses twice, for the gateway's app id
// and for `enduser.id`: the outer middleware allocates a mutable slot and puts
// a *pointer* to it in the context, and whatever learns the answer later fills
// it in. So the duration measured still covers the rate limiter and the origin
// check, and the line still names the person.

type accessInfoKey struct{}

// accessInfo is that slot. Every field is filled from somewhere further in:
// the principal and the credential from requestMetaMiddleware on the platform
// router, and from appProxyGuard on the gateway, which is the only place a
// viewer of a deployed app is resolved.
type accessInfo struct {
	principalID string
	apiKeyID    string
	keyKind     string
	appID       string
	requestID   string
	clientIP    string
}

func accessInfoFrom(ctx context.Context) *accessInfo {
	v, _ := ctx.Value(accessInfoKey{}).(*accessInfo)
	return v
}

// setAccessPrincipal records who this request turned out to be from. Safe to
// call with a nil principal and on a route with no access log installed.
func setAccessPrincipal(ctx context.Context, p *auth.Principal) {
	if p == nil {
		return
	}
	if ai := accessInfoFrom(ctx); ai != nil {
		ai.principalID = p.ID
		if p.IsAPIKey() {
			ai.apiKeyID = p.APIKeyID
			ai.keyKind = "user"
		}
	}
}

// setAccessKey records which API key a gateway request was spent on, beside
// the person it resolved to.
func setAccessKey(ctx context.Context, keyID, kind string) {
	if ai := accessInfoFrom(ctx); ai != nil {
		ai.apiKeyID, ai.keyKind = keyID, kind
	}
}

// setAccessApp records which app served this request.
func setAccessApp(ctx context.Context, appID string) {
	if ai := accessInfoFrom(ctx); ai != nil {
		ai.appID = appID
	}
}

// setAccessRequestID carries the id through the slot rather than the context.
//
// requestIDMiddleware runs *inside* this one and puts its id on a context this
// middleware never sees — the same copy-on-WithContext problem that stops chi's
// Logger naming a principal, and it caught this code too: the first version
// read `requestIDFrom(ctx)` at the end and silently logged no id at all.
func setAccessRequestID(ctx context.Context, rid string) {
	if ai := accessInfoFrom(ctx); ai != nil {
		ai.requestID = rid
	}
}

// setAccessClientIP carries the forwarded client address through the slot, for
// the reason setAccessRequestID does: realIPMiddleware rewrites RemoteAddr on a
// request copy this middleware never sees, so reading the outer request logged
// the proxy's address on every line behind one.
func setAccessClientIP(ctx context.Context, ip string) {
	if ai := accessInfoFrom(ctx); ai != nil {
		ai.clientIP = ip
	}
}

// accessLog replaces chi's middleware.Logger on both routers.
//
// `surface` is the same vocabulary telemetry uses — `platform` or `app` — so a
// line and a span describe the request the same way. It is the listener's
// surface rather than the served one: unlike a metric, a log line carries the
// app id explicitly, so nothing is lost by not re-deriving it.
func accessLog(surface string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ai := &accessInfo{}
			ctx := context.WithValue(r.Context(), accessInfoKey{}, ai)
			rec := &accessRecorder{ResponseWriter: w}

			// The line is written in a deferred closure, because a handler can
			// panic: ReverseProxy ends every response the client abandons —
			// an SSE subscription, a download — with panic(http.ErrAbortHandler),
			// which passes this middleware on its way to net/http. ADR §46
			// promises one line per request, so the line is written either
			// way, marked `aborted` when the handler never returned.
			//
			// No recover(): ErrAbortHandler must reach net/http so the
			// connection is torn down, and any other panic must reach
			// Recoverer so the client gets its 500 and the stack is printed.
			finished := false
			defer func() {
				status := rec.statusCode()
				if !finished && rec.status == 0 && !rec.hijacked {
					// Nothing was written; Recoverer is about to write a 500.
					status = http.StatusInternalServerError
				}

				// Read after the handler: chi cannot say which pattern matched
				// until routing has happened, and the raw path is not a thing to
				// put in a field that will be aggregated on. Same rule as the span
				// name (ADR §31 D5).
				route := "__unmatched__"
				if rc := chi.RouteContext(ctx); rc != nil && rc.RoutePattern() != "" {
					route = rc.RoutePattern()
				}

				ip := ai.clientIP
				if ip == "" {
					ip = clientIP(r)
				}

				who := ai.principalID
				if who == "" {
					who = "anonymous"
				}

				attrs := []any{
					"kind", "access",
					"surface", surface,
					"method", r.Method,
					"route", route,
					"status", status,
					"bytes", rec.written,
					"duration_ms", time.Since(start).Milliseconds(),
					"ip", ip,
					"user", who,
					// The field this whole thread started from: it is what tells
					// `toft/1.2.3` from a browser from an SDK's bare `node`.
					"user_agent", r.UserAgent(),
				}
				// Present only when there is something to say. A line whose every
				// request carries `api_key_id=""` is a line nobody can grep.
				if ai.apiKeyID != "" {
					attrs = append(attrs, "api_key_id", ai.apiKeyID, "credential", ai.keyKind)
				}
				if ai.appID != "" {
					attrs = append(attrs, "app_id", ai.appID)
				}
				if ai.requestID != "" {
					attrs = append(attrs, "request_id", ai.requestID)
				}

				if !finished {
					attrs = append(attrs, "aborted", true)
				}

				// The context carries the span, so `trace_id` and `span_id` are
				// added by the handler in internal/obs. That is what makes this
				// line and the exported span the same request.
				level := levelFor(status)
				if isHealthProbe(r, ai, status) {
					level = slog.LevelDebug
				}
				slog.LogAttrs(ctx, level, "http request", attrsOf(attrs)...)
			}()
			next.ServeHTTP(rec, r.WithContext(ctx))
			finished = true
		})
	}
}

// levelFor keeps an ordinary request out of the way at LOG_LEVEL=warn while
// still surfacing the failures. A 4xx is the caller's problem and stays Info: a
// scanner walking the estate must not be able to fill an operator's warning
// stream.
func levelFor(status int) slog.Level {
	if status >= 500 {
		return slog.LevelError
	}
	return slog.LevelInfo
}

// A load balancer's or an orchestrator's liveness probe, answered without
// fault. These are most of what an idle install logs — an ALB probes from every
// zone it has a node in, against both target groups — and each says only "still
// up", so they go to Debug: LOG_LEVEL=debug brings them back, and a probe that
// got a 5xx stays an Error like any other.
//
// The user agent alone would let anyone hide a request from the access log by
// claiming to be a health checker, so the line is demoted only when everything
// else agrees too: anonymous, a GET, and one of the two paths the probes use —
// /api/v1/config on the platform, the root on the gateway. Neither reads
// anything an anonymous caller could not already read.
var healthProbeAgents = []string{"ELB-HealthChecker/", "kube-probe/", "toft-healthcheck"}

func isHealthProbe(r *http.Request, ai *accessInfo, status int) bool {
	if status >= 500 || r.Method != http.MethodGet || ai.principalID != "" || ai.apiKeyID != "" {
		return false
	}
	if p := r.URL.Path; p != "/api/v1/config" && p != "/" {
		return false
	}
	ua := r.UserAgent()
	for _, a := range healthProbeAgents {
		if strings.HasPrefix(ua, a) {
			return true
		}
	}
	return false
}

// attrsOf converts the alternating key/value form to typed attrs. LogAttrs is
// the allocation-free entry point and it wants slog.Attr; building the slice by
// hand at every call site is what this avoids.
func attrsOf(kv []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, slog.Any(kv[i].(string), kv[i+1]))
	}
	return out
}

// accessRecorder captures the status and the byte count.
//
// Written here rather than taken from chi's middleware package for the reason
// `internal/telemetry` has its own: chi's wrapper reports 0 on a hijacked
// connection, and a WebSocket upgrade that logs as 0 — or worse, as 200 — is a
// lie about the one request most likely to be under investigation. The two
// copies exist because telemetry's is unexported and that package must not
// import this one.
type accessRecorder struct {
	http.ResponseWriter
	status   int
	written  int64
	hijacked bool
}

// WriteHeader latches only a final status. ReverseProxy relays an
// upstream's interim 100 Continue and 103 Early Hints through here before the
// real one, and the request's status is the last answer, not the first. Every
// code still reaches the underlying writer.
func (w *accessRecorder) WriteHeader(code int) {
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

func (w *accessRecorder) statusCode() int {
	switch {
	case w.hijacked:
		return http.StatusSwitchingProtocols
	case w.status == 0:
		return http.StatusOK // handler returned without writing; net/http wrote a 200
	default:
		return w.status
	}
}

// Flush, Hijack and ReadFrom are forwarded rather than merely inherited:
// embedding http.ResponseWriter hides them, and a wrapper without Flusher
// breaks SSE and the log stream while one without Hijacker breaks every
// WebSocket on the install.

func (w *accessRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		f.Flush()
	}
}

func (w *accessRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

func (w *accessRecorder) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	var (
		n   int64
		err error
	)
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(w.ResponseWriter, src)
	}
	w.written += n
	return n, err
}

func (w *accessRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
