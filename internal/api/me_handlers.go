package api

import (
	"net/http"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
)

// meDormancy is what the idle sweep will do to *this* account, and it exists
// because ADR §48 locks people who then find out by being unable to sign in
// and reading a generic message.
//
// It is computed here rather than shipped as a policy the client applies. The
// threshold lives in platform settings, which only an administrator may read,
// and the two conditions on showing it at all are rules rather than taste:
// absent when the sweep is off (absent, not dormant), and absent for an
// administrator, whom the sweep never locks — a warning that cannot come true
// is a lie with a date on it. Deciding that on the server means no client can
// get it wrong, and no threshold leaks to somebody who may not read it.
type meDormancy struct {
	ThresholdDays int       `json:"threshold_days"`
	LocksOn       time.Time `json:"locks_on"`
}

// meAccountResponse is the administrator's record shape plus the one fact only
// the subject is owed. The embedded struct is anonymous so the wire shape stays
// exactly `UserDetail` with an optional field beside it.
type meAccountResponse struct {
	userDetailResponse
	Dormancy *meDormancy `json:"dormancy,omitempty"`
	// CanChangePassword says whether POST /auth/local/password would do
	// anything for this caller. Exactly one account on any install can use it —
	// the built-in administrator — and only while local sign-in is switched on.
	//
	// Decided here rather than inferred by the client from the provider list,
	// because "is this account the built-in administrator" is not a fact the
	// client should be reconstructing. False means the Security tab renders no
	// password section at all — absent, not a disabled form explaining that
	// your identity provider owns your password.
	CanChangePassword bool `json:"can_change_password"`
}

// dormancyFor returns the warning this account should see, or nil when it
// should see none.
func (s *Server) dormancyFor(user *database.UserActivity) *meDormancy {
	// Never for an administrator: the sweep skips them by construction, not by
	// configuration, so there is nothing here to warn about.
	if user.SystemRole == database.SystemRoleAdmin {
		return nil
	}
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		// A settings read that failed is not evidence the sweep is on. Saying
		// nothing is the safe direction — the same reasoning the sweeper uses
		// when it refuses to fall back to a default threshold.
		return nil
	}
	if !settings.DormantLockEnabled || settings.DormantLockDays <= 0 {
		return nil
	}
	// last_active_at is GREATEST(created_at, …) and created_at is NOT NULL, so
	// there is always a clock to count from.
	return &meDormancy{
		ThresholdDays: settings.DormantLockDays,
		LocksOn:       user.LastActiveAt.AddDate(0, 0, settings.DormantLockDays),
	}
}

// The account's view of itself.
//
// Everything here has an administrator-facing twin under /users/{id}, and the
// split is the point. The obvious cheap move would have been to relax the
// admin gate on those handlers to `id != p.ID && !admin`. That version is
// correct on the day it is written and one refactor away from an IDOR in the
// most sensitive read on the install, because it makes the authorization
// invariant — *anything taking an ID from the URL scopes its query by that
// ID* — conditional, and a conditional rule has to be re-audited every time
// the handler changes.
//
// These routes take no id at all. The subject is the principal and cannot be
// anything else, so that class of bug is structurally absent rather than
// tested for, /users/{id} keeps its unconditional gate, and the namespace
// matches the one the router already had for /me/api-keys and
// /me/promotion-requests, which were built this way for this reason.
//
// What is *not* duplicated is the computation. Both reads below call the same
// database helpers and the same access-report builder the admin routes call,
// so there is one implementation of "what is this person's effective role on
// this app" and one of "what is this provider called". A page is a reader of a
// computation, never a second copy of it.

