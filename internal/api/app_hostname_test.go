package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/password"
)

// An app on a hostname of its own. The first two tests here are the ones
// whose failure is silent: every page still renders, and an isolation is
// quietly gone.

func strPtr(s string) *string { return &s }

// hostedApp is an app with a backend of its own, its hostname recorded and
// switched on, and its release flipped to basePath.
func hostedApp(t *testing.T, srv *Server, slug, hostname, basePath, body string) *database.App {
	t.Helper()
	app, _ := hostedAppBackend(t, srv, slug, hostname, basePath, body)
	return app
}

// hostedAppBackend is hostedApp handing back the backend too, for a test that
// reads what the app was sent.
func hostedAppBackend(t *testing.T, srv *Server, slug, hostname, basePath, body string) (*database.App, *backend) {
	t.Helper()
	be := newBackend(t, nil)
	be.body = body
	if err := srv.db.UpsertUser(&database.User{ID: "owner-" + slug, Login: "owner-" + slug, SystemRole: database.SystemRolePublisher}); err != nil {
		t.Fatal(err)
	}
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: "owner-" + slug, Framework: "flask",
		Port: database.Port(be.port), CurrentRelease: slug + "-r1",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	app.RunningMode = "shared"
	app.ReleaseBasePath = strPtr(basePath)
	if err := srv.db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}
	if hostname != "" {
		if err := srv.db.SetAppHostname(app.ID, "", false, hostname, true, "system", "", database.RequestMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := srv.db.GetApp(app.ID)
	return got, be
}

func onHost(t *testing.T, srv *Server, method, host, path string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "https://"+host+path, nil)
	req.Host = host
	req.RemoteAddr = "127.0.0.1:34567"
	if mutate != nil {
		mutate(req)
	}
	rr := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rr, req)
	return rr
}

// Every path on an app host is that app's. /apps/<another> on
// reports.example.com reaches reports — broken, a neighbour runs on this
// origin and every page still renders.
func TestAnAppHostServesOnlyItsApp(t *testing.T) {
	srv := twoHostServer(t)
	hostedApp(t, srv, "reports", "reports.example.com", "", "reports")
	hostedApp(t, srv, "payroll", "", "/apps/payroll", "payroll")

	for _, path := range []string{"/", "/apps/payroll/", "/apps/reports/", "/anything"} {
		rr := onHost(t, srv, http.MethodGet, "reports.example.com", path, nil)
		if rr.Code != http.StatusOK || rr.Body.String() != "reports" {
			t.Errorf("GET reports.example.com%s = %d %q, want reports' own 200", path, rr.Code, rr.Body.String())
		}
	}
	// The neighbour is still on the shared origin, untouched.
	if rr := onHost(t, srv, http.MethodGet, "apps.example.com", "/apps/payroll/", nil); rr.Body.String() != "payroll" {
		t.Errorf("payroll on the apps origin = %d %q", rr.Code, rr.Body.String())
	}
	// A host nobody configured falls through to the gateway as today.
	if rr := onHost(t, srv, http.MethodGet, "stranger.example.org", "/apps/payroll/", nil); rr.Body.String() != "payroll" {
		t.Errorf("an unconfigured host = %d %q, want today's gateway", rr.Code, rr.Body.String())
	}
}

// On an app host every path is the app's, /__api__ included: the platform
// router's rsconnect rewrite runs only after the app host is resolved, so the
// app gets its own path and its own Authorization header (security review
// 2026-11-14, SI-3).
func TestAppHostOwnsTheConnectAPIPath(t *testing.T) {
	srv := twoHostServer(t)
	_, be := hostedAppBackend(t, srv, "reports", "reports.example.com", "", "reports")

	req := httptest.NewRequest(http.MethodGet, "https://reports.example.com/__api__/v1/x", nil)
	req.Host = "reports.example.com"
	req.RemoteAddr = "127.0.0.1:34567"
	req.Header.Set("Authorization", "Key k")
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "reports" {
		t.Fatalf("/__api__ on the app host through the platform router = %d %q, want the app's 200", rr.Code, rr.Body.String())
	}
	if be.lastReq == nil {
		t.Fatal("the app received nothing")
	}
	if got := be.lastReq.URL.Path; got != "/__api__/v1/x" {
		t.Errorf("the app was sent %q, want /__api__/v1/x", got)
	}
	if got := be.lastReq.Header.Get("Authorization"); got != "Key k" {
		t.Errorf("the app was sent Authorization %q, want it unchanged", got)
	}
}

