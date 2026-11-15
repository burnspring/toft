package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/config"
)

func TestOriginCheck(t *testing.T) {
	cfg := &config.Config{
		BaseURL:     "https://toft.example",
		Port:        8080,
		AppsPort:    8081,
		AppsBaseURL: "https://toft.example:8081",
	}
	s := &Server{cfg: cfg}

	handler := s.originCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name   string
		origin string
		want   int
	}{
		{"no origin (navigation or server call)", "", http.StatusOK},
		{"platform origin", "https://toft.example", http.StatusOK},
		{"platform origin, case insensitive", "HTTPS://TOFT.EXAMPLE", http.StatusOK},
		{"apps gateway origin is rejected", "https://toft.example:8081", http.StatusForbidden},
		{"unrelated site", "https://evil.example", http.StatusForbidden},
		{"scheme downgrade", "http://toft.example", http.StatusForbidden},
		{"opaque origin is refused on a write", "null", http.StatusForbidden},
		{"dev origin blocked in production config", "http://localhost:5173", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// What a forged request from an app page can look like once a bare foreign
// Origin is refused: `Origin: null` (no-referrer, a sandboxed frame), or no
// Origin at all with the form's own content type or the fetch metadata giving
// it away. The apps gateway is same-site, so the Lax cookie rides all of them.
func TestOriginCheckRefusesBrowserForms(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example", AppsBaseURL: "https://toft.example:8081"}}
	handler := s.originCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name    string
		method  string
		origin  string
		cookie  bool
		authz   string
		ctype   string
		fetchBy string
		want    int
	}{
		{"null origin on a read passes", http.MethodGet, "null", true, "", "", "", http.StatusOK},
		{"null origin on a write is refused", http.MethodPost, "null", true, "", "application/json", "", http.StatusForbidden},
		{"text/plain form with the cookie", http.MethodPost, "", true, "", "text/plain", "", http.StatusForbidden},
		{"urlencoded form with the cookie", http.MethodPost, "", true, "", "application/x-www-form-urlencoded; charset=utf-8", "", http.StatusForbidden},
		{"same-site fetch metadata without Origin", http.MethodPost, "", true, "", "application/json", "same-site", http.StatusForbidden},
		{"cross-site fetch metadata without Origin", http.MethodDelete, "", true, "", "", "cross-site", http.StatusForbidden},
		{"the SPA's own JSON write", http.MethodPost, "https://toft.example", true, "", "application/json", "same-origin", http.StatusOK},
		{"the SPA's multipart upload", http.MethodPost, "https://toft.example", true, "", "multipart/form-data; boundary=x", "same-origin", http.StatusOK},
		{"a key-bearing script may send text", http.MethodPost, "", true, "Bearer toftk_x", "text/plain", "", http.StatusOK},
		{"no cookie: a webhook or a CLI", http.MethodPost, "", false, "", "application/x-www-form-urlencoded", "", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/admin/promotion-requests/1/approve", strings.NewReader("x"))
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "jwt"})
			}
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			if tc.ctype != "" {
				req.Header.Set("Content-Type", tc.ctype)
			}
			if tc.fetchBy != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchBy)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// The store browser PUTs a File to the host data plane with the browser's own
