package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// transferRequest names the new owning principal.
//
// NewOwnerType is optional and defaults to "user", which is what keeps every
// existing client working: the CLI, the UI before it learned about groups, and
// anything anyone scripted against this route all send a bare new_owner_id and
// all still mean a person. An install with no groups never has to say the word.
type transferRequest struct {
	NewOwnerType string `json:"new_owner_type"`
	NewOwnerID   string `json:"new_owner_id"`
}

func (s *Server) handleTransferOwnership(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionTransferOwnership, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	// A transfer to a group widens who owns the app, the grant PUT's bypass
	// one route further (security review 2026-11-15, LSA-2). An
	// administrator's POST /users/{id}/transfer-apps stays open for a
	// leaver's locked apps.
	if s.refuseIfLocked(w, app) {
		return
	}

	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewOwnerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "new_owner_id required", "hint": `send the new owner's id, as {"new_owner_id": "<user or group id>"} — "new_owner_type": "user" or "group" is optional`})
		return
	}

	ownerType, refusal := s.resolveOwnerTarget(req.NewOwnerType, req.NewOwnerID)
	if refusal != nil {
		refusal.write(w)
		return
	}

	// 409 is for the one refusal the database makes on purpose — the target
	// already owns the app. Anything else is the server's own failure and says
	// so, because a client that treats a conflict as final does not retry.
	if err := s.db.TransferOwnership(r.Context(), appID, ownerType, req.NewOwnerID, p.ID, requestMeta(r.Context())); err != nil {
		if errors.Is(err, database.ErrAlreadyOwner) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "that principal already owns this app", "hint": "nothing to do; name a different owner to move it"})
			return
		}
		if writeStorageNotAttachableByNewOwner(w, err) {
			return
		}
		log.Printf("transfer ownership of %s: %v", appID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeStorageNotAttachableByNewOwner is the 409 a transfer answers when the
// new owner cannot attach storage the app's toft.toml declares: a manifest
// mapping is held by the owner, so it moves with the owner or not at all.
func writeStorageNotAttachableByNewOwner(w http.ResponseWriter, err error) bool {
	var missing *database.StorageNotAttachableByNewOwnerError
	if !errors.As(err, &missing) {
		return false
	}
	names := make([]string, 0, len(missing.Missing))
	for _, m := range missing.Missing {
		name := m.Resource
		if m.Folder != "" {
			name += "/" + m.Folder
		}
		names = append(names, fmt.Sprintf("%s (%s)", name, m.Access))
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": fmt.Sprintf("this app's toft.toml declares storage the new owner cannot attach: %s. "+
			"Ask an administrator to let the new owner attach it, or remove it from toft.toml and deploy first",
			strings.Join(names, ", ")),
		"code":    "storage_not_attachable_by_new_owner",
		"storage": missing.Missing, "hint": "ask an administrator to let the new owner attach that storage, or remove it from toft.toml and deploy, then transfer again",
	})
	return true
}

