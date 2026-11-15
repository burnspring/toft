package builder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func testEncryptor(t *testing.T) *crypto.Encryptor {
	t.Helper()
	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

func envRowsOf(t *testing.T, db *database.DB, enc *crypto.Encryptor, appID string) map[string]database.EnvVar {
	t.Helper()
	rows, err := db.GetEnvVars(appID)
	if err != nil {
		t.Fatalf("GetEnvVars: %v", err)
	}
	out := map[string]database.EnvVar{}
	for _, v := range rows {
		plain, err := enc.Decrypt(v.Value)
		if err != nil {
			t.Fatalf("Decrypt %s: %v", v.Key, err)
		}
		v.Value = plain
		out[v.Key] = v
	}
	return out
}

// Every [env] refusal, with its code and field, from both doors: the deploy's
// parse and `toft check`'s preflight.
func TestEnvTableRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, field string
	}{
		{"a name no shell can read", "[env]\n1BAD = \"x\"\n", "invalid_env_name", "env.1BAD"},
		{"a name the platform sets", "[env]\nPORT = \"8000\"\n", "reserved_env_name", "env.PORT"},
		{"a secret with a default", "[env]\nTOKEN = { sensitive = true, default = \"x\" }\n", EnvSensitiveDefaultCode, "env.TOKEN"},
		{"a number", "[env]\nWORKERS = 4\n", EnvEntryInvalidCode, "env.WORKERS"},
		{"a flag that is not a bool", "[env]\nA = { sensitive = \"yes\" }\n", EnvEntryInvalidCode, "env.A.sensitive"},
		{"a default that is not a string", "[env]\nA = { default = 3 }\n", EnvEntryInvalidCode, "env.A.default"},
		{"a key nothing reads", "[env]\nA = { secret = true }\n", EnvEntryInvalidCode, "env.A.secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeManifest(t, tc.body)
			_, _, err := ParseManifestWithWarnings(dir)
			var me ManifestError
			if !errors.As(err, &me) || me.Code != tc.code || me.Field != tc.field {
				t.Fatalf("ParseManifestWithWarnings = %#v, want %s on %s", err, tc.code, tc.field)
			}
			if tc.code == EnvSensitiveDefaultCode && !strings.Contains(me.Hint, "toft env set --from-file") {
				t.Errorf("hint = %q, want the cure named", me.Hint)
			}
			p := RunPreflight(dir, "shared", nil, config.Envelope{})
			found := false
			for _, pp := range p.Problems {
				found = found || pp.Code == tc.code
			}
			if !found {
				t.Errorf("preflight problems = %+v, want %s", p.Problems, tc.code)
			}
		})
	}
}

