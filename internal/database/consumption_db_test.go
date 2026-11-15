package database_test

import (
	"math"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/resource"
)

const mib = 1 << 20

func seedConsumptionApp(t *testing.T, db *database.DB, slug, owner, visibility string) *database.App {
	t.Helper()
	if err := db.UpsertUser(&database.User{ID: owner, Login: owner}); err != nil {
		t.Fatalf("UpsertUser %s: %v", owner, err)
	}
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: owner,
		Status: database.AppStatusRunning, Visibility: visibility,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("create app %s: %v", slug, err)
	}
	return app
}

func nearly(t *testing.T, label string, got, want float64) {
	t.Helper()
	tol := math.Max(math.Abs(want)*0.0001, 0.001)
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want ~%v", label, got, want)
	}
}

func TestConsumptionRoundTrip(t *testing.T) {
	db := dbtest.New(t)
	app := seedConsumptionApp(t, db, "demo", "alice", database.VisibilityPrivate)

	now := time.Now().UTC()
	err := db.RecordConsumption([]resource.Interval{
		{AppID: app.ID, Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second),
			CPUSeconds: 3, MemoryByteSeconds: 100 * mib * 10, PeakMemoryBytes: 120 * mib},
		{AppID: app.ID, Start: now.Add(-10 * time.Second), End: now,
			CPUSeconds: 2, MemoryByteSeconds: 200 * mib * 10, PeakMemoryBytes: 300 * mib},
	})
	if err != nil {
		t.Fatalf("RecordConsumption: %v", err)
	}

	got, err := db.AppConsumption(adminScope, app.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AppConsumption: %v", err)
	}

	nearly(t, "cpu_seconds", got.CPUSeconds, 5)
	nearly(t, "observed_seconds", got.ObservedSeconds, 20)
	// Peak is a maximum, not a sum: two windows peaking at 120 and 300 MiB did
	// not together peak at 420.
	if got.PeakMemoryBytes != 300*mib {
		t.Errorf("peak = %d MiB, want 300", got.PeakMemoryBytes/mib)
	}
	nearly(t, "average memory (MiB)", got.AverageMemoryBytes()/mib, 150)
	if got.HasGap {
		t.Error("no interval was a gap; the period should not be flagged")
	}
}

// A gap anywhere in the period must be visible in the total, or a report built
// on an incomplete accounting reads as a complete one.
func TestGapPropagatesToTheTotal(t *testing.T) {
	db := dbtest.New(t)
	app := seedConsumptionApp(t, db, "demo", "alice", database.VisibilityPrivate)

	now := time.Now().UTC()
	if err := db.RecordConsumption([]resource.Interval{
		{AppID: app.ID, Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second), CPUSeconds: 0, Gap: true},
		{AppID: app.ID, Start: now.Add(-10 * time.Second), End: now, CPUSeconds: 2},
	}); err != nil {
		t.Fatal(err)
	}

	got, _ := db.AppConsumption(adminScope, app.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if !got.HasGap {
		t.Error("a period containing a gap must report it")
	}
}

// --- Roll-up ---

