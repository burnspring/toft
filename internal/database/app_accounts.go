package database

import (
	"database/sql"
	"errors"
	"fmt"
)

// App accounts: the number behind each app's own OS account (ADR §122).
//
// **The database is the source of an account's uid, and the host's
// /etc/passwd is a cache of it** — the rule storage_gates.go states for a
// gate's gid, for the same reason. An auto-created account lives in a
// container's account database and dies with it; the files it owns do not, and
// several tasks may mount one EFS. Recreated by whichever useradd ran first, it
// came back at whatever number was free in the order apps started, and the
// numbers on the disk then belonged to other apps.

// The default account range: 1.9 million numbers, above the 16-bit ids every
// distribution and older tool treats as special, and below 2097151, the largest
// uid a plain tar header holds (runtime.AccountIDMax). Wide because a number is
// never reused: an install that creates and deletes a couple of hundred apps a
// day — the e2e suite on trytoft does — would exhaust two thousand in a week.
// One number is the uid and the per-app gid.
const (
	DefaultAccountIDLow  = 100000
	DefaultAccountIDHigh = 1999999
)

// ErrAccountRangeExhausted means every number in the range is held, retired
// accounts included.
var ErrAccountRangeExhausted = errors.New("no number left in APP_ACCOUNT_ID_RANGE for another app account")

// WithAccountIDRange sets the range account numbers are allocated from. The
// bounds are already validated; see runtime.ParseGIDRange.
func WithAccountIDRange(low, high int) Option {
	return func(db *DB) { db.accountLow, db.accountHigh = low, high }
}

func (db *DB) accountRange() (int, int) {
	if db.accountLow == 0 {
		return DefaultAccountIDLow, DefaultAccountIDHigh
	}
	return db.accountLow, db.accountHigh
}

// AppAccountNumber is the number of the account called name: its live row's,
// or a new one allocated now. The lowest free number in the range is taken
// under a table lock, so two first starts of one app — or of two apps on two
// tasks — cannot both see the same number free. A retired number is never free.
func (db *DB) AppAccountNumber(name string) (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT number FROM app_accounts WHERE name = ? AND retired_at IS NULL`, name).Scan(&n)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`LOCK TABLE app_accounts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return 0, err
	}
	// Again under the lock: the other first start may have written it.
	err = tx.QueryRow(`SELECT number FROM app_accounts WHERE name = ? AND retired_at IS NULL`, name).Scan(&n)
	if err == nil {
		return n, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	low, high := db.accountRange()
	err = tx.QueryRow(`
		SELECT n FROM generate_series(?::int, ?::int) AS n
		 WHERE NOT EXISTS (SELECT 1 FROM app_accounts a WHERE a.number = n)
		 ORDER BY n LIMIT 1`, low, high).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w (%d-%d)", ErrAccountRangeExhausted, low, high)
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO app_accounts (name, number) VALUES (?, ?)`, name, n); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// RetireAppAccount marks the account called name as belonging to no app. The
// row and its number stay: a new app on the same slug gets a number of its own,
// so it cannot inherit files the deleted one left in shared storage.
func (db *DB) RetireAppAccount(name string) error {
	_, err := db.conn.Exec(`UPDATE app_accounts SET retired_at = now() WHERE name = ? AND retired_at IS NULL`, name)
	return err
}
