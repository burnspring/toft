package database_test

import (
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The sweep deletes nothing it was not told to. These are the failures no
// screen would show: an upgrade that quietly removed a year of the audit log,
// or a corrupt row read as a short retention.

func seedAccessRecord(t *testing.T, db *database.DB, appID string, ages ...int) {
	t.Helper()
	now := time.Now().UTC()
	for _, days := range ages {
		at := now.AddDate(0, 0, -days)
		if err := db.WriteAuditRow(database.AuditEntry{ActorType: "user", ActorID: "u1",
			Action: "app.stop", TargetType: "app", TargetID: appID, OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := db.BumpAppUsage([]database.UsageBump{{AppID: appID, UserID: "u1", Day: at, Requests: 1}}); err != nil {
			t.Fatal(err)
		}
		if err := db.BumpAPIKeyUsage([]database.APIKeyUsageBump{{KeyID: "k1", KeyKind: database.KeyKindUser,
			AppID: appID, Day: at, StatusClass: 2, Requests: 1}}); err != nil {
			t.Fatal(err)
		}
	}
}

func countOlder(t *testing.T, db *database.DB, cutoff time.Time) map[string]int {
	t.Helper()
	out := map[string]int{}
	for table, q := range map[string]string{
		"audit_log":           `SELECT COUNT(*) FROM audit_log WHERE action = 'app.stop' AND occurred_at < ?`,
		"app_usage_daily":     `SELECT COUNT(*) FROM app_usage_daily WHERE day < ?::date`,
		"api_key_usage_daily": `SELECT COUNT(*) FROM api_key_usage_daily WHERE day < ?::date`,
	} {
		var n int
		arg := any(cutoff.Format("2006-01-02"))
		if table == "audit_log" {
			arg = cutoff
		}
		if err := db.Conn().QueryRow(q, arg).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func TestTheAccessRecordSweepKeepsEverythingAtZero(t *testing.T) {
	db := dbtest.New(t)
	app := seedAuditApp(t, db)
	seedAccessRecord(t, db, app, 3650, 400, 10)

	// A row written in psql below the floor reads as 0, keep everything.
	if _, err := db.PutSetting(database.SettingAccessRecordRetentionDays, "200", database.BuiltinAdminID); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessRecordRetentionDays != 0 {
		t.Fatalf("a stored 200 reads as %d, want 0", s.AccessRecordRetentionDays)
	}
	pass, err := db.PruneAccessRecord(s.AccessRecordRetentionDays, time.Minute)
	if err != nil || pass.Total() != 0 {
		t.Fatalf("pass at 0 = %+v (%v), want nothing removed", pass, err)
	}
	if got := countOlder(t, db, time.Now().UTC()); got["audit_log"] != 3 || got["app_usage_daily"] != 3 {
		t.Errorf("rows after a pass at 0 = %v, want every one kept", got)
	}
}

func TestTheAccessRecordSweepCutsAllThreeOnOneCutoff(t *testing.T) {
	db := dbtest.New(t)
	app := seedAuditApp(t, db)
	seedAccessRecord(t, db, app, 3650, 400, 10)

	pass, err := db.PruneAccessRecord(365, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := countOlder(t, db, pass.Cutoff); got["audit_log"]+got["app_usage_daily"]+got["api_key_usage_daily"] != 0 {
		t.Errorf("older than the cutoff after a pass = %v, want none", got)
	}
	if got := countOlder(t, db, time.Now().UTC().AddDate(1, 0, 0)); got["audit_log"] != 1 || got["app_usage_daily"] != 1 || got["api_key_usage_daily"] != 1 {
		t.Errorf("rows left = %v, want the 10-day-old row in each table", got)
	}

	var after string
	if err := db.Conn().QueryRow(`SELECT after::text FROM audit_log WHERE action = ?`, database.ActionAuditPruned).Scan(&after); err != nil {
		t.Fatalf("no audit_log.pruned row: %v", err)
	}
	for _, want := range []string{`"retention_days": 365`, `"truncated": false`, `"audit_log": 2`, `"cutoff"`} {
		if !strings.Contains(after, want) {
			t.Errorf("pruned row = %s, want %s", after, want)
		}
	}

	// A second pass removes nothing and writes no row.
	if _, err := db.PruneAccessRecord(365, time.Minute); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, database.ActionAuditPruned).Scan(&n); err != nil || n != 1 {
		t.Errorf("pruned rows = %d (%v), want 1 — a pass that removed nothing writes none", n, err)
	}
}

// Under truncation the rollups are cut before the log, never the reverse: a
// log that outlives its usage is harmless, and usage that outlives the log
// names somebody whose deletion record is gone.
func TestATruncatedPassCutsTheRollupsBeforeTheLog(t *testing.T) {
	db := dbtest.New(t)
	app := seedAuditApp(t, db)
	seedAccessRecord(t, db, app, 900, 800, 700)
	defer database.SetAccessRecordBatchForTest(1)()
	defer database.StepAccessRecordClockForTest()()

	// One row per statement and a budget of four statements: the rollup's
	// three rows and the statement that finds none, then out of time.
	pass, err := db.PruneAccessRecord(365, 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !pass.Truncated {
		t.Fatalf("pass = %+v, want truncated", pass)
	}
	if pass.Removed["app_usage_daily"] != 3 || pass.Removed["audit_log"] != 0 {
		t.Errorf("removed = %v, want the rollup cut and the log untouched", pass.Removed)
	}
	var after string
	if err := db.Conn().QueryRow(`SELECT after::text FROM audit_log WHERE action = ?`, database.ActionAuditPruned).Scan(&after); err != nil ||
		!strings.Contains(after, `"truncated": true`) {
		t.Errorf("pruned row = %q (%v), want truncated: true", after, err)
	}
}

// Quiet's fourth basis: past the retained window, an app with no retained
// visit reads "not opened in at least the window" rather than "never visited
// since a deploy two years ago"; a deploy inside the window still wins.
func TestQuietStartsAtTheRetainedWindow(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	window := database.AccessRecordCutoff(365, now)
	old := database.AppActivity{CreatedAt: now.AddDate(-2, 0, 0), LastDeploy: now.AddDate(-2, 0, 1), RetainedFrom: window}
	if q := old.Quiet(now); q.QuietBasis != database.QuietBasisRetention || !q.QuietSince.Equal(window) {
		t.Errorf("an app with nothing retained = %s since %s, want retention since %s", q.QuietBasis, q.QuietSince, window)
	}
	recent := old
	recent.LastDeploy = now.AddDate(0, -1, 0)
	if q := recent.Quiet(now); q.QuietBasis != database.QuietBasisDeploy {
		t.Errorf("a deploy inside the window = %s, want deploy", q.QuietBasis)
	}
	keepAll := old
	keepAll.RetainedFrom = time.Time{}
	if q := keepAll.Quiet(now); q.QuietBasis != database.QuietBasisDeploy {
		t.Errorf("keeping everything = %s, want deploy", q.QuietBasis)
	}
}