// The whole value of the daily table is that it agrees with the intervals it
// was built from.
func TestRollUpSumsToItsIntervals(t *testing.T) {
	db := dbtest.New(t)
	app := seedConsumptionApp(t, db, "demo", "alice", database.VisibilityPrivate)

	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	var intervals []resource.Interval
	var wantCPU, wantByteSeconds, wantObserved float64
	for i := 0; i < 12; i++ {
		start := base.Add(time.Duration(i) * 10 * time.Second)
		iv := resource.Interval{
			AppID: app.ID, Start: start, End: start.Add(10 * time.Second),
			CPUSeconds: 1.5, MemoryByteSeconds: 100 * mib * 10, PeakMemoryBytes: int64(100+i) * mib,
		}
		intervals = append(intervals, iv)
		wantCPU += iv.CPUSeconds
		wantByteSeconds += iv.MemoryByteSeconds
		wantObserved += iv.Duration().Seconds()
	}
	if err := db.RecordConsumption(intervals); err != nil {
		t.Fatal(err)
	}

	before, err := db.AppConsumption(adminScope, app.ID, base.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	nearly(t, "pre-rollup cpu", before.CPUSeconds, wantCPU)

	if _, err := db.RollUpConsumption(time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("RollUpConsumption: %v", err)
	}

	after, err := db.AppConsumption(adminScope, app.ID, base.Add(-time.Hour), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	nearly(t, "post-rollup cpu", after.CPUSeconds, wantCPU)
	nearly(t, "post-rollup byte-seconds", after.MemoryByteSeconds, wantByteSeconds)
	nearly(t, "post-rollup observed", after.ObservedSeconds, wantObserved)
	if after.PeakMemoryBytes != 111*mib {
		t.Errorf("post-rollup peak = %d MiB, want 111", after.PeakMemoryBytes/mib)
	}
	// The average must survive the roll-up unchanged, which is the reason
	// byte-seconds are stored rather than an average.
	nearly(t, "average across the roll-up", after.AverageMemoryBytes(), before.AverageMemoryBytes())
}

// Running the sweep twice must not double-count. The fold and the delete are
// one transaction precisely so that a retried or overlapping sweep is safe.
func TestRollUpIsIdempotent(t *testing.T) {
	db := dbtest.New(t)
	app := seedConsumptionApp(t, db, "demo", "alice", database.VisibilityPrivate)

	base := time.Now().UTC().Add(-48 * time.Hour)
	if err := db.RecordConsumption([]resource.Interval{
		{AppID: app.ID, Start: base, End: base.Add(10 * time.Second), CPUSeconds: 7, PeakMemoryBytes: 50 * mib},
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := db.RollUpConsumption(cutoff); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}

	got, _ := db.AppConsumption(adminScope, app.ID, base.Add(-time.Hour), time.Now().UTC())
	nearly(t, "cpu after three sweeps", got.CPUSeconds, 7)
}

// A period straddling the roll-up boundary must report both halves. Reading
// only one table would silently halve a week-long report the moment part of it
// aged out.
func TestQuerySpansTheRollUpBoundary(t *testing.T) {
	db := dbtest.New(t)
	app := seedConsumptionApp(t, db, "demo", "alice", database.VisibilityPrivate)

	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	if err := db.RecordConsumption([]resource.Interval{
		{AppID: app.ID, Start: old, End: old.Add(10 * time.Second), CPUSeconds: 10, PeakMemoryBytes: 80 * mib},
		{AppID: app.ID, Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second), CPUSeconds: 4, PeakMemoryBytes: 40 * mib},
	}); err != nil {
		t.Fatal(err)
	}

	// Roll up only the old half.
	if _, err := db.RollUpConsumption(now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := db.AppConsumption(adminScope, app.ID, old.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	nearly(t, "cpu across the boundary", got.CPUSeconds, 14)
	if got.PeakMemoryBytes != 80*mib {
		t.Errorf("peak across the boundary = %d MiB, want 80", got.PeakMemoryBytes/mib)
	}
}

// --- Attribution and scoping ---

// A job run's cost is visible beside the app that triggered it, and is counted
// once into the app's total rather than twice.
func TestJobRunBreakdown(t *testing.T) {
	db := dbtest.New(t)
	app := seedConsumptionApp(t, db, "demo", "alice", database.VisibilityPrivate)

	now := time.Now().UTC()
	if err := db.RecordConsumption([]resource.Interval{
		{AppID: app.ID, Start: now.Add(-30 * time.Second), End: now.Add(-20 * time.Second), CPUSeconds: 1},
		{AppID: app.ID, JobRunID: "run-a", Start: now.Add(-30 * time.Second), End: now.Add(-20 * time.Second), CPUSeconds: 20},
		{AppID: app.ID, JobRunID: "run-b", Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second), CPUSeconds: 5},
	}); err != nil {
		t.Fatal(err)
	}

	runs, err := db.JobRunConsumption(adminScope, app.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("JobRunConsumption: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2: %+v", len(runs), runs)
	}
	// Ordered by cost, so the expensive run is the one an admin sees first.
	if runs[0].JobRunID != "run-a" {
		t.Errorf("runs not ordered by cost: %+v", runs)
	}
	nearly(t, "run-a cpu", runs[0].CPUSeconds, 20)
	nearly(t, "run-b cpu", runs[1].CPUSeconds, 5)

	// The app's own total includes the runs.
	total, _ := db.AppConsumption(adminScope, app.ID, now.Add(-time.Hour), now.Add(time.Hour))
	nearly(t, "app total including runs", total.CPUSeconds, 26)
}

// Consumption reads narrow through the same readable-apps predicate as
// everything else. A per-app cost figure is as good an existence oracle as a
// tag count, so a caller must not learn that a private app exists — or how
// busy it is — from this endpoint.
func TestConsumptionCannotProbeForUnreadableApps(t *testing.T) {
	db := dbtest.New(t)

	mine := seedConsumptionApp(t, db, "mine", "alice", database.VisibilityPrivate)
	theirs := seedConsumptionApp(t, db, "theirs", "bob", database.VisibilityPrivate)
	open := seedConsumptionApp(t, db, "open", "bob", database.VisibilityPublic)

	now := time.Now().UTC()
	if err := db.RecordConsumption([]resource.Interval{
		{AppID: mine.ID, Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second), CPUSeconds: 1},
		{AppID: theirs.ID, Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second), CPUSeconds: 99},
		{AppID: open.ID, Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second), CPUSeconds: 3},
	}); err != nil {
		t.Fatal(err)
	}

	scope := userScope("alice")
	rows, err := db.ListConsumption(scope, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListConsumption: %v", err)
	}

	seen := map[string]bool{}
	for _, c := range rows {
		seen[c.AppID] = true
	}
	if seen[theirs.ID] {
		t.Error("another user's private app appeared in a consumption listing")
	}
	if !seen[mine.ID] {
		t.Error("the caller's own app is missing")
	}
	if !seen[open.ID] {
		t.Error("a public app is missing")
	}

	// And asking for it directly reports nothing rather than its real cost.
	direct, err := db.AppConsumption(scope, theirs.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if direct.CPUSeconds != 0 {
		t.Errorf("a private app's consumption leaked directly: %v CPU-seconds", direct.CPUSeconds)
	}

	// The same must hold after a roll-up, or the predicate is only applied to
	// half the data.
	if _, err := db.RollUpConsumption(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	direct, _ = db.AppConsumption(scope, theirs.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if direct.CPUSeconds != 0 {
		t.Errorf("a private app's rolled-up consumption leaked: %v CPU-seconds", direct.CPUSeconds)
	}

	// A job-run breakdown is the same read by another name.
	runs, err := db.JobRunConsumption(scope, theirs.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("a private app's job runs leaked: %+v", runs)
	}
}

// The period bounds must actually bound: a report for last week must not
// quietly include this week.
func TestConsumptionRespectsThePeriod(t *testing.T) {
	db := dbtest.New(t)
	app := seedConsumptionApp(t, db, "demo", "alice", database.VisibilityPrivate)

	now := time.Now().UTC()
	if err := db.RecordConsumption([]resource.Interval{
		{AppID: app.ID, Start: now.Add(-72 * time.Hour), End: now.Add(-72*time.Hour + 10*time.Second), CPUSeconds: 100},
		{AppID: app.ID, Start: now.Add(-20 * time.Second), End: now.Add(-10 * time.Second), CPUSeconds: 7},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.AppConsumption(adminScope, app.ID, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	nearly(t, "cpu in the last hour", got.CPUSeconds, 7)
}

func TestRecordConsumptionIgnoresEmptyBatches(t *testing.T) {
	db := dbtest.New(t)
	if err := db.RecordConsumption(nil); err != nil {
		t.Errorf("empty batch: %v", err)
	}
	if err := db.RecordConsumption([]resource.Interval{{AppID: ""}}); err != nil {
		t.Errorf("unattributable interval: %v", err)
	}
}
