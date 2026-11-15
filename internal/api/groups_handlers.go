package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	netmail "net/mail"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/authprovider"
	"toft/internal/database"
	"toft/internal/entitlements"
)

type createGroupRequest struct {
	Name string `json:"name"`
}

// maxGroupNameLen bounds what goes in `groups.name`. Long enough for a real
// team name with a department in front of it, short enough that the column
// cannot become a place to store a paragraph — and it is the same figure on
// both doors into that column, which is the only property that matters.
const maxGroupNameLen = 120

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	gs, err := s.db.ListGroups()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if gs == nil {
		gs = []database.GroupWithCount{}
	}
	writeJSON(w, http.StatusOK, gs)
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin && p.SystemRole != database.SystemRolePublisher {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator for the publisher role: POST /api/v1/me/promotion-requests"})
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureGroups) {
		return
	}
	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required", "hint": "send the group's name as JSON, like {\"name\": \"Finance\"}"})
		return
	}
	// Trimmed here as well as on the rename, so the two doors into
	// `groups.name` cannot disagree about what "  Finance " is.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required", "hint": "send the group's name as JSON, like {\"name\": \"Finance\"}"})
		return
	}
	if len(name) > maxGroupNameLen {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("a group name is at most %d characters", maxGroupNameLen), "hint": "choose a shorter name"})
		return
	}
	g, err := s.db.CreateGroup(r.Context(), name, p.ID, requestMeta(r.Context()))
	if errors.Is(err, database.ErrGroupNameTaken) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Another group is already called that.", "hint": "choose another name, or add people to the group that already has it"})
		return
	}
	if err != nil {
		log.Printf("create group: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// A publisher who makes a group is its first member: roster writes need
	// membership (canWriteGroupRoster), and a creator locked out of the group
	// they just made could not add anyone to it.
	if p.SystemRole == database.SystemRolePublisher {
		if err := s.db.AddGroupMember(r.Context(), g.ID, p.ID, p.ID, requestMeta(r.Context())); err != nil &&
			!errors.Is(err, database.ErrGroupCapReached) {
			log.Printf("create group: adding creator %s: %v", p.ID, err)
		}
		s.keepOwnSession(w, r, p.ID)
	}
	writeJSON(w, http.StatusCreated, g)
}

// handleGetGroup returns a group and, to those entitled to see it, its roster.
//
// It previously had no principal check at all — every sibling handler in this
// file starts with one — so an anonymous caller holding a group id got the full
// membership including email addresses and system roles. Group ids are
// enumerable by any authenticated user via handleListGroups.
func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	id := chi.URLParam(r, "id")
	g, err := s.db.GetGroup(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if g == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/groups lists the groups and their ids"})
		return
	}

	out := map[string]interface{}{"group": g}

	// The roster is for the group's own members and for admins. Everyone else
	// gets the group's existence — which handleListGroups already gives them —
	// and nothing about who is in it.
	if s.canSeeGroupRoster(p, id) {
		members, _ := s.db.ListGroupMembers(id)
		if members == nil {
			members = []database.GroupMember{}
		}
		if !p.IsAdmin() {
			// Email is directory data; a fellow group member has no need of it.
			for i := range members {
				members[i].Email = ""
			}
		}
		out["members"] = members
	}

	writeJSON(w, http.StatusOK, out)
}

