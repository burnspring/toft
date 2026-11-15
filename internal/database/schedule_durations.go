package database

// TypicalDuration is how long a schedule's firing usually occupies the
// install: the median of its recent scheduled runs, and how many runs that is
// over. Admin → Automation's Upcoming view draws a firing this long (ADR §156).
type TypicalDuration struct {
	Seconds int `json:"typical_sec"`
	Basis   int `json:"basis"`
}

// typicalBasis is how many recent runs a median is taken over.
const typicalBasis = 10

// TypicalJobDurations returns, per scheduled job definition, the median of
// finished_at − started_at over its last ten scheduled runs that started and
// finished. A skipped run never started; a cancelled one measures somebody's
// decision, not the job; so neither counts. The median rather than the mean,
// so one run that hung for its timeout does not stretch every future night.
// A definition with no such run is absent.
func (db *DB) TypicalJobDurations() (map[string]TypicalDuration, error) {
	rows, err := db.conn.Query(`
		SELECT j.id,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM r.finished_at - r.started_at)),
		       COUNT(*)
		FROM job_definitions j
			JOIN LATERAL (
				SELECT started_at, finished_at FROM job_runs
				WHERE definition_id = j.id AND started_by = ?
				  AND started_at IS NOT NULL AND finished_at IS NOT NULL
				  AND status NOT IN (?, ?)
				ORDER BY created_at DESC LIMIT ?
			) r ON TRUE
		WHERE j.schedule IS NOT NULL AND j.schedule <> ''
		GROUP BY j.id`, JobStartedBySchedule, JobRunSkipped, JobRunCancelled, typicalBasis)
	if err != nil {
		return nil, err
	}
	return scanTypical(rows)
}

// TypicalRenderDurations is TypicalJobDurations for re-renders, keyed by app:
// the median build over the app's last ten finished `render` deployments.
func (db *DB) TypicalRenderDurations() (map[string]TypicalDuration, error) {
	rows, err := db.conn.Query(`
		SELECT s.app_id,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM d.finished_at - d.started_at)),
		       COUNT(*)
		FROM app_render_schedules s
			JOIN LATERAL (
				SELECT started_at, finished_at FROM deployments
				WHERE app_id = s.app_id AND triggered_by = ?
				  AND started_at IS NOT NULL AND finished_at IS NOT NULL
				ORDER BY started_at DESC LIMIT ?
			) d ON TRUE
		GROUP BY s.app_id`, DeployTriggerRender, typicalBasis)
	if err != nil {
		return nil, err
	}
	return scanTypical(rows)
}

type typicalRows interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}

func scanTypical(rows typicalRows) (map[string]TypicalDuration, error) {
	defer rows.Close()
	out := map[string]TypicalDuration{}
	for rows.Next() {
		var id string
		var median float64
		var n int
		if err := rows.Scan(&id, &median, &n); err != nil {
			return nil, err
		}
		if median < 0 {
			median = 0
		}
		out[id] = TypicalDuration{Seconds: int(median + 0.5), Basis: n}
	}
	return out, rows.Err()
}
