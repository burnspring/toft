package database_test

import (
	"context"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/schedule"
)

func sched(expr string, next time.Time) *database.ManifestSchedule {
	return &database.ManifestSchedule{Schedule: expr, NextRunAt: &next}
}

// A re-render's own flip reconciles the schedule that fired it, and must not
// move it: same row, recipients intact, and the next_run_at the claim advanced
// to rather than the one detect computed (ADR §144).
func TestAnUnchangedScheduleKeepsItsRowRecipientsAndNextFiring(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "weekly")
	claimed := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.ReconcileManifestSchedules(app.ID, nil, sched("0 6 * * 1", claimed)); err != nil {
		t.Fatal(err)
	}
	before, _ := db.GetRenderSchedule(app.ID)
	if err := db.ReplaceScheduleRecipients(context.Background(), before.ID, []string{"owner-weekly"}, ""); err != nil {
		t.Fatal(err)
	}

	detect := time.Now().UTC().Add(time.Hour)
	removed, err := db.ReconcileManifestSchedules(app.ID, nil, sched("0 6 * * 1", detect))
	if err != nil || removed.Any() {
		t.Fatalf("reconcile = %+v, %v", removed, err)
	}
	after, _ := db.GetRenderSchedule(app.ID)
	if after.ID != before.ID {
		t.Errorf("the row was replaced (%s → %s)", before.ID, after.ID)
	}
	if after.NextRunAt == nil || !after.NextRunAt.Equal(claimed) {
		t.Errorf("next_run_at = %v, want the stored %v", after.NextRunAt, claimed)
	}
	people, _ := db.ListScheduleRecipients(after.ID)
	if len(people) != 1 {
		t.Errorf("recipients = %+v, want the one kept", people)
	}
}

// A changed schedule or zone updates the row in place: the recipients survive,
// and the firing is the new schedule's.
func TestAChangedScheduleKeepsItsRecipientsAndTakesTheNewFiring(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "zoned")
	db.ReconcileManifestSchedules(app.ID, nil, sched("0 6 * * 1", time.Now().Add(time.Hour)))
	before, _ := db.GetRenderSchedule(app.ID)
	db.ReplaceScheduleRecipients(context.Background(), before.ID, []string{"owner-zoned"}, "")

	want := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	zoned := schedule.Join("Europe/London", "0 6 * * 1")
	if _, err := db.ReconcileManifestSchedules(app.ID, nil, sched(zoned, want)); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetRenderSchedule(app.ID)
	if after.ID != before.ID || after.Schedule != zoned {
		t.Errorf("after = %+v", after)
	}
	if after.NextRunAt == nil || !after.NextRunAt.Equal(want) {
		t.Errorf("next_run_at = %v, want %v", after.NextRunAt, want)
	}
	if people, _ := db.ListScheduleRecipients(after.ID); len(people) != 1 {
		t.Errorf("recipients = %+v", people)
	}
}

// A deploy resumes a pause, and the resumed firing is the one computed now.
func TestADeployResumesAPausedSchedule(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "paused")
	far := time.Now().UTC().Add(time.Hour)
	db.ReconcileManifestSchedules(app.ID, sched("0 4 * * *", far), sched("0 6 * * 1", far))
	if ok, err := db.SetRestartScheduleEnabled(app.ID, false, schedule.Next); !ok || err != nil {
		t.Fatalf("pause restart = %v, %v", ok, err)
	}
	if ok, err := db.SetRenderScheduleEnabled(app.ID, false, schedule.Next); !ok || err != nil {
		t.Fatalf("pause render = %v, %v", ok, err)
	}
	r, _ := db.GetRestartSchedule(app.ID)
	if r.Enabled || r.NextRunAt != nil {
		t.Fatalf("paused restart = %+v", r)
	}

	now := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	db.ReconcileManifestSchedules(app.ID, sched("0 4 * * *", now), sched("0 6 * * 1", now))
	r, _ = db.GetRestartSchedule(app.ID)
	g, _ := db.GetRenderSchedule(app.ID)
	for name, s := range map[string]struct {
		on   bool
		next *time.Time
	}{"restart": {r.Enabled, r.NextRunAt}, "render": {g.Enabled, g.NextRunAt}} {
		if !s.on || s.next == nil || !s.next.Equal(now) {
			t.Errorf("%s after deploy: enabled=%v next=%v, want resumed at %v", name, s.on, s.next, now)
		}
	}
}

