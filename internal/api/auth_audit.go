package api

import (
	"log"
	"net/http"

	"toft/internal/database"
)

// The session ledger.
//
// audit_log was built to answer "who changed what", and it deliberately did not
// answer "who was here": ResolveIdentity audits provisioning and linking and
// skips the routine sign-in, on the grounds that a row per sign-in would bury
// the governance trail (internal/database/authproviders.go).
//
// That reasoning holds for *app* traffic, which is high-volume, belongs to the
// usage rollups and the collector, and may never be written on the proxy
// request path at all. It does not hold for authentication itself. Sign-ins are
// bounded by the number of people who work here, and their absence is the first
// thing a security review asks about: until now an install could not say
// whether an administrator had signed in, from where, or how many times
// somebody had failed to.
//
// So `session.*` is a third family beside `identity.*` (an account changed) and
// `user.*` (an administrator acted on an account). It has a chip of its own in
// the admin feed, which is what keeps the original objection answered — the
// noisy family can be read on its own, and never becomes what "Access" means.
//
// Three rules apply here and nowhere else in this table:
//
//   - It is written outside the transaction, through WriteAuditRow. A sign-in
//     is not a mutation whose record has to be atomic with it, and an audit
//     insert that failed must never be the reason somebody cannot sign in.
//   - The actor may be unknown, because an attempt against an account that does
//     not exist has no user id. What was typed goes in the detail and never in
//     actor_id — the feed and the user page read actor_id as an id, so a caller
//     who could choose it could write rows into somebody else's history.
//   - Every reason that telemetry also counts uses telemetry's outcome string
//     verbatim. The metric and the row disagreeing about what "locked" means is
//     a debugging session nobody should have to have.
const (
	// ActionSignin is a session that was actually minted. Not "the password
	// verified": the must-change path verifies a password and mints nothing,
	// and recording that as a sign-in would say an install still holding its
	// published default password was signing people in fine.
	ActionSignin = "session.signin"
	// ActionSigninFailed is any attempt that ended without a session — wrong
	// credential, locked account, throttled account, or a policy refusal. The
	// reason in the detail is what tells them apart.
	ActionSigninFailed = "session.signin_failed"
	// ActionSignout is an explicit sign-out, which revokes every session the
	// user has. Expiry is not an act anybody performs, so it is not one of
	// these.
	ActionSignout = "session.signout"
)

// Refusals that telemetry does not count, and therefore have no outcome string
// to borrow.
//
// Both are OIDC-side policy rather than a failed credential: the person
// authenticated perfectly well and this install declined to seat them. They are
// audit-only on purpose — adding them to `toft.auth.signin` would push its
// outcome enum past the 8 the cardinality budget declares, and the budget is a
// test, not a comment.
const (
	// signinDomainRefused: the provider vouched for an address outside the
	// domains this provider allows.
	signinDomainRefused = "email_not_allowed"
	// signinNotProvisioned: authentication worked and this install does not
	// hand out accounts. Distinct from telemetry's `no_account`, which is the
	// password path's "no such user" and means the opposite thing about
	// whether the person exists.
	signinNotProvisioned = "not_provisioned"
	// signinGroupRefused: the provider asserted a group claim and it held none
	// of the values this provider permits. Present-and-empty lands here too —
	// "in no groups" is an assertion, and it is honoured.
	signinGroupRefused = "group_not_permitted"
	// signinGroupClaimAbsent: this provider gates on groups and the sign-in
	// asserted no group claim at all.
	//
	// Distinct from signinGroupRefused, and the distinction is the point: a
	// claim that arrived and matched nothing is fixed in the admin form, and a
	// claim that never arrived is fixed at the IdP. The person is told the same
	// sentence either way; the administrator is not.
	signinGroupClaimAbsent = "group_claim_absent"
)

// auditActorAnonymous marks a row whose actor could not be identified. It is
// not a database.PrincipalType — nothing holds a grant as anonymous — it is
// only how the feed labels the "who" column for an attempt against an account
// that does not exist.
const auditActorAnonymous = "anonymous"

// maxAttemptedLogin bounds the one caller-supplied string that reaches this
// table. Long enough for a real login or a work email address, short enough
// that nobody can use the audit log as storage.
const maxAttemptedLogin = 64

// signinAudit is one authentication outcome, ready to be written.
type signinAudit struct {
	Action   string // one of the three above
	UserID   string // empty when no account was identified
	Provider string // database.ProviderKind*
	Reason   string // a telemetry sign-in outcome, or one of the two above
	// Attempted is what the caller claimed to be. Recorded only when UserID is
	// empty — once the account is known, its id says it better and this would
	// only be a second spelling of the same fact.
	Attempted string
}

// recordSignin writes one row and never fails the request.
//
// Every caller has already decided what to tell the person, and none of them
// may revisit that because a row would not insert. A failure is logged and the
// sign-in carries on — the same rule the last-login stamp follows.
func (s *Server) recordSignin(r *http.Request, a signinAudit) {
	e := auditEntryFor(r)
	e.Action = a.Action
	if a.UserID != "" {
		e.ActorType = database.PrincipalTypeUser
		e.ActorID = a.UserID
		// Actor and target are the same person: an authentication is something
		// an account does to itself, unlike `user.lock`, where an administrator
		// acts on somebody else.
		e.TargetType = "user"
		e.TargetID = a.UserID
	} else {
		e.ActorType = auditActorAnonymous
	}

	detail := map[string]string{"provider": a.Provider}
	if a.Reason != "" {
		detail["reason"] = a.Reason
	}
	if a.UserID == "" && a.Attempted != "" {
		detail["attempted"] = truncateForAudit(a.Attempted, maxAttemptedLogin)
	}
	e.After = jsonValStr(detail)

	if err := s.db.WriteAuditRow(e); err != nil {
		log.Printf("audit: recording %s: %v", a.Action, err)
	}
}

// truncateForAudit cuts on a rune boundary, so a multi-byte name is shortened
// rather than turned into invalid UTF-8 that the JSON encoder then drops.
func truncateForAudit(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
