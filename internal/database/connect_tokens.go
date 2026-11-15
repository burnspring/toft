package database

import (
	"database/sql"
	"time"
)

// connectTokenPendingTTL is how long a registered-but-never-approved token is
// kept. Its device code dies after ten minutes, so anything older is a token
// nobody can approve any more; the day is slack, not a policy.
const connectTokenPendingTTL = 24 * time.Hour

// ConnectToken is one rsconnect request-signing token (ADR §102): the public
// key every signed request is verified against, and either the approval it is
// waiting on or the personal key it became.
type ConnectToken struct {
	TokenID        string
	PublicKey      []byte
	DeviceCodeHash string
	// KeyHash is the claimed personal key's hash — the same value a presented
	// toftu_ secret hashes to, so ResolveUserAPIKey answers for both and every
	// check on a key is made once. Empty while pending.
	KeyHash string
}

// CreateConnectToken registers a token and the device code a person will
// approve it through, in one transaction, and sweeps tokens nobody approved.
func (db *DB) CreateConnectToken(t *ConnectToken, d *DeviceCode, codeHash string) error {
	tx, err := db.Conn().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if _, err := tx.Exec(`DELETE FROM connect_tokens WHERE user_api_key_id IS NULL AND created_at < ?`,
		now.Add(-connectTokenPendingTTL)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cli_device_codes WHERE expires_at < ?`, now); err != nil {
		return err
	}
	if err := pendingCodesFull(tx, d.RequestedFrom, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO cli_device_codes
			(device_code_hash, user_code, hostname, client_version, client, requested_from, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		codeHash, d.UserCode, d.Hostname, d.ClientVersion, clientOrLP(d.Client), d.RequestedFrom,
		d.CreatedAt, d.ExpiresAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO connect_tokens (token_id, public_key, device_code_hash, created_at)
		VALUES (?, ?, ?, ?)`, t.TokenID, t.PublicKey, codeHash, now); err != nil {
		return err
	}
	return tx.Commit()
}

// GetConnectToken reads a token by the id rsconnect sends in X-Auth-Token, or
// nil.
func (db *DB) GetConnectToken(tokenID string) (*ConnectToken, error) {
	var t ConnectToken
	var codeHash, keyHash sql.NullString
	err := db.Conn().QueryRow(`
		SELECT c.token_id, c.public_key, c.device_code_hash, k.key_hash
		  FROM connect_tokens c
		  LEFT JOIN user_api_keys k ON k.id = c.user_api_key_id
		 WHERE c.token_id = ?`, tokenID).Scan(&t.TokenID, &t.PublicKey, &codeHash, &keyHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.DeviceCodeHash = codeHash.String
	t.KeyHash = keyHash.String
	return &t, nil
}

// ClaimConnectToken turns an approved token into a personal key: it consumes
// the device code, creates the key, binds the token to it and audits the
// creation, all or nothing. It answers false when the approval is no longer
// there to claim — taken by a concurrent request, or expired.
func (db *DB) ClaimConnectToken(tokenID, codeHash string, k *UserAPIKey, audit AuditEntry) (bool, error) {
	tx, err := db.Conn().Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		DELETE FROM cli_device_codes
		 WHERE device_code_hash = ? AND approved_at IS NOT NULL AND expires_at > ?`,
		codeHash, time.Now().UTC())
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	// Full scope, by the column's default, for `toft login`'s reason: this is a
	// publishing client, and its verbs would 403 under `read`.
	if _, err := tx.Exec(`
		INSERT INTO user_api_keys (id, user_id, name, key_hash, key_prefix, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.UserID, k.Name, k.KeyHash, k.KeyPrefix, k.CreatedAt, k.ExpiresAt); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`
		UPDATE connect_tokens SET user_api_key_id = ?, device_code_hash = NULL
		 WHERE token_id = ? AND device_code_hash = ?`, k.ID, tokenID, codeHash); err != nil {
		return false, err
	}
	if err := WriteAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DeleteConnectToken drops a token whose approval was refused or ran out, with
// its device code, so it stops costing a lookup on every poll.
func (db *DB) DeleteConnectToken(tokenID, codeHash string) error {
	if _, err := db.Conn().Exec(`DELETE FROM cli_device_codes WHERE device_code_hash = ?`, codeHash); err != nil {
		return err
	}
	_, err := db.Conn().Exec(`DELETE FROM connect_tokens WHERE token_id = ? AND user_api_key_id IS NULL`, tokenID)
	return err
}
