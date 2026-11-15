package auth

import "errors"

// Action represents an authorization-relevant operation.
type Action string

const (
	ActionRead                 Action = "read"
	ActionEditContent          Action = "edit_content"
	ActionDelete               Action = "delete"
	ActionManageGrants         Action = "manage_grants"
	ActionChangeVisibility     Action = "change_visibility"
	ActionTransferOwnership    Action = "transfer_ownership"
	ActionApproveAccessRequest Action = "approve_access_request"
	ActionPublish              Action = "publish"
)

// The object-role ladder, in precedence order. These are the values persisted
// in object_grants.role and handed back as Decision.EffectiveRole, and
// internal/database aliases them rather than spelling them a second time — so
// the ladder has exactly one spelling anywhere in Go.
//
// They live in this package because this is where they are *decided with*:
// matrixAllows compares against them, and a raw literal that drifts from one of
// them does not fail loudly, it silently changes an authorization answer.
//
// Not to be confused with the system roles (system_admin | publisher | viewer),
// which are a different axis that happens to share the word "viewer".
const (
	ObjectRoleOwner  = "owner"
	ObjectRoleEditor = "editor"
	ObjectRoleViewer = "viewer"
	// ObjectRoleList is the rung below every way of reading, including
	// anonymous: the object appears in your listings and you may not open it.
	//
	// It sits below "anonymous" rather than beside "viewer" because the rungs
	// under viewer are not authority, they are how read was obtained — and
	// `list` is how read was *not* obtained. matrixAllows needs no branch for
	// it: ActionRead is `>= anonymous`, and one integer below that is the whole
	// enforcement.
	ObjectRoleList = "list"
)

// noObjectActions are the only actions meaningful without a target object;
// everything else must name what it acts on (see step 4 of Authorize).
var noObjectActions = map[Action]bool{
	ActionPublish: true,
}

// Object is the subject of an authorization check. Pass nil for no-object actions.
type Object struct {
	ID         string
	OwnerID    string
	Visibility string // public | authenticated | private
	// ListableToAll is the install-wide `list` rung: an administrator has said
	// every signed-in person may see that this app exists. It is a **floor**,
	// not a substitution — it can only raise `none` to [ObjectRoleList], never
	// lower or replace anything a grant conferred.
	//
	// It is carried on the object rather than read from settings inside the
	// authorizer for the reason the anonymity suppressors are: the listing and
	// the refusal are two seams that must agree, and the only way they agree is
	// by reading one fact computed in one place. An app that appears in
	// somebody's list and 404s when they open it is the exact disagreement.
	ListableToAll bool
}

// Decision is the result of an authorization check.
type Decision struct {
	Allow         bool
	EffectiveRole string // owner|editor|viewer|authenticated|anonymous|list|none
	// Reason is one of: allow, deny_no_principal, deny_system_role,
	// deny_user_only, deny_no_read, deny_list_only, deny_owner_only,
	// deny_no_action.
	Reason string
}

// RoleResolver resolves grants for (object, user|group|service_account principals).
// Passed in so authorize() stays pure and testable.
type RoleResolver interface {
	EffectiveRole(objectID, principalType, principalID string, groups []string) (string, error)
}

