package database_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The scheduler, tested once against every table it drives.
//
// This file is the reason the claim is shared at all (ADR §33, open question
// 1). Before it, each kind had its own copy and the copies had quietly drifted:
// one seeded schedules with no first fire time and the other did not, and
// *neither* told anybody when it switched a schedule off. Every case below is
// run against `job_definitions` and `app_render_schedules` from one body, so a
// guarantee cannot hold for one of them and quietly not for the other — which
// is exactly what adding a table would otherwise be an invitation to.

// scheduledRow is one created schedule: what it takes to read it back.
//
// A pair rather than an id, because the third table's reader is keyed by app —
// `app_render_schedules` holds at most one row per app, so `GetRenderSchedule`
// takes an app id and there is no by-id read for a test to reach for. Carrying
// both is what lets one body cover a per-app table and two per-name ones.
type scheduledRow struct {
	appID string
	id    string
}

// scheduleFixture creates one schedule row in the table under test and reads it
// back, so the cases below can be written once for every kind.
type scheduleFixture struct {
	table database.ScheduleTable
	// create makes an enabled schedule with the given expression and next fire
	// time (nil for none). `name` distinguishes rows within one case.
	//
	// **It may not use the app it is given.** A render schedule is one per app,
	// so a case that wants three schedules needs three apps, and the fixture is
	// the only thing that knows which. The app id it used comes back in the
	// row.
	create func(t *testing.T, db *database.DB, appID, name, expr string, next *time.Time) scheduledRow
	// enabled and nextRun read the row back.
	enabled func(t *testing.T, db *database.DB, row scheduledRow) bool
	nextRun func(t *testing.T, db *database.DB, row scheduledRow) *time.Time
}

func scheduleFixtures() []scheduleFixture {
	return []scheduleFixture{
		{
			table: database.ScheduleTableJobs,
			create: func(t *testing.T, db *database.DB, appID, name, expr string, next *time.Time) scheduledRow {
				t.Helper()
				d := &database.JobDefinition{
					AppID: appID, Name: name,
					Command: "echo hi", Schedule: expr, Enabled: true, NextRunAt: next,
					TimeoutSec: 3600,
				}
				if err := upsertJobDef(db, d); err != nil {
					t.Fatalf("UpsertJobDefinition: %v", err)
				}
				return scheduledRow{appID: appID, id: d.ID}
			},
			enabled: func(t *testing.T, db *database.DB, row scheduledRow) bool {
				t.Helper()
				got, err := db.GetJobDefinition(row.appID, row.id)
				if err != nil || got == nil {
					t.Fatalf("GetJobDefinition: %v", err)
				}
				return got.Enabled
			},
			nextRun: func(t *testing.T, db *database.DB, row scheduledRow) *time.Time {
				t.Helper()
				got, _ := db.GetJobDefinition(row.appID, row.id)
				return got.NextRunAt
			},
		},
		{
			table: database.ScheduleTableRenders,
			// **An app of its own per row.** A render schedule is one per app —
			// an app has one output, so it has at most one — and the unique
			// index enforces it. A case wanting six due schedules therefore
			// wants six notebooks, which is exactly what six due re-renders
			// are, and the fixture is where that belongs rather than in every
			// case that needs more than one row.
			create: func(t *testing.T, db *database.DB, appID, name, expr string, next *time.Time) scheduledRow {
				t.Helper()
				own := mkApp(t, db, "notebook-"+name)
				if _, err := db.ReconcileManifestSchedules(own.ID, nil,
					&database.ManifestSchedule{Schedule: expr, NextRunAt: next}); err != nil {
					t.Fatalf("ReconcileManifestSchedules: %v", err)
				}
				sch, err := db.GetRenderSchedule(own.ID)
				if err != nil || sch == nil {
					t.Fatalf("GetRenderSchedule: %v", err)
				}
				return scheduledRow{appID: own.ID, id: sch.ID}
			},
			enabled: func(t *testing.T, db *database.DB, row scheduledRow) bool {
				t.Helper()
				got, err := db.GetRenderSchedule(row.appID)
				if err != nil || got == nil {
					t.Fatalf("GetRenderSchedule: %v", err)
				}
				return got.Enabled
			},
			nextRun: func(t *testing.T, db *database.DB, row scheduledRow) *time.Time {
				t.Helper()
				got, _ := db.GetRenderSchedule(row.appID)
				if got == nil {
					return nil
				}
				return got.NextRunAt
			},
		},
		{
			table: database.ScheduleTableRestarts,
			// One per app, like renders, and written only by a deploy's
			// reconcile — so that is what the fixture calls.
			create: func(t *testing.T, db *database.DB, appID, name, expr string, next *time.Time) scheduledRow {
				t.Helper()
				own := mkApp(t, db, "service-"+name)
				if _, err := db.ReconcileManifestSchedules(own.ID,
					&database.ManifestSchedule{Schedule: expr, NextRunAt: next}, nil); err != nil {
					t.Fatalf("ReconcileManifestSchedules: %v", err)
				}
				got, err := db.GetRestartSchedule(own.ID)
				if err != nil || got == nil {
					t.Fatalf("GetRestartSchedule: %v", err)
				}
				return scheduledRow{appID: own.ID, id: got.ID}
			},
			enabled: func(t *testing.T, db *database.DB, row scheduledRow) bool {
				t.Helper()
				got, err := db.GetRestartSchedule(row.appID)
				if err != nil || got == nil {
					t.Fatalf("GetRestartSchedule: %v", err)
				}
				return got.Enabled
			},
			nextRun: func(t *testing.T, db *database.DB, row scheduledRow) *time.Time {
				t.Helper()
				got, _ := db.GetRestartSchedule(row.appID)
				if got == nil {
					return nil
				}
				return got.NextRunAt
			},
		},
	}
}

