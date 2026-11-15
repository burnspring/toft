package database_test

import (
	"os"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The cleanup check's own test, and the one whose absence nothing would show:
// a writer that forgets its app ships, and its row is missing from that app's
// history forever.
func TestAuditScopeViolationsNameTheAction(t *testing.T) {
	db := dbtest.New(t)
	for _, e := range []database.AuditEntry{
		// An app-scoped action whose target is not the app and that says
		// nothing about it.
		{ActorType: "user", ActorID: "u1", Action: "object_grant.create", TargetType: "user", TargetID: "u2"},
		// An install act filed under an app.
		{ActorType: "user", ActorID: "u1", Action: "user.lock", TargetType: "user", TargetID: "u2", AppID: "app-1"},
		// Neither of these is a violation.
		{ActorType: "user", ActorID: "u1", Action: "app.stop", TargetType: "app", TargetID: "app-1"},
		{ActorType: "user", ActorID: "u1", Action: "access_request.create", TargetType: "role_promotion", TargetID: "r1"},
	} {
		if err := db.WriteAuditRow(e); err != nil {
			t.Fatal(err)
		}
	}
	bad, err := db.AuditScopeViolations()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(bad, "; ")
	if len(bad) != 2 || !strings.Contains(got, "object_grant.create") || !strings.Contains(got, "user.lock") {
		t.Errorf("violations = %v, want object_grant.create and user.lock", bad)
	}
	// This test's own rows would fail its cleanup, which is the check working.
	if _, err := db.Conn().Exec(`DELETE FROM audit_log WHERE action IN ('object_grant.create', 'user.lock')`); err != nil {
		t.Fatal(err)
	}
}

// The derivation: an app target or an app actor names the app with nothing
// said at the call site, and an explicit value wins.
func TestAuditAppIDIsDerivedFromTheRow(t *testing.T) {
	db := dbtest.New(t)
	for _, e := range []database.AuditEntry{
		{ActorType: "user", ActorID: "u1", Action: "app.stop", TargetType: "app", TargetID: "app-target"},
		{ActorType: "app", ActorID: "app-actor", Action: "store.presign", TargetType: "storage_resource", TargetID: "s1"},
		{ActorType: "user", ActorID: "u1", Action: "access_request.deny", TargetType: "access_request", TargetID: "r1", AppID: "app-explicit"},
		{ActorType: "user", ActorID: "u1", Action: "user.lock", TargetType: "user", TargetID: "u2"},
	} {
		if err := db.WriteAuditRow(e); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Conn().Query(`SELECT action, app_id FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var a, id string
		if err := rows.Scan(&a, &id); err != nil {
			t.Fatal(err)
		}
		got[a] = id
	}
	for action, want := range map[string]string{
		"app.stop": "app-target", "store.presign": "app-actor",
		"access_request.deny": "app-explicit", "user.lock": "",
	} {
		if got[action] != want {
			t.Errorf("%s: app_id = %q, want %q", action, got[action], want)
		}
	}
}

// Every action has a scope by its most specific pattern, and an exact entry
// beats its family.
func TestScopeOfAuditActionTakesTheMostSpecificPattern(t *testing.T) {
	for action, want := range map[string]database.AuditScope{
		"app.stop":                      database.AuditScopeApp,
		"app.create_refused_tier":       database.AuditScopeInstall,
		"credential.attach":             database.AuditScopeApp,
		"credential.create":             database.AuditScopeInstall,
		"integration.connection.create": database.AuditScopeInstall,
		"integration.attached":          database.AuditScopeApp,
		"access_request.deny":           database.AuditScopeEither,
	} {
		if got, ok := database.ScopeOfAuditAction(action); !ok || got != want {
			t.Errorf("%s: scope = %q (%v), want %q", action, got, ok, want)
		}
	}
	if _, ok := database.ScopeOfAuditAction("nothing.at_all"); ok {
		t.Error("an unknown action has a scope")
	}
}

// The backfill reads the log and nothing else, and the statements under test
// are the migration's own, read out of the file that ships them.
//
// A resource attached to app B now, whose mapping row was written about app A,
// stays A's: the row's target is what it says, whatever is mapped today. A
// denial inherits the app of the request it decided.
func TestTheAppIDBackfillReadsOnlyTheLog(t *testing.T) {
	db := dbtest.New(t)
	insert := func(id, actorType, actorID, action, targetType, targetID, after string) {
		t.Helper()
		var afterArg any
		if after != "" {
			afterArg = after
		}
		if _, err := db.Conn().Exec(`INSERT INTO audit_log (id, actor_type, actor_id, action, target_type, target_id, after, app_id)
			VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, '')`, id, actorType, actorID, action, targetType, targetID, afterArg); err != nil {
			t.Fatal(err)
		}
	}
	insert("1", "user", "u1", "storage_mapping.update", "app", "app-A", "")
	insert("2", "app", "app-P", "store.presign", "storage_resource", "s1", "")
	insert("3", "user", "u1", "access_request.create", "object_access", "req-1", `{"object_id": "app-R"}`)
	insert("4", "user", "u2", "access_request.deny", "access_request", "req-1", "")
	insert("5", "user", "u1", "access_request.create", "role_promotion", "req-2", `{"object_id": ""}`)
	insert("6", "user", "u1", "user.lock", "user", "u3", "")

	raw, err := os.ReadFile("migrations/00071_access_record.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	ran := 0
	for _, stmt := range strings.Split(up, ";") {
		stmt = strings.TrimSpace(stripSQLComments(stmt))
		if strings.HasPrefix(stmt, "UPDATE audit_log") {
			if _, err := db.Conn().Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
			ran++
		}
	}
	if ran != 4 {
		t.Fatalf("ran %d backfill statements, want 4 — the migration's shape moved", ran)
	}

	want := map[string]string{"1": "app-A", "2": "app-P", "3": "app-R", "4": "app-R", "5": "", "6": ""}
	for id, w := range want {
		var got string
		if err := db.Conn().QueryRow(`SELECT app_id FROM audit_log WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("row %s: app_id = %q, want %q", id, got, w)
		}
	}
}

func stripSQLComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// The deployment and its app.deploy are one transaction; with no record —
// a rollback, a refused auto-deploy — there is no row, and the claimed commit
// a system starter already knows rides on the record, not the deployment.
func TestCreateDeploymentWritesItsRecordWithIt(t *testing.T) {
	db := dbtest.New(t)
	app := seedAuditApp(t, db)

	d := &database.Deployment{AppID: app, Branch: "main", Status: database.DeployStatusPending,
		TriggeredBy: database.DeployTriggerAuto}
	if err := db.CreateDeployment(d, &database.DeployRecord{ActorType: "system", Via: "git", CommitSHA: "abc123"}); err != nil {
		t.Fatal(err)
	}
	var after, actorType string
	if err := db.Conn().QueryRow(`SELECT after::text, actor_type FROM audit_log WHERE action = ? AND app_id = ?`,
		database.ActionAppDeploy, app).Scan(&after, &actorType); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"deployment_id": "` + d.ID + `"`, `"commit_sha": "abc123"`, `"via": "git"`, `"trigger": "auto"`} {
		if !strings.Contains(after, want) {
			t.Errorf("app.deploy after = %s, want %s", after, want)
		}
	}
	if actorType != "system" {
		t.Errorf("actor_type = %q, want system", actorType)
	}
	var sha string
	if err := db.Conn().QueryRow(`SELECT commit_sha FROM deployments WHERE id = ?`, d.ID).Scan(&sha); err != nil || sha != "" {
		t.Errorf("deployment commit_sha = %q (%v), want empty until the build stamps it", sha, err)
	}

	if err := db.CreateDeployment(&database.Deployment{AppID: app, Status: database.DeployStatusPending,
		TriggeredBy: database.DeployTriggerRollback}, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, database.ActionAppDeploy).Scan(&n); err != nil || n != 1 {
		t.Errorf("app.deploy rows = %d (%v), want 1 — a nil record writes none", n, err)
	}
}

func seedAuditApp(t *testing.T, db *database.DB) string {
	t.Helper()
	if err := db.UpsertUser(&database.User{ID: "owner", Login: "owner", SystemRole: database.SystemRolePublisher}); err != nil {
		t.Fatal(err)
	}
	app := &database.App{Slug: "audited", Name: "Audited", OwnerID: "owner", Status: database.AppStatusRunning}
	if err := db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	return app.ID
}
