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

// The two-host handshake, at the handler level.
//
// internal/auth/appgrant_test.go proves the credential is bound and signed;
// these prove the endpoints refuse everything they should, which is the half a
// forged blob would attack.

func twoHostServer(t *testing.T) *Server {
	t.Helper()
	db := dbtest.New(t, database.WithBootstrapAdminPassword(password.Default))
	return newTestServerWithConfig(t, db, &config.Config{
		BaseURL:     "https://lp.example.com",
		AppsBaseURL: "https://apps.example.com",
		Port:        8080,
		AppsPort:    8081,
	})
}

// appsGet drives a GET through the APPS router, which is a different router
// from the platform's — calling the redeem handler directly would test a
// handler no route can reach.
//
// It carries the nonce cookie a gateway would have left (testGrantNonce), so a
// grant minted with that nonce is this browser's; appsGetWith sends exactly
// the cookies it is given.
func appsGet(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return appsGetWith(t, srv, path, &http.Cookie{Name: appGrantNonceCookie, Value: testGrantNonce})
}

// testGrantNonce is the nonce appsGet's browser holds.
const testGrantNonce = "test-grant-nonce-0123456789"

func appsGetWith(t *testing.T, srv *Server, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://apps.example.com"+path, nil)
	req.Host = "apps.example.com"
	req.RemoteAddr = "127.0.0.1:34567"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rr, req)
	return rr
}

func TestTwoHostModeIsDetected(t *testing.T) {
	srv := twoHostServer(t)
	if !srv.cfg.TwoHostMode() {
		t.Fatal("two distinct hostnames did not produce two-host mode")
	}
}

// The redeem endpoint is the only thing on the apps origin that is not an app,
// so it has to exist there and nowhere else.
func TestRedeemPathIsReservedOnTheAppsOriginOnly(t *testing.T) {
	srv := twoHostServer(t)

	if rr := appsGet(t, srv, appHandshakePath); rr.Code == http.StatusNotFound {
		t.Error("the handshake path is not routed on the apps origin")
	}
	// And is not a platform route: the platform origin has a session already.
	if rr := getThroughRouter(t, srv, appHandshakePath); rr.Code != http.StatusNotFound {
		t.Errorf("the handshake path answered on the platform origin with %d", rr.Code)
	}
}

// A grant is a bearer credential in a URL. Everything that is not a grant this
// install minted, for this origin, right now, must be refused — and refused
// rather than redirected, because a redirect on a bad grant is a loop.
func TestRedeemRefusesBadGrants(t *testing.T) {
	srv := twoHostServer(t)
	ret := url.QueryEscape("https://apps.example.com/apps/demo/")

	cases := map[string]string{
		"no grant at all":  "",
		"garbage":          "not-a-grant",
		"truncated":        "g1|user|https://apps.example.com|1",
		"forged signature": "g1|user-1|https://apps.example.com|1|99999999999|AAAA",
	}
	for name, grant := range cases {
		t.Run(name, func(t *testing.T) {
			rr := appsGet(t, srv, appHandshakePath+"?grant="+url.QueryEscape(grant)+"&redirect="+ret)
			if rr.Code != http.StatusForbidden {
				t.Errorf("got %d, want 403 (body %q)", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if c := rr.Result().Cookies(); len(c) > 0 {
				t.Errorf("a refused grant still set %d cookie(s)", len(c))
			}
		})
	}
}

// A grant minted for one origin must not establish a session at another — most
// of all at the platform's own, which is the boundary being defended.
func TestRedeemRefusesAGrantForAnotherOrigin(t *testing.T) {
	srv := twoHostServer(t)
	grant := srv.jwt.MintAppGrant("builtin-admin", "https://lp.example.com", 0, time.Now().UTC(), testGrantNonce, time.Now().UTC())

	rr := appsGet(t, srv, appHandshakePath+"?grant="+url.QueryEscape(grant)+
		"&redirect="+url.QueryEscape("https://apps.example.com/apps/demo/"))
	if rr.Code != http.StatusForbidden {
		t.Errorf("a grant bound to the platform origin was accepted at the apps origin: %d", rr.Code)
	}
}

// The redirect is attacker-controlled and travels beside a credential. An
// unvalidated one would make this an open redirect that hands over a session.
func TestHandshakeRefusesForeignRedirects(t *testing.T) {
	srv := twoHostServer(t)
	grant := srv.jwt.MintAppGrant("builtin-admin", "https://apps.example.com", 0, time.Now().UTC(), testGrantNonce, time.Now().UTC())

	for _, bad := range []string{
		"https://evil.example.com/",
		"//evil.example.com/",
		`/\evil.example.com`,
	} {
		rr := appsGet(t, srv, appHandshakePath+"?grant="+url.QueryEscape(grant)+
			"&redirect="+url.QueryEscape(bad))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("redeem accepted redirect %q with %d", bad, rr.Code)
		}
		rr = getThroughRouter(t, srv, "/api/v1/auth/app-grant?redirect="+url.QueryEscape(bad))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("grant accepted redirect %q with %d", bad, rr.Code)
		}
	}
}

