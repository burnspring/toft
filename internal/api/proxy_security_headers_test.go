package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The other half of the exemption, and the half that needs a database.
//
// securityHeaders sets the platform's headers on everything, because a
// middleware looking at `/apps/{slug}` cannot tell a deployed app from
// `/apps/new` or from an app id — the slug and the id occupy the same segment.
// So the decision is made here instead, once the slug has actually been
// resolved. Both directions are asserted through the real router, because both
// were wrong at once while this was a prefix: the app got the exemption *and*
// so did every app-management page in the application.
func TestProxyStripsThePlatformsSecurityHeaders(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)

	// apps.owner_id is a real foreign key under Postgres.
	if err := srv.db.UpsertUser(&database.User{
		ID: "u1", Login: "u1", SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	app := &database.App{
		Slug: "reports", Name: "Reports", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port), EndpointHost: "127.0.0.1",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// A stand-in for the SPA. What matters is only that it is reached and that
	// the headers survive to it.
	router := srv.Router(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<!doctype html><title>SPA</title>"))
	}))

	// The deployed app: the platform's policy must not follow the routing in.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apps/reports/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxying the app: %d — %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("the app's response carries the platform's X-Frame-Options: %q", got)
	}
	// nosniff and Referrer-Policy are the baseline's now, filled where the app
	// sent none — once each, never the platform's copy beside a second.
	for _, name := range []string{"X-Content-Type-Options", "Referrer-Policy"} {
		if got := rec.Header().Values(name); len(got) != 1 {
			t.Errorf("%s on the app's response = %v, want exactly the baseline's one", name, got)
		}
	}
	// One Content-Security-Policy, and it is the framing one the proxy adds —
	// not the SPA's, whose `frame-ancestors 'none'` would blank the live
	// preview on every app page.
	csp := rec.Header().Values("Content-Security-Policy")
	if len(csp) != 1 || !strings.HasPrefix(csp[0], "frame-ancestors ") {
		t.Errorf("policy on the app's response = %v, want only the framing one", csp)
	}

	// And the paths that merely look like one. These are this application's own
	// pages — the app list, the create page, and every tab of an app's settings
	// — and each was served with no policy at all while the exemption was a
	// prefix. A scanner found the first; the rest are behind sign-in, so
	// nothing would have found them.
	for _, path := range []string{
		"/apps",
		"/apps/new",
		"/apps/8f14e45f-ceea-467a-9fdb-7c39f8f4e4a1",
		"/apps/8f14e45f-ceea-467a-9fdb-7c39f8f4e4a1/keys",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: the SPA lost the platform's CSP", path)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY — the page is framable", path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
	}
}

// An app's own HSTS never reaches the browser, and the install's arrives
// exactly once.
//
// This is the test whose failure nothing else would show. Without the delete in
// ModifyResponse, an app's `max-age=0` would follow the install's header and
// only header order would keep HSTS on for the host — every app on it, not
// just this one. Without the middleware there would be no header at all and
// no symptom until a downgrade.
func TestAnAppsOwnHSTSIsReplacedByTheInstalls(t *testing.T) {
	srv := newTestServer(t)
	be := newBackend(t, nil)
	be.headers.Set("Strict-Transport-Security", "max-age=0; includeSubDomains")

	if err := srv.db.UpsertUser(&database.User{
		ID: "u1", Login: "u1", SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	app := &database.App{
		Slug: "reports", Name: "Reports", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port), EndpointHost: "127.0.0.1",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	router := srv.Router(http.NotFoundHandler())

	// Over TLS: the install's value, once, and nothing of the app's.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://localhost:8080/apps/reports/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxying the app: %d — %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Values("Strict-Transport-Security"); len(got) != 1 || got[0] != hstsValue {
		t.Errorf("Strict-Transport-Security = %v, want exactly [%s]", got, hstsValue)
	}

	// Over http on an http install: the app's is still gone, and nothing
	// replaces it.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apps/reports/", nil))
	if got := rec.Header().Values("Strict-Transport-Security"); len(got) != 0 {
		t.Errorf("Strict-Transport-Security = %v over http, want none", got)
	}
}

// proxiedApp is a running public app on newTestServer's single-origin router,
// in front of a backend that sends the given headers.
func proxiedApp(t *testing.T, headers map[string]string) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(t)
	be := newBackend(t, nil)
	for k, v := range headers {
		be.headers.Set(k, v)
	}
	if err := srv.db.UpsertUser(&database.User{
		ID: "u1", Login: "u1", SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := srv.db.CreateApp(&database.App{
		Slug: "reports", Name: "Reports", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port), EndpointHost: "127.0.0.1",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return srv, srv.Router(http.NotFoundHandler())
}

func getApp(t *testing.T, router http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxying the app: %d — %s", rec.Code, rec.Body.String())
	}
	return rec
}

// Silence gets the baseline.
func TestTheBaselineFillsAnAppThatSentNeitherHeader(t *testing.T) {
	_, router := proxiedApp(t, nil)
	rec := getApp(t, router, "/apps/reports/")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
}

// An app's own headers pass through untouched, and once. A duplicate would be
// resolved by the browser by order, and nobody would see it happen — which is
// why the platform's values come off before the proxy and the fill is decided
// by the header's name, not its value.
func TestAnAppsOwnHeadersWinAndAppearOnce(t *testing.T) {
	_, router := proxiedApp(t, map[string]string{
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "whatever",
	})
	rec := getApp(t, router, "/apps/reports/")
	if got := rec.Header().Values("Referrer-Policy"); len(got) != 1 || got[0] != "no-referrer" {
		t.Errorf("Referrer-Policy = %v, want exactly the app's [no-referrer]", got)
	}
	if got := rec.Header().Values("X-Content-Type-Options"); len(got) != 1 || got[0] != "whatever" {
		t.Errorf("X-Content-Type-Options = %v, want exactly the app's [whatever]", got)
	}
}

// The switch, through both write doors, visible on the very next proxied
// response. If this breaks, an administrator turns the baseline off, watches
// the save succeed, and nothing changes. HSTS is not behind it.
func TestTheSwitchIsReadOnTheNextProxiedResponse(t *testing.T) {
	srv, router := proxiedApp(t, nil)
	getApp(t, router, "https://localhost:8080/apps/reports/") // fill the cache

	s, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.AppSecurityHeaders = false
	if err := srv.db.SetPlatformSettings(&s, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	rec := getApp(t, router, "https://localhost:8080/apps/reports/")
	for _, name := range []string{"X-Content-Type-Options", "Referrer-Policy"} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("switch off: %s = %q, want none", name, got)
		}
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != hstsValue {
		t.Errorf("switch off took HSTS with it: %q", got)
	}

	if _, err := srv.db.PutSettingAudited(database.SettingAppSecurityHeaders, "true",
		database.AuditEntry{ActorType: "user", ActorID: database.BuiltinAdminID, Action: "platform_settings.update"}); err != nil {
		t.Fatal(err)
	}
	rec = getApp(t, router, "https://localhost:8080/apps/reports/")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("switch back on: X-Content-Type-Options = %q, want nosniff", got)
	}
}
