package database

import (
	"time"

	"github.com/google/uuid"
)

// ManifestSchedule is one of toft.toml's one-per-app schedules, `[restart]` or
// `[render]`, as the deploy that declares it records it.
type ManifestSchedule struct {
	// Schedule is the expression with its zone joined in (schedule.Join), so
	// the claim sees one string.
	Schedule string
	// NextRunAt is the first firing computed at detect. Taken only when the
	// stored schedule changed, is new, or was paused (ReconcileManifestJobs's
	// rule).
	NextRunAt *time.Time
}

// RemovedSchedules is what a reconcile deleted, so the deploy can say so
// (ADR §144): a schedule that stops because a line left a
// commit should be a sentence in somebody's inbox, not a gap.
type RemovedSchedules struct {
	Restart bool
	Render  bool
	// RenderRecipients is how many people were on the removed re-render's
	// list, which cascades with it.
	RenderRecipients int
}

// Any reports whether the reconcile removed anything.
func (r RemovedSchedules) Any() bool { return r.Restart || r.Render }

// ReconcileManifestSchedules makes an app's restart and render schedules
// exactly what its serving release's toft.toml declares, in one transaction.
// nil means the table is absent, and the row goes.
//
// Both of ReconcileManifestJobs's rules (ADR §98): a deploy resumes a pause
// (`enabled = TRUE`), and a live schedule the deploy did not change keeps its
// stored `next_run_at` — which is what lets a re-render's own flip reconcile
// its schedule without moving the firing the claim just advanced to.
//
// An upsert on `app_id` keeps the row's `id`, which is what a re-render's
// recipients reference, so they survive every change to the schedule and its
// zone. The restart's last-outcome columns are kept the same way.
//
// keepPauses is the one exception, for a scheduled re-render's own flip: a
// deployment nobody made does not resume what a person paused, so a paused
// row stays paused with no next_run_at (ReconcileManifestSchedulesKeeping).
func (db *DB) ReconcileManifestSchedules(appID string, restart, render *ManifestSchedule) (RemovedSchedules, error) {
	return db.ReconcileManifestSchedulesKeeping(appID, restart, render, false)
}

// ReconcileManifestSchedulesKeeping is ReconcileManifestSchedules, keeping a
// paused row paused when keepPauses is set.
func (db *DB) ReconcileManifestSchedulesKeeping(appID string, restart, render *ManifestSchedule, keepPauses bool) (RemovedSchedules, error) {
	var removed RemovedSchedules
	tx, err := db.conn.Begin()
	if err != nil {
		return removed, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, t := range []struct {
		table, name string
		decl        *ManifestSchedule
		gone        *bool
	}{
		{"app_restart_schedules", RestartScheduleName, restart, &removed.Restart},
		{"app_render_schedules", RenderScheduleName, render, &removed.Render},
	} {
		if t.decl == nil {
			if t.table == "app_render_schedules" {
				if err := tx.QueryRow(`SELECT COUNT(*) FROM app_render_schedule_recipients r
					JOIN app_render_schedules s ON s.id = r.schedule_id WHERE s.app_id = ?`,
					appID).Scan(&removed.RenderRecipients); err != nil {
					return removed, err
				}
			}
			res, err := tx.Exec(`DELETE FROM `+t.table+` WHERE app_id = ?`, appID)
			if err != nil {
				return removed, err
			}
			n, _ := res.RowsAffected()
			*t.gone = n > 0
			if n == 0 && t.table == "app_render_schedules" {
				removed.RenderRecipients = 0
			}
			continue
		}
		var next *time.Time
		if t.decl.NextRunAt != nil {
			at := t.decl.NextRunAt.UTC()
			next = &at
		}
		if _, err := tx.Exec(`
			INSERT INTO `+t.table+` (id, app_id, name, schedule, enabled, next_run_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, TRUE, ?, ?, ?)
			ON CONFLICT (app_id) DO UPDATE SET
				schedule = excluded.schedule,
				enabled = CASE WHEN ? THEN `+t.table+`.enabled ELSE TRUE END,
				next_run_at = CASE
					WHEN ? AND NOT `+t.table+`.enabled THEN NULL
					WHEN `+t.table+`.enabled
					     AND `+t.table+`.schedule = excluded.schedule
					     AND `+t.table+`.next_run_at IS NOT NULL
					THEN `+t.table+`.next_run_at
					ELSE excluded.next_run_at END,
				updated_at = CASE
					WHEN ? AND NOT `+t.table+`.enabled THEN `+t.table+`.updated_at
					WHEN `+t.table+`.enabled AND `+t.table+`.schedule = excluded.schedule
					THEN `+t.table+`.updated_at
					ELSE excluded.updated_at END`,
			uuid.NewString(), appID, t.name, t.decl.Schedule, next, now, now,
			keepPauses, keepPauses, keepPauses); err != nil {
			return removed, err
		}
	}
	return removed, tx.Commit()
}
