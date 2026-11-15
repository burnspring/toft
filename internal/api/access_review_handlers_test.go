package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// The report is decoded into the type the handler actually writes
// (access_review_handlers.go) rather than a local restatement of it. A second
// copy of the wire shape here would keep passing after the real one changed,
// which is the failure this file exists to catch.

// The access-review route is cross-tenant introspection: admins only, and
// non-admins must get 404 — the route must not confirm which user IDs exist.
func TestUserAccessAdminOnly(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)

	if rec := getAs(t, srv, router, "", "/api/v1/users/pub/access"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", rec.Code)
	}
	if rec := getAs(t, srv, router, "pub", "/api/v1/users/root/access"); rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin: %d, want 404", rec.Code)
	}
	if rec := getAs(t, srv, router, "root", "/api/v1/users/no-such-user/access"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing user: %d, want 404", rec.Code)
	}
	if rec := getAs(t, srv, router, "root", "/api/v1/users/pub/access"); rec.Code != http.StatusOK {
		t.Fatalf("admin: %d, want 200", rec.Code)
	}
}

// Correctness against a seeded matrix: owned app, direct grant, group grant,
// and direct+group on the same app (one row, highest role, all sources).
func TestUserAccessReport(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "alice", database.SystemRolePublisher)
	seedUser(t, srv.db, "victim", database.SystemRolePublisher)

	mk := func(slug, visibility string) *database.App {
		a := &database.App{
			Slug: slug, Name: slug, OwnerID: "victim",
			Framework: "flask", Status: database.AppStatusRunning, Visibility: visibility,
		}
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create %s: %v", slug, err)
		}
		return a
	}
	owned := &database.App{
		Slug: "a-mine", Name: "Mine", OwnerID: "alice",
		Framework: "flask", Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(owned); err != nil {
		t.Fatalf("create owned: %v", err)
	}
	direct := mk("b-direct", database.VisibilityPrivate)
	both := mk("c-both", database.VisibilityPrivate)
	mk("d-unrelated", database.VisibilityPublic) // alice has no path to it; counts as public

	g, err := srv.db.CreateGroup(t.Context(), "data-team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "alice", "root", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	meta := database.RequestMeta{}
	if err := srv.db.UpsertGrant(t.Context(), direct.ID, "user", "alice", database.ObjectRoleViewer, "victim", meta); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.UpsertGrant(t.Context(), both.ID, "user", "alice", database.ObjectRoleEditor, "victim", meta); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.UpsertGrant(t.Context(), both.ID, "group", g.ID, database.ObjectRoleViewer, "victim", meta); err != nil {
		t.Fatal(err)
	}

	rec := getAs(t, srv, router, "root", "/api/v1/users/alice/access")
	if rec.Code != http.StatusOK {
		t.Fatalf("report: %d body=%s", rec.Code, rec.Body.String())
	}
	var rep accessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decoding report: %v", err)
	}

	if rep.User.ID != "alice" || rep.User.Locked {
		t.Fatalf("user header wrong: %+v", rep.User)
	}
	if len(rep.Explicit) != 3 {
		t.Fatalf("explicit entries = %d, want 3 (owned, direct, both): %s", len(rep.Explicit), rec.Body.String())
	}

	// Sorted by slug: a-mine, b-direct, c-both.
	if rep.Explicit[0].App.ID != owned.ID || rep.Explicit[0].EffectiveRole != database.ObjectRoleOwner {
		t.Fatalf("owned entry wrong: %+v", rep.Explicit[0])
	}
	if len(rep.Explicit[0].Sources) != 1 || rep.Explicit[0].Sources[0].Kind != "owner" {
		t.Fatalf("owned sources wrong: %+v", rep.Explicit[0].Sources)
	}

	if rep.Explicit[1].App.ID != direct.ID || rep.Explicit[1].EffectiveRole != database.ObjectRoleViewer {
		t.Fatalf("direct entry wrong: %+v", rep.Explicit[1])
	}

	// direct editor + group viewer on the same app: one row, highest
	// role wins, both sources attributed.
	e := rep.Explicit[2]
	if e.App.ID != both.ID || e.EffectiveRole != database.ObjectRoleEditor {
		t.Fatalf("both entry wrong: %+v", e)
	}
	if len(e.Sources) != 2 {
		t.Fatalf("both sources = %d, want 2: %+v", len(e.Sources), e.Sources)
	}
	kinds := map[string]bool{}
	for _, s := range e.Sources {
		kinds[s.Kind] = true
		if s.Kind == "group" && (s.GroupID != g.ID || s.GroupName != "data-team") {
			t.Fatalf("group source not attributed: %+v", s)
		}
	}
	if !kinds["direct"] || !kinds["group"] {
		t.Fatalf("expected direct+group sources, got %+v", e.Sources)
	}

	if rep.VisibilityAccess.PublicApps != 1 || rep.VisibilityAccess.AuthenticatedApps != 0 {
		t.Fatalf("visibility counts wrong: %+v", rep.VisibilityAccess)
	}
}

// Viewer-system-role and locked users must render with the same read-time
// rules the authorizer applies: editor downgrades to viewer, and the
// header reports the lock.
func TestUserAccessViewerDowngradeAndLocked(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "viewer-user", database.SystemRoleViewer)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		Slug: "tool", Name: "Tool", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	// An editor row for a viewer-system-role user (hand-edited or left by
	// a later demotion) must read as viewer.
	if _, err := srv.db.Conn().Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by)
		VALUES (?, 'user', 'viewer-user', 'editor', 'owner')
	`, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Conn().Exec(`UPDATE users SET locked_at = NOW() WHERE id = 'viewer-user'`); err != nil {
		t.Fatal(err)
	}

	rec := getAs(t, srv, router, "root", "/api/v1/users/viewer-user/access")
	if rec.Code != http.StatusOK {
		t.Fatalf("report: %d body=%s", rec.Code, rec.Body.String())
	}
	var rep accessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.User.Locked {
		t.Fatal("locked user not reported as locked")
	}
	if len(rep.Explicit) != 1 || rep.Explicit[0].EffectiveRole != database.ObjectRoleViewer {
		t.Fatalf("viewer-exclusion downgrade not applied: %+v", rep.Explicit)
	}
}
