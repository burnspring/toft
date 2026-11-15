package database

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// A trusted issuer and the short-lived key its word buys (ADR §157).
//
// An administrator names an OIDC issuer and the audience its identity tokens
// must carry. A caller holding such a token — Posit Workbench's rsconnect,
// Garth at session start — exchanges it for an `toftx_` key for the
// person the token names. The key is that person's, like a personal key, and
// differs from one in exactly the two ways that make it safe to hand a sandbox:
// it expires on its own, within a day, and it reaches the deploy surface and
// nothing else.

// The two ways a token can name its person.
const (
	// SubjectClaimSub looks the token's `sub` up as an identity on the
	// issuer's sign-in provider — what sign-in itself does.
	SubjectClaimSub = "sub"
	// SubjectClaimEmail matches the token's verified `email` against exactly
	// one account, for an issuer whose `sub` is per client (Entra's is).
	SubjectClaimEmail = "email"
)

// The bounds on what an administrator may set an issuer's key lifetime to.
// An hour at least, because rsconnect caches the key until the identity token
// it came from expires, and an hour is a typical token; a day at most, because
// past that it is no longer a short-lived key.
const (
	ExchangedKeyMinMinutes     = 60
	ExchangedKeyMaxMinutes     = 1440
	ExchangedKeyDefaultMinutes = 480
)

// ExchangedKeyScope is the word the principal carries for a `toftx_` key.
const ExchangedKeyScope = "deploy"

// TrustedIssuer is one row of trusted_issuers.
type TrustedIssuer struct {
	ID                 string    `json:"id"`
	DisplayName        string    `json:"display_name"`
	Issuer             string    `json:"issuer"`
	Audience           string    `json:"audience"`
	SubjectClaim       string    `json:"subject_claim"`
	ProviderID         string    `json:"provider_id"`
	KeyLifetimeMinutes int       `json:"key_lifetime_minutes"`
	Enabled            bool      `json:"enabled"`
	CreatedBy          string    `json:"created_by,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`

	// ActiveKeys is how many unexpired, unrevoked keys it has vouched for.
	// List-only.
	ActiveKeys int `json:"active_keys"`
}

const trustedIssuerColumns = `id, display_name, issuer, audience, subject_claim, provider_id,
	key_lifetime_minutes, enabled, COALESCE(created_by, ''), created_at, updated_at`