func everyMinute(_ string, after time.Time) (time.Time, error) {
	return after.Add(time.Minute), nil
}

// An enabled schedule with no first fire time gets one, computed from now — so
// it does not immediately fire for a window that has already passed.
//
// The task table never had this pass at all: a row that reached NULL
// `next_run_at` was dead, and nothing repaired it.
func TestASchedulesFirstFireTimeIsSeededAndIsNotDue(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			row := f.create(t, db, app.ID, "nightly", "0 3 * * *", nil)

			now := time.Now().UTC()
			claimed, err := db.ClaimDueSchedulesIn(f.table, 10, now, everyMinute)
			if err != nil {
				t.Fatal(err)
			}
			if claimed.Seeded != 1 {
				t.Errorf("seeded %d, want 1", claimed.Seeded)
			}
			// Seeded, therefore not fired in the same pass.
			if len(claimed.DueIDs) != 0 {
				t.Errorf("a schedule seeded in this pass fired in it: %v", claimed.DueIDs)
			}
			at := f.nextRun(t, db, row)
			if at == nil || !at.After(now) {
				t.Fatalf("next_run_at = %v, want a time after now", at)
			}
		})
	}
}

// A due schedule is claimed exactly once, whichever table it is in, and the
// same firing is never picked up twice.
func TestADueScheduleIsClaimedOnce(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			past := time.Now().UTC().Add(-time.Minute)
			future := time.Now().UTC().Add(time.Hour)
			due := f.create(t, db, app.ID, "due", "* * * * *", &past)
			f.create(t, db, app.ID, "later", "* * * * *", &future)

			now := time.Now().UTC()
			first, err := db.ClaimDueSchedulesIn(f.table, 10, now, everyMinute)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.DueIDs) != 1 || first.DueIDs[0] != due.id {
				t.Fatalf("claimed %v, want just the due one", first.DueIDs)
			}

			again, err := db.ClaimDueSchedulesIn(f.table, 10, now, everyMinute)
			if err != nil {
				t.Fatal(err)
			}
			if len(again.DueIDs) != 0 {
				t.Errorf("the same firing was claimed twice: %v", again.DueIDs)
			}
		})
	}
}

