package database

import (
	"database/sql"
	"time"

	"toft/internal/textutil"
)

// PlatformLogLine is one line the platform said about one app.
//
// It carries no source. Every row in this table is the platform speaking, and
// the read supplies the label — see the migration for why a column that can
// hold one value is furniture.
type PlatformLogLine struct {
	AppID        string
	WorkloadName string
	// DeploymentID is empty for every line a background actor writes, because
	// none of them holds a deployment. It is the column's absence rather than
	// its emptiness that workload_name exists to cover.
	DeploymentID string
	At           time.Time
	Seq          int
	Stream       string
	Line         string
}

// InsertPlatformLog stores a batch. One statement rather than one per line, for
// the reason AppendJobRunLogs is batched: the sink accumulates whatever a start
// narrated and a round trip per stage mark would put the cost back on the path
// the buffer exists to keep clean.
//
// **The batch is install-wide, so a line belonging to a deleted app must not
// take the rest of it down.** applog.Sink fills one buffer from every app and
// flushes it every two seconds; app_id is `REFERENCES apps(id) ON DELETE
// CASCADE`, so a plain multi-row INSERT carrying one line for an app deleted in
// the last two seconds fails the whole statement — and every other app loses the
// narration the invariants say is kept per app, for a reason that has nothing to
// do with them. Invisible: the lines are simply not there, and the app they
// belong to is fine.
//
// So the rows are filtered on the app still existing, inside the one statement.
// A deleted app's lines are dropped, which is what the cascade would have done
// to them a moment later anyway; every other app's lines are written. There is
// no error, so there is no log line and no level to argue about — see
// internal/events for the other writer of this race, which reports it as an
// expected outcome rather than a failure.
//
// Deliberately not the Go-side version of this: querying the apps table first
// is an extra round trip every two seconds on a path this file is at pains to
// keep cheap, to answer a question the insert can answer for itself.
//
// AppendJobRunLogs and AppendCronRunLogs need none of this. Each takes a single
// run id, so a whole batch belongs to one run and a deleted one can only ever
// lose its own lines — the mixing is what makes this batch different.
//
// The casts are load-bearing: Postgres cannot infer a parameter's type inside a
// VALUES list used as a relation, and infers each column from its first row.
func (db *DB) InsertPlatformLog(lines []PlatformLogLine) error {
	if len(lines) == 0 {
		return nil
	}
	// One batch carries every app's lines, so one line Postgres refuses must
	// not cost the others theirs: cleaned first, and row by row if the batch
	// still fails (AppendJobRunLogs has the same fallback, for one run).
	for i := range lines {
		lines[i].Stream = textutil.Clean(lines[i].Stream)
		lines[i].Line = textutil.Clean(lines[i].Line)
	}
	err := db.insertPlatformLog(lines)
	if err == nil || len(lines) == 1 {
		return err
	}
	return rowByRow(len(lines), err, func(i int) error {
		return db.insertPlatformLog(lines[i : i+1])
	})
}

func (db *DB) insertPlatformLog(lines []PlatformLogLine) error {
	query := `INSERT INTO app_platform_log (app_id, workload_name, deployment_id, at, seq, stream, line)
		SELECT v.app_id, v.workload_name, v.deployment_id, v.at, v.seq, v.stream, v.line
		FROM (VALUES `
	args := make([]any, 0, len(lines)*7)
	for i, l := range lines {
		if i > 0 {
			query += ", "
		}
		query += "(?::text, ?::text, ?::text, ?::timestamptz, ?::integer, ?::text, ?::text)"
		at := l.At
		if at.IsZero() {
			at = time.Now().UTC()
		}
		var deployment any
		if l.DeploymentID != "" {
			deployment = l.DeploymentID
		}
		args = append(args, l.AppID, l.WorkloadName, deployment, at.UTC(), l.Seq, l.Stream, l.Line)
	}
	query += `) AS v(app_id, workload_name, deployment_id, at, seq, stream, line)
		WHERE EXISTS (SELECT 1 FROM apps a WHERE a.id = v.app_id)`

	_, err := db.conn.Exec(query, args...)
	return err
}

// PlatformLogTail returns the newest `limit` lines for an app, **oldest first**.
//
// Newest-first in the query and reversed on the way out, which is the same
// trade the isolated tail makes across two container generations: the budget
// has to be spent on the most recent lines, and the caller wants them in the
// order they were said.
func (db *DB) PlatformLogTail(appID string, limit int) ([]PlatformLogLine, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.conn.Query(
		`SELECT workload_name, deployment_id, at, seq, stream, line
		 FROM app_platform_log
		 WHERE app_id=? ORDER BY at DESC, seq DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PlatformLogLine
	for rows.Next() {
		l := PlatformLogLine{AppID: appID}
		var deployment sql.NullString
		if err := rows.Scan(&l.WorkloadName, &deployment, &l.At, &l.Seq, &l.Stream, &l.Line); err != nil {
			return nil, err
		}
		l.DeploymentID = deployment.String
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// DeletePlatformLogBefore ages out narration, and returns how much it took so
// the sweep can say so.
func (db *DB) DeletePlatformLogBefore(cutoff time.Time) (int64, error) {
	res, err := db.conn.Exec(`DELETE FROM app_platform_log WHERE at < ?`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// LatestDeploymentEnd is the newest deployment of an app, narrowly: its id,
// status, when it finished and why it failed — the four columns the log read
// needs to say what a start that failed printed (BB-003), without dragging a
// build log through the connection on every read of the tab. Nil when the app
// has none.
func (db *DB) LatestDeploymentEnd(appID string) (*Deployment, error) {
	var d Deployment
	err := db.conn.QueryRow(`
		SELECT id, status, started_at, finished_at, failure_reason, failure_message, failure_output
		FROM deployments WHERE app_id=? ORDER BY started_at DESC NULLS LAST, id DESC LIMIT 1`,
		appID).Scan(&d.ID, &d.Status, &d.StartedAt, &d.FinishedAt, &d.FailureReason, &d.FailureMessage, &d.FailureOutput)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.AppID = appID
	return &d, nil
}
