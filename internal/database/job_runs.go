package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/runtime"
	"toft/internal/textutil"
)

// ErrQueueLimit means the app's queue is full: as many runs are waiting as
// the install allows (max_queued_runs_per_app). Runs at once beyond the app's
// limit are not refused — they queue — so this is the one refusal a start gets
// for being busy.
var ErrQueueLimit = errors.New("this app's queue is full")

// QueueLimitError is ErrQueueLimit with its two numbers, so the refusal can
// carry them: an app's QueueFull reads in_flight and limit from the 429, and a
// body without them gave every SDK 0 and 0 (drills on trytoft, 2026-10-23).
type QueueLimitError struct {
	Queued int // runs waiting now
	Limit  int // max_queued_runs_per_app
}

func (e *QueueLimitError) Error() string {
	return fmt.Sprintf("%s: %d runs are waiting, the most this install allows", ErrQueueLimit, e.Queued)
}

func (e *QueueLimitError) Unwrap() error { return ErrQueueLimit }

const jobRunColumns = `id, definition_id, app_id, job_name, status, started_by, COALESCE(actor_id,'') AS actor_id,
	params, execution_mode, external_id, release_ref, claimed_by, claimed_at,
	queued_at, started_at, finished_at, deadline_at, exit_code,
	failure_stage, failure_reason, progress, progress_message, idempotency_key, output_key,
	rerun_of, peak_memory_mb, created_at, scheduled_for, cpu_throttled_seconds, scheduled_for_offset`

func scanJobRun(scan func(...any) error) (*JobRun, error) {
	var r JobRun
	var defID, idemKey, outputKey, rerunOf sql.NullString
	var params []byte
	var offset string
	err := scan(&r.ID, &defID, &r.AppID, &r.JobName, &r.Status, &r.StartedBy, &r.ActorID,
		&params, &r.ExecutionMode, &r.ExternalID, &r.ReleaseRef, &r.ClaimedBy, &r.ClaimedAt,
		&r.QueuedAt, &r.StartedAt, &r.FinishedAt, &r.DeadlineAt, &r.ExitCode,
		&r.FailureStage, &r.FailureReason, &r.Progress, &r.ProgressMessage, &idemKey, &outputKey,
		&rerunOf, &r.PeakMemoryMB, &r.CreatedAt, &r.ScheduledFor, &r.CPUThrottledSeconds, &offset)
	if err != nil {
		return nil, err
	}
	r.ScheduledFor = InScheduledOffset(r.ScheduledFor, offset)
	r.DefinitionID = defID.String
	r.IdempotencyKey = idemKey.String
	r.OutputKey = outputKey.String
	r.RerunOf = rerunOf.String
	r.HasOutput = r.OutputKey != ""
	r.Params = json.RawMessage(params)
	return &r, nil
}

// EnqueueJobRun creates a queued run, refusing only when the app's queue is
// already full.
func (db *DB) EnqueueJobRun(r *JobRun, maxQueued int) error {
	_, err := db.EnqueueJobRunIdempotent(r, maxQueued, 0)
	return err
}

// EnqueueJobRunIdempotent is EnqueueJobRun with deduplication.
//
// When r carries an idempotency key, a run started with that key within
// `window` of now is returned instead of a new row being inserted, and r is
// left untouched. A nil first return means the insert happened and r now
// describes it.
//
// The lookup and the queue count run under a per-app advisory lock, which is
// what makes them decisions rather than guesses: a plain COUNT takes no locks
// and READ COMMITTED gives each concurrent enqueue a snapshot from before the
// others inserted, so N callers would all read "0 queued" and all proceed.
// Scoped to the app, so enqueues for different apps never contend.
//
// Deduplication is checked *before* the queue cap, deliberately. A repeat of a
// run already accepted adds nothing to the queue, and refusing it would turn
// the double-fire this exists to absorb into the loudest possible failure.
func (db *DB) EnqueueJobRunIdempotent(r *JobRun, maxQueued int, window time.Duration) (*JobRun, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`SELECT pg_advisory_xact_lock(('x' || substr(md5(?), 1, 16))::bit(64)::bigint)`,
		"job-enqueue:"+r.AppID); err != nil {
		return nil, err
	}

	if r.IdempotencyKey != "" && window > 0 {
		row := tx.QueryRow(`
			SELECT `+jobRunColumns+` FROM job_runs
			WHERE app_id=? AND idempotency_key=? AND queued_at > ?
			ORDER BY queued_at DESC LIMIT 1`,
			r.AppID, r.IdempotencyKey, time.Now().UTC().Add(-window))
		existing, err := scanJobRun(row.Scan)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	if maxQueued > 0 {
		var queued int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM job_runs WHERE app_id=? AND status=?`,
			r.AppID, JobRunQueued).Scan(&queued); err != nil {
			return nil, err
		}
		if queued >= maxQueued {
			return nil, &QueueLimitError{Queued: queued, Limit: maxQueued}
		}
	}

	if err := insertJobRun(tx, r); err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}

// insertJobRun writes r as a new row, filling its defaults.
func insertJobRun(tx *Tx, r *JobRun) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	if r.Status == "" {
		r.Status = JobRunQueued
	}
	if r.StartedBy == "" {
		r.StartedBy = JobStartedByUI
	}
	if r.ExecutionMode == "" {
		// The column's own default, applied here because the INSERT below names
		// the column and therefore never reaches it. The mode is recorded per
		// run rather than read from the app, so a promotion between runs does
		// not retroactively reclassify history.
		r.ExecutionMode = string(runtime.ModeIsolated)
	}
	if len(r.Params) == 0 {
		r.Params = json.RawMessage(`{}`)
	}
	now := time.Now().UTC()
	r.QueuedAt, r.CreatedAt = now, now
	// The offset the instant was given in: a named one's as named, a
	// schedule's tick in its schedule's zone (the scheduler hands it so), so
	// every read of the run says it (BB-045, BB-050).
	offset := ScheduledOffset(r.ScheduledFor)
	_, err := tx.Exec(`
		INSERT INTO job_runs (id, definition_id, app_id, job_name, status, started_by, actor_id,
			params, execution_mode, release_ref, idempotency_key, rerun_of,
			scheduled_for, failure_reason, finished_at, queued_at, created_at, scheduled_for_offset)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, nullIfEmpty(r.DefinitionID), r.AppID, r.JobName, r.Status, r.StartedBy, nullIfEmpty(r.ActorID),
		[]byte(r.Params), r.ExecutionMode, r.ReleaseRef, nullIfEmpty(r.IdempotencyKey),
		nullIfEmpty(r.RerunOf), r.ScheduledFor, r.FailureReason, r.FinishedAt, now, now, offset)
	return err
}

