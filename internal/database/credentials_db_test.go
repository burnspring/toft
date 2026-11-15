package database_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The credential store's tests. The first two lead because their failure would
// be **silent**: a value where nobody would look for it, and a rotation that
// reports success while every app keeps the old password.

// A trivial cipher. The real one is internal/crypto; what these tests need is
// something that makes plaintext and stored-form distinguishable, so a test can
// assert a value was never stored in the clear.

func credStr(v string) *string { return &v }

func fakeEncrypt(v string) (string, error) { return "enc:" + v, nil }
func fakeDecrypt(v string) (string, error) { return strings.TrimPrefix(v, "enc:"), nil }

func mkCredential(t *testing.T, db *database.DB, name string, values map[string]string) *database.Credential {
	t.Helper()
	c, err := db.CreateCredential(context.Background(), name, "", values, fakeEncrypt,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"})
	if err != nil {
		t.Fatalf("CreateCredential(%s): %v", name, err)
	}
	return c
}

func mkCredApp(t *testing.T, db *database.DB, slug string) *database.App {
	t.Helper()
	app := &database.App{
		ID: slug, Slug: slug, Name: slug, OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// SILENT #1. The entire feature is that the owner never holds the value. If one
// leaked into any read, it would look exactly like the feature working — a page
// rendering normally, with the warehouse password on it.
func TestNoCredentialReadEverReturnsAValue(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	c := mkCredential(t, db, "warehouse-prod", map[string]string{
		"SNOWFLAKE_USER":     "svc_reports",
		"SNOWFLAKE_PASSWORD": "hunter2-the-actual-secret",
	})
	if err := db.AttachCredential(context.Background(), app.ID, c.ID,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}); err != nil {
		t.Fatal(err)
	}

	const secret = "hunter2-the-actual-secret"

	list, err := db.ListCredentials()
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "ListCredentials", list, secret)

	held, err := db.AppCredentials(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "AppCredentials", held, secret)

	// And the keys *are* there, because an owner debugging a connection has to
	// know where SNOWFLAKE_USER came from.
	if len(held) != 1 || len(held[0].Keys) != 2 {
		t.Fatalf("AppCredentials = %+v, want one credential with two key names", held)
	}

	// The one function that does return plaintext has one caller, and it is the
	// builder assembling an environment.
	env, err := db.AppCredentialValues(app.ID, fakeDecrypt)
	if err != nil {
		t.Fatal(err)
	}
	if env["SNOWFLAKE_PASSWORD"] != secret {
		t.Errorf("the workload would not receive the value: %v", env)
	}
}

func assertNoSecret(t *testing.T, what string, v any, secret string) {
	t.Helper()
	if strings.Contains(jsonish(v), secret) {
		t.Errorf("%s returned the value itself — the owner is not supposed to be able to read it", what)
	}
}

// jsonish renders anything to a string for a substring check. Deliberately
// crude: the point is that no field anywhere carries the value, whatever it is
// called.
func jsonish(v any) string { return strings.ToLower(fmt.Sprintf("%+v", v)) }

// SILENT #2. The unapplied-change banner is a version counter on the app row, so
// a rotation that does not bump it leaves every attached app reading "up to
// date" while running the old password — and an operator who has just rotated a
// leaked credential believes they are finished.
func TestRotatingBumpsEveryAttachedAppsVersion(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	one := mkCredApp(t, db, "reports")
	two := mkCredApp(t, db, "dashboard")
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "old"})

	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	for _, app := range []*database.App{one, two} {
		if err := db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
			t.Fatal(err)
		}
		// The attach itself is a pending change; clear it by pretending a start
		// applied it, so the rotation's bump is the thing under test.
		if err := db.MarkEnvApplied(app.ID); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.UpdateCredential(context.Background(), c.ID, nil, nil,
		map[string]string{"PW": "new"}, nil, fakeEncrypt, audit); err != nil {
		t.Fatal(err)
	}

	for _, app := range []*database.App{one, two} {
		n, err := db.UnappliedEnvChanges(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("%s reads up to date after a rotation — an operator who rotated a leaked "+
				"credential would believe they were finished", app.Slug)
		}
	}

	// And both apps get the new value at their next start.
	for _, app := range []*database.App{one, two} {
		env, err := db.AppCredentialValues(app.ID, fakeDecrypt)
		if err != nil {
			t.Fatal(err)
		}
		if env["PW"] != "new" {
			t.Errorf("%s would start with %q, want the rotated value", app.Slug, env["PW"])
		}
	}
}