// Authorize implements the 7-step algorithm from the PRD.
//
// Precedence of effective role: owner > editor > viewer > authenticated > anonymous > none.
// Viewer-exclusion rule: a principal with system_role == viewer cannot hold a
// write-bearing object role; downgrade owner/editor → viewer at read time.
func Authorize(p *Principal, action Action, obj *Object, resolver RoleResolver) Decision {
	// Step 1: system_admin bypass.
	//
	// **This was retired on 2026-08-31 and put back the same day.** ADR §90 D10
	// argued that an administrator's read of an app is the one authority in this
	// product that is a property of the principal rather than a row somebody can
	// read — true, and worth fixing — and D11 claimed the change reached exactly
	// one surface: an administrator following a link into a private app's detail
	// page. It reaches at least four. Three tests state the opposite decision in
	// their own words, and each is right:
	//
	//   - viewer identity (app_viewer_handlers_test.go): testing the ladder
	//     instead of the gate "would answer *no viewer* to the one person with
	//     full authority over the app";
	//   - the gallery's installed list (gallery_handlers_test.go): derived from
	//     what the caller may read, so an administrator saw one of two copies;
	//   - the knowledge base (knowledge_base_handlers_test.go): the pointer
	//     resolves through the gate that decides who may read the app, so an
	//     administrator could not resolve the app they themselves picked.
	//
	// The fix is not to give each of those its own opinion about administrators
	// — that is the bypass scattered into call sites, which is what one
	// entrypoint exists to prevent. It is a decision about how wide the retirement
	// is meant to be, and it is recorded in docs/work-remaining.md rather than
	// guessed at here.
	if p != nil && p.SystemRole == "system_admin" {
		role := "system_admin"
		if obj != nil {
			role = ObjectRoleOwner // admin has effective owner for introspection purposes
		}
		return Decision{Allow: true, EffectiveRole: role, Reason: "allow"}
	}

	// Step 1a: a deploy key. Possession is the authorization, and the bound is
	// the route policy rather than a role.
	//
	// This is the one principal in the system whose permissions are not derived
	// from a relationship to the object, so it is worth being blunt about what
	// it grants: **a deploy key acts as owner on every app.** That is what an
	// install-scoped promotion credential is — a pipeline deploys apps it did
	// not create and whose owners it has never heard of — and pretending
	// otherwise by inventing a relationship would only move the grant somewhere
	// harder to read.
	//
	// What keeps it bounded is `deployKeyAllowedRoutes` in internal/api: a
	// strict subset of what a personal key may do — create, upload, deploy,
	// roll back, restart, stop, read history — and nothing that manages a
	// credential or changes who may do what. TestEveryRouteIsClassified walks
	// the router against all three maps, so the fail-closed property extends to
	// this class by the mechanism that already exists.
	//
	// It never reaches an object it should not, because it never reaches a
	// route that would hand it one. If that list is ever widened carelessly,
	// this is the line that made it dangerous — which is exactly why the grant
	// is stated here rather than left implicit in a middleware.
	if p != nil && p.IsDeployKey() {
		if obj != nil {
			return Decision{Allow: true, EffectiveRole: ObjectRoleOwner, Reason: "allow_deploy_key"}
		}
		if noObjectActions[action] {
			return Decision{Allow: true, Reason: "allow_deploy_key"}
		}
		return Decision{Allow: false, Reason: "deny_requires_object"}
	}

	// Step 2: system-role capability check.
	if !systemRoleAllows(p, action) {
		return Decision{Allow: false, Reason: "deny_system_role"}
	}

	// Step 3: user-only action guard.
	if isUserOnlyAction(action) && p != nil && p.Type == "service_account" {
		return Decision{Allow: false, Reason: "deny_user_only"}
	}

	// Step 4: no target object → allow only for actions that are defined
	// without one. A blanket allow here would make Authorize(p, ActionDelete,
	// nil, …) succeed for any authenticated principal on a future call site
	// that forgets its object.
	if obj == nil {
		if p == nil {
			return Decision{Allow: false, Reason: "deny_no_principal"}
		}
		if !noObjectActions[action] {
			return Decision{Allow: false, Reason: "deny_requires_object"}
		}
		return Decision{Allow: true, Reason: "allow"}
	}

	// Steps 5–6: compute effective role.
	effective := EffectiveObjectRole(p, obj, resolver)

	// The install-wide rung, as a floor under what the ladder just said.
	// Signed-in only: it is a statement about people who can sign in, and there
	// is nothing below `none` for it to raise an anonymous caller from.
	//
	// **Here rather than inside EffectiveObjectRole**, which is what ADR §90 and
	// the invariants have always said and what the code did not do. That
	// function answers what a principal's explicit relationships confer — the
	// access review reports it, and GrantedRole refuses `list` on the strength
	// of it — and an install-wide fact about listings is neither. Folding it in
	// there made an administrator with no grant resolve as a list holder, which
	// GrantedRole then refused, which stopped them opening any private app
	// through the proxy.
	//
	// Nothing about any decision changes by moving it: it is applied before the
	// matrix, so `effective` reaching matrixAllows and writeAuthzError's
	// deny_list_only branch is exactly what it was.
	if obj != nil && obj.ListableToAll && p != nil && p.ID != "" &&
		rolePrecedenceStr(ObjectRoleList) > rolePrecedenceStr(effective) {
		effective = ObjectRoleList
	}

	// Step 7: matrix lookup.
	allowed := matrixAllows(effective, action, p)
	reason := "allow"
	if !allowed {
		switch {
		case effective == ObjectRoleList:
			// The one refusal that says which object it is about, and the only
			// place in this function that distinguishes *cannot see it* from
			// *has been told it exists*. Upstream answers 403 rather than 404
			// on this reason alone; the bound on that exception is that nothing
			// reaches it without a grant somebody deliberately wrote.
			reason = "deny_list_only"
		case rolePrecedenceStr(effective) < rolePrecedenceStr(ObjectRoleViewer) && rolePrecedenceStr(effective) <= rolePrecedenceStr("anonymous"):
			// Can't read at all, and it stays "deny_no_read" whatever the
			// action was: it still means 404 upstream.
			reason = "deny_no_read"
		case matrixAllows(ObjectRoleOwner, action, p) && !matrixAllows(ObjectRoleEditor, action, p):
			// Deleting, changing who can see it, managing its grants: the
			// owner's (or an administrator's) and nobody else's. A refusal
			// worded for deny_no_action sends the caller to ask for editor,
			// which an editor already holds and which would not help a
			// viewer either (black-box BB-32, 2026-11-05).
			reason = "deny_owner_only"
		default:
			reason = "deny_no_action"
		}
	}
	return Decision{Allow: allowed, EffectiveRole: effective, Reason: reason}
}

