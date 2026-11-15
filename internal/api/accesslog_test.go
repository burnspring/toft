package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
)

// capture swaps the default logger for one writing JSON into a buffer, and
// returns the single record the request produced.
func capture(t *testing.T, h http.Handler, req *http.Request) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h.ServeHTTP(httptest.NewRecorder(), req)

	line := bytes.TrimSpace(buf.Bytes())
	if len(line) == 0 {
		t.Fatal("the request produced no log line")
	}
	var rec map[string]any
	if err := json.Unmarshal(line, &rec); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, line)
	}
	return rec
}

// The whole point: a request that changed nothing still names its caller. Note
// the principal is set by an *inner* middleware — which is the case chi's
// Logger cannot serve, because it is mounted above authentication and never
// sees the context the authentication middlewares built.
func TestAccessLogNamesAPrincipalSetFurtherIn(t *testing.T) {
	r := chi.NewRouter()
	r.Use(accessLog("platform"))
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			setAccessPrincipal(r.Context(), &auth.Principal{ID: "user-7", APIKeyID: "key-3"})
			next.ServeHTTP(w, r)
		})
	})
	r.Get("/api/v1/apps/{appID}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	rec := capture(t, r, httptest.NewRequest("GET", "/api/v1/apps/abc-123", nil))

	if rec["user"] != "user-7" {
		t.Errorf("user = %v, want user-7", rec["user"])
	}
	if rec["api_key_id"] != "key-3" || rec["credential"] != "user" {
		t.Errorf("credential = %v / %v", rec["api_key_id"], rec["credential"])
	}
	if rec["status"] != float64(404) {
		t.Errorf("status = %v, want 404", rec["status"])
	}
	// The pattern, never the path — one scanner walking an estate must not turn
	// into a field with a million values (ADR §31 D5).
	if rec["route"] != "/api/v1/apps/{appID}" {
		t.Errorf("route = %v, want the pattern", rec["route"])
	}
	if b, _ := json.Marshal(rec); bytes.Contains(b, []byte("abc-123")) {
		t.Errorf("the raw path parameter reached the line: %s", b)
	}
	if rec["kind"] != "access" {
		t.Errorf("kind = %v — one stream is told apart by this field", rec["kind"])
	}
}

// The id on the line is the id the caller was handed and the id on the audit
// row. It has to travel through the slot: requestIDMiddleware runs inside the
// access log and builds a context the access log never sees, which is the same
// trap that stops chi's Logger naming a principal — and the first version of
// this file fell into it and logged no id at all.
func TestAccessLogCarriesTheRequestID(t *testing.T) {
	r := chi.NewRouter()
	r.Use(accessLog("platform"))
	r.Use(requestIDMiddleware)
	r.Get("/x", func(w http.ResponseWriter, r *http.Request) {})

	rec := capture(t, r, httptest.NewRequest("GET", "/x", nil))

	rid, _ := rec["request_id"].(string)
	if rid == "" {
		t.Fatal("no request_id on the access line")
	}
	if len(rid) < 32 {
		t.Errorf("request_id = %q, want the minted UUID", rid)
	}
}

// Behind a trusted proxy the line names the client, not the proxy. realIP
// rewrites RemoteAddr on a request copy the access log never sees, so this too
// has to travel through the slot — before it did, every install behind Caddy or
// an ingress logged `ip=127.0.0.1` on every line.
func TestAccessLogNamesTheForwardedClient(t *testing.T) {
	r := chi.NewRouter()
	r.Use(accessLog("platform"))
	r.Use(realIPMiddleware(trustedCIDRs(t, "192.0.2.0/24")))
	r.Get("/x", func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "192.0.2.10:44123"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if rec := capture(t, r, req); rec["ip"] != "203.0.113.7" {
		t.Errorf("ip = %v, want the forwarded client 203.0.113.7", rec["ip"])
	}

	// An untrusted peer's header is not believed, and the peer is logged.
	req = httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "198.51.100.9:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if rec := capture(t, r, req); rec["ip"] != "198.51.100.9" {
		t.Errorf("ip = %v, want the socket peer 198.51.100.9", rec["ip"])
	}
}

// Nobody signed in is a fact, not a blank.
func TestAccessLogSaysAnonymous(t *testing.T) {
	r := chi.NewRouter()
	r.Use(accessLog("app"))
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hi")) })

	req := httptest.NewRequest("GET", "/apps/reports/index.html", nil)
	req.Header.Set("User-Agent", "toft/1.2.3")
	rec := capture(t, r, req)

	if rec["user"] != "anonymous" {
		t.Errorf("user = %v, want anonymous", rec["user"])
	}
	if _, ok := rec["api_key_id"]; ok {
		t.Error("api_key_id is present on a request that used no key")
	}
	// The field this whole thread started from.
	if rec["user_agent"] != "toft/1.2.3" {
		t.Errorf("user_agent = %v", rec["user_agent"])
	}
	if rec["bytes"] != float64(2) {
		t.Errorf("bytes = %v, want 2", rec["bytes"])
	}
}

// The gateway's line carries which app served it and which credential was
// spent, which is the half that made this worth doing on that router at all.
func TestAccessLogCarriesAppAndKeyOnTheGateway(t *testing.T) {
	r := chi.NewRouter()
	r.Use(accessLog("app"))
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			setAccessApp(r.Context(), "app-9")
			setAccessKey(r.Context(), "key-1", "app")
			next.ServeHTTP(w, r)
		})
	})
	r.Get("/apps/{slug}/*", func(w http.ResponseWriter, r *http.Request) {})

	rec := capture(t, r, httptest.NewRequest("GET", "/apps/reports/a.html", nil))

	if rec["app_id"] != "app-9" {
		t.Errorf("app_id = %v", rec["app_id"])
	}
	if rec["api_key_id"] != "key-1" || rec["credential"] != "app" {
		t.Errorf("credential = %v / %v", rec["api_key_id"], rec["credential"])
	}
	if rec["surface"] != "app" {
		t.Errorf("surface = %v", rec["surface"])
	}
}

