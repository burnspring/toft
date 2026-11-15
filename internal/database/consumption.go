package database

import (
	"fmt"
	"strings"
	"time"

	"toft/internal/resource"
)

// Retention is tiered, following the existing daily request roll-up: fine
// intervals for investigation, daily roll-ups for history.
const (
	// IntervalRetention is how long the per-flush rows survive. Long enough to
	// investigate last week's incident and to see a finished job run's cost
	// beside the app that triggered it; short enough that a busy install is
	// not storing a row per app per ten seconds indefinitely.
	IntervalRetention = 7 * 24 * time.Hour
	// DailyRetention bounds the history. Just over a year, so a
	// year-on-year comparison is possible and an unbounded table is not.
	DailyRetention = 400 * 24 * time.Hour
)

// RecordConsumption writes a batch of accumulated intervals. Called only from
// the resource recorder's flush loop — never from a request path.
func (db *DB) RecordConsumption(intervals []resource.Interval) error {
	if len(intervals) == 0 {
		return nil
	}

	// One multi-row INSERT per chunk: a flush covers every running app at
	// once, and a statement per app would turn a routine tick into N round
	// trips. Chunked, because the extended protocol takes 65,535 bind
	// parameters and a batch held through an outage outgrew it (H-4).
	const perStatement = 2000
	for start := 0; start < len(intervals); start += perStatement {
		end := min(start+perStatement, len(intervals))
		if err := db.recordConsumptionChunk(intervals[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) recordConsumptionChunk(intervals []resource.Interval) error {
	var (
		placeholders []string
		args         []any
	)
	for _, iv := range intervals {
		if iv.AppID == "" {
			continue
		}
		placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		var periods, throttled, throttledSecs any
		if t := iv.Throttling; t != nil {
			periods, throttled, throttledSecs = t.Periods, t.ThrottledPeriods, t.ThrottledSeconds
		}
		args = append(args,
			iv.AppID, iv.JobRunID, iv.Start.UTC(), iv.End.UTC(),
			iv.CPUSeconds, iv.MemoryByteSeconds, iv.PeakMemoryBytes, iv.Gap,
			periods, throttled, throttledSecs)
	}
	if len(placeholders) == 0 {
		return nil
	}

	_, err := db.conn.Exec(`
		INSERT INTO app_consumption_intervals
			(app_id, job_run_id, started_at, ended_at, cpu_seconds, memory_byte_seconds, peak_memory_bytes, has_gap,
			 periods, throttled_periods, throttled_seconds)
		VALUES `+strings.Join(placeholders, ", "), args...)
	return err
}

// RolledUpConsumption is what one roll-up pass did.
//
// Folded and Removed are separate numbers because they are separate facts: the
// first is app-days *written* into history, the second is interval and daily
// rows *deleted*. Reporting the fold as a removal would have the sweep claim to
// have reclaimed rows it created.
type RolledUpConsumption struct {
	// Folded is how many app-day rows the fold wrote or updated.
	Folded int64
	// Removed is interval rows plus daily rows past their own retention.
	Removed int64
}

// RollUpConsumption folds intervals that ended before `before` into the daily
// table and deletes them, then trims history past its retention.
//
// The fold and the delete are one transaction on purpose: rolling up rows that
// are still present would double-count them on the next sweep, and the whole
// value of the daily table is that it agrees with the intervals it was built
// from.
func (db *DB) RollUpConsumption(before time.Time) (res RolledUpConsumption, err error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	cutoff := before.UTC()

	// Job runs lose their individual identity here and are folded into their
	// app's totals — per-run detail is an investigation question answered from
	// the interval table while it is retained.
	fold, err := tx.Exec(`
		INSERT INTO app_consumption_daily
			(app_id, day, cpu_seconds, memory_byte_seconds, peak_memory_bytes, observed_seconds, has_gap,
			 periods, throttled_periods, throttled_seconds)
		SELECT app_id,
		       (started_at AT TIME ZONE 'UTC')::date,
		       SUM(cpu_seconds),
		       SUM(memory_byte_seconds),
		       MAX(peak_memory_bytes),
		       SUM(EXTRACT(EPOCH FROM (ended_at - started_at))),
		       BOOL_OR(has_gap),
		       SUM(periods), SUM(throttled_periods), SUM(throttled_seconds)
		FROM app_consumption_intervals
		WHERE ended_at < ?
		GROUP BY app_id, (started_at AT TIME ZONE 'UTC')::date
		ON CONFLICT (app_id, day) DO UPDATE SET
			cpu_seconds         = app_consumption_daily.cpu_seconds + EXCLUDED.cpu_seconds,
			memory_byte_seconds = app_consumption_daily.memory_byte_seconds + EXCLUDED.memory_byte_seconds,
			-- Peak is a maximum, not a sum: two windows each peaking at 1 GiB
			-- did not together peak at 2.
			peak_memory_bytes   = GREATEST(app_consumption_daily.peak_memory_bytes, EXCLUDED.peak_memory_bytes),
			observed_seconds    = app_consumption_daily.observed_seconds + EXCLUDED.observed_seconds,
			has_gap             = app_consumption_daily.has_gap OR EXCLUDED.has_gap,
			-- Absent stays absent until a measured day adds to it.
			periods             = CASE WHEN EXCLUDED.periods IS NULL THEN app_consumption_daily.periods
			                           ELSE COALESCE(app_consumption_daily.periods, 0) + EXCLUDED.periods END,
			throttled_periods   = CASE WHEN EXCLUDED.throttled_periods IS NULL THEN app_consumption_daily.throttled_periods
			                           ELSE COALESCE(app_consumption_daily.throttled_periods, 0) + EXCLUDED.throttled_periods END,
			throttled_seconds   = CASE WHEN EXCLUDED.throttled_seconds IS NULL THEN app_consumption_daily.throttled_seconds
			                           ELSE COALESCE(app_consumption_daily.throttled_seconds, 0) + EXCLUDED.throttled_seconds END
	`, cutoff)
	if err != nil {
		return RolledUpConsumption{}, fmt.Errorf("rolling up consumption: %w", err)
	}
	res.Folded, _ = fold.RowsAffected()

	intervals, err := tx.Exec(`DELETE FROM app_consumption_intervals WHERE ended_at < ?`, cutoff)
	if err != nil {
		return RolledUpConsumption{}, fmt.Errorf("clearing rolled-up intervals: %w", err)
	}
	n, _ := intervals.RowsAffected()
	res.Removed += n

	daily, err := tx.Exec(`DELETE FROM app_consumption_daily WHERE day < ?`,
		time.Now().UTC().Add(-DailyRetention))
	if err != nil {
		return RolledUpConsumption{}, fmt.Errorf("trimming consumption history: %w", err)
	}
	n, _ = daily.RowsAffected()
	res.Removed += n

	return res, tx.Commit()
}

// Consumption is one app's totals over a period, in the shape both the
// interval and the daily table reduce to. Nothing here records which execution
// mode or backend produced the numbers — that is the point of one metric
// model, and it is what makes a mixed install comparable.
type Consumption struct {
	AppID string `json:"app_id"`
	// JobRunID is set only on per-run breakdowns.
	JobRunID string `json:"job_run_id,omitempty"`

	CPUSeconds        float64 `json:"cpu_seconds"`
	MemoryByteSeconds float64 `json:"memory_byte_seconds"`
	PeakMemoryBytes   int64   `json:"peak_memory_bytes"`
	// ObservedSeconds is how much measured time the figures cover, which is
	// what an average must be divided by. A period containing an outage is not
	// a cheap period.
	ObservedSeconds float64 `json:"observed_seconds"`
	// HasGap warns that the period is not a complete accounting.
	HasGap bool `json:"has_gap"`
}

// AverageMemoryBytes is the time-weighted mean over the observed window.
func (c Consumption) AverageMemoryBytes() float64 {
	if c.ObservedSeconds <= 0 {
		return float64(c.PeakMemoryBytes)
	}
	return c.MemoryByteSeconds / c.ObservedSeconds
}

// consumptionSelect is the aggregate both tables reduce to, so a query
// spanning the roll-up boundary sums like with like.
const consumptionSelect = `
	COALESCE(SUM(cpu_seconds), 0),
	COALESCE(SUM(memory_byte_seconds), 0),
	COALESCE(MAX(peak_memory_bytes), 0),
	COALESCE(SUM(observed_seconds), 0),
	COALESCE(BOOL_OR(has_gap), FALSE)`

// ListConsumption returns per-app totals over [from, to), for the apps the
// scope allows.
//
// Reads narrow through the same readable-apps predicate as everything else, so
// consumption data cannot be used to probe for apps the caller could not
// otherwise see — a per-app cost figure is as good an existence oracle as a
// tag count.
func (db *DB) ListConsumption(scope AppScope, from, to time.Time) ([]Consumption, error) {
	pred, predArgs := scope.readingPredicate()

	// Both tables are read and summed together: the interval table covers the
	// recent window and the daily table the rest, and a period straddling the
	// roll-up boundary must not silently report only one half.
	query := `
		SELECT app_id, ` + consumptionSelect + `
		FROM (
			SELECT i.app_id, i.cpu_seconds, i.memory_byte_seconds, i.peak_memory_bytes,
			       EXTRACT(EPOCH FROM (i.ended_at - i.started_at)) AS observed_seconds, i.has_gap
			FROM app_consumption_intervals i
			JOIN apps ON apps.id = i.app_id AND ` + pred + `
			WHERE i.started_at >= ? AND i.started_at < ?

			UNION ALL

			SELECT d.app_id, d.cpu_seconds, d.memory_byte_seconds, d.peak_memory_bytes,
			       d.observed_seconds, d.has_gap
			FROM app_consumption_daily d
			JOIN apps ON apps.id = d.app_id AND ` + pred + `
			WHERE d.day >= ?::date AND d.day < ?::date
		) rows
		GROUP BY app_id
		ORDER BY 2 DESC`

	args := append([]any{}, predArgs...)
	args = append(args, from.UTC(), to.UTC())
	args = append(args, predArgs...)
	args = append(args, from.UTC(), to.UTC())

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Consumption
	for rows.Next() {
		var c Consumption
		if err := rows.Scan(&c.AppID, &c.CPUSeconds, &c.MemoryByteSeconds,
			&c.PeakMemoryBytes, &c.ObservedSeconds, &c.HasGap); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppConsumption returns one app's totals over [from, to). The caller must
// already have authorized read on the app; passing the scope keeps the
// visibility rule in one place even so.
func (db *DB) AppConsumption(scope AppScope, appID string, from, to time.Time) (Consumption, error) {
	all, err := db.ListConsumption(scope, from, to)
	if err != nil {
		return Consumption{}, err
	}
	for _, c := range all {
		if c.AppID == appID {
			return c, nil
		}
	}
	// No rows is a legitimate answer — an app that has not run consumed
	// nothing — and is not distinguishable from, nor should it be reported
	// differently to, an app the caller cannot see.
	return Consumption{AppID: appID}, nil
}

// JobRunConsumption breaks one app's period down by job run, so a two-hour
// job's cost is visible beside the app that triggered it. Only the interval
// table can answer this: the daily roll-up folds runs into their app.
func (db *DB) JobRunConsumption(scope AppScope, appID string, from, to time.Time) ([]Consumption, error) {
	pred, predArgs := scope.readingPredicate()

	args := append([]any{}, predArgs...)
	args = append(args, appID, from.UTC(), to.UTC())

	rows, err := db.conn.Query(`
		SELECT job_run_id, `+consumptionSelect+`
		FROM (
			SELECT i.job_run_id, i.cpu_seconds, i.memory_byte_seconds, i.peak_memory_bytes,
			       EXTRACT(EPOCH FROM (i.ended_at - i.started_at)) AS observed_seconds, i.has_gap
			FROM app_consumption_intervals i
			JOIN apps ON apps.id = i.app_id AND `+pred+`
			WHERE i.job_run_id <> ''
			  AND i.app_id = ?
			  AND i.started_at >= ? AND i.started_at < ?
		) rows
		GROUP BY job_run_id
		ORDER BY 2 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Consumption
	for rows.Next() {
		var c Consumption
		c.AppID = appID
		if err := rows.Scan(&c.JobRunID, &c.CPUSeconds, &c.MemoryByteSeconds,
			&c.PeakMemoryBytes, &c.ObservedSeconds, &c.HasGap); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CPUThrottling is one app's throttling over a window: its service's own
// intervals only (a job's figure is on its run), and only those that carried
// the counters, with how many seconds of the window they cover.
type CPUThrottling struct {
	Periods          int64
	ThrottledPeriods int64
	ThrottledSeconds float64
	// MeasuredSeconds is how much of the window the counted intervals
	// cover: a window is judged only once it is nearly whole.
	MeasuredSeconds float64
}

// AppCPUThrottling sums an app's throttling since a moment.
func (db *DB) AppCPUThrottling(appID string, since time.Time) (CPUThrottling, error) {
	var t CPUThrottling
	err := db.conn.QueryRow(`
		SELECT COALESCE(SUM(periods), 0), COALESCE(SUM(throttled_periods), 0),
		       COALESCE(SUM(throttled_seconds), 0),
		       COALESCE(SUM(EXTRACT(EPOCH FROM (ended_at - started_at))), 0)
		FROM app_consumption_intervals
		WHERE app_id = ? AND job_run_id = '' AND started_at >= ? AND periods IS NOT NULL`,
		appID, since.UTC()).Scan(&t.Periods, &t.ThrottledPeriods, &t.ThrottledSeconds, &t.MeasuredSeconds)
	return t, err
}
