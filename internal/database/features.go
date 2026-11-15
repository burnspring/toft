package database

import (
	"database/sql"
	"errors"
	"time"
)

// Feature gating — gate 2 of the four.
//
// Gate 1 (licensed) has no table: it is derived from a license key by
// internal/entitlements, which is permissive until a licensing system exists.
// Gate 3 (granted on this app) is a table per capability, owned by the package
// that has one. Gate 4 (permitted right now) is computed by appint.Check
// immediately before the transport, because off has to mean now.

// Feature is an administrator's switch for one feature.
//
// Absent means never enabled, and that is a different state from
// enabled-and-then-switched-off: the first shows nothing in the UI at all, and
// the second keeps its row visible so an administrator can see why the thing
// they installed stopped answering.
type Feature struct {
	Name      string     `json:"name"`
	Enabled   bool       `json:"enabled"`
	EnabledAt *time.Time `json:"enabled_at,omitempty"`
	EnabledBy string     `json:"enabled_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// GetFeature returns a feature's row, or nil when no administrator has ever
// touched it. Nil is the common case and is not an error.
func (db *DB) GetFeature(name string) (*Feature, error) {
	return getFeature(db.conn, name)
}

func getFeature(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, name string) (*Feature, error) {
	var f Feature
	var enabledAt sql.NullTime
	err := q.QueryRow(`
		SELECT name, enabled, enabled_at, COALESCE(enabled_by,''), created_at, updated_at
		FROM features WHERE name = ?`, name).
		Scan(&f.Name, &f.Enabled, &enabledAt, &f.EnabledBy, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if enabledAt.Valid {
		f.EnabledAt = &enabledAt.Time
	}
	return &f, nil
}

// FeatureEnabled is the gate-2 read: has an administrator switched this on.
//
// A feature with no row is off. That is the whole rule, and it is stated as its
// own function so no call site has to remember that nil means false rather than
// "unset, so probably fine".
func (db *DB) FeatureEnabled(name string) (bool, error) {
	f, err := db.GetFeature(name)
	if err != nil || f == nil {
		return false, err
	}
	return f.Enabled, nil
}

// ListFeatures returns every feature row an administrator has ever touched.
//
// It deliberately does not synthesize rows for features that have never been
// enabled — the admin surface merges this with entitlements.Known, because the
// catalog of what this binary offers is a property of the binary and not of the
// database.
func (db *DB) ListFeatures() ([]Feature, error) {
	rows, err := db.conn.Query(`
		SELECT name, enabled, enabled_at, COALESCE(enabled_by,''), created_at, updated_at
		FROM features ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Feature{}
	for rows.Next() {
		var f Feature
		var enabledAt sql.NullTime
		if err := rows.Scan(&f.Name, &f.Enabled, &enabledAt, &f.EnabledBy,
			&f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		if enabledAt.Valid {
			f.EnabledAt = &enabledAt.Time
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetFeatureEnabled flips an administrator's switch and audits it in the same
// transaction.
//
// enabled_at and enabled_by record the last *enabling*, not the last edit:
// "when was this turned on, and by whom" is the question the log is read for,
// and overwriting it on the way off would erase the answer at exactly the moment
// somebody is asking.
func (db *DB) SetFeatureEnabled(name string, enabled bool, actorID string, meta RequestMeta) error {
	return db.setFeatureEnabled(nil, name, enabled, actorID, meta)
}

// setFeatureEnabled is the shared body. A nil tx opens one; a non-nil tx is
// joined, which is what lets the install action switch the feature on, create
// the app and write the grant as one thing that either all happened or did not.
func (db *DB) setFeatureEnabled(tx *Tx, name string, enabled bool, actorID string, meta RequestMeta) error {
	before, err := db.GetFeature(name)
	if err != nil {
		return err
	}

	own := tx == nil
	if own {
		tx, err = db.conn.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	now := time.Now().UTC()
	if enabled {
		if _, err := tx.Exec(`
			INSERT INTO features (name, enabled, enabled_at, enabled_by, created_at, updated_at)
			VALUES (?, true, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET
				enabled = true, enabled_at = EXCLUDED.enabled_at,
				enabled_by = EXCLUDED.enabled_by, updated_at = EXCLUDED.updated_at`,
			name, now, nullIfEmpty(actorID), now, now); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`
			INSERT INTO features (name, enabled, created_at, updated_at)
			VALUES (?, false, ?, ?)
			ON CONFLICT (name) DO UPDATE SET
				enabled = false, updated_at = EXCLUDED.updated_at`,
			name, now, now); err != nil {
			return err
		}
	}

	// Through the transaction: on the pool the write above is not yet
	// visible, and the audit row's after was the state from before it (DB2-2).
	after, err := getFeature(tx, name)
	if err != nil {
		return err
	}
	if err := WriteAuditTx(tx, AuditEntry{
		ActorType:  PrincipalTypeUser,
		ActorID:    actorID,
		Action:     "feature.enable",
		TargetType: "feature",
		TargetID:   name,
		Before:     jsonStr(before),
		After:      jsonStr(after),
		OccurredAt: now,
		RequestID:  meta.RequestID,
		IP:         meta.IP,
		UserAgent:  meta.UserAgent,
		APIKeyID:   meta.APIKeyID,
	}); err != nil {
		return err
	}

	if own {
		return tx.Commit()
	}
	return nil
}
