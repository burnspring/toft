package database

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/advisory"
	"toft/internal/deps"
)

// The sweep's record of itself: one row per app per reading.
//
// **The observation, never the decision.** `deployments.dependency_verdict`
// stays where it is and keeps its rule — decided once at deploy, read by the
// runner at every bootstrap, never updated afterwards, because *a restart is
// not a deploy*. This table records what a reading *saw*, at the moment it was
// taken, and nothing derives one from the other. Two facts, two homes, one
// direction of reference.
//
// **Written whether or not anything changed.** The question a history answers
// is *when did we last look, and what did we see*; a table that only recorded
// changes could not tell "checked, unchanged for thirty days" from "not checked
// for thirty days". Same rule as `not recorded is not unknown` in the gallery
// and `never-measured is never zero` in the build cache.
//
// **It cannot be reconstructed later.** FindingsFor joins the stored inventory
// against a corpus that ReplaceSnapshot swaps wholesale, so an advisory that is
// withdrawn upstream takes its finding with it and no query run tomorrow can
// reproduce what the tab said today. This is the fact that makes it a table.

// The three reasons a reading is taken. `deploy` is the build's own record, so
// the history has no hole precisely where the app changed; `scheduled` is the
// sweep; `manual` is the Check again button.
//
// **The lock decision reads this**: an automatic consequence needs an automatic
// trigger, and a button is a person.
const (
	DependencyScanTriggerDeploy    = "deploy"
	DependencyScanTriggerScheduled = "scheduled"
	DependencyScanTriggerManual    = "manual"
)

// MaxScanAdvisoryIDs caps what one row carries.
//
// The ids are here so that "which advisory arrived on 3 August, and is it the
// one still open" stays answerable; they are not a second copy of the finding
// set. Forty is past the point where a person reads the list and well short of
// the row becoming somewhere to keep an inventory.
const MaxScanAdvisoryIDs = 40

// DependencyScan is one reading of one app.
type DependencyScan struct {
	ID           string    `json:"id"`
	AppID        string    `json:"app_id"`
	DeploymentID string    `json:"deployment_id,omitempty"`
	Trigger      string    `json:"trigger"`
	OccurredAt   time.Time `json:"occurred_at"`

	// Scanned is coverage in the policy's own sense — an inventory in an
	// ecosystem this build matches against. **False is unknown**, and no
	// surface may render it as "no findings".
	Scanned bool `json:"scanned"`
	// FeedAsOf is the corpus age this reading was taken against. Every number
	// here is scoped to a feed and its age.
	FeedAsOf *time.Time `json:"feed_as_of,omitempty"`

	Findings int `json:"findings"`
	// Counted is what this install said it cares about, minus waived: the one
	// number a history line renders large.
	Counted  int `json:"counted"`
	Malware  int `json:"malware"`
	Critical int `json:"critical"`
	High     int `json:"high"`
	Moderate int `json:"moderate"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
	Waived   int `json:"waived"`
	Packages int `json:"packages"`

	// Advisories is the notable ids, capped. Never the findings themselves.
	Advisories []string `json:"advisories"`
}

// SummarizeScan turns a finding set into the row that will be stored.
//
// Here rather than at each caller because the deploy, the sweep and the button
// all write one, and three counters that disagree about what "counted" means
// would make a history that cannot be read across its own rows. It is pure: no
// clock, no database.
func SummarizeScan(policy deps.Policy, findings []advisory.Finding, inventory []deps.Dependency) DependencyScan {
	scan := DependencyScan{
		Packages:   len(inventory),
		FeedAsOf:   policy.FeedAsOf,
		Advisories: []string{},
	}
	seen := map[string]bool{}
	for _, f := range findings {
		scan.Findings++
		switch {
		case f.Malware():
			scan.Malware++
		case f.Severity == advisory.SeverityCritical:
			scan.Critical++
		case f.Severity == advisory.SeverityHigh:
			scan.High++
		case f.Severity == advisory.SeverityModerate:
			scan.Moderate++
		case f.Severity == advisory.SeverityLow:
			scan.Low++
		default:
			scan.Unknown++
		}
		if f.Waived {
			scan.Waived++
			continue
		}
		if !policy.Counts(f) {
			continue
		}
		scan.Counted++
		if !seen[f.AdvisoryID] && len(scan.Advisories) < MaxScanAdvisoryIDs {
			seen[f.AdvisoryID] = true
			scan.Advisories = append(scan.Advisories, f.AdvisoryID)
		}
	}
	return scan
}

// RecordDependencyScan writes one reading and prunes that app's history to the
// newest `kept` rows, in one transaction.
//
// **Pruned inline rather than by a daily sweep.** Retention here is a *count*
// per app, so it is bounded by construction: it needs no second timer and it
// cannot drift. `app_event_retention_days` is deliberately not reused — an
// event is a problem that opened and closed, a scan is an observation, and one
// dial for both would make "keep six months of scans" depend on how often the
// install sweeps.
func (db *DB) RecordDependencyScan(scan *DependencyScan, kept int) error {
	if scan.ID == "" {
		scan.ID = uuid.New().String()
	}
	if scan.OccurredAt.IsZero() {
		scan.OccurredAt = time.Now().UTC()
	}
	if scan.Advisories == nil {
		scan.Advisories = []string{}
	}
	ids, err := json.Marshal(scan.Advisories)
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO dependency_scans (
			id, app_id, deployment_id, trigger, occurred_at, scanned, feed_as_of,
			findings, counted, malware, critical, high, moderate, low, unknown,
			waived, packages, advisories)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		scan.ID, scan.AppID, nullIfEmpty(scan.DeploymentID), scan.Trigger,
		scan.OccurredAt, scan.Scanned, scan.FeedAsOf,
		scan.Findings, scan.Counted, scan.Malware, scan.Critical, scan.High,
		scan.Moderate, scan.Low, scan.Unknown, scan.Waived, scan.Packages,
		string(ids)); err != nil {
		return err
	}

	if kept > 0 {
		// **Newest kept, oldest dropped.** An ORDER BY the wrong way round
		// here keeps the *oldest* thirty readings forever and looks entirely
		// normal on every screen: a full table, plausible dates, and every
		// answer silently three months stale. It has its own test.
		if _, err := tx.Exec(`
			DELETE FROM dependency_scans
			WHERE app_id = ? AND id NOT IN (
				SELECT id FROM dependency_scans
				WHERE app_id = ?
				ORDER BY occurred_at DESC, id DESC
				LIMIT ?)`, scan.AppID, scan.AppID, kept); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDependencyScans is one app's history, newest first.
func (db *DB) ListDependencyScans(appID string, limit int) ([]DependencyScan, error) {
	if limit <= 0 {
		limit = MaxDependencyScanHistoryKept
	}
	rows, err := db.conn.Query(`
		SELECT id, app_id, COALESCE(deployment_id, ''), trigger, occurred_at,
		       scanned, feed_as_of, findings, counted, malware, critical, high,
		       moderate, low, unknown, waived, packages, advisories
		FROM dependency_scans
		WHERE app_id = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DependencyScan{}
	for rows.Next() {
		scan, err := scanDependencyScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, scan)
	}
	return out, rows.Err()
}