// Content-Type — text/plain for a .txt — and the Path=/ cookie riding along.
// The forged-form backstop leaves that route alone; the Origin allow-list does
// not (security review 2026-11-15, HD-3).
func TestOriginCheckLetsTheStoreBrowserUploadText(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example", AppsBaseURL: "https://toft.example:8081"}}
	handler := s.originCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, tc := range []struct {
		name, path, origin string
		want               int
	}{
		{"a text upload to the data plane", "/store-data/tok", "https://toft.example", http.StatusOK},
		{"the same request on the API", "/api/v1/apps/x/store/notes.txt", "https://toft.example", http.StatusForbidden},
		{"the data plane from a foreign origin", "/store-data/tok", "https://evil.example", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader("x"))
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", "text/plain")
			req.AddCookie(&http.Cookie{Name: auth.HostSessionCookie, Value: "jwt"})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestOriginCheckAllowsViteInDev(t *testing.T) {
	// A loopback BASE_URL is what marks this as somebody's laptop rather than a
	// deployment.
	s := &Server{cfg: &config.Config{BaseURL: "http://localhost:8080"}}

	handler := s.originCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// EXTRA_ORIGINS is what lets a public install still be driven through an SSH
// tunnel: BASE_URL is the https hostname, so the tunnel's Origin would
// otherwise be rejected and every mutation would 403 while reads kept working.
func TestOriginCheckAllowsExtraOrigins(t *testing.T) {
	s := &Server{cfg: &config.Config{
		BaseURL:      "https://toft.example",
		AppsPort:     8081,
		AppsBaseURL:  "https://toft.example:8081",
		ExtraOrigins: []string{"http://localhost:8080", "http://localhost:5173"},
	}}

	handler := s.originCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name   string
		origin string
		want   int
	}{
		{"tunnelled platform port", "http://localhost:8080", http.StatusOK},
		{"tunnelled vite dev server", "http://localhost:5173", http.StatusOK},
		{"base url still allowed", "https://toft.example", http.StatusOK},
		{"unlisted port is not implied", "http://localhost:9090", http.StatusForbidden},
		{"apps gateway still rejected", "https://toft.example:8081", http.StatusForbidden},
		{"unrelated site", "https://evil.example", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", nil)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// The OAuth callback has to come back to the origin the sign-in started on,
// because that is the only one holding the state and PKCE cookies. Pinning it
// to BASE_URL meant a tunnelled sign-in died on "invalid state - mismatch"
// after the password had already been accepted.
func TestRedirectURIFollowsRequestOrigin(t *testing.T) {
	s := &Server{cfg: &config.Config{
		BaseURL:      "https://toft.example",
		ExtraOrigins: []string{"http://localhost:8080"},
	}}

	tests := []struct {
		name string
		host string
		want string
	}{
		{"public hostname", "toft.example", "https://toft.example/api/v1/auth/dex/callback"},
		{"trusted tunnel", "localhost:8080", "http://localhost:8080/api/v1/auth/dex/callback"},
		{"untrusted host falls back to BASE_URL", "evil.example", "https://toft.example/api/v1/auth/dex/callback"},
		{"host with no port does not match a ported origin", "localhost", "https://toft.example/api/v1/auth/dex/callback"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/dex/login", nil)
			req.Host = tc.host
			if got := s.redirectURIFor(req, "dex"); got != tc.want {
				t.Errorf("redirectURIFor = %q, want %q", got, tc.want)
			}
		})
	}

	// The admin form shows the canonical URL to register with the IdP, which
	// must stay BASE_URL regardless of who is looking at the page.
	if got := s.redirectURI("dex"); got != "https://toft.example/api/v1/auth/dex/callback" {
		t.Errorf("redirectURI = %q, want the BASE_URL form", got)
	}
}

func TestNormalizeOrigin(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://example.com", "https://example.com"},
		{"https://example.com/path?q=1", "https://example.com"},
		{"HTTP://Example.COM:8081", "http://example.com:8081"},
		{"  https://example.com  ", "https://example.com"},
		{"not-a-url", ""},
		{"", ""},
	}

	for _, tc := range tests {
		if got := normalizeOrigin(tc.in); got != tc.want {
			t.Errorf("normalizeOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// loopbackTrusted is the default TRUSTED_PROXY_CIDRS, hand-built so these
// tests exercise realIPMiddleware itself rather than a context flag set by
// hand — the marking and the reading are the pair under test.
func loopbackTrusted(t *testing.T) []*net.IPNet {
	t.Helper()
	_, v4, err := net.ParseCIDR("127.0.0.1/32")
	if err != nil {
		t.Fatalf("parsing loopback CIDR: %v", err)
	}
	return []*net.IPNet{v4}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestOriginCheckTrustsForwardedOrigin covers the fresh-install case: BASE_URL
// still at its packaged default while the install is actually served at a real
// hostname behind a proxy. Before the forwarded origin was believed, the login
// page rendered (no Origin on a same-origin GET) and the password POST was
// refused as cross-origin — nobody could sign in at all, including the built-in
// administrator, and so nobody could reach a settings page to fix it.
func TestOriginCheckTrustsForwardedOrigin(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "http://localhost:8080"}}
	handler := realIPMiddleware(loopbackTrusted(t))(s.originCheck(okHandler()))

	tests := []struct {
		name       string
		peer       string
		host       string
		fwdProto   string
		fwdHost    string
		origin     string
		want       int
		wantReason string
	}{
		{
			name:     "proxy on this box declares the public origin",
			peer:     "127.0.0.1:52000",
			host:     "toft.example",
			fwdProto: "https",
			origin:   "https://toft.example",
			want:     http.StatusOK,
		},
		{
			name:     "X-Forwarded-Host wins over the Host header",
			peer:     "127.0.0.1:52000",
			host:     "toft.internal:8080",
			fwdProto: "https",
			fwdHost:  "toft.example",
			origin:   "https://toft.example",
			want:     http.StatusOK,
		},
		{
			// The whole safety property. A remote client sending the same
			// headers is nominating its own origin, and must not be believed.
			name:     "the same headers from a remote peer are ignored",
			peer:     "203.0.113.9:52000",
			host:     "toft.example",
			fwdProto: "https",
			origin:   "https://toft.example",
			want:     http.StatusForbidden,
		},
		{
			// Trusting the forwarded origin does not stop the check biting:
			// a cross-site page's Origin still disagrees with the address the
			// request travelled to.
			name:     "cross-site origin still rejected behind the proxy",
			peer:     "127.0.0.1:52000",
			host:     "toft.example",
			fwdProto: "https",
			origin:   "https://evil.example",
			want:     http.StatusForbidden,
		},
		{
			// X-Forwarded-Host alone is still a declaration: the scheme is
			// guessed as http, so an https Origin does not match, and the
			// refusal has to explain itself rather than read as a bad password.
			name:       "trusted proxy that forwards a host but no scheme",
			peer:       "127.0.0.1:52000",
			host:       "toft.internal:8080",
			fwdHost:    "toft.example",
			origin:     "https://toft.example",
			want:       http.StatusForbidden,
			wantReason: "does not recognise as its own address",
		},
		{
			// The loopback default cannot tell Caddy on this box from curl on
			// this box. Nothing was forwarded, so nothing is declared, and a
			// local caller cannot make itself the install's public address.
			name:   "loopback with no forwarded headers declares nothing",
			peer:   "127.0.0.1:52000",
			host:   "toft.example",
			origin: "http://toft.example",
			want:   http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/local/login", nil)
			req.RemoteAddr = tc.peer
			req.Host = tc.host
			req.Header.Set("Origin", tc.origin)
			if tc.fwdProto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.fwdProto)
			}
			if tc.fwdHost != "" {
				req.Header.Set("X-Forwarded-Host", tc.fwdHost)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.wantReason != "" && !strings.Contains(rec.Body.String(), tc.wantReason) {
				t.Errorf("body = %q, want it to explain %q", rec.Body.String(), tc.wantReason)
			}
		})
	}
}

// TestRejectOriginDiagnosesMisconfiguration pins the distinction the refusal
// draws. An Origin that matches the address the request was sent to cannot be
// forgery — the browser is on a page this install served — so it earns an
// explanation. An Origin naming another site earns nothing that would help.
func TestRejectOriginDiagnosesMisconfiguration(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "http://localhost:8080"}}
	handler := s.originCheck(okHandler())

	t.Run("own address, no proxy configured", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/local/login", nil)
		req.Host = "toft.example"
		req.Header.Set("Origin", "https://toft.example")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{"https://toft.example", "BASE_URL", "Nothing is wrong with your account"} {
			if !strings.Contains(body, want) {
				t.Errorf("body = %q, want it to mention %q", body, want)
			}
		}
	})

	t.Run("another site gets the bare refusal", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", nil)
		req.Host = "toft.example"
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "BASE_URL") {
			t.Errorf("a cross-site refusal must not name configuration; got %q", body)
		}
	})
}

