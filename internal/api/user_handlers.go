package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

type updateUserRoleRequest struct {
	SystemRole string `json:"system_role"`
}

// handleUpdateUserRole — PATCH /users/:id/role. system_admin only. Enforces last-admin invariant.
func (s *Server) handleUpdateUserRole(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to change this account's role"})
		return
	}

	userID := chi.URLParam(r, "id")
	var req updateUserRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	if req.SystemRole != database.SystemRoleAdmin && req.SystemRole != database.SystemRolePublisher && req.SystemRole != database.SystemRoleViewer {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid system_role", "hint": "pass system_role as viewer, publisher or system_admin"})
		return
	}

	target, err := s.db.GetUser(userID)
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
		return
	}

	tx, err := s.db.Conn().Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback()

	// The last-administrator check, inside the transaction and through the
	// one predicate the lock and the deletion use: a locked administrator
	// cannot unlock anybody, so they do not count.
	if target.SystemRole == database.SystemRoleAdmin && req.SystemRole != database.SystemRoleAdmin {
		others, err := database.OtherUnlockedAdminsTx(tx, userID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if others == 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": database.ErrLastAdmin.Error(), "hint": "make another account an administrator first, then change this one"})
			return
		}
	}

	// token_version bump revokes the target's live sessions in the same
	// transaction — otherwise a demoted admin stays admin until their JWT
	// expires, making the last-admin invariant bypassable in effect.
	if _, err := tx.Exec(`UPDATE users SET system_role=?, token_version = token_version + 1 WHERE id=?`, req.SystemRole, userID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update"})
		return
	}

	meta := requestMeta(r.Context())
	// One outcome, two doors. Promoting somebody from the directory used to
	// leave their pending publisher request in the queue for a role they now
	// held, and an administrator working the queue then approved a promotion
	// that had already happened.
	if err := database.ResolvePendingPromotionTx(tx, userID, p.ID, req.SystemRole, meta); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := database.WriteAuditTx(tx, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		Action: "user.role_change", TargetType: "user", TargetID: userID,
		Before:    jsonValStr(map[string]string{"system_role": target.SystemRole}),
		After:     jsonValStr(map[string]string{"system_role": req.SystemRole}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "commit failed"})
		return
	}

	target.SystemRole = req.SystemRole
	writeJSON(w, http.StatusOK, target)
}

// handleLockUser — POST /users/:id/lock. system_admin only. Locking is
// sessions-scoped: the user's live sessions die (token_version bump in the
// same transaction), login is refused, and they become invalid transfer/grant
// targets. Apps they own keep running and those apps' API keys keep working.
func (s *Server) handleLockUser(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to lock the account"})
		return
	}

	userID := chi.URLParam(r, "id")
	if userID == p.ID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot lock yourself", "hint": "ask another administrator to lock this account"})
		return
	}

	target, err := s.db.GetUser(userID)
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
		return
	}

	meta := requestMeta(r.Context())
	_, err = s.db.LockUser(r.Context(), userID, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	})
	switch {
	case errors.Is(err, database.ErrLastAdmin):
		writeJSON(w, http.StatusConflict, map[string]string{"error": database.ErrLastAdmin.Error(), "hint": "make another account an administrator first, then lock this one"})
		return
	case errors.Is(err, database.ErrCannotLockSelf):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot lock yourself", "hint": "ask another administrator to lock this account"})
		return
	case err != nil:
		log.Printf("locking %s: %v", userID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to lock"})
		return
	}

	updated, err := s.db.GetUser(userID)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleSetScanner — POST /users/:id/scanner (set) and DELETE (clear).
// system_admin only.
//
// The account an AppSec tool signs in as. The mark is believed for
// `X-Toft-Scan` at the gateway and admits the account to the estate read, and
// it never grants: what the account reaches is still exactly what its grants
// say. Reversible and one click, so no dialog — a toast in the UI.
func (s *Server) handleSetScanner(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.GetPrincipal(r.Context())
		if p == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if p.SystemRole != database.SystemRoleAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to mark the scanner account"})
			return
		}
		userID := chi.URLParam(r, "id")
		target, err := s.db.GetUser(userID)
		if err != nil || target == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
			return
		}

		meta := requestMeta(r.Context())
		_, err = s.db.SetScannerAccount(r.Context(), userID, on, database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: p.ID,
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
		})
		switch {
		case errors.Is(err, database.ErrCannotMarkSelf):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "mark a different account, or ask another administrator to mark yours"})
			return
		case err != nil:
			log.Printf("setting the scanner mark on %s: %v", userID, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update"})
			return
		}

		updated, err := s.db.GetUser(userID)
		if err != nil || updated == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, updated)
	}
}

