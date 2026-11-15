package telemetry

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
)

// Middleware is the server-side instrumentation for one surface.
//
// It is written here rather than taken from
// contrib/instrumentation/net/http/otelhttp, and the reason is not that fifty
// lines are cheap. otelhttp's value *is* its opinions about attributes, and
// this package has already decided those for itself — a route template rather
// than a path, a bounded app dimension, a closed error enum, a surface. Taking
// the library would mean taking its opinions and then fighting them with views.
//
// `trusted` answers "may this request's inbound trace context be believed". It
// decides exactly one thing, whether an inbound traceparent is honoured, and
// the router answers it with TRUST_INBOUND_TRACE_CONTEXT *and* the same
// TRUSTED_PROXY_CIDRS peer test the X-Forwarded-* handling asks. See the
// comment on extract below for why that is a security decision and not a
// compatibility one, and why the peer test alone is not enough.
func Middleware(surface string, trusted func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Loaded once. The increment below goes to this pipeline, so the
			// decrement must too: a settings save mid-request swaps in a
			// pipeline that never saw the increment.
			p := live.Load()
			if p == nil {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			method := normalizeMethod(r.Method)
			scheme := requestScheme(r)

			ctx := extract(r, trusted)
			// The app id is not known yet — the gateway resolves a slug inside
			// its handler — so the middleware carries a slot the handler fills.
			st := &requestState{}
			ctx = context.WithValue(ctx, requestStateKey{}, st)

			// Named provisionally. chi cannot say which pattern matched until
			// the handler has run, and a span named after a raw path is the
			// cardinality bug D5 exists to prevent — so the real name is set at
			// the end, from the pattern.
			ctx, span := StartServer(ctx, method,
				attribute.String("http.request.method", method),
				attribute.String("url.scheme", scheme),
			)
			r = r.WithContext(ctx)

			AddHTTPActive(ctx, surface, method, 1)
			ww := &responseRecorder{ResponseWriter: w}

			// Everything after the handler is deferred, because a handler can
			// panic: ReverseProxy ends every response the client abandons with
			// panic(http.ErrAbortHandler), and any other panic reaches chi's
			// Recoverer, outside this middleware. Without the defer the gauge
			// keeps its +1 for good and the span never ends.
			//
			// No recover() here. ErrAbortHandler must reach net/http so the
			// connection is torn down, and any other panic must reach
			// Recoverer so the client gets its 500 and the stack is printed.
			// `finished` says whether the handler returned.
			finished := false
			defer func() {
				if live.Load() == p {
					AddHTTPActive(ctx, surface, method, -1)
				}

				route := routePattern(r)
				status := ww.statusCode()
				if !finished && ww.status == 0 && !ww.hijacked {
					// Nothing was written; Recoverer is about to write a 500.
					status = http.StatusInternalServerError
				}
				elapsed := time.Since(start)
				// The surface describes what was *served*, not which listener
				// took the call. On a single-origin install (ADR §15) the apps
				// gateway is mounted on the platform router, and an app's
				// traffic recorded as `platform` would put somebody's Flask
				// app inside the platform's own error rate — which is the one
				// number an operator pages on.
				//
				// The in-flight gauge above keeps the listener's surface,
				// because that is genuinely what it counts, and because a
				// gauge whose increment and decrement disagreed about their
				// attributes would drift upwards forever.
				served := surface
				if st.appID != "" {
					served = SurfaceApp
				}

				span.SetName(method + " " + route)
				span.Set(
					attribute.String("http.route", route),
					attribute.Int("http.response.status_code", status),
					attribute.String("toft.surface", served),
				)
				if st.appID != "" {
					span.Set(attribute.String("toft.app.id", st.appID))
				}
				// Set unconditionally; the redaction stage drops it unless the
				// operator asked for it. It reaches no metric — RecordHTTPServer
				// below is never given it.
				if st.enduserID != "" {
					span.Set(enduserIDKey.String(st.enduserID))
				}
				// The raw path, on a span only, and only when nothing matched —
				// which is exactly the case where "what was it asking for" is
				// the whole question. The metric gets __unmatched__ regardless.
				// Cut to maxSpanPath: a request line may be a megabyte and a
				// sampled span sits in a 2048-slot queue. The redaction stage
				// repairs whatever the cut or the client left invalid as UTF-8.
				if route == UnmatchedRoute && served == SurfacePlatform {
					path := r.URL.Path
					if len(path) > maxSpanPath {
						path = path[:maxSpanPath]
					}
					span.Set(attribute.String("url.path", path))
				}
				if et := StatusErrorType(status); et != ErrNone {
					span.Set(attribute.String("error.type", et))
				}
				// Ended *after* the response is written, never before. This is
				// the D2 property, and it holds by construction here: the
				// handler has already served the response.
				span.EndOK("")

				RecordHTTPServer(ctx, served, method, route, scheme, status, st.appID, elapsed, r.ContentLength)
			}()
			next.ServeHTTP(ww, r)
			finished = true
		})
	}
}

