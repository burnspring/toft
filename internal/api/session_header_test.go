package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// X-Toft-Session is platform-set or absent, never client-supplied. The
// guard deletes the inbound header on every path — anonymous and API-key
// included, where it never sets one — and on the session path replaces it
// with the login's own jti. Merely omitting it on the credential-less paths
// would let a forged value sail through to the app.
func TestSessionHeaderIsPlatformSetOnEveryPath(t *testing.T) {
	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	be := newBackend(t, nil)
	seedUser(t, srv.db, "viewer", database.SystemRolePublisher)
	app := &database.App{
		ID: "app-echo", Slug: "echo", Name: "Echo", OwnerID: "viewer",
		Framework: "go", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	enablePersonalKeys(t, srv, true)
	apiSecret := mintKey(t, srv, "viewer", "ci", nil, false)

	router := srv.AppsRouter()
	// Every request arrives with a forged header; what the app receives is
	// what the guard decided.
	forwarded := func(mutate func(*http.Request)) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/apps/echo/data", nil)
		req.Header.Set(sessionHeader, "forged-by-client")
		if mutate != nil {
			mutate(req)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		return be.lastReq.Header.Get(sessionHeader)
	}

	t.Run("anonymous requests carry no header", func(t *testing.T) {
		if got := forwarded(nil); got != "" {
			t.Errorf("forged header reached the app: %q", got)
		}
	})

	t.Run("API-key requests carry no header", func(t *testing.T) {
		got := forwarded(func(r *http.Request) { r.Header.Set("X-API-Key", apiSecret) })
		if got != "" {
			t.Errorf("forged header reached the app: %q", got)
		}
	})

	t.Run("a session carries its own id, stable per login and distinct across logins", func(t *testing.T) {
		// Two requests replaying one cookie are one browser login.
		login := sessionCookieFor(t, srv, "viewer")
		first := forwarded(func(r *http.Request) { r.AddCookie(login) })
		if first == "" || first == "forged-by-client" {
			t.Fatalf("session id = %q, want a platform-minted value", first)
		}
		if again := forwarded(func(r *http.Request) { r.AddCookie(login) }); again != first {
			t.Errorf("one login produced two ids: %q then %q", first, again)
		}
		// A second sign-in — another browser or device — is a new login.
		other := forwarded(func(r *http.Request) { r.AddCookie(sessionCookieFor(t, srv, "viewer")) })
		if other == first {
			t.Errorf("two logins share an id: %q", other)
		}
	})
}