// ScheduledOffset is the offset an instant was named in, as RFC 3339 writes
// it, for the column beside scheduled_for: "" for none and for UTC, which
// the column's timestamp already says (BB-045, fourth 2026-11-03 black-box
// list). A retry copies its original's instant read back through
// InScheduledOffset, so it keeps the offset too.
func ScheduledOffset(at *time.Time) string {
	if at == nil || at.Location() == time.UTC {
		return ""
	}
	if _, secs := at.Zone(); secs == 0 {
		return ""
	}
	return at.Format("-07:00")
}

// InScheduledOffset is the stored instant in the offset it was named in, or
// in UTC when none was recorded.
func InScheduledOffset(at *time.Time, offset string) *time.Time {
	if at == nil {
		return nil
	}
	utc := at.UTC()
	if offset == "" {
		return &utc
	}
	parsed, err := time.Parse("-07:00", offset)
	if err != nil {
		return &utc
	}
	_, secs := parsed.Zone()
	in := at.In(time.FixedZone("", secs))
	return &in
}

// ErrTickAlreadyEnqueued is a scheduled firing whose tick already has a run of
// this job started by the schedule: the tick was enqueued once, and a second
// enqueue would run the same minute twice. Nothing is written.
var ErrTickAlreadyEnqueued = errors.New("this scheduled time already has a run")

// EnqueueScheduledJobRun is the schedule's own enqueue: under the same lock as
// every other start, it records a `skipped` run instead when the job's overlap
// policy is skip and its last run is still open, or when the app's queue is
// full. The first return reports whether the firing was recorded as skipped.
//
// A skipped firing is a row, never a gap, so "it did not happen" is something
// to read rather than something to infer from a missing night. **A full queue
// is a skipped row too**, and its *QueueLimitError comes back beside true so the
// caller still records the job_schedule_skipped event — an operational problem
// an owner is told about, where an overlap skip is the policy working. Once
// that was the event alone, and `toft job runs` read as if the schedule had
// never fired (drills on trytoft, 2026-10-28).
//
// **One run per tick.** A run with started_by = schedule already holding this
// job's scheduled_for refuses with ErrTickAlreadyEnqueued, whatever the state of
// next_run_at that let the tick come round again.
func (db *DB) EnqueueScheduledJobRun(r *JobRun, overlap string, maxQueued int) (bool, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`SELECT pg_advisory_xact_lock(('x' || substr(md5(?), 1, 16))::bit(64)::bigint)`,
		"job-enqueue:"+r.AppID); err != nil {
		return false, err
	}

	if r.ScheduledFor != nil {
		var seen int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM job_runs
			WHERE app_id=? AND job_name=? AND started_by=? AND scheduled_for=?`,
			r.AppID, r.JobName, JobStartedBySchedule, r.ScheduledFor.UTC()).Scan(&seen); err != nil {
			return false, err
		}
		if seen > 0 {
			return false, ErrTickAlreadyEnqueued
		}
	}

	skip := func(reason string) error {
		now := time.Now().UTC()
		r.Status = JobRunSkipped
		r.FinishedAt = &now
		r.FailureReason = reason
		return insertJobRun(tx, r)
	}

	if overlap == JobOverlapSkip {
		var open int
		if err := tx.QueryRow(`
			SELECT COUNT(*) FROM job_runs
			WHERE app_id=? AND job_name=? AND status IN ('queued','claimed','running')`,
			r.AppID, r.JobName).Scan(&open); err != nil {
			return false, err
		}
		if open > 0 {
			if err := skip("the last run was still going, and this job is set to overlap = \"skip\""); err != nil {
				return false, err
			}
			return true, tx.Commit()
		}
	}

	if maxQueued > 0 {
		var queued int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM job_runs WHERE app_id=? AND status=?`,
			r.AppID, JobRunQueued).Scan(&queued); err != nil {
			return false, err
		}
		if queued >= maxQueued {
			full := &QueueLimitError{Queued: queued, Limit: maxQueued}
			if err := skip(fmt.Sprintf("the app's queue was full (%d runs were waiting, the most this install allows)",
				queued)); err != nil {
				return false, err
			}
			if err := tx.Commit(); err != nil {
				return false, err
			}
			return true, full
		}
	}
	if err := insertJobRun(tx, r); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// QueuePosition reports how many queued runs are ahead of this one for its
