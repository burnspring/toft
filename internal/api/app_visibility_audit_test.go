package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"toft/internal/database"
)

// Who may read an app is an authorization-relevant fact, so a change to it is
// audited in the same transaction that makes it. Two things follow, and
// neither was true before: a PATCH that re-sends the visibility an app already
// has records nothing, and a write that fails leaves no row saying it
// happened.
//
// The first is what the reachability model's only exit depends on — an owner
// re-sending `public` on a public app must not read as a decision — but the
// defect is older than that feature and these are its regression tests.

func visibilityTestEnv(t *testing.T) (*Server, http.Handler, *database.App) {
	t.Helper()

	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		Slug: "payroll", Name: "Payroll", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return srv, srv.Router(http.NotFoundHandler()), app
}

func countVisibilityAudits(t *testing.T, srv *Server, appID string) int {
	t.Helper()
	var n int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'app.visibility_change' AND target_id = $1`,
		appID).Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	return n
}

// The no-op. `public` on an app that is already public is not a change, and
// the audit log is a record of changes.
func TestVisibilityPatch_SameValue_IsNotAChange(t *testing.T) {
	srv, router, app := visibilityTestEnv(t)

	rec := patchAppAs(t, srv, router, "owner", app.ID, `{"visibility":"public"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if n := countVisibilityAudits(t, srv, app.ID); n != 0 {
		t.Errorf("audit rows = %d, want 0 — a PATCH that changed nothing was recorded as a change", n)
	}

	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != database.VisibilityPublic {
		t.Errorf("visibility = %q, want %q", got.Visibility, database.VisibilityPublic)
	}
}

// The change. One row, and its before and after are different values.
func TestVisibilityPatch_RealChange_IsAuditedOnce(t *testing.T) {
	srv, router, app := visibilityTestEnv(t)

	rec := patchAppAs(t, srv, router, "owner", app.ID, `{"visibility":"private"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if n := countVisibilityAudits(t, srv, app.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}

	var before, after string
	if err := srv.db.Conn().QueryRow(
		`SELECT before, after FROM audit_log WHERE action = 'app.visibility_change' AND target_id = $1`,
		app.ID).Scan(&before, &after); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if before == after {
		t.Errorf("before = after = %q — the row does not describe a change", before)
	}
	// Read back as JSON rather than as bytes: the column is jsonb and Postgres
	// re-renders it with its own spacing.
	if got := visibilityIn(t, before); got != database.VisibilityPublic {
		t.Errorf("before = %q, want %q", got, database.VisibilityPublic)
	}
	if got := visibilityIn(t, after); got != database.VisibilityPrivate {
		t.Errorf("after = %q, want %q", got, database.VisibilityPrivate)
	}

	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != database.VisibilityPrivate {
		t.Errorf("visibility = %q, want %q", got.Visibility, database.VisibilityPrivate)
	}
}

// The failed write. The change and its record are one transaction, so a write
// that touches no row rolls the record back with it.
func TestSetAppVisibility_NoSuchApp_WritesNoAuditRow(t *testing.T) {
	srv, _, _ := visibilityTestEnv(t)

	err := srv.db.SetAppVisibility("no-such-app", database.VisibilityPublic,
		database.VisibilityPrivate, database.PrincipalTypeUser, "owner", "req", "127.0.0.1", "test", "")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetAppVisibility on a missing app = %v, want sql.ErrNoRows", err)
	}
	if n := countVisibilityAudits(t, srv, "no-such-app"); n != 0 {
		t.Errorf("audit rows = %d, want 0 — a change that never happened was recorded", n)
	}
}

func visibilityIn(t *testing.T, raw string) string {
	t.Helper()
	var v struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("parsing %s: %v", raw, err)
	}
	return v.Visibility
}