type activateUserRequest struct {
	SystemRole string `json:"system_role"`
}

// handleActivateUser — POST /users/:id/activate. system_admin only.
//
// The other half of a provider whose `default_role` is `pending`: that policy
// creates the account and refuses every sign-in it makes, and this is the act
// that finishes it. Two facts in one transaction, because they are one
// decision — the account is admitted, and the role it holds is chosen now
// rather than months ago when somebody configured the provider.
//
// Viewer or publisher, never system_admin. Not because activating an
// administrator is unthinkable, but because "let this person in" and "hand this
// person the install" should not be the same click on a queue an administrator
// works through quickly. PATCH /users/{id}/role is one step away and says what
// it does.
//
// Not an unlock, and it does not become one: an account an administrator locked
// while it was pending stays locked after this, because two refusals are two
// decisions and lifting one may not lift the other.
func (s *Server) handleActivateUser(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to activate the account"})
		return
	}

	userID := chi.URLParam(r, "id")
	var req activateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	if req.SystemRole == "" {
		// The queue's own default, and the floor: an activation that forgot to
		// say a role must not be the one that guesses upwards.
		req.SystemRole = database.SystemRoleViewer
	}
	if req.SystemRole != database.SystemRoleViewer && req.SystemRole != database.SystemRolePublisher {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "system_role must be viewer or publisher — promote to administrator separately", "hint": "activate the account as viewer or publisher, then change its role with PATCH /api/v1/users/{id}/role"})
		return
	}

	target, err := s.db.GetUser(userID)
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
		return
	}
	if !target.PendingActivation() {
		// 409 rather than a quiet success: this is a queue two administrators
		// may be working at once, and "already done" is worth saying.
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this account is not awaiting activation", "hint": "GET /api/v1/users/{id} shows the account's current state"})
		return
	}

	tx, err := s.db.Conn().Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback()

	// Guarded on the column, so two administrators clicking at once produce one
	// activation and one audit row. No token_version bump: an account that has
	// never been able to sign in has no session to revoke.
	res, err := tx.Exec(`
		UPDATE users SET system_role = ?, pending_activation_at = NULL
		WHERE id = ? AND pending_activation_at IS NOT NULL`, req.SystemRole, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to activate"})
		return
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this account is not awaiting activation", "hint": "GET /api/v1/users/{id} shows the account's current state"})
		return
	}

	meta := requestMeta(r.Context())
	if err := database.WriteAuditTx(tx, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		Action: "user.activate", TargetType: "user", TargetID: userID,
		Before:    jsonValStr(map[string]any{"pending_activation": true, "system_role": target.SystemRole}),
		After:     jsonValStr(map[string]any{"pending_activation": false, "system_role": req.SystemRole}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "commit failed"})
		return
	}

	updated, err := s.db.GetUser(userID)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// After the response is decided and never able to fail it: the account is
	// active whether or not anything could be sent, which is the rule every
	// notification in this product follows.
	s.notifyAccountActivated(updated)
	writeJSON(w, http.StatusOK, updated)
}