// app, 1 for the front of the queue and 0 once it is no longer queued.
//
// Cheap, because the queue drains ORDER BY queued_at: "3rd in line" is most of
// what somebody actually wants when they ask about a queue.
func (db *DB) QueuePosition(run *JobRun) (int, error) {
	// A claimed run is still queued to everyone reading it (ShownStatus), so it
	// has a position, and claimed runs ahead count — they have not started.
	if run == nil || run.ShownStatus() != JobRunQueued {
		return 0, nil
	}
	var ahead int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM job_runs WHERE app_id=? AND status IN (?, ?) AND queued_at < ?`,
		run.AppID, JobRunQueued, JobRunClaimed, run.QueuedAt).Scan(&ahead)
	return ahead + 1, err
}

// ResolveJobRunID turns the eight-character prefix `toft job runs` prints into
// the run's full id, within one app. "" when nothing or more than one run
// matches; an id that is already full comes back as itself.
func (db *DB) ResolveJobRunID(appID, idOrPrefix string) (string, error) {
	if len(idOrPrefix) < 6 || strings.ContainsAny(idOrPrefix, "%_") {
		return "", nil
	}
	rows, err := db.conn.Query(`SELECT id FROM job_runs WHERE app_id=? AND id LIKE ? LIMIT 2`,
		appID, idOrPrefix+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		return "", rows.Err()
	}
	return ids[0], rows.Err()
}

// SetJobRunOutputKey records where this run's output object landed. Called when
// a run actually PUTs something, so an empty column keeps meaning "produced no
// artifact" rather than "we did not look".
func (db *DB) SetJobRunOutputKey(appID, runID, key string) error {
	_, err := db.conn.Exec(
		`UPDATE job_runs SET output_key=? WHERE id=? AND app_id=?`, nullIfEmpty(key), runID, appID)
	return err
}

// ListActorJobRuns is scope=viewer: one person's runs of one app, newest first.
//
// The actor is resolved by the platform from a token the app relayed, never
// asserted by the app — so this answers a question *about* the viewer without
// disclosing the viewer, and an app stores no per-user state to make "come back
// tomorrow" work. An empty actorID would match every app-started run and is
// refused rather than silently widening the scope.
func (db *DB) ListActorJobRuns(appID, actorID, jobName, status string, limit int) ([]JobRun, error) {
	if actorID == "" {
		return []JobRun{}, nil
	}
	limit = clampListLimit(limit)
	query := `SELECT ` + jobRunColumns + ` FROM job_runs WHERE app_id=? AND actor_id=?`
	args := []any{appID, actorID}
	if jobName != "" {
		query += ` AND job_name=?`
		args = append(args, jobName)
	}
	// The same status predicate ListJobRuns applies: the viewer-scoped list
	// validated status and then ignored it, so Mine(status="failed") answered
	// every run (code review 2026-11-11, H-6).
	switch status {
	case "":
	case JobRunQueued:
		query += ` AND status IN ('queued','claimed')`
	default:
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY queued_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []JobRun{}
	for rows.Next() {
		r, err := scanJobRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimJobRuns takes ownership of up to `limit` queued runs for this node,
// never more for one app than that app's limit on runs at once allows.
//
// The per-app limit is why this is not a bare SKIP LOCKED claim: a run waits
// in the queue until one of its own app's slots frees, and another app's runs
// go past it in the meantime. The claim takes a global advisory lock for its
// transaction, so two dispatchers cannot both see the same free slot — a claim
// is one short statement every few seconds, and serializing it costs nothing a
// person could notice.
//
// **The outer status=queued is not redundant with the subquery's.** A cancel
// takes the row lock without the advisory one, and Postgres re-checks only the
// outer WHERE against the row that writer committed — never the subquery that
// chose it. Without it, a run cancelled while the claim waited went back to
// `claimed`, was started, and held one of its app's slots.
//
// **A scheduled run waits its turn behind its own job** — overlap = "queue",
// ADR §98. It is not claimable while another run of the same job is claimed or
// running, or while one queued before it is still waiting. Only the schedule
// is held: overlap is what a *scheduled firing* does, and a person's start runs
// beside whatever is going, as it always has. The hold is applied before the
// ranking, so a held run takes none of its app's slots from another job's.
// Once it was the app's runs-at-once limit alone, and a 90-second every-minute
// job ran two at a time (drills on trytoft, 2026-10-28).
func (db *DB) ClaimJobRuns(nodeID string, limit, perAppRunning int) ([]JobRun, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(('x' || substr(md5('job-claim'), 1, 16))::bit(64)::bigint)`); err != nil {
		return nil, err
	}
	if perAppRunning < 1 {
		perAppRunning = 1
	}
	now := time.Now().UTC()
	rows, err := tx.Query(`
		UPDATE job_runs SET status=?, claimed_by=?, claimed_at=?
		WHERE status=? AND id IN (
			SELECT id FROM (
				SELECT q.id, q.queued_at,
				       ROW_NUMBER() OVER (PARTITION BY q.app_id ORDER BY q.queued_at) AS n,
				       (SELECT COUNT(*) FROM job_runs a
				        WHERE a.app_id = q.app_id AND a.status IN ('claimed','running')) AS busy
				FROM job_runs q WHERE q.status=?
				AND NOT (q.started_by=? AND EXISTS (
					SELECT 1 FROM job_runs o
					WHERE o.app_id = q.app_id AND o.job_name = q.job_name AND o.id <> q.id
					  AND (o.status IN ('claimed','running')
					       OR (o.status = 'queued' AND (o.queued_at, o.id) < (q.queued_at, q.id)))))
			) ranked
			WHERE n + busy <= ?
			ORDER BY queued_at
			LIMIT ?
		)
		RETURNING `+jobRunColumns,
		JobRunClaimed, nodeID, now, JobRunQueued, JobRunQueued, JobStartedBySchedule, perAppRunning, limit)
	if err != nil {
		return nil, err
	}
	var out []JobRun
	for rows.Next() {
		r, err := scanJobRun(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// MarkJobRunStarted records that the backend has accepted the workload, along
// with the handle used to ask about it later and the deadline the sweep
// enforces.
//
// The deadline is *stored* rather than kept as an in-memory timer, so a
// platform restart does not lose it and leave a job running forever.
//
// Only by the node holding the claim: a claim released as stale and taken by
// another dispatcher is that one's to start, and a starter that stalled past
// the claim timeout and then wrote its handle over it would leave two
// workloads behind one row, the second unattended. Refused, it stops its own.
func (db *DB) MarkJobRunStarted(runID, nodeID, externalID string, deadline time.Time) error {
	now := time.Now().UTC()
	res, err := db.conn.Exec(`
		UPDATE job_runs SET status=?, external_id=?, started_at=?, deadline_at=?
		WHERE id=? AND status=? AND claimed_by=?`,
		JobRunRunning, externalID, now, deadline.UTC(), runID, JobRunClaimed, nodeID)
	if err != nil {
		return err
	}
	// A cancel that landed while the backend was starting the workload leaves
	// the row non-claimed, and a stale release hands it to another node;
	// reporting that lets the caller stop what it started.
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// FinishJobRun writes a terminal status. It refuses to overwrite one that is
// already terminal, so a late completion callback cannot resurrect a run the
// sweep has already timed out — the first terminal answer wins.
func (db *DB) FinishJobRun(runID, status string, exitCode *int, stage, reason string) error {
	_, err := db.FinishJobRunOnce(runID, status, exitCode, stage, reason)
	return err
}

// FinishJobRunOnce is FinishJobRun reporting whether this call is the one that
// made the run terminal. Two writers reach every run — the runner's own report
// and the poller that backs it up — and only the one that moved the row may
// record the outcome, or a failure is reported twice or not at all.
func (db *DB) FinishJobRunOnce(runID, status string, exitCode *int, stage, reason string) (bool, error) {
	if !JobRunTerminal(status) {
		return false, fmt.Errorf("%q is not a terminal status", status)
	}
	// No caller can make a terminal write fail on its text (LB-6).
	reason = textutil.Clip(reason, 1000)
	// A queued run ended (an app locked, say) never started, so it is not
	// stopped at app_exec; one claimed or running was.
	res, err := db.conn.Exec(`
		UPDATE job_runs
		SET status=?, exit_code=?,
			failure_stage=CASE WHEN status='queued' THEN ? ELSE ? END,
			failure_reason=?, finished_at=?
		WHERE id=? AND status IN ('queued','claimed','running')`,
		status, exitCode, stage, StoppedStage(status, stage), reason, time.Now().UTC(), runID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// StageAppExec is the failure stage of a run whose command could not be
// started or was stopped before it finished.
const StageAppExec = "app_exec"

// StoppedStage is the failure stage a run ending in status is recorded with: a
// run stopped before it finished — timed out, out of memory, cancelled while
// it ran — is at app_exec unless something more specific was said, so a reader
// learns the command was running when it ended (BB-089). A run that ran and
// exited non-zero keeps no stage: its exit code and reason say it.
func StoppedStage(status, stage string) string {
	if stage != "" {
		return stage
	}
	switch status {
	case JobRunTimedOut, JobRunOutOfMemory, JobRunCancelled:
		return StageAppExec
	}
	return stage
}

// TimeOutJobRun marks a run timed out if it is still in flight, and reports
// whether this call is the one that did. Two callers can reach the same run —
// the timer armed at start and the sweep that backs it up — and only the one
// that moved the row may record the outcome, or a run would be counted twice.
func (db *DB) TimeOutJobRun(runID, reason string) (bool, error) {
	reason = textutil.Clip(reason, 1000)
	res, err := db.conn.Exec(`
		UPDATE job_runs SET status=?, failure_stage=?, failure_reason=?, finished_at=?,
		       stop_owed_at = CASE WHEN external_id <> '' THEN now() END
		WHERE id=? AND status IN ('claimed','running')`,
		JobRunTimedOut, StageAppExec, reason, time.Now().UTC(), runID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CancelJobRun stops a run. A queued run is cancelled outright and never
// starts; a running one is marked so the dispatcher stops it at the backend.
// Returns whether the run still needs stopping and, when it does, the
// external id the backend knows it by — read under the same lock, because a
// run the caller read as queued may have been claimed and started since, and
// the id on the caller's stale copy is then empty and the cancel a no-op.
//
// changed is false when the run had already finished: cancelling it is a no-op,
// not an error, and the caller says so rather than reporting a cancel.
func (db *DB) CancelJobRun(appID, runID string) (changed, wasRunning bool, externalID string, err error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, false, "", err
	}
	defer tx.Rollback()

	var status string
	var started bool
	err = tx.QueryRow(`SELECT status, external_id, started_at IS NOT NULL FROM job_runs WHERE id=? AND app_id=? FOR UPDATE`,
		runID, appID).Scan(&status, &externalID, &started)
	if err == sql.ErrNoRows {
		return false, false, "", sql.ErrNoRows
	}
	if err != nil {
		return false, false, "", err
	}
	if JobRunTerminal(status) {
		return false, false, "", nil
	}

	// A run cancelled while it ran was stopped before it finished (BB-089);
	// one cancelled before it started — in the queue, or claimed with its
	// process still being spawned — never ran the app, and has no stage. The
	// claimed run is still stopped below, for that process.
	stage := ""
	if started {
		stage = StageAppExec
	}
	if _, err := tx.Exec(
		`UPDATE job_runs SET status=?, finished_at=?, failure_stage=?, failure_reason=?,
		        stop_owed_at = CASE WHEN external_id <> '' THEN now() END
		 WHERE id=?`,
		JobRunCancelled, time.Now().UTC(), stage, "cancelled", runID); err != nil {
		return false, false, "", err
	}
	if err := tx.Commit(); err != nil {
		return false, false, "", err
	}
	return true, status == JobRunRunning || status == JobRunClaimed, externalID, nil
}

// StopOwedRun is a terminal run whose workload the platform still owes a stop
// (stop_owed_at): what the jobs sweep needs to ask the backend about it again.
type StopOwedRun struct {
	ID         string
	AppID      string
	ExternalID string
	OwedSince  time.Time
}

// MarkJobRunStopOwed records that a terminal run's workload is owed a stop,
// for the paths whose terminal write is not TimeOutJobRun or CancelJobRun (a
// lock, a backend-reported infrastructure failure, a reported run that would
// not exit). handle is the workload's backend id: it is written only onto a
// row that has none — an orphaned start, whose MarkJobRunStarted was refused —
// and never over a different one. A run still open is not marked; its own
// controls still reach it.
func (db *DB) MarkJobRunStopOwed(runID, handle string) error {
	_, err := db.conn.Exec(`
		UPDATE job_runs SET
		    external_id = CASE WHEN external_id = '' THEN ? ELSE external_id END,
		    stop_owed_at = COALESCE(stop_owed_at, now())
		WHERE id=? AND status NOT IN ('queued','claimed','running')
		  AND (external_id = ? OR (external_id = '' AND ? <> ''))`,
		handle, runID, handle, handle)
	return err
}

// ListStopOwedJobRuns returns the terminal runs whose workload is still owed a
// stop, oldest first.
func (db *DB) ListStopOwedJobRuns(limit int) ([]StopOwedRun, error) {
	rows, err := db.conn.Query(`
		SELECT id, app_id, external_id, stop_owed_at FROM job_runs
		WHERE stop_owed_at IS NOT NULL
		ORDER BY stop_owed_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StopOwedRun
	for rows.Next() {
		var r StopOwedRun
		if err := rows.Scan(&r.ID, &r.AppID, &r.ExternalID, &r.OwedSince); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearStopOwed records that the backend reported a run's workload gone.
func (db *DB) ClearStopOwed(runID string) error {
	_, err := db.conn.Exec(`UPDATE job_runs SET stop_owed_at = NULL WHERE id=?`, runID)
	return err
}

// SetJobRunProgress records a progress report from inside the run: a
// percentage and an optional short message. Ignored once terminal: a straggling
// report must not un-finish a run.
func (db *DB) SetJobRunProgress(runID string, percent int, message string) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	// Cleaned and clipped on a character, never a byte: a byte-slice cut inside
	// a multi-byte sequence, or a NUL anywhere, was refused by Postgres and the
	// progress call answered 500 — and the SDK then raised inside the job
	// (code review 2026-11-11, H-2).
	message = textutil.Clip(textutil.Clean(message), 200)
	_, err := db.conn.Exec(`
		UPDATE job_runs SET progress=?, progress_message=? WHERE id=? AND status IN ('claimed','running')`,
		percent, message, runID)
	return err
}

// NoteJobRunPeakMemory raises the run's recorded peak to mb when mb is higher.
// Reported as the run goes, so a run the kernel kills keeps the figure that
// explains why.
func (db *DB) NoteJobRunPeakMemory(runID string, mb int) error {
	if mb <= 0 {
		return nil
	}
	_, err := db.conn.Exec(`
		UPDATE job_runs SET peak_memory_mb = GREATEST(COALESCE(peak_memory_mb, 0), ?) WHERE id=?`,
		mb, runID)
	return err
}

// NoteJobRunThrottled raises the run's recorded CPU throttling to seconds when
// that is higher: the runner reports the counter cumulatively, so the latest
// report is the run's figure so far.
func (db *DB) NoteJobRunThrottled(runID string, seconds float64) error {
	if seconds < 0 {
		return nil
	}
	_, err := db.conn.Exec(`
		UPDATE job_runs SET cpu_throttled_seconds = GREATEST(COALESCE(cpu_throttled_seconds, 0), ?) WHERE id=?`,
		seconds, runID)
	return err
}

// LastStderrLines is the last n non-blank lines a run wrote to stderr, oldest
// first, trimmed; none when it wrote none. A failed run's reason is chosen from
// them (jobs.reasonLine): the last thing a program prints before it exits
// non-zero is almost always why, but not always — Node ends an uncaught
// exception with its own version banner, after the error.
func (db *DB) LastStderrLines(runID string, n int) ([]string, error) {
	rows, err := db.conn.Query(`
		SELECT line FROM job_run_logs
		WHERE run_id=? AND stream='stderr' AND btrim(line) <> ''
		ORDER BY seq DESC LIMIT ?`, runID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSpace(line))
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (db *DB) GetJobRun(runID string) (*JobRun, error) {
	row := db.conn.QueryRow(`SELECT `+jobRunColumns+` FROM job_runs WHERE id=?`, runID)
	r, err := scanJobRun(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// labelledJobRunColumns adds the actor's login to the standard set.
//
// Resolved at read time by a join rather than denormalized onto the row: a
// person can be renamed, and history that kept saying the old name would be
// wrong in the one place somebody goes to find out who did something.
const labelledJobRunColumns = `r.id, r.definition_id, r.app_id, r.job_name, r.status, r.started_by,
	COALESCE(r.actor_id, '') AS actor_id, r.params, r.execution_mode, r.external_id, r.release_ref, r.claimed_by,
	r.claimed_at, r.queued_at, r.started_at, r.finished_at, r.deadline_at, r.exit_code,
	r.failure_stage, r.failure_reason, r.progress, r.progress_message, r.idempotency_key,
	r.output_key, r.rerun_of, r.peak_memory_mb, r.created_at, r.scheduled_for, r.cpu_throttled_seconds,
	r.scheduled_for_offset, COALESCE(u.login, '')`

func scanLabelledJobRun(scan func(...any) error) (*JobRun, error) {
	var label string
	r, err := scanJobRun(func(dest ...any) error {
		return scan(append(dest, &label)...)
	})
	if err != nil {
		return nil, err
	}
	r.ActorLabel = label
	return r, nil
}

// GetAppJobRun scopes the lookup by app as well as run ID. Anything taking an
// ID from the URL must scope its query by the app it authorized against.
//
// actorID scopes it further, to runs that person started; empty means no
// viewer scope — an app token with no relayed viewer reads the app's runs, and
// the owner's own surface reads all of them. The scope is in the query rather
// than compared by the caller after the read, because the caller that
// remembers to compare is the one that has the bug's fix and the three beside
// it are the ones that forgot. A run another viewer started is nil here: its
// existence is theirs.
func (db *DB) GetAppJobRun(appID, runID, actorID string) (*JobRun, error) {
	query := `SELECT ` + labelledJobRunColumns + `
		FROM job_runs r LEFT JOIN users u ON u.id = r.actor_id
		WHERE r.id=? AND r.app_id=?`
	args := []any{runID, appID}
	if actorID != "" {
		query += ` AND r.actor_id=?`
		args = append(args, actorID)
	}
	row := db.conn.QueryRow(query, args...)
	r, err := scanLabelledJobRun(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ListJobRuns is the app's own view of its runs, newest first. jobName narrows
// it in SQL, as ListActorJobRuns does: a filter applied after the LIMIT fetches
// the newest runs of *any* job and drops the rest, so `job=report&limit=20`
// could answer with none of the report's twenty.
func (db *DB) ListJobRuns(appID, jobName, status string, limit int) ([]JobRun, error) {
	limit = clampListLimit(limit)
	query := `SELECT ` + labelledJobRunColumns + `
		 FROM job_runs r LEFT JOIN users u ON u.id = r.actor_id
		 WHERE r.app_id=?`
	args := []any{appID}
	if jobName != "" {
		query += ` AND r.job_name=?`
		args = append(args, jobName)
	}
	switch status {
	case "":
	case JobRunQueued:
		// What a reader sees as queued is both stored states before a run
		// starts executing.
		query += ` AND r.status IN ('queued','claimed')`
	default:
		query += ` AND r.status=?`
		args = append(args, status)
	}
	query += ` ORDER BY r.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobRun
	for rows.Next() {
		r, err := scanLabelledJobRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// AdminJobRun is one run with enough of its app to render a row on the
// install-wide page. The app columns are joined at read time for the same
// reason the actor's login is: a rename must not leave history saying the old
// name.
type AdminJobRun struct {
	JobRun
	AppName    string `json:"app_name"`
	AppSlug    string `json:"app_slug"`
	OwnerLogin string `json:"owner_login,omitempty"`
}

// The two pseudo-statuses. Neither is ever stored — a row's status is always
// one of the constants above — and both exist because the question an operator
// arrives with ("what is running", "what went wrong") is a group rather than a
// state.
//
// JobRunUnsuccessful groups the three ways a run ends badly. That is a filter,
// never a label: the states are kept apart because the fix differs — a bigger
// memory limit, less work, or a code change — and every row still shows which
// one it was.
const (
	JobRunInFlight     = "in_flight"
	JobRunUnsuccessful = "unsuccessful"
)

// JobRunFilter is the install-wide list's query. Status takes a stored status
// or one of the two pseudo-statuses.
type JobRunFilter struct {
	AppID  string
	Status string
	Since  *time.Time
	Limit  int
}

// ListAllJobRuns is the estate-wide run history, for Admin → Job Runs.
//
// The per-app reads are the ones an owner uses; this is the one an operator
// does, and it is deliberately not scoped by app: an app-started run that fails
// is otherwise invisible until somebody complains. It is admin-only for the
// same reason /system/processes is — it names every app on the install.
func (db *DB) ListAllJobRuns(f JobRunFilter) ([]AdminJobRun, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.AppID != "" {
		where = append(where, "r.app_id = ?")
		args = append(args, f.AppID)
	}
	switch f.Status {
	case "":
	case JobRunInFlight:
		where = append(where, "r.status IN (?, ?, ?)")
		args = append(args, JobRunQueued, JobRunClaimed, JobRunRunning)
	case JobRunUnsuccessful:
		where = append(where, "r.status IN (?, ?, ?)")
		args = append(args, JobRunFailed, JobRunTimedOut, JobRunOutOfMemory)
	case JobRunQueued:
		where = append(where, "r.status IN (?, ?)")
		args = append(args, JobRunQueued, JobRunClaimed)
	default:
		where = append(where, "r.status = ?")
		args = append(args, f.Status)
	}
	if f.Since != nil {
		where = append(where, "r.queued_at >= ?")
		args = append(args, *f.Since)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	args = append(args, limit)

	rows, err := db.conn.Query(
		`SELECT `+labelledJobRunColumns+`, a.name, a.slug, COALESCE(o.login, '')
		 FROM job_runs r
			JOIN apps a ON a.id = r.app_id
			LEFT JOIN users u ON u.id = r.actor_id
			LEFT JOIN users o ON o.id = a.owner_user_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY r.queued_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AdminJobRun{}
	for rows.Next() {
		var a AdminJobRun
		r, err := scanLabelledJobRun(func(dest ...any) error {
			return rows.Scan(append(dest, &a.AppName, &a.AppSlug, &a.OwnerLogin)...)
		})
		if err != nil {
			return nil, err
		}
		a.JobRun = *r
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListActiveJobRuns returns every run the platform believes is in flight.
//
// This is what makes the dispatcher a reconciler rather than a launcher: on
// boot it asks the *backend* about each of these rather than trusting
// in-memory state, so runs survive a platform restart and orphans are caught.
func (db *DB) ListActiveJobRuns() ([]JobRun, error) {
	rows, err := db.conn.Query(
		`SELECT ` + jobRunColumns + ` FROM job_runs WHERE status IN ('claimed','running') ORDER BY queued_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobRun
	for rows.Next() {
		r, err := scanJobRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListOverdueJobRuns returns running rows past their stored deadline.
func (db *DB) ListOverdueJobRuns(now time.Time) ([]JobRun, error) {
	rows, err := db.conn.Query(
		`SELECT `+jobRunColumns+` FROM job_runs
		 WHERE status IN ('claimed','running') AND deadline_at IS NOT NULL AND deadline_at < ?`,
		now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobRun
	for rows.Next() {
		r, err := scanJobRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ReleaseStaleClaims returns runs claimed by a node that never started them to
// the queue. Without this a node dying between claim and start would strand the
// run forever — invisible to every other dispatcher because it is no longer
// queued.
func (db *DB) ReleaseStaleClaims(olderThan time.Time) (int64, error) {
	res, err := db.conn.Exec(`
		UPDATE job_runs SET status=?, claimed_by='', claimed_at=NULL
		WHERE status=? AND claimed_at < ?`,
		JobRunQueued, JobRunClaimed, olderThan.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReleaseClaimsOf returns every run nodeID claimed and never started to the
// queue. For boot: a node whose previous process died between claim and start
// holds claims nothing will ever start, and they need not wait out
// ReleaseStaleClaims' timeout. A claim with a workload is left to Reconcile.
func (db *DB) ReleaseClaimsOf(nodeID string) (int64, error) {
	if nodeID == "" {
		return 0, nil
	}
	res, err := db.conn.Exec(`
		UPDATE job_runs SET status=?, claimed_by='', claimed_at=NULL
		WHERE status=? AND claimed_by=? AND external_id=''`,
		JobRunQueued, JobRunClaimed, nodeID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- Run logs ---

// JobLogLine is one shipped log line.
type JobLogLine struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
}

// AppendJobRunLogs stores a batch shipped over the callback channel. Batched
// by the runner and inserted in one statement: a chatty job would otherwise
// cost a round trip per line.
//
// A line is the job's own output, so it is cleaned of what a text column
// refuses (a NUL, invalid UTF-8) before it is sent. Should the batch still
// fail, it is written again a row at a time, so one line Postgres will not
// take loses that line and not the flush window around it; the error comes
// back only when no row could be written, which is a cause no line carries.
func (db *DB) AppendJobRunLogs(runID string, lines []JobLogLine) error {
	if len(lines) == 0 {
		return nil
	}
	for i := range lines {
		lines[i].Stream = textutil.Clean(lines[i].Stream)
		lines[i].Line = textutil.Clean(lines[i].Line)
	}
	err := db.insertJobRunLogs(runID, lines)
	if err == nil || len(lines) == 1 {
		return err
	}
	return rowByRow(len(lines), err, func(i int) error {
		return db.insertJobRunLogs(runID, lines[i:i+1])
	})
}

// rowByRow is the fallback after a batch insert of n rows failed with
// batchErr: it writes each row on its own and reports the batch's error only
// when every row failed too — a deleted run, a dropped connection, nothing a
// single line explains. Rows it could not write are counted in the operator's
// log, since the surface they were bound for is the one that refused them.
func rowByRow(n int, batchErr error, insert func(i int) error) error {
	failed := 0
	var last error
	for i := 0; i < n; i++ {
		if err := insert(i); err != nil {
			failed++
			last = err
		}
	}
	if failed == n {
		return batchErr
	}
	if failed > 0 {
		log.Printf("log insert: dropped %d of %d line(s) Postgres refused: %v", failed, n, last)
	}
	return nil
}

func (db *DB) insertJobRunLogs(runID string, lines []JobLogLine) error {
	query := `INSERT INTO job_run_logs (run_id, seq, at, stream, line) VALUES `
	args := make([]any, 0, len(lines)*5)
	for i, l := range lines {
		if i > 0 {
			query += ", "
		}
		query += "(?, ?, ?, ?, ?)"
		at := l.At
		if at.IsZero() {
			at = time.Now().UTC()
		}
		args = append(args, runID, l.Seq, at.UTC(), l.Stream, l.Line)
	}
	_, err := db.conn.Exec(query, args...)
	return err
}

// clampListLimit is a run list's page: the default for nothing or a
// non-positive number, the maximum for anything above it — not the default,
// which answered a request for 500 with 50 (code review 2026-11-11, K-1).
func clampListLimit(limit int) int {
	switch {
	case limit <= 0:
		return 50
	case limit > 200:
		return 200
	}
	return limit
}

// GetJobRunLogs returns a run's log lines in order, from `afterSeq` exclusive.
// Ordered by (seq, id): the drop notice shares the seq of the line before it
// (H-8), and insertion order places it after.
//
// **A page never ends inside a seq.** Every client pages by setting `after` to
// the last seq it was given, so a page cut between a line and the notice that
// shares its seq lost the notice for good — and a dropping batch is always
// exactly 2000 lines, so a caught-up follower lost it every time. When a page
// comes back full, every row of its last seq is read and those trailing rows
// replaced with them; the page may then be longer than limit.
func (db *DB) GetJobRunLogs(runID string, afterSeq, limit int) ([]JobLogLine, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	out, err := db.jobRunLogRows(
		`SELECT seq, at, stream, line FROM job_run_logs
		 WHERE run_id=? AND seq > ? ORDER BY seq, id LIMIT ?`, runID, afterSeq, limit)
	if err != nil || len(out) < limit {
		return out, err
	}
	last := out[len(out)-1].Seq
	whole, err := db.jobRunLogRows(
		`SELECT seq, at, stream, line FROM job_run_logs
		 WHERE run_id=? AND seq=? ORDER BY id`, runID, last)
	if err != nil {
		return nil, err
	}
	cut := len(out)
	for cut > 0 && out[cut-1].Seq == last {
		cut--
	}
	return append(out[:cut], whole...), nil
}

func (db *DB) jobRunLogRows(query string, args ...any) ([]JobLogLine, error) {
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobLogLine
	for rows.Next() {
		var l JobLogLine
		if err := rows.Scan(&l.Seq, &l.At, &l.Stream, &l.Line); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ExpireJobRuns trims history. Logs go first and rows outlive them, because a
// run's cost and outcome stay interesting long after its output does.
//
// It returns the output objects belonging to the rows it deleted, so the caller
// can remove them from the store. That is the whole of "an output object
// expires with its run row": nothing trims an artifact at the *log* horizon,
// which would leave a succeeded run with a dead download behind it, and nothing
// leaves one behind after the row that explained it is gone.
//
// It returns the two counts beside them because the caller reports what it
// removed, and the two clocks are the reason they are counted apart: a pass
// that took forty thousand log lines and no rows is retention working, and one
// number for both would make that unreadable.
func (db *DB) ExpireJobRuns(logsBefore, rowsBefore time.Time) (ExpiredRuns, error) {
	var res ExpiredRuns

	logs, err := db.conn.Exec(`
		DELETE FROM job_run_logs WHERE run_id IN (
			SELECT id FROM job_runs WHERE finished_at IS NOT NULL AND finished_at < ?
		)`, logsBefore.UTC())
	if err != nil {
		return res, err
	}
	res.LogLines, _ = logs.RowsAffected()

	rows, err := db.conn.Query(`
		DELETE FROM job_runs WHERE finished_at IS NOT NULL AND finished_at < ?
		RETURNING app_id, id, output_key`, rowsBefore.UTC())
	if err != nil {
		return res, err
	}
	defer rows.Close()

	for rows.Next() {
		var e ExpiredOutput
		var key sql.NullString
		if err := rows.Scan(&e.AppID, &e.RunID, &key); err != nil {
			return res, err
		}
		res.Rows++
		if key.String != "" {
			e.OutputKey = key.String
			res.Outputs = append(res.Outputs, e)
		}
	}
	return res, rows.Err()
}

// ExpiredRuns is what one expiry pass removed.
type ExpiredRuns struct {
	// Rows is how many run rows went.
	Rows int64
	// LogLines is how many pushed log lines went, on their own shorter clock.
	LogLines int64
	// Outputs are the objects the deleted rows owned, for the caller to remove
	// from the store. A subset of Rows: most runs write no output.
	Outputs []ExpiredOutput
}

// ExpiredOutput names one object left over by a deleted run row.
type ExpiredOutput struct {
	AppID     string
	RunID     string
	OutputKey string
}

// MarkJobRunBootstrapped records a host run's single bootstrap, and reports
// false when it had already happened — the second presentation of a credential
// that was meant to be used once.
func (db *DB) MarkJobRunBootstrapped(runID string) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE job_runs SET bootstrapped_at = ? WHERE id = ? AND bootstrapped_at IS NULL`,
		time.Now().UTC(), runID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// JobRunPinnedReleases lists the release references an app's unfinished runs
// are pinned to. Deleting one of those releases makes a queued run fail before
// any code runs, so every deletion path keeps them.
func (db *DB) JobRunPinnedReleases(appID string) ([]string, error) {
	rows, err := db.conn.Query(`
		SELECT DISTINCT release_ref FROM job_runs
		WHERE app_id=? AND release_ref <> '' AND status IN ('queued', 'claimed', 'running')`,
		appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
