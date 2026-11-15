package database

import (
	"fmt"
	"time"
)

// The key kind in api_key_usage_daily. One class is left since app keys were
// removed (migration 00065); the column stays, so a future class need not
// reshape the counter table.
const KeyKindUser = "user"

// APIKeyUsageBump is one aggregated counter increment, produced by
// internal/usage's flush. AppID is "" for a platform API call.
type APIKeyUsageBump struct {
	KeyID       string
	KeyKind     string
	Day         time.Time
	AppID       string
	StatusClass int // 2, 4, 5 — the first digit of the response status
	Requests    int64
}

// APIKeyUsagePoint is one time bucket's traffic, split by outcome. The split
// is the difference between "my automation is running" and "my automation is
// running and failing", which is the question a token's owner actually has.
type APIKeyUsagePoint struct {
	Bucket      string `json:"bucket"` // YYYY-MM-DD, the first day of the bucket
	Requests    int64  `json:"requests"`
	Success     int64  `json:"success"`
	ClientError int64  `json:"client_error"`
	ServerError int64  `json:"server_error"`
}

// APIKeyUsageTarget is what a key spends its calls on: one deployed app, or
// the platform API itself.
type APIKeyUsageTarget struct {
	AppID    string `json:"app_id"` // "" = the platform API
	AppName  string `json:"app_name,omitempty"`
	Requests int64  `json:"requests"`
}

// bucketExpr maps a caller's bucket name to a SQL truncation. Whitelisted
// rather than parameterized so the expression can appear in both the select
// list and the GROUP BY without binding the same value twice.
func bucketExpr(bucket string) (string, error) {
	switch bucket {
	case "day", "":
		return "day", nil
	case "week":
		return "date_trunc('week', day)::date", nil
	case "month":
		return "date_trunc('month', day)::date", nil
	}
	return "", fmt.Errorf("bucket must be day, week or month")
}

// BumpAPIKeyUsage upserts a batch of per-key counters. Same contract as
// BumpAppUsage: small batches, one transaction, and a failed batch is dropped
// by the caller rather than retried.
func (db *DB) BumpAPIKeyUsage(rows []APIKeyUsageBump) error {
	if len(rows) == 0 {
		return nil
	}

	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, r := range rows {
		_, err := tx.Exec(`
			INSERT INTO api_key_usage_daily (key_id, key_kind, day, app_id, status_class, requests)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (key_id, day, app_id, status_class)
			DO UPDATE SET requests = api_key_usage_daily.requests + EXCLUDED.requests`,
			r.KeyID, r.KeyKind, r.Day, r.AppID, r.StatusClass, r.Requests)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetAPIKeyUsage returns one key's traffic over the last `days` days, grouped
// into day, week or month buckets. Nothing stores a weekly or monthly figure —
// both are a SUM over the same daily rows, which is why changing the bucket
// never changes the total.
func (db *DB) GetAPIKeyUsage(keyID, bucket string, days int) ([]APIKeyUsagePoint, error) {
	expr, err := bucketExpr(bucket)
	if err != nil {
		return nil, err
	}

	rows, err := db.Conn().Query(`
		SELECT to_char(`+expr+`, 'YYYY-MM-DD'),
		       COALESCE(SUM(requests), 0),
		       COALESCE(SUM(requests) FILTER (WHERE status_class = 2), 0),
		       COALESCE(SUM(requests) FILTER (WHERE status_class = 4), 0),
		       COALESCE(SUM(requests) FILTER (WHERE status_class = 5), 0)
		FROM api_key_usage_daily
		WHERE key_id = ? AND day > CURRENT_DATE - ?::int
		GROUP BY `+expr+`
		ORDER BY `+expr, keyID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []APIKeyUsagePoint{}
	for rows.Next() {
		var p APIKeyUsagePoint
		if err := rows.Scan(&p.Bucket, &p.Requests, &p.Success, &p.ClientError, &p.ServerError); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetAPIKeyUsageTotals is the window total, so a caller never has to sum a
// series to render a headline number.
func (db *DB) GetAPIKeyUsageTotals(keyID string, days int) (*APIKeyUsagePoint, error) {
	var p APIKeyUsagePoint
	err := db.Conn().QueryRow(`
		SELECT COALESCE(SUM(requests), 0),
		       COALESCE(SUM(requests) FILTER (WHERE status_class = 2), 0),
		       COALESCE(SUM(requests) FILTER (WHERE status_class = 4), 0),
		       COALESCE(SUM(requests) FILTER (WHERE status_class = 5), 0)
		FROM api_key_usage_daily
		WHERE key_id = ? AND day > CURRENT_DATE - ?::int`, keyID, days).
		Scan(&p.Requests, &p.Success, &p.ClientError, &p.ServerError)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetAPIKeyUsageTargets breaks a key's traffic down by what it called. An app
// that has since been deleted keeps its row with an empty name, for the same
// reason usage has no foreign keys.
func (db *DB) GetAPIKeyUsageTargets(keyID string, days int) ([]APIKeyUsageTarget, error) {
	rows, err := db.Conn().Query(`
		SELECT u.app_id, COALESCE(a.name, ''), COALESCE(SUM(u.requests), 0) AS requests
		FROM api_key_usage_daily u
		LEFT JOIN apps a ON a.id = u.app_id
		WHERE u.key_id = ? AND u.day > CURRENT_DATE - ?::int
		GROUP BY u.app_id, a.name
		ORDER BY requests DESC`, keyID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []APIKeyUsageTarget{}
	for rows.Next() {
		var t APIKeyUsageTarget
		if err := rows.Scan(&t.AppID, &t.AppName, &t.Requests); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UsageKeyPrefix marks API-key rows in app_usage_daily's user_id column,
// keeping machine traffic separable from human viewers.
const UsageKeyPrefix = "key:"