// LatestDependencyScan is the most recent reading, or nil when this app has
// never been swept.
//
// Nil rather than a zero row, because *never checked* and *checked and found
// nothing* are different answers and the tab says which.
func (db *DB) LatestDependencyScan(appID string) (*DependencyScan, error) {
	rows, err := db.conn.Query(`
		SELECT id, app_id, COALESCE(deployment_id, ''), trigger, occurred_at,
		       scanned, feed_as_of, findings, counted, malware, critical, high,
		       moderate, low, unknown, waived, packages, advisories
		FROM dependency_scans
		WHERE app_id = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT 1`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	scan, err := scanDependencyScan(rows)
	if err != nil {
		return nil, err
	}
	return &scan, nil
}

// CountDependencyScans is how many readings this app is holding, for the
// history panel's own footer.
func (db *DB) CountDependencyScans(appID string) (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM dependency_scans WHERE app_id = ?`, appID).Scan(&n)
	return n, err
}

type scanRow interface{ Scan(dest ...any) error }

func scanDependencyScan(row scanRow) (DependencyScan, error) {
	var s DependencyScan
	var ids string
	if err := row.Scan(&s.ID, &s.AppID, &s.DeploymentID, &s.Trigger, &s.OccurredAt,
		&s.Scanned, &s.FeedAsOf, &s.Findings, &s.Counted, &s.Malware, &s.Critical,
		&s.High, &s.Moderate, &s.Low, &s.Unknown, &s.Waived, &s.Packages, &ids); err != nil {
		return s, err
	}
	s.Advisories = []string{}
	if strings.TrimSpace(ids) != "" {
		// A document that will not parse is an empty list rather than an
		// error: the counts beside it are the answer to every question a
		// history is asked, and refusing to read the row would lose them too.
		_ = json.Unmarshal([]byte(ids), &s.Advisories)
		if s.Advisories == nil {
			s.Advisories = []string{}
		}
	}
	return s, nil
}
