package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Every stored ciphertext this install holds, in one place.
//
// **The list is the specification.** A new kind of secret that is
// encrypted and does not appear below is a secret the rotate command silently skips and
// the startup report silently calls readable — which is worse than not having
// either, because both would then be lying with authority. Adding an encrypted
// column or an encrypted settings key means adding it here in the same change.
//
// Nothing else is encrypted: audit_log, app_events, build logs, the app data
// document and every uploaded archive are in the clear by design (ADR §62).

// encryptedColumns are the ciphertext-bearing columns, by table.
var encryptedColumns = []struct {
	class    string
	table    string
	column   string
	idColumn string
}{
	{"app environment variable", "env_vars", "value", "id"},
	{"app git token", "apps", "git_token", "id"},
	{"auth provider client secret", "auth_providers", "client_secret", "id"},
	// A connected git host holds three, and they are three because they answer
	// to three different things: the picker's OAuth client, the credential the
	// install clones with, and the HMAC key a delivery is verified against.
	{"git connection picker secret", "git_connections", "client_secret", "id"},
	{"git connection clone credential", "git_connections", "clone_secret", "id"},
	{"git connection webhook secret", "git_connections", "webhook_secret", "id"},
	// The fourth is a *person's* token against the host, and it is a stored
	// ciphertext like the rest — which the concept's schema sketch missed,
	// because a per-user row does not look like a credential store.
	{"git connection picker token", "git_connection_identities", "access_token", "id"},
	// One row per integration connection, holding whichever of that kind's
	// credential fields the catalog declares secret, as one JSON document
	// encrypted as one record. It replaces the Slack bot token's settings row:
	// eight kinds' credentials as eight settings keys would have been eight
	// entries below, and eight chances to add the ninth and forget.
	{"integration connection credential", "integration_connections", "credential_secret", "id"},
	// An administrator's credential store: one row per field of a credential,
	// keyed (credential_id, key). The locator is that pair as one expression,
	// which the SELECT reads and the UPDATE matches on; a credential id is a
	// generated id with no ':' in it, so the pair reads back unambiguously.
	// Left off the list, rotate-key skipped every attached credential and the
	// old key's removal broke each app using one at its next start (security
	// review 2026-11-10, H-2).
	{"attached credential value", "credential_values", "value", "credential_id || ':' || key"},
}

// encryptedSettings are the ciphertext-bearing rows of the settings table. They
// are the typed settings documents' credential fields, which are stored as
// ordinary settings rows like everything else in those documents.
var encryptedSettings = []struct {
	class string
	key   string
}{
	{"SMTP password", SettingEmailPassword},
	{"mail API key", SettingEmailAPIKey},
	{"telemetry collector headers", SettingTelemetryHeaders},
	{"dependency feed credential", SettingDependencyFeedCredential},
}

// EncryptedSecret is one stored ciphertext and enough to write it back.
type EncryptedSecret struct {
	// Class names the kind of secret in words an operator reads in a report.
	Class string
	// Record is the stored value — a crypto record, not a plaintext.
	Record string

	// Where it lives. Table, column and idColumn are compile-time constants
	// from the lists above and never anything a caller supplied, which is what
	// makes interpolating them into the UPDATE safe. idColumn may be an
	// expression over the row's key columns, for a table with a composite key.
	table    string
	column   string
	idColumn string
	id       string
}

// Where locates the secret for a human, without disclosing anything about it.
func (s EncryptedSecret) Where() string {
	return s.table + "." + s.column + " " + s.id
}

