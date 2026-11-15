package database

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The platform lock — an administrator's decision that nobody may reach an app.
//
// What it is for: an install accumulates apps whose author left, whose purpose
// ended, or which nobody has opened since the quarter they were built. Deleting
// one is irreversible and takes a decision nobody wants to make in a hurry;
// leaving it running keeps a port, a process and a memory footprint for
// something no one wants. A lock is the reversible middle: the app stops, the
// resources come back, visitors get a sentence explaining what happened, and
// every fact about the app survives for whoever eventually decides.
//
// Why it is not a status. See the migration and App.LockedAt: status describes
// the workload, this describes a decision about the app, and `stopped` is a
// state the *owner* may leave. A lock the owner can lift is not a lock.

// DefaultAppLockedMessage is what a visitor is told when neither the app nor
// the install says anything more specific.
//
// It names an actor and a route to a human, because the alternative — "this app
// is unavailable" — sends the reader to the owner, who cannot help, and then to
// a support queue that does not know either. It does not say *why*: the reason
// belongs to whoever locked it, and a built-in default that guessed at one
// would be wrong in exactly the cases that matter.
const DefaultAppLockedMessage = "An administrator has locked this app, so it is not currently available. " +
	"Contact your Toft administrator if you need it back."

// ResolveLockMessage picks what a refused visitor is shown, in the order the
// person closest to the decision wins.
//
// Three layers: what the administrator typed when locking *this* app, then what
// this install says about locked apps generally, then the built-in sentence.
// Empty is a gap to fill at every layer rather than a meaningful value — the
// one thing a refused visitor may never get is a blank explanation, which is
// the same rule the account-lock and no-account messages already follow.
//
// Pure, and the only thing that should read App.LockedMessage: the fallback
// chain is easy to get subtly wrong and it is read from the proxy, the API and
// the UI.
func ResolveLockMessage(app *App, s PlatformSettings) string {
	if app != nil {
		if m := strings.TrimSpace(app.LockedMessage); m != "" {
			return m
		}
	}
	if m := strings.TrimSpace(s.AppLockedMessage); m != "" {
		return m
	}
	return DefaultAppLockedMessage
}

// Locked reports whether an administrator has locked this app.
//
// A method rather than a `!= nil` at each call site because it is asked at
// every enforcement point — the proxy, the wake path, restart, deploy, upload,
// auto-deploy, jobs and schedules — and one spelling of the question is what
// keeps a new enforcement point from inventing a subtly different one.
func (a *App) Locked() bool {
	return a != nil && a.LockedAt != nil
}

// LockSource is who placed the lock, with the default every row written before
// locked_source existed carries. Empty is `admin`, because that is what a lock
// meant on every build before the platform could place one.
func (a *App) LockSource() string {
	if a == nil || strings.TrimSpace(a.LockedSource) == "" {
		return AppLockSourceAdmin
	}
	return a.LockedSource
}

// LockedByPolicy reports a lock the dependency policy placed by itself, which
// is the one a person did not decide and the one whose way out is a waiver.
func (a *App) LockedByPolicy() bool {
	return a.Locked() && a.LockSource() == AppLockSourceDependency
}

// The two sources a lock can have. `admin` is what every row written before
// this existed means, and it is the default; `dependency` is a lock the
// platform placed by itself.
//
// A column rather than a synthetic `users` row for the platform. `locked_by` is
// a foreign key into `users` and NULL there already carries two meanings — no
// lock, and a lock whose administrator has since been deleted — so a platform
// lock has no id to store and must not take NULL as a third. The alternative,
// a users row that can never sign in, is worse in a way that spreads: every
// people listing, every seat count, every directory sweep, every group
// membership query and every deleted-administrator branch would have to learn
// to exclude it, forever, to avoid one migration.
const (
	AppLockSourceAdmin      = "admin"
	AppLockSourceDependency = "dependency"
)

