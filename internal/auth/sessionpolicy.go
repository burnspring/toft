package auth

import "time"

// The session's two numbers, and the rule that turns them into one `exp`.
//
// Every security baseline names these separately, because they answer different
// questions: how long a session may live at all, and how long it may sit unused.
// NIST SP 800-63B AAL2 — the level an internal app-hosting platform is assessed
// at — says twelve hours and thirty minutes. Toft's answers were a single
// Go constant and "there isn't one", which is a defensible pair of values and an
// indefensible pair of facts: a control an organization cannot configure is a
// control it cannot assert.
//
// There is a second reason, and on a running install it is the larger one. **The
// IdP cannot end a Toft session.** The authorization code is redeemed, the
// profile is read from userinfo, and no refresh token is stored — deliberately,
// and `internal/authprovider/provider.go` argues it well. What falls out is that
// somebody disabled in Okta at 09:00 keeps working here until their session runs
// out. Until back-channel logout exists, a shorter lifetime is the only lever an
// install has, and it is worthless if the number is compiled in.
//
// What this design deliberately does not have is a sessions table. The browser's
// own cookie carries the idle clock and the signature is what makes that safe —
// so there is no `last_seen` write, no per-request state, and no exemption
// needed from the rule that forbids a database write on the proxy request path.

// SessionPolicy is the pair, as durations.
type SessionPolicy struct {
	// Absolute is the ceiling, measured from `auth_time` — when the person
	// actually authenticated, which renewal does not move. A session is refused
	// past it whatever its `exp` says, so an administrator who shortens the
	// lifetime this morning reaches the sessions minted last night.
	Absolute time.Duration

	// Idle is the sliding window. Zero turns renewal off entirely and collapses
	// the rule below to `auth_time + Absolute` — a flat lifetime from sign-in,
	// which is exactly what this platform did when both numbers were constants.
	// That is what makes `0` a real setting rather than a gap.
	Idle time.Duration
}

// The bounds, and the defaults.
//
// Here rather than beside the handler that validates them, for the reason
// `database.MinDormantLockDays` is here rather than there: the handler is not
// the only reader — the settings read path clamps a corrupt row against the same
// figures, and two copies of a range is how a stored value comes to sit outside
// the range the form will accept.
const (
	// One hour is a real policy for an install that means it. A day is the
	// ceiling because it is the largest number anybody can argue is a
	// *session*: past that the word has stopped describing the thing.
	MinSessionAbsoluteHours = 1
	MaxSessionAbsoluteHours = 24

	// 0 is off, and is a real value rather than "unset". Below five minutes an
	// idle timeout signs people out while they read a page; eight hours is the
	// point where it has stopped being one, and the absolute lifetime is the
	// control that still applies.
	MinSessionIdleMinutes = 5
	MaxSessionIdleMinutes = 480
)

// The defaults, and the two are opposite in character.
//
// Absolute is unchanged from the constant it replaced — no install's sessions
// get longer or shorter on upgrade in the dimension they already had — and it
// is already stricter than AAL2's twelve-hour ceiling, so there is no reason to
// relax toward the standard.
//
// Idle ships **on**, at the standard's own number, and it is the one default
// here that changes behaviour. That is deliberate: shipping the standard
// switched off means every install is non-compliant until somebody finds the
// box. An active person never reaches it — being used is what renews a session
// — and the escape hatch is `0`, which is exactly what the platform did before.
const (
	DefaultSessionAbsoluteHours = 8
	DefaultSessionIdleMinutes   = 30
)

// DefaultSessionPolicy is the fallback for a caller with no settings to read —
// tests, and any future path that authenticates before the database is up. The
// install's real answer comes from the two settings rows.
func DefaultSessionPolicy() SessionPolicy {
	return SessionPolicy{
		Absolute: DefaultSessionAbsoluteHours * time.Hour,
		Idle:     DefaultSessionIdleMinutes * time.Minute,
	}
}

// Expiry is the rule: `min(now + idle, auth_time + absolute)`.
//
// With Idle at zero the first term drops out and this is a flat lifetime from
// sign-in. With it on, `exp` slides forward on every renewal and is clamped by
// the ceiling, so the last stretch of a long session stops moving — which is
// also what stops renewal happening at all near the end (see ShouldRenew).
func (p SessionPolicy) Expiry(authTime, now time.Time) time.Time {
	ceiling := authTime.Add(p.Absolute)
	if p.Idle <= 0 {
		return ceiling
	}
	if sliding := now.Add(p.Idle); sliding.Before(ceiling) {
		return sliding
	}
	return ceiling
}

// Expired reports whether a session that began at authTime is past the absolute
// ceiling. This is the check that is independent of `exp`, and the reason it
// exists separately: a shortened policy has to bite sessions already
// outstanding, or it is a control an assessor cannot test. It is the same rule
// the machine-credentials invariant states as "off means absent, and off means
// now".
func (p SessionPolicy) Expired(authTime, now time.Time) bool {
	return !now.Before(authTime.Add(p.Absolute))
}

// renewDivisor is the fraction of the idle window that must elapse before a
// request re-mints the cookie. A quarter means one `Set-Cookie` every seven and
// a half minutes per active browser at the default — not one per response,
// which would put a header on every poll, every image and every XHR a page
// makes.
const renewDivisor = 4

// ShouldRenew reports whether this request should re-mint the session cookie.
//
// Three ways to answer no, and the third is the interesting one: renewal stops
// when it would **change nothing**. Near the absolute ceiling `Expiry` is pinned
// there, so the re-minted token would carry the same `exp` as the one presented,
// and a `Set-Cookie` on every request for the last stretch of every long session
// is pure noise. Past the ceiling it says no for the same reason, and
// CheckSession is what actually refuses.
//
// Deliberately "different" rather than "later". A *shortened* idle window has
// to reach a session that is already open — the same "off means now" the
// absolute ceiling gets — and a renewal that only ever moved `exp` forward would
// leave an outstanding token living out the window it was minted under. Moving
// it backwards is safe because renewal cannot outlive the ceiling either way,
// and the effect converges after one renewal.
func (p SessionPolicy) ShouldRenew(c *Claims, now time.Time) bool {
	if p.Idle <= 0 || c == nil || c.IssuedAt == nil || c.ExpiresAt == nil {
		return false
	}
	if now.Sub(c.IssuedAt.Time) <= p.Idle/renewDivisor {
		return false
	}
	return !p.Expiry(c.SessionStart(), now).Equal(c.ExpiresAt.Time)
}