// A loopback address is right for the inbound checks and wrong as a suggestion.
// The Vite dev server preserves the browser's Host while proxying from
// loopback, so the settings page once offered `http://localhost:5173` as the
// address to put in every notification — and it was taken. Two guards stand
// there now, and this pins the second.
func TestPublicObservedOriginRefusesLoopback(t *testing.T) {
	trusted := loopbackTrusted(t)

	cases := []struct {
		name         string
		host         string
		fwdHost      string
		fwdProto     string
		wantInbound  string
		wantOfferred string
	}{
		{
			name:         "a real public address is both trusted and offered",
			host:         "toft.example",
			fwdProto:     "https",
			wantInbound:  "https://toft.example",
			wantOfferred: "https://toft.example",
		},
		{
			name:         "loopback passes the origin check but is never suggested",
			host:         "localhost:5173",
			fwdHost:      "localhost:5173",
			fwdProto:     "http",
			wantInbound:  "http://localhost:5173",
			wantOfferred: "",
		},
		{
			name:         "127.0.0.1 likewise",
			host:         "127.0.0.1:8080",
			fwdHost:      "127.0.0.1:8080",
			fwdProto:     "http",
			wantInbound:  "http://127.0.0.1:8080",
			wantOfferred: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotInbound, gotOffered string
			h := realIPMiddleware(trusted)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotInbound, gotOffered = forwardedOrigin(r), publicObservedOrigin(r)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/system/settings", nil)
			req.RemoteAddr = "127.0.0.1:52000"
			req.Host = tc.host
			if tc.fwdHost != "" {
				req.Header.Set("X-Forwarded-Host", tc.fwdHost)
			}
			req.Header.Set("X-Forwarded-Proto", tc.fwdProto)
			h.ServeHTTP(httptest.NewRecorder(), req)

			if gotInbound != tc.wantInbound {
				t.Errorf("forwardedOrigin = %q, want %q", gotInbound, tc.wantInbound)
			}
			if gotOffered != tc.wantOfferred {
				t.Errorf("publicObservedOrigin = %q, want %q", gotOffered, tc.wantOfferred)
			}
		})
	}
}