// A 5xx is the platform's problem and rises; a 4xx is the caller's and does
// not, or a scanner walking the estate fills the operator's error stream.
func TestAccessLogLevels(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusNotFound, "INFO"},
		{http.StatusTooManyRequests, "INFO"},
		{http.StatusBadGateway, "ERROR"},
	} {
		r := chi.NewRouter()
		r.Use(accessLog("platform"))
		r.Get("/x", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })

		rec := capture(t, r, httptest.NewRequest("GET", "/x", nil))
		if rec["level"] != tc.want {
			t.Errorf("status %d logged at %v, want %v", tc.status, rec["level"], tc.want)
		}
	}
}

// A successful probe is Debug; anything that only looks like one stays Info, so
// claiming a health checker's user agent cannot hide a request.
func TestAccessLogDemotesHealthProbes(t *testing.T) {
	for _, tc := range []struct {
		name, path, agent string
		status            int
		principal         bool
		want              string
	}{
		{"alb on the platform", "/api/v1/config", "ELB-HealthChecker/2.0", 200, false, "DEBUG"},
		{"alb on the gateway root", "/", "ELB-HealthChecker/2.0", 404, false, "DEBUG"},
		{"kubelet", "/api/v1/config", "kube-probe/1.31", 200, false, "DEBUG"},
		{"the image's own check", "/api/v1/config", "toft-healthcheck", 200, false, "DEBUG"},
		{"a failing probe", "/api/v1/config", "ELB-HealthChecker/2.0", 503, false, "ERROR"},
		{"a browser on the same path", "/api/v1/config", "Mozilla/5.0", 200, false, "INFO"},
		{"a probe agent on another path", "/api/v1/apps", "ELB-HealthChecker/2.0", 200, false, "INFO"},
		{"a probe agent with a caller", "/api/v1/config", "ELB-HealthChecker/2.0", 200, true, "INFO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Use(accessLog("platform"))
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.principal {
						setAccessPrincipal(r.Context(), &auth.Principal{ID: "user-7"})
					}
					next.ServeHTTP(w, r)
				})
			})
			r.Get(tc.path, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status) })

			req := httptest.NewRequest("GET", tc.path, nil)
			req.Header.Set("User-Agent", tc.agent)
			rec := capture(t, r, req)
			if rec["level"] != tc.want {
				t.Errorf("logged at %v, want %v", rec["level"], tc.want)
			}
		})
	}
}

// A WebSocket upgrade writes its 101 to a hijacked socket. Reporting 0 — or
// calling 0 "200" — is a lie about the request most likely to be under
// investigation.
func TestAccessRecorderReportsHijackAsSwitchingProtocols(t *testing.T) {
	w := &accessRecorder{hijacked: true}
	if got := w.statusCode(); got != http.StatusSwitchingProtocols {
		t.Errorf("hijacked connection reported %d, want 101", got)
	}
	if got := (&accessRecorder{}).statusCode(); got != http.StatusOK {
		t.Errorf("handler that wrote nothing reported %d, want 200", got)
	}
}

// TestAccessLogWritesALineForAnAbortedRequest is TL-3. ReverseProxy ends a
// response the client abandoned with panic(http.ErrAbortHandler), which passes
// this middleware on its way to net/http; ADR §46 promises a line anyway.
func TestAccessLogWritesALineForAnAbortedRequest(t *testing.T) {
	r := chi.NewRouter()
	r.Use(accessLog("app"))
	r.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic(http.ErrAbortHandler)
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Errorf("the panic reaching the caller was %v; the middleware must not swallow it", v)
			}
		}()
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil))
	}()

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 1 || len(lines[0]) == 0 {
		t.Fatalf("got %d log lines, want exactly one", len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal(lines[0], &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if rec["aborted"] != true {
		t.Errorf("aborted = %v, want true", rec["aborted"])
	}
	if rec["status"] != float64(200) {
		t.Errorf("status = %v, want the 200 latched before the abort", rec["status"])
	}
}

// An ordinary request carries no `aborted` field at all.
func TestAccessLogOmitsAbortedForAFinishedRequest(t *testing.T) {
	r := chi.NewRouter()
	r.Use(accessLog("platform"))
	r.Get("/ok", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	rec := capture(t, r, httptest.NewRequest("GET", "/ok", nil))
	if _, ok := rec["aborted"]; ok {
		t.Errorf("a finished request carried aborted=%v", rec["aborted"])
	}
}

// TL-7: an interim 1xx relayed from an app is not the request's status.
func TestAccessRecorderIgnoresAnInterimResponse(t *testing.T) {
	w := &accessRecorder{ResponseWriter: httptest.NewRecorder()}
	w.WriteHeader(http.StatusContinue)
	w.WriteHeader(http.StatusEarlyHints)
	w.WriteHeader(http.StatusInternalServerError)
	if got := w.statusCode(); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	up := &accessRecorder{ResponseWriter: httptest.NewRecorder()}
	up.WriteHeader(http.StatusSwitchingProtocols)
	if got := up.statusCode(); got != http.StatusSwitchingProtocols {
		t.Errorf("a 101 written through WriteHeader reported %d", got)
	}
}
