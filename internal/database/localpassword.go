package database

import (
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"toft/internal/password"
)

// The built-in local administrator and its password.
//
// Everything here concerns exactly one account. It is not the beginning of a
// local user directory: the platform's answer to "who may sign in" is an
// identity provider, and this account exists so that answering it is possible in
// the first place — an install with no provider yet, or one whose provider has
// broken, still has a way in.
//
// Hashing lives in internal/password. This file stores what that package
// produces and never inspects it.

const (
	// BuiltinAdminID is the fixed user id of the local administrator, created by
	// migration 21. Fixed rather than generated because the CLI break-glass path
	// must find this account on an install nobody can log into, and the login is
	// something an administrator may rename.
	BuiltinAdminID = "builtin-admin"
	// BuiltinAdminLogin and BuiltinAdminEmail are what migration 21 created the
	// account with. An administrator may rename it afterwards — these are the
	// documented starting values, not a live lookup, and the CLI prints them
	// rather than assuming they still hold.
	BuiltinAdminLogin = "admin"
	BuiltinAdminEmail = "admin@toft.local"

	// LocalProviderID is the reserved provider id for password sign-in. An admin
	// cannot create, delete or duplicate it.
	LocalProviderID = "local"
)

const (
	// passwordMaxFailures is how many wrong passwords a single account tolerates
	// before it stops answering for a while.
	passwordMaxFailures = 10
	// passwordLockout is how long that lasts. Long enough to make an online
	// guessing attack pointless, short enough that a locked-out administrator
	// waits rather than reaches for the CLI.
	passwordLockout = 15 * time.Minute
)

// ensureBuiltinAdmin makes the built-in administrator exist and have a password.
//
// Migration 21 creates the account, so on a normal install this finds everything
// already in place and does nothing. It runs on every boot anyway because the
// guarantee it enforces — *there is always one way in* — has to survive things a
// migration cannot see: a `DELETE FROM users` in a dev script, an operator
// tidying up the user table, a restore from a partial dump. Without it, an
// install in that state has no local administrator and no way to notice.
//
// The password hash is written here rather than in SQL because hashing is Go code
// with cost parameters that will change, and a literal hash in a migration would
// be a copy of it that nothing keeps in step.
//
// must_change is always set, whatever the password is: it proves the operator
// reached the credential, and buys nothing but the right to replace it.
//
// WHERE THE FIRST PASSWORD COMES FROM, and why it is not a constant.
//
// It used to be the published default, `toft`, on every install. That is
// safe against session-minting — must_change sees to it — but it is not safe
// against being *claimed*: whoever reaches the login page first replaces the
// password and owns the administrator. On a laptop that is nothing. On an
// install published at a hostname, it is a race with the internet, and
// scripts/k3s.sh has always had to print a warning saying so.
//
// So a fresh install now gets one of two things, and neither is a constant:
//
//   - the password the operator supplied (BOOTSTRAP_ADMIN_PASSWORD), for an
//     install whose deployment already has somewhere safe to put one — the Helm
//     chart generates it into the release Secret, where `kubectl get secret`
//     reads it back under normal RBAC; or
//   - a generated one, printed to the log ONCE, at the only moment it exists in
//     plaintext anywhere.
//
// The published default is now something an install must ask for by name. It
// stays documented for the local dev loop, where the convenience is real and the
// exposure is not.
func (db *DB) ensureBuiltinAdmin() error {
	// The account. Not upserted: an administrator may rename it, change its
	// email, or set it to a different role, and a boot must not undo that.
	res, err := db.conn.Exec(`
		INSERT INTO users (id, login, name, email, avatar_url, system_role)
		VALUES (?, ?, 'Local administrator', ?, '', ?)
		ON CONFLICT (id) DO NOTHING`,
		BuiltinAdminID, BuiltinAdminLogin, BuiltinAdminEmail, SystemRoleAdmin)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("The built-in administrator was missing and has been re-created. " +
			"Its password follows, and is subject to the same rules as a fresh install's.")
	}

	if _, err := db.conn.Exec(`
		INSERT INTO user_identities (provider_id, subject, user_id, email, login, name)
		VALUES (?, ?, ?, ?, ?, 'Local administrator')
		ON CONFLICT (provider_id, subject) DO NOTHING`,
		LocalProviderID, BuiltinAdminID, BuiltinAdminID, BuiltinAdminEmail, BuiltinAdminLogin); err != nil {
		return err
	}

	var exists int
	err = db.conn.QueryRow(`SELECT 1 FROM user_passwords WHERE user_id = ?`, BuiltinAdminID).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	pw, generated := db.bootstrapAdminPassword, false
	if pw == "" {
		if pw, err = password.Generate(); err != nil {
			return err
		}
		generated = true
	}
	// Deliberately not length-validated. This credential carries must_change, so
	// it authorizes nothing but its own replacement, and refusing to boot over
	// the shape of a password that cannot mint a session would turn a weak choice
	// into an outage.
	hash, err := password.Hash(pw)
	if err != nil {
		return err
	}
	// No audit row: this is the install coming into existence, not somebody
	// acting on it, and there is no actor to name.
	if _, err := db.conn.Exec(`
		INSERT INTO user_passwords (user_id, password_hash, must_change, updated_at)
		VALUES (?, ?, TRUE, ?)
		ON CONFLICT (user_id) DO NOTHING`, BuiltinAdminID, hash, time.Now().UTC()); err != nil {
		return err
	}

	// The one moment this string exists in plaintext. Printing it is the whole
	// retrieval mechanism for an install that supplied nothing: there is no
	// second copy, because the only other place to keep one would be a file or a
	// column that outlives the need for it.
	if generated {
		log.Printf("\n"+
			"  ┌─ FIRST BOOT ─────────────────────────────────────────────────────\n"+
			"  │ Built-in administrator\n"+
			"  │   username  %s\n"+
			"  │   password  %s\n"+
			"  │\n"+
			"  │ Shown once, here, and stored only as a hash. It must be replaced\n"+
			"  │ at first sign-in — no session is issued until it is.\n"+
			"  │ Lost it? `toft local-admin enable` prints a new one.\n"+
			"  └──────────────────────────────────────────────────────────────────\n",
			BuiltinAdminLogin, pw)
	} else {
		log.Printf("Built-in administrator %q created with the supplied bootstrap password, "+
			"which must be replaced at first sign-in.", BuiltinAdminLogin)
	}
	return nil
}

