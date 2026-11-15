package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// BB-95, BB-107: at the apps gateway a list holder — who was shown the app in
// their list — gets the 403 the API gives them, and a browser a card linking
// to the app's page where Request access is. A stranger keeps the 404 bytes a
// slug naming no app gets.
func TestTheGatewayTellsAListHolderTheAppExistsAndAStrangerNothing(t *testing.T) {
	srv := newTestServer(t)
	app := seedListRungApp(t, srv)
	enablePersonalKeys(t, srv, true)
	router := srv.AppsRouter()

	browse := func(who, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "text/html")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Cookie", appSessionFor(t, srv, who))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := browse("lister", "/apps/payroll/")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a list holder's browser = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, "/apps/"+app.ID+`"`) || !strings.Contains(body, "Request access") ||
		!strings.Contains(body, "Payroll") {
		t.Errorf("the card does not lead to the app's page: %s", body)
	}

	stranger := browse("stranger", "/apps/payroll/")
	nothing := browse("stranger", "/apps/no-such-app/")
	if stranger.Code != http.StatusNotFound || stranger.Body.String() != nothing.Body.String() {
		t.Errorf("a stranger = %d and differs from a missing app's answer", stranger.Code)
	}
	if strings.Contains(stranger.Body.String(), "Payroll") {
		t.Error("a stranger's 404 names the app")
	}

	key := func(who string) *httptest.ResponseRecorder {
		secret := mintKey(t, srv, who, who+"-key", nil, false)
		req := httptest.NewRequest(http.MethodGet, "/apps/payroll/", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := key("lister"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "deny_list_only") {
		t.Errorf("a list holder's key = %d %s, want 403 deny_list_only", rec.Code, rec.Body)
	}
	if rec := key("stranger"); rec.Code != http.StatusNotFound {
		t.Errorf("a stranger's key = %d, want 404", rec.Code)
	}
}

// The list holder's card is a gateway page like the others: its inline style
// is named by hash in its own policy, never 'unsafe-inline'.
func TestTheListHolderCardsPolicyNamesItsStyle(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/apps/payroll/", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()
	s.serveGatewayListOnly(rec, req, &database.App{ID: "a1", Slug: "payroll", Name: "Pay <roll>"})
	csp := rec.Header().Get("Content-Security-Policy")
	for _, b := range inlineBlocks(rec.Body.String(), "style") {
		sum := sha256.Sum256([]byte(b))
		if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, want) {
			t.Errorf("the served <style> hashes to %s, which the policy does not name: %s", want, csp)
		}
	}
	if strings.Contains(rec.Body.String(), "<roll>") {
		t.Error("the app's name was not escaped")
	}
}
