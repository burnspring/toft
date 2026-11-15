package database

import (
	"fmt"
	"time"
)

// The access record's retention: one sweep over audit_log and the two usage
// rollups, on access_record_retention_days and one cutoff.
//
// Here rather than in main.go, where the other sweeps are scheduled, because
// it writes an audit action (audit_log.pruned) and the guard that gives every
// action a phrase and a scope reads internal/ only.

// accessRecordBatch is how many rows one DELETE removes. Small enough that no
// statement holds its locks for long, large enough that cutting years takes
// hours rather than weeks.
var accessRecordBatch = 10000

// accessRecordClock is what the budget is measured on; a test steps it.
var accessRecordClock = time.Now

// AccessRecordPass is what one pass did.
type AccessRecordPass struct {
	// Cutoff is midnight UTC, retention days before today. Nothing older
	// remains in any of the three tables once a pass that was not Truncated
	// has run — that is the claim, and the counts are commentary on it.
	Cutoff time.Time
	// Removed is rows per table.
	Removed map[string]int64
	// Truncated is a pass that ran out of budget before it finished; the
	// scheduler runs the next one sooner.
	Truncated bool
}

// Total is every row the pass removed.
func (p AccessRecordPass) Total() int64 {
	var n int64
	for _, v := range p.Removed {
		n += v
	}
	return n
}

// AccessRecordCutoff is the instant before which a pass at this retention
// removes rows, and the earliest day the install still keeps.
func AccessRecordCutoff(retentionDays int, now time.Time) time.Time {
	today := now.UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -retentionDays)
}

// PruneAccessRecord runs one pass. At 0 it returns before touching anything:
// 0 keeps everything, and is every install's default.
//
// **The rollups first, the log last.** A pass that stops early has then cut
// the usage further than the log, which is the safe direction: a log that
// outlives its usage is harmless, and the reverse leaves rows naming somebody
// whose deletion record is gone. One cutoff for all three, so a usage row can
// never outlive the user.delete or app.delete row that explains it.
//
// A pass that removed anything writes audit_log.pruned with the cutoff, the
// setting, the counts and whether it was truncated. A crash between batches
// can remove rows no pruned row counts; the next pass's row states the cutoff,
// and "nothing before X remains" is what an assessor needs. That is stated
// rather than designed around with a row per batch.
func (db *DB) PruneAccessRecord(retentionDays int, budget time.Duration) (AccessRecordPass, error) {
	pass := AccessRecordPass{Removed: map[string]int64{}}
	if retentionDays == 0 {
		return pass, nil
	}
	pass.Cutoff = AccessRecordCutoff(retentionDays, time.Now())
	deadline := accessRecordClock().Add(budget)

	for _, t := range []struct{ table, cond string }{
		{"app_usage_daily", "day < ?::date"},
		{"api_key_usage_daily", "day < ?::date"},
		{"app_audience_daily", "day < ?::date"},
		{"audit_log", "occurred_at < ?"},
	} {
		arg := any(pass.Cutoff)
		if t.cond == "day < ?::date" {
			arg = pass.Cutoff.Format("2006-01-02")
		}
		for {
			if accessRecordClock().After(deadline) {
				pass.Truncated = true
				break
			}
			res, err := db.conn.Exec(`DELETE FROM `+t.table+` WHERE ctid IN (
				SELECT ctid FROM `+t.table+` WHERE `+t.cond+` LIMIT `+fmt.Sprint(accessRecordBatch)+`)`, arg)
			if err != nil {
				return pass, fmt.Errorf("pruning %s: %w", t.table, err)
			}
			n, _ := res.RowsAffected()
			pass.Removed[t.table] += n
			if n < int64(accessRecordBatch) {
				break
			}
		}
		if pass.Truncated {
			break
		}
	}

	if pass.Total() > 0 {
		if err := db.WriteAuditRow(AuditEntry{
			ActorType: "system", Action: ActionAuditPruned,
			TargetType: "platform", TargetID: "access_record",
			After: jsonStr(map[string]any{
				"cutoff": pass.Cutoff.Format(time.RFC3339), "retention_days": retentionDays,
				"removed": pass.Removed, "truncated": pass.Truncated,
			}),
		}); err != nil {
			return pass, fmt.Errorf("recording the pass: %w", err)
		}
	}
	return pass, nil
}
