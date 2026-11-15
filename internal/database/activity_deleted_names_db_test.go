package database_test

import (
	"fmt"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// BB-113: naming a deleted app's rows looked its app.delete row up with no
// target_type, so audit_log_target_idx could not serve it and every row of a
// deleted app scanned the log — a page of Activity took ~43 s on trytoft, and
// 3–9 s on this seed. Indexed it is ~0.1 s; the bound is generous on purpose.
func TestTheActivityPageNamesDeletedAppsWithoutScanningTheLog(t *testing.T) {
	db := dbtest.New(t)
	c := db.Conn()
	for i := 0; i < 400; i++ {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		if _, err := c.Exec(`INSERT INTO audit_log (id, occurred_at, actor_type, actor_id, action, target_type, target_id, app_id, before)
			VALUES (gen_random_uuid(), now(), 'user', 'x', 'app.delete', 'app', ?, ?, '{"slug":"gone"}')`, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(`INSERT INTO audit_log (id, occurred_at, actor_type, actor_id, action, target_type, target_id, app_id)
			SELECT gen_random_uuid(), now() - (g||' minutes')::interval, 'user', 'x', 'app.deploy', 'app', ?, ?
			FROM generate_series(1,20) g`, id, id); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	got, err := db.ListActivity(database.ActivityFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("a five-row page took %v over 8400 rows of deleted apps", took)
	}
	if len(got) == 0 || got[0].TargetLabel != "gone" {
		t.Errorf("a deleted app's row is not named by its recorded slug: %+v", got)
	}
}