// EffectiveObjectRole computes the principal's effective role on obj from
// ownership, grants, and visibility, including the viewer-exclusion downgrade
// (steps 5–6 of the PRD algorithm). It deliberately has no system_admin
// bypass — the access-review report uses it to show what a user's explicit
// relationships confer, and Authorize applies the bypass before calling it.
func EffectiveObjectRole(p *Principal, obj *Object, resolver RoleResolver) string {
	effective := "none"

	// Owner denormalized check (apps.owner_user_id).
	//
	// OwnerID is the owning *user*, and empty when a group owns the object — so
	// this simply does not fire for a group-owned app, which is correct. A
	// group owner is not a special case here; it arrives below as an ordinary
	// owner grant, which is why group ownership changed nothing in this
	// function.
	if p != nil && p.Type == "user" && p.ID != "" && obj.OwnerID == p.ID {
		effective = ObjectRoleOwner
	}

	// Direct + group grants via resolver.
	if p != nil && p.ID != "" && resolver != nil {
		role, _ := resolver.EffectiveRole(obj.ID, p.Type, p.ID, p.Groups)
		if rolePrecedenceStr(role) > rolePrecedenceStr(effective) {
			effective = role
		}
	}

	// **The install-wide rung is not applied here**, and it used to be. It is a
	// floor in Authorize instead — which is where ADR §90 and the invariants
	// always said it was, and where the code had drifted from.
	//
	// The drift was invisible while the switch defaulted off and became three
	// bugs the day it defaulted on (2026-08-31). This function is documented as
	// what a principal's **explicit relationships** confer, and three callers
	// rely on exactly that reading: GrantedRole refuses `list` by design, so an
	// administrator with no grant on a private app — who reads by the bypass and
	// resolves to `none` here — became `list` and made CallerRole refuse to
	// forward their request to the workload, and the viewer-identity route
	// answer 500. An install-wide fact about *listings* had been folded into the
	// one function whose whole subject is what somebody was individually given.
	//
	// The floor's effect on every authorization decision is unchanged: Authorize
	// applies it immediately after calling this, before the matrix.

	// Visibility-based effective role.
	authenticated := p != nil && p.ID != ""
	if obj.Visibility == "authenticated" && authenticated {
		if rolePrecedenceStr("authenticated") > rolePrecedenceStr(effective) {
			effective = "authenticated"
		}
	}
	if obj.Visibility == "public" {
		if rolePrecedenceStr("anonymous") > rolePrecedenceStr(effective) {
			effective = "anonymous"
		}
	}

	// Viewer-exclusion downgrade. The "viewer" being compared on the left is the
	// *system* role and the one assigned on the right is the *object* role; they
	// share a spelling and are not the same axis.
	if p != nil && p.SystemRole == "viewer" && (effective == ObjectRoleOwner || effective == ObjectRoleEditor) {
		effective = ObjectRoleViewer
	}

	return effective
}

