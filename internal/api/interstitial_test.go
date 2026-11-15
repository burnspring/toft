package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/config"
)

// app.Name is attacker-controlled and the interstitial renders on the apps
// gateway origin, where script can reach every app the victim can. It must
// be escaped. Regression test for hardening task 1.2.
func TestInterstitialEscapesAppName(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()

	s.serveInterstitial(rec, `<script>steal()</script>`, "victim", "", stateStopped, "This app has been stopped by its owner.")

	body := rec.Body.String()
	if strings.Contains(body, "<script>steal()") {
		t.Fatalf("app name rendered unescaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected HTML-escaped app name, got:\n%s", body)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestInterstitialRefreshOnlyForTransientStates(t *testing.T) {
	s := &Server{}
	// Every state the product actually has, and the promise each one makes,
	// read off the state itself rather than restated here — a second list of
	// status words compared in a test is exactly the drift the states exist to
	// end.
	for _, state := range interstitialStates {
		rec := httptest.NewRecorder()
		status, wantRefresh := state.Status, state.Refresh
		s.serveInterstitial(rec, "app", "app", "", state, "msg")
		// The countdown, not a header: a `Refresh: 5` no reader can stop is
		// WCAG 2.2.1, so the promise is made in the page and can be ended
		// there. The assertion moves with it.
		if got := strings.Contains(rec.Body.String(), "Checking again in"); got != wantRefresh {
			t.Errorf("%s: countdown present = %v, want %v", status, got, wantRefresh)
		}
		if h := rec.Header().Get("Refresh"); h != "" {
			t.Errorf("%s: Refresh header %q — the countdown replaced it", status, h)
		}
	}
}

// The settled variant is the same card with the promise removed: one status
// word carries both readings, because a crashed app is being restarted right
// up until the supervisor stops trying.
func TestSettledInterstitialMakesNoPromise(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()

	s.serveSettledInterstitial(rec, "app", "app", "", stateCrashed,
		"This app has crashed and is not being restarted. Its owner will need to look at it.")

	body := rec.Body.String()
	if strings.Contains(body, "Checking again in") {
		t.Errorf("a countdown on a page that is not going to change:\n%s", body)
	}
	if strings.Contains(body, "automatically") {
		t.Errorf("the copy still promises a refresh:\n%s", body)
	}
	// The dot stops pulsing too: the animation is the promise in the same way
	// the header is.
	if !strings.Contains(body, `class="dot tone-danger settled"`) {
		t.Errorf("expected the settled dot, got:\n%s", body)
	}
}

func TestValidateAppName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"My Dashboard", true},
		{"", true}, // create paths default empty to the slug
		{"emoji 🚀 fine", true},
		{"<b>html is escaped at render, allowed</b>", true},
		{strings.Repeat("a", 100), true},
		{strings.Repeat("a", 101), false},
		{"line\nbreak", false},
		{"tab\there", false},
		{"esc\x1b[31m", false},
		{"del\x7f", false},
	}
	for _, c := range cases {
		err := validateAppName(c.name)
		if c.ok && err != nil {
			t.Errorf("validateAppName(%q) = %v, want nil", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validateAppName(%q) = nil, want error", c.name)
		}
	}
}

// The card carries a way back. A viewer whose app is asleep, building or
// locked had no anchor on this page at all, so the app's own page — its owner,
// its status, the person to ask — was unreachable from the one screen telling
// them something was wrong.
func TestInterstitialLinksBackToTheApp(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example.com"}}
	rec := httptest.NewRecorder()

	// The app's **id**, and this is the one link where the slug was a loop
	// rather than a 404: the platform origin recognises a deployed app's slug
	// and redirects it to the apps origin — the page the reader is already
	// looking at.
	s.serveInterstitial(rec, "Sales dashboard", "app_1a2b", "", stateSleeping, "This app is asleep and cannot be woken here.")

	if body := rec.Body.String(); !strings.Contains(body, `href="https://toft.example.com/apps/app_1a2b"`) {
		t.Errorf("no route back to the app:\n%s", body)
	}
}

// An install that has not worked out its own address renders the card without
// the link rather than with a broken one.
func TestInterstitialOmitsAnUnknownLink(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()

	s.serveInterstitial(rec, "app", "app", "", stateStopped, "msg")

	if body := rec.Body.String(); strings.Contains(body, "Open in Toft") {
		t.Errorf("offered a link with no address to put in it:\n%s", body)
	}
}

