package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(cookies map[string][]string, browser bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/apps", nil)
	for name, vals := range cookies {
		for _, v := range vals {
			r.AddCookie(&http.Cookie{Name: name, Value: v})
		}
	}
	if browser {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	return r
}

// A page on the apps origin can plant a cookie the platform's host receives —
// another port of the same host, or Domain= the parent of a sibling. Two copies
// is therefore no session: choosing either is choosing whose session a person
// gets.
func TestTwoCopiesOfTheSessionCookieAreNoSession(t *testing.T) {
	r := req(map[string][]string{SessionCookie: {"planted", "genuine"}}, true)
	if got := SessionTokenFor(r, false); got != "" {
		t.Errorf("one-host: token = %q, want none from two copies", got)
	}
	r = req(map[string][]string{SessionCookie: {"genuine"}}, true)
	if got := SessionTokenFor(r, false); got != "genuine" {
		t.Errorf("one-host: token = %q, want the one copy", got)
	}
}

// On a two-host install a browser is authenticated by the __Host- cookie alone,
// which a sibling host cannot set. The plain name — the one it can toss — still
// serves a client that is not a browser.
func TestTwoHostBrowsersUseOnlyTheHostCookie(t *testing.T) {
	r := req(map[string][]string{HostSessionCookie: {"genuine"}, SessionCookie: {"tossed"}}, true)
	if got := SessionTokenFor(r, true); got != "genuine" {
		t.Errorf("token = %q, want the __Host- cookie's", got)
	}
	r = req(map[string][]string{SessionCookie: {"tossed"}}, true)
	if got := SessionTokenFor(r, true); got != "" {
		t.Errorf("a browser presenting only the plain name = %q, want no session", got)
	}
	r = req(map[string][]string{SessionCookie: {"cli"}}, false)
	if got := SessionTokenFor(r, true); got != "cli" {
		t.Errorf("a non-browser client with the plain name = %q, want its token", got)
	}
	// On a one-host install the __Host- name is the one a page on the apps
	// port *can* set, so it is never read there.
	r = req(map[string][]string{HostSessionCookie: {"planted"}, SessionCookie: {"genuine"}}, true)
	if got := SessionTokenFor(r, false); got != "genuine" {
		t.Errorf("one-host: token = %q, want the plain cookie's", got)
	}
}