// resolveOwnerTarget validates a would-be owning principal and returns the
// principal type to write, or a message explaining the refusal.
//
// One function rather than a check at each call site, because there are three —
// transfer, bulk transfer-apps, and app creation with a deploy key — and the
// eligibility rules are the same at all of them. A target that could be
// accepted by one and refused by another is how an app ends up owned by a group
// that cannot administer it.
//
// The rules, and each is somebody being unable to act rather than a matter of
// taste: a user who is a system viewer can never hold owner (the clamp in
// auth.EffectiveObjectRole would downgrade them the moment they tried), a
// locked user cannot sign in to try, and a group with no member who is both
// unlocked and not a system viewer is a group whose apps nobody can administer.
//
// The fourth rule is not about ability but about authority: a group whose
// membership comes from a directory may not own an app (ADR §51). Every path
// that makes a group an owner runs through here — transfer, bulk transfer-apps,
// `POST /apps` and the upload create — so **that is four enforcement points and
// one implementation**, with `AddGroupClaimMapping` refusing the same rule from
// the other end, on a group that already owns.
func (s *Server) resolveOwnerTarget(ownerType, ownerID string) (resolved string, refusal *ownerRefusal) {
	if ownerType == "" {
		// A bare id still means a person, which is what every older client
		// sends — but a group's id is a UUID no user carries, so a bare group
		// id is a group rather than "target user not found" (BB-82).
		ownerType = database.PrincipalTypeUser
		if u, err := s.db.GetUser(ownerID); err != nil || u == nil {
			if g, err := s.db.GetGroup(ownerID); err == nil && g != nil {
				ownerType = database.PrincipalTypeGroup
			}
		}
	}

	switch ownerType {
	case database.PrincipalTypeUser:
		target, err := s.db.GetUser(ownerID)
		if err != nil || target == nil {
			return "", &ownerRefusal{"owner_not_found", "target user not found", ownerListHint}
		}
		if target.SystemRole == database.SystemRoleViewer {
			return "", &ownerRefusal{"owner_is_viewer", "viewer-system-role users cannot hold owner object role", "name a user above the viewer role, or a group — or have an administrator raise their role first"}
		}
		if target.Locked() {
			return "", &ownerRefusal{"owner_user_locked", "locked users cannot receive ownership", "name an unlocked user or a group — or have an administrator unlock the account first"}
		}
		return database.PrincipalTypeUser, nil

	case database.PrincipalTypeGroup:
		group, err := s.db.GetGroup(ownerID)
		if err != nil || group == nil {
			return "", &ownerRefusal{"owner_not_found", "target group not found", ownerListHint}
		}
		// A group whose membership comes from a directory may hold grants and
		// may not own. ADR §51 permitted claim mapping on exactly that
		// condition: a directory administrator may widen who reads an app and
		// may not take away the ability to undo it. The other enforcement point
		// is attaching a mapping, which refuses a group that already owns apps
		// — two orders of events, two refusals, and the pair is the rule.
		mapped, err := s.db.GroupIsMapped(ownerID)
		if err != nil {
			return "", &ownerRefusal{"owner_unchecked", "target group could not be checked", "try again; the group could not be read"}
		}
		if mapped {
			return "", &ownerRefusal{"owner_group_mapped", fmt.Sprintf("%s draws its membership from a directory, so it cannot own an app. "+
				"Grant it access instead, or transfer to a group an administrator manages", group.Name),
				"name a group an administrator manages, not one mapped from a directory — or grant this one access instead"}
		}
		n, err := s.db.CountEligibleGroupMembers(ownerID, "")
		if err != nil {
			return "", &ownerRefusal{"owner_unchecked", "target group could not be checked", "try again; the group could not be read"}
		}
		if n == 0 {
			// Two different fixes, so two sentences: an empty group needs a
			// member, a group of locked users and viewers needs one who is
			// neither. Telling the first that its members are locked sends
			// someone looking for members that are not there.
			if members, err := s.db.ListGroupMembers(ownerID); err == nil && len(members) == 0 {
				return "", &ownerRefusal{"owner_group_empty", fmt.Sprintf("%s has no members, so nobody could administer the app — "+
					"add a member to it first", group.Name),
					"have someone who manages the group add a member (PUT /api/v1/groups/{id}/members/{userId}, or the group's page), or name a group that has one"}
			}
			return "", &ownerRefusal{"owner_group_no_eligible_member", fmt.Sprintf("%s has no member who could administer the app — "+
				"every member is either locked or a system viewer", group.Name),
				"add an unlocked member above the viewer role to the group, or name a group that has one"}
		}
		return database.PrincipalTypeGroup, nil
	}

	return "", &ownerRefusal{"invalid_owner_type", "new_owner_type must be user or group", `send "user" or "group" as the owner type`}
}

// ownerRefusal is why resolveOwnerTarget will not let a principal own an app:
// a code of its own for each rule, so a caller is not handed the envelope's
// generic `invalid` for a group with no members (BB-019), and the next step.
type ownerRefusal struct {
	Code, Msg, Hint string
}

const ownerListHint = "name a group, or an unlocked user above the viewer role — GET /api/v1/directory/users?q=<name> and GET /api/v1/directory/groups?q=<name> find them (q is at least two characters)"

// write answers the refusal, 422 as every owner refusal always was.
func (o *ownerRefusal) write(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": o.Msg, "code": o.Code, "hint": o.Hint})
}
