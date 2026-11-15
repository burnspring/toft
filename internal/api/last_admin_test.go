package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// "The last administrator" means the last one who can sign in. A locked
// administrator cannot unlock anybody, so with A and B administrators and B
// locked, A is the last one — and the demotion, the deletion and the lock
// all have to say so, in the same words, because they are one refusal through
// one predicate. Before this test the demotion counted B and let A go, and
// the install was left with nobody who could administer it.
func TestTheLastUnlockedAdministratorIsRefusedOnEveryDoor(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "alpha", database.SystemRoleAdmin)
	seedUser(t, srv.db, "beta", database.SystemRoleAdmin)
	seedUser(t, srv.db, "third", database.SystemRoleAdmin)
	// The built-in admin is locked too, so the seeded three are the only
	// administrators who count.
	if _, err := srv.db.LockUser(context.Background(), database.BuiltinAdminID, database.AuditEntry{
		ActorType: database.PrincipalTypeOperator, ActorID: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// alpha locks beta: allowed, alpha and third can still sign in.
	if rr := postAs(t, srv, router, "alpha", "/api/v1/users/beta/lock"); rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("locking beta with two other admins = %d body=%s", rr.Code, rr.Body.String())
	}
	// third demotes themself: allowed, alpha remains.
	if rr := patchRoleAs(t, srv, router, "third", "third", database.SystemRolePublisher); rr.Code != http.StatusOK {
		t.Fatalf("demoting third with alpha unlocked = %d body=%s", rr.Code, rr.Body.String())
	}

	// alpha is now the last administrator who can sign in. Three admins exist
	// by the old count; one by the right one.
	want := database.ErrLastAdmin.Error()

	rr := patchRoleAs(t, srv, router, "alpha", "alpha", database.SystemRolePublisher)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), want) {
		t.Errorf("demoting the last unlocked admin = %d %s, want 409 %q", rr.Code, rr.Body.String(), want)
	}
	// Deleting and locking oneself are refused for reasons of their own, and
	// nobody else who could ask is an administrator; the database calls the
	// handlers and the SCIM feed wrap are where the predicate lives.
	err := srv.db.DeleteUser("alpha", database.AuditEntry{
		ActorType: database.PrincipalTypeOperator, ActorID: "test",
	})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("deleting the last unlocked admin = %v, want %q", err, want)
	}
	_, err = srv.db.LockUser(context.Background(), "alpha", database.AuditEntry{
		ActorType: database.PrincipalTypeProvider, ActorID: "okta",
	})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("locking the last unlocked admin = %v, want %q", err, want)
	}

	after, err := srv.db.GetUser("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if after.SystemRole != database.SystemRoleAdmin || after.Locked() {
		t.Fatalf("alpha after the refusals: role=%s locked=%v — a refusal wrote something", after.SystemRole, after.Locked())
	}
}

func patchRoleAs(t *testing.T, srv *Server, router http.Handler, who, target, role string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"system_role": role})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+target+"/role", bytes.NewReader(body))
	req.AddCookie(sessionCookieFor(t, srv, who))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
