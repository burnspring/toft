package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
)

func deleteAs(t *testing.T, srv *Server, router http.Handler, who, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// apps.owner_id has no cascade and an ownerless app is unadministrable, so
// ownership has to move before an account can go.
func TestDeleteUser_RefusedWhileTheyOwnApps(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		Slug: "theirs", Name: "Theirs", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	if rec := deleteAs(t, srv, router, "root", "/api/v1/users/owner"); rec.Code != http.StatusConflict {
		t.Fatalf("deleting an app owner: %d, want 409", rec.Code)
	}
	if u, _ := srv.db.GetUser("owner"); u == nil {
		t.Fatal("the account was deleted despite owning an app")
	}

	// Transfer the app away and the same delete succeeds.
	if err := srv.db.TransferOwnership(t.Context(), app.ID, database.PrincipalTypeUser, "root", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if rec := deleteAs(t, srv, router, "root", "/api/v1/users/owner"); rec.Code != http.StatusNoContent {
		t.Fatalf("deleting after transfer: %d, want 204", rec.Code)
	}
	if u, _ := srv.db.GetUser("owner"); u != nil {
		t.Fatal("the account survived its deletion")
	}
}

// Deletion takes the authority with it, keeps the history, and stays admin-only.
func TestDeleteUser_TakesGrantsKeepsHistory(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "leaver", database.SystemRolePublisher)

	app := &database.App{
		Slug: "shared-app", Name: "Shared App", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if rec := putGrantAs(t, srv, router, "owner", app.ID, "user", "leaver", database.ObjectRoleViewer); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("grant: %d body=%s", rec.Code, rec.Body.String())
	}
	g, err := srv.db.CreateGroup(t.Context(), "team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "leaver", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Traffic they generated must outlive them: the usage table has no FK to
	// users so an app's numbers still add up.
	seedUsage(t, srv.db, app.ID, "leaver", 0, 12)

	// Not an admin's route for anyone else, and it does not confirm the id.
	if rec := deleteAs(t, srv, router, "owner", "/api/v1/users/leaver"); rec.Code != http.StatusNotFound {
		t.Fatalf("publisher deleting a user: %d, want 404", rec.Code)
	}
	// Nobody deletes themselves — it would revoke the session mid-request.
	if rec := deleteAs(t, srv, router, "root", "/api/v1/users/root"); rec.Code != http.StatusConflict {
		t.Fatalf("self-delete: %d, want 409", rec.Code)
	}

	if rec := deleteAs(t, srv, router, "root", "/api/v1/users/leaver"); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete: %d body=%s", rec.Code, rec.Body.String())
	}

	grants, err := srv.db.ListGrants(app.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	for _, gr := range grants {
		if gr.PrincipalType == database.PrincipalTypeUser && gr.PrincipalID == "leaver" {
			t.Fatal("a deleted account still holds a grant")
		}
	}
	members, err := srv.db.ListGroupMembers(g.ID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("group still has %d member(s) after the account was deleted", len(members))
	}

	// The audit log keeps the deletion, attributed to the admin who asked.
	var n int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action='user.delete' AND target_id='leaver' AND actor_id='root'`,
	).Scan(&n); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if n != 1 {
		t.Fatalf("user.delete audit entries = %d, want 1", n)
	}

	// And their traffic still counts toward the app's totals.
	totals, err := srv.db.GetAppUsageTotals(app.ID, 30)
	if err != nil {
		t.Fatalf("usage totals: %v", err)
	}
	if totals.Requests != 12 {
		t.Fatalf("requests = %d, want 12 — a deleted account took its traffic with it", totals.Requests)
	}
}

// The last-admin invariant holds for deletion exactly as it does for demotion
// and locking; otherwise the system becomes unadministrable.
func TestDeleteUser_CannotRemoveTheLastAdmin(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "second", database.SystemRoleAdmin)

	// Two admins: removing one is fine.
	if rec := deleteAs(t, srv, router, "root", "/api/v1/users/second"); rec.Code != http.StatusNoContent {
		t.Fatalf("deleting one of two admins: %d, want 204", rec.Code)
	}

	// One admin left, and they cannot be deleted by anyone — including the
	// only account that could ask.
	seedUser(t, srv.db, "helper", database.SystemRolePublisher)
	if err := srv.db.UpdateUserRole("helper", database.SystemRoleAdmin); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if err := srv.db.UpdateUserRole("root", database.SystemRolePublisher); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if rec := deleteAs(t, srv, router, "helper", "/api/v1/users/helper"); rec.Code != http.StatusConflict {
		t.Fatalf("last admin deleting themselves: %d, want 409", rec.Code)
	}
}