// handleMyAccount — GET /me/account. The self-scoped half of GET /users/{id}:
// the activity clocks the dormancy sweep reads, group memberships with the
// date each was granted, and the external logins that resolve here.
//
// Allowed to a personal API key. These are facts about the identity the key
// acts as — the same class as GET /auth/me, which a key may already read — and
// a script that wants to know what it is before acting is the intended use.
func (s *Server) handleMyAccount(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// The activity-carrying row rather than the bare user: "last signed in" and
	// "last used" are different facts and the pair is the whole reason this
	// page can answer "will I be locked for being idle".
	user, err := s.db.GetUserActivity(p.ID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "sign in again, with toft login or through the browser"})
		return
	}

	groups, err := s.db.ListUserGroupMemberships(p.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	identities, err := s.resolveUserIdentities(p.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The same response shape as the administrator's view, deliberately: two
	// renderings of one record should not need two types to read it.
	writeJSON(w, http.StatusOK, meAccountResponse{
		userDetailResponse: userDetailResponse{User: user, Groups: groups, Identities: identities},
		Dormancy:           s.dormancyFor(user),
		CanChangePassword:  p.ID == database.BuiltinAdminID && s.localProviderEnabled(),
	})
}

// The window and the cap on a person's own sign-in history. Constants rather
// than parameters: D4's whole argument for opening a second read path into
// audit_log is that it stays a keyhole, and a caller-supplied range is the
// first step to it being a door.
const (
	mySigninDays  = 30
	mySigninLimit = 50
)

// handleMySignins — GET /me/sign-ins. Who has signed in as you, from where,
// and when.
//
// "Did somebody else sign in as me" is the one security question a person can
// answer about themselves and act on without an administrator — and the
// platform has recorded the answer since sign-ins became auditable while
// showing it to nobody but administrators. The action a person takes on a bad
// answer is POST /me/sessions/revoke, immediately below.
//
// **Denied to a personal API key.** This is a governance read about the
// *human's* sessions, not about the credential asking. A key's own history is
// /me/api-keys/{id}/usage, which it already has.
func (s *Server) handleMySignins(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	since := time.Now().UTC().AddDate(0, 0, -mySigninDays)
	rows, err := s.db.ListMySignins(p.ID, since, mySigninLimit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sign_ins": rows,
		// The bounds travel with the answer so the page can say what it is
		// showing rather than implying it is everything.
		"days":  mySigninDays,
		"limit": mySigninLimit,
	})
}

// handleRevokeMySessions — POST /me/sessions/revoke. End every session this
// account has, everywhere.
//
// One button over a mechanism that has always existed: bumping token_version
// invalidates every outstanding JWT at once, and it already happens on a
// demotion, a group change and an administrator's lock. Nothing let a person do
// it to themselves after losing a laptop.
//
// The bump and its audit row are one transaction — an authorization-relevant
// mutation records who performed it, or it does not happen. The caller's own
// cookies are cleared on the way out, because the session pressing the button
// is one of the sessions it ends and leaving the browser holding a token that
// now fails every request is a worse answer than landing on the login page.
//
// **Denied to a personal API key**: a key never manages credentials, which is
// the rule keyroutes.go exists to hold.
func (s *Server) handleRevokeMySessions(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	if err := s.db.RevokeAllSessions(p.ID, audit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	s.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleMyAccess — GET /me/access. The self-scoped half of
// GET /users/{id}/access: which apps the caller can reach, the role they hold
// on each, and the source of it — owned, owned via a group, granted directly,
// or granted to a group they belong to.
//
// It reads the same builder the administrator's review reads, so the answer a
// person gets about themselves and the answer an auditor gets about them are
// the same answer. This route does not re-derive reach from grants; if it did,
// the two would drift and the access review would be the one that was wrong.
//
// Allowed to a personal API key: a script checking what it can reach before
// acting on it is the intended use, and the reach it reports is the key
// owner's own.
func (s *Server) handleMyAccess(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// The principal carries a role and groups from the token, but the report is
	// built from the row: a session minted before a demotion should not make
	// this page overstate what its holder may do.
	user, err := s.db.GetUser(p.ID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "sign in again, with toft login or through the browser"})
		return
	}

	report, err := s.buildAccessReport(user)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, report)
}
