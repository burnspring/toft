package database

import (
	"context"
	"errors"
)

// Locking and unlocking an account.
//
// These were written inline in `handleLockUser` and `handleUnlockUser` — the
// transaction, the last-administrator policy, the `token_version` bump and the
// audit row, interleaved with URL parsing and HTTP responses. That was correct
// while a person clicking a button was the only caller. It stops being correct
// the moment a second one exists that has no person behind it, because the
// policy would then live in a handler one caller happens to route through.
//
// So the decision is here and the handlers keep the HTTP. A caller supplies who
// is acting; everything about *whether* the lock is allowed is below.

// A lock that would leave the install with no unlocked administrator is refused
// with ErrLastAdmin, which `database.go` already declares for the deletion that
// refuses on the same ground. One error for one rule: an install with no
// administrator who can sign in is unadministrable however it got that way, and
// two errors saying so would eventually disagree about what they mean.
//
// The caller has to be able to tell it apart from "already locked" and from a
// database failure, because it is the one refusal here that is a policy decision
// rather than a state. A person reads it as a sentence; a feed renders it into
// its own protocol's error shape.

// ErrCannotLockSelf refuses the one lock nobody should be able to perform by
// hand. Absent for a caller with no identity — a feed cannot be the account it
// is deactivating.
var ErrCannotLockSelf = errors.New("an account cannot lock itself")

// LockUser locks an account and ends its live sessions.
//
// **Sessions-scoped, and that is the whole of it.** Apps the person owns keep
// running and those apps' API keys keep working: an app is not a person, and
// stopping production because somebody left is a second incident on top of the
// first. What a lock takes away is the ability to sign in and to be chosen as a
// grant or transfer target.
//
// The `token_version` bump is in the same transaction as the `locked_at` write,
// because a lock that leaves live sessions breathing until their JWTs expire is
// not a lock.
//
// Returns (false, nil) when the account was already locked — no write, no audit
// row, and the caller decides what that means. A repeated lock is not an error,
// and for a caller that retries on a schedule it must not become one.
// OtherUnlockedAdminsTx counts the administrators who can sign in, leaving out
// the one named. It is the one predicate behind the last-administrator refusal
// — the lock, the demotion, the deletion and the SCIM deactivation all ask it —
// because four spellings of "the last administrator" drifted into three, and
// the one that counted *locked* administrators let the last person who could
// sign in be demoted from the browser. An administrator who is locked cannot
// unlock anybody, so they do not count.
//
// **Serialised install-wide** by a transaction advisory lock, held to commit:
// two transactions that each remove a different administrator each counted
// the other and both committed, leaving nobody who could sign in (security
// review 2026-11-14, DB-10). FOR UPDATE is not available on an aggregate.
func OtherUnlockedAdminsTx(tx *Tx, excludingID string) (int, error) {
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(('x' || substr(md5('last-admin'), 1, 16))::bit(64)::bigint)`); err != nil {
		return 0, err
	}
	var others int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM users WHERE system_role = ? AND locked_at IS NULL AND id <> ?`,
		SystemRoleAdmin, excludingID).Scan(&others)
	return others, err
}

func (db *DB) LockUser(ctx context.Context, userID string, audit AuditEntry) (bool, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if audit.ActorType == PrincipalTypeUser && audit.ActorID == userID {
		return false, ErrCannotLockSelf
	}

	var role string
	var locked *string
	if err := tx.QueryRow(`SELECT system_role, locked_at::text FROM users WHERE id=?`, userID).
		Scan(&role, &locked); err != nil {
		return false, err
	}
	if locked != nil {
		return false, nil
	}

	// The last-administrator protection. An install whose only unlocked
	// administrator is locked cannot be administered at all — including to undo
	// the lock — so this is refused whoever asks and however they ask.
	if role == SystemRoleAdmin {
		others, err := OtherUnlockedAdminsTx(tx, userID)
		if err != nil {
			return false, err
		}
		if others == 0 {
			return false, ErrLastAdmin
		}
	}

	if _, err := tx.Exec(
		`UPDATE users SET locked_at = NOW(), token_version = token_version + 1 WHERE id=? AND locked_at IS NULL`,
		userID); err != nil {
		return false, err
	}

	audit.Action = "user.lock"
	audit.TargetType = "user"
	audit.TargetID = userID
	audit.Before = jsonStr(map[string]bool{"locked": false})
	if audit.After == "" {
		audit.After = jsonStr(map[string]bool{"locked": true})
	}
	if err := writeAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// UnlockUser clears a lock and restarts the dormancy clock.
//
// `unlocked_at` is stamped by the same statement and is not decoration: it is
// one of the four inputs to the dormancy clock (ADR §48). Without it, unlocking
// an account the sweep locked leaves the other three inputs still older than the
// threshold, and the next pass locks it straight back — which reads as the
// unlock button being broken. An unlock is somebody asserting the account should
// be usable *now*, so the grace period runs from here.
//
// Returns (false, nil) when the account was not locked, for LockUser's reason.
func (db *DB) UnlockUser(ctx context.Context, userID string, audit AuditEntry) (bool, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE users SET locked_at = NULL, unlocked_at = NOW() WHERE id=? AND locked_at IS NOT NULL`,
		userID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n == 0 {
		return false, nil
	}

	audit.Action = "user.unlock"
	audit.TargetType = "user"
	audit.TargetID = userID
	audit.Before = jsonStr(map[string]bool{"locked": true})
	if audit.After == "" {
		audit.After = jsonStr(map[string]bool{"locked": false})
	}
	if err := writeAuditTx(tx, audit); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