// Rotating one name keeps the others it was told to keep, drops the ones it was
// not, and refuses to "keep" a name that is not stored. Before `keep`, a
// rotation had to restate every value, so rotating one password on a three-name
// credential silently deleted the two left blank.
func TestRotatingOneValueKeepsTheOthers(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	c := mkCredential(t, db, "warehouse-prod", map[string]string{
		"USER": "svc", "HOST": "wh.internal", "PW": "old",
	})
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	if err := db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}

	// PW rotated, USER kept, HOST dropped.
	if err := db.UpdateCredential(context.Background(), c.ID, nil, nil,
		map[string]string{"PW": "new"}, []string{"USER"}, fakeEncrypt, audit); err != nil {
		t.Fatal(err)
	}
	env, err := db.AppCredentialValues(app.ID, fakeDecrypt)
	if err != nil {
		t.Fatal(err)
	}
	if env["PW"] != "new" || env["USER"] != "svc" {
		t.Errorf("after rotating PW and keeping USER, env = %v", env)
	}
	if _, ok := env["HOST"]; ok {
		t.Errorf("HOST was neither sent nor kept, and survived: %v", env)
	}

	// Keeping only — dropping a name without retyping the rest — is a change.
	if err := db.UpdateCredential(context.Background(), c.ID, nil, nil,
		map[string]string{}, []string{"PW"}, fakeEncrypt, audit); err != nil {
		t.Fatal(err)
	}
	env, err = db.AppCredentialValues(app.ID, fakeDecrypt)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env["PW"] != "new" {
		t.Errorf("after keeping only PW, env = %v", env)
	}

	// A kept name that is not stored is refused, and nothing changes.
	err = db.UpdateCredential(context.Background(), c.ID, nil, nil,
		map[string]string{"PW": "newer"}, []string{"HOST"}, fakeEncrypt, audit)
	if err == nil || !strings.Contains(err.Error(), "cannot be kept") {
		t.Fatalf("keeping a name that is not stored = %v, want a refusal", err)
	}
	env, _ = db.AppCredentialValues(app.ID, fakeDecrypt)
	if env["PW"] != "new" {
		t.Errorf("a refused update still changed PW to %q", env["PW"])
	}

	// Keeping nothing and sending nothing supplies nothing.
	if err := db.UpdateCredential(context.Background(), c.ID, nil, nil,
		map[string]string{}, []string{}, fakeEncrypt, audit); err == nil {
		t.Error("an update that would leave the credential empty was accepted")
	}
}

// A collision is refused where a person is looking, because both precedences are
// defensible and both are silent.
func TestAKeyCollisionRefusesTheAttachment(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	if err := db.SetEnvVar(app.ID, "PW", "enc:mine", true); err != nil {
		t.Fatal(err)
	}
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "theirs", "USER": "svc"})

	err := db.AttachCredential(context.Background(), app.ID, c.ID,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"})
	var collision *database.CollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("AttachCredential = %v, want a collision error", err)
	}
	if len(collision.Collisions) != 1 || collision.Collisions[0].Key != "PW" {
		t.Errorf("collisions = %+v, want exactly PW", collision.Collisions)
	}
	if collision.Collisions[0].From == "" {
		t.Error("a collision that does not say what already supplies the key is not actionable")
	}

	// Nothing was attached on the way to the refusal.
	held, err := db.AppCredentials(app.ID)
	if err != nil || len(held) != 0 {
		t.Errorf("a refused attach left %+v behind", held)
	}
}

// The mirror: an owner adding a variable a credential supplies is told the
// credential's name, which is on their app's page anyway.
func TestAnOwnersVariableIsRefusedWhenACredentialSuppliesIt(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "theirs"})
	if err := db.AttachCredential(context.Background(), app.ID, c.ID,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}); err != nil {
		t.Fatal(err)
	}

	name, err := db.EnvKeyIsFromCredential(app.ID, "PW")
	if err != nil {
		t.Fatal(err)
	}
	if name != "warehouse-prod" {
		t.Errorf("EnvKeyIsFromCredential = %q, want the credential's name", name)
	}
	if other, _ := db.EnvKeyIsFromCredential(app.ID, "SOMETHING_ELSE"); other != "" {
		t.Errorf("an unrelated key reported %q", other)
	}
}