// canSeeGroupRoster reports whether the principal may read a group's membership.
// Membership is read from the database rather than the token's claims, which go
// stale for up to the token lifetime.
func (s *Server) canSeeGroupRoster(p *auth.Principal, groupID string) bool {
	if p.IsAdmin() {
		return true
	}
	ids, err := s.db.GetUserGroupIDs(p.ID)
	if err != nil {
		return false
	}
	for _, id := range ids {
		if id == groupID {
			return true
		}
	}
	return false
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to delete the group"})
		return
	}
	id := chi.URLParam(r, "id")
	g, _ := s.db.GetGroup(id)
	if g == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/groups lists the groups and their ids"})
		return
	}
	if err := s.db.DeleteGroup(r.Context(), id, p.ID, requestMeta(r.Context())); err != nil {
		// §9.1, and it mirrors handleDeleteUser's refusal word for word on
		// purpose: the hazard is identical, so the sentence should be too.
		if errors.Is(err, database.ErrGroupOwnsApps) {
			n, _ := s.db.CountAppsOwnedByGroup(id)
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("%s owns %d app%s — transfer them to a person or another group first",
					g.Name, n, plural(n)),
				"code":      "owns_apps",
				"owns_apps": fmt.Sprint(n),
				"hint":      "move each app with POST /api/v1/apps/{id}/transfer, then delete the group",
			})
			return
		}
		if writeSubjectHoldsMappings(w, g.Name, err) {
			return
		}
		log.Printf("delete group %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// For an administrator who was a member, whose session the delete's bump
	// just revoked along with everyone else's; for one who was not, a renewal.
	s.keepOwnSession(w, r, p.ID)
	w.WriteHeader(http.StatusNoContent)
}

type updateGroupRequest struct {
	// Email is a pointer so that "not supplied" and "cleared" are different
	// requests. Absent leaves the address alone; "" clears it, which puts the
	// group back on member expansion rather than silencing its notifications.
	Email *string `json:"email"`

	// Name is a pointer for the same reason, and for one more: empty is not a
	// meaningful value here the way it is above. A group with no name cannot be
	// picked out of a list, granted to, or told apart from another one — the id
	// is not a name — so a blank is refused rather than stored.
	Name *string `json:"name"`
}

