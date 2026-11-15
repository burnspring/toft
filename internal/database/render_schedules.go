package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// The render schedule: at most one per app, and what it fires is a deploy.
//
// See ADR §73 (`docs/architecture-decisions.md` § A folder of HTML) for why it
// is a table rather than a column on `apps` — the shared claim selects `app_id`
// by name and `apps` has none, so a column would mean forking the claim and
// putting the multi-node safety property in two places — and why a re-render is
// neither kind of automation: a job cannot change what the workload serves,
// because a start replaces the tree.
//
// Nothing here fires anything. The claim is `ClaimDueSchedulesIn`'s, shared with
// the jobs and restart tables so the multi-node safety property lives in one
// place, and the firing is `internal/builder`'s — one `Enqueue` per claimed row.

// RenderScheduleName is what the single row per app is called. Not a person's
// label: there is one per app, so there is nothing to tell apart, and asking
// for a name would be asking for a decision nobody has. It is read by the app
// event a disabled schedule writes.
const RenderScheduleName = "Re-render"

// RenderSchedule is one app's re-render schedule.
type RenderSchedule struct {
	ID        string     `json:"id"`
	AppID     string     `json:"app_id"`
	Name      string     `json:"name"`
	Schedule  string     `json:"schedule"`
	Enabled   bool       `json:"enabled"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Recipients are the people told when a render lands. Not a column — filled
	// by the handler from `app_render_schedule_recipients` — and omitted when
	// empty, so a schedule with nobody on it reads exactly as it did before this
	// existed.
	Recipients []ScheduleRecipient `json:"recipients,omitempty"`
}

const renderScheduleColumns = `id, app_id, name, schedule, enabled, next_run_at, created_at, updated_at`

func scanRenderSchedule(scan func(...any) error) (RenderSchedule, error) {
	var s RenderSchedule
	err := scan(&s.ID, &s.AppID, &s.Name, &s.Schedule, &s.Enabled, &s.NextRunAt,
		&s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// GetRenderSchedule reads an app's schedule, or (nil, nil) when it has none.
//
// Scoped by app id, like every other read that takes an id from a URL: the
// caller has already authorized the app, and a row is only ever reachable
// through the app it belongs to.
func (db *DB) GetRenderSchedule(appID string) (*RenderSchedule, error) {
	s, err := scanRenderSchedule(db.Conn().QueryRow(
		`SELECT `+renderScheduleColumns+` FROM app_render_schedules WHERE app_id = ?`, appID).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ClaimDueRenderSchedules is one scheduler pass: the shared claim, then the
// rows it took.
//
// The same shape as ClaimDueCronJobs, and deliberately so — the claim is the
// part that must not be written twice, and the read after it is the part that
// differs per table. Read after the commit, in the order the claim fired them:
// the rows are already advanced, so nothing else will take them, and reading
// them inside the transaction would only hold their locks for longer.
func (db *DB) ClaimDueRenderSchedules(limit int, now time.Time, next NextFunc) ([]RenderSchedule, []DisabledSchedule, error) {
	claimed, err := db.ClaimDueSchedulesIn(ScheduleTableRenders, limit, now, next)
	if err != nil {
		return nil, nil, err
	}
	if len(claimed.DueIDs) == 0 {
		return nil, claimed.Disabled, nil
	}

	out := make([]RenderSchedule, 0, len(claimed.DueIDs))
	for _, id := range claimed.DueIDs {
		s, err := scanRenderSchedule(db.Conn().QueryRow(
			`SELECT `+renderScheduleColumns+` FROM app_render_schedules WHERE id = ?`, id).Scan)
		if err != nil {
			// Deleted between the claim and this read — the app was deleted, or
			// its owner turned the schedule off a second ago. Nothing to fire,
			// and the rest of the pass still should.
			if err == sql.ErrNoRows {
				continue
			}
			return nil, claimed.Disabled, err
		}
		out = append(out, s)
	}
	return out, claimed.Disabled, nil
}

// --- Recipients, and the record of what reached them ---

// ScheduleRecipient is one person on a report's list.
//
// The list is the owner's *intent*: who this report is for. What actually
// happened on a given run is a `report_delivery_recipients` row, and the two are
// deliberately separate — collapsing them would let a week of failed sends
// quietly rewrite who the report is for.
type ScheduleRecipient struct {
	UserID  string `json:"user_id"`
	Login   string `json:"login"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	AddedAt string `json:"added_at,omitempty"`
}