// EachEncryptedSecret calls fn once for every stored ciphertext on the install.
// Empty values are skipped: an unset credential is not a record, and treating
// it as one would report an install with no Slack as having an unreadable Slack
// token.
func (db *DB) EachEncryptedSecret(fn func(EncryptedSecret) error) error {
	for _, src := range encryptedColumns {
		query := fmt.Sprintf(
			`SELECT %s, %s FROM %s WHERE %s IS NOT NULL AND %s <> ''`,
			src.idColumn, src.column, src.table, src.column, src.column)
		rows, err := db.conn.Query(query)
		if err != nil {
			return fmt.Errorf("reading %s.%s: %w", src.table, src.column, err)
		}
		var batch []EncryptedSecret
		for rows.Next() {
			var id, record string
			if err := rows.Scan(&id, &record); err != nil {
				rows.Close()
				return fmt.Errorf("reading %s.%s: %w", src.table, src.column, err)
			}
			batch = append(batch, EncryptedSecret{
				Class:    src.class,
				Record:   record,
				table:    src.table,
				column:   src.column,
				idColumn: src.idColumn,
				id:       id,
			})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("reading %s.%s: %w", src.table, src.column, err)
		}
		// Drained before fn runs: fn rewrites rows in the table this query is
		// reading, and holding the cursor open across that is how a rotation
		// deadlocks against itself.
		for _, s := range batch {
			if err := fn(s); err != nil {
				return err
			}
		}
	}

	keys := make([]string, 0, len(encryptedSettings))
	class := make(map[string]string, len(encryptedSettings))
	placeholders := make([]string, 0, len(encryptedSettings))
	args := make([]any, 0, len(encryptedSettings))
	for _, s := range encryptedSettings {
		keys = append(keys, s.key)
		class[s.key] = s.class
		placeholders = append(placeholders, "?")
		args = append(args, s.key)
	}

	query := rebind(fmt.Sprintf(
		`SELECT key, value FROM settings WHERE key IN (%s) AND value <> ''`,
		strings.Join(placeholders, ", ")))
	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return fmt.Errorf("reading settings credentials: %w", err)
	}
	var batch []EncryptedSecret
	for rows.Next() {
		var key, record string
		if err := rows.Scan(&key, &record); err != nil {
			rows.Close()
			return fmt.Errorf("reading settings credentials: %w", err)
		}
		batch = append(batch, EncryptedSecret{
			Class:    class[key],
			Record:   record,
			table:    "settings",
			column:   "value",
			idColumn: "key",
			id:       key,
		})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("reading settings credentials: %w", err)
	}
	for _, s := range batch {
		if err := fn(s); err != nil {
			return err
		}
	}

	return nil
}

// RewriteEncryptedSecret replaces one stored ciphertext with another, matching
// on the record it read. The WHERE clause carries the old value on purpose: a
// rotation running beside a live platform must not overwrite a credential an
// administrator changed in between, and re-encrypting a value that is no longer
// there would do exactly that. A zero row count is that case, not an error —
// the new write is already under the encrypt key.
func (db *DB) RewriteEncryptedSecret(s EncryptedSecret, record string) (bool, error) {
	query := rebind(fmt.Sprintf(
		`UPDATE %s SET %s = ? WHERE %s = ? AND %s = ?`,
		s.table, s.column, s.idColumn, s.column))
	res, err := db.conn.Exec(query, record, s.id, s.Record)
	if err != nil {
		return false, fmt.Errorf("rewriting %s: %w", s.Where(), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rewriting %s: %w", s.Where(), err)
	}
	return n > 0, nil
}

// --- Wrapped data keys, for the managed encryption backend (ADR §62) ---
//
// These three methods are the DEKStore internal/crypto asks for. The interface
// is declared there and satisfied here without either package importing the
// other, which keeps internal/crypto a leaf: it encrypts, and it has never
// needed to know what a database is.

// ActiveDEK returns the generation new records are written under. No rows is
// ok=false rather than an error — a first boot on a managed install has none
// yet, and that is the normal path rather than a failure.
func (db *DB) ActiveDEK() (string, []byte, bool, error) {
	var id string
	var wrapped []byte
	err := db.conn.QueryRow(rebind(
		`SELECT id, wrapped FROM encryption_data_keys WHERE active LIMIT 1`)).Scan(&id, &wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	return id, wrapped, true, nil
}

// DEK returns one generation by id, for reading a record written under a key
// that is no longer the active one.
func (db *DB) DEK(id string) ([]byte, bool, error) {
	var wrapped []byte
	err := db.conn.QueryRow(rebind(
		`SELECT wrapped FROM encryption_data_keys WHERE id = ?`), id).Scan(&wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return wrapped, true, nil
}

// PutDEK records a new generation and makes it the active one, in one
// transaction. The previous generation is stood down rather than deleted: its
// records still exist, and dropping the row that opens them would destroy them.
func (db *DB) PutDEK(id string, wrapped []byte, kekURI string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(rebind(
		`UPDATE encryption_data_keys SET active = FALSE WHERE active`)); err != nil {
		return err
	}
	if _, err := tx.Exec(rebind(`
		INSERT INTO encryption_data_keys (id, wrapped, kek_uri, active)
		VALUES (?, ?, ?, TRUE)
		ON CONFLICT (id) DO UPDATE SET wrapped = excluded.wrapped, kek_uri = excluded.kek_uri, active = TRUE
	`), id, wrapped, kekURI); err != nil {
		return err
	}
	return tx.Commit()
}