// maxSpanPath bounds the raw path an unmatched request puts on its span.
const maxSpanPath = 512

// extract decides whether an inbound trace context is believed.
//
// Parent-based sampling means a caller that sets traceparent with the sampled
// bit decides that Toft records the trace. That is correct behaviour
// inside a trusted mesh and a denial-of-service lever outside one: this
// platform's edge is public, and its apps gateway receives requests from
// deployed apps' browsers. A stranger who can set a header should not be able
// to choose what the operator's collector is billed for.
//
// The peer test is necessary and not sufficient. X-Forwarded-For can be
// believed from a peer inside TRUSTED_PROXY_CIDRS because the proxy appends it
// itself; traceparent is an ordinary end-to-end header that nginx, Caddy,
// Traefik, HAProxy, ALB and Cloudflare relay verbatim, so "it came through our
// proxy" says nothing about who wrote it. Re-deciding the sample locally would
// not help either: the ratio sampler decides from the trace id, and whoever
// picks the id picks the decision. So the caller passes a `trusted` that is
// also gated on TRUST_INBOUND_TRACE_CONTEXT, off by default — the operator who
// turns it on asserts that their edge strips or re-issues the header. An
// ignored traceparent is not an error: the request simply starts a fresh
// root, subject to ratio sampling.
func extract(r *http.Request, trusted func(*http.Request) bool) context.Context {
	if trusted == nil || !trusted(r) {
		return r.Context()
	}
	return propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
}

// InjectInto writes the current trace context into outbound headers. Used by
// the proxy when PROPAGATE_TRACE_TO_APPS is on, and by nothing else.
func InjectInto(ctx context.Context, h http.Header) {
	if !TracingEnabled() {
		return
	}
	propagator.Inject(ctx, propagation.HeaderCarrier(h))
}

// routePattern reads chi's matched pattern, which is only available after
// routing. Never r.URL.Path: one scanner walking /wp-admin/… for an hour is
// tens of thousands of series that never recur, and that is the standard way a
// metrics rollout takes down the monitoring system it was supposed to protect.
func routePattern(r *http.Request) string {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return UnmatchedRoute
	}
	if p := rctx.RoutePattern(); p != "" {
		return p
	}
	return UnmatchedRoute
}

// normalizeMethod folds anything that is not a known method into _OTHER, which
// is what semconv specifies and what stops a client inventing methods from
// inventing series.
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return m
	}
	return "_OTHER"
}

// requestScheme reports http or https.
//
// It reads TLS only. Not the URL: an absolute-form request line
// (`GET x1://h/ HTTP/1.1`) puts any scheme the client likes there, up to the
// header limit, and net/http serves it — so a scheme read from the request
// line is client text minting series on a metric declared at two values.
//
// Not X-Forwarded-Proto either, deliberately: this is a metric attribute, the
// forwarded value is only believed from a trusted proxy anyway, and by the time
// this runs realIPMiddleware has already applied that rule to everything that
// depends on it. Getting the scheme slightly wrong on a dashboard is a smaller
// problem than a second place that decides what to believe about a header.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// --- carrying the app id out of a handler -----------------------------------