// ListScheduleRecipients returns a schedule's list, with enough of each person
// to render a picker without a second query.
func (db *DB) ListScheduleRecipients(scheduleID string) ([]ScheduleRecipient, error) {
	rows, err := db.conn.Query(`
		SELECT r.user_id, u.login, u.name, u.email
		FROM app_render_schedule_recipients r
		JOIN users u ON u.id = r.user_id
		WHERE r.schedule_id = ?
		ORDER BY u.login`, scheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduleRecipient{}
	for rows.Next() {
		var r ScheduleRecipient
		if err := rows.Scan(&r.UserID, &r.Login, &r.Name, &r.Email); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceScheduleRecipients writes a schedule's list wholesale.
//
// Replace rather than add-and-remove, because there is one row and one entry
// point onto it — the same reason the schedule itself is a PUT. Two entry points
// onto one list is how a UI comes to have a create that fails on a second save.
//
// Ids that name no account are dropped rather than refused: the picker cannot
// offer one, so this can only be a stale client, and failing the whole save over
// a person who was deleted between opening the dialog and pressing save would be
// an argument nobody can act on.
func (db *DB) ReplaceScheduleRecipients(ctx context.Context, scheduleID string, userIDs []string, actorID string) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM app_render_schedule_recipients WHERE schedule_id=?`, scheduleID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range userIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.Exec(`
			INSERT INTO app_render_schedule_recipients (schedule_id, user_id, added_at, added_by)
			SELECT ?, id, now(), ? FROM users WHERE id = ?
			ON CONFLICT DO NOTHING`, scheduleID, nullIfEmpty(actorID), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReportDelivery is one run of a report's delivery, sent or not.
type ReportDelivery struct {
	ID           string                    `json:"id"`
	AppID        string                    `json:"app_id"`
	ScheduleID   string                    `json:"schedule_id"`
	DeploymentID string                    `json:"deployment_id"`
	CreatedAt    time.Time                 `json:"created_at"`
	Delivered    int                       `json:"delivered"`
	Skipped      int                       `json:"skipped"`
	Failed       int                       `json:"failed"`
	Recipients   []ReportDeliveryRecipient `json:"recipients,omitempty"`
}

// ReportDeliveryRecipient is what happened to one person on one run.
//
// `login` and `email` are copied rather than joined, exactly as the
// announcement's are: the row has to keep making sense after the account is
// deleted, and a delivery record that dissolves when somebody leaves is not a
// record.
type ReportDeliveryRecipient struct {
	UserID string `json:"user_id,omitempty"`
	Login  string `json:"login"`
	Email  string `json:"email,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// The three outcomes. A fourth would mean a run this product cannot describe.
const (
	DeliveryDelivered = "delivered"
	DeliverySkipped   = "skipped"
	DeliveryFailed    = "failed"
)

// RecordReportDelivery writes a run and its per-person rows in one transaction,
// with the counts computed from the rows rather than passed alongside them.
//
// A count that cannot distinguish outcomes is not a record — the announcement's
// rule, and this send has the same claim on being believed. Deriving them here
// is what stops a caller incrementing a counter it forgot to pair with a row.
func (db *DB) RecordReportDelivery(ctx context.Context, d *ReportDelivery) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	d.Delivered, d.Skipped, d.Failed = 0, 0, 0
	for _, r := range d.Recipients {
		switch r.Status {
		case DeliveryDelivered:
			d.Delivered++
		case DeliverySkipped:
			d.Skipped++
		case DeliveryFailed:
			d.Failed++
		}
	}
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if _, err := tx.Exec(`
		INSERT INTO report_deliveries (id, app_id, schedule_id, deployment_id, created_at, delivered, skipped, failed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.AppID, d.ScheduleID, d.DeploymentID, d.CreatedAt, d.Delivered, d.Skipped, d.Failed); err != nil {
		return err
	}
	for _, r := range d.Recipients {
		if _, err := tx.Exec(`
			INSERT INTO report_delivery_recipients (id, delivery_id, user_id, login, email, status, reason, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), d.ID, nullIfEmpty(r.UserID), r.Login, r.Email, r.Status, r.Reason, d.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LastReportDeliveryAt is when this schedule last attempted a delivery, which is
// what the floor is measured from.
//
// **Attempted, not delivered.** A run suppressed by the floor still wrote a row,
// so measuring from the last row is what stops a minutely schedule from
// resetting its own clock by failing — and measuring from the last *successful*
// send would let one broken relay turn the floor off.
func (db *DB) LastReportDeliveryAt(scheduleID string) (*time.Time, error) {
	var at time.Time
	err := db.conn.QueryRow(
		`SELECT created_at FROM report_deliveries WHERE schedule_id=? ORDER BY created_at DESC LIMIT 1`,
		scheduleID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// ListReportDeliveries returns an app's recent runs, newest first, each with its
// per-person rows.
func (db *DB) ListReportDeliveries(appID string, limit int) ([]ReportDelivery, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := db.conn.Query(`
		SELECT id, app_id, schedule_id, deployment_id, created_at, delivered, skipped, failed
		FROM report_deliveries WHERE app_id=? ORDER BY created_at DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportDelivery{}
	for rows.Next() {
		var d ReportDelivery
		if err := rows.Scan(&d.ID, &d.AppID, &d.ScheduleID, &d.DeploymentID,
			&d.CreatedAt, &d.Delivered, &d.Skipped, &d.Failed); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		people, err := db.reportDeliveryRecipients(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Recipients = people
	}
	return out, nil
}

func (db *DB) reportDeliveryRecipients(deliveryID string) ([]ReportDeliveryRecipient, error) {
	rows, err := db.conn.Query(`
		SELECT COALESCE(user_id, ''), login, email, status, reason
		FROM report_delivery_recipients WHERE delivery_id=? ORDER BY login`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportDeliveryRecipient{}
	for rows.Next() {
		var r ReportDeliveryRecipient
		if err := rows.Scan(&r.UserID, &r.Login, &r.Email, &r.Status, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneReportDeliveryRecipients drops the per-person rows under old deliveries,
// on the announcement's clock and for the announcement's reason.
//
// **The delivery row and its three counts stay.** Two facts with different
// lifetimes: "on the 3rd of March this report reached 12 people and 2 were
// skipped" is the install's own record of what it did, and there is no date on
// which that stops being worth having. "We mailed ana@example.com on the 3rd of
// March" is a named person's data with a shorter honest answer — the same
// distinction the announcement and the app send ledger already draw.
//
// It shares `announcement_recipient_retention_days` rather than owning a second
// setting, because an administrator answering "how long do we keep who we
// mailed" is answering one question about this install, not one per feature.
func (db *DB) PruneReportDeliveryRecipients(days int) (int, time.Time, error) {
	if days <= 0 {
		return 0, time.Time{}, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	res, err := db.conn.Exec(`
		DELETE FROM report_delivery_recipients
		WHERE delivery_id IN (SELECT id FROM report_deliveries WHERE created_at < ?)`, cutoff)
	if err != nil {
		return 0, cutoff, err
	}
	n, err := res.RowsAffected()
	return int(n), cutoff, err
}

// EstateSchedule is one app's re-render or restart schedule, as Admin →
// Automation lists it beside the install's jobs. The two have the same shape to
// that page — one per app, no command, one outcome per firing — so they share
// it; what "last" means differs and is the caller's query.
type EstateSchedule struct {
	ID           string     `json:"id"`
	AppID        string     `json:"app_id"`
	AppName      string     `json:"app_name"`
	AppSlug      string     `json:"app_slug"`
	OwnerLogin   string     `json:"owner_login,omitempty"`
	OwnerID      string     `json:"owner_id,omitempty"`
	OwnerGroupID string     `json:"owner_group_id,omitempty"`
	Schedule     string     `json:"schedule"`
	Enabled      bool       `json:"enabled"`
	NextRunAt    *time.Time `json:"next_run_at,omitempty"`
	// LastRunAt and LastStatus are absent for a schedule that has never fired.
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	LastStatus string     `json:"last_status,omitempty"`
	// AppLocked is the app's lock, which holds every firing without pausing
	// the schedule (ADR §156).
	AppLocked bool `json:"app_locked"`
}

// ListAllRenderSchedules returns every re-render schedule on the install, for
// Admin → Automation's "every schedule on the install" (ADR §33). The last
// firing is the newest deployment the schedule started, since a re-render's
// history is the deployments list.
func (db *DB) ListAllRenderSchedules() ([]EstateSchedule, error) {
	rows, err := db.conn.Query(`
		SELECT s.id, s.app_id, a.name, a.slug, COALESCE(u.login, ''),
		       COALESCE(a.owner_user_id, ''), COALESCE(a.owner_group_id, ''),
		       s.schedule, s.enabled, s.next_run_at, d.started_at, COALESCE(d.status, ''),
		       a.locked_at IS NOT NULL
		FROM app_render_schedules s
			JOIN apps a ON a.id = s.app_id
			LEFT JOIN users u ON u.id = a.owner_user_id
			LEFT JOIN LATERAL (
				SELECT started_at, status FROM deployments
				WHERE app_id = s.app_id AND triggered_by = ?
				ORDER BY started_at DESC NULLS LAST LIMIT 1
			) d ON TRUE
		ORDER BY a.name`, DeployTriggerRender)
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
