package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The test whose failure nothing on a screen would show: a hash in the card's
// policy that is not the hash of what was served. The browser refuses the
// script, the card stops refreshing, and <noscript> does not appear because
// scripting is still on. html/template strips comments when it renders, which
// is why the hashes are taken from a render and this compares against one.
func TestTheCardHashesMatchWhatIsServed(t *testing.T) {
	s := &Server{}
	render := func(state interstitialState, msg string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.serveInterstitial(rec, "Sales", "app_1", "frame-ancestors 'self'", state, msg)
		return rec
	}
	waking := render(stateWaking, "This app was asleep and is starting up.")
	building := render(stateBuilding, "This app is being built.")

	csp := waking.Header().Get("Content-Security-Policy")
	for _, tag := range []string{"style", "script"} {
		blocks := inlineBlocks(waking.Body.String(), tag)
		if len(blocks) == 0 {
			t.Fatalf("a refreshing card has no <%s>", tag)
		}
		for _, b := range blocks {
			sum := sha256.Sum256([]byte(b))
			if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
				t.Errorf("the served <%s> hashes to %s, which the policy does not name: %s", tag, want, csp)
			}
		}
		// A template action inside a block would make one render stop
		// standing for every render; two different cards must agree.
		if a, b := strings.Join(blocks, "|"), strings.Join(inlineBlocks(building.Body.String(), tag), "|"); a != b {
			t.Errorf("<%s> differs between two cards — the init-time hash no longer covers every render", tag)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("the card's policy fell back to 'unsafe-inline': %s", csp)
	}
}

// A card is framed by its app's answer, because the preview iframe shows it
// before the app; one that does not refresh runs no script and names none.
func TestAStatusCardIsFramedLikeItsApp(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).serveSettledInterstitial(rec, "Sales", "app_1",
		"frame-ancestors 'self' https://toft.example", stateCrashed, "msg")
	h := rec.Header()
	csp := h.Get("Content-Security-Policy")
	if !strings.HasSuffix(csp, "frame-ancestors 'self' https://toft.example") {
		t.Errorf("the card is not framed like its app: %s", csp)
	}
	if !strings.HasPrefix(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Errorf("a settled card's policy: %s", csp)
	}
	if h.Get("X-Frame-Options") != "" {
		t.Error("X-Frame-Options would blank the preview iframe")
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
		t.Errorf("headers: %v", h)
	}
}

// The 404 card stands in for no app, so nobody frames it.
func TestTheNotFoundCardIsFramedByNobody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/apps/nope/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	(&Server{}).serveGatewayNotFound(rec, req)
	h := rec.Header()
	if csp := h.Get("Content-Security-Policy"); !strings.HasSuffix(csp, "frame-ancestors 'none'") ||
		!strings.HasPrefix(csp, "default-src 'none'") {
		t.Errorf("the 404 card's policy: %s", csp)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
		t.Errorf("headers: %v", h)
	}
}
