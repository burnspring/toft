package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// Seeding toft.toml [env] names rests on this primitive: adding
// never overwrites, so a value someone set through the platform survives every
// deploy that carries the same key.
func TestAddEnvVarIfAbsent(t *testing.T) {
	db := dbtest.New(t)
	app := mkVisibleApp(t, db, "envapp", database.VisibilityPrivate, "flask")

	inserted, err := db.AddEnvVarIfAbsent(app.ID, "API_URL", "enc-first", false)
	if err != nil {
		t.Fatalf("AddEnvVarIfAbsent: %v", err)
	}
	if !inserted {
		t.Fatal("first add should insert")
	}

	inserted, err = db.AddEnvVarIfAbsent(app.ID, "API_URL", "enc-second", true)
	if err != nil {
		t.Fatalf("AddEnvVarIfAbsent (second): %v", err)
	}
	if inserted {
		t.Fatal("second add should not insert")
	}

	vars, err := db.GetEnvVars(app.ID)
	if err != nil {
		t.Fatalf("GetEnvVars: %v", err)
	}
	if len(vars) != 1 || vars[0].Value != "enc-first" || vars[0].Sensitive {
		t.Fatalf("stored row = %+v, want the first write kept (value and flag)", vars)
	}
}

// The environment tag defaults to empty (feature off), stores trimmed, and
// round-trips through the typed settings document.
func TestEnvironmentTagSetting(t *testing.T) {
	db := dbtest.New(t)

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if s.EnvironmentTag != "" {
		t.Fatalf("default EnvironmentTag = %q, want empty", s.EnvironmentTag)
	}

	s.EnvironmentTag = "  stage  "
	if err := db.SetPlatformSettings(&s, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
	if s.EnvironmentTag != "stage" {
		t.Fatalf("normalized EnvironmentTag = %q, want %q", s.EnvironmentTag, "stage")
	}

	got, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings (reread): %v", err)
	}
	if got.EnvironmentTag != "stage" {
		t.Fatalf("stored EnvironmentTag = %q, want %q", got.EnvironmentTag, "stage")
	}
}
