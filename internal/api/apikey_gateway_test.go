package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// A removed app key is refused by name at the gateway, in both header forms,
// and never falls through to a cookie or to anonymous — on a private app and
// on a public one alike, because a presented credential that silently became
// no credential is the failure the old branch refused too. It never reaches
// the app.
func TestARemovedAppKeyIsRefusedByNameAtTheGateway(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		ID: "app-public", Slug: "public-api", Name: "Public API", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	secret := "toft_0123456789abcdef0123456789abcdef0123456789abcdef"
	router := srv.AppsRouter()

	for _, tc := range []struct{ header, value string }{
		{"X-API-Key", secret},
		{"Authorization", "Bearer " + secret},
	} {
		t.Run(tc.header, func(t *testing.T) {
			be.lastReq = nil
			req := httptest.NewRequest(http.MethodGet, "/apps/public-api/data", nil)
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 — a public app must not quietly admit an old "+
					"key as anonymous", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "were removed") {
				t.Errorf("body = %q, want the sentence naming what replaced it", rec.Body.String())
			}
			if be.lastReq != nil {
				t.Error("the request reached the app")
			}
		})
	}
}

// A non-platform Authorization header must reach the app untouched — apps
// running their own bearer auth depend on it.
func TestForeignAuthorizationHeaderPassesThrough(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		ID: "app-public", Slug: "public-api", Name: "Public API", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/apps/public-api/data", nil)
	req.Header.Set("Authorization", "Bearer some-app-token")
	rec := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := be.lastReq.Header.Get("Authorization"); got != "Bearer some-app-token" {
		t.Errorf("app's own Authorization header = %q, want it forwarded unchanged", got)
	}
}

// **A read-scoped personal key writes through the apps gateway, and that is a
// decision rather than an oversight.**
//
// The scope bounds Toft's own routes. It says nothing about a deployed
// app's, because Toft has no view into them: a bound claimed there would
// be asserting HTTP semantics on somebody else's code, and it would be false
// the first time an app puts a side effect behind a GET. Refusing the gateway
// outright was the other candidate and costs the read case its most obvious
// use — a script that reads a dashboard's data.
//
// This is a test rather than a comment because the failure mode is a later,
// well-intentioned tightening of proxy_handler.go that breaks customers'
// automation and reads, in the diff, like a fix.
func TestAReadScopedKeyStillWritesThroughTheGateway(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	enablePersonalKeys(t, srv, true)

	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		ID: "app-scoped-gw", Slug: "scoped-gw", Name: "Scoped Gateway App", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	secret := mintScopedKey(t, srv, "owner", "narrow", database.UserKeyScopeRead, nil, false)
	router := srv.AppsRouter()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/apps/scoped-gw/data", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s through the gateway with a read-scoped key: %d, want 200 — the "+
				"scope bounds Toft's routes and never an app's", method, rec.Code)
		}
	}

	// And the platform API, with the same credential, refuses the same verb.
	// The two answers together are the whole of D7: one fence, one side of it.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+app.ID+"/restart", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	srv.Router(http.NotFoundHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("the same key on the platform API: %d, want 403 — otherwise this test is "+
			"asserting nothing about the scope", rec.Code)
	}
}

// A deploy key at the gateway is owner on every app of the install and opens
// none of them: the request goes on as anonymous — what `toft deploy --verify`
// under TOFT_TOKEN relies on — and the credential never reaches the app's code
// in either header form or under a lower-case scheme (code review 2026-11-11,
// A-1).
func TestADeployKeyDoesNotReachAnApp(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		ID: "app-dk", Slug: "public-dk", Name: "Public DK", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	secret := deployKeySecretPrefix + "0123456789abcdef0123456789abcdef"
	router := srv.AppsRouter()
	for _, tc := range []struct{ header, value string }{
		{"X-API-Key", secret},
		{"Authorization", "Bearer " + secret},
		{"Authorization", "bearer " + secret},
	} {
		t.Run(tc.header+" "+tc.value[:6], func(t *testing.T) {
			be.lastReq = nil
			req := httptest.NewRequest(http.MethodGet, "/apps/public-dk/data", nil)
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s, want the public app served anonymously", rec.Code, rec.Body.String())
			}
			if be.lastReq == nil {
				t.Fatal("the request did not reach the app")
			}
			for _, name := range []string{"Authorization", "X-API-Key"} {
				if v := be.lastReq.Header.Get(name); strings.Contains(v, deployKeySecretPrefix) {
					t.Errorf("%s reached the app: %q", name, v)
				}
			}
		})
	}
}

// A personal key under a lower-case `bearer` scheme is the same credential:
// resolved, authorized and stripped, never forwarded as an unknown header.
func TestALowerCaseBearerPersonalKeyIsStillAKey(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	be := newBackend(t, nil)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		ID: "app-lb", Slug: "public-lb", Name: "Public LB", OwnerID: "owner",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/apps/public-lb/data", nil)
	req.Header.Set("Authorization", "bearer "+userKeySecretPrefix+"not-a-real-key")
	rec := httptest.NewRecorder()
	srv.AppsRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d %s, want 401 for an unknown personal key", rec.Code, rec.Body.String())
	}
	if be.lastReq != nil {
		t.Error("the request reached the app")
	}
}

// Slug availability under a personal key answers, as it did before the route
// moved under the guard (code review 2026-11-11, A-5).
func TestSlugAvailabilityAnswersAKey(t *testing.T) {
	srv, router := keyEnv(t)
	secret := mintKey(t, srv, "alice", "ci", nil, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/slugs/brand-new/available", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":true`) {
		t.Fatalf("slug availability under a key: %d %s", rec.Code, rec.Body.String())
	}
}
