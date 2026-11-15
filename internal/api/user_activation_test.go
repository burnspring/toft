package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/password"
)

// The activation queue, driven through the real router.
//
// The property under test is not "the column flips" — it is that the two halves
// of the decision land together and that neither of them can be reached any
// other way: an account is admitted and its role is chosen in one audited
// transaction, and nothing but an administrator gets to do it.

// pending marks a seeded account as awaiting activation, which is what a
// provider with `default_role: pending` does at provisioning time. Done in SQL
// rather than through ResolveIdentity because these tests are about the
// handler, and a real OAuth callback is not reachable from here.
func pending(t *testing.T, srv *Server, id string) {
	t.Helper()
	if _, err := srv.db.Conn().Exec(
		`UPDATE users SET pending_activation_at = NOW() WHERE id = ?`, id); err != nil {
		t.Fatalf("marking %s pending: %v", id, err)
	}
}

func activateAs(t *testing.T, srv *Server, router http.Handler, who, target, role string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"system_role": role})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/"+target+"/activate", bytes.NewReader(body))
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestActivateAdmitsTheAccountAndSetsItsRole(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "newcomer", database.SystemRoleViewer)
	pending(t, srv, "newcomer")

	rec := activateAs(t, srv, router, "root", "newcomer", database.SystemRolePublisher)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate: %d body=%s", rec.Code, rec.Body.String())
	}

	after, err := srv.db.GetUser("newcomer")
	if err != nil || after == nil {
		t.Fatalf("GetUser: %v", err)
	}
	if after.PendingActivation() {
		t.Error("still pending after activation")
	}
	// The role is the other half of the same decision, and an activation that
	// admitted somebody without applying the chosen role would be a queue that
	// quietly ignores what the administrator picked.
	if after.SystemRole != database.SystemRolePublisher {
		t.Errorf("system_role = %q, want publisher", after.SystemRole)
	}

	var n int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action='user.activate' AND target_id='newcomer' AND actor_id='root'`).
		Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("user.activate audit rows = %d, want 1", n)
	}

	// Twice is a conflict, not a second activation. Two administrators working
	// the same queue is the ordinary case, and the second one has to be told
	// rather than silently re-deciding a role.
	if rec := activateAs(t, srv, router, "root", "newcomer", database.SystemRoleViewer); rec.Code != http.StatusConflict {
		t.Fatalf("second activation: %d, want 409", rec.Code)
	}
	after, _ = srv.db.GetUser("newcomer")
	if after.SystemRole != database.SystemRolePublisher {
		t.Errorf("a refused second activation changed the role to %q", after.SystemRole)
	}
}

// Activation defaults to the floor. An administrator who sends no role is
// admitting somebody, not promoting them.
func TestActivateWithoutARoleAdmitsAViewer(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "newcomer", database.SystemRolePublisher)
	pending(t, srv, "newcomer")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/newcomer/activate",
		bytes.NewReader([]byte(`{}`)))
	req.AddCookie(sessionCookieFor(t, srv, "root"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate: %d body=%s", rec.Code, rec.Body.String())
	}

	after, _ := srv.db.GetUser("newcomer")
	if after.SystemRole != database.SystemRoleViewer {
		t.Errorf("system_role = %q, want viewer — an omitted role must not be read as a promotion",
			after.SystemRole)
	}
}

// Two refusals the route has to keep: it is not a promotion path, and it is not
// an unlock.
func TestActivateRefusesAdminAndIsNotAnUnlock(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "newcomer", database.SystemRoleViewer)
	pending(t, srv, "newcomer")

	if rec := activateAs(t, srv, router, "root", "newcomer", database.SystemRoleAdmin); rec.Code != http.StatusBadRequest {
		t.Fatalf("activate as system_admin: %d, want 400 — letting somebody in and handing "+
			"them the install are not the same click", rec.Code)
	}

	// Locked *and* pending: activating lifts one refusal and must leave the
	// other standing.
	if rec := postAs(t, srv, router, "root", "/api/v1/users/newcomer/lock"); rec.Code != http.StatusOK {
		t.Fatalf("lock: %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := activateAs(t, srv, router, "root", "newcomer", database.SystemRoleViewer); rec.Code != http.StatusOK {
		t.Fatalf("activate a locked pending account: %d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := srv.db.GetUser("newcomer")
	if !after.Locked() {
		t.Error("activation cleared the lock — two refusals are two decisions")
	}
}

// Nobody but an administrator, and an account that was never in the queue is a
// conflict rather than a way to reset somebody's role.
func TestActivateIsAdminOnlyAndOnlyForPendingAccounts(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "mallory", database.SystemRolePublisher)
	seedUser(t, srv.db, "newcomer", database.SystemRoleViewer)
	pending(t, srv, "newcomer")

	if rec := activateAs(t, srv, router, "mallory", "newcomer", database.SystemRolePublisher); rec.Code != http.StatusForbidden {
		t.Fatalf("activation by a publisher: %d, want 403", rec.Code)
	}
	if rec := activateAs(t, srv, router, "", "newcomer", database.SystemRolePublisher); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous activation: %d, want 401", rec.Code)
	}
	after, _ := srv.db.GetUser("newcomer")
	if !after.PendingActivation() {
		t.Fatal("a refused activation let the account in anyway")
	}

	// An ordinary account is not a role-reset endpoint.
	if rec := activateAs(t, srv, router, "root", "mallory", database.SystemRoleViewer); rec.Code != http.StatusConflict {
		t.Fatalf("activating an account that was never pending: %d, want 409", rec.Code)
	}
	unchanged, _ := srv.db.GetUser("mallory")
	if unchanged.SystemRole != database.SystemRolePublisher {
		t.Errorf("system_role = %q — activation demoted an account it should not have touched",
			unchanged.SystemRole)
	}
}

// An unactivated account gets no session, however well it authenticated. The
// provider callback is not reachable from a test, so this pins the other
// sign-in path that reads the same fact.
func TestPendingAccountCannotSignInLocally(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	// The built-in administrator, because it is the one account in a test
	// install that has a password. Nothing provisions it as pending in
	// production — this is the sign-in path being pinned, not the policy.
	pending(t, srv, database.BuiltinAdminID)

	body, _ := json.Marshal(map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/local/login", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("sign-in by a pending account: %d body=%s, want 403", rec.Code, rec.Body.String())
	}

	var payload refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}
	// The reason is a wire contract the login page reads to pick which of the
	// administrator's sentences to show, and folding it into the locked one
	// would tell somebody in a queue that their account had been taken away.
	if payload["reason"] != deniedPending {
		t.Errorf("reason = %q, want %q", payload["reason"], deniedPending)
	}
	if payload["error"] != database.DefaultPendingActivationMessage {
		t.Errorf("the refusal did not carry the administrator's pending message: %q", payload["error"])
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			t.Error("a session cookie was issued to an unactivated account")
		}
	}
}