// A grant carries the token version it was cut from, and the redeemer checks it
// live. A sign-out, demotion, group change or lock between mint and redeem has
// to land here — the signature alone cannot know about it.
func TestRedeemRefusesARevokedSession(t *testing.T) {
	srv := twoHostServer(t)

	tv, err := srv.db.GetTokenVersion(database.BuiltinAdminID)
	if err != nil {
		t.Fatal(err)
	}
	grant := srv.jwt.MintAppGrant(database.BuiltinAdminID, "https://apps.example.com", tv, time.Now().UTC(), testGrantNonce, time.Now().UTC())

	// Anything that revokes a session bumps this counter.
	if err := srv.db.BumpTokenVersion(database.BuiltinAdminID); err != nil {
		t.Fatal(err)
	}

	rr := appsGet(t, srv, appHandshakePath+"?grant="+url.QueryEscape(grant)+
		"&redirect="+url.QueryEscape("https://apps.example.com/apps/demo/"))
	if rr.Code != http.StatusForbidden {
		t.Errorf("a grant from a revoked session was redeemed: %d", rr.Code)
	}
}

// The happy path: a valid grant becomes a cookie on THIS origin and nothing
// else. In particular not the platform's API cookie, which has no business
// existing on an origin that never serves /api/v1.
func TestRedeemSetsOnlyTheAppSessionCookie(t *testing.T) {
	srv := twoHostServer(t)

	tv, err := srv.db.GetTokenVersion(database.BuiltinAdminID)
	if err != nil {
		t.Fatal(err)
	}
	grant := srv.jwt.MintAppGrant(database.BuiltinAdminID, "https://apps.example.com", tv, time.Now().UTC(), testGrantNonce, time.Now().UTC())
	target := "https://apps.example.com/apps/demo/"

	rr := appsGet(t, srv, appHandshakePath+"?grant="+url.QueryEscape(grant)+
		"&redirect="+url.QueryEscape(target))
	if rr.Code != http.StatusFound {
		t.Fatalf("got %d, want 302 (body %q)", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if loc := rr.Header().Get("Location"); loc != target {
		t.Errorf("redirected to %q, want %q", loc, target)
	}
	// The grant travelled in a URL; nothing downstream should learn it.
	if rr.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("redeem did not set Referrer-Policy: no-referrer")
	}

	// The session and the handshake marker, and nothing else — in particular
	// not the platform's API cookie.
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		switch c.Name {
		case appSessionCookie:
			session = c
		case appsSeenCookie:
		case appGrantNonceCookie:
			if c.MaxAge >= 0 {
				t.Errorf("the nonce cookie was set again (MaxAge %d), not expired", c.MaxAge)
			}
		default:
			t.Errorf("set %q, which has no business on this origin", c.Name)
		}
	}
	if session == nil {
		t.Fatalf("the redeem set no %s cookie", appSessionCookie)
	}
	if session.Domain != "" {
		t.Errorf("cookie carries Domain=%q — it must be host-only, or the session spans the zone", session.Domain)
	}
	if !session.HttpOnly {
		t.Error("app session cookie is not HttpOnly")
	}
}

