package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Job run states. The set is closed and its distinctions are the ones a
// developer needs to fix the right problem: a run waits for a slot, runs once,
// and ends in exactly one of five states — or is `skipped`, a scheduled firing
// the job's overlap policy declined. Nothing re-runs a run by itself.
const (
	JobRunQueued = "queued"
	// JobRunClaimed is a run a dispatcher has taken and is starting. Shown as
	// queued: it has not started executing anything.
	JobRunClaimed     = "claimed"
	JobRunRunning     = "running"
	JobRunSucceeded   = "succeeded"
	JobRunFailed      = "failed"
	JobRunCancelled   = "cancelled"
	JobRunTimedOut    = "timed_out"
	JobRunOutOfMemory = "out_of_memory"
	JobRunSkipped     = "skipped"
)

// Who started a run. The vocabulary every surface prints, and the column's.
const (
	// JobStartedByApp is the app acting as itself.
	JobStartedByApp = "app"
	// JobStartedByViewer is the app acting for the signed-in person in front of
	// it; the run's actor is that person.
	JobStartedByViewer = "viewer"
	// JobStartedByCLI is `toft job run`, or anything else holding a key.
	JobStartedByCLI = "cli"
	// JobStartedBySchedule is the job's own schedule.
	JobStartedBySchedule = "schedule"
	// JobStartedByUI is Run now, or Run again, in Toft's UI.
	JobStartedByUI = "ui"
)

// Overlap policies: what a scheduled firing does while the job's last run is
// still going.
const (
	JobOverlapSkip  = "skip"
	JobOverlapQueue = "queue"
)

// Notify policies: who hears about a run's outcome.
const (
	JobNotifyNone     = "none"
	JobNotifyFailures = "failures"
	JobNotifyAlways   = "always"
)

// JobRunTerminal reports whether a status will not change again.
func JobRunTerminal(status string) bool {
	switch status {
	case JobRunSucceeded, JobRunFailed, JobRunCancelled,
		JobRunTimedOut, JobRunOutOfMemory, JobRunSkipped:
		return true
	}
	return false
}

// JobRunOpen reports whether a run is waiting or executing: queued, claimed or
// running. The three a cancel can stop and an overlap check counts.
func JobRunOpen(status string) bool {
	switch status {
	case JobRunQueued, JobRunClaimed, JobRunRunning:
		return true
	}
	return false
}

