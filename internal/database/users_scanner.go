package database

import (
	"context"
	"database/sql"
	"errors"
)

// The scanner mark.
//
// An administrator marks the account an AppSec tool signs in as. The mark does
// two things and the list is the specification: `X-Toft-Scan` is believed from
// that account at the gateway, so its scan traffic is not written to the apps'
// usage, and the account may read /admin/estate/targets. **It never grants.** No
// authorization path reads it — a marked account reaches exactly the apps its
// grants reach, and a private app nobody granted it stays a 404.

// ErrCannotMarkSelf refuses an administrator marking their own account: the
// one person who could then make their own traffic uncounted is the person
// setting the switch.
var ErrCannotMarkSelf = errors.New("an administrator cannot mark their own account as the scanner")

// SetScannerAccount sets or clears the mark, with its audit row in the same
// transaction. It reports whether anything changed: setting a mark already set,
// or clearing one already clear, is no change and writes no row.
func (db *DB) SetScannerAccount(ctx context.Context, userID string, on bool, audit AuditEntry) (bool, error) {
	if on && audit.ActorType == PrincipalTypeUser && audit.ActorID == userID {
		return false, ErrCannotMarkSelf
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var marked bool
	if err := tx.QueryRow(`SELECT scanner_since IS NOT NULL FROM users WHERE id=?`, userID).
		Scan(&marked); err != nil {
		return false, err
	}
	if marked == on {
		return false, nil
	}

	if on {
		_, err = tx.Exec(`UPDATE users SET scanner_since = NOW(), scanner_set_by = ? WHERE id = ?`,
			nullIfEmpty(audit.ActorID), userID)
		audit.Action = "user.scanner_set"
	} else {
		_, err = tx.Exec(`UPDATE users SET scanner_since = NULL, scanner_set_by = NULL WHERE id = ?`, userID)
		audit.Action = "user.scanner_cleared"
	}
	if err != nil {
		return false, err
	}
	audit.TargetType = "user"
	audit.TargetID = userID
	audit.Before = jsonStr(map[string]bool{"scanner": marked})
	audit.After = jsonStr(map[string]bool{"scanner": on})
	if err := writeAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// IsScannerAccount is whether this account carries the mark and can use it.
//
// A locked account authenticates nowhere, so its mark is inert rather than
// cleared: the lock has several writers (the button, the dormancy sweep, a
// directory's deactivation) and none of them needs to know the mark exists.
// Reading `locked_at` here is what keeps that true. Missing accounts are not
// scanners.
func (db *DB) IsScannerAccount(userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var ok bool
	err := db.conn.QueryRow(
		`SELECT scanner_since IS NOT NULL AND locked_at IS NULL FROM users WHERE id = ?`, userID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return ok, err
}