// handleUpdateGroup — PATCH /groups/{id}. system_admin only.
//
// The address is an administrative fact about how the install reaches a team,
// not something a member sets about themselves, and it is the kind of thing
// that quietly redirects failure mail — so it sits with the other admin-only
// group operations rather than with the roster.
func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to change the group"})
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureGroups) {
		return
	}
	id := chi.URLParam(r, "id")
	g, _ := s.db.GetGroup(id)
	if g == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/groups lists the groups and their ids"})
		return
	}

	var req updateGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	if req.Email == nil && req.Name == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to update", "hint": "send name, email or both, like {\"name\": \"Finance\"}"})
		return
	}

	// The rename first, and each field through its own setter with its own
	// audit row — two facts about a group, changed in one request or in two,
	// and an audit trail that says which of them moved either way.
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "a group needs a name", "hint": "send a name with at least one character that is not a space, or leave name out to keep it"})
			return
		}
		if len(name) > maxGroupNameLen {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": fmt.Sprintf("a group name is at most %d characters", maxGroupNameLen), "hint": "choose a shorter name"})
			return
		}
		if name != g.Name {
			// Unchanged is not a rename: it writes nothing and audits nothing,
			// for the reason a re-sent visibility does not — "who renamed this
			// group" is a question an identical before and after cannot answer.
			switch err := s.db.SetGroupName(r.Context(), id, name, p.ID, requestMeta(r.Context())); {
			case errors.Is(err, database.ErrGroupNameTaken):
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "Another group is already called that.", "hint": "choose another name"})
				return
			case err != nil:
				log.Printf("rename group %s: %v", id, err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}
	}

	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		// Parsed, not just checked for an "@": a list such as "a@x.org, b@x.org"
		// fails go-mail's parse when the notice is sent, and before VN-2 lost the
		// mail for every recipient of it. The admin group address is held to the
		// same rule.
		if _, perr := netmail.ParseAddress(email); email != "" && perr != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "that does not look like an address", "hint": "send an email address like finance@example.com, or an empty string to clear it"})
			return
		}
		if err := s.db.SetGroupEmail(r.Context(), id, email, p.ID, requestMeta(r.Context())); err != nil {
			log.Printf("set group email %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	updated, _ := s.db.GetGroup(id)
	writeJSON(w, http.StatusOK, updated)
}

// canWriteGroupRoster reports whether the caller may change this group's
// membership. Membership is authorization — a group can own apps and hold
// editor grants — so a roster write needs at least the authority it confers:
// an administrator, or a publisher who is already a member. Read from the
// database, not the token's claims, which can be stale.
func (s *Server) canWriteGroupRoster(p *auth.Principal, groupID string) bool {
	if p.IsAdmin() {
		return true
	}
	if p.SystemRole != database.SystemRolePublisher {
		return false
	}
	return s.canSeeGroupRoster(p, groupID)
}

func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin && p.SystemRole != database.SystemRolePublisher {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator for the publisher role: POST /api/v1/me/promotion-requests"})
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureGroups) {
		return
	}
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")
	if !s.canWriteGroupRoster(p, id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found", "hint": "GET /api/v1/groups lists the groups and their ids"})
		return
	}

	// Group must exist and user must exist.
	if g, _ := s.db.GetGroup(id); g == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found", "hint": "GET /api/v1/groups lists the groups and their ids"})
		return
	}
	if u, _ := s.db.GetUser(userID); u == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "user not found", "hint": "GET /api/v1/directory/users finds a person and their id"})
		return
	}

	err := s.db.AddGroupMember(r.Context(), id, userID, p.ID, requestMeta(r.Context()))
	if errors.Is(err, database.ErrGroupCapReached) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "user is already in 10 groups", "hint": "remove the person from a group they no longer need first, with DELETE /api/v1/groups/{id}/members/{userId}"})
		return
	}
	if err != nil {
		log.Printf("add group member: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.keepOwnSession(w, r, userID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin && p.SystemRole != database.SystemRolePublisher {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator for the publisher role: POST /api/v1/me/promotion-requests"})
		return
	}
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")
	if !s.canWriteGroupRoster(p, id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found", "hint": "GET /api/v1/groups lists the groups and their ids"})
		return
	}
	if err := s.db.RemoveGroupMember(r.Context(), id, userID, p.ID, requestMeta(r.Context())); err != nil {
		// §9.2. Refused rather than allowed-with-a-warning: the state it
		// prevents — apps owned by a group nobody in it can act for — is not
		// visible anywhere until somebody needs to deploy one.
		if errors.Is(err, database.ErrGroupWouldBeEmpty) {
			n, _ := s.db.CountAppsOwnedByGroup(id)
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("that is the last member who could administer the %d app%s this group owns — "+
					"add someone else first, or transfer the apps away", n, plural(n)),
				"code":      "would_be_empty",
				"owns_apps": fmt.Sprint(n),
				"hint":      "add another member who can administer the apps first, or move the apps with POST /api/v1/apps/{id}/transfer",
			})
			return
		}
		log.Printf("remove group member %s from %s: %v", userID, id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.keepOwnSession(w, r, userID)
	w.WriteHeader(http.StatusNoContent)
}

// --- Directory claim mappings (ADR §51's revisit) ---

// requireGroupMappingAdmin narrows these three routes to system administrators.
//
// Narrower than the rest of this file, which admits publishers: a publisher may
// create a group and manage its members, and attaching a *directory* to one
// decides who is in it on every future sign-in. That is an install-shaped
// decision beside the provider it names, and the provider routes are already
// admin-only.
func (s *Server) requireGroupMappingAdmin(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, false
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to change where this group's members come from"})
		return nil, false
	}
	if g, _ := s.db.GetGroup(chi.URLParam(r, "id")); g == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "group not found", "hint": "GET /api/v1/groups lists the groups and their ids"})
		return nil, false
	}
	return p, true
}

// handleListGroupClaimMappings answers with the claim values attached to a
// group. An empty list, never a 404: a group with no mapping is an ordinary
// group, not a missing one.
func (s *Server) handleListGroupClaimMappings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGroupMappingAdmin(w, r); !ok {
		return
	}
	rows, err := s.db.ListGroupClaimMappings(chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("listing claim mappings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": rows})
}

type groupClaimMappingRequest struct {
	ProviderID string `json:"provider_id"`
	ClaimValue string `json:"claim_value"`
}