// GrantedRole collapses EffectiveObjectRole's seven values to the three an app
// is told, and is the only place that mapping exists.
//
// Six of the seven split into two groups and only one of them is an app's
// business.
// `owner`, `editor` and `viewer` are exactly what the Sharing tab shows a human,
// and all three survive verbatim — an app reporting `owner` for somebody the UI
// labels *Editor* is a mismatch nobody can debug, which is why this mirrors the
// product's words rather than inventing better ones.
//
// `authenticated`, `anonymous` and `none` are not tiers of authority at all.
// They record *how* read access was obtained, which is the platform's own
// bookkeeping, and all three mean the same thing to an app: this person can see
// it and holds no grant. They become `viewer`.
//
// `none` is the case worth stating, because it looks like a bug and is not. This
// function is only ever called about somebody who has already passed CanRead,
// and EffectiveObjectRole has no system_admin bypass by design — so an
// administrator with no grant on a private app lands here as `none` while
// Authorize said yes. Reporting them as `viewer` is the literal truth (no grant)
// and the `is_admin` field beside it carries the rest (administers the install).
//
// The seventh, [ObjectRoleList], is the one value this function refuses. A list
// holder cannot read the object, so they cannot reach a workload, so they can
// never legitimately arrive here — and one arriving means something upstream let
// them through. Collapsing them to `viewer` like the other four would tell an
// app that somebody who may not open this app is one of its readers, which is
// the exact lie the caller-role header exists not to tell. So this returns an
// error and its callers fail loudly: the gateway refuses the request rather than
// forwarding it, and nothing is minted.
var ErrRoleNotGranted = errors.New("effective role grants no access to the object")

func GrantedRole(effective string) (string, error) {
	switch effective {
	case ObjectRoleOwner, ObjectRoleEditor:
		return effective, nil
	case ObjectRoleList:
		return "", ErrRoleNotGranted
	default:
		return ObjectRoleViewer, nil
	}
}

// CanRead is a convenience used by handlers to decide 404 vs 403.
func CanRead(p *Principal, obj *Object, resolver RoleResolver) bool {
	return Authorize(p, ActionRead, obj, resolver).Allow
}

func systemRoleAllows(p *Principal, a Action) bool {
	if p == nil {
		// Anonymous — only public reads allowed; further gated by matrix.
		return a == ActionRead
	}
	if a == ActionPublish && p.SystemRole != "publisher" && p.SystemRole != "system_admin" {
		return false
	}
	return true
}

func isUserOnlyAction(a Action) bool {
	switch a {
	case ActionTransferOwnership, ActionApproveAccessRequest:
		return true
	}
	return false
}

// rolePrecedenceStr orders the ladder. The values are ordinal and are never
// persisted — object_grants.role stores the word — so the only thing that
// matters about them is their order and the room between them.
//
// The gaps are deliberate. ObjectRoleList had to go strictly between "none" and
// "anonymous", and the adjacent integers this used to return had nothing there;
// a renumbering that has to happen a second time is one nobody trusts, so the
// rungs are spaced by ten and the next one has somewhere to land.
//
// The single most dangerous line in this file is ObjectRoleList's. If it ever
// returns a value at or above "anonymous", every private object in the install
// becomes readable to everyone holding a list grant, and no happy-path test
// notices. TestListSatisfiesNoAction is what stands in front of that.
// RolePrecedence is rolePrecedenceStr for callers outside this package. It
// exists because internal/database has to compare two grant words to pick the
// stronger, and a second ladder over there — which is what used to be there —
// is a second answer to an authorization question that nothing keeps in step
// with this one. It had already drifted: it ranked three rungs and knew nothing
// of the fourth, so a list grant resolved to the empty role and a list holder
// looked exactly like a stranger.
func RolePrecedence(r string) int { return rolePrecedenceStr(r) }

func rolePrecedenceStr(r string) int {
	switch r {
	case ObjectRoleOwner:
		return 50
	case ObjectRoleEditor:
		return 40
	case ObjectRoleViewer:
		return 30
	case "authenticated":
		return 20
	case "anonymous":
		return 10
	case ObjectRoleList:
		return 5
	}
	return 0
}

// matrixAllows implements §7 of the PRD.
func matrixAllows(effective string, a Action, p *Principal) bool {
	switch a {
	case ActionRead:
		return rolePrecedenceStr(effective) >= rolePrecedenceStr("anonymous")
	case ActionEditContent:
		return effective == ObjectRoleOwner || effective == ObjectRoleEditor
	case ActionDelete, ActionManageGrants, ActionChangeVisibility:
		return effective == ObjectRoleOwner
	case ActionTransferOwnership, ActionApproveAccessRequest:
		// Owner-only; user-only handled in step 3. system_admin already short-circuited.
		return effective == ObjectRoleOwner
	case ActionPublish:
		// Requires publisher system role (checked in systemRoleAllows) plus owner object role.
		return effective == ObjectRoleOwner
	}
	return false
}