type requestStateKey struct{}

// requestState is the slot a handler fills once it knows something the
// middleware could not. The app id is a slug the gateway resolves; the end user
// is a principal that does not exist until the authentication middlewares have
// run, both of which happen inside this one.
type requestState struct {
	appID     string
	enduserID string
}

// SetRequestApp records which app this request was served by, for the span and
// the metric the middleware writes when the handler returns.
//
// The value goes through the capped registry before it is stored, so a call
// site cannot put a raw app id on a metric even by accident.
func SetRequestApp(ctx context.Context, appID string) {
	if appID == "" || live.Load() == nil {
		return
	}
	if st, ok := ctx.Value(requestStateKey{}).(*requestState); ok {
		st.appID = AppAttr(appID)
	}
}

// SetRequestEnduser records which person this request was served for.
//
// The span exists before authentication does — this middleware is outermost, so
// that the duration it measures includes the rate limiter and the origin check
// — which means the middleware itself can never know the principal. This is the
// slot for it, filled from `requestMetaMiddleware` in internal/api, the one
// place that already runs after every authentication middleware.
//
// **A span attribute and never a metric one.** That is D5's rule and it has no
// override: a person is not a dimension, and `TestEnduserIDIsOffByDefault`
// walks the metric inventory to prove nobody has added one.
//
// This deliberately does not consult OTEL_RECORD_ENDUSER_ID. Whether the value
// reaches the wire is the redaction stage's decision (D7) — the point of
// redaction being a stage rather than a convention is that a producer cannot
// get the policy wrong, and a second copy of the check here is a second thing
// to keep in step.
//
// Not called on the apps gateway. "Who reached which app" is answered by a
// rollup and a metric on purpose, and putting a name on every proxied request
// is a different decision from this one — it wants making deliberately, not as
// a side effect of the platform API getting attribution.
func SetRequestEnduser(ctx context.Context, userID string) {
	if userID == "" || live.Load() == nil {
		return
	}
	if st, ok := ctx.Value(requestStateKey{}).(*requestState); ok {
		st.enduserID = userID
	}
}

// --- the response recorder --------------------------------------------------

// responseRecorder captures the status code.
//
// Written here rather than taken from chi's middleware package because this one
// has to be honest about a hijacked connection. A WebSocket upgrade writes its
// 101 straight to the hijacked socket, so a wrapper that only sees WriteHeader
// reports 0 — and a wrapper that then reports 0 as 200 puts a lie in the
// metric. Here a hijacked connection is recorded as 101, which is what actually
// happened.
type responseRecorder struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

// WriteHeader latches only a final status. ReverseProxy relays an
// upstream's interim 100 Continue and 103 Early Hints through here before the
// real one, and the request's status is the last answer, not the first. Every
// code still reaches the underlying writer.
func (w *responseRecorder) WriteHeader(code int) {
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *responseRecorder) statusCode() int {
	switch {
	case w.hijacked:
		return http.StatusSwitchingProtocols
	case w.status == 0:
		// The handler returned without writing anything, so net/http wrote a
		// 200 on its way out.
		return http.StatusOK
	default:
		return w.status
	}
}

// Flush, Hijack and ReadFrom are forwarded rather than merely inherited,
// because embedding http.ResponseWriter hides them: a wrapper that does not
// implement Flusher breaks SSE and log streaming, and one that does not
// implement Hijacker breaks every WebSocket on the install. Both have happened
// to somebody; neither is caught by a test that only makes ordinary requests.

func (w *responseRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		f.Flush()
	}
}

func (w *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
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

func (w *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		return rf.ReadFrom(src)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return io.Copy(w.ResponseWriter, src)
}

// Unwrap lets anything that needs the real writer find it, which is what
// net/http's own ResponseController uses.
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
