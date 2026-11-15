package database

import (
	"fmt"
	"time"
)

// The scheduler's claim, once, for every schedule an app has.
//
// Three tables hold schedules — `job_definitions` for jobs,
// `app_render_schedules` for a document's re-render and
// `app_restart_schedules` for a scheduled restart — and until ADR §33's open
// question 1 was answered each kind had its own copy of the claim. That is the one piece of this subsystem
// where a mistake is expensive and invisible: a claim that is not atomic across
// nodes fires a nightly report twice, and one that advances `next_run_at`
// outside the transaction that read it can lose a firing entirely. Two copies
// of it meant two chances to get that wrong, and they had already drifted —
// one seeded schedules with no next fire time and the other did not, and
// *neither* actually told anybody when it switched a schedule off.
//
// What is unified is the claim and nothing else. Firing is not shared and must
// not be: a job's firing is a row on a durable queue, a re-render's is a
// deploy, and a restart's is a start of the serving workload.

// ScheduleTable is one of the tables holding schedules.
//
// A closed set, and interpolated into SQL — which is safe because these are
// constants in this file and never reach it from a request, and is spelled with
// a type and a switch so that stays true rather than being remembered.
type ScheduleTable string

const (
	// ScheduleTableJobs holds job definitions: a command in a workload of its
	// own. Only some of its rows carry a schedule.
	ScheduleTableJobs ScheduleTable = "job_definitions"
	// ScheduleTableRenders holds notebook render schedules: at most one per
	// app, and what it fires is a *deploy* rather than either kind of
	// automation. See the migration that created it — a re-render that is not
	// published as a new release vanishes at the next wake, because a start
	// replaces the tree.
	ScheduleTableRenders ScheduleTable = "app_render_schedules"
	// ScheduleTableRestarts holds restart schedules: at most one per app, and
	// a table rather than a column on `apps` for the same reason as renders.
	ScheduleTableRestarts ScheduleTable = "app_restart_schedules"
)

func (t ScheduleTable) valid() bool {
	switch t {
	case ScheduleTableJobs, ScheduleTableRenders, ScheduleTableRestarts:
		return true
	}
	return false
}

// NextFunc computes the first firing strictly after `after`.
type NextFunc func(schedule string, after time.Time) (time.Time, error)

// DisabledSchedule is a schedule the claim switched off because its expression
// no longer parses.
//
// Returned rather than swallowed. A nightly report that simply stops, with
// nothing anywhere saying why, is the exact failure shape app events exist for
// — and it is what both schedulers used to do: the task claim dropped such rows
// silently, and the job claim filtered them out *before* the branch that was
// supposed to record them, so that branch had never once run.
type DisabledSchedule struct {
	ID       string
	AppID    string
	Name     string
	Schedule string
}

// ClaimedSchedules is one pass of the scheduler over one table.
type ClaimedSchedules struct {
	// DueIDs are the schedules claimed by this node for this window, in fire
	// order. The caller loads its own rows: the two tables have nothing in
	// common past the four columns this file touches, and a shared row type
	// would be a third shape that is neither.
	DueIDs []string
	// Ticks is the next_run_at each due schedule was claimed at — the instant
	// the firing is *for*, which the advance below overwrites. A caller that
	// has no use for it ignores it.
	Ticks map[string]time.Time
	// Disabled are the schedules this pass switched off.
	Disabled []DisabledSchedule
	// Seeded counts schedules given a first fire time by this pass.
	Seeded int
}

