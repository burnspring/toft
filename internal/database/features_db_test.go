package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// DB2-2: the after-state was read on the pool, where the transaction's own
// write is invisible, so a first enable audited after as null — the one row
// that says who turned a feature on said nobody had.
func TestFeatureEnableAuditsTheStateItWrote(t *testing.T) {
	db := dbtest.New(t)
	if err := db.SetFeatureEnabled("integrations", true, "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	var enabled *bool
	if err := db.Conn().QueryRow(`
		SELECT (after->>'enabled')::boolean FROM audit_log
		WHERE action='feature.enable' AND target_id='integrations'
		ORDER BY occurred_at DESC LIMIT 1`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled == nil || !*enabled {
		t.Fatalf("audit after.enabled = %v, want true", enabled)
	}
}