// JobDefinition is one declared job. Definitions come from toft.toml and
// nowhere else: a deploy reconciles them, and the UI and the API only operate
// them (start, pause, cancel).
type JobDefinition struct {
	ID    string `json:"id"`
	AppID string `json:"app_id"`
	Name  string `json:"name"`

	// Command is a shell command line; Args is a single binary's argv. Exactly
	// one is set.
	Command  string   `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	Schedule string   `json:"schedule,omitempty"`
	// ParamsSpec is the declared parameter list, stored as JSON: the job's
	// contract with the code that reads it, defaults and choices included.
	ParamsSpec json.RawMessage `json:"params_spec"`

	// TimeoutSec is what toft.toml declared, 0 when it declared nothing. The
	// effective figure — after the install's default and ceiling — is
	// computed by the caller that has the settings in hand.
	TimeoutSec int `json:"timeout_sec"`
	// OverrideMemoryMB and OverrideCPUMillicores are an administrator's limits
	// for this one job, 0 when none is set and the install's job defaults
	// apply. toft.toml declares neither, and a deploy never writes them, so
	// an override survives every redeploy (UpsertJobDefinitions).
	OverrideMemoryMB      int `json:"override_memory_mb,omitempty"`
	OverrideCPUMillicores int `json:"override_cpu_millicores,omitempty"`

	// Overlap is skip or queue; Notify is none, failures or always.
	Overlap string `json:"overlap"`
	Notify  string `json:"notify"`

	// Enabled is false while the job is paused from the Automation tab. A
	// pause lasts until the next deploy, which re-applies toft.toml.
	Enabled   bool       `json:"enabled"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	// ExpectWithinSec is how long the job may go without a successful run
	// before it is reported overdue (ADR §105); nil is no expectation.
	ExpectWithinSec *int `json:"expect_within_sec,omitempty"`
	// EnabledAt is when the definition was created or last switched on — the
	// floor of the overdue window, so a new job is not overdue on arrival.
	EnabledAt time.Time `json:"enabled_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type JobRun struct {
	ID           string `json:"id"`
	DefinitionID string `json:"definition_id,omitempty"`
	AppID        string `json:"app_id"`
	JobName      string `json:"job"`

	Status string `json:"status"`
	// StartedBy is app, viewer, cli, schedule or ui.
	StartedBy string `json:"started_by"`
	ActorID   string `json:"-"`
	// ActorLabel is who to call the person who started this, resolved at read
	// time and empty when nobody did. Populated only by the platform-facing
	// reads: an app must never learn who its viewer is, so the app-facing
	// surface has a response type of its own that carries neither of these.
	ActorLabel string `json:"started_by_user,omitempty"`

	Params json.RawMessage `json:"params"`

	ExecutionMode string `json:"execution_mode"`
	ExternalID    string `json:"-"`
	ReleaseRef    string `json:"-"`

	ClaimedBy string     `json:"-"`
	ClaimedAt *time.Time `json:"-"`

	QueuedAt   time.Time  `json:"queued_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DeadlineAt *time.Time `json:"deadline,omitempty"`

	ExitCode      *int   `json:"exit_code,omitempty"`
	FailureStage  string `json:"failure_stage,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	// PeakMemoryMB is the most memory the run was seen to use, nil when the
	// run never reported any.
	PeakMemoryMB *int `json:"peak_memory_mb,omitempty"`
	// CPUThrottledSeconds is how long the run was held at its CPU limit, nil
	// where its cgroup kept no such counter.
	CPUThrottledSeconds *float64 `json:"cpu_throttled_seconds,omitempty"`

	Progress        int    `json:"progress"`
	ProgressMessage string `json:"progress_message,omitempty"`

	// RerunOf is the run this one deliberately repeats — `toft job rerun`, Run
	// again. A soft reference: retention may delete the run it points at.
	RerunOf string `json:"rerun_of,omitempty"`

	// IdempotencyKey is what collapsed a repeated start onto this run, derived
	// by default and overridable by the caller. Not serialized: it is a
	// mechanism, and for a derived key it is a hash of the actor, which is not
	// a thing to hand an app that deliberately cannot learn who its viewer is.
	IdempotencyKey string `json:"-"`
	// OutputKey addresses this run's output object, empty when the run never
	// wrote one. The distinction is the point: "succeeded with no artifact" and
	// "succeeded with an artifact that has expired" must be tellable apart, or
	// the second renders as a broken download.
	//
	// The key itself is not serialized — a caller has no use for a store path —
	// but HasOutput is, so a page knows whether to offer a download at all.
	OutputKey string `json:"-"`
	HasOutput bool   `json:"has_output"`

	// ScheduledFor is the schedule tick this run belongs to, or the instant a
	// person named for it. Nil when nobody gave one — never assumed to be now,
	// because a re-run of last Tuesday's work would then process today.
	ScheduledFor *time.Time `json:"scheduled_for,omitempty"`

	// QueuePosition is 1-based while queued and zero otherwise. Computed on
	// demand rather than stored, because it changes every time anything ahead of
	// this run starts.
	QueuePosition int `json:"queue_position,omitempty"`

	// StatusURL and LogsURL are filled by the surface that hands the run out:
	// the run's live status page on the app's origin, and its log.
	StatusURL string `json:"status_url,omitempty"`
	LogsURL   string `json:"logs_url,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// ShownStatus is the status every surface prints. `claimed` is the platform's
// own bookkeeping — a dispatcher has taken the run and is starting it — and to
// anybody waiting on it the run is still queued.
func (r *JobRun) ShownStatus() string {
	if r.Status == JobRunClaimed {
		return JobRunQueued
	}
	return r.Status
}

// --- Definitions ---

// jobDefColumns are the columns a deploy writes. The overrides are not among
// them: they are an administrator's, and a redeploy must not touch them.
const jobDefColumns = `id, app_id, name, command, args, schedule, params_spec,
	timeout_sec, overlap, notify, enabled, next_run_at,
	created_at, updated_at, expect_within_sec, enabled_at`

// jobDefReadColumns are what a read scans: the deploy's, then the overrides.
// GREATEST hides the migration's seed sentinel (00076) until the start that
// resolves it.
const jobDefReadColumns = jobDefColumns + `,
	GREATEST(COALESCE(override_memory_mb, 0), 0), COALESCE(override_cpu_millicores, 0)`

func scanJobDefinition(scan func(...any) error) (*JobDefinition, error) {
	var d JobDefinition
	var schedule sql.NullString
	var spec, args []byte
	err := scan(&d.ID, &d.AppID, &d.Name, &d.Command, &args, &schedule, &spec,
		&d.TimeoutSec, &d.Overlap, &d.Notify, &d.Enabled,
		&d.NextRunAt, &d.CreatedAt, &d.UpdatedAt, &d.ExpectWithinSec, &d.EnabledAt,
		&d.OverrideMemoryMB, &d.OverrideCPUMillicores)
	if err != nil {
		return nil, err
	}
	d.Schedule = schedule.String
	d.ParamsSpec = json.RawMessage(spec)
	if len(args) > 0 {
		if err := json.Unmarshal(args, &d.Args); err != nil {
			return nil, fmt.Errorf("job %s: args: %w", d.Name, err)
		}
	}
	return &d, nil
}

// jobJSONOrEmpty keeps a JSON column's NOT NULL promise. `{}` and "none" are the
// same thing here, and storing one of them as SQL NULL would make every reader
// handle a third case.
func jobJSONOrEmpty(v json.RawMessage) []byte {
	if len(v) == 0 {
		return []byte("{}")
	}
	return v
}

// argsOrNull is a definition's args as the column stores them: NULL when the
// job is a command line.
func argsOrNull(args []string) any {
	if len(args) == 0 {
		return nil
	}
	b, _ := json.Marshal(args)
	return b
}

func overlapOrQueue(o string) string {
	if o == JobOverlapSkip {
		return o
	}
	return JobOverlapQueue
}

func notifyOrNone(n string) string {
	switch n {
	case JobNotifyFailures, JobNotifyAlways:
		return n
	}
	return JobNotifyNone
}

func (db *DB) GetJobDefinition(appID, id string) (*JobDefinition, error) {
	// Scoped by app as well as by ID: a handler that authorized against the
	// app in the path and then looked the row up by its own ID alone is an
	// IDOR, and this is the query that would make one possible.
	row := db.conn.QueryRow(`SELECT `+jobDefReadColumns+` FROM job_definitions WHERE id=? AND app_id=?`, id, appID)
	d, err := scanJobDefinition(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (db *DB) GetJobDefinitionByName(appID, name string) (*JobDefinition, error) {
	row := db.conn.QueryRow(`SELECT `+jobDefReadColumns+` FROM job_definitions WHERE app_id=? AND name=?`, appID, name)
	d, err := scanJobDefinition(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (db *DB) ListJobDefinitions(appID string) ([]JobDefinition, error) {
	rows, err := db.conn.Query(`SELECT `+jobDefReadColumns+` FROM job_definitions WHERE app_id=? ORDER BY name`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobDefinition
	for rows.Next() {
		d, err := scanJobDefinition(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ScheduledJob is one app's scheduled background job as the estate-wide view
// needs it: the definition, whose app it belongs to, and how its last run went.
//
// Admin → Automation asks one question — what does this install fire, and
// when — and this is its answer.
type ScheduledJob struct {
	JobDefinition
	AppName    string `json:"app_name"`
	AppSlug    string `json:"app_slug"`
	OwnerLogin string `json:"owner_login,omitempty"`
	// The owning principal, so a group-owned app is not rendered as having no
	// owner at all — which is the one thing the ownership model says is
	// impossible. `AppOwner` on the client renders either from these.
	OwnerID      string     `json:"owner_id,omitempty"`
	OwnerGroupID string     `json:"owner_group_id,omitempty"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	LastStatus   string     `json:"last_status,omitempty"`
	// AppLocked is the owning app's lock, which holds every firing without
	// pausing the schedule (ADR §156).
	AppLocked bool `json:"app_locked"`
}

// ListAllScheduledJobs returns every job on the install that has a schedule.
// Admin-only at the handler: this is the estate, including apps the caller
// holds no grant on.
//
// Only scheduled jobs, because a job that is triggered and never fires by
// itself has no answer to "when" — it belongs to the run list, not the
// schedule list.
func (db *DB) ListAllScheduledJobs() ([]ScheduledJob, error) {
	rows, err := db.conn.Query(`
		SELECT j.id, j.app_id, j.name, j.command, j.args, j.schedule, j.params_spec,
		       j.timeout_sec, j.overlap, j.notify,
		       j.enabled, j.next_run_at, j.created_at, j.updated_at,
		       j.expect_within_sec, j.enabled_at,
		       GREATEST(COALESCE(j.override_memory_mb, 0), 0), COALESCE(j.override_cpu_millicores, 0),
		       a.name, a.slug, COALESCE(u.login, ''),
		       COALESCE(a.owner_user_id, ''), COALESCE(a.owner_group_id, ''),
		       r.queued_at, COALESCE(r.status, ''), a.locked_at IS NOT NULL
		FROM job_definitions j
			JOIN apps a ON a.id = j.app_id
			LEFT JOIN users u ON u.id = a.owner_user_id
			LEFT JOIN LATERAL (
				SELECT queued_at, status FROM job_runs
				WHERE definition_id = j.id ORDER BY queued_at DESC LIMIT 1
			) r ON TRUE
		WHERE j.schedule IS NOT NULL
		ORDER BY a.name, j.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScheduledJob
	for rows.Next() {
		var s ScheduledJob
		var schedule sql.NullString
		var spec, args []byte
		if err := rows.Scan(&s.ID, &s.AppID, &s.Name, &s.Command, &args, &schedule,
			&spec, &s.TimeoutSec, &s.Overlap, &s.Notify,
			&s.Enabled, &s.NextRunAt, &s.CreatedAt, &s.UpdatedAt,
			&s.ExpectWithinSec, &s.EnabledAt,
			&s.OverrideMemoryMB, &s.OverrideCPUMillicores,
			&s.AppName, &s.AppSlug, &s.OwnerLogin,
			&s.OwnerID, &s.OwnerGroupID, &s.LastRunAt, &s.LastStatus, &s.AppLocked); err != nil {
			return nil, err
		}
		s.Schedule = schedule.String
		s.ParamsSpec = json.RawMessage(spec)
		if len(args) > 0 {
			_ = json.Unmarshal(args, &s.Args)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetJobEnabled pauses or resumes a definition's schedule — the one field the
// platform writes on a definition toft.toml owns. A pause lasts until the next
// deploy, which reconciles every definition back to enabled.
func (db *DB) SetJobEnabled(appID, id string, enabled bool, nextRun *time.Time) error {
	_, err := db.conn.Exec(
		`UPDATE job_definitions SET
			enabled_at=CASE WHEN NOT enabled AND ? THEN ? ELSE enabled_at END,
			enabled=?, next_run_at=?, updated_at=? WHERE id=? AND app_id=?`,
		enabled, time.Now().UTC(), enabled, nextRun, time.Now().UTC(), id, appID)
	return err
}

// SetJobResources records an administrator's limits for one job, audited
// job.resources.change. nil leaves a figure as it is; 0 clears it, handing
// the job back to the install's job default. Its own statement, beside the
// pause in the same request, so the handler can apply both or neither.
func (db *DB) SetJobResources(appID, id string, memoryMB, cpuMillicores *int, actorType, actorID, requestID, ip, userAgent string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var fromMem, fromCPU int
	if err := tx.QueryRow(`SELECT GREATEST(COALESCE(override_memory_mb, 0), 0), COALESCE(override_cpu_millicores, 0)
		FROM job_definitions WHERE id=? AND app_id=? FOR UPDATE`, id, appID).Scan(&fromMem, &fromCPU); err != nil {
		return err
	}
	toMem, toCPU := fromMem, fromCPU
	if memoryMB != nil {
		toMem = *memoryMB
	}
	if cpuMillicores != nil {
		toCPU = *cpuMillicores
	}
	if _, err := tx.Exec(`UPDATE job_definitions SET override_memory_mb=?, override_cpu_millicores=?, updated_at=?
		WHERE id=? AND app_id=?`, nullIfZero(toMem), nullIfZero(toCPU), time.Now().UTC(), id, appID); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action: "job.resources.change", AppID: appID,
		TargetType: "job", TargetID: id,
		Before:    jsonStr(map[string]int{"memory_limit_mb": fromMem, "cpu_limit_millicores": fromCPU}),
		After:     jsonStr(map[string]int{"memory_limit_mb": toMem, "cpu_limit_millicores": toCPU}),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// SeedJobMemoryOverrides resolves migration 00076's sentinel: a job whose
// memory, before job limits became an administrator's, was the install's app
// default — which may be the environment's, and SQL cannot see it. The
// sentinel is -<ceiling that applied>; the job keeps min(app default,
// ceiling) as its override, or none where that is the job default. Run at
// every start, and a no-op once nothing carries the sentinel.
func (db *DB) SeedJobMemoryOverrides(appDefaultMB, jobDefaultMB int) error {
	_, err := db.conn.Exec(`UPDATE job_definitions SET override_memory_mb =
			CASE WHEN LEAST(?::integer, -override_memory_mb) = ?::integer THEN NULL
			     ELSE LEAST(?::integer, -override_memory_mb) END
		WHERE override_memory_mb < 0`, appDefaultMB, jobDefaultMB, appDefaultMB)
	return err
}

// nullIfZero is an optional integer column's write: 0 is NULL.
func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// ReconcileManifestJobs replaces the definitions of an app with exactly
// `defs`, in one transaction. toft.toml is the only place a job is defined, so
// what it no longer declares is deleted — its runs stay, keyed by name.
//
// A deploy resumes a paused job: a pause lasts until the next deploy, because
// the deploy re-applies toft.toml and toft.toml says the job runs.
//
// **A live schedule the deploy did not change keeps its stored next_run_at.**
// The manifest's value is computed from "now" at detect, seconds before this
// write, so taking it unconditionally dropped a firing whenever a deploy
// landed just after a cron minute (the due tick was overwritten with the next
// one), and put back an already-fired tick when the scheduler's sweep ran
// between detect and this write — the minute ran twice and the next was lost
// (drills on trytoft, 2026-10-28). The zone is part of the expression, so
// comparing `schedule` compares both. A changed schedule, a new job and a
// paused one take the manifest's value.
//
// keepPauses is the one exception, for a scheduled re-render's own flip: a
// deployment nobody made does not resume a job a person paused, so a paused
// definition keeps enabled, enabled_at and next_run_at as they are.
func (db *DB) ReconcileManifestJobs(appID string, defs []JobDefinition) error {
	return db.ReconcileManifestJobsKeeping(appID, defs, false)
}

// ReconcileManifestJobsKeeping is ReconcileManifestJobs, keeping a paused
// definition paused when keepPauses is set.
func (db *DB) ReconcileManifestJobsKeeping(appID string, defs []JobDefinition, keepPauses bool) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	keep := make([]string, 0, len(defs))
	now := time.Now().UTC()
	for i := range defs {
		d := &defs[i]
		d.AppID = appID
		if d.ID == "" {
			d.ID = uuid.New().String()
		}
		d.ParamsSpec = jobJSONOrEmpty(d.ParamsSpec)
		d.Overlap = overlapOrQueue(d.Overlap)
		d.Notify = notifyOrNone(d.Notify)
		d.Enabled = true
		if _, err := tx.Exec(`
			INSERT INTO job_definitions (`+jobDefColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (app_id, name) DO UPDATE SET
				command=excluded.command, args=excluded.args, schedule=excluded.schedule,
				params_spec=excluded.params_spec, timeout_sec=excluded.timeout_sec,
				overlap=excluded.overlap, notify=excluded.notify,
				enabled=CASE WHEN ? THEN job_definitions.enabled ELSE TRUE END,
				enabled_at=CASE WHEN job_definitions.enabled OR ? THEN job_definitions.enabled_at
				                ELSE excluded.enabled_at END,
				next_run_at=CASE
					WHEN ? AND NOT job_definitions.enabled THEN job_definitions.next_run_at
					WHEN job_definitions.enabled
					     AND job_definitions.schedule IS NOT DISTINCT FROM excluded.schedule
					     AND job_definitions.next_run_at IS NOT NULL
					     AND excluded.next_run_at IS NOT NULL
					THEN job_definitions.next_run_at
					ELSE excluded.next_run_at END,
				expect_within_sec=excluded.expect_within_sec,
				updated_at=excluded.updated_at`,
			d.ID, appID, d.Name, d.Command, argsOrNull(d.Args), nullIfEmpty(d.Schedule),
			[]byte(d.ParamsSpec), d.TimeoutSec,
			d.Overlap, d.Notify, true, d.NextRunAt,
			now, now, d.ExpectWithinSec, now,
			keepPauses, keepPauses, keepPauses); err != nil {
			return fmt.Errorf("upserting job %q: %w", d.Name, err)
		}
		keep = append(keep, d.Name)
	}

	if len(keep) == 0 {
		if _, err := tx.Exec(`DELETE FROM job_definitions WHERE app_id=?`, appID); err != nil {
			return err
		}
		return tx.Commit()
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := []any{appID}
	for _, n := range keep {
		args = append(args, n)
	}
	if _, err := tx.Exec(
		`DELETE FROM job_definitions WHERE app_id=? AND name NOT IN (`+placeholders+`)`,
		args...); err != nil {
		return err
	}
	return tx.Commit()
}

// --- Scheduling ---

// ClaimDueSchedules claims the jobs due at `now` and returns them with anything
// the pass disabled.
//
// The claim itself is `ClaimDueSchedulesIn` — one implementation over both
// schedule tables, including the seed pass this used to carry alone (see
// `schedules.go`). What is left here is the part that is genuinely a job's:
// loading the definitions the dispatcher needs.
func (db *DB) ClaimDueSchedules(limit int, now time.Time, next NextFunc) ([]DueJob, []DisabledSchedule, error) {
	claimed, err := db.ClaimDueSchedulesIn(ScheduleTableJobs, limit, now, next)
	if err != nil {
		return nil, nil, err
	}
	if len(claimed.DueIDs) == 0 {
		return nil, claimed.Disabled, nil
	}

	defs := make([]DueJob, 0, len(claimed.DueIDs))
	for _, id := range claimed.DueIDs {
		d, err := scanJobDefinition(db.conn.QueryRow(
			`SELECT `+jobDefReadColumns+` FROM job_definitions WHERE id = ?`, id).Scan)
		if err != nil {
			// Deleted between the claim and this read: nothing to enqueue, and
			// the rest of the pass still should.
			if err == sql.ErrNoRows {
				continue
			}
			return nil, claimed.Disabled, err
		}
		defs = append(defs, DueJob{JobDefinition: *d, Tick: claimed.Ticks[id]})
	}
	return defs, claimed.Disabled, nil
}

// DueJob is a claimed schedule and the tick it was claimed for. The definition's
// own next_run_at has already moved on by the time it is read, so the tick
// travels beside it rather than being re-derived.
type DueJob struct {
	JobDefinition
	Tick time.Time
}

// SetNextRunAt seeds or clears a schedule's next fire time.
func (db *DB) SetNextRunAt(id string, at *time.Time) error {
	_, err := db.conn.Exec(`UPDATE job_definitions SET next_run_at=? WHERE id=?`, at, id)
	return err
}

// OverdueJob is an enabled job with an expectation and no success inside it.
type OverdueJob struct {
	ID, AppID, Name string
	ExpectWithinSec int
	// Since is the floor the window ran from: the last success, or when the
	// job was created or switched on, whichever is later.
	Since time.Time
}

// ListOverdueJobs returns the enabled jobs whose expect_within_sec has passed
// since their last successful run (or since enabled_at, when that is later),
// and that do not already have an open overdue event — so one stuck job is one
// event, resolved by its next success, rather than a row per sweep.
//
// A disabled job is never overdue: a pause is somebody's decision. A locked
// app's jobs are not either, because the lock is the reason and it is shown.
func (db *DB) ListOverdueJobs(now time.Time) ([]OverdueJob, error) {
	rows, err := db.conn.Query(`
		SELECT j.id, j.app_id, j.name, j.expect_within_sec,
		       GREATEST(j.enabled_at, COALESCE(s.last, j.enabled_at)) AS since
		FROM job_definitions j
			JOIN apps a ON a.id = j.app_id
			LEFT JOIN LATERAL (
				SELECT max(finished_at) AS last FROM job_runs
				WHERE definition_id = j.id AND status = 'succeeded'
			) s ON TRUE
		WHERE j.enabled AND j.expect_within_sec IS NOT NULL AND a.locked_at IS NULL
			AND GREATEST(j.enabled_at, COALESCE(s.last, j.enabled_at))
			    + j.expect_within_sec * interval '1 second' < ?
			AND NOT EXISTS (
				SELECT 1 FROM app_events e
				WHERE e.app_id = j.app_id AND e.kind = 'job_run_overdue'
					AND e.fingerprint = j.id || ':overdue' AND e.resolved_at IS NULL
			)
		ORDER BY j.app_id, j.name`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OverdueJob
	for rows.Next() {
		var o OverdueJob
		if err := rows.Scan(&o.ID, &o.AppID, &o.Name, &o.ExpectWithinSec, &o.Since); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
