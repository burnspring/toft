package database

import (
	"io/fs"
	"strings"
	"testing"
)

// The schema ships inside the binary. If the embed directive or the file
// naming ever breaks, migrations silently become a no-op and the app starts
// against an empty database — so assert the files are actually there.
func TestMigrationsAreEmbedded(t *testing.T) {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("globbing embedded migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded — check the //go:embed directive in migrate.go")
	}

	for _, name := range entries {
		body, err := fs.ReadFile(migrationFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		sql := string(body)

		if !strings.Contains(sql, "-- +goose Up") {
			t.Errorf("%s: missing '-- +goose Up' annotation", name)
		}
		if !strings.Contains(sql, "-- +goose Down") {
			t.Errorf("%s: missing '-- +goose Down' annotation", name)
		}

		// A plpgsql body contains semicolons that goose would otherwise split
		// into separate statements; it needs explicit Statement markers.
		if strings.Contains(sql, "$$") {
			begin := strings.Count(sql, "-- +goose StatementBegin")
			end := strings.Count(sql, "-- +goose StatementEnd")
			if begin == 0 {
				t.Errorf("%s: contains a $$-quoted body but no '-- +goose StatementBegin'", name)
			}
			if begin != end {
				t.Errorf("%s: unbalanced statement markers (%d begin, %d end)", name, begin, end)
			}
		}
	}
}

// SQLite-only syntax must not survive the Postgres port; these forms parse
// nowhere in Postgres and would only fail at runtime.
func TestMigrationsContainNoSQLiteSyntax(t *testing.T) {
	banned := []string{
		"AUTOINCREMENT",
		"INSERT OR IGNORE",
		"INSERT OR REPLACE",
		"PRAGMA",
		"RAISE(ABORT",
		"WITHOUT ROWID",
	}

	entries, _ := fs.Glob(migrationFS, "migrations/*.sql")
	for _, name := range entries {
		body, err := fs.ReadFile(migrationFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		upper := strings.ToUpper(string(body))
		for _, b := range banned {
			if strings.Contains(upper, b) {
				t.Errorf("%s: contains SQLite-only syntax %q", name, b)
			}
		}
	}
}