// DB1-3: the collision check ran on the pool before the attach's
// transaction, so two attaches of credentials sharing a key could both pass,
// and an owner's save of the key was never checked inside its write. Both now
// serialize on the app row and check inside the lock.
func TestConcurrentAttachesNeverGiveOneKeyTwoSuppliers(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	for round := 0; round < 5; round++ {
		a := mkCredential(t, db, fmt.Sprintf("a-%d", round), map[string]string{fmt.Sprintf("K%d", round): "1"})
		b := mkCredential(t, db, fmt.Sprintf("b-%d", round), map[string]string{fmt.Sprintf("K%d", round): "2"})
		errs := make(chan error, 2)
		for _, c := range []*database.Credential{a, b} {
			go func(id string) { errs <- db.AttachCredential(context.Background(), app.ID, id, audit) }(c.ID)
		}
		refused := 0
		for i := 0; i < 2; i++ {
			var collision *database.CollisionError
			if err := <-errs; errors.As(err, &collision) {
				refused++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if refused != 1 {
			t.Fatalf("round %d: %d of two colliding attaches refused, want exactly 1", round, refused)
		}
	}

	var from *database.EnvKeyFromCredentialError
	if err := db.SetEnvVar(app.ID, "K0", "enc:mine", false); !errors.As(err, &from) {
		t.Fatalf("SetEnvVar of a credential's key = %v, want EnvKeyFromCredentialError", err)
	}
}

// Deleting an attached credential is refused, and the refusal can name the apps.
func TestDeletingAnAttachedCredentialIsRefused(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "x"})
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	if err := db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteCredential(context.Background(), c.ID, audit); !errors.Is(err, database.ErrCredentialInUse) {
		t.Fatalf("DeleteCredential = %v, want ErrCredentialInUse", err)
	}
	apps, err := db.AppsUsingCredential(c.ID)
	if err != nil || len(apps) != 1 || apps[0] != "reports" {
		t.Errorf("AppsUsingCredential = %v, %v", apps, err)
	}

	// Detached, it goes.
	if err := db.DetachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCredential(context.Background(), c.ID, audit); err != nil {
		t.Errorf("DeleteCredential after detach: %v", err)
	}
}

