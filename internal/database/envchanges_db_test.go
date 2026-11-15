package database_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// fakeCipher is credentials_db_test's trivial cipher as an EnvCipher.
type fakeCipher struct{}

func (fakeCipher) Encrypt(v string) (string, error) { return fakeEncrypt(v) }
func (fakeCipher) Decrypt(v string) (string, error) { return fakeDecrypt(v) }

func envStr(v string) *string { return &v }
func envBool(v bool) *bool    { return &v }

func envRows(t *testing.T, db *database.DB, appID string) map[string]database.EnvVar {
	t.Helper()
	vars, err := db.GetEnvVars(appID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]database.EnvVar{}
	for _, v := range vars {
		out[v.Key] = v
	}
	return out
}

func applyEnv(t *testing.T, db *database.DB, appID string, set map[string]database.EnvSet, unset []string, onlyMissing bool) (database.EnvChangeResult, error) {
	t.Helper()
	return db.ApplyEnvChanges(appID, set, unset, onlyMissing, fakeCipher{},
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "owner", Action: "app.env_change", TargetType: "app", TargetID: appID})
}

// SILENT #2 for the environment: no write path stores a key an attached
// credential supplies, because at start the credential would shadow it and
// the owner's value would be a row that does nothing.
func TestNoEnvWritePathStoresACredentialsKey(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "reports")
	c := mkCredential(t, db, "warehouse", map[string]string{"SNOWFLAKE_USER": "svc"})
	if err := db.AttachCredential(context.Background(), app.ID, c.ID,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}); err != nil {
		t.Fatal(err)
	}

	var fromCred *database.EnvKeyFromCredentialError
	if _, err := db.AddEnvVarIfAbsent(app.ID, "SNOWFLAKE_USER", "enc:", true); !errors.As(err, &fromCred) || fromCred.Credential != "warehouse" {
		t.Fatalf("AddEnvVarIfAbsent = %v, want the credential named", err)
	}
	_, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{
		"OK":             {Value: envStr("v")},
		"SNOWFLAKE_USER": {Value: envStr("mine")},
	}, nil, false)
	if !errors.As(err, &fromCred) {
		t.Fatalf("ApplyEnvChanges = %v, want key_from_credential", err)
	}
	if rows := envRows(t, db, app.ID); len(rows) != 0 {
		t.Fatalf("a refused request wrote %v; one bad key must write nothing", rows)
	}
}

