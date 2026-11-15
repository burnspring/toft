package database_test

import (
	"fmt"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The typical duration is the median of the last ten scheduled runs that
// started and finished. A run nobody's schedule started, a skipped one, a
// cancelled one and one still running are none of them how long the schedule
// occupies the install.
func TestTypicalJobDurationIsTheMedianOfRecentScheduledRuns(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "etl")
	def := seedJobDef(t, db, app.ID, "nightly", "0 2 * * *")
	never := seedJobDef(t, db, app.ID, "fresh", "0 4 * * *")

	base := time.Now().UTC().Add(-30 * 24 * time.Hour)
	n := 0
	run := func(startedBy, status string, secs int, finished bool, age time.Duration) {
		t.Helper()
		n++
		start := base.Add(age)
		var end any
		if finished {
			end = start.Add(time.Duration(secs) * time.Second)
		}
		if _, err := db.Conn().Exec(`
			INSERT INTO job_runs (id, app_id, job_name, definition_id, status, started_by,
			                      started_at, finished_at, queued_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("run-%d", n), app.ID, def.Name, def.ID, status, startedBy,
			start, end, start, start); err != nil {
			t.Fatal(err)
		}
	}
	// Eleven runs, oldest first: the oldest is a 10,000 s outlier that falls
	// out of the last ten.
	run(database.JobStartedBySchedule, database.JobRunSucceeded, 10000, true, 0)
	for i, secs := range []int{100, 200, 300, 400, 500, 600, 700, 800, 900, 5000} {
		run(database.JobStartedBySchedule, database.JobRunSucceeded, secs, true, time.Duration(i+1)*time.Hour)
	}
	// Newer than all of them, and none counted.
	run(database.JobStartedByUI, database.JobRunSucceeded, 1, true, 20*time.Hour)
	run(database.JobStartedBySchedule, database.JobRunCancelled, 2, true, 21*time.Hour)
	run(database.JobStartedBySchedule, database.JobRunSkipped, 3, true, 22*time.Hour)
	run(database.JobStartedBySchedule, database.JobRunRunning, 4, false, 23*time.Hour)

	got, err := db.TypicalJobDurations()
	if err != nil {
		t.Fatal(err)
	}
	if d := got[def.ID]; d.Seconds != 550 || d.Basis != 10 {
		t.Fatalf("typical = %+v, want the median 550 s over 10 runs", d)
	}
	if _, ok := got[never.ID]; ok {
		t.Fatalf("a job with no runs has a typical duration: %+v", got[never.ID])
	}
}

// The estate lists carry the app's lock, which holds a schedule without
// pausing it.
func TestTheEstateScheduleListCarriesTheLock(t *testing.T) {
	db := dbtest.New(t)
	app := seedJobApp(t, db, "held")
	seedJobDef(t, db, app.ID, "nightly", "0 2 * * *")
	if err := db.SetAppLock(app.ID, "", "held for review", time.Now(), app.Slug, database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	jobs, err := db.ListAllScheduledJobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !jobs[0].AppLocked {
		t.Fatalf("jobs = %+v, want one, locked", jobs)
	}
}