// ClaimDueSchedulesIn seeds, claims, and advances the schedules in one table.
//
// Three things happen in order, and the order matters:
//
//  1. **Seed.** An enabled schedule with no `next_run_at` — newly created, or
//     left NULL by a previous boot — is given one, computed from *now*. It is
//     therefore not due in this pass, which is right: a schedule that has just
//     acquired its first fire time must not immediately fire for a window that
//     already passed.
//  2. **Claim.** `FOR UPDATE SKIP LOCKED` inside the transaction that advances
//     `next_run_at` is what makes this multi-node safe. A second node's query
//     does not see the rows this one holds, and by the time the lock releases
//     they are no longer due — so a schedule fires on exactly one node.
//  3. **Advance,** from `now` rather than from the missed time. A schedule due
//     during downtime fires once, late, on the first pass after it — never
//     once per firing it missed, which would be a backlog of nightly reports
//     all at once on boot.
//
// A schedule whose expression no longer parses is disabled rather than retried
// forever, and is reported so the caller can say so.
func (db *DB) ClaimDueSchedulesIn(table ScheduleTable, limit int, now time.Time, next NextFunc) (ClaimedSchedules, error) {
	var out ClaimedSchedules
	if !table.valid() {
		return out, fmt.Errorf("not a schedule table: %q", table)
	}
	now = now.UTC()

	seeded, disabled, err := db.seedSchedules(table, now, next)
	if err != nil {
		return out, err
	}
	out.Seeded = seeded
	out.Disabled = append(out.Disabled, disabled...)

	tx, err := db.conn.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()

	// `schedule <> ''` as well as NOT NULL: `app_render_schedules.schedule` is
	// a NOT NULL column and `job_definitions.schedule` is nullable, and this one
	// predicate is correct for both rather than being correct for whichever
	// table it was written against.
	rows, err := tx.Query(`
		SELECT id, app_id, name, schedule, next_run_at FROM `+string(table)+`
		WHERE enabled AND schedule IS NOT NULL AND schedule <> ''
			AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at
		LIMIT ?
		FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return out, err
	}
	type claimed struct {
		id, appID, name, expr string
		tick                  time.Time
	}
	var all []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.appID, &c.name, &c.expr, &c.tick); err != nil {
			rows.Close()
			return out, err
		}
		all = append(all, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	for _, c := range all {
		at, err := next(c.expr, now)
		// The zero time is a schedule that never fires, whatever the function
		// said: written back, it is due again on the next tick, forever.
		if err != nil || at.IsZero() {
			if _, derr := tx.Exec(
				`UPDATE `+string(table)+` SET enabled = FALSE, next_run_at = NULL, updated_at = ? WHERE id = ?`,
				now, c.id); derr != nil {
				return out, derr
			}
			out.Disabled = append(out.Disabled, DisabledSchedule{
				ID: c.id, AppID: c.appID, Name: c.name, Schedule: c.expr})
			continue
		}
		if _, err := tx.Exec(
			`UPDATE `+string(table)+` SET next_run_at = ? WHERE id = ?`, at.UTC(), c.id); err != nil {
			return out, err
		}
		out.DueIDs = append(out.DueIDs, c.id)
		if out.Ticks == nil {
			out.Ticks = map[string]time.Time{}
		}
		out.Ticks[c.id] = c.tick.UTC()
	}

	if err := tx.Commit(); err != nil {
		return ClaimedSchedules{Seeded: seeded}, err
	}
	return out, nil
}

// seedSchedules gives a first fire time to enabled schedules that have none.
//
// Outside the claim transaction on purpose: it must not hold locks on rows it
// is not firing, and a seeded row is by construction not due in this pass. A
// schedule whose expression does not parse is disabled and reported here: the
// claim selects `next_run_at IS NOT NULL`, so a row that could not be seeded
// never reached it and was re-parsed every tick for ever, never disabled and
// never reported (code review 2026-11-11, C-9).
func (db *DB) seedSchedules(table ScheduleTable, now time.Time, next NextFunc) (int, []DisabledSchedule, error) {
	rows, err := db.conn.Query(`
		SELECT id, app_id, name, schedule FROM ` + string(table) + `
		WHERE enabled AND schedule IS NOT NULL AND schedule <> '' AND next_run_at IS NULL`)
	if err != nil {
		return 0, nil, err
	}
	type pending struct{ id, appID, name, expr string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.appID, &p.name, &p.expr); err != nil {
			rows.Close()
			return 0, nil, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	seeded := 0
	var disabled []DisabledSchedule
	for _, p := range todo {
		at, err := next(p.expr, now)
		if err != nil || at.IsZero() {
			if _, derr := db.conn.Exec(
				`UPDATE `+string(table)+` SET enabled = FALSE, updated_at = ? WHERE id = ? AND next_run_at IS NULL`,
				now, p.id); derr != nil {
				return seeded, disabled, derr
			}
			disabled = append(disabled, DisabledSchedule{ID: p.id, AppID: p.appID, Name: p.name, Schedule: p.expr})
			continue
		}
		if _, err := db.conn.Exec(
			`UPDATE `+string(table)+` SET next_run_at = ? WHERE id = ? AND next_run_at IS NULL`,
			at.UTC(), p.id); err != nil {
			return seeded, disabled, err
		}
		seeded++
	}
	return seeded, disabled, nil
}