func TestApplyEnvChanges(t *testing.T) {
	db := dbtest.New(t)
	mkUserIn(t, db, "owner", database.SystemRolePublisher, nil)
	app := mkCredApp(t, db, "orders")
	if err := db.SetEnvVar(app.ID, "SECRET", "enc:old", true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetEnvVar(app.ID, "HOST", "enc:db1", false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetEnvVar(app.ID, "UNTOUCHED", "enc:same", false); err != nil {
		t.Fatal(err)
	}

	t.Run("new key is visible; unmentioned key untouched; empty clears a secret", func(t *testing.T) {
		res, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{
			"NEW":    {Value: envStr("x")},
			"SECRET": {Value: envStr("")},
			"HOST":   {Value: envStr("db1")}, // unchanged: reported apart, not counted
		}, []string{"GONE"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res.Set, []string{"NEW"}) || !reflect.DeepEqual(res.Unchanged, []string{"HOST"}) ||
			!reflect.DeepEqual(res.Cleared, []string{"SECRET"}) ||
			!reflect.DeepEqual(res.NotPresent, []string{"GONE"}) || res.Changed != 2 {
			t.Fatalf("result = %+v", res)
		}
		rows := envRows(t, db, app.ID)
		if rows["NEW"].Sensitive {
			t.Error("a new key without a flag must be visible: the caller decides what is a secret")
		}
		if rows["SECRET"].Value != "enc:" || !rows["SECRET"].Sensitive {
			t.Errorf("SECRET = %+v, want cleared and still sensitive", rows["SECRET"])
		}
		if rows["UNTOUCHED"].Value != "enc:same" {
			t.Errorf("an unmentioned key changed: %+v", rows["UNTOUCHED"])
		}
	})

	t.Run("only_missing keeps what is stored", func(t *testing.T) {
		res, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{
			"HOST":  {Value: envStr("db2")},
			"FRESH": {Value: envStr("f")},
		}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res.Kept, []string{"HOST"}) || !reflect.DeepEqual(res.Set, []string{"FRESH"}) {
			t.Fatalf("result = %+v", res)
		}
		if envRows(t, db, app.ID)["HOST"].Value != "enc:db1" {
			t.Error("only_missing overwrote a stored value")
		}
	})

	t.Run("only_missing fills an empty variable, secret or not", func(t *testing.T) {
		if err := db.SetEnvVar(app.ID, "DECLARED", "enc:", true); err != nil {
			t.Fatal(err)
		}
		if err := db.SetEnvVar(app.ID, "CHOSEN_EMPTY", "enc:", false); err != nil {
			t.Fatal(err)
		}
		res, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{
			"DECLARED":     {Value: envStr("filled")},
			"CHOSEN_EMPTY": {Value: envStr("x")},
		}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res.Set, []string{"CHOSEN_EMPTY", "DECLARED"}) || len(res.Kept) != 0 {
			t.Fatalf("result = %+v", res)
		}
		rows := envRows(t, db, app.ID)
		if rows["DECLARED"].Value != "enc:filled" || !rows["DECLARED"].Sensitive {
			t.Errorf("DECLARED = %+v, want filled and still sensitive", rows["DECLARED"])
		}
	})

	t.Run("a flag never reveals a stored secret", func(t *testing.T) {
		if err := db.SetEnvVar(app.ID, "PW", "enc:hunter2", true); err != nil {
			t.Fatal(err)
		}
		var refused *database.EnvChangeError
		_, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{"PW": {Sensitive: envBool(false)}}, nil, false)
		if !errors.As(err, &refused) || refused.Code != "sensitive_needs_value" {
			t.Fatalf("err = %v, want sensitive_needs_value", err)
		}
		if _, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{"PW": {Value: envStr("open"), Sensitive: envBool(false)}}, nil, false); err != nil {
			t.Fatalf("a flip with its value: %v", err)
		}
		if r := envRows(t, db, app.ID)["PW"]; r.Sensitive || r.Value != "enc:open" {
			t.Errorf("PW = %+v", r)
		}
		// Towards sensitive needs no value.
		if _, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{"PW": {Sensitive: envBool(true)}}, nil, false); err != nil {
			t.Fatal(err)
		}
		if r := envRows(t, db, app.ID)["PW"]; !r.Sensitive || r.Value != "enc:open" {
			t.Errorf("PW = %+v, want the value kept and the flag closed", r)
		}
	})

	t.Run("the audit row names keys, never a value", func(t *testing.T) {
		if _, err := applyEnv(t, db, app.ID, map[string]database.EnvSet{"ROTATED": {Value: envStr("the-actual-secret")}}, []string{"FRESH"}, false); err != nil {
			t.Fatal(err)
		}
		rows, err := db.Conn().Query(`SELECT COALESCE(after::text,'') FROM audit_log WHERE action='app.env_change' AND target_id=? ORDER BY occurred_at`, app.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var afters []string
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				t.Fatal(err)
			}
			afters = append(afters, a)
		}
		if len(afters) == 0 {
			t.Fatal("no app.env_change row")
		}
		for _, a := range afters {
			if strings.Contains(a, "the-actual-secret") || strings.Contains(a, "hunter2") || strings.Contains(a, `"open"`) {
				t.Fatalf("audit After carries a value: %s", a)
			}
		}
		if last := afters[len(afters)-1]; !strings.Contains(last, "ROTATED") || !strings.Contains(last, "FRESH") {
			t.Errorf("latest audit row = %s, want the keys named", last)
		}
	})
}
