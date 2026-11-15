package database

import (
	"database/sql"
	"time"
)

// UserAPIKey is a personal access token: a credential that authenticates its
// owner to the platform API and to the apps gateway.
//
// It carries no claims. Role, groups and grants are resolved from the database
// on every request, which is what makes a demotion, a revoked grant or a lock
// take effect on the next call rather than whenever a token would have expired.
// users.token_version does not revoke these — that counter exists to invalidate
// JWTs holding stale claims, and this credential holds none.
type UserAPIKey struct {
	ID string `json:"id"`
	// UserID is a real foreign key with ON DELETE CASCADE: a personal key *is*
	// the person, and must not outlive them.
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	KeyHash   string `json:"-"`
	KeyPrefix string `json:"key_prefix"`
	// Scope is `read` or `full` — which routes this key may call, and never
	// which objects it may reach. Set at mint and never updated.
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`

	// Requests is traffic over the reporting window, from api_key_usage_daily.
	// List-only.
	Requests int64 `json:"requests"`
	// OwnerLogin is filled only by the install-wide admin listing, where the
	// question is whose key this is.
	OwnerLogin string `json:"owner_login,omitempty"`
}

// The two scopes a personal key can be minted with, and what separates them is
// the method rather than the object.
//
// A `read` key reaches every GET on the allow-list and is refused every other
// method on it; a `full` key is the allow-list alone, which is what every key
// minted before this existed still has. Neither narrows *which* objects the
// owner reaches — a scope is not a role, and Authorize never reads it.
//
// Two values and not three: the write-shaped middle is already a credential
// class of its own (ADR §51), and a `deploy` scope here would answer that
// question a second time with a word that made the wrong credential look
// sanctioned. The peers are DeployKeyScopeRead/Deploy in deploy_keys.go; the
// shared word `read` is deliberate, because one vocabulary across both classes
// is worth more than a spelling that is marginally more precise in one of them.
const (
	UserKeyScopeRead = "read"
	UserKeyScopeFull = "full"
)

// ValidUserKeyScope reports whether a scope names one of the two. One
// definition, because the API refuses on it and the database constrains on it
// and a third spelling is how those two come to disagree.
func ValidUserKeyScope(s string) bool {
	return s == UserKeyScopeRead || s == UserKeyScopeFull
}

// Key states. Computed rather than stored: expiry is a comparison against now,
// and a stored state would need a sweeper to stay true.
const (
	KeyStateActive   = "active"
	KeyStateDisabled = "disabled"
	KeyStateExpired  = "expired"
)

// State reports how this key would be treated if presented right now.
func (k *UserAPIKey) State(now time.Time) string {
	switch {
	case k.DisabledAt != nil:
		return KeyStateDisabled
	case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
		return KeyStateExpired
	}
	return KeyStateActive
}

// MarshalState is what the DTO carries, so the UI never re-derives the rule.
func (k *UserAPIKey) MarshalState() string { return k.State(time.Now().UTC()) }

const userKeyColumns = `id, user_id, name, key_prefix, scope, created_at, expires_at,
	disabled_at, last_used_at, COALESCE(last_used_ip, '')`

func scanUserKey(row interface{ Scan(...any) error }) (*UserAPIKey, error) {
	var k UserAPIKey
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyPrefix, &k.Scope, &k.CreatedAt,
		&k.ExpiresAt, &k.DisabledAt, &k.LastUsedAt, &k.LastUsedIP)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// CreateUserAPIKeyTx mints a key and writes its audit entry atomically:
// minting a credential is an access grant and must never happen unrecorded.
func (db *DB) CreateUserAPIKeyTx(k *UserAPIKey, audit AuditEntry) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO user_api_keys (id, user_id, name, key_hash, key_prefix, scope, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.UserID, k.Name, k.KeyHash, k.KeyPrefix, k.Scope, k.CreatedAt, k.ExpiresAt)
	if err != nil {
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// ListUserAPIKeys returns one user's keys, newest first, with their traffic
// over the last `days` days.
func (db *DB) ListUserAPIKeys(userID string, days int) ([]UserAPIKey, error) {
	rows, err := db.Conn().Query(`
		SELECT k.id, k.user_id, k.name, k.key_prefix, k.scope, k.created_at, k.expires_at,
		       k.disabled_at, k.last_used_at, COALESCE(k.last_used_ip, ''),
		       COALESCE(u.requests, 0)
		FROM user_api_keys k
		LEFT JOIN (
			SELECT key_id, SUM(requests) AS requests
			FROM api_key_usage_daily
			WHERE key_kind = 'user' AND day > CURRENT_DATE - ?::int
			GROUP BY key_id
		) u ON u.key_id = k.id
		WHERE k.user_id = ?
		ORDER BY k.created_at DESC`, days, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserAPIKey{}
	for rows.Next() {
		var k UserAPIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyPrefix, &k.Scope, &k.CreatedAt,
			&k.ExpiresAt, &k.DisabledAt, &k.LastUsedAt, &k.LastUsedIP, &k.Requests); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListAllUserAPIKeys is the install-wide inventory an administrator reads to
// find and kill a leaked credential. Ordered by most recently used, because
// during an incident the live ones are the question.
func (db *DB) ListAllUserAPIKeys(days int) ([]UserAPIKey, error) {
	rows, err := db.Conn().Query(`
		SELECT k.id, k.user_id, k.name, k.key_prefix, k.scope, k.created_at, k.expires_at,
		       k.disabled_at, k.last_used_at, COALESCE(k.last_used_ip, ''),
		       COALESCE(u.requests, 0), COALESCE(usr.login, '')
		FROM user_api_keys k
		LEFT JOIN users usr ON usr.id = k.user_id
		LEFT JOIN (
			SELECT key_id, SUM(requests) AS requests
			FROM api_key_usage_daily
			WHERE key_kind = 'user' AND day > CURRENT_DATE - ?::int
			GROUP BY key_id
		) u ON u.key_id = k.id
		ORDER BY k.last_used_at DESC NULLS LAST, k.created_at DESC`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UserAPIKey{}
	for rows.Next() {
		var k UserAPIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyPrefix, &k.Scope, &k.CreatedAt,
			&k.ExpiresAt, &k.DisabledAt, &k.LastUsedAt, &k.LastUsedIP,
			&k.Requests, &k.OwnerLogin); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetUserAPIKey is scoped by owner (IDOR convention): a key id from a URL is
// never trusted to name a key the caller may touch.
func (db *DB) GetUserAPIKey(userID, keyID string) (*UserAPIKey, error) {
	k, err := scanUserKey(db.Conn().QueryRow(
		`SELECT `+userKeyColumns+` FROM user_api_keys WHERE id = ? AND user_id = ?`, keyID, userID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// GetAnyUserAPIKey resolves a key without an owner scope. Only the admin
// routes may call it — they are the ones acting on someone else's credential.
func (db *DB) GetAnyUserAPIKey(keyID string) (*UserAPIKey, error) {
	k, err := scanUserKey(db.Conn().QueryRow(
		`SELECT `+userKeyColumns+` FROM user_api_keys WHERE id = ?`, keyID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// APIKeyPrincipal is everything the authentication path needs about a
// presented key and its owner, in one indexed read. The owner's live role
// comes back with it precisely because the key stores no claims of its own.
type APIKeyPrincipal struct {
	KeyID   string
	KeyName string
	// KeyScope is `read` or `full`, read in the same query as everything else
	// here. It rides on the principal and subtracts routes; it grants nothing,
	// which is the only reason a credential that carries no claims may carry
	// this one word.
	KeyScope   string
	UserID     string
	SystemRole string
	Locked     bool
	// Pending is an account still awaiting an administrator's activation,
	// which no credential may act as (docs/invariants.md § Identity).
	Pending    bool
	DisabledAt *time.Time
	ExpiresAt  *time.Time
	// Enabled is the install-wide capability switch, read in the same query
	// rather than a second one. It rides along here so that turning personal
	// keys off takes effect on the very next request — an administrator
	// flipping that switch during an incident means "now", and a cached copy
	// would mean "within the TTL".
	Enabled bool
}

// ResolveUserAPIKey looks a presented secret's hash up together with its
// owner's current role and lock state. It returns the row even when the key is
// disabled, expired or the owner is locked: the caller decides, and it decides
// with a distinguishable reason, because a caller who presented a matching
// hash already holds the key and telling them why leaks nothing.
func (db *DB) ResolveUserAPIKey(hash string) (*APIKeyPrincipal, error) {
	var p APIKeyPrincipal
	var lockedAt sql.NullTime
	var enabled string
	err := db.Conn().QueryRow(`
		SELECT k.id, k.name, k.scope, k.user_id, u.system_role, u.locked_at, u.pending_activation_at IS NOT NULL,
		       k.disabled_at, k.expires_at,
		       COALESCE((SELECT value FROM settings WHERE key = ?), 'false')
		FROM user_api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ?`, SettingAPIKeysEnabled, hash).
		Scan(&p.KeyID, &p.KeyName, &p.KeyScope, &p.UserID, &p.SystemRole, &lockedAt, &p.Pending,
			&p.DisabledAt, &p.ExpiresAt, &enabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Locked = lockedAt.Valid
	p.Enabled = enabled == "true"
	return &p, nil
}

// UpdateUserAPIKeyTx applies a rename, a disable or an enable, and audits it.
// Nil fields are left alone; Disabled is a tri-state pointer so that "enable"
// and "do not touch" are different requests.
func (db *DB) UpdateUserAPIKeyTx(keyID string, name *string, disabled *bool, audit AuditEntry) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if name != nil {
		if _, err := tx.Exec(`UPDATE user_api_keys SET name = ? WHERE id = ?`, *name, keyID); err != nil {
			return err
		}
	}
	if disabled != nil {
		var at *time.Time
		if *disabled {
			now := time.Now().UTC()
			at = &now
		}
		if _, err := tx.Exec(`UPDATE user_api_keys SET disabled_at = ? WHERE id = ?`, at, keyID); err != nil {
			return err
		}
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteUserAPIKeyTx revokes a key and records the revocation atomically. The
// key's usage rows are deliberately left behind — there is no FK, and traffic
// that happened is still a fact.
func (db *DB) DeleteUserAPIKeyTx(keyID string, audit AuditEntry) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM user_api_keys WHERE id = ?`, keyID); err != nil {
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) CountUserAPIKeys(userID string) (int, error) {
	var n int
	err := db.Conn().QueryRow(`SELECT COUNT(*) FROM user_api_keys WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// TouchUserAPIKey records last use. Callers throttle this to once a minute per
// key — it is the only database write the key authentication path adds, and it
// stays bounded for the same reason the usage counters are buffered.
func (db *DB) TouchUserAPIKey(id string, t time.Time, ip string) error {
	_, err := db.Conn().Exec(
		`UPDATE user_api_keys SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, t, ip, id)
	return err
}
