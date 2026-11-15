package database

import (
	"database/sql"
	"time"
)

// The restart schedule: at most one per app, declared as `[restart]` in its
// toft.toml and written only by the deploy that reconciles it
// (ReconcileManifestSchedules). A table rather than a column on `apps` for the
// re-render's reason — the shared claim selects `app_id` by name.
//
// Nothing here fires anything. The claim is `ClaimDueSchedulesIn`'s, and the
// firing is `internal/api`'s restartScheduler, beside the restart it calls.

// RestartScheduleName is what the single row per app is called, read by the
// app event a disabled schedule writes.
const RestartScheduleName = "Restart"

// The outcomes of a firing. A decline is `skipped` with its reason, never a
// gap: "did last night's restart happen" always has an answer.
const (
	RestartOutcomeRestarted = "restarted"
	RestartOutcomeSkipped   = "skipped"
	RestartOutcomeFailed    = "failed"
)

// RestartSchedule is one app's restart schedule.
type RestartSchedule struct {
	ID        string     `json:"id"`
	AppID     string     `json:"app_id"`
	Name      string     `json:"name"`
	Schedule  string     `json:"schedule"`
	Enabled   bool       `json:"enabled"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// The last firing, absent before the first.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	LastOutcome string     `json:"last_outcome,omitempty"`
	LastReason  string     `json:"last_reason,omitempty"`
}

const restartScheduleColumns = `id, app_id, name, schedule, enabled, next_run_at, created_at, updated_at,
	last_fired_at, last_outcome, last_reason`

func scanRestartSchedule(scan func(...any) error) (RestartSchedule, error) {
	var s RestartSchedule
	err := scan(&s.ID, &s.AppID, &s.Name, &s.Schedule, &s.Enabled, &s.NextRunAt,
		&s.CreatedAt, &s.UpdatedAt, &s.LastFiredAt, &s.LastOutcome, &s.LastReason)
	return s, err
}

// GetRestartSchedule reads an app's schedule, or (nil, nil) when it has none.
func (db *DB) GetRestartSchedule(appID string) (*RestartSchedule, error) {
	s, err := scanRestartSchedule(db.conn.QueryRow(
		`SELECT `+restartScheduleColumns+` FROM app_restart_schedules WHERE app_id = ?`, appID).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ClaimDueRestartSchedules is one scheduler pass: the shared claim, then the
// rows it took — ClaimDueRenderSchedules's shape, for the same reason.
func (db *DB) ClaimDueRestartSchedules(limit int, now time.Time, next NextFunc) ([]RestartSchedule, []DisabledSchedule, error) {
	claimed, err := db.ClaimDueSchedulesIn(ScheduleTableRestarts, limit, now, next)
	if err != nil {
		return nil, nil, err
	}
	out := make([]RestartSchedule, 0, len(claimed.DueIDs))
	for _, id := range claimed.DueIDs {
		s, err := scanRestartSchedule(db.conn.QueryRow(
			`SELECT `+restartScheduleColumns+` FROM app_restart_schedules WHERE id = ?`, id).Scan)
		if err != nil {
			// Deleted between the claim and this read — the app went, or a
			// deploy removed `[restart]`. Nothing to fire.
			if err == sql.ErrNoRows {
				continue
			}
			return nil, claimed.Disabled, err
		}
		out = append(out, s)
	}
	return out, claimed.Disabled, nil
}

// RecordRestartOutcome writes what a firing did. Scoped by the schedule's id,
// so a row a deploy replaced in between is simply not there to write to.
func (db *DB) RecordRestartOutcome(id string, at time.Time, outcome, reason string) error {
	_, err := db.conn.Exec(`UPDATE app_restart_schedules
		SET last_fired_at = ?, last_outcome = ?, last_reason = ? WHERE id = ?`,
		at.UTC(), outcome, reason, id)
	return err
}

// SetRestartScheduleEnabled pauses or resumes an app's restart — the one field
// anything but a deploy writes. A pause lasts until the next deploy. It reports
// whether the app has a schedule to pause.
func (db *DB) SetRestartScheduleEnabled(appID string, enabled bool, next NextFunc) (bool, error) {
	return setScheduleEnabled(db, "app_restart_schedules", appID, enabled, next)
}

// SetRenderScheduleEnabled is SetRestartScheduleEnabled for the re-render.
func (db *DB) SetRenderScheduleEnabled(appID string, enabled bool, next NextFunc) (bool, error) {
	return setScheduleEnabled(db, "app_render_schedules", appID, enabled, next)
}

// setScheduleEnabled writes `enabled` on one of the two one-per-app tables.
// Resuming recomputes the next firing from now, so a resume never fires for a
// window that passed while paused; pausing clears it, so a paused row never
// reads as a promise.
func setScheduleEnabled(db *DB, table, appID string, enabled bool, next NextFunc) (bool, error) {
	var expr string
	err := db.conn.QueryRow(`SELECT schedule FROM `+table+` WHERE app_id = ?`, appID).Scan(&expr)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	var nextRun *time.Time
	if enabled {
		if at, err := next(expr, now); err == nil && !at.IsZero() {
			at = at.UTC()
			nextRun = &at
		}
	}
	_, err = db.conn.Exec(`UPDATE `+table+` SET enabled = ?, next_run_at = ?, updated_at = ? WHERE app_id = ?`,
		enabled, nextRun, now, appID)
	return err == nil, err
}

// ListAllRestartSchedules returns every restart schedule on the install, for
// Admin → Automation. The last firing is the row's own outcome columns.
func (db *DB) ListAllRestartSchedules() ([]EstateSchedule, error) {
	rows, err := db.conn.Query(`
		SELECT s.id, s.app_id, a.name, a.slug, COALESCE(u.login, ''),
		       COALESCE(a.owner_user_id, ''), COALESCE(a.owner_group_id, ''),
		       s.schedule, s.enabled, s.next_run_at, s.last_fired_at, s.last_outcome,
		       a.locked_at IS NOT NULL
		FROM app_restart_schedules s
			JOIN apps a ON a.id = s.app_id
			LEFT JOIN users u ON u.id = a.owner_user_id
		ORDER BY a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EstateSchedule{}
	for rows.Next() {
		var s EstateSchedule
		if err := rows.Scan(&s.ID, &s.AppID, &s.AppName, &s.AppSlug, &s.OwnerLogin,
			&s.OwnerID, &s.OwnerGroupID, &s.Schedule, &s.Enabled, &s.NextRunAt,
			&s.LastRunAt, &s.LastStatus, &s.AppLocked); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