// 404 over 403 is only true if the two are indistinguishable. A slug that names
// no app and a slug naming an app this account may not read must be the same
// bytes — so the card names neither the app nor its owner, and both paths go
// through one function.
func TestGatewayNotFoundSaysNothingAboutTheApp(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example.com"}}

	req := httptest.NewRequest(http.MethodGet, "/somebody-elses-app", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.serveGatewayNotFound(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Go to Toft") {
		t.Errorf("no route back:\n%s", body)
	}
	for _, leak := range []string{"owner", "private", "permission denied", "forbidden"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("card says %q, which distinguishes refused from absent:\n%s", leak, body)
		}
	}
}

// An app's own fetch for a missing file keeps the sentence it has always had.
// A page of HTML in place of a missing asset is worse than a plain 404.
func TestGatewayNotFoundStaysPlainForNonNavigations(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/app/main.js", nil)
	rec := httptest.NewRecorder()
	s.serveGatewayNotFound(rec, req)

	if got := rec.Header().Get("Content-Type"); strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q on a non-navigation", got)
	}
}

// Every state this card can show renders a dot the stylesheet can colour.
//
// The class used to be `strings.ToLower(status)`, so "Out of Memory" became
// the four classes `out of memory settled` — none of which the stylesheet has
// — and the base `.dot` rule declared no `background`. The most severe state
// the interstitial can show was the one state that rendered a 10px transparent
// hole, and nothing could have reported it: a class that matches nothing is
// silent. This test is what makes that loud.
//
// It walks `interstitialStates` rather than a list of its own, so a state
// added to the product is a state this covers.
func TestEveryInterstitialStateHasADotTheStylesheetHas(t *testing.T) {
	s := &Server{}
	for _, state := range interstitialStates {
		if !interstitialTones[state.Tone] {
			t.Errorf("%q: tone %q is not one the stylesheet has a rule for",
				state.Status, state.Tone)
			continue
		}
		rec := httptest.NewRecorder()
		s.serveInterstitial(rec, "app", "app", "", state, "msg")
		body := rec.Body.String()

		class := "tone-" + state.Tone
		if !strings.Contains(body, `class="dot `+class) {
			t.Errorf("%q: expected a dot with %q, got:\n%s", state.Status, class, body)
		}
		// The rule the class needs, in the stylesheet the card carries. Read
		// out of the rendered page rather than out of the template constant,
		// because what a visitor gets is the rendered page.
		if !strings.Contains(body, ".dot."+class+" { background:") {
			t.Errorf("%q: no `.dot.%s` rule in the card's own stylesheet", state.Status, class)
		}
	}
}

// And the base rule paints something, so a state that somehow escapes the
// table above is grey rather than invisible.
func TestInterstitialDotHasABackgroundFallback(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.serveInterstitial(rec, "app", "app", "",
		interstitialState{Status: "Something new", Tone: "chartreuse", Refresh: false}, "msg")
	body := rec.Body.String()

	if !strings.Contains(body, `class="dot tone-neutral settled"`) {
		t.Errorf("an unnamed tone should fall back to neutral, got:\n%s", body)
	}
	if !strings.Contains(body, "background: var(--tone-neutral-solid);\n           animation") {
		t.Errorf("the base .dot rule declares no background:\n%s", body)
	}
}

// The words on the card are the product's, in the product's sentence case.
// "Deploy Failed" and "Out of Memory" were Title Case, which is the
// fingerprint of a surface that never got a design pass — and these two are on
// the most-seen screen this product has.
func TestInterstitialStatusesAreSentenceCase(t *testing.T) {
	for _, state := range interstitialStates {
		words := strings.Fields(state.Status)
		for i, w := range words {
			if i == 0 {
				continue
			}
			if w[0] >= 'A' && w[0] <= 'Z' {
				t.Errorf("%q: %q is capitalized mid-phrase — sentence case, like the rest of the product",
					state.Status, w)
			}
		}
	}
}

// Every card names its state on the response, one distinct word per state, so
// a client can tell the platform's page from the app's without reading HTML.
func TestInterstitialNamesItsStateInAHeader(t *testing.T) {
	seen := map[string]bool{}
	for _, state := range interstitialStates {
		rec := httptest.NewRecorder()
		(&Server{}).serveInterstitial(rec, "app", "app", "", state, "msg")
		got := rec.Header().Get(interstitialHeader)
		if got == "" || got != state.Key {
			t.Errorf("%q: %s = %q, want %q", state.Status, interstitialHeader, got, state.Key)
		}
		if seen[got] {
			t.Errorf("%q: key %q is shared with another state", state.Status, got)
		}
		seen[got] = true
	}
}
