package database

import (
	"errors"
	"fmt"
	"time"

	"github.com/axiomhq/hyperloglog"
)

// The anonymous audience's store: one HyperLogLog sketch per app per UTC day
// of the anonymous browsers whose visitor cookie came back (ADR § The
// anonymous audience). A sketch estimates how many ids were added and cannot
// say which, which is the whole of the privacy argument: there is no table of
// visitor ids to join across apps or days, because there is no table of ids.

// AudienceSketch is one (app, day) sketch from a recorder's flush, in the
// library's binary encoding.
type AudienceSketch struct {
	AppID  string
	Day    time.Time
	Sketch []byte
}

// NewAudienceSketch is the one constructor every writer and reader uses, so
// the precision is decided once: 14, ~0.8 % standard error, ~16 KB dense and
// far less while sparse.
func NewAudienceSketch() *hyperloglog.Sketch { return hyperloglog.New14() }

// MergeAudienceSketches folds a flush's sketches into the stored rows.
//
// **One row per app per day, merged under a row lock**, because a sketch is
// not a counter: the additive upsert app_usage_daily uses cannot express a
// union. Per sketch, in one transaction: create the row empty if it is not
// there, lock it, merge in Go, write it back. Two replicas flushing the same
// row serialize for the length of one merge. There is no per-node row,
// because there is no stable node identity to key one by (a pod name changes
// on every rollout). A failed batch is dropped by the caller, as the counters
// are.
func (db *DB) MergeAudienceSketches(rows []AudienceSketch) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	empty, err := NewAudienceSketch().MarshalBinary()
	if err != nil {
		return err
	}
	for _, r := range rows {
		in := NewAudienceSketch()
		if err := in.UnmarshalBinary(r.Sketch); err != nil {
			return fmt.Errorf("audience sketch for %s: %w", r.AppID, err)
		}
		day := r.Day.UTC().Format("2006-01-02")
		if _, err := tx.Exec(`
			INSERT INTO app_audience_daily (app_id, day, sketch) VALUES (?, ?::date, ?)
			ON CONFLICT (app_id, day) DO NOTHING`, r.AppID, day, empty); err != nil {
			return err
		}
		var stored []byte
		if err := tx.QueryRow(`
			SELECT sketch FROM app_audience_daily WHERE app_id = ? AND day = ?::date FOR UPDATE`,
			r.AppID, day).Scan(&stored); err != nil {
			return err
		}
		merged := NewAudienceSketch()
		if err := merged.UnmarshalBinary(stored); err != nil {
			// A row that does not decode is replaced rather than wedging every
			// later flush of the day: the day undercounts, which is the same
			// failure a dropped batch has.
			merged = NewAudienceSketch()
		}
		if err := merged.Merge(in); err != nil {
			return err
		}
		out, err := merged.MarshalBinary()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE app_audience_daily SET sketch = ? WHERE app_id = ? AND day = ?::date`,
			out, r.AppID, day); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// audienceScan streams the sketches matching where (a condition over app_id
// and day) and hands each to fold. Streaming, so an estate's year of sketches
// is never held at once: what is held is one sketch per bucket.
func (db *DB) audienceScan(where string, args []any, fold func(appID, day string, sk *hyperloglog.Sketch)) error {
	rows, err := db.Conn().Query(`
		SELECT app_id, to_char(day, 'YYYY-MM-DD'), sketch FROM app_audience_daily WHERE `+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var appID, day string
		var b []byte
		if err := rows.Scan(&appID, &day, &b); err != nil {
			return err
		}
		sk := NewAudienceSketch()
		if err := sk.UnmarshalBinary(b); err != nil {
			continue // see MergeAudienceSketches: an undecodable row counts nothing
		}
		fold(appID, day, sk)
	}
	return rows.Err()
}

// AppAudienceDaily is one app's distinct anonymous browsers per day over the
// last `days` days, keyed by YYYY-MM-DD. A day with no sketch is absent — the
// switch was off, or nobody's cookie came back — and the caller reports it as
// null, never zero.
func (db *DB) AppAudienceDaily(appID string, days int) (map[string]int64, error) {
	out := map[string]int64{}
	err := db.audienceScan(`app_id = ? AND day > (now() AT TIME ZONE 'UTC')::date - ?::int`,
		[]any{appID, days}, func(_, day string, sk *hyperloglog.Sketch) {
			out[day] = int64(sk.Estimate())
		})
	return out, err
}

// AppAudienceTotal is the union of one app's sketches over the last `days`
// days: one browser on seven days is one. Nil when there is no sketch in the
// window at all.
func (db *DB) AppAudienceTotal(appID string, days int) (*int64, error) {
	var union *hyperloglog.Sketch
	err := db.audienceScan(`app_id = ? AND day > (now() AT TIME ZONE 'UTC')::date - ?::int`,
		[]any{appID, days}, func(_, _ string, sk *hyperloglog.Sketch) {
			if union == nil {
				union = sk
				return
			}
			_ = union.Merge(sk)
		})
	if err != nil || union == nil {
		return nil, err
	}
	n := int64(union.Estimate())
	return &n, nil
}

// Audience grains for the estate read.
const (
	AudienceGrainDay   = "day"
	AudienceGrainWeek  = "week"
	AudienceGrainMonth = "month"
)

// AudienceBucket is one bucket of the estate's audience: distinct signed-in
// people, distinct anonymous browsers, and page loads. AnonymousBrowsers is
// nil for a bucket with no sketch, so a gap in the series reads as a gap.
type AudienceBucket struct {
	Start             string `json:"start"` // YYYY-MM-DD, the bucket's first day
	People            int    `json:"people"`
	AnonymousBrowsers *int64 `json:"anonymous_browsers"`
	PageLoads         int64  `json:"page_loads"`
}

// AudienceTotals is the same three across the whole window — people and
// browsers distinct across it, so neither is the sum of the buckets.
type AudienceTotals struct {
	People            int    `json:"people"`
	AnonymousBrowsers *int64 `json:"anonymous_browsers"`
	PageLoads         int64  `json:"page_loads"`
}

// bucketStart is the first day of the bucket holding day, in Go so the
// sketches (folded here) and the counters (grouped in SQL by date_trunc, whose
// week is the ISO Monday-start week) agree on every boundary.
func bucketStart(day time.Time, grain string) time.Time {
	switch grain {
	case AudienceGrainWeek:
		off := (int(day.Weekday()) + 6) % 7 // Monday = 0
		return day.AddDate(0, 0, -off)
	case AudienceGrainMonth:
		return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return day
}

// EstateAudience is the install's audience between since and until (UTC days,
// inclusive), bucketed by grain, oldest first, every bucket present. Across
// every app, deleted ones included: the rows outlive their app until the
// access record's retention, and the question is the install's reach.
func (db *DB) EstateAudience(since, until time.Time, grain string) ([]AudienceBucket, AudienceTotals, error) {
	var totals AudienceTotals
	switch grain {
	case AudienceGrainDay, AudienceGrainWeek, AudienceGrainMonth:
	default:
		return nil, totals, errors.New("grain must be day, week or month")
	}
	since, until = since.UTC().Truncate(24*time.Hour), until.UTC().Truncate(24*time.Hour)
	s, u := since.Format("2006-01-02"), until.Format("2006-01-02")

	var buckets []AudienceBucket
	index := map[string]int{}
	for d := bucketStart(since, grain); !d.After(until); {
		key := d.Format("2006-01-02")
		index[key] = len(buckets)
		buckets = append(buckets, AudienceBucket{Start: key})
		switch grain {
		case AudienceGrainWeek:
			d = d.AddDate(0, 0, 7)
		case AudienceGrainMonth:
			d = d.AddDate(0, 1, 0)
		default:
			d = d.AddDate(0, 0, 1)
		}
	}

	rows, err := db.Conn().Query(`
		SELECT to_char(date_trunc(?::text, day::timestamp)::date, 'YYYY-MM-DD'),
		       COUNT(DISTINCT user_id) FILTER (WHERE user_id <> '' AND user_id NOT LIKE 'key:%'),
		       COALESCE(SUM(page_loads), 0)
		FROM app_usage_daily
		WHERE day >= ?::date AND day <= ?::date
		GROUP BY 1`, grain, s, u)
	if err != nil {
		return nil, totals, err
	}
	for rows.Next() {
		var start string
		var people int
		var loads int64
		if err := rows.Scan(&start, &people, &loads); err != nil {
			rows.Close()
			return nil, totals, err
		}
		if i, ok := index[start]; ok {
			buckets[i].People, buckets[i].PageLoads = people, loads
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, totals, err
	}

	if err := db.Conn().QueryRow(`
		SELECT COUNT(DISTINCT user_id) FILTER (WHERE user_id <> '' AND user_id NOT LIKE 'key:%'),
		       COALESCE(SUM(page_loads), 0)
		FROM app_usage_daily WHERE day >= ?::date AND day <= ?::date`, s, u).
		Scan(&totals.People, &totals.PageLoads); err != nil {
		return nil, totals, err
	}

	per := map[string]*hyperloglog.Sketch{}
	var all *hyperloglog.Sketch
	err = db.audienceScan(`day >= ?::date AND day <= ?::date`, []any{s, u},
		func(_, day string, sk *hyperloglog.Sketch) {
			t, perr := time.Parse("2006-01-02", day)
			if perr != nil {
				return
			}
			key := bucketStart(t, grain).Format("2006-01-02")
			if b := per[key]; b != nil {
				_ = b.Merge(sk)
			} else {
				per[key] = sk.Clone()
			}
			if all == nil {
				all = sk
			} else {
				_ = all.Merge(sk)
			}
		})
	if err != nil {
		return nil, totals, err
	}
	for key, sk := range per {
		if i, ok := index[key]; ok {
			n := int64(sk.Estimate())
			buckets[i].AnonymousBrowsers = &n
		}
	}
	if all != nil {
		n := int64(all.Estimate())
		totals.AnonymousBrowsers = &n
	}
	return buckets, totals, nil
}