// handleUnlockUser — POST /users/:id/unlock. system_admin only. Symmetric to
// lock; no token bump needed (the user has no live sessions to widen).
func (s *Server) handleUnlockUser(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to unlock the account"})
		return
	}

	userID := chi.URLParam(r, "id")
	target, err := s.db.GetUser(userID)
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
		return
	}

	meta := requestMeta(r.Context())
	if _, err := s.db.UnlockUser(r.Context(), userID, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		log.Printf("unlocking %s: %v", userID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to unlock"})
		return
	}

	updated, err := s.db.GetUser(userID)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteUser — DELETE /users/{id}. system_admin only, and only for an
// account that owns nothing: apps.owner_id has no cascade, and an app with no
// owner is unadministrable, so ownership must be transferred first. Locking is
// the answer for someone who owns apps and should not be able to sign in.
//
// What goes with them: their identities and group memberships (FK cascades),
// their grants, and any access request they raised — all of which are live
// authority or would resurface as a row naming nobody. What stays: the audit
// log and usage stats, deliberately. History that erases its actor is not
// history, and `app_usage_daily` carries no FK to users precisely so an app's
// traffic still adds up after someone leaves.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	userID := chi.URLParam(r, "id")
	if userID == p.ID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot delete your own account", "hint": "ask another administrator to delete this account"})
		return
	}
	// The built-in administrator is the install's guarantee that one way in always
	// exists, and the next boot re-creates it with the published default password
	// anyway — so deleting it does not remove it, it resets it. Refused, with the
	// action that does what the person probably meant.
	if userID == database.BuiltinAdminID {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the built-in administrator cannot be deleted — it is what gets you back in if " +
				"single sign-on breaks. Switch local sign-in off under Admin → Authentication instead.", "hint": "to stop it being used, switch the local provider off with PATCH /api/v1/admin/auth-providers/{id} and {\"enabled\": false}"})
		return
	}

	target, err := s.db.GetUser(userID)
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
		return
	}

	owned, err := s.db.ListAppsOwnedBy(userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if len(owned) > 0 {
		// The one place the product may propose a group to somebody who has not
		// used one: this refusal is the moment "this should not be one person's"
		// is the thought already being had, and moving the apps to a group is
		// the answer this whole feature exists for. Only mentioned when groups
		// exist — on an install with none, the sentence would be an
		// advertisement for a concept nobody has.
		hint := "transfer or delete them first"
		if groups, err := s.db.ListGroups(); err == nil && len(groups) > 0 {
			hint = "transfer them to a person or a group, or delete them first"
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("%s owns %d app%s — %s",
				target.Login, len(owned), plural(len(owned)), hint),
			// Machine-readable so the UI can offer the bulk move rather than
			// making somebody read the sentence and find the route.
			"code":      "owns_apps",
			"owns_apps": fmt.Sprint(len(owned)),
			"transfer":  fmt.Sprintf("/api/v1/users/%s/transfer-apps", userID),
			"hint":      "POST /api/v1/users/{id}/transfer-apps moves all of them at once",
		})
		return
	}

	meta := requestMeta(r.Context())
	if err := s.db.DeleteUser(userID, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		Action: "user.delete", TargetType: "user", TargetID: userID,
		Before:    jsonValStr(map[string]string{"login": target.Login, "email": target.Email, "system_role": target.SystemRole}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		if errors.Is(err, database.ErrLastAdmin) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": database.ErrLastAdmin.Error(), "hint": "make another account an administrator first, then delete this one"})
			return
		}
		if errors.Is(err, database.ErrUserOwnsApps) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "that account owns apps — transfer them first", "hint": "POST /api/v1/users/{id}/transfer-apps moves all of them at once"})
			return
		}
		if writeSubjectHoldsMappings(w, target.Login, err) {
			return
		}
		log.Printf("deleting user %s: %v", userID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete the account"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeSubjectHoldsMappings is the 409 a user or group delete answers while the
// subject's delegations stamp live mappings (ADR § Storage somebody else can
// attach): the apps named, each with the resource whose revoke clears it.
// Shared by both deletes because the hazard is one.
func writeSubjectHoldsMappings(w http.ResponseWriter, name string, err error) bool {
	var held *database.SubjectHoldsMappingsError
	if !errors.As(err, &held) {
		return false
	}
	apps := map[string]bool{}
	for _, m := range held.Mappings {
		apps[m.AppID] = true
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": fmt.Sprintf("%s's storage delegations mount storage into %d app%s — "+
			"revoke those delegations on each resource first, which restarts the apps it names",
			name, len(apps), plural(len(apps))),
		"code":     "holds_storage_mappings",
		"mappings": held.Mappings,
		"hint":     "revoke each delegation the mappings name with DELETE /api/v1/admin/storage/{id}/managers/{subject}, then delete again",
	})
	return true
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// userDetailIdentity is one external account attached to the user, named by
// its provider. The subject is deliberately absent: it is the join key, not
// something an admin needs to read, and it is never the email.
type userDetailIdentity struct {
	ProviderID   string     `json:"provider_id"`
	ProviderName string     `json:"provider_name"`
	Login        string     `json:"login"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

type userDetailResponse struct {
	// The activity-carrying row, not the bare user: it embeds User so the
	// shape is unchanged, and adds the dormancy clock. The account record is
	// where "why is this considered unused" gets asked, and answering it from
	// last_login_at alone is what this whole feature exists to stop.
	User       *database.UserActivity         `json:"user"`
	Groups     []database.UserGroupMembership `json:"groups"`
	Identities []userDetailIdentity           `json:"identities"`
	// Whether this person asked to publish, when, and who decided.
	//
	// On the response the record already fetches rather than a second query
	// from the page: an administrator promotes from a `<Select>` on this page,
	// and doing that without being able to see that a request exists — or that
	// somebody has already turned this person down, and why — is the one
	// question the queue page exists to answer, asked in the one place the
	// answer was not. Newest first; empty on an install nobody has asked on.
	PromotionRequests []database.PromotionRequestDetail `json:"promotion_requests"`
}

// handleGetUser — GET /users/{id}. system_admin only: the whole account
// record, including which external logins reach it. Non-admins get 404 rather
// than 403 — the route must not confirm which user IDs exist. Apps the user
// can reach come from GET /users/{id}/access, which already computes the
// effective role per app; this endpoint does not restate them.
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	userID := chi.URLParam(r, "id")
	target, err := s.db.GetUserActivity(userID)
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
		return
	}

	groups, err := s.db.ListUserGroupMemberships(userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out, err := s.resolveUserIdentities(userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Not fatal: the record is worth rendering without it.
	promotions, err := s.db.ListPromotionRequestsForUser(userID)
	if err != nil {
		log.Printf("promotion requests for %s: %v", userID, err)
		promotions = nil
	}

	writeJSON(w, http.StatusOK, userDetailResponse{
		User: target, Groups: groups, Identities: out, PromotionRequests: promotions,
	})
}

// resolveUserIdentities lists the external logins that reach an account, each
// carrying the provider's display name rather than its bare id.
//
// Shared by the administrator's view of somebody else (handleGetUser) and the
// subject's view of themselves (handleMyAccount) so the two cannot disagree
// about what a provider is called — the drift §9's second open question warns
// about, closed here for this one fact rather than argued about later.
//
// A provider row can be deleted while identities minted through it remain; the
// identity is still a fact, so it reports under its bare id.
func (s *Server) resolveUserIdentities(userID string) ([]userDetailIdentity, error) {
	identities, err := s.db.ListUserIdentities(userID)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	out := make([]userDetailIdentity, 0, len(identities))
	for _, i := range identities {
		name, ok := names[i.ProviderID]
		if !ok {
			name = i.ProviderID
			if prov, err := s.authProvider(i.ProviderID); err == nil && prov.DisplayName != "" {
				name = prov.DisplayName
			}
			names[i.ProviderID] = name
		}
		out = append(out, userDetailIdentity{
			ProviderID: i.ProviderID, ProviderName: name,
			Login: i.Login, Name: i.Name, Email: i.Email,
			CreatedAt: i.CreatedAt, LastLoginAt: i.LastLoginAt,
		})
	}
	return out, nil
}

// handleListUsers — GET /users. system_admin only.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator for the list of accounts; GET /api/v1/me/account shows your own"})
		return
	}
	// With the dormancy clock on every row. It rides along whether or not the
	// sweep is enabled — see ListUsersWithActivity — so an administrator can
	// see what enabling it would do before enabling it.
	users, err := s.db.ListUsersWithActivity()
	if err != nil && err != sql.ErrNoRows {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if users == nil {
		users = []database.UserActivity{}
	}
	writeJSON(w, http.StatusOK, users)
}
