package database

import (
	"database/sql"
	"strings"
	"time"

	"toft/internal/advisory"
	"toft/internal/textutil"
)

// The advisory corpus: a third party's document about everybody, stored so
// that this install's package list never has to leave.
//
// This file implements advisory.Store. The interface is there rather than a
// direct *DB because `database` imports `advisory` for its types, so an import
// the other way would close a cycle — the same seam events.Recorder uses, for
// a reason that is load-bearing here rather than stylistic.

// insertBatch is how many rows go in one statement. Postgres caps a statement
// at 65535 parameters, and the widest insert below is nine columns, so 500
// rows is comfortably inside it while still turning ~40,000 advisories into
// eighty round trips rather than forty thousand.
const insertBatch = 500

// ReplaceSnapshot swaps one ecosystem's corpus in a single transaction.
//
// The swap is what makes a failed sync harmless: nothing is deleted until the
// replacement is in hand, and the whole exchange either happens or does not.
// A corpus that is empty and one that is unreachable must stay tellable apart,
// and the way to guarantee that is never to be briefly in the first state
// while trying to reach the second.
//
// An empty snapshotID means "unchanged" — a 304 — and only the state row
// moves. The corpus is not rewritten, and, deliberately, neither is its age.
func (db *DB) ReplaceSnapshot(ecosystem, snapshotID string, records []advisory.Record,
	state advisory.FeedState) error {

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if snapshotID != "" {
		// Every range for this ecosystem goes, and the new set replaces it.
		// Deleting by ecosystem rather than by advisory is what handles an
		// advisory that covers npm *and* PyPI: its npm ranges are this
		// snapshot's business and its PyPI ranges are not.
		if _, err := tx.Exec(`DELETE FROM advisory_affected WHERE ecosystem=?`, ecosystem); err != nil {
			return err
		}
		if err := insertAdvisories(tx, snapshotID, records); err != nil {
			return err
		}
		if err := insertAffected(tx, ecosystem, records); err != nil {
			return err
		}
		// An advisory with no ranges left describes nothing this install
		// stores. Deleting by orphan rather than by snapshot is again the
		// multi-ecosystem case: an advisory whose npm half was just replaced
		// still has its PyPI half, and must survive.
		if _, err := tx.Exec(`
			DELETE FROM advisories
			WHERE NOT EXISTS (SELECT 1 FROM advisory_affected WHERE advisory_id = advisories.id)`); err != nil {
			return err
		}
	}

	if err := writeFeedState(tx, state, snapshotID != ""); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAdvisories(tx *Tx, snapshotID string, records []advisory.Record) error {
	for start := 0; start < len(records); start += insertBatch {
		end := start + insertBatch
		if end > len(records) {
			end = len(records)
		}
		chunk := records[start:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO advisories
			(id, source, kind, severity, cvss_vector, summary, url, published_at, withdrawn_at, snapshot_id)
			VALUES `)
		args := make([]any, 0, len(chunk)*10)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, r.ID, r.Source, r.Kind, r.Severity, r.CVSSVector,
				clip(r.Summary, 2000), r.URL, r.PublishedAt, r.WithdrawnAt, snapshotID)
		}
		// An advisory already present from another ecosystem's snapshot is
		// updated rather than rejected: the record is the same document, and
		// whichever sync saw it most recently has the freshest copy.
		sb.WriteString(` ON CONFLICT (id) DO UPDATE SET
			source=excluded.source, kind=excluded.kind, severity=excluded.severity,
			cvss_vector=excluded.cvss_vector, summary=excluded.summary, url=excluded.url,
			published_at=excluded.published_at, withdrawn_at=excluded.withdrawn_at,
			snapshot_id=excluded.snapshot_id`)

		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func insertAffected(tx *Tx, ecosystem string, records []advisory.Record) error {
	type row struct {
		advisoryID string
		a          advisory.Affected
	}
	var rows []row
	for _, r := range records {
		for _, a := range r.Affected {
			if a.Ecosystem != ecosystem {
				// A record fetched from the npm snapshot may name a PyPI
				// package too. That half belongs to the PyPI snapshot, and
				// writing it here would have this sync delete it next time.
				continue
			}
			rows = append(rows, row{r.ID, a})
		}
	}

	for start := 0; start < len(rows); start += insertBatch {
		end := start + insertBatch
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO advisory_affected
			(advisory_id, ecosystem, package, introduced, fixed, last_affected, versions) VALUES `)
		args := make([]any, 0, len(chunk)*7)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?)")
			args = append(args, r.advisoryID, r.a.Ecosystem, r.a.Package,
				r.a.Introduced, r.a.Fixed, r.a.LastAffected, strings.Join(r.a.Versions, ","))
		}
		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// writeFeedState records what happened. On an unchanged fetch the snapshot's
// own age is deliberately left alone — an archive served unmodified for a
// month is a month old, and touching it here is exactly how a stale corpus
// would come to look fresh.
func writeFeedState(tx *Tx, s advisory.FeedState, corpusChanged bool) error {
	if corpusChanged {
		_, err := tx.Exec(`
			INSERT INTO advisory_feed_state
				(ecosystem, endpoint, last_sync_at, last_success_at, last_error,
				 snapshot_id, etag, advisory_count, origin, snapshot_at)
			VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?)
			ON CONFLICT (ecosystem) DO UPDATE SET
				endpoint=excluded.endpoint, last_sync_at=excluded.last_sync_at,
				last_success_at=excluded.last_success_at, last_error='',
				snapshot_id=excluded.snapshot_id, etag=excluded.etag,
				advisory_count=excluded.advisory_count, origin=excluded.origin,
				snapshot_at=excluded.snapshot_at`,
			s.Ecosystem, s.Endpoint, s.LastSyncAt, s.LastSuccessAt,
			s.SnapshotID, s.ETag, s.AdvisoryCount, s.Origin, s.SnapshotAt)
		return err
	}

	_, err := tx.Exec(`
		INSERT INTO advisory_feed_state (ecosystem, endpoint, last_sync_at, last_success_at, etag, origin)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (ecosystem) DO UPDATE SET
			endpoint=excluded.endpoint, last_sync_at=excluded.last_sync_at,
			last_success_at=excluded.last_success_at, last_error='', etag=excluded.etag`,
		s.Ecosystem, s.Endpoint, s.LastSyncAt, s.LastSuccessAt, s.ETag, s.Origin)
	return err
}

// RecordSyncFailure notes that a refresh did not work, without touching the
// corpus. The last good one keeps serving with its age beside it.
func (db *DB) RecordSyncFailure(ecosystem, endpoint, message string) error {
	now := time.Now().UTC()
	_, err := db.conn.Exec(`
		INSERT INTO advisory_feed_state (ecosystem, endpoint, last_sync_at, last_error)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (ecosystem) DO UPDATE SET
			endpoint=excluded.endpoint, last_sync_at=excluded.last_sync_at,
			last_error=excluded.last_error`,
		ecosystem, endpoint, now, clip(message, 1000))
	return err
}

// FeedStates is every ecosystem's corpus as the platform last left it.
func (db *DB) FeedStates() ([]advisory.FeedState, error) {
	rows, err := db.conn.Query(`
		SELECT ecosystem, endpoint, last_sync_at, last_success_at, last_error,
		       snapshot_id, etag, advisory_count, origin, snapshot_at
		FROM advisory_feed_state ORDER BY ecosystem`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []advisory.FeedState{}
	for rows.Next() {
		var s advisory.FeedState
		if err := rows.Scan(&s.Ecosystem, &s.Endpoint, &s.LastSyncAt, &s.LastSuccessAt,
			&s.LastError, &s.SnapshotID, &s.ETag, &s.AdvisoryCount, &s.Origin,
			&s.SnapshotAt); err != nil {
			return nil, err
		}
		s.Covered = advisory.Covered(s.Ecosystem)
		out = append(out, s)
	}
	return out, rows.Err()
}

// OldestFeedAge is the age of the *oldest* corpus this install keeps, which is
// the one number a staleness banner may honestly show.
//
// The oldest rather than the newest, deliberately: a fresh npm corpus does not
// make a six-month-old PyPI one current, and a figure that said otherwise
// would be the wrong kind of reassuring. Nil means nothing has ever loaded —
// which is not "fresh" either, and every surface distinguishes the two.
//
// **Over the covered ecosystems, not every synced one.** This is the same
// sentence as advisory.Covered, applied to age instead of to silence. The
// number exists to say how far a *verdict* can be trusted, and only a covered
// ecosystem produces a verdict — an uncovered one contributes findings it
// holds and never an absence of them, so its age cannot make a verdict older
// than it is. Without the filter, CRAN decides this figure on every install
// that has ever synced: OSV's CRAN archive is republished when a volunteer
// files an R advisory, which is roughly twice a year, so its Last-Modified is
// months behind npm's by design rather than by failure. That would pin
// Policy.Stale to true forever, put a permanent warn on the checks page, and
// leave the install with no way to distinguish it from a sync that has
// actually stopped — which is the reading the whole staleness rule exists to
// give. CRAN's own age is not hidden: FeedStates carries it and the admin
// Dependencies page prints it per ecosystem, where it is a fact about a corpus
// rather than a verdict about this install.
func (db *DB) OldestFeedAge() (*time.Time, error) {
	states, err := db.FeedStates()
	if err != nil {
		return nil, err
	}
	var oldest *time.Time
	for i := range states {
		at := states[i].SnapshotAt
		if at == nil || !advisory.Covered(states[i].Ecosystem) {
			continue
		}
		if oldest == nil || at.Before(*oldest) {
			oldest = at
		}
	}
	return oldest, nil
}

// LoadedEcosystems is every covered ecosystem whose corpus has loaded at least
// once — an advisory_feed_state row with snapshot_at set. An ecosystem with no
// row, or a row whose syncs have only ever failed, is absent: checked against
// nothing (security review 2026-11-14, DG-5).
func (db *DB) LoadedEcosystems() (map[string]bool, error) {
	states, err := db.FeedStates()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range states {
		if states[i].SnapshotAt != nil && advisory.Covered(states[i].Ecosystem) {
			out[states[i].Ecosystem] = true
		}
	}
	return out, nil
}

// CountAdvisories is how many records the corpus holds, for the admin page.
func (db *DB) CountAdvisories() (total, malware int, err error) {
	err = db.conn.QueryRow(`
		SELECT COUNT(*), COUNT(*) FILTER (WHERE kind=?) FROM advisories WHERE withdrawn_at IS NULL`,
		advisory.KindMalware).Scan(&total, &malware)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	return total, malware, err
}

// clip bounds a string that came from a document this platform did not write.
// A summary is displayed and an error is logged; neither is a reason to store a
// megabyte somebody else chose the size of.
//
// Distinct from the package's existing truncate, and the difference is the
// ellipsis: these strings are read by a person, and a sentence that stops
// mid-word with no mark is indistinguishable from one the publisher wrote that
// way. Truncation is disclosed here for the same reason it is everywhere else
// in this product.
func clip(s string, n int) string {
	s = textutil.Clean(strings.TrimSpace(s))
	if len(s) <= n {
		return s
	}
	return textutil.Clip(s, n) + "…"
}
