package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// A search box's text is matched literally. Postgres's default LIKE escape is
// the backslash, so a query ending in one was a malformed pattern and a 500,
// and a `_` or `%` matched anything.
func TestSearchesMatchWildcardsLiterally(t *testing.T) {
	db := dbtest.New(t)
	if err := db.WriteAuditRow(database.AuditEntry{
		ActorType: database.PrincipalTypeUser, Action: "app.never_sleep_change",
		TargetType: "app", TargetID: "x",
	}); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{`foo\`, `%`, `\`} {
		if _, err := db.ListActivity(database.ActivityFilter{Query: q}); err != nil {
			t.Errorf("activity search %q: %v", q, err)
		}
		if _, err := db.SearchPackageOccurrences(database.PackageFilter{Query: q}); err != nil {
			t.Errorf("package search %q: %v", q, err)
		}
	}

	got, err := db.ListActivity(database.ActivityFilter{Query: "never_sleep", Source: "audit"})
	if err != nil || len(got) != 1 {
		t.Errorf("a literal underscore found %d rows (%v), want 1", len(got), err)
	}
	if got, _ := db.ListActivity(database.ActivityFilter{Query: "never%sleep", Source: "audit"}); len(got) != 0 {
		t.Errorf("a %% matched as a wildcard: %d rows", len(got))
	}
	if got, _ := db.ListActivity(database.ActivityFilter{Query: "never_s_eep", Source: "audit"}); len(got) != 0 {
		t.Errorf("an _ matched as a wildcard: %d rows", len(got))
	}
}
