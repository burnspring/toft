package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The overdue query is the whole of the alert's correctness: it must name a job
// past its window, and must not name one that is paused, recently succeeded,
// new, or already reported.
func TestListOverdueJobs(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "overdue")
	hour := 3600
	mk := func(name string, enabled bool) *database.JobDefinition {
		d := &database.JobDefinition{AppID: app.ID, Name: name, Command: "true", Enabled: enabled,
			TimeoutSec: 600, ExpectWithinSec: &hour}
		if err := upsertJobDef(db, d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	stale := mk("stale", true)
	fresh := mk("fresh", true)
	paused := mk("paused", false)
	mk("new", true)

	// Push three of them back past their window; "new" keeps enabled_at = now.
	for _, id := range []string{stale.ID, fresh.ID, paused.ID} {
		if _, err := db.Conn().Exec(`UPDATE job_definitions SET enabled_at = now() - interval '3 hours' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	// "fresh" succeeded ten minutes ago.
	run := &database.JobRun{AppID: app.ID, DefinitionID: fresh.ID, JobName: "fresh"}
	if err := db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJobRun(run.ID, database.JobRunSucceeded, nil, "", ""); err != nil {
		t.Fatal(err)
	}

	got, err := db.ListOverdueJobs(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != stale.ID {
		t.Fatalf("overdue = %+v, want only %q", got, "stale")
	}

	// Once reported, it is not reported again until that event is resolved.
	if err := db.InsertAppEvent(&database.AppEvent{AppID: app.ID, Kind: "job_run_overdue",
		Source: database.EventSourceJobs, Summary: "x", Fingerprint: stale.ID + ":overdue"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.ListOverdueJobs(time.Now()); len(again) != 0 {
		t.Errorf("an open overdue event did not hold the job back: %+v", again)
	}
}
