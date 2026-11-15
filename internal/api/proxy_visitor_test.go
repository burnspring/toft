package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The anonymous audience at the gateway (ADR §150). The
// first three tests are the ones that fail silently — the product would look
// right and be wrong.

func audienceOn(t *testing.T, srv *Server, on bool) {
	t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	if _, err := srv.db.PutSetting(database.SettingAudienceMeasurementEnabled, v, ""); err != nil {
		t.Fatal(err)
	}
}

func publicEchoApp(be *backend) *database.App {
	return &database.App{
		Slug: "echo", Name: "Echo", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
}

// gateway serves one request through the router a browser would reach: the
// platform's on a single-origin install, the apps router on two hosts (with
// the handshake's marker, so a page load is served rather than sent through
// the handshake). The record site lives in guardedAppProxy, which
// handleAppProxy alone would skip.
func gateway(t *testing.T, srv *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req.RemoteAddr = "127.0.0.1:34567"
	rr := httptest.NewRecorder()
	if srv.cfg.TwoHostMode() {
		req.Host = "apps.example.com"
		req.AddCookie(&http.Cookie{Name: appsSeenCookie, Value: "1"})
		srv.AppsRouter().ServeHTTP(rr, req)
		return rr
	}
	srv.Router(http.NotFoundHandler()).ServeHTTP(rr, req)
	return rr
}

// createApp is proxyTo's setup without its request.
func createApp(t *testing.T, srv *Server, app *database.App) {
	t.Helper()
	if err := srv.db.UpsertUser(&database.User{
		ID: app.OwnerID, Login: app.OwnerID, SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
}

// serveNew creates the app and serves its first request through gateway.
func serveNew(t *testing.T, srv *Server, app *database.App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	createApp(t, srv, app)
	return gateway(t, srv, req)
}

func pageLoad(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Accept", "text/html")
	return req
}

func visitorSet(rr *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if strings.TrimPrefix(c.Name, "__Host-") == visitorCookie {
			return c
		}
	}
	return nil
}

func validVisitor() string {
	return base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
}

func browsersToday(t *testing.T, srv *Server, appID string) *int64 {
	t.Helper()
	srv.usage.Flush()
	n, err := srv.db.AppAudienceTotal(appID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func pageLoadsToday(t *testing.T, srv *Server, appID string) int64 {
	t.Helper()
	srv.usage.Flush()
	tot, err := srv.db.GetAppUsageTotals(appID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return tot.PageLoads
}

// 1. The cookie never reaches the app, on either install shape (D6).
func TestVisitorCookieNeverReachesTheApp(t *testing.T) {
	for _, shape := range []struct {
		name string
		srv  func(t *testing.T) *Server
	}{
		{"single origin", newTestServer},
		{"two hosts", twoHostServer},
	} {
		t.Run(shape.name, func(t *testing.T) {
			srv := shape.srv(t)
			audienceOn(t, srv, true)
			be := newBackend(t, nil)
			req := httptest.NewRequest(http.MethodGet, "/apps/echo/style.css", nil)
			req.Header.Set("Cookie", "toft_visitor="+validVisitor()+"; __Host-toft_visitor="+validVisitor()+
				"; toft_apps_seen=1; app_pref=dark")
			serveNew(t, srv, publicEchoApp(be), req)
			if be.lastReq == nil {
				t.Fatal("backend was not called")
			}
			got := be.lastReq.Header.Get("Cookie")
			if strings.Contains(got, "visitor") || strings.Contains(got, validVisitor()) {
				t.Errorf("the visitor id reached the app: %q", got)
			}
			if !strings.Contains(got, "app_pref=dark") {
				t.Errorf("the app's own cookie was dropped: %q", got)
			}
		})
	}
}

// 2. A client with no cookie jar counts zero browsers, and every page load (D5).
func TestACookielessClientCountsZeroBrowsers(t *testing.T) {
	srv := newTestServer(t)
	audienceOn(t, srv, true)
	be := newBackend(t, nil)
	app := publicEchoApp(be)
	rr := serveNew(t, srv, app, pageLoad("/apps/echo/"))
	if visitorSet(rr) == nil {
		t.Fatal("an anonymous page load with the switch on was issued no id")
	}
	for i := 1; i < 1000; i++ {
		gateway(t, srv, pageLoad("/apps/echo/"))
	}
	if n := browsersToday(t, srv, app.ID); n != nil && *n != 0 {
		t.Errorf("anonymous browsers = %d, want 0 — counting at issue is a bot count", *n)
	}
	if n := pageLoadsToday(t, srv, app.ID); n != 1000 {
		t.Errorf("page loads = %d, want 1000", n)
	}
}

// 3. Switch off sets nothing, stores nothing, and expires a presented cookie (D3, D9).
func TestSwitchOffSetsNothingAndExpiresThePresentedCookie(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)
	app := publicEchoApp(be)

	rr := serveNew(t, srv, app, pageLoad("/apps/echo/"))
	if c := visitorSet(rr); c != nil {
		t.Fatalf("switch off issued %q", c.String())
	}
	req := pageLoad("/apps/echo/")
	req.AddCookie(&http.Cookie{Name: visitorCookie, Value: validVisitor()})
	rr = gateway(t, srv, req)
	c := visitorSet(rr)
	if c == nil || c.MaxAge >= 0 {
		t.Fatalf("a presented cookie with the switch off came back %v, want expired", c)
	}
	if n := browsersToday(t, srv, app.ID); n != nil {
		t.Errorf("switch off stored a sketch: %d", *n)
	}
}

// The round trip: issued on the page load, counted when it comes back on an
// asset, once however often it comes back.
func TestAReturningBrowserCountsOnce(t *testing.T) {
	srv := newTestServer(t)
	audienceOn(t, srv, true)
	be := newBackend(t, nil)
	app := publicEchoApp(be)

	c := visitorSet(serveNew(t, srv, app, pageLoad("/apps/echo/")))
	if c == nil {
		t.Fatal("no id issued")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != visitorMaxAge || c.Path != appSessionCookiePath {
		t.Errorf("cookie attributes = %+v", c)
	}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/apps/echo/style.css", nil)
		req.AddCookie(&http.Cookie{Name: visitorCookie, Value: c.Value})
		if again := visitorSet(gateway(t, srv, req)); again != nil {
			t.Fatalf("a presented cookie was reissued: %q", again.String())
		}
	}
	if n := browsersToday(t, srv, app.ID); n == nil || *n != 1 {
		t.Errorf("browsers = %v, want 1", n)
	}
}

// 8. Sec-GPC: 1 — the page load counts, the browser is neither issued nor counted.
func TestGlobalPrivacyControlIsHonoured(t *testing.T) {
	srv := newTestServer(t)
	audienceOn(t, srv, true)
	be := newBackend(t, nil)
	app := publicEchoApp(be)

	req := pageLoad("/apps/echo/")
	req.Header.Set("Sec-GPC", "1")
	if c := visitorSet(serveNew(t, srv, app, req)); c != nil {
		t.Errorf("Sec-GPC browser issued %q", c.String())
	}
	req = pageLoad("/apps/echo/")
	req.Header.Set("Sec-GPC", "1")
	req.AddCookie(&http.Cookie{Name: visitorCookie, Value: validVisitor()})
	gateway(t, srv, req)
	if n := browsersToday(t, srv, app.ID); n != nil {
		t.Errorf("Sec-GPC browser counted: %d", *n)
	}
	if n := pageLoadsToday(t, srv, app.ID); n != 2 {
		t.Errorf("page loads = %d, want 2", n)
	}
}

// 10. A signed-in request with a visitor cookie adds nothing, and is issued nothing.
func TestASignedInBrowserIsNotInTheSketch(t *testing.T) {
	srv := newTestServer(t)
	audienceOn(t, srv, true)
	seedUser(t, srv.db, "alice", database.SystemRoleViewer)
	be := newBackend(t, nil)
	app := publicEchoApp(be)

	req := pageLoad("/apps/echo/")
	req.AddCookie(sessionCookieFor(t, srv, "alice"))
	if c := visitorSet(serveNew(t, srv, app, req)); c != nil {
		t.Errorf("a signed-in page load was issued %q", c.String())
	}
	req = pageLoad("/apps/echo/")
	req.AddCookie(sessionCookieFor(t, srv, "alice"))
	req.AddCookie(&http.Cookie{Name: visitorCookie, Value: validVisitor()})
	gateway(t, srv, req)
	if n := browsersToday(t, srv, app.ID); n != nil {
		t.Errorf("a signed-in browser was counted: %d", *n)
	}
}

// 11. A malformed cookie is reissued on a page load and never counted.
func TestAMalformedVisitorCookieIsReissued(t *testing.T) {
	srv := newTestServer(t)
	audienceOn(t, srv, true)
	be := newBackend(t, nil)
	app := publicEchoApp(be)

	for i, bad := range []string{"garbage!", base64.RawURLEncoding.EncodeToString([]byte("short")), ""} {
		req := pageLoad("/apps/echo/")
		req.Header.Set("Cookie", fmt.Sprintf("%s=%s", visitorCookie, bad))
		var rr *httptest.ResponseRecorder
		if i == 0 {
			rr = serveNew(t, srv, app, req)
		} else {
			rr = gateway(t, srv, req)
		}
		if visitorSet(rr) == nil {
			t.Errorf("malformed %q was not reissued", bad)
		}
	}
	if n := browsersToday(t, srv, app.ID); n != nil {
		t.Errorf("a malformed id was counted: %d", *n)
	}
}

// An asset request without a cookie is never issued one: the document is, and
// everything it loads returns it.
func TestOnlyAPageLoadIsIssuedAnID(t *testing.T) {
	srv := newTestServer(t)
	audienceOn(t, srv, true)
	be := newBackend(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/apps/echo/app.js", nil)
	req.Header.Set("Sec-Fetch-Dest", "script")
	if c := visitorSet(serveNew(t, srv, publicEchoApp(be), req)); c != nil {
		t.Errorf("an asset request was issued %q", c.String())
	}
}
