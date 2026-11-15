package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func mkLine(appID string, seq int, line string) database.PlatformLogLine {
	return database.PlatformLogLine{
		AppID:        appID,
		WorkloadName: appID + "-1",
		At:           time.Now().UTC(),
		Seq:          seq,
		Stream:       "stdout",
		Line:         line,
	}
}

// The one that matters, and nothing observable changes when it breaks: the
// lines are simply not there, and the app they belong to is fine.
//
// applog.Sink's buffer is install-wide and flushes every two seconds into one
// statement. app_platform_log.app_id cascades from apps, so a line for an app
// deleted inside that window used to fail the whole statement and take every
// other app's narration with it — for a reason that has nothing to do with
// them, and against the invariant that the platform's own voice about an app is
// kept per app.
func TestAPlatformLogFlushSurvivesADeletedAppInTheBatch(t *testing.T) {
	db := dbtest.New(t)
	live := mkApp(t, db, "plog-live")
	doomed := mkApp(t, db, "plog-doomed")

	if err := db.DeleteApp(doomed.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	// The order is the point: the deleted app's line is in the middle of the
	// batch, where a failing statement would take both of its neighbours.
	batch := []database.PlatformLogLine{
		mkLine(live.ID, 1, "starting"),
		mkLine(doomed.ID, 1, "stopping"),
		mkLine(live.ID, 2, "listening on 3100"),
	}
	if err := db.InsertPlatformLog(batch); err != nil {
		t.Fatalf("InsertPlatformLog: %v — a deleted app in the batch must not fail the flush", err)
	}

	got, err := db.PlatformLogTail(live.ID, 10)
	if err != nil {
		t.Fatalf("PlatformLogTail: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the live app kept %d of its 2 lines: %+v", len(got), got)
	}
	for i, want := range []string{"starting", "listening on 3100"} {
		if got[i].Line != want {
			t.Errorf("line %d = %q, want %q", i, got[i].Line, want)
		}
	}

	// And the deleted app's line is dropped rather than orphaned — which is
	// what the cascade would have done to it a moment later anyway.
	if gone, err := db.PlatformLogTail(doomed.ID, 10); err != nil {
		t.Fatalf("PlatformLogTail: %v", err)
	} else if len(gone) != 0 {
		t.Errorf("the deleted app kept %d lines", len(gone))
	}
}

// The ordinary path, asserted because the filtered insert is a different
// statement from the one this file used to run: every column has to survive it,
// including the null deployment_id every background writer produces.
func TestInsertPlatformLogRoundTrips(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "plog-round")
	at := time.Now().UTC().Truncate(time.Millisecond)

	withDeployment := mkLine(app.ID, 1, "building")
	withDeployment.At = at
	withDeployment.DeploymentID = "dep-1"

	bare := mkLine(app.ID, 2, "no deployment holds this one")
	bare.At = at

	// A zero At is filled in rather than refused; the column is NOT NULL.
	zeroed := mkLine(app.ID, 3, "written by a caller with no clock")
	zeroed.At = time.Time{}

	if err := db.InsertPlatformLog([]database.PlatformLogLine{withDeployment, bare, zeroed}); err != nil {
		t.Fatalf("InsertPlatformLog: %v", err)
	}

	got, err := db.PlatformLogTail(app.ID, 10)
	if err != nil {
		t.Fatalf("PlatformLogTail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("stored %d lines, want 3", len(got))
	}
	if got[0].DeploymentID != "dep-1" {
		t.Errorf("deployment_id = %q, want dep-1", got[0].DeploymentID)
	}
	if got[1].DeploymentID != "" {
		t.Errorf("a line no deployment holds came back with deployment_id %q", got[1].DeploymentID)
	}
	if got[0].WorkloadName != app.ID+"-1" || got[0].Stream != "stdout" || got[0].Seq != 1 {
		t.Errorf("the row did not round-trip: %+v", got[0])
	}
	if !got[0].At.Equal(at) {
		t.Errorf("at = %v, want %v", got[0].At, at)
	}
	if got[2].At.IsZero() {
		t.Error("a zero At was stored as zero rather than filled in")
	}
}

// A batch of one still has to carry its parameter types: Postgres infers a
// VALUES list's column types from its first row, so the casts are the statement
// working at all rather than a nicety.
func TestInsertPlatformLogTakesASingleLine(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "plog-one")

	if err := db.InsertPlatformLog([]database.PlatformLogLine{mkLine(app.ID, 1, "alone")}); err != nil {
		t.Fatalf("InsertPlatformLog: %v", err)
	}
	got, err := db.PlatformLogTail(app.ID, 10)
	if err != nil {
		t.Fatalf("PlatformLogTail: %v", err)
	}
	if len(got) != 1 || got[0].Line != "alone" {
		t.Fatalf("stored %+v", got)
	}
}

// Every line belonging to an app that is gone leaves nothing to insert, and
// that is not an error either.
func TestInsertPlatformLogWithNothingLeftToWriteIsNotAnError(t *testing.T) {
	db := dbtest.New(t)
	doomed := mkApp(t, db, "plog-all-gone")
	if err := db.DeleteApp(doomed.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if err := db.InsertPlatformLog([]database.PlatformLogLine{mkLine(doomed.ID, 1, "x")}); err != nil {
		t.Fatalf("InsertPlatformLog: %v", err)
	}
}

// One app's line holding a NUL or invalid UTF-8 used to fail the statement
// and every app's lines in the flush with it (JOBS-1). It is cleaned; and a
// row Postgres still refuses costs that row alone.
func TestAPlatformLogFlushSurvivesOneBadLine(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "plog-bad")

	refused := mkLine(app.ID, 2, "refused")
	refused.Seq = 1 << 40 // out of range for the integer column
	batch := []database.PlatformLogLine{
		mkLine(app.ID, 1, "binary \x00 here"),
		refused,
		mkLine(app.ID, 3, "cut \xe2\x82"),
	}
	if err := db.InsertPlatformLog(batch); err != nil {
		t.Fatalf("InsertPlatformLog: %v", err)
	}
	got, err := db.PlatformLogTail(app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Line != "binary � here" || got[1].Line != "cut �" {
		t.Fatalf("kept %+v, want the two good lines cleaned", got)
	}
}
