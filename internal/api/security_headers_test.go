package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/config"
)

// headerServer is a router and nothing behind it. Building one is pure — the
// same construction `RouteInventory` uses — so these need no database, which is
// the point: a header is set by middleware or it is not, and a Postgres
// container cannot tell you which.
func headerServer(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	s := &Server{cfg: cfg}
	return s.Router(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// The two things worth asserting are opposites: that the platform's own pages
// carry the headers, and that a deployed app's pages do not. They are tested in
// two places now, because they are decided in two places — this file has the
// half a middleware can answer, and the proxy's tests have the half that needs
// an app to have been resolved. The second is the one that would break an
// install rather than merely weaken it: a policy written for this SPA, applied
// to somebody else's Shiny app, forbids the inline scripts that app is made of.

func TestSecurityHeadersOnThePlatformsOwnPages(t *testing.T) {
	router := headerServer(t, &config.Config{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy on the SPA")
	}
	for _, want := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"script-src 'self'",
		"base-uri 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("policy is missing %q: %s", want, csp)
		}
	}
	// script-src is the half that matters for XSS, and it may not gain
	// 'unsafe-inline' or 'unsafe-eval' quietly. style-src has the first
	// deliberately — see the file. 'wasm-unsafe-eval' is the one exception and
	// is named: it compiles WebAssembly and evaluates no string as script.
	for _, directive := range strings.Split(csp, ";") {
		d := strings.TrimSpace(directive)
		if !strings.HasPrefix(d, "script-src") {
			continue
		}
		for _, src := range strings.Fields(d)[1:] {
			if strings.Contains(src, "unsafe") && src != "'wasm-unsafe-eval'" {
				t.Errorf("script-src has been weakened by %s: %s", src, d)
			}
		}
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got == "" {
		t.Error("no Referrer-Policy")
	}
}

// The middleware exempts nothing, and `/apps/…` is the case that says so.
//
// This test asserted the opposite until a scan found `/apps` — the app list,
// one of this application's own pages — being served with no CSP and no
// X-Frame-Options, because the middleware skipped a path prefix and three
// unrelated things share that prefix. Every path below except the last is the
// SPA; the last is a deployed app, and it is here to make the point that the
// middleware cannot tell, which is why it no longer tries. What comes off an
// app's response comes off in appProxyGuard, and TestProxyStripsThePlatforms
// SecurityHeaders is where that half is proved.
//
// The middleware directly rather than through the router: what is under test is
// that it makes no decision about a path at all.
func TestSecurityHeadersAreSetOnEveryPath(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{
		"/",
		"/api/v1/apps",
		"/apps",     // the SPA's app list
		"/apps/new", // the SPA's create page
		"/apps/8f14e45f-ceea-467a-9fdb-7c39f8f4e4a1",      // an app id, not a slug
		"/apps/8f14e45f-ceea-467a-9fdb-7c39f8f4e4a1/keys", // and one of its tabs
		"/apps/reports/static/app.js",                     // a deployed app, stripped later
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s has no Content-Security-Policy", path)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", path, got)
		}
	}
}

// The preview iframe embeds the app's own origin, so a policy that forgot it
// would render a blank box on the app page of every separate-origin install.
func TestPolicyAdmitsTheAppsOriginWhenItIsSeparate(t *testing.T) {
	separate := newSecurityHeaderPolicy("http://localhost:8081", true, nil)
	if !strings.Contains(separate.csp, "frame-src 'self' http://localhost:8081") {
		t.Errorf("frame-src does not admit the apps origin: %s", separate.csp)
	}

	same := newSecurityHeaderPolicy("http://localhost:8080", false, nil)
	if strings.Contains(same.csp, "frame-src 'self' http") {
		t.Errorf("single-origin install names the apps origin twice: %s", same.csp)
	}
	if !strings.Contains(same.csp, "frame-src 'self'") {
		t.Errorf("single-origin install cannot frame its own apps: %s", same.csp)
	}
}

