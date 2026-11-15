package database_test

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// An app appearing and an app disappearing are the two most consequential
// things that happen to an estate, and neither was in `audit_log`. A create
// left only the owner grant — a row about access standing in for a row about
// existence — and a delete left nothing at all, so an app could be removed with
// its whole history and the log could not say who did it or that it had ever
// been there.
//
// Both rows are written in the transaction that makes the change, per
// docs/invariants.md § Authorization.

func auditByAction(t *testing.T, db *database.DB, action string) []database.ActivityEntry {
	t.Helper()
	rows, err := db.ListActivity(database.ActivityFilter{
		Source: "audit", Actions: []string{action}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	return rows
}

func TestCreatingAnAppIsAudited(t *testing.T) {
	db := dbtest.New(t)
	if err := db.UpsertUser(&database.User{ID: "alice", Login: "alice"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}

	app := &database.App{
		Slug: "payroll", Name: "Payroll", OwnerID: "alice",
		RepoURL: "https://github.com/acme/payroll", Branch: "main",
		Status:  database.AppStatusBuilding,
	}
	if err := db.CreateAppTx(app, "", "alice", "req-1", "10.0.0.1", "curl"); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}

	rows := auditByAction(t, db, "app.create")
	if len(rows) != 1 {
		t.Fatalf("app.create rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.TargetID != app.ID {
		t.Errorf("target = %q, want the new app %q", row.TargetID, app.ID)
	}
	if row.ActorLabel != "alice" {
		t.Errorf("actor = %q, want alice", row.ActorLabel)
	}
	// The repository is the fact a reviewer is actually after: an app is a URL
	// somebody pointed the platform at.
	if !strings.Contains(row.Detail, "acme/payroll") {
		t.Errorf("the row should name the repository it was created from: %s", row.Detail)
	}
	if !strings.Contains(row.Detail, "payroll") {
		t.Errorf("the row should name the slug: %s", row.Detail)
	}
}

func TestDeletingAnAppIsAuditedAndTheRowOutlivesIt(t *testing.T) {
	db := dbtest.New(t)
	if err := db.UpsertUser(&database.User{ID: "alice", Login: "alice"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: "payroll", Name: "Payroll", OwnerID: "alice",
		RepoURL: "https://github.com/acme/payroll", Branch: "main",
		Status:  database.AppStatusBuilding,
	}
	if err := db.CreateAppTx(app, "", "alice", "", "", ""); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}

	if err := db.DeleteAppTx(app, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "alice", RequestID: "req-2",
	}); err != nil {
		t.Fatalf("DeleteAppTx: %v", err)
	}

	if got, err := db.GetApp(app.ID); err != nil || got != nil {
		t.Fatalf("GetApp after delete = %v, %v; want nil, nil", got, err)
	}

	rows := auditByAction(t, db, "app.delete")
	if len(rows) != 1 {
		t.Fatalf("app.delete rows = %d, want 1", len(rows))
	}
	// The point of the `before` blob: an id names nothing once the row it
	// pointed at is gone, and this record exists to be read afterwards.
	if !strings.Contains(rows[0].Detail, "payroll") {
		t.Errorf("the row should say what the app was, not just its id: %s", rows[0].Detail)
	}
}

// Every row about an app that has since been deleted — its create, its
// deletion — names it by the slug its deletion recorded and says it is gone.
// The page linked the bare id to /apps/<id>, which led nowhere on every row.
func TestActivityAboutADeletedAppNamesItAndSaysItIsGone(t *testing.T) {
	db := dbtest.New(t)
	if err := db.UpsertUser(&database.User{ID: "alice", Login: "alice"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	gone := &database.App{
		Slug: "payroll", Name: "Payroll", OwnerID: "alice",
		RepoURL: "https://github.com/acme/payroll", Branch: "main",
		Status:  database.AppStatusBuilding,
	}
	kept := &database.App{
		Slug: "ledger", Name: "Ledger", OwnerID: "alice",
		RepoURL: "https://github.com/acme/ledger", Branch: "main",
		Status:  database.AppStatusBuilding,
	}
	for _, a := range []*database.App{gone, kept} {
		if err := db.CreateAppTx(a, "", "alice", "", "", ""); err != nil {
			t.Fatalf("CreateAppTx: %v", err)
		}
	}
	if err := db.DeleteAppTx(gone, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "alice",
	}); err != nil {
		t.Fatalf("DeleteAppTx: %v", err)
	}

	for _, action := range []string{"app.create", "app.delete"} {
		for _, row := range auditByAction(t, db, action) {
			switch row.TargetID {
			case gone.ID:
				if row.TargetLabel != "payroll" || !row.TargetGone {
					t.Errorf("%s on the deleted app: label %q gone %v; want \"payroll\", true",
						action, row.TargetLabel, row.TargetGone)
				}
			case kept.ID:
				if row.TargetLabel != "ledger" || row.TargetGone {
					t.Errorf("%s on the live app: label %q gone %v; want \"ledger\", false",
						action, row.TargetLabel, row.TargetGone)
				}
			}
		}
	}
}

// Search finds what the Who and Target columns print. It matched a live app's
// slug and a target user's login only, so a deleted app — the usual reason to
// search — and the person who acted both came back empty (trytoft, e2e-fx).
func TestActivitySearchFindsADeletedAppAndTheActor(t *testing.T) {
	db := dbtest.New(t)
	for _, login := range []string{"alice", "bob"} {
		if err := db.UpsertUser(&database.User{ID: login, Login: login}); err != nil {
			t.Fatalf("UpsertUser: %v", err)
		}
	}
	gone := &database.App{
		Slug: "e2e-fx", Name: "FX", OwnerID: "alice",
		RepoURL: "https://github.com/acme/fx", Branch: "main",
		Status:  database.AppStatusBuilding,
	}
	kept := &database.App{
		Slug: "ledger", Name: "Ledger", OwnerID: "bob",
		RepoURL: "https://github.com/acme/ledger", Branch: "main",
		Status:  database.AppStatusBuilding,
	}
	if err := db.CreateAppTx(gone, "", "alice", "", "", ""); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}
	if err := db.CreateAppTx(kept, "", "bob", "", "", ""); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}
	if err := db.DeleteAppTx(gone, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "alice",
	}); err != nil {
		t.Fatalf("DeleteAppTx: %v", err)
	}

	search := func(q string) map[string]bool {
		t.Helper()
		rows, err := db.ListActivity(database.ActivityFilter{Query: q, Limit: 50})
		if err != nil {
			t.Fatalf("ListActivity %q: %v", q, err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			got[r.Action+" "+r.TargetLabel] = true
		}
		return got
	}

	got := search("FX")
	for _, want := range []string{"app.create e2e-fx", "app.delete e2e-fx"} {
		if !got[want] {
			t.Errorf("search %q missed %q; got %v", "FX", want, got)
		}
	}
	if got["app.create ledger"] {
		t.Errorf("search %q matched the other app: %v", "FX", got)
	}

	if got := search("bob"); !got["app.create ledger"] || got["app.create e2e-fx"] {
		t.Errorf("search by the actor's login = %v; want bob's create only", got)
	}
}

// A failed create is undone with DeleteApp, and that is not somebody deleting
// an app. Two handlers do it, and an audit row there would report an app having
// been removed seconds after it appeared.
func TestRollingBackAFailedCreateIsNotAudited(t *testing.T) {
	db := dbtest.New(t)
	if err := db.UpsertUser(&database.User{ID: "alice", Login: "alice"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: "halfborn", Name: "Halfborn", OwnerID: "alice",
		Status: database.AppStatusBuilding,
	}
	if err := db.CreateAppTx(app, "", "alice", "", "", ""); err != nil {
		t.Fatalf("CreateAppTx: %v", err)
	}
	if err := db.DeleteApp(app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if rows := auditByAction(t, db, "app.delete"); len(rows) != 0 {
		t.Errorf("app.delete rows = %d, want none for a rollback", len(rows))
	}
}