// App hosts are redirect targets, never allowed origins. A write to /api/v1
// from an app host's page is refused, and the same origin is somewhere a
// sign-in may return to. Broken, every page of the app acts on the platform as
// its viewer, and nothing fails.
func TestAppHostsAreNeverAllowedOrigins(t *testing.T) {
	srv := twoHostServer(t)
	hostedApp(t, srv, "reports", "reports.example.com", "", "reports")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://reports.example.com")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:34567"
	req.AddCookie(sessionCookieFor(t, srv, database.BuiltinAdminID))
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("POST /api/v1/apps from an app host's origin = %d, want 403", rr.Code)
	}
	if srv.allowedOrigins(req)["https://reports.example.com"] {
		t.Error("an app host is in the platform's allowed origins")
	}
	if got := srv.safeRedirect(req, "https://reports.example.com/x"); got == "" {
		t.Error("safeRedirect refused a served app host; a sign-in could not return to it")
	}
	if got := srv.safeRedirect(req, "https://unknown.example.com/x"); got != "" {
		t.Errorf("safeRedirect accepted a host that is no app's: %q", got)
	}
}

// The old path answers 308 to the canonical origin, path remainder and query
// kept — and only once the release has moved. Before the flip the old path
// keeps serving the old release and the app host says the app is moving.
func TestTheOldPathRedirectsFromTheFlip(t *testing.T) {
	srv := twoHostServer(t)
	moved := hostedApp(t, srv, "moved", "moved.example.com", "", "moved")
	rr := onHost(t, srv, http.MethodPost, "apps.example.com", "/apps/moved/x/y?z=1", nil)
	if rr.Code != http.StatusPermanentRedirect || rr.Header().Get("Location") != "https://moved.example.com/x/y?z=1" {
		t.Errorf("/apps/moved/x/y?z=1 = %d %q, want 308 to https://moved.example.com/x/y?z=1",
			rr.Code, rr.Header().Get("Location"))
	}
	_ = moved

	hostedApp(t, srv, "moving", "moving.example.com", "/apps/moving", "moving")
	if rr := onHost(t, srv, http.MethodGet, "apps.example.com", "/apps/moving/", nil); rr.Code != http.StatusOK || rr.Body.String() != "moving" {
		t.Errorf("mid-move, the old path = %d %q; it must keep serving until the flip", rr.Code, rr.Body.String())
	}
	if rr := onHost(t, srv, http.MethodGet, "moving.example.com", "/", nil); rr.Header().Get(interstitialHeader) != stateMoving.Key {
		t.Errorf("mid-move, the app host = %d %q, want the moving card", rr.Code, rr.Header().Get(interstitialHeader))
	}

	// Mid-move off: the name is switched off and the release is still at the
	// root, so the host keeps serving it until the flip.
	off := hostedApp(t, srv, "leaving", "leaving.example.com", "", "leaving")
	if err := srv.db.SetAppHostname(off.ID, off.Hostname, true, off.Hostname, false, "system", "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if rr := onHost(t, srv, http.MethodGet, "leaving.example.com", "/", nil); rr.Body.String() != "leaving" {
		t.Errorf("mid-move off, the app host = %d %q; it serves until the flip", rr.Code, rr.Body.String())
	}
}