// On a single-origin install the platform router also serves the apps inline,
// and an app's own form post there takes the apps gateway's rule — a null
// origin its page sent under no-referrer, a urlencoded body riding the
// `__Host-` session cookie — while /api/v1 keeps the platform's (RO-1).
func TestSingleOriginAppProxyTakesTheGatewayOriginRule(t *testing.T) {
	_, router := proxiedApp(t, nil)

	post := func(target, origin, site string, cookie bool, ctype string) int {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("a=1"))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		if cookie {
			req.AddCookie(&http.Cookie{Name: auth.HostSessionCookie, Value: "x"})
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	const app = "/apps/reports/submit"
	const api = "/api/v1/admin/promotion-requests/1/approve"
	const form = "application/x-www-form-urlencoded"
	if got := post(app, "null", "same-origin", false, ""); got == http.StatusForbidden {
		t.Errorf("app: null origin, same-origin = 403, want admitted")
	}
	if got := post(app, "null", "cross-site", false, ""); got != http.StatusForbidden {
		t.Errorf("app: null origin, cross-site = %d, want 403", got)
	}
	if got := post(app, "", "", true, form); got == http.StatusForbidden {
		t.Errorf("app: urlencoded with the session cookie = 403, want admitted")
	}
	if got := post(api, "null", "same-origin", false, ""); got != http.StatusForbidden {
		t.Errorf("api: null origin, same-origin = %d, want 403", got)
	}
	if got := post(api, "", "", true, form); got != http.StatusForbidden {
		t.Errorf("api: urlencoded with the session cookie = %d, want 403", got)
	}
}

// The carve-out is single-origin only and is decided on both spellings of the
// path, so an encoding chi would route elsewhere keeps the platform's rule.
func TestRootOriginCheckCarvesOutOnlyTheInlineProxy(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	single := (&Server{cfg: &config.Config{BaseURL: "https://toft.example", Port: 8080, AppsPort: 8080,
		AppsBaseURL: "https://toft.example"}}).rootOriginCheck(ok)
	split := (&Server{cfg: &config.Config{BaseURL: "https://toft.example", Port: 8080, AppsPort: 8081,
		AppsBaseURL: "https://toft.example:8081"}}).rootOriginCheck(ok)

	run := func(h http.Handler, target string) int {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.Header.Set("Origin", "null")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := run(single, "/apps/reports/submit"); got != http.StatusOK {
		t.Errorf("single-origin app path = %d, want 200", got)
	}
	if got := run(single, "/%61pps/reports/submit"); got != http.StatusForbidden {
		t.Errorf("single-origin, prefix only in the decoded path = %d, want 403", got)
	}
	if got := run(single, "/api/v1/apps"); got != http.StatusForbidden {
		t.Errorf("single-origin api path = %d, want 403", got)
	}
	if got := run(split, "/apps/reports/submit"); got != http.StatusForbidden {
		t.Errorf("two-listener install, app path on the platform = %d, want 403", got)
	}
}