// A table the release no longer declares goes, and the reconcile says what
// went — including the recipients that cascaded with a re-render.
func TestARemovedTableIsDeletedAndReported(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "removed")
	far := time.Now().UTC().Add(time.Hour)
	db.ReconcileManifestSchedules(app.ID, sched("0 4 * * *", far), sched("0 6 * * 1", far))
	g, _ := db.GetRenderSchedule(app.ID)
	db.ReplaceScheduleRecipients(context.Background(), g.ID, []string{"owner-removed"}, "")

	removed, err := db.ReconcileManifestSchedules(app.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !removed.Restart || !removed.Render || removed.RenderRecipients != 1 {
		t.Errorf("removed = %+v", removed)
	}
	if r, _ := db.GetRestartSchedule(app.ID); r != nil {
		t.Errorf("restart row left: %+v", r)
	}
	// Nothing left to remove is not a removal.
	if again, _ := db.ReconcileManifestSchedules(app.ID, nil, nil); again.Any() {
		t.Errorf("second reconcile reported %+v", again)
	}
}

// The restart's last outcome is the deploy's to keep, not to clear.
func TestARestartOutcomeSurvivesADeploy(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "outcome")
	far := time.Now().UTC().Add(time.Hour)
	db.ReconcileManifestSchedules(app.ID, sched("0 4 * * *", far), nil)
	r, _ := db.GetRestartSchedule(app.ID)
	at := time.Now().UTC().Truncate(time.Second)
	if err := db.RecordRestartOutcome(r.ID, at, database.RestartOutcomeSkipped, "sleeping"); err != nil {
		t.Fatal(err)
	}
	db.ReconcileManifestSchedules(app.ID, sched("0 5 * * *", far), nil)
	r, _ = db.GetRestartSchedule(app.ID)
	if r.LastOutcome != database.RestartOutcomeSkipped || r.LastReason != "sleeping" ||
		r.LastFiredAt == nil || !r.LastFiredAt.Equal(at) {
		t.Errorf("after deploy = %+v", r)
	}
}

// Pausing what is not there says so rather than pretending.
func TestPausingAnAbsentScheduleReportsNothingToPause(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "none")
	if ok, err := db.SetRestartScheduleEnabled(app.ID, false, schedule.Next); ok || err != nil {
		t.Errorf("= %v, %v; want false, nil", ok, err)
	}
}

// A scheduled re-render is a deployment nobody made: its flip keeps a pause,
// with no next firing, where a person's deploy resumes it (RT-11).
func TestAReRenderKeepsAPausedSchedule(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "rerendered")
	far := time.Now().UTC().Add(time.Hour)
	db.ReconcileManifestSchedules(app.ID, sched("0 4 * * *", far), sched("0 6 * * 1", far))
	if ok, err := db.SetRenderScheduleEnabled(app.ID, false, schedule.Next); !ok || err != nil {
		t.Fatalf("pause render = %v, %v", ok, err)
	}

	now := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	if _, err := db.ReconcileManifestSchedulesKeeping(app.ID, sched("0 4 * * *", now), sched("0 6 * * 1", now), true); err != nil {
		t.Fatal(err)
	}
	g, _ := db.GetRenderSchedule(app.ID)
	if g.Enabled || g.NextRunAt != nil {
		t.Errorf("render after a re-render: enabled=%v next=%v, want still paused", g.Enabled, g.NextRunAt)
	}
	if r, _ := db.GetRestartSchedule(app.ID); !r.Enabled {
		t.Error("a live schedule was disabled by keeping pauses")
	}

	if _, err := db.ReconcileManifestSchedulesKeeping(app.ID, sched("0 4 * * *", now), sched("0 6 * * 1", now), false); err != nil {
		t.Fatal(err)
	}
	if g, _ := db.GetRenderSchedule(app.ID); !g.Enabled || g.NextRunAt == nil {
		t.Errorf("render after a person's deploy: enabled=%v next=%v, want resumed", g.Enabled, g.NextRunAt)
	}
}