// The move back off a cleared hostname is a card that names the app, so it is
// served past authorization: an anonymous caller of a private app gets the
// sign-in answer and no card (security review 2026-11-15, HD-6).
func TestTheOldPathCardForAMoveBackIsBehindAuthorization(t *testing.T) {
	srv := twoHostServer(t)
	app := hostedApp(t, srv, "backhome", "", "", "backhome")
	if err := srv.db.SetAppVisibility(app.ID, database.VisibilityPublic, database.VisibilityPrivate, "system", "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rr := onHost(t, srv, http.MethodGet, "apps.example.com", "/apps/backhome/", nil)
	if rr.Header().Get(interstitialHeader) == stateMoving.Key || strings.Contains(rr.Body.String(), app.ID) {
		t.Fatalf("anonymous: %d %q, the moving card was served before authorization", rr.Code, rr.Header().Get(interstitialHeader))
	}
	if rr.Code == http.StatusOK {
		t.Errorf("anonymous on a private app = 200, want the sign-in answer")
	}
	// A session minted for the apps origin, as the handshake mints it.
	sess, err := srv.jwt.Generate(auth.GenerateParams{
		UserID: "owner-backhome", Login: "owner-backhome", SystemRole: database.SystemRolePublisher,
		Policy: srv.sessionPolicy(), Audience: auth.AppsAudience("https://apps.example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rr = onHost(t, srv, http.MethodGet, "apps.example.com", "/apps/backhome/", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: appSessionCookie, Value: sess.Token})
	})
	if rr.Header().Get(interstitialHeader) != stateMoving.Key {
		t.Errorf("the owner = %d %q, want the moving card", rr.Code, rr.Header().Get(interstitialHeader))
	}
}

// On a two-listener install the platform origin sends an old /apps/ link to
// the apps origin with its path as sent, not as decoded (HD-9).
func TestTheOldPathRedirectKeepsTheEscapedPath(t *testing.T) {
	srv := twoHostServer(t)
	hostedApp(t, srv, "esc", "", "/apps/esc", "esc")
	req := httptest.NewRequest(http.MethodGet, "https://lp.example.com/apps/esc/a%3Fb?z=1", nil)
	req.Host = "lp.example.com"
	req.RemoteAddr = "127.0.0.1:34567"
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)
	if want := "https://apps.example.com/apps/esc/a%3Fb?z=1"; rr.Code != http.StatusTemporaryRedirect || rr.Header().Get("Location") != want {
		t.Errorf("GET /apps/esc/a%%3Fb?z=1 = %d %q, want 307 to %s", rr.Code, rr.Header().Get("Location"), want)
	}
}

// A grant is bound to the origin it was minted for: one for reports is dead
// at payroll's host and at the apps origin.
func TestAGrantForOneAppHostIsDeadAtAnother(t *testing.T) {
	srv := twoHostServer(t)
	hostedApp(t, srv, "reports", "reports.example.com", "", "reports")
	hostedApp(t, srv, "payroll", "payroll.example.com", "", "payroll")
	tv, _ := srv.db.GetTokenVersion(database.BuiltinAdminID)
	now := time.Now().UTC()
	grant := srv.jwt.MintAppGrant(database.BuiltinAdminID, "https://reports.example.com", tv, now, testGrantNonce, now)

	for _, host := range []string{"payroll.example.com", "apps.example.com"} {
		ret := url.QueryEscape("https://" + host + "/")
		rr := onHost(t, srv, http.MethodGet, host, appHandshakePath+"?grant="+url.QueryEscape(grant)+"&redirect="+ret, nil)
		if rr.Code != http.StatusForbidden {
			t.Errorf("reports' grant at %s = %d, want 403", host, rr.Code)
		}
	}

	// At its own host it is redeemed, as the host's own cookie: __Host- at
	// Path=/, which no sibling can plant.
	ret := url.QueryEscape("https://reports.example.com/")
	rr := onHost(t, srv, http.MethodGet, "reports.example.com", appHandshakePath+"?grant="+url.QueryEscape(grant)+"&redirect="+ret,
		func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "__Host-" + appGrantNonceCookie, Value: testGrantNonce})
		})
	if rr.Code != http.StatusFound {
		t.Fatalf("reports' grant at its own host = %d %s", rr.Code, rr.Body.String())
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "__Host-"+appSessionCookie {
			session = c
		}
	}
	if session == nil || session.Path != "/" || !session.Secure {
		t.Fatalf("app host session cookie = %+v, want __Host-%s at Path=/ and Secure", session, appSessionCookie)
	}

	// And it is a session there: the next request is the admin's.
	req := httptest.NewRequest(http.MethodGet, "https://reports.example.com/", nil)
	req.Host = "reports.example.com"
	req.AddCookie(&http.Cookie{Name: session.Name, Value: session.Value})
	req = req.WithContext(withAppHost(req, "reports.example.com"))
	if srv.appSessionToken(req) == "" {
		t.Error("the app host's own cookie is not read as its session")
	}
	// Two copies is no session, as on the apps origin.
	req.AddCookie(&http.Cookie{Name: session.Name, Value: "planted"})
	if srv.appSessionToken(req) != "" {
		t.Error("two copies of the app host's cookie were read as a session")
	}
}

// A page on the shared apps origin, or on another app's host, cannot drive an
// app on its own host; the host's own origin and the platform's can.
func TestAnAppHostAdmitsOnlyItsOwnOriginAndThePlatforms(t *testing.T) {
	srv := twoHostServer(t)
	hostedApp(t, srv, "reports", "reports.example.com", "", "reports")
	for origin, want := range map[string]int{
		"https://reports.example.com": http.StatusOK,
		"https://lp.example.com":      http.StatusOK,
		"https://apps.example.com":    http.StatusForbidden,
		"https://payroll.example.com": http.StatusForbidden,
	} {
		rr := onHost(t, srv, http.MethodPost, "reports.example.com", "/submit", func(r *http.Request) {
			r.Header.Set("Origin", origin)
		})
		if rr.Code != want {
			t.Errorf("POST from %s = %d, want %d", origin, rr.Code, want)
		}
	}
}

