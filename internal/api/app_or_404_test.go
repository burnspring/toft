package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"toft/internal/database"
)

// A database that cannot answer is not "app not found". Forty-eight handlers
// once said it was, and a client that has learnt 404 means what 5xx means
// cannot be taught to retry. The closed pool below is the Postgres blip: the
// same request that was 404 before the helper is 500 now, and a made-up id
// on a working pool is still 404.
func TestADatabaseErrorIsNotAppNotFound(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/no-such-app/grants", nil)
	req.AddCookie(sessionCookieFor(t, srv, "root"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a made-up id on a working pool = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	// With the pool closed the session check cannot run either, so a request
	// through the router is anonymous and the /apps guard answers 401 before
	// any lookup (BB-40). The lookup itself is what this test is about, so it
	// is asked directly.
	if err := srv.db.Close(); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	if _, ok := srv.appOr404(rec, "no-such-app"); ok {
		t.Fatal("a lookup on a closed pool reported an app")
	}
	if rec.Code == http.StatusNotFound {
		t.Fatalf("a database error was answered 404 — the client will believe the app is gone: %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a database error = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

// The same for the app proxy's slug lookup: a failed read is a 500, never the
// "no such app" answer (the SPA, or the gateway's 404 card) for a live app.
func TestTheProxyGuardDoesNotReadADatabaseErrorAsNoSuchApp(t *testing.T) {
	srv := newTestServer(t)
	unresolved := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }
	r := chi.NewRouter()
	r.Route("/apps/{slug}", func(r chi.Router) {
		r.Use(srv.appProxyGuard(unresolved))
		r.HandleFunc("/*", func(w http.ResponseWriter, r *http.Request) {})
	})
	get := func() int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/apps/no-such-app/", nil))
		return rec.Code
	}
	if code := get(); code != http.StatusTeapot {
		t.Fatalf("an unknown slug on a working pool = %d, want the unresolved handler", code)
	}
	if err := srv.db.Close(); err != nil {
		t.Fatal(err)
	}
	if code := get(); code != http.StatusInternalServerError {
		t.Fatalf("a database error = %d, want 500", code)
	}
}