// A public app on two hosts identifies a signed-in visitor once per browser.
// The platform's cookie is host-only and never reaches the apps origin, so
// without one hop a colleague who signed in on the platform was anonymous to
// every public app for the life of their browser, while the single-origin
// install identified them. The hop happens on the first browser navigation
// with no gateway session and no marker, and the marker means not again for
// as long as its answer holds (TestTheHandshakeMarkerLastsAsLongAsItsAnswer).
func TestAPublicAppOnTwoHostsIdentifiesOncePerBrowser(t *testing.T) {
	srv := twoHostServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "demo", Name: "Demo", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	target := "https://apps.example.com/apps/demo/"

	navigate := func(cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Host = "apps.example.com"
		req.RemoteAddr = "127.0.0.1:34567"
		req.Header.Set("Accept", "text/html")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rr := httptest.NewRecorder()
		srv.AppsRouter().ServeHTTP(rr, req)
		return rr
	}

	// First visit: through the grant URL, marked public, with this browser's
	// nonce left behind for the redeem.
	rr := navigate()
	if rr.Code != http.StatusFound {
		t.Fatalf("first navigation = %d, want a redirect through the handshake", rr.Code)
	}
	var nonce *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == appGrantNonceCookie {
			nonce = c
		}
	}
	if nonce == nil || nonce.Path != appHandshakePath || !nonce.HttpOnly {
		t.Fatalf("the gateway left nonce cookie %+v, want one HttpOnly at %s", nonce, appHandshakePath)
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), "https://lp.example.com/api/v1/auth/app-grant?") || loc.Query().Get("public") != "1" {
		t.Fatalf("redirected to %q, want the grant URL marked public", loc)
	}

	// Anonymous on the platform: back to the apps origin with no grant.
	rr = getThroughRouter(t, srv, loc.RequestURI())
	if rr.Code != http.StatusFound {
		t.Fatalf("grant for an anonymous public visitor = %d, want 302 back to the app, not a login page", rr.Code)
	}
	back, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Host != "apps.example.com" || back.Path != appHandshakePath || back.Query().Get("anonymous") != "1" {
		t.Fatalf("anonymous grant redirected to %q, want the redeem path marked anonymous", back)
	}

	// The redeem sets the marker, mints nothing, and sends them on.
	rr = appsGet(t, srv, back.RequestURI())
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != target {
		t.Fatalf("anonymous redeem = %d %q, want 302 to the app", rr.Code, rr.Header().Get("Location"))
	}
	var marker *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == appSessionCookie {
			t.Fatal("an anonymous visitor was minted a session")
		}
		if c.Name == appsSeenCookie {
			marker = c
		}
	}
	if marker == nil {
		t.Fatal("the anonymous redeem set no marker; the visitor would loop forever")
	}

	// With the marker, the app is served and nobody is redirected again.
	rr = navigate(marker)
	if rr.Code == http.StatusFound && strings.Contains(rr.Header().Get("Location"), "app-grant") {
		t.Fatalf("a marked browser was sent through the handshake again")
	}

	// A signed-in person on the platform: the grant carries them across and
	// the redeem sets both the session and the marker.
	rr = getThroughRouterAs(t, srv, "owner", loc.RequestURI())
	if rr.Code != http.StatusFound {
		t.Fatalf("grant for a signed-in visitor = %d", rr.Code)
	}
	redeem, _ := url.Parse(rr.Header().Get("Location"))
	if redeem.Query().Get("grant") == "" {
		t.Fatalf("a signed-in visitor's grant URL carries no grant: %q", redeem)
	}
	rr = appsGetWith(t, srv, redeem.RequestURI(), &http.Cookie{Name: nonce.Name, Value: nonce.Value})
	names := map[string]bool{}
	for _, c := range rr.Result().Cookies() {
		names[c.Name] = true
	}
	if !names[appSessionCookie] || !names[appsSeenCookie] {
		t.Fatalf("signed-in redeem set %v, want the session and the marker", names)
	}
}