// SetAppLock locks an app, recording who and when, and writes the audit row in
// the same transaction. Locking an already-locked app is not an error: it
// replaces the message and the actor, which is what an administrator correcting
// their own wording expects.
//
// Its own setter rather than a field on UpdateApp, for the reason retention,
// auto-deploy and execution mode each have one: the builder calls UpdateApp
// mid-build with an App it loaded before the build started, so a lock written
// through it could be silently reverted by an overlapping deploy. A control
// whose entire purpose is to stop a deploy must not be undoable by one.
//
// **The audit row is in the transaction.** It used to be the caller's, written
// after the UPDATE had already committed, on the reading that a lock alters no
// authorization state. That reading was wrong in the way that matters: a lock
// refuses everybody, which is the most authorization-shaped thing this platform
// does, and the invariant is that an authorization-relevant mutation writes
// `audit_log` in the same transaction. The window was small and manual while
// every lock had a person behind it; LockAppForDependency puts an automatic
// writer in it four times a day.
func (db *DB) SetAppLock(id, actorID, message string, now time.Time, slug string, meta RequestMeta) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	msg := strings.TrimSpace(message)
	if _, err := tx.Exec(`
		UPDATE apps SET locked_at=?, locked_by=?, locked_message=?, locked_source=?, updated_at=?
		WHERE id=?`,
		now.UTC(), nullIfEmpty(actorID), msg, AppLockSourceAdmin, now.UTC(), id); err != nil {
		return err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "app.lock", TargetType: "app", TargetID: id,
		After: jsonStr(map[string]any{
			"locked": true, "message": msg, "app_slug": slug, "source": AppLockSourceAdmin,
		}),
		OccurredAt: now.UTC(),
		RequestID:  meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// LockAppForDependency is the platform locking an app by itself, once.
//
// **One guarded transition, and never a re-lock.** The `WHERE locked_at IS
// NULL` is what makes this idempotent against itself and safe against everyone
// else: two nodes sweeping at once means exactly one locks and the other
// changes no row and writes nothing, and a lock an administrator already placed
// is never overwritten — their message is theirs, and a policy that silently
// replaced an administrator's sentence with a generated one would be rewriting
// the record. LockDormantUser's shape exactly, for LockDormantUser's reason.
//
// Reports whether this call is the one that locked it, so the caller knows
// whether to tell anybody.
//
// The actor is PrincipalTypeSystem with no id, which is the dormancy sweep's
// convention and its comment applies unchanged: the administrator chose a
// policy months ago, they did not lock this app today, and an audit trail that
// says they did is wrong about the only thing it exists to record. What the row
// carries instead is the advisory, the package and the threshold that fired,
// because this is an entry nobody was present for and *why is my app locked* is
// answered here or by guesswork.
func (db *DB) LockAppForDependency(appID, slug, message string, detail map[string]any, now time.Time) (bool, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE apps SET locked_at=?, locked_by=NULL, locked_message=?, locked_source=?, updated_at=?
		WHERE id=? AND locked_at IS NULL`,
		now.UTC(), strings.TrimSpace(message), AppLockSourceDependency, now.UTC(), appID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Somebody, or some other node, got there first. Not an error, and
		// deliberately not an audit row either: nothing changed.
		return false, nil
	}

	after := map[string]any{
		"locked": true, "message": strings.TrimSpace(message),
		"app_slug": slug, "source": AppLockSourceDependency,
	}
	for k, v := range detail {
		after[k] = v
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeSystem, ActorID: "",
		Action: "app.lock_dependency", TargetType: "app", TargetID: appID,
		Before:     jsonStr(map[string]any{"locked": false}),
		After:      jsonStr(after),
		OccurredAt: now.UTC(),
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// LiftDependencyLock lifts a lock the dependency policy placed, because a
// deploy's release no longer carries what it was placed for. Reports whether
// this call lifted it.
//
// **Guarded by source, in the statement.** An administrator who locks the app
// while the deploy builds has made a decision no release answers, and `WHERE
// locked_source = 'dependency'` is what keeps a clean build from undoing it —
// the same guarded-transition shape LockAppForDependency has, mirrored. No row
// changed means no audit entry. The actor is the system with no id, as for the
// lock: nobody unlocked it, a release cleared it, and the row names which.
func (db *DB) LiftDependencyLock(appID, slug, deploymentID string, now time.Time) (bool, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var message string
	if err := tx.QueryRow(`SELECT locked_message FROM apps WHERE id=? AND locked_at IS NOT NULL AND locked_source=?`,
		appID, AppLockSourceDependency).Scan(&message); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	res, err := tx.Exec(`
		UPDATE apps SET locked_at=NULL, locked_by=NULL, locked_message='', locked_source=?, updated_at=?
		WHERE id=? AND locked_at IS NOT NULL AND locked_source=?`,
		AppLockSourceAdmin, now.UTC(), appID, AppLockSourceDependency)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeSystem, ActorID: "",
		Action: "app.unlock_dependency", TargetType: "app", TargetID: appID,
		Before: jsonStr(map[string]any{
			"locked": true, "message": message, "source": AppLockSourceDependency, "app_slug": slug,
		}),
		After:      jsonStr(map[string]any{"locked": false, "deployment_id": deploymentID}),
		OccurredAt: now.UTC(),
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ClearAppLock unlocks an app and forgets the message with it.
//
// Forgetting is deliberate. A stale message left behind would reappear verbatim
// the next time somebody locked the app, attributing an old administrator's
// sentence to a new decision — and the record of what happened lives in
// audit_log, which is where it belongs.
//
// Unlocking does **not** start the app. The lock stopped it, but a lock lasting
// three months means the release may no longer be one anybody wants running,
// and starting a workload as a side effect of a permission change is how an
// administrator gets a surprise. The app returns as `stopped`, and somebody
// presses Start.
// It audits in the same transaction, for the reason SetAppLock does.
//
// locked_source returns to `admin` rather than being left behind. It is inert
// while locked_at is NULL, and a stale `dependency` on an unlocked app would
// read on every surface as a lock this policy placed — the same reason the
// message is forgotten rather than kept.
func (db *DB) ClearAppLock(id, actorID, slug string, before *App, meta RequestMeta) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		UPDATE apps SET locked_at=NULL, locked_by=NULL, locked_message='', locked_source=?, updated_at=?
		WHERE id=?`, AppLockSourceAdmin, now, id); err != nil {
		return err
	}

	was := map[string]any{"locked": true, "app_slug": slug}
	if before != nil {
		was["message"] = before.LockedMessage
		was["locked_by"] = before.LockedBy
		was["source"] = before.LockSource()
	}
	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action: "app.unlock", TargetType: "app", TargetID: id,
		Before:     jsonStr(was),
		After:      jsonStr(map[string]any{"locked": false}),
		OccurredAt: now,
		RequestID:  meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// CountAppsLockedByDependency is the estate figure for the one switch that can
// take apps off the air without anybody present.
func (db *DB) CountAppsLockedByDependency() (int, error) {
	var n int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM apps
		WHERE locked_at IS NOT NULL AND locked_source = ?`, AppLockSourceDependency).Scan(&n)
	return n, err
}

// CountLockedApps is the estate figure, for the administrator who wants to know
// what this install is holding down without walking every app.
func (db *DB) CountLockedApps() (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM apps WHERE locked_at IS NOT NULL`).Scan(&n)
	return n, err
}