// Several nodes claiming at once partition the work rather than racing: a
// nightly report that fires twice is not a recoverable mistake.
func TestConcurrentClaimsFireEachScheduleOnce(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			past := time.Now().UTC().Add(-time.Minute)
			for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
				f.create(t, db, app.ID, name, "* * * * *", &past)
			}

			var (
				mu    sync.Mutex
				fired = map[string]int{}
				wg    sync.WaitGroup
			)
			now := time.Now().UTC()
			for i := 0; i < 6; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got, err := db.ClaimDueSchedulesIn(f.table, 10, now, everyMinute)
					if err != nil {
						t.Errorf("claim: %v", err)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					for _, id := range got.DueIDs {
						fired[id]++
					}
				}()
			}
			wg.Wait()

			if len(fired) != 6 {
				t.Errorf("fired %d distinct schedules, want 6", len(fired))
			}
			for id, n := range fired {
				if n != 1 {
					t.Errorf("schedule %s fired %d times", id, n)
				}
			}
		})
	}
}

// A schedule due during downtime fires once and is advanced from *now*: a
// backlog of nightly reports all firing at once on boot is worse than skipping
// them.
func TestAMissedWindowCollapsesToOneFiring(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			overdue := time.Now().UTC().Add(-72 * time.Hour)
			row := f.create(t, db, app.ID, "nightly", "0 3 * * *", &overdue)

			now := time.Now().UTC()
			claimed, err := db.ClaimDueSchedulesIn(f.table, 10, now, func(_ string, after time.Time) (time.Time, error) {
				return after.Add(24 * time.Hour), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(claimed.DueIDs) != 1 {
				t.Fatalf("claimed %d, want 1 — three missed days collapse to one firing", len(claimed.DueIDs))
			}
			at := f.nextRun(t, db, row)
			if at == nil || at.Before(now) {
				t.Errorf("next_run_at = %v, want a time after now — the lost days must not queue up", at)
			}
		})
	}
}

// A schedule whose expression no longer parses is disabled rather than retried
// forever — and **reported**, so the app's owners can be told. Both halves used
// to be missing in different ways: the task claim dropped such rows silently,
// and the job claim filtered them out before the branch meant to report them.
func TestAnUnparseableScheduleIsDisabledAndReported(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			past := time.Now().UTC().Add(-time.Minute)
			row := f.create(t, db, app.ID, "broken", "not a cron expression", &past)

			claimed, err := db.ClaimDueSchedulesIn(f.table, 10, time.Now().UTC(),
				func(s string, _ time.Time) (time.Time, error) {
					return time.Time{}, errors.New("cannot parse " + s)
				})
			if err != nil {
				t.Fatal(err)
			}
			if len(claimed.DueIDs) != 0 {
				t.Error("a schedule that cannot be parsed must not fire")
			}
			if len(claimed.Disabled) != 1 || claimed.Disabled[0].ID != row.id {
				t.Fatalf("disabled = %+v, want the one broken schedule", claimed.Disabled)
			}
			// With its app and name, because the caller has to write an event
			// naming both and cannot go looking for a row that is now off. The
			// name is the fixture's business: a render schedule has one per app
			// rather than one per row, so it carries the platform's word rather
			// than the case's.
			if claimed.Disabled[0].AppID != row.appID || claimed.Disabled[0].Name == "" {
				t.Errorf("disabled = %+v, want the app and name carried with it", claimed.Disabled[0])
			}
			if f.enabled(t, db, row) {
				t.Error("the schedule should be disabled rather than retried forever")
			}
		})
	}
}

// A schedule that never fires (`0 0 31 2 *`) is disabled and reported like one
// that does not parse, even from a next function that answers the zero time
// with no error — written back, that is due again on every tick (JOBS-8).
func TestAScheduleThatNeverFiresIsDisabledAndReported(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			past := time.Now().UTC().Add(-time.Minute)
			row := f.create(t, db, app.ID, "never", "0 0 31 2 *", &past)

			claimed, err := db.ClaimDueSchedulesIn(f.table, 10, time.Now().UTC(),
				func(string, time.Time) (time.Time, error) { return time.Time{}, nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(claimed.DueIDs) != 0 {
				t.Error("a schedule that never fires fired")
			}
			if len(claimed.Disabled) != 1 || claimed.Disabled[0].ID != row.id {
				t.Fatalf("disabled = %+v, want the one schedule", claimed.Disabled)
			}
			if f.enabled(t, db, row) {
				t.Error("the schedule should be disabled rather than claimed every tick")
			}
		})
	}
}