// The store browser uploads straight to the bucket, so the bucket's origins
// are in connect-src when uploads are on -- without them the page refused
// every upload as a network error (docs/bugs 2026-10-22-e2e-storage #1).
func TestPolicyAdmitsTheObjectStoreWhenUploadsAreOn(t *testing.T) {
	p := newSecurityHeaderPolicy("http://localhost:8081", true,
		[]string{"https://s3.us-east-1.amazonaws.com", "https://*.s3.us-east-1.amazonaws.com"})
	if !strings.Contains(p.csp, "connect-src 'self' http://localhost:8081 https://s3.us-east-1.amazonaws.com https://*.s3.us-east-1.amazonaws.com") {
		t.Errorf("connect-src does not admit the store: %s", p.csp)
	}
	if !strings.Contains(p.csp, "script-src 'self' 'wasm-unsafe-eval'") {
		t.Errorf("script-src cannot compile the upload's digest module: %s", p.csp)
	}
	if none := newSecurityHeaderPolicy("", false, nil); strings.Contains(none.csp, "amazonaws") {
		t.Errorf("an install with no store names one: %s", none.csp)
	}
}

// HSTS, through the router: it is set beside the platform's headers and is
// decided by the listener's address as well as by the request.
//
// The second case is the one that would fail silently. A chart install whose
// trusted-proxy list is still the loopback default reads every request as
// http, and a header sent only on an https request would then be sent to
// nobody — with nothing on any screen saying so.
func TestHSTSFollowsTheAddressAndTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, target string
		xfp                   string
		want                  bool
	}{
		{"https address, request reads http", "https://toft.example", "http://toft.example/", "", true},
		{"forwarded https from an untrusted peer is ignored, the address still sends it", "https://toft.example", "http://toft.example/", "https", true},
		{"http address, http request", "http://localhost:8080", "http://localhost:8080/", "", false},
		{"http address, forwarded https from an untrusted peer", "http://localhost:8080", "http://localhost:8080/", "https", false},
		{"http address, the request itself arrived over TLS", "http://localhost:8080", "https://localhost:8080/", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := headerServer(t, &config.Config{BaseURL: tc.baseURL})
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.xfp != "" {
				req.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			got := rec.Header().Values("Strict-Transport-Security")
			if tc.want && (len(got) != 1 || got[0] != hstsValue) {
				t.Errorf("Strict-Transport-Security = %v, want exactly [%s]", got, hstsValue)
			}
			if !tc.want && len(got) != 0 {
				t.Errorf("Strict-Transport-Security = %v on an http install, want none", got)
			}
		})
	}
}

// One host, two ports, two schemes: HSTS ignores the port, so the https side's
// header would send every browser that saw it to an https port nothing serves.
// Withheld on both listeners. Two hosts under different schemes are two hosts,
// and each sends what its own address says.
func TestHSTSIsWithheldOnOneHostWithMixedSchemes(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, tc := range []struct {
		name              string
		cfg               config.Config
		withheld          bool
		platform, gateway bool
	}{
		{"one host, mixed schemes", config.Config{
			BaseURL: "https://toft.example", AppsBaseURL: "http://toft.example:8081", Port: 8080, AppsPort: 8081,
		}, true, false, false},
		{"one host, both https", config.Config{
			BaseURL: "https://toft.example", AppsBaseURL: "https://toft.example:8081", Port: 8080, AppsPort: 8081,
		}, false, true, true},
		{"two hosts, mixed schemes", config.Config{
			BaseURL: "https://toft.example", AppsBaseURL: "http://apps.example:8081", Port: 8080, AppsPort: 8081,
		}, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if got := hstsWithheld(&cfg); got != tc.withheld {
				t.Fatalf("hstsWithheld = %v, want %v", got, tc.withheld)
			}
			s := &Server{cfg: &cfg}
			for _, side := range []struct {
				name string
				url  string
				want bool
			}{{"platform", cfg.BaseURL, tc.platform}, {"gateway", cfg.AppsBaseURL, tc.gateway}} {
				rec := httptest.NewRecorder()
				s.hsts(side.url)(ok).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://toft.example/", nil))
				if got := rec.Header().Get("Strict-Transport-Security") != ""; got != side.want {
					t.Errorf("%s: HSTS sent = %v, want %v", side.name, got, side.want)
				}
			}
		})
	}
}
