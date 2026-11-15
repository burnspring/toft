package api

import (
	"strings"
	"testing"

	"toft/internal/config"
)

// The OAuth `redirect` parameter was stored in a cookie and used verbatim as the
// post-login target, so a login link could send the user off-site after a real,
// successful GitHub login. Finding 0.8.
func TestSafeRedirect(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg = &config.Config{BaseURL: "https://toft.example.com"}

	for _, tc := range []struct {
		name, in, want string
	}{
		// Accepted.
		{"site-relative path", "/apps/reports", "/apps/reports"},
		{"root", "/", "/"},
		{"path with query", "/apps?tab=logs", "/apps?tab=logs"},
		{"the platform's own origin", "https://toft.example.com/apps", "https://toft.example.com/apps"},

		// Rejected.
		{"the vite dev server, from a deployed install", "http://localhost:5173/apps", ""},
		{"absolute off-site URL", "https://evil.example/", ""},
		{"scheme-relative URL", "//evil.example/", ""},
		{"backslash scheme-relative", `/\evil.example/`, ""},
		{"lookalike host prefix", "https://toft.example.com.evil.example/", ""},
		{"javascript scheme", "javascript:alert(1)", ""},
		{"data scheme", "data:text/html,<script>alert(1)</script>", ""},
		{"header injection", "/apps\r\nLocation: https://evil.example", ""},
		{"tab inside the leading slashes", "/\t/evil.example", ""},
		{"newline inside the leading slashes", "/\n/evil.example", ""},
		{"empty", "", ""},
		{"bare word", "evil.example", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := srv.safeRedirect(nil, tc.in); got != tc.want {
				t.Errorf("safeRedirect(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// The Vite dev server is a legitimate target on a loopback install, and only
	// there — the base URL decides.
	t.Run("the vite dev server, on a loopback install", func(t *testing.T) {
		// A bare Server: safeRedirect reads nothing but the config, and copying the
		// real one would copy a sync.Map.
		dev := &Server{cfg: &config.Config{BaseURL: "http://localhost:8080"}}
		if got := dev.safeRedirect(nil, "http://localhost:5173/apps"); got != "http://localhost:5173/apps" {
			t.Errorf("safeRedirect on a dev install = %q, want the dev server URL", got)
		}
	})
}

// The hostile value must not even reach the cookie.
func TestGitHubLoginDropsAnOffSiteRedirect(t *testing.T) {
	srv := newTestServer(t)

	rec := getThroughRouter(t, srv, "/api/v1/auth/github/login?redirect=https://evil.example/")

	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthRedirectCookie && strings.Contains(c.Value, "evil.example") {
			t.Fatalf("off-site redirect was stored in a cookie: %q", c.Value)
		}
	}
}

func TestGitHubLoginKeepsARelativeRedirect(t *testing.T) {
	srv := newTestServer(t)

	rec := getThroughRouter(t, srv, "/api/v1/auth/github/login?redirect=/apps/reports")

	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthRedirectCookie {
			found = true
			if c.Value != "/apps/reports" {
				t.Errorf("oauth_redirect = %q, want /apps/reports", c.Value)
			}
		}
	}
	if !found {
		t.Error("a legitimate relative redirect was dropped")
	}
}
