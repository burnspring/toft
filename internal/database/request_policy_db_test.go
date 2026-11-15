package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// DB1-2: the request-policy cache was dropped only by the node that wrote the
// setting, so on a multi-replica install a shortened session ceiling never
// reached the others. A write that bypasses this node's invalidation — what
// another replica's write is, seen from here — must still land within the TTL.
func TestAnotherNodesSettingsWriteReachesThisNodeWithinTheTTL(t *testing.T) {
	db := dbtest.New(t)
	defer database.SetRequestPolicyTTLForTest(50 * time.Millisecond)()

	before := db.SessionPolicy() // warms the cache
	if _, err := db.Conn().Exec(`
		INSERT INTO settings (key, value, updated_at) VALUES (?, '1', NOW())
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, database.SettingSessionAbsoluteHours); err != nil {
		t.Fatal(err)
	}
	if before.Absolute == time.Hour {
		t.Fatal("the default already reads one hour; pick another value")
	}
	time.Sleep(100 * time.Millisecond)
	if got := db.SessionPolicy().Absolute; got != time.Hour {
		t.Fatalf("absolute = %v after the TTL, want the other node's 1h", got)
	}
}