// BuiltinAdminHasDefaultPassword reports whether the account still holds the
// published default. The server logs a warning about it at startup, because an
// install left in that state has an administrator anyone can claim.
func (db *DB) BuiltinAdminHasDefaultPassword() (bool, error) {
	cred, err := db.GetPasswordCredential(BuiltinAdminID)
	if err != nil || cred == nil {
		return false, err
	}
	return cred.MustChange && password.Verify(cred.Hash, password.Default), nil
}

// PasswordCredential is one account's stored password and its guessing state.
type PasswordCredential struct {
	UserID string
	// Hash is the encoded form from internal/password. Never leaves the server.
	Hash string
	// MustChange means verifying this password authorizes replacing it and
	// nothing else — no session is minted while it is set.
	MustChange     bool
	FailedAttempts int
	// LockedUntil is when guessing may resume. Nil, or in the past, means now.
	LockedUntil *time.Time
	UpdatedAt   time.Time
}

// Throttled reports whether this credential is currently refusing attempts.
func (c *PasswordCredential) Throttled(now time.Time) bool {
	return c.LockedUntil != nil && c.LockedUntil.After(now)
}

const passwordColumns = `user_id, password_hash, must_change, failed_attempts, locked_until, updated_at`

func scanPassword(scan func(...any) error) (*PasswordCredential, error) {
	c := &PasswordCredential{}
	if err := scan(&c.UserID, &c.Hash, &c.MustChange, &c.FailedAttempts, &c.LockedUntil, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return c, nil
}

// GetPasswordCredential returns the stored password for a user, or nil when the
// user has none — which is every user but one.
func (db *DB) GetPasswordCredential(userID string) (*PasswordCredential, error) {
	c, err := scanPassword(func(dest ...any) error {
		return db.conn.QueryRow(`SELECT `+passwordColumns+` FROM user_passwords WHERE user_id = ?`, userID).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// FindPasswordUser resolves what someone typed into the username box.
//
// Login or email, case-insensitively, and only for accounts that actually have a
// password — an SSO user's login must not become a target for guessing. Returns
// nil, nil when nothing matches; the caller must answer identically to a wrong
// password, so this reports absence rather than an error.
func (db *DB) FindPasswordUser(identifier string) (*User, *PasswordCredential, error) {
	id := strings.ToLower(strings.TrimSpace(identifier))
	if id == "" {
		return nil, nil, nil
	}

	var userID string
	err := db.conn.QueryRow(`
		SELECT u.id FROM users u
		JOIN user_passwords p ON p.user_id = u.id
		WHERE LOWER(u.login) = ? OR LOWER(COALESCE(u.email, '')) = ?`, id, id).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	user, err := db.GetUser(userID)
	if err != nil || user == nil {
		return nil, nil, err
	}
	cred, err := db.GetPasswordCredential(userID)
	if err != nil {
		return nil, nil, err
	}
	return user, cred, nil
}

// RecordPasswordSignIn stamps the identity row after a successful local sign-in.
//
// `users.last_login_at` is stamped by mintSession, which every path goes through.
// This is the other half — the per-identity "when did this login last work",
// which the account page shows next to each way in. Without it the local identity
// reads as never used, on the one account that is definitely being used.
//
// Reporting, not authorization: the caller logs a failure and carries on.
func (db *DB) RecordPasswordSignIn(userID string) error {
	_, err := db.conn.Exec(`
		UPDATE user_identities SET last_login_at = ?
		WHERE provider_id = ? AND user_id = ?`, time.Now().UTC(), LocalProviderID, userID)
	return err
}

// SetPassword stores a new password, clears the guessing state, and revokes
// every session the account already had.
//
// The token version bump is the point: changing a credential must invalidate
// sessions minted with the old one, or a stolen password stays useful after the
// theft is noticed. Audited in the same transaction, like any other
// authorization-relevant mutation — actorID is whoever performed the change,
// which for the CLI is the operator and not the account.
func (db *DB) SetPassword(userID, hash string, mustChange bool, audit AuditEntry) error {
	now := time.Now().UTC()

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO user_passwords (user_id, password_hash, must_change, failed_attempts, locked_until, updated_at)
		VALUES (?, ?, ?, 0, NULL, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			password_hash = excluded.password_hash, must_change = excluded.must_change,
			failed_attempts = 0, locked_until = NULL, updated_at = excluded.updated_at`,
		userID, hash, mustChange, now); err != nil {
		return err
	}

	if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id = ?`, userID); err != nil {
		return err
	}

	audit.Action = "password.set"
	audit.TargetType = "user"
	audit.TargetID = userID
	audit.After = jsonStr(map[string]any{"must_change": mustChange})
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordPasswordFailure counts a wrong password and returns when attempts may
// resume, or nil if the account is not yet locked out.
//
// The counter is not reset by the lockout expiring — it is reset by a successful
// sign-in or a password change. So an attacker who waits out one lockout gets
// one attempt, not another ten. That also lets anyone re-arm the lock with one
// wrong password a window; what keeps the person it protects signing in is the
// sign-in device, which the handler counts instead (ADR §75) — not a reset here,
// which would buy an attacker ten guesses a window and still keep the lock armed.
func (db *DB) RecordPasswordFailure(userID string) (*time.Time, error) {
	var attempts int
	var lockedUntil *time.Time
	err := db.conn.QueryRow(`
		UPDATE user_passwords
		SET failed_attempts = failed_attempts + 1,
			locked_until = CASE WHEN failed_attempts + 1 >= ? THEN ? ELSE locked_until END
		WHERE user_id = ?
		RETURNING failed_attempts, locked_until`,
		passwordMaxFailures, time.Now().UTC().Add(passwordLockout), userID).Scan(&attempts, &lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return lockedUntil, nil
}

// ClearPasswordFailures forgets the guessing state after a correct password.
func (db *DB) ClearPasswordFailures(userID string) error {
	_, err := db.conn.Exec(
		`UPDATE user_passwords SET failed_attempts = 0, locked_until = NULL WHERE user_id = ?`, userID)
	return err
}

// SetAuthProviderEnabled flips one provider's enabled flag and nothing else.
//
// Separate from UpdateAuthProvider because the callers are different in kind: an
// admin disabling local sign-in through the API, and the CLI re-enabling it on a
// box nobody can log into. Neither has a full provider row to write back, and
// rewriting every column to change one boolean is how an unrelated field gets
// clobbered.
func (db *DB) SetAuthProviderEnabled(id string, enabled bool, audit AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var was bool
	err = tx.QueryRow(`SELECT enabled FROM auth_providers WHERE id = ?`, id).Scan(&was)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProviderNotFound
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`UPDATE auth_providers SET enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, time.Now().UTC(), id); err != nil {
		return err
	}

	audit.Action = "auth_provider.enabled"
	audit.TargetType = "auth_provider"
	audit.TargetID = id
	audit.Before = jsonStr(map[string]any{"enabled": was})
	audit.After = jsonStr(map[string]any{"enabled": enabled})
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// HasAdminIdentityOn reports whether any unlocked system_admin can sign in
// through one of the given providers.
//
// This is the question behind "may local sign-in be switched off": not whether
// another provider exists, but whether an administrator is actually reachable
// through one. A provider configured five minutes ago that nobody has ever
// signed in through is not a way back in.
func (db *DB) HasAdminIdentityOn(providerIDs []string) (bool, error) {
	if len(providerIDs) == 0 {
		return false, nil
	}
	// Built by hand rather than with ANY(): the placeholder rewriting this
	// package does works on `?`, and a driver-specific array type would be the
	// only thing in here that is not portable SQL.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(providerIDs)), ",")
	args := make([]any, 0, len(providerIDs))
	for _, id := range providerIDs {
		args = append(args, id)
	}
	var one int
	err := db.conn.QueryRow(`
		SELECT 1 FROM user_identities i
		JOIN users u ON u.id = i.user_id
		WHERE i.provider_id IN (`+placeholders+`)
		  AND u.system_role = 'system_admin' AND u.locked_at IS NULL
		LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UnlockUserForRecovery clears a lock without an actor, for the CLI.
//
// The API's unlock path is an administrator acting on someone else and is
// audited as such. This one is the operator repairing an install from a shell,
// which is a different fact and is recorded as one.
func (db *DB) UnlockUserForRecovery(userID string, audit AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE users SET locked_at = NULL, unlocked_at = NOW() WHERE id = ? AND locked_at IS NOT NULL`, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already unlocked. Not an error, and not worth an audit row.
		return nil
	}

	audit.Action = "user.unlock"
	audit.TargetType = "user"
	audit.TargetID = userID
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}
