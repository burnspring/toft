package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/password"
)

func postAs(t *testing.T, srv *Server, router http.Handler, who, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Locking must kill the target's live sessions in the same transaction — a
// lock that leaves sessions breathing until JWT expiry is not a lock.
func TestLockKillsLiveSessions(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)

	bobSession := sessionCookieFor(t, srv, "bob")

	// Sanity: bob's session works before the lock.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/capabilities", nil)
	req.AddCookie(bobSession)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-lock request: %d, want 200", rec.Code)
	}

	rec = postAs(t, srv, router, "root", "/api/v1/users/bob/lock")
	if rec.Code != http.StatusOK {
		t.Fatalf("lock: %d body=%s", rec.Code, rec.Body.String())
	}

	// The live session must be dead on the very next request.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me/capabilities", nil)
	req.AddCookie(bobSession)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-lock request with old session: %d, want 401", rec.Code)
	}

	// The lock is audited.
	var n int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action='user.lock' AND target_id='bob' AND actor_id='root'`).Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("user.lock audit rows = %d, want 1", n)
	}

	// Unlock restores the account; a fresh session works again.
	rec = postAs(t, srv, router, "root", "/api/v1/users/bob/unlock")
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock: %d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me/capabilities", nil)
	req.AddCookie(sessionCookieFor(t, srv, "bob"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-unlock request with fresh session: %d, want 200", rec.Code)
	}
}

// An admin cannot lock themselves — with a single admin this is also the
// last-unlocked-admin case. Locking another admin while the caller remains
// unlocked is allowed.
func TestLockInvariants(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	rec := postAs(t, srv, router, "root", "/api/v1/users/root/lock")
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-lock: %d, want 409", rec.Code)
	}

	// A second admin can be locked — the caller is still an unlocked admin.
	seedUser(t, srv.db, "admin2", database.SystemRoleAdmin)
	rec = postAs(t, srv, router, "root", "/api/v1/users/admin2/lock")
	if rec.Code != http.StatusOK {
		t.Fatalf("locking admin2: %d body=%s", rec.Code, rec.Body.String())
	}

	// Non-admins cannot lock anyone.
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)
	rec = postAs(t, srv, router, "pub", "/api/v1/users/root/lock")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin lock: %d, want 403", rec.Code)
	}
	rec = postAs(t, srv, router, "", "/api/v1/users/root/lock")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous lock: %d, want 401", rec.Code)
	}
}

// Locked accounts are refused at login — no session is minted. Exercised via
// local password sign-in; the provider callback runs the identical Locked()
// check.
func TestLockedUserLoginRefused(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	if _, err := srv.db.Conn().Exec(`UPDATE users SET locked_at = NOW() WHERE id = $1`,
		database.BuiltinAdminID); err != nil {
		t.Fatalf("locking the built-in administrator: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/local/login", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("locked user login: %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			t.Fatal("a session cookie was issued to a locked user")
		}
	}
}

// Locked users are invalid transfer targets and invalid new-grant targets;
// grants they already hold stay (inert while locked).
func TestLockedUserInvalidTargets(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "frozen", database.SystemRolePublisher)

	app := &database.App{
		Slug: "handoff", Name: "Handoff", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	rec := postAs(t, srv, router, "root", "/api/v1/users/frozen/lock")
	if rec.Code != http.StatusOK {
		t.Fatalf("lock: %d body=%s", rec.Code, rec.Body.String())
	}

	// Transfer to a locked user → 422.
	body, _ := json.Marshal(map[string]string{"new_owner_id": "frozen"})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/apps/%s/transfer", app.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookieFor(t, srv, "owner"))
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("transfer to locked user: %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}

	// New grant to a locked user → 422.
	rec = putGrantAs(t, srv, router, "owner", app.ID, "user", "frozen", database.ObjectRoleViewer)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("grant to locked user: %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
}
