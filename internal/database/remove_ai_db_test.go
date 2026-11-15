package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The three tests 00033 needs, and the reason all three are here rather than
// spread across the files that own their subjects: what they guard is one
// migration's completeness, and a migration is the one change in this codebase
// whose mistakes are silent by construction. Nothing errors, nothing logs, and
// the surface that would have shown the leftover was deleted in the same
// release.

// A ciphertext left behind after its reader is deleted is a secret nobody will
// ever rotate again.
//
// `internal/database/secrets.go` lost its "AI provider credential" row in the
// same change that wrote this migration, and the two halves have to move
// together in one direction only: the registry row is what makes a stored
// credential re-encryptable when a key is rotated and visible in the startup
// report, so removing it while the document survives produces a secret that no
// rotation touches and no surface shows — worse than either half alone.
//
// Silent because there is nothing to observe. No error, no log line, no page.
func TestNoAISettingSurvivesTheMigration(t *testing.T) {
	db := dbtest.New(t)

	rows, err := db.Conn().Query(`SELECT key FROM settings WHERE key LIKE 'ai.%'`)
	if err != nil {
		t.Fatalf("reading settings: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		t.Errorf("%q survived 00033 — if it is `ai.credential` it is a ciphertext "+
			"no key rotation will ever re-encrypt, because secrets.go no longer "+
			"names it", key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading settings: %v", err)
	}

	// And the tables, which is the loud half of the same change: a query
	// against either would fail somewhere, but only if something still ran one.
	for _, table := range []string{"ai_runs", "app_ai"} {
		var n int
		if err := db.Conn().QueryRow(`
			SELECT COUNT(*) FROM pg_tables
			WHERE schemaname = 'public' AND tablename = $1`, table).Scan(&n); err != nil {
			t.Fatalf("looking for %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s is still in the schema", table)
		}
	}
}

// Every registry entry names something the database can actually produce.
//
// The registry is a list of places a credential is stored, and it is what the
// rotate command walks and what the startup report reads. An entry naming a
// table that no longer exists is the mirror of the failure above: the sweep
// would fail rather than lie, but it would fail on every install and for a
// reason that has nothing to do with that install.
//
// This is the general form of the check 00033 needed — it costs nothing to
// keep, and the next subsystem removal gets it for free.
func TestEveryRegisteredSecretLocationStillExists(t *testing.T) {
	db := dbtest.New(t)

	if err := db.EachEncryptedSecret(func(database.EncryptedSecret) error {
		return nil
	}); err != nil {
		t.Fatalf("walking the secrets registry: %v — an entry names something "+
			"the schema no longer has", err)
	}
}
