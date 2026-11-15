package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The landing page put content on `/` for anybody who can reach the host, and
// this is the answer to the crawler that follows. The assertion is the literal
// file rather than "contains Disallow", because the two lines *are* the
// decision — a `robots.txt` that allowed anything would still parse.
func TestRobotsRefusesEveryCrawler(t *testing.T) {
	// No database and no config: the answer does not depend on this install,
	// which is the same rule the page it protects is written under.
	srv := &Server{}

	rec := httptest.NewRecorder()
	srv.handleRobots(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), "User-agent: *\nDisallow: /\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}