// BB-69: the marker suppresses the next hop, so it may not outlive the answer
// it records. "Nobody is signed in" lasts a minute — a colleague who opened a
// public app before signing in was otherwise anonymous on every public app for
// 400 days — and a real grant's marker dies with the app session it minted, so
// an expired session is asked for again.
func TestTheHandshakeMarkerLastsAsLongAsItsAnswer(t *testing.T) {
	srv := twoHostServer(t)
	target := "https://apps.example.com/apps/demo/"

	rr := appsGet(t, srv, appHandshakePath+"?anonymous=1&redirect="+url.QueryEscape(target))
	var anon *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == appsSeenCookie {
			anon = c
		}
	}
	if anon == nil || anon.MaxAge <= 0 || anon.MaxAge > appsSeenAnonymousMaxAge {
		t.Fatalf("the anonymous answer's marker = %+v, want MaxAge in (0, %d]", anon, appsSeenAnonymousMaxAge)
	}

	tv, err := srv.db.GetTokenVersion(database.BuiltinAdminID)
	if err != nil {
		t.Fatal(err)
	}
	grant := srv.jwt.MintAppGrant(database.BuiltinAdminID, "https://apps.example.com", tv, time.Now().UTC(), testGrantNonce, time.Now().UTC())
	rr = appsGet(t, srv, appHandshakePath+"?grant="+url.QueryEscape(grant)+"&redirect="+url.QueryEscape(target))
	var session, marker *http.Cookie
	for _, c := range rr.Result().Cookies() {
		switch c.Name {
		case appSessionCookie:
			session = c
		case appsSeenCookie:
			marker = c
		}
	}
	if session == nil || marker == nil {
		t.Fatalf("redeem set session=%v marker=%v", session, marker)
	}
	if marker.MaxAge <= 0 || marker.MaxAge > session.MaxAge {
		t.Errorf("the grant's marker lives %d s, the session it minted %d s — it must not outlive it",
			marker.MaxAge, session.MaxAge)
	}
}

// A client that keeps no cookies and asks for HTML — a crawler, a link
// unfurler, an uptime probe — is served, not sent round a handshake whose only
// exit is a cookie it will never return (BB-41, BB-56).
func TestAPublicAppServesACookielessHTMLClient(t *testing.T) {
	srv := twoHostServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "demo", Name: "Demo", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	for _, accept := range []string{"text/html", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"} {
		req := httptest.NewRequest(http.MethodGet, "https://apps.example.com/apps/demo/", nil)
		req.Host = "apps.example.com"
		req.RemoteAddr = "127.0.0.1:34567"
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
		rr := httptest.NewRecorder()
		srv.AppsRouter().ServeHTTP(rr, req)
		if loc := rr.Header().Get("Location"); strings.Contains(loc, "app-grant") || strings.Contains(loc, appHandshakePath) {
			t.Errorf("Accept %q: a cookieless client was sent into the handshake (%d → %s); it can never leave it", accept, rr.Code, loc)
		}
	}
}

func getThroughRouterAs(t *testing.T, srv *Server, who, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:34567"
	req.AddCookie(sessionCookieFor(t, srv, who))
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)
	return rr
}

