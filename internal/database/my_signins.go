package database

import "time"

// A person's own sign-in history.
//
// This is the **second** read path `audit_log` has ever had, and the first was
// `/admin/audit` — system_admin only, 404 to everyone else. That the ledger had
// exactly one reader was a property worth something, so this is deliberately
// shaped as a keyhole rather than a door:
//
//   - `actor_id` is the caller and there is no parameter that could widen it.
//     Not a filter with a default — an argument the handler fills from the
//     principal and nothing else can reach.
//   - The `session.*` family only, matched here rather than passed in. The
//     ledger's other families describe things done *to* the install; this
//     answers one question about one credential.
//   - A fixed window and a fixed cap, both the caller's to pass but both
//     bounded by the handler that owns them. No cursor, no `since`, no paging —
//     paging is how a keyhole becomes a door.
//   - Four columns. `before` and `after` are never selected: nothing in the
//     session family needs them, and they are exactly where a widened query
//     would start leaking the rest of the ledger.
//
// A row whose actor is the caller cannot name another person, which is what
// makes this safe to show without any redaction step.

// MySignin is one authentication outcome on the caller's own account. The
// action carries the outcome — signed in, refused, signed out, sessions
// revoked — and the reason detail is deliberately not part of it.
type MySignin struct {
	OccurredAt time.Time `json:"occurred_at"`
	Action     string    `json:"action"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
}

// ListMySignins returns the caller's own session events, newest first.
func (db *DB) ListMySignins(userID string, since time.Time, limit int) ([]MySignin, error) {
	rows, err := db.conn.Query(`
		SELECT occurred_at, action, COALESCE(ip, ''), COALESCE(user_agent, '')
		FROM audit_log
		WHERE actor_id = ?
		  AND actor_type = ?
		  AND action LIKE 'session.%'
		  AND occurred_at >= ?
		ORDER BY occurred_at DESC
		LIMIT ?`,
		userID, PrincipalTypeUser, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []MySignin{}
	for rows.Next() {
		var s MySignin
		if err := rows.Scan(&s.OccurredAt, &s.Action, &s.IP, &s.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
