package database

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// UserTimeZoneBrowser is the one stored value of users.time_zone that is not a
// zone: this person reads times in their own browser's zone, whatever the
// install's default says. Empty is the other non-zone — follow the install.
const UserTimeZoneBrowser = "browser"

// NormalizeTimeZone checks a zone a person chose for display (ADR §154).
// Empty stays empty, and means "no choice" to whoever stores it. Otherwise it
// is "UTC" or an IANA name the embedded tz database knows — main imports
// time/tzdata, so the answer does not depend on the image. "Local" is refused:
// it names the server's zone, which is the one zone a hosted install must never
// show anybody.
func NormalizeTimeZone(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if strings.EqualFold(v, "utc") || strings.EqualFold(v, "etc/utc") {
		return "UTC", nil
	}
	if strings.EqualFold(v, "local") {
		return "", errors.New(`"Local" is the server's zone; name a zone such as Europe/London`)
	}
	if _, err := time.LoadLocation(v); err != nil {
		return "", errors.New("is not a time zone name; use one such as America/New_York or UTC")
	}
	return v, nil
}

// NormalizeUserTimeZone is NormalizeTimeZone plus the one value only a person
// may store: "browser".
func NormalizeUserTimeZone(raw string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(raw), UserTimeZoneBrowser) {
		return UserTimeZoneBrowser, nil
	}
	return NormalizeTimeZone(raw)
}

// GetUserTimeZone returns the person's stored choice: "" (follow the install),
// "browser", "UTC" or an IANA name. A value this binary no longer knows reads
// as "" rather than as a page that cannot format a date.
func (db *DB) GetUserTimeZone(userID string) (string, error) {
	var v string
	if err := db.conn.QueryRow(`SELECT time_zone FROM users WHERE id = ?`, userID).Scan(&v); err != nil {
		return "", err
	}
	n, err := NormalizeUserTimeZone(v)
	if err != nil {
		return "", nil
	}
	return n, nil
}

// SetUserTimeZone stores an already-normalized choice. sql.ErrNoRows when the
// user is gone.
func (db *DB) SetUserTimeZone(userID, zone string) error {
	res, err := db.conn.Exec(`UPDATE users SET time_zone = ? WHERE id = ?`, zone, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
