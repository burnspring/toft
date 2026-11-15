package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// noteLimit caps the sharer's own words. Long enough for the sentence somebody
// actually writes beside a share, short enough that this is not a channel for
// sending documents through the platform's relay.
const noteLimit = 500

type putGrantRequest struct {
	Role string `json:"role"`
	// Notify asks for the person to be emailed about this. Optional, ignored
	// for anything that is not a person, and honoured only on a **new** share —
	// see notifyGrantShared, which owns every refusal and names it.
	Notify bool `json:"notify"`
	// Note is the sharer's own words, and it goes out escaped like every other
	// piece of text somebody typed.
	Note string `json:"note"`
}

func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionManageGrants, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	grants, err := s.db.ListGrantsDetailed(appID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if grants == nil {
		grants = []database.GrantWithPrincipal{}
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) handlePutGrant(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionManageGrants, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	// A grant written under a lock is reach the app gains the moment the lock
	// lifts, the widening PATCH visibility is refused for (security review
	// 2026-11-15, LSA-2). Narrowing under a lock is DELETE, which stays open.
	if s.refuseIfLocked(w, app) {
		return
	}

	principalType := chi.URLParam(r, "principalType")
	principalID := chi.URLParam(r, "principalID")

	if principalType != database.PrincipalTypeUser && principalType != database.PrincipalTypeGroup && principalType != database.PrincipalTypeServiceAccount {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid principal_type", "hint": "pass principal_type as user, group or service_account in the path"})
		return
	}

	var req putGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}

	// Owner grants cannot be set via this API — use transfer instead.
	if req.Role == database.ObjectRoleOwner {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "owner role must be set via transfer", "hint": "transfer ownership with POST /api/v1/apps/{id}/transfer instead"})
		return
	}
	if req.Role != database.ObjectRoleEditor && req.Role != database.ObjectRoleViewer &&
		req.Role != database.ObjectRoleList {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role", "hint": `send "role" as editor, viewer or list`})
		return
	}

	// A group may hold editor now, and the write-time rejection that used
	// to sit here is gone with the resolver's clamp (ADR §51). Owner is still
	// refused above, for every principal type — ownership moves by transfer
	// only, so apps.owner_* and the owner grant row cannot desync. That is the
	// rule that survived; "groups are viewer-only" is the one that did not.

	// The owning principal's role only moves via transfer — overwriting that
	// grant row here would desync it from apps.owner_user_id/owner_group_id.
	// Tested against the owning *principal* rather than app.OwnerID, so it
	// covers the group case: without this, a member of the owning group could
	// demote their own group to editor and leave the app with a column
	// saying one thing and a grant saying another.
	if ownerType, ownerID := app.OwnerPrincipal(); principalType == ownerType && principalID == ownerID {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the owner's grant can only be changed by transferring ownership", "hint": "transfer ownership with POST /api/v1/apps/{id}/transfer to change who owns the app"})
		return
	}

	// Viewer-system-role users cannot hold write-bearing object roles (see PRD §5).
	if principalType == database.PrincipalTypeUser {
		target, _ := s.db.GetUser(principalID)
		if target == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "user not found", "hint": "GET /api/v1/directory/users lists the people you can grant to, with their ids"})
			return
		}
		if target.SystemRole == database.SystemRoleViewer && (req.Role == database.ObjectRoleEditor || req.Role == database.ObjectRoleOwner) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "viewer-system-role users cannot hold editor/owner object roles", "hint": `grant "role": "viewer" instead, or ask an administrator to give the person the publisher role`})
			return
		}
		// Locked users are invalid new-grant targets; existing grants stay
		// (they're inert while the user is locked).
		if target.Locked() {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "locked users cannot receive new grants", "hint": "ask an administrator to unlock the account first"})
			return
		}
	}

	if len(req.Note) > noteLimit {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "the note is too long — keep it under " + strconv.Itoa(noteLimit) + " characters", "hint": "shorten the note to the length the sentence names and send it again"})
		return
	}

	// Read **before** the write, because "is this a new share" is only knowable
	// from here: the upsert is the thing that stops it being one. A failure to
	// read is treated as "already shared", which is the answer that sends
	// nothing — a notification is the losable half of this request.
	existing, err := s.db.GetGrant(appID, principalType, principalID)
	if err != nil {
		slog.Warn("reading a grant before sharing", "app", appID, "error", err)
	}
	isNew := err == nil && existing == nil

	meta := requestMeta(r.Context())
	if err := s.db.UpsertGrant(r.Context(), appID, principalType, principalID, req.Role, p.ID, meta); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to upsert grant"})
		return
	}

	// After the write and never before it: the share has happened, and no
	// notification channel may change that or delay the answer saying so.
	notified, skipped := false, ""
	if req.Notify {
		notified, skipped = s.notifyGrantShared(app, principalType, principalID, req.Role, req.Note, !isNew, p.ID)
	}
	// The two extra keys are reported rather than logged, because the person
	// who ticked the box would otherwise assume a message went out. `notify`
	// absent means nobody asked, which is not the same as asked-and-skipped.
	out := map[string]any{"status": "ok", "notified": notified}
	if skipped != "" {
		out["notify_skipped"] = skipped
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionManageGrants, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	principalType := chi.URLParam(r, "principalType")
	principalID := chi.URLParam(r, "principalID")

	// The owning principal's grant can only move via transfer — deleting it
	// would leave the app with an owner column and no matching grant row, and
	// for a group-owned app that grant row is the only thing making the app
	// readable by its members at all.
	if ownerType, ownerID := app.OwnerPrincipal(); principalType == ownerType && principalID == ownerID {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "the owner's grant can only be changed by transferring ownership", "hint": "transfer ownership with POST /api/v1/apps/{id}/transfer to change who owns the app"})
		return
	}

	meta := requestMeta(r.Context())
	existed, err := s.db.DeleteGrant(r.Context(), appID, principalType, principalID, p.ID, meta)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete grant"})
		return
	}
	if !existed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "grant not found", "hint": "GET /api/v1/apps/{id}/grants lists this app's grants"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
