package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/schedule"
)

// declareRender is what a deploy of a release declaring [render] writes.
func declareRender(t *testing.T, db *database.DB, appID, expr string) *database.RenderSchedule {
	t.Helper()
	next, _ := schedule.Next(expr, time.Now().UTC())
	if _, err := db.ReconcileManifestSchedules(appID, nil,
		&database.ManifestSchedule{Schedule: expr, NextRunAt: &next}); err != nil {
		t.Fatalf("ReconcileManifestSchedules: %v", err)
	}
	got, err := db.GetRenderSchedule(appID)
	if err != nil || got == nil {
		t.Fatalf("GetRenderSchedule: %v", err)
	}
	return got
}

// One app, one output, one schedule.
//
// The unique index is what enforces it, and it is not decoration: two rows
// claiming independently would enqueue two builds of the same release every
// night. A second deploy changing the schedule updates the one row.
func TestAnAppHasAtMostOneRenderSchedule(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")

	first := declareRender(t, db, app.ID, "0 3 * * *")
	second := declareRender(t, db, app.ID, "0 6 * * 1")
	if second.ID != first.ID {
		t.Errorf("a second deploy made a second row (%s then %s)", first.ID, second.ID)
	}
	if second.Schedule != "0 6 * * 1" {
		t.Errorf("schedule = %q, want the one just declared", second.Schedule)
	}
}

// Absent is the ordinary state of most notebooks, and it is not an error.
func TestAnAppWithNoRenderScheduleIsNotAnError(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")

	got, err := db.GetRenderSchedule(app.ID)
	if err != nil {
		t.Fatalf("GetRenderSchedule: %v", err)
	}
	if got != nil {
		t.Errorf("schedule = %+v, want none", got)
	}
}

// The row goes with the app. A schedule that outlived its app would be claimed
// forever against an app id nothing can load.
func TestARenderScheduleCascadesWithItsApp(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "reports")
	declareRender(t, db, app.ID, "0 3 * * *")
	if err := db.DeleteApp(app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	claimed, err := db.ClaimDueSchedulesIn(database.ScheduleTableRenders, 10,
		time.Now().UTC().Add(48*time.Hour), schedule.Next)
	if err != nil {
		t.Fatalf("ClaimDueSchedulesIn: %v", err)
	}
	if len(claimed.DueIDs) != 0 {
		t.Errorf("a deleted app's schedule is still claimable: %v", claimed.DueIDs)
	}
}