func scanTrustedIssuer(row interface{ Scan(...any) error }, extra ...any) (*TrustedIssuer, error) {
	var t TrustedIssuer
	dest := []any{&t.ID, &t.DisplayName, &t.Issuer, &t.Audience, &t.SubjectClaim, &t.ProviderID,
		&t.KeyLifetimeMinutes, &t.Enabled, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	return &t, nil
}

// ErrTrustedIssuerExists is a second row for the same issuer and audience.
var ErrTrustedIssuerExists = errors.New("that issuer is already trusted for that audience")

// ListTrustedIssuers returns every issuer, with its count of live keys.
func (db *DB) ListTrustedIssuers() ([]TrustedIssuer, error) {
	rows, err := db.Conn().Query(`SELECT ` + trustedIssuerColumns + `,
		(SELECT COUNT(*) FROM exchanged_keys k
		  WHERE k.issuer_id = trusted_issuers.id AND k.revoked_at IS NULL AND k.expires_at > now())
		FROM trusted_issuers ORDER BY display_name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrustedIssuer{}
	for rows.Next() {
		var n int
		t, err := scanTrustedIssuer(rows, &n)
		if err != nil {
			return nil, err
		}
		t.ActiveKeys = n
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetTrustedIssuer returns one issuer, or nil.
func (db *DB) GetTrustedIssuer(id string) (*TrustedIssuer, error) {
	t, err := scanTrustedIssuer(db.Conn().QueryRow(
		`SELECT `+trustedIssuerColumns+` FROM trusted_issuers WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

// EnabledTrustedIssuersFor returns the enabled rows for one issuer URL — the
// candidates a presented token is verified against, one per audience.
func (db *DB) EnabledTrustedIssuersFor(issuer string) ([]TrustedIssuer, error) {
	rows, err := db.Conn().Query(`SELECT `+trustedIssuerColumns+`
		FROM trusted_issuers WHERE enabled AND issuer = ? ORDER BY id`, strings.TrimSuffix(issuer, "/"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrustedIssuer
	for rows.Next() {
		t, err := scanTrustedIssuer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// CreateTrustedIssuerTx inserts an issuer and audits it.
func (db *DB) CreateTrustedIssuerTx(t *TrustedIssuer, audit AuditEntry) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = tx.QueryRow(`INSERT INTO trusted_issuers
		(id, display_name, issuer, audience, subject_claim, provider_id, key_lifetime_minutes, enabled, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT ON CONSTRAINT trusted_issuers_issuer_audience_key DO NOTHING
		RETURNING created_at, updated_at`,
		t.ID, t.DisplayName, t.Issuer, t.Audience, t.SubjectClaim, t.ProviderID,
		t.KeyLifetimeMinutes, t.Enabled, nullIfEmpty(t.CreatedBy)).Scan(&t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return ErrTrustedIssuerExists
	}
	if err != nil {
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateTrustedIssuerTx writes every editable field and audits it. The issuer
// URL and audience are editable too: a row is the administrator's statement of
// trust, and correcting a typo in it should not mean deleting its keys.
func (db *DB) UpdateTrustedIssuerTx(t *TrustedIssuer, audit AuditEntry) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE trusted_issuers SET display_name = ?, issuer = ?, audience = ?,
		subject_claim = ?, provider_id = ?, key_lifetime_minutes = ?, enabled = ?, updated_at = now()
		WHERE id = ?`,
		t.DisplayName, t.Issuer, t.Audience, t.SubjectClaim, t.ProviderID, t.KeyLifetimeMinutes, t.Enabled, t.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrTrustedIssuerExists
		}
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteTrustedIssuerTx removes an issuer, and with it every key it vouched
// for (the foreign key cascades), and audits it.
func (db *DB) DeleteTrustedIssuerTx(id string, audit AuditEntry) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM trusted_issuers WHERE id = ?`, id); err != nil {
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// FindUserForIssuer resolves the person a verified token names, or "" when
// nobody matches. **It never creates an account**: an exchange vouches for a
// person the install already knows, and provisioning is sign-in's.
func (db *DB) FindUserForIssuer(t *TrustedIssuer, subject string) (string, error) {
	var userID string
	var err error
	switch t.SubjectClaim {
	case SubjectClaimEmail:
		// Exactly one account, or none: an address two accounts share names
		// nobody in particular.
		//
		// **And only an address somebody verified.** users.email is written
		// from a sign-in's assertion whatever its email_verified said, so a
		// provider that lets a user claim an address unverified made every
		// exchange for that address resolve to the claimant (security review
		// 2026-11-10, A-2). An account's address counts when a non-local
		// identity verified it, or when the account has no external identity
		// at all — then the address is an administrator's word.
		var n int
		err = db.Conn().QueryRow(`SELECT COUNT(*), COALESCE(MIN(u.id), '') FROM users u
			WHERE lower(u.email) = lower(?) AND u.email <> ''
			  AND (EXISTS (SELECT 1 FROM user_identities i
			               WHERE i.user_id = u.id AND i.provider_id <> 'local'
			                 AND i.email_verified AND lower(i.email) = lower(u.email))
			       OR NOT EXISTS (SELECT 1 FROM user_identities i
			               WHERE i.user_id = u.id AND i.provider_id <> 'local'))`, subject).Scan(&n, &userID)
		if err == nil && n != 1 {
			return "", nil
		}
	default:
		err = db.Conn().QueryRow(`SELECT user_id FROM user_identities WHERE provider_id = ? AND subject = ?`,
			t.ProviderID, subject).Scan(&userID)
	}
	if err == sql.ErrNoRows {
		return "", nil
	}
	return userID, err
}

// ExchangedKey is one row of exchanged_keys, for the admin listing.
type ExchangedKey struct {
	ID           string     `json:"id"`
	UserID       string     `json:"user_id"`
	UserLogin    string     `json:"user_login"`
	IssuerID     string     `json:"issuer_id"`
	IssuerName   string     `json:"issuer_name"`
	KeyPrefix    string     `json:"key_prefix"`
	TokenSubject string     `json:"token_subject"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP   string     `json:"last_used_ip,omitempty"`
}

// CreateExchangedKeyTx stores a minted key and audits the exchange, and
// sweeps rows that expired more than a week ago — the table's only cleanup,
// done here because an exchange is the only thing that adds rows.
//
// It also holds one person's live keys from one issuer to maxLive: past it,
// the oldest are revoked rather than the exchange refused, because the caller
// is a sandbox with nobody to read a 409, and exchanging the same token again
// is legitimate (a client that lost its cached key). Without the bound one
// token minted keys without limit until its exp. The person's row is locked
// first so two exchanges at once count each other.
func (db *DB) CreateExchangedKeyTx(k *ExchangedKey, hash string, audit AuditEntry, maxLive int) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM exchanged_keys WHERE expires_at < now() - interval '7 days'`); err != nil {
		return err
	}
	if maxLive > 0 {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM users WHERE id = ? FOR UPDATE`, k.UserID).Scan(&one); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE exchanged_keys SET revoked_at = now()
			WHERE id IN (
				SELECT id FROM exchanged_keys
				WHERE user_id = ? AND issuer_id = ? AND revoked_at IS NULL AND expires_at > now()
				ORDER BY created_at DESC, id DESC
				OFFSET ?)`, k.UserID, k.IssuerID, maxLive-1); err != nil {
			return err
		}
	}
	err = tx.QueryRow(`INSERT INTO exchanged_keys
		(id, user_id, issuer_id, key_hash, key_prefix, token_subject, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING created_at`,
		k.ID, k.UserID, k.IssuerID, hash, k.KeyPrefix, k.TokenSubject, k.ExpiresAt).Scan(&k.CreatedAt)
	if err != nil {
		return err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// ListActiveExchangedKeys returns every key that is neither expired nor
// revoked, newest first.
func (db *DB) ListActiveExchangedKeys() ([]ExchangedKey, error) {
	rows, err := db.Conn().Query(`
		SELECT k.id, k.user_id, u.login, k.issuer_id, i.display_name, k.key_prefix, k.token_subject,
		       k.created_at, k.expires_at, k.revoked_at, k.last_used_at, k.last_used_ip
		FROM exchanged_keys k
		JOIN users u ON u.id = k.user_id
		JOIN trusted_issuers i ON i.id = k.issuer_id
		WHERE k.revoked_at IS NULL AND k.expires_at > now()
		ORDER BY k.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExchangedKey{}
	for rows.Next() {
		var k ExchangedKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.UserLogin, &k.IssuerID, &k.IssuerName, &k.KeyPrefix,
			&k.TokenSubject, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.LastUsedIP); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetExchangedKey returns one key, or nil.
func (db *DB) GetExchangedKey(id string) (*ExchangedKey, error) {
	var k ExchangedKey
	err := db.Conn().QueryRow(`
		SELECT k.id, k.user_id, u.login, k.issuer_id, i.display_name, k.key_prefix, k.token_subject,
		       k.created_at, k.expires_at, k.revoked_at, k.last_used_at, k.last_used_ip
		FROM exchanged_keys k
		JOIN users u ON u.id = k.user_id
		JOIN trusted_issuers i ON i.id = k.issuer_id
		WHERE k.id = ?`, id).Scan(&k.ID, &k.UserID, &k.UserLogin, &k.IssuerID, &k.IssuerName, &k.KeyPrefix,
		&k.TokenSubject, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.LastUsedIP)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// GetExchangedKeyByHash returns the key a presented secret names, or nil.
func (db *DB) GetExchangedKeyByHash(hash string) (*ExchangedKey, error) {
	var id string
	err := db.Conn().QueryRow(`SELECT id FROM exchanged_keys WHERE key_hash = ?`, hash).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return db.GetExchangedKey(id)
}

// RevokeExchangedKeyTx revokes a key and audits it. A key already revoked is
// left as it was and reported false, so the audit log holds one revocation.
func (db *DB) RevokeExchangedKeyTx(id string, audit AuditEntry) (bool, error) {
	tx, err := db.Conn().Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE exchanged_keys SET revoked_at = now() WHERE id = ? AND revoked_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ExchangedKeyPrincipal is everything authentication needs about a presented
// `toftx_` key, in one read — APIKeyPrincipal's shape, plus the issuer that
// vouched for it and whether it still does.
type ExchangedKeyPrincipal struct {
	APIKeyPrincipal
	IssuerID      string
	IssuerName    string
	IssuerEnabled bool
	RevokedAt     *time.Time
}

// ResolveExchangedKey looks a presented secret's hash up with its owner's live
// role and lock, the issuer's switch and the install's personal-key switch.
// Like ResolveUserAPIKey it returns the row whatever its state; the caller
// refuses with a reason.
func (db *DB) ResolveExchangedKey(hash string) (*ExchangedKeyPrincipal, error) {
	var p ExchangedKeyPrincipal
	var lockedAt sql.NullTime
	var expires time.Time
	var enabled string
	err := db.Conn().QueryRow(`
		SELECT k.id, k.user_id, u.system_role, u.locked_at, u.pending_activation_at IS NOT NULL, k.expires_at, k.revoked_at,
		       i.id, i.display_name, i.enabled,
		       COALESCE((SELECT value FROM settings WHERE key = ?), 'false')
		FROM exchanged_keys k
		JOIN users u ON u.id = k.user_id
		JOIN trusted_issuers i ON i.id = k.issuer_id
		WHERE k.key_hash = ?`, SettingAPIKeysEnabled, hash).
		Scan(&p.KeyID, &p.UserID, &p.SystemRole, &lockedAt, &p.Pending, &expires, &p.RevokedAt,
			&p.IssuerID, &p.IssuerName, &p.IssuerEnabled, &enabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.KeyName = "via " + p.IssuerName
	p.KeyScope = ExchangedKeyScope
	p.ExpiresAt = &expires
	p.Locked = lockedAt.Valid
	p.Enabled = enabled == "true"
	return &p, nil
}

// TouchExchangedKey records last use; callers throttle it, as for personal keys.
func (db *DB) TouchExchangedKey(id string, t time.Time, ip string) error {
	_, err := db.Conn().Exec(
		`UPDATE exchanged_keys SET last_used_at = ?, last_used_ip = ? WHERE id = ?`, t, ip, id)
	return err
}
