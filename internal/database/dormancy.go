package database

import (
	"time"
)

// UserActivity is a user row with its dormancy clock computed beside it.
//
// The clock is the newest of four facts, and the reason it is four rather than
// one is the trap this whole feature turns on: `users.last_login_at` is written
// only where a session is minted, and a personal API key authenticates its
// owner without minting one. An account driving a nightly CI job has signed in
// once, a year ago, and is the most active credential on the install — sweeping
// on last_login_at alone would lock exactly the accounts whose locking hurts
// most, and it would do it silently.
//
// The four:
//
//   - created_at, so a provisioned account nobody ever used starts its clock
//     when it was made rather than reading as infinitely dormant. It is also
//     what makes the clock NOT NULL: every account has one.
//   - last_login_at — they proved who they are.
//   - the newest last_used_at across their personal API keys — something they
//     hold made a request.
//   - unlocked_at — an administrator asserted the account should be usable.
//     Without it the sweep undoes its own unlocks; see migration 00047.
type UserActivity struct {
	User
	// LastActiveAt is the clock. Never zero: created_at is its floor.
	LastActiveAt time.Time `json:"last_active_at"`
	// LastKeyUseAt is when one of this person's personal API keys was last
	// used, nil if never. Reported beside the clock so a surface can be honest
	// about an account that is plainly active and has never signed in —
	// "Never signed in" alone, next to a clock reading two days, is a
	// contradiction somebody has to come and ask about.
	LastKeyUseAt *time.Time `json:"last_key_use_at,omitempty"`
}

// DormantFor is how long the account has been idle as of now.
func (u UserActivity) DormantFor(now time.Time) time.Duration {
	return now.Sub(u.LastActiveAt)
}

// userActivityQuery is the one place the dormancy clock is expressed. The
// People page and the sweep both read it from here, because the failure mode of
// two copies is a page that says an account is fine and a sweep that locks it
// an hour later.
//
// GREATEST ignores NULLs in Postgres and u.created_at is NOT NULL, so the
// result is never NULL and no COALESCE is needed around it.
const userActivityQuery = `
	SELECT u.id, u.login, u.name, u.email, u.avatar_url, u.system_role,
	       u.locked_at, u.created_at, u.last_login_at, u.unlocked_at,
	       u.pending_activation_at, u.scanner_since,
	       k.last_used_at,
	       GREATEST(u.created_at, u.last_login_at, u.unlocked_at, k.last_used_at) AS last_active_at
	FROM users u
	LEFT JOIN (
		SELECT user_id, MAX(last_used_at) AS last_used_at
		FROM user_api_keys
		GROUP BY user_id
	) k ON k.user_id = u.id`

func scanUserActivity(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}) ([]UserActivity, error) {
	defer rows.Close()
	var out []UserActivity
	for rows.Next() {
		var u UserActivity
		if err := rows.Scan(&u.ID, &u.Login, &u.Name, &u.Email, &u.AvatarURL,
			&u.SystemRole, &u.LockedAt, &u.CreatedAt, &u.LastLoginAt, &u.UnlockedAt,
			&u.PendingActivationAt, &u.ScannerSince, &u.LastKeyUseAt, &u.LastActiveAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListUsersWithActivity is the admin People list: every account, with the
// clock. The clock rides along whether or not dormancy locking is switched on,
// because "when was this account last used" is a question worth answering on an
// install that has never enabled the sweep — and a column that appeared only
// once the feature was on would make the feature impossible to evaluate before
// turning it on.
func (db *DB) ListUsersWithActivity() ([]UserActivity, error) {
	rows, err := db.conn.Query(userActivityQuery + ` ORDER BY u.created_at ASC`)
	if err != nil {
		return nil, err
	}
	return scanUserActivity(rows)
}

// GetUserActivity is one account's row with its clock, for the account record.
// Nil (with no error) when there is no such user, like GetUser.
//
// It reads the clock through the same expression as the list rather than
// recomputing it, because "the People page said 90 days and the account page
// says 12" is a bug report nobody can act on.
func (db *DB) GetUserActivity(id string) (*UserActivity, error) {
	rows, err := db.conn.Query(`
		SELECT * FROM (`+userActivityQuery+`) AS a WHERE a.id = ?`, id)
	if err != nil {
		return nil, err
	}
	found, err := scanUserActivity(rows)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return &found[0], nil
}

// ListDormantUsers returns every account whose clock is older than cutoff —
// administrators and already-locked accounts included.
//
// It filters on the clock and on nothing else on purpose. Who may be locked is
// the sweep's decision and it is stated in one readable place there, rather
// than smuggled into a WHERE clause where "and never an admin" is a fragment of
// SQL nobody reads. The report of dormant administrators needs the same rows
// the sweep skips, and this way it is the same query.
func (db *DB) ListDormantUsers(cutoff time.Time) ([]UserActivity, error) {
	rows, err := db.conn.Query(`
		SELECT * FROM (`+userActivityQuery+`) AS a
		WHERE a.last_active_at < ?
		ORDER BY a.last_active_at ASC`, cutoff)
	if err != nil {
		return nil, err
	}
	return scanUserActivity(rows)
}

// CountDormantLockable is how many accounts the sweep would lock if it ran
// now with this cutoff: past the threshold, not already locked, not an
// administrator, and not still waiting to be activated. The four exclusions
// are the sweep's rules, restated here
// because this answers "what will pressing this do" and an answer that counted
// rows the sweep will not touch would be worse than no answer.
//
// It exists for the settings page's warning. An administrator switching this on
// for the first time is about to lock a batch of real people in one pass, and
// finding out afterwards is finding out from them.
func (db *DB) CountDormantLockable(cutoff time.Time) (int, error) {
	var n int
	err := db.conn.QueryRow(`
		SELECT COUNT(*) FROM (`+userActivityQuery+`) AS a
		WHERE a.last_active_at < ? AND a.locked_at IS NULL AND a.system_role <> ?
		  AND a.pending_activation_at IS NULL`,
		cutoff, SystemRoleAdmin).Scan(&n)
	return n, err
}

// LockDormantUser locks one account for dormancy and audits it in the same
// transaction, which is the rule every authorization-relevant mutation follows.
//
// The token_version bump is the same one handleLockUser makes and for the same
// reason: a lock that leaves live sessions breathing until the JWT expires is
// not a lock. The guarded WHERE means two nodes sweeping at once cannot both
// lock the same account, and neither can this race an administrator doing it by
// hand — the second one changes no row and writes no audit entry.
//
// The actor is PrincipalTypeSystem, never the administrator who happened to
// enable the setting. They chose a policy months ago; they did not lock this
// account today, and an audit trail that says they did is wrong about the only
// thing it exists to record.
func (db *DB) LockDormantUser(u UserActivity, thresholdDays int, now time.Time) (bool, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE users SET locked_at = ?, token_version = token_version + 1
		WHERE id = ? AND locked_at IS NULL`, now, u.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Somebody else got there first. Not an error, and deliberately not an
		// audit row either: nothing changed.
		return false, nil
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeSystem, ActorID: "",
		Action:     "user.lock_dormant",
		TargetType: "user", TargetID: u.ID,
		Before: jsonStr(map[string]any{"locked": false}),
		// The threshold and the clock are on the row because this is the one
		// audit entry nobody was present for. "Why is my account locked" is
		// answered here or it is answered by guesswork.
		After: jsonStr(map[string]any{
			"locked":         true,
			"reason":         "dormant",
			"threshold_days": thresholdDays,
			"last_active_at": u.LastActiveAt.UTC().Format(time.RFC3339),
		}),
		OccurredAt: now,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
