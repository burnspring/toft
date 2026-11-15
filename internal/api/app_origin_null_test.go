package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"toft/internal/config"
)

// `Origin: null` on a state-changing request reaches an app only when the
// browser says the request is the page's own (security review 2026-11-10,
// D-3): a sandboxed iframe on a neighbour's host sends null with same-site.
func TestAnAppRefusesANullOriginFromAnotherSite(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example", AppsBaseURL: "https://apps.toft.example"}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	checks := map[string]http.Handler{"apps origin": s.appsOriginCheck(ok), "app host": s.appHostOriginCheck(ok)}
	for name, h := range checks {
		for _, tc := range []struct {
			method, site string
			want         int
		}{
			{http.MethodGet, "cross-site", http.StatusOK},
			{http.MethodPost, "cross-site", http.StatusForbidden},
			{http.MethodPost, "same-site", http.StatusForbidden},
			{http.MethodPost, "same-origin", http.StatusOK},
			{http.MethodPost, "", http.StatusOK},
		} {
			req := httptest.NewRequest(tc.method, "https://a.apps.toft.example/x", nil)
			req = req.WithContext(context.WithValue(req.Context(), appHostKey{}, "a.apps.toft.example"))
			req.Header.Set("Origin", "null")
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s: %s null origin, Sec-Fetch-Site %q = %d, want %d", name, tc.method, tc.site, rec.Code, tc.want)
			}
		}
	}
}

// A WebSocket handshake is a GET, but a socket's answers have no same-origin
// protection: a null-origin upgrade is held to the write rule, and one
// carrying a session cookie is refused outright (security review 2026-11-15,
// HD-1 — WebKit sends a same-site sandboxed frame's Lax cookie with it).
func TestAnAppRefusesANullOriginUpgradeFromAnotherSite(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example", AppsBaseURL: "https://apps.toft.example"}}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	checks := map[string]http.Handler{"apps origin": s.appsOriginCheck(ok), "app host": s.appHostOriginCheck(ok)}
	for name, h := range checks {
		for _, tc := range []struct {
			label, site, cookie string
			upgrade             bool
			want                int
		}{
			{"cross-site upgrade", "cross-site", "", true, http.StatusForbidden},
			{"same-site upgrade", "same-site", "", true, http.StatusForbidden},
			{"upgrade with the app cookie", "", appSessionCookie, true, http.StatusForbidden},
			{"upgrade with the host app cookie", "", "__Host-" + appSessionCookie, true, http.StatusForbidden},
			{"upgrade with the platform cookie", "same-origin", "toft_session", true, http.StatusForbidden},
			{"bare upgrade", "", "", true, http.StatusOK},
			{"same-origin upgrade", "same-origin", "", true, http.StatusOK},
			{"plain GET", "cross-site", appSessionCookie, false, http.StatusOK},
		} {
			req := httptest.NewRequest(http.MethodGet, "https://a.apps.toft.example/ws", nil)
			req = req.WithContext(context.WithValue(req.Context(), appHostKey{}, "a.apps.toft.example"))
			req.Header.Set("Origin", "null")
			if tc.upgrade {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: tc.cookie, Value: "v"})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s: %s = %d, want %d", name, tc.label, rec.Code, tc.want)
			}
		}
	}
}

// A client's Connection header cannot strip what the proxy sets: ReverseProxy
// applies it after the Director (security review 2026-11-10, D-5).
func TestAConnectionHeaderCannotStripPlatformHeaders(t *testing.T) {
	var got http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	proxy := &httputil.ReverseProxy{Director: func(req *http.Request) {
		settleConnectionHeader(req.Header)
		req.URL.Scheme, req.URL.Host = target.Scheme, target.Host
		req.Header.Set("X-Toft-Act-For", "signed")
		req.Header.Set("X-Real-IP", "192.0.2.1")
	}}
	front := httptest.NewServer(proxy)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodGet, front.URL, nil)
	req.Header.Set("Connection", "X-Toft-Act-For, X-Real-IP, X-Client-Hop")
	req.Header.Set("X-Client-Hop", "dropped")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("X-Toft-Act-For") != "signed" || got.Get("X-Real-IP") != "192.0.2.1" {
		t.Errorf("the platform's headers were stripped: %v", got)
	}
	if got.Get("X-Client-Hop") != "" {
		t.Error("a header the client named hop-by-hop was passed on")
	}
}

// An underscore spelling of a platform header is dropped (security review
// 2026-11-10, G-2); an unrelated underscore header stays.
func TestUnderscoreSpellingsOfPlatformHeadersAreDropped(t *testing.T) {
	h := http.Header{}
	h["X_Toft_Session"] = []string{"forged"}
	h["x_toft_request_deadline"] = []string{"forged"}
	h["X_Custom"] = []string{"kept"}
	dropUnderscoreSpellings(h, "X-Toft-Session", "X-Toft-Request-Deadline")
	if len(h["X_Toft_Session"]) != 0 || len(h["x_toft_request_deadline"]) != 0 {
		t.Errorf("an underscore spelling survived: %v", h)
	}
	if h["X_Custom"][0] != "kept" {
		t.Error("an unrelated header was dropped")
	}
}
