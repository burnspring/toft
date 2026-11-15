package database

import "database/sql"

// The two reads telemetry's observable gauges are built on.
//
// Both are called from a metric collection callback — once per export interval,
// sixty seconds by default — and never from a request path. That is the rule
// this whole subsystem follows (`docs/dev-reference.md` § Telemetry): telemetry
// may *read* the accounting paths, and may never become the reason one of them
// writes.
//
// They live in their own file because they belong to no existing subject: one
// is a count of the estate and the other is not a query at all.

// CountQueuedJobRuns is the depth behind toft.job.queue.depth.
//
// `pending` only, not pending-plus-claimed-plus-running: the gauge answers "is
// work piling up",
// and a run that is executing is not piling up. The alert that pages on this
// pairs it with the completion rate, because a deep queue that is moving is a
// busy afternoon and a deep queue that is not is a wedged dispatcher.
func (db *DB) CountQueuedJobRuns() (int64, error) {
	var n int64
	err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM job_runs WHERE status = ?`, JobRunQueued).Scan(&n)
	return n, err
}

// PoolStats reports the database/sql pool's own accounting.
//
// This is a `database/sql` pool over the pgx stdlib driver, not a pgxpool, and
// that decides what can honestly be reported. `sql.DBStats` gives connection
// counts and a *cumulative* wait total; it does not give a per-acquire
// duration, so there is no histogram to be had without instrumenting every
// query — which would put telemetry on the request path to answer a question
// `rate(wait_seconds_total)` already answers from these two counters.
func (db *DB) PoolStats() sql.DBStats { return db.conn.db.Stats() }

// CountAppsByStatus is the estate inventory behind toft.apps.by_status.
//
// Grouped in the database rather than by listing every app and counting in Go:
// the gauge is observed on every collection cycle, and an install with five
// hundred apps should not build five hundred structs a minute to produce seven
// numbers.
//
// Statuses with no apps are absent rather than zero. That is the same
// distinction the observers themselves draw — zero is a claim, absent is the
// truth — and Prometheus renders a missing series and a zero series
// identically on a stacked graph anyway.
func (db *DB) CountAppsByStatus() (map[string]int64, error) {
	rows, err := db.conn.Query(`SELECT status, COUNT(*) FROM apps GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