// Not signed in yet: the grant endpoint sends the viewer through the normal
// login and back to itself, so the handshake resumes rather than dropping them
// somewhere unrelated.
func TestGrantSendsAnAnonymousViewerToLoginAndBack(t *testing.T) {
	srv := twoHostServer(t)
	target := "https://apps.example.com/apps/demo/"

	rr := getThroughRouter(t, srv, "/api/v1/auth/app-grant?redirect="+url.QueryEscape(target)+"&nonce="+testGrantNonce)
	if rr.Code != http.StatusFound {
		t.Fatalf("got %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/login?redirect=") {
		t.Fatalf("did not send to login: %q", loc)
	}
	// And the login's own redirect must come back to the grant endpoint, or the
	// person signs in and lands on a dashboard wondering what happened.
	if !strings.Contains(loc, url.QueryEscape("/api/v1/auth/app-grant")) {
		t.Errorf("login redirect does not resume the handshake: %q", loc)
	}
	// With the browser's nonce, or the grant minted after sign-in could not
	// be redeemed by the browser that asked for it.
	if !strings.Contains(loc, url.QueryEscape("nonce="+testGrantNonce)) {
		t.Errorf("login redirect drops the nonce: %q", loc)
	}
}

// The handshake proves nothing new, so the apps-origin session carries the
// platform session's own auth_time rather than restarting the absolute
// ceiling from the moment of the hop — and it is not a login, so
// last_login_at is untouched. Before this the second origin ran on a fresh
// clock: signed in at 09:00, first app opened at 16:30, and the apps origin
// kept serving until 00:30 while the platform refused at 17:00.
func TestRedeemMintsFromThePlatformSessionsClock(t *testing.T) {
	srv := twoHostServer(t)

	tv, err := srv.db.GetTokenVersion(database.BuiltinAdminID)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	grant := srv.jwt.MintAppGrant(database.BuiltinAdminID, "https://apps.example.com", tv, start, testGrantNonce, time.Now().UTC())

	before, err := srv.db.GetUser(database.BuiltinAdminID)
	if err != nil {
		t.Fatal(err)
	}

	rr := appsGet(t, srv, appHandshakePath+"?grant="+url.QueryEscape(grant)+
		"&redirect="+url.QueryEscape("https://apps.example.com/apps/demo/"))
	if rr.Code != http.StatusFound {
		t.Fatalf("got %d, want 302 (body %q)", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == appSessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("the redeem set no session cookie")
	}
	// The apps-origin session is this origin's alone, never a platform
	// session nor another origin's (security review 2026-11-10, D-1).
	if _, err := srv.jwt.Validate(session.Value); err == nil {
		t.Error("the apps-origin session validates as a platform session")
	}
	if _, err := srv.jwt.ValidateFor(session.Value, auth.AppsAudience("https://reports.example.com")); err == nil {
		t.Error("the apps-origin session validates for another origin")
	}
	claims, err := srv.jwt.ValidateFor(session.Value, auth.AppsAudience("https://apps.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if !claims.SessionStart().Equal(start) {
		t.Errorf("apps-origin auth_time = %v, want the platform session's %v", claims.SessionStart(), start)
	}

	after, err := srv.db.GetUser(database.BuiltinAdminID)
	if err != nil {
		t.Fatal(err)
	}
	if (before.LastLoginAt == nil) != (after.LastLoginAt == nil) ||
		(before.LastLoginAt != nil && !before.LastLoginAt.Equal(*after.LastLoginAt)) {
		t.Errorf("the handshake stamped last_login_at (%v -> %v) — it is not a login", before.LastLoginAt, after.LastLoginAt)
	}
}

// And the grant endpoint puts the platform session's start into the grant, so
// the two halves agree end to end.
func TestGrantCarriesThePlatformSessionsStart(t *testing.T) {
	srv := twoHostServer(t)
	target := "https://apps.example.com/apps/demo/"

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/app-grant?redirect="+url.QueryEscape(target)+"&nonce="+testGrantNonce, nil)
	req.RemoteAddr = "127.0.0.1:34567"
	cookie := sessionCookieFor(t, srv, database.BuiltinAdminID)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("grant = %d (body %q)", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	platformClaims, err := srv.jwt.Validate(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	g, err := srv.jwt.VerifyAppGrant(loc.Query().Get("grant"), "https://apps.example.com", time.Now().UTC())
	if err != nil {
		t.Fatalf("the grant the endpoint minted does not verify: %v", err)
	}
	if !g.SessionStart.Equal(platformClaims.SessionStart().Truncate(time.Second)) {
		t.Errorf("grant start = %v, want the platform session's %v", g.SessionStart, platformClaims.SessionStart())
	}
}

// The grant is tied to the browser that started the handshake (security
// review 2026-11-14, SI-2). A grant somebody minted for themselves and
// navigated into another browser finds no matching nonce there and sets
// nothing; a request to mint one without a nonce is refused.
func TestRedeemRequiresTheStartingBrowsersNonce(t *testing.T) {
	srv := twoHostServer(t)
	tv, err := srv.db.GetTokenVersion(database.BuiltinAdminID)
	if err != nil {
		t.Fatal(err)
	}
	grant := srv.jwt.MintAppGrant(database.BuiltinAdminID, "https://apps.example.com", tv, time.Now().UTC(), testGrantNonce, time.Now().UTC())
	path := appHandshakePath + "?grant=" + url.QueryEscape(grant) + "&redirect=" + url.QueryEscape("https://apps.example.com/apps/demo/")

	for name, cookies := range map[string][]*http.Cookie{
		"no nonce cookie": nil,
		"another nonce":   {{Name: appGrantNonceCookie, Value: "someone-elses-nonce-0000"}},
		"two copies": {{Name: appGrantNonceCookie, Value: testGrantNonce},
			{Name: appGrantNonceCookie, Value: testGrantNonce}},
	} {
		rr := appsGetWith(t, srv, path, cookies...)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: redeem = %d, want 403", name, rr.Code)
		}
		if c := rr.Result().Cookies(); len(c) > 0 {
			t.Errorf("%s: a refused redeem set %d cookie(s)", name, len(c))
		}
	}
	if rr := appsGet(t, srv, path); rr.Code != http.StatusFound {
		t.Errorf("with the nonce cookie, redeem = %d, want 302", rr.Code)
	}

	rr := getThroughRouterAs(t, srv, database.BuiltinAdminID,
		"/api/v1/auth/app-grant?redirect="+url.QueryEscape("https://apps.example.com/apps/demo/"))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a grant request with no nonce = %d, want 400", rr.Code)
	}
}

// One host: no handshake runs, so the redeem sets nothing whatever it is
// handed — a forged navigation would otherwise overwrite the gateway's
// session with a token it refuses.
func TestRedeemIsRefusedOnAOneHostInstall(t *testing.T) {
	db := dbtest.New(t, database.WithBootstrapAdminPassword(password.Default))
	srv := newTestServerWithConfig(t, db, &config.Config{
		BaseURL:     "https://lp.example.com",
		AppsBaseURL: "https://lp.example.com:8081",
		Port:        8080,
		AppsPort:    8081,
	})
	if srv.cfg.TwoHostMode() {
		t.Fatal("one host with two ports read as two-host mode")
	}
	tv, _ := srv.db.GetTokenVersion(database.BuiltinAdminID)
	grant := srv.jwt.MintAppGrant(database.BuiltinAdminID, "https://lp.example.com:8081", tv, time.Now().UTC(), testGrantNonce, time.Now().UTC())
	req := httptest.NewRequest(http.MethodGet, "https://lp.example.com:8081"+appHandshakePath+"?grant="+url.QueryEscape(grant)+
		"&redirect="+url.QueryEscape("https://lp.example.com:8081/apps/demo/"), nil)
	req.Host = "lp.example.com:8081"
	req.RemoteAddr = "127.0.0.1:34567"
	req.AddCookie(&http.Cookie{Name: appGrantNonceCookie, Value: testGrantNonce})
	rr := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rr, req)
	if rr.Code == http.StatusFound {
		t.Fatalf("a one-host redeem = %d, want refused", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == appSessionCookie {
			t.Fatal("a one-host redeem set the app session cookie")
		}
	}
}