// A paused schedule is never seeded and never claimed. `enabled` is the switch
// an owner threw, and a seed pass that ignored it would restart everything the
// install had deliberately turned off.
func TestAPausedScheduleIsLeftAlone(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			past := time.Now().UTC().Add(-time.Minute)
			row := f.create(t, db, app.ID, "paused", "* * * * *", &past)
			if _, err := db.Conn().Exec(
				`UPDATE `+string(f.table)+` SET enabled = FALSE, next_run_at = NULL WHERE id = ?`, row.id); err != nil {
				t.Fatal(err)
			}

			claimed, err := db.ClaimDueSchedulesIn(f.table, 10, time.Now().UTC(), everyMinute)
			if err != nil {
				t.Fatal(err)
			}
			if claimed.Seeded != 0 || len(claimed.DueIDs) != 0 {
				t.Errorf("a paused schedule was touched: %+v", claimed)
			}
			if at := f.nextRun(t, db, row); at != nil {
				t.Errorf("next_run_at = %v, want none — a paused schedule has no next firing", at)
			}
		})
	}
}

// A job with no schedule is only ever triggered, and must never be swept up by
// the pass that fires scheduled ones. (`job_definitions.schedule` is nullable;
// `app_render_schedules.schedule` is not, so this case exists on one table only
// — the shared predicate has to be right for both.)
func TestAnUnscheduledJobIsNeverClaimed(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	d := &database.JobDefinition{
		AppID: app.ID, Name: "on-demand",
		Command: "echo hi", Enabled: true,
		TimeoutSec: 3600,
	}
	if err := upsertJobDef(db, d); err != nil {
		t.Fatal(err)
	}

	claimed, err := db.ClaimDueSchedulesIn(database.ScheduleTableJobs, 10, time.Now().UTC(), everyMinute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Seeded != 0 || len(claimed.DueIDs) != 0 {
		t.Errorf("a job with no schedule was scheduled: %+v", claimed)
	}
}

// The table is a closed set, and anything else is refused rather than
// interpolated into SQL.
func TestOnlyTheKnownScheduleTablesAreAccepted(t *testing.T) {
	db := dbtest.New(t)
	if _, err := db.ClaimDueSchedulesIn("apps; DROP TABLE users", 10, time.Now().UTC(), everyMinute); err == nil {
		t.Fatal("an unknown schedule table must be refused")
	}
}

// The claim hands back the tick each schedule was due at — the instant the
// firing is for — and not the advanced next_run_at it writes in the same
// statement. Without it a run that waited in a queue could not know which
// firing it is.
func TestTheClaimReturnsTheTickItClaimed(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			tick := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Minute)
			due := f.create(t, db, app.ID, "due", "* * * * *", &tick)

			got, err := db.ClaimDueSchedulesIn(f.table, 10, time.Now().UTC(), everyMinute)
			if err != nil {
				t.Fatal(err)
			}
			if at, ok := got.Ticks[due.id]; !ok || !at.Equal(tick) {
				t.Errorf("tick = %v (present %v), want %s", at, ok, tick)
			}
		})
	}
}

// An enabled schedule with no next_run_at whose expression errors is disabled
// and reported by the seeding pass: the claim selects only seeded rows, so
// such a row was re-parsed every tick for ever and never reported (code review
// 2026-11-11, C-9).
func TestAnUnseedableScheduleIsDisabledAndReported(t *testing.T) {
	for _, f := range scheduleFixtures() {
		t.Run(string(f.table), func(t *testing.T) {
			db := dbtest.New(t)
			app := mkApp(t, db, "demo")
			row := f.create(t, db, app.ID, "broken", "not a cron expression", nil)
			claimed, err := db.ClaimDueSchedulesIn(f.table, 10, time.Now().UTC(),
				func(s string, _ time.Time) (time.Time, error) {
					return time.Time{}, errors.New("cannot parse " + s)
				})
			if err != nil {
				t.Fatal(err)
			}
			if len(claimed.Disabled) != 1 || claimed.Disabled[0].ID != row.id || claimed.Disabled[0].AppID != row.appID {
				t.Fatalf("disabled = %+v, want the unseedable schedule", claimed.Disabled)
			}
			if f.enabled(t, db, row) {
				t.Error("the schedule should be disabled rather than re-parsed every tick")
			}
		})
	}
}