// No audit row this file writes carries a value, in any of the five actions.
// The whole point of a credential is that its values live in one place, and an
// audit log is a second place.
func TestNoAuditRowCarriesACredentialValue(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "admin2", database.SystemRoleAdmin, nil)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "admin2"}

	const secret = "the-actual-secret-value"
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": secret})
	if err := db.UpdateCredential(context.Background(), c.ID, nil, credStr("rotated"),
		map[string]string{"PW": secret + "-2"}, nil, fakeEncrypt, audit); err != nil {
		t.Fatal(err)
	}
	if err := db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}
	if err := db.DetachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCredential(context.Background(), c.ID, audit); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Conn().Query(
		`SELECT action, COALESCE(before::text, ''), COALESCE(after::text, '') FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var action, before, after string
		if err := rows.Scan(&action, &before, &after); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(action, "credential.") {
			seen++
		}
		if strings.Contains(before+after, secret) {
			t.Errorf("%s carried a credential value in the audit log", action)
		}
	}
	if seen < 5 {
		t.Errorf("wrote %d credential audit rows, want all five acts recorded", seen)
	}
}

// Two credentials cannot both supply one key on one app: the second attach is
// refused with the first's name, so the administrator knows which to change.
func TestASecondCredentialSupplyingTheSameKeyIsRefused(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}

	first := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "a"})
	second := mkCredential(t, db, "warehouse-staging", map[string]string{"PW": "b"})
	if err := db.AttachCredential(context.Background(), app.ID, first.ID, audit); err != nil {
		t.Fatal(err)
	}

	err := db.AttachCredential(context.Background(), app.ID, second.ID, audit)
	var collision *database.CollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("the second attach = %v, want a collision", err)
	}
	if collision.Collisions[0].From != "warehouse-prod" {
		t.Errorf("collision names %q, want the credential already supplying it",
			collision.Collisions[0].From)
	}
}

// A name is unique: it is what an administrator attaches by and what an app
// owner sees, so two of them is an ambiguity in both directions.
func TestACredentialNameIsUnique(t *testing.T) {
	db := dbtest.New(t)
	mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "a"})
	_, err := db.CreateCredential(context.Background(), "warehouse-prod", "", map[string]string{"PW": "b"},
		fakeEncrypt, database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"})
	if !errors.Is(err, database.ErrCredentialNameTaken) {
		t.Errorf("a duplicate name = %v, want ErrCredentialNameTaken", err)
	}
}

// The credential's own page needs the apps by id, not by name: a name is what a
// refusal message quotes, and an id is what a detach is addressed by. An
// administrator looking at "3 apps" who cannot act on the three has to go
// looking for them one app page at a time.
func TestListCredentialAppsNamesThemAndWhoDecided(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	admin := mkUserIn(t, db, "the-admin", database.SystemRoleAdmin, nil)
	one := mkCredApp(t, db, "reports")
	two := mkCredApp(t, db, "billing")
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "x"})
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: admin.ID}

	if got, err := db.ListCredentialApps(c.ID); err != nil || len(got) != 0 {
		t.Fatalf("ListCredentialApps before any attach = %v, %v; want empty", got, err)
	}
	for _, app := range []*database.App{one, two} {
		if err := db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.ListCredentialApps(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListCredentialApps = %d rows, want 2", len(got))
	}
	// Ordered by name, so "billing" leads.
	if got[0].AppSlug != "billing" || got[1].AppSlug != "reports" {
		t.Errorf("rows = %s, %s; want billing, reports", got[0].AppSlug, got[1].AppSlug)
	}
	if got[0].AppID != two.ID {
		t.Errorf("app_id = %q, want %q — a detach is addressed by id", got[0].AppID, two.ID)
	}
	if got[0].AttachedBy != "the-admin" {
		t.Errorf("attached_by = %q, want the administrator's name", got[0].AttachedBy)
	}
	if got[0].AttachedAt.IsZero() {
		t.Error("attached_at is zero — the page renders it as when somebody decided")
	}

	// And it carries no value, like every other read in this file.
	assertNoSecret(t, "ListCredentialApps", got, "x")
}

// Renaming is the one write here that takes nothing away: no app is detached,
// nothing restarts, and no attached app is marked as carrying a pending change
// — the name is a label an administrator attaches by, never something a
// workload holds.
func TestRenamingChangesNothingOperational(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "x"})
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	if err := db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkEnvApplied(app.ID); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateCredential(context.Background(), c.ID,
		credStr("warehouse-reporting"), nil, nil, nil, fakeEncrypt, audit); err != nil {
		t.Fatal(err)
	}

	creds, err := db.ListCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 || creds[0].Name != "warehouse-reporting" {
		t.Fatalf("after rename = %+v", creds)
	}
	// The keys are untouched: a rename is not a rotation, and an attached app
	// still gets exactly what it got before.
	if len(creds[0].Keys) != 1 || creds[0].Keys[0] != "PW" {
		t.Errorf("keys = %v, want [PW] — a rename must not disturb what it supplies", creds[0].Keys)
	}
	n, err := db.UnappliedEnvChanges(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d unapplied changes after a rename, want 0 — a label change must not "+
			"tell an owner their app is running something stale", n)
	}
}

// A description left alone by a rename, and a name left alone by a rotation:
// the two update surfaces are two acts against one route, and neither may
// silently clear what the other owns.
func TestAnAbsentFieldIsOneNobodyIsChanging(t *testing.T) {
	db := dbtest.New(t)
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"PW": "x"})
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	if err := db.UpdateCredential(context.Background(), c.ID, nil,
		credStr("Read-only service account"), nil, nil, fakeEncrypt, audit); err != nil {
		t.Fatal(err)
	}

	// A rotation carries values and neither of the others.
	if err := db.UpdateCredential(context.Background(), c.ID, nil, nil,
		map[string]string{"PW": "y"}, nil, fakeEncrypt, audit); err != nil {
		t.Fatal(err)
	}
	creds, err := db.ListCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds[0].Name != "warehouse-prod" {
		t.Errorf("name = %q after a rotation, want it untouched", creds[0].Name)
	}
	if creds[0].Description != "Read-only service account" {
		t.Errorf("description = %q after a rotation, want it untouched", creds[0].Description)
	}
}

// Two credentials cannot share a name, on the rename as on the create: the name
// is what an administrator attaches by and what an app owner sees, so two of
// them is an ambiguity in both directions.
func TestRenamingOntoATakenNameIsRefused(t *testing.T) {
	db := dbtest.New(t)
	first := mkCredential(t, db, "warehouse-prod", map[string]string{"A": "x"})
	second := mkCredential(t, db, "warehouse-dev", map[string]string{"B": "y"})
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}

	err := db.UpdateCredential(context.Background(), second.ID,
		credStr("warehouse-prod"), nil, nil, nil, fakeEncrypt, audit)
	if !errors.Is(err, database.ErrCredentialNameTaken) {
		t.Fatalf("rename onto a taken name = %v, want ErrCredentialNameTaken", err)
	}

	// Renaming to what it already is is not a collision with itself.
	if err := db.UpdateCredential(context.Background(), first.ID,
		credStr("warehouse-prod"), credStr("same name, new note"), nil, nil, fakeEncrypt, audit); err != nil {
		t.Errorf("renaming to its own name: %v", err)
	}

	// And an empty one is refused rather than stored.
	if err := db.UpdateCredential(context.Background(), first.ID,
		credStr("   "), nil, nil, nil, fakeEncrypt, audit); err == nil {
		t.Error("renaming to blank was accepted")
	}
}

// DB1-3: a rotation that adds a key an attached app already has — its own
// variable, or another attached credential's — is refused like the attach
// would be, and nothing of the rotation lands.
func TestARotationThatAddsACollidingKeyIsRefused(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	if err := db.SetEnvVar(app.ID, "PW", "enc:mine", true); err != nil {
		t.Fatal(err)
	}
	c := mkCredential(t, db, "warehouse-prod", map[string]string{"USER": "svc"})
	other := mkCredential(t, db, "smtp", map[string]string{"HOST": "mail"})
	for _, id := range []string{c.ID, other.ID} {
		if err := db.AttachCredential(context.Background(), app.ID, id, audit); err != nil {
			t.Fatal(err)
		}
	}

	for key, from := range map[string]string{"PW": "this app's own variables", "HOST": "smtp"} {
		err := db.UpdateCredential(context.Background(), c.ID, nil, nil,
			map[string]string{key: "new"}, []string{"USER"}, fakeEncrypt, audit)
		var collision *database.CollisionError
		if !errors.As(err, &collision) {
			t.Fatalf("rotation adding %s = %v, want a collision error", key, err)
		}
		if collision.App != "reports" || len(collision.Collisions) != 1 ||
			collision.Collisions[0].Key != key || collision.Collisions[0].From != from {
			t.Errorf("rotation adding %s: %+v on %q, want %s from %q", key, collision.Collisions, collision.App, key, from)
		}
	}
	held, err := db.AppCredentials(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		if h.ID == c.ID && (len(h.Keys) != 1 || h.Keys[0] != "USER") {
			t.Errorf("a refused rotation left keys %v, want [USER]", h.Keys)
		}
	}

	// Rotating the keys it already supplies is no collision with itself.
	if err := db.UpdateCredential(context.Background(), c.ID, nil, nil,
		map[string]string{"USER": "svc2"}, nil, fakeEncrypt, audit); err != nil {
		t.Errorf("plain rotation: %v", err)
	}
}

// A rotation adding a key and an attach of a credential supplying it, at once:
// both lock the app row, so exactly one is refused.
func TestARotationAndAnAttachNeverGiveOneKeyTwoSuppliers(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}
	for round := 0; round < 5; round++ {
		key := fmt.Sprintf("K%d", round)
		held := mkCredential(t, db, fmt.Sprintf("held-%d", round), map[string]string{fmt.Sprintf("H%d", round): "1"})
		if err := db.AttachCredential(context.Background(), app.ID, held.ID, audit); err != nil {
			t.Fatal(err)
		}
		incoming := mkCredential(t, db, fmt.Sprintf("in-%d", round), map[string]string{key: "2"})
		errs := make(chan error, 2)
		go func() { errs <- db.AttachCredential(context.Background(), app.ID, incoming.ID, audit) }()
		go func() {
			errs <- db.UpdateCredential(context.Background(), held.ID, nil, nil,
				map[string]string{key: "1"}, []string{fmt.Sprintf("H%d", round)}, fakeEncrypt, audit)
		}()
		refused := 0
		for i := 0; i < 2; i++ {
			var collision *database.CollisionError
			if err := <-errs; errors.As(err, &collision) {
				refused++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if refused != 1 {
			t.Fatalf("round %d: %d of a colliding rotation and attach refused, want exactly 1", round, refused)
		}
	}
}