// handleAddGroupClaimMapping attaches a claim value to a group.
//
// The refusal that matters is the one for a group that owns apps, and it names
// them. ADR §51 permitted this feature on the condition that a directory
// administrator cannot become an owner of an install's apps, and this is one of
// the two places that condition is enforced — the other is the ownership
// transfer.
func (s *Server) handleAddGroupClaimMapping(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireGroupMappingAdmin(w, r)
	if !ok {
		return
	}
	if s.refusedUnlicensed(w, r, entitlements.FeatureGroups) {
		return
	}
	var body groupClaimMappingRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	body.ProviderID = strings.TrimSpace(body.ProviderID)
	body.ClaimValue = strings.TrimSpace(body.ClaimValue)
	if body.ProviderID == "" || body.ClaimValue == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "a sign-in provider and a claim value are both required",
			"hint":  "send both, like {\"provider_id\": \"<id>\", \"claim_value\": \"finance\"}; GET /api/v1/admin/auth-providers lists the providers and their ids",
		})
		return
	}

	// The provider must be one this install has, and one that can assert a
	// claim. The id was taken on trust: a mapping to `sso` on an install with
	// no `sso` was accepted, matched nobody, and kept Admin → Checks warning
	// that the IdP was not sending its claim (trytoft, 2026-10-23).
	if refusal := claimProviderRefusal(s.authProvider(body.ProviderID)); refusal != nil {
		writeJSON(w, http.StatusUnprocessableEntity, refusal)
		return
	}

	groupID := chi.URLParam(r, "id")
	err := s.db.AddGroupClaimMapping(r.Context(), groupID, body.ProviderID, body.ClaimValue, p.ID, requestMeta(r.Context()))
	if errors.Is(err, database.ErrGroupOwnsApps) {
		owned, _ := s.db.AppsOwnedByGroup(groupID)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this group owns apps, so its membership cannot come from a directory. " +
				"Transfer them to a person or to a group with no mapping first.",
			"code": "group_owns_apps",
			"apps": owned,
			"hint": "move each app named in apps with POST /api/v1/apps/{id}/transfer, then add the mapping",
		})
		return
	}
	if err != nil {
		log.Printf("adding claim mapping: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// claimProviderRefusal is why a claim mapping cannot name this provider, or nil.
// An unknown id is refused by name rather than stored to match nobody, and the
// local password sign-in is refused because it asserts no claims at all.
func claimProviderRefusal(prov *authprovider.Provider, err error) map[string]any {
	switch {
	case errors.Is(err, errNoSuchProvider):
		return map[string]any{"code": "unknown_provider", "retryable": false,
			"error": "this install has no sign-in provider with that id — a mapping to it would match nobody",
			"hint":  "use the id Admin → Sign-in lists for the provider that sends the group claim"}
	case err != nil:
		return map[string]any{"code": "unknown_provider", "retryable": false,
			"error": "the sign-in provider could not be read: " + err.Error()}
	case prov.Kind == database.ProviderKindPassword:
		return map[string]any{"code": "provider_asserts_no_claims", "retryable": false,
			"error": "the local password sign-in asserts no group claim, so nothing could ever arrive through this mapping"}
	}
	return nil
}

// handleRemoveGroupClaimMapping detaches a claim value and removes the
// memberships it wrote. See database.RemoveGroupClaimMapping for why those go
// now rather than at each person's next sign-in.
func (s *Server) handleRemoveGroupClaimMapping(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireGroupMappingAdmin(w, r)
	if !ok {
		return
	}
	providerID := strings.TrimSpace(r.URL.Query().Get("provider_id"))
	claimValue := strings.TrimSpace(r.URL.Query().Get("claim_value"))
	if providerID == "" || claimValue == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "provider_id and claim_value are both required",
			"hint":  "pass both in the query, like ?provider_id=<id>&claim_value=finance; GET /api/v1/groups/{id}/claim-mappings lists them",
		})
		return
	}
	if err := s.db.RemoveGroupClaimMapping(r.Context(), chi.URLParam(r, "id"),
		providerID, claimValue, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("removing claim mapping: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