func TestEnvDeclsAreRead(t *testing.T) {
	dir := writeManifest(t, "[env]\nDB_HOST = \"db.internal\"\nAPI_URL = {}\n"+
		"LOG_LEVEL = { default = \"info\", sensitive = false }\nDB_PASSWORD = { sensitive = true }\n")
	decls, err := ParseEnvDecls(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvDecl{
		{Name: "API_URL"},
		{Name: "DB_HOST", Default: "db.internal"},
		{Name: "DB_PASSWORD", Sensitive: true},
		{Name: "LOG_LEVEL", Default: "info"},
	}
	if fmt.Sprint(decls) != fmt.Sprint(want) {
		t.Fatalf("ParseEnvDecls = %+v, want %+v", decls, want)
	}
	if _, warnings, err := ParseManifestWithWarnings(dir); err != nil || len(warnings) != 0 {
		t.Fatalf("[env] warned or refused: %v %v", warnings, err)
	}
	if decls, err := ParseEnvDecls(writeManifest(t, "")); err != nil || decls != nil {
		t.Fatalf("no manifest = %v, %v", decls, err)
	}
}

// A stored row survives every deploy — value and flag, including one its
// owner flipped; a new variable arrives with its default and the file's flag;
// a variable the file stopped declaring stays; a deleted one comes back.
func TestApplyEnvDeclsIsAddOnly(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t)
	b := testBuilder(t, db)
	b.encryptor = enc
	app := mkApp(t, db, "declared", "")

	tuned, _ := enc.Encrypt("db.prod.internal")
	if err := db.SetEnvVar(app.ID, "DB_HOST", tuned, true); err != nil {
		t.Fatal(err)
	}
	secret, _ := enc.Encrypt("hunter2")
	if err := db.SetEnvVar(app.ID, "DB_PASSWORD", secret, true); err != nil {
		t.Fatal(err)
	}

	var logs []string
	logf := func(s string) { logs = append(logs, s) }
	deploy := func(body string) {
		t.Helper()
		if err := b.applyEnvDecls(app, writeManifest(t, body), logf); err != nil {
			t.Fatalf("applyEnvDecls: %v", err)
		}
	}
	deploy("[env]\nDB_HOST = \"db.default\"\nDB_PASSWORD = { sensitive = true }\n" +
		"API_TOKEN = { sensitive = true }\nLOG_LEVEL = \"info\"\n")

	rows := envRowsOf(t, db, enc, app.ID)
	if r := rows["DB_HOST"]; r.Value != "db.prod.internal" || !r.Sensitive {
		t.Errorf("DB_HOST = %+v, want the stored value and the owner's flag untouched", r)
	}
	if r := rows["DB_PASSWORD"]; r.Value != "hunter2" || !r.Sensitive {
		t.Errorf("DB_PASSWORD = %+v, want kept", r)
	}
	if r, ok := rows["API_TOKEN"]; !ok || r.Value != "" || !r.Sensitive {
		t.Errorf("API_TOKEN = %+v (present %v), want empty and sensitive", r, ok)
	}
	if r, ok := rows["LOG_LEVEL"]; !ok || r.Value != "info" || r.Sensitive {
		t.Errorf("LOG_LEVEL = %+v (present %v), want its default, visible", r, ok)
	}
	if last := logs[len(logs)-1]; last != "==> toft.toml [env]: added API_TOKEN, LOG_LEVEL (4 declared, 2 already set)" {
		t.Errorf("log = %q", last)
	}

	// Dropped from the file: kept. Deleted on the platform: back.
	if err := db.DeleteEnvVar(app.ID, "API_TOKEN"); err != nil {
		t.Fatal(err)
	}
	deploy("[env]\nAPI_TOKEN = { sensitive = true }\n")
	rows = envRowsOf(t, db, enc, app.ID)
	if _, ok := rows["DB_HOST"]; !ok {
		t.Error("a variable the file stopped declaring was deleted")
	}
	if r, ok := rows["API_TOKEN"]; !ok || !r.Sensitive {
		t.Error("a deleted declared variable did not come back")
	}
}

// A name an attached credential supplies is skipped and said, and the deploy
// goes on.
func TestApplyEnvDeclsSkipsACredentialsKey(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t)
	b := testBuilder(t, db)
	b.encryptor = enc
	app := mkApp(t, db, "warehoused", "")
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID}
	c, err := db.CreateCredential(context.Background(), "warehouse", "", map[string]string{"SNOWFLAKE_USER": "svc"}, enc.Encrypt, audit)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}

	var logs []string
	if err := b.applyEnvDecls(app, writeManifest(t, "[env]\nSNOWFLAKE_USER = {}\nOTHER = \"x\"\n"),
		func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatalf("applyEnvDecls: %v", err)
	}
	rows := envRowsOf(t, db, enc, app.ID)
	if _, ok := rows["SNOWFLAKE_USER"]; ok {
		t.Error("seeded a key the credential supplies")
	}
	if _, ok := rows["OTHER"]; !ok {
		t.Error("the other name was not seeded")
	}
	if !strings.Contains(strings.Join(logs, "\n"), `SNOWFLAKE_USER is supplied by the "warehouse" credential`) {
		t.Errorf("logs = %v", logs)
	}
}