// Assigning a hostname is an administrator's decision, and the validator's
// refusals reach the caller by name.
func TestOnlyAnAdminMayGiveAnAppAHostname(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	if rec := patchAppAs(t, srv, router, "owner", app.ID, `{"hostname":"demo.example.org"}`); rec.Code != http.StatusForbidden {
		t.Errorf("owner PATCH hostname = %d, want 403", rec.Code)
	}
	if rec := patchAppAs(t, srv, router, "owner", app.ID, `{"hostname_enabled":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("owner PATCH hostname_enabled = %d, want 403", rec.Code)
	}
	if rec := patchAppAs(t, srv, router, "root", app.ID, `{"hostname_enabled":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("switching on with no name = %d, want 400", rec.Code)
	}
	// localhost:8080 is the platform's; a child of it is refused by name.
	if rec := patchAppAs(t, srv, router, "root", app.ID, `{"hostname":"x.localhost"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "invalid_hostname") {
		t.Errorf("a child of the platform's host = %d %s, want 400 invalid_hostname", rec.Code, rec.Body.String())
	}
	if rec := patchAppAs(t, srv, router, "root", app.ID, `{"hostname":"Demo.Example.org","hostname_enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("admin PATCH hostname = %d %s", rec.Code, rec.Body.String())
	}
	got, _ := srv.db.GetApp(app.ID)
	if got.Hostname != "demo.example.org" || !got.HostnameEnabled {
		t.Errorf("stored %q enabled=%v", got.Hostname, got.HostnameEnabled)
	}

	// Taken, whether or not the holder serves it.
	other := &database.App{Slug: "other", Name: "Other", OwnerID: "owner", Status: database.AppStatusStopped}
	if err := srv.db.CreateAppTx(other, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if rec := patchAppAs(t, srv, router, "root", other.ID, `{"hostname":"demo.example.org"}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "hostname_taken") {
		t.Errorf("a taken name = %d %s, want 409 hostname_taken", rec.Code, rec.Body.String())
	}
}

// Serving reads the release, never the wish. A renamed Next.js app is reached
// at its new path and handed the path its release was built for, until its
// move flips; the slug alone would forward /apps/new/… to a tree built for
// /apps/old, which is the 404 a rename used to leave behind.
func TestTheProxyForwardsTheReleasesPrefix(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)
	if err := srv.db.UpsertUser(&database.User{ID: "u1", Login: "u1", SystemRole: database.SystemRolePublisher}); err != nil {
		t.Fatal(err)
	}
	app := &database.App{
		Slug: "renamed", Name: "Renamed", OwnerID: "u1", Framework: "nextjs",
		Port: database.Port(be.port), CurrentRelease: "renamed-r1",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	app.RunningMode = "shared"
	app.ReleaseBasePath = strPtr("/apps/before")
	if err := srv.db.SetAppRelease(app); err != nil {
		t.Fatal(err)
	}
	rec := proxyServe(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/renamed/page?q=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := be.lastReq.URL.Path; got != "/apps/before/page" {
		t.Errorf("upstream path = %q, want the release's /apps/before/page", got)
	}
	if got := be.lastReq.Header.Get("X-Forwarded-Prefix"); got != "/apps/before" {
		t.Errorf("X-Forwarded-Prefix = %q, want /apps/before", got)
	}
}

// A sibling app host can toss `Domain=<parent>; toft_session=…` onto the
// platform's host. Once any app has a hostname recorded, a browser's platform
// session is the __Host- cookie alone, as it is on a two-host install; with
// none, a single-origin install keeps the plain cookie it always had.
func TestARecordedHostnameMovesThePlatformToTheHostCookie(t *testing.T) {
	db := dbtest.New(t, database.WithBootstrapAdminPassword(password.Default))
	srv := newTestServerWithConfig(t, db, &config.Config{BaseURL: "https://toft.example.com", AppsBaseURL: "https://toft.example.com"})
	if srv.cfg.TwoHostMode() {
		t.Fatal("fixture is meant to be single-origin")
	}
	if srv.hostSessionCookie() {
		t.Fatal("a single-origin install with no app hostname uses the host cookie")
	}

	app := hostedApp(t, srv, "reports", "", "/apps/reports", "reports")
	if err := srv.db.SetAppHostname(app.ID, "", false, "reports.example.com", false, "system", "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	srv.hostnamesSeen.Store(nil)
	if !srv.hostSessionCookie() {
		t.Fatal("a recorded hostname left the platform on the plain, tossable cookie")
	}

	// A browser presenting only the plain name — the one a sibling can set — is nobody.
	req := httptest.NewRequest(http.MethodGet, "https://toft.example.com/api/v1/me", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "tossed"})
	if got := srv.sessionToken(req); got != "" {
		t.Errorf("a browser's plain cookie was read as its session: %q", got)
	}
	// And a sign-in now sets the host cookie.
	rr := httptest.NewRecorder()
	srv.setSessionCookies(rr, auth.Session{Token: "t", ExpiresAt: time.Now().Add(time.Hour)})
	found := false
	for _, c := range rr.Result().Cookies() {
		found = found || c.Name == auth.HostSessionCookie
	}
	if !found {
		t.Error("a sign-in did not set the __Host- session cookie")
	}
}
