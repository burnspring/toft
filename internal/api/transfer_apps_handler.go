package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

type transferAppsRequest struct {
	ToType string `json:"to_type"`
	ToID   string `json:"to_id"`
	// Storage is "" or "keep_as_admin". Empty refuses at the first app whose
	// toft.toml declares storage the new owner cannot attach; keep_as_admin
	// stamps those mappings `admin` — an administrator's write adopting them —
	// so offboarding does not stall on a missing delegation.
	Storage string `json:"storage"`
}

type transferAppsResponse struct {
	Transferred int      `json:"transferred"`
	Apps        []string `json:"apps"`
}

// handleTransferUserApps — POST /users/{id}/transfer-apps. system_admin only.
//
// Moves every app this person owns **personally** to one new owner, a user or a
// group. It is two things at once and both are the point:
//
//   - the offboarding tool. Deleting an account is refused while it owns
//     anything, and doing that one app at a time is the toil that made an
//     administrator's departure a project. This is the one action that clears
//     it.
//   - the migration path. An install adopting group ownership moves an existing
//     owner's estate to a group with one call, which is the only realistic way
//     the two hundred production apps in the motivating case ever get there.
//
// Deliberately not "every app they hold owner on": that set now includes
// everything owned by every group they belong to, and moving those would take
// apps away from groups the person merely belongs to. Personal ownership is
// what blocks the delete, so personal ownership is what this clears.
//
// Not atomic across apps, and it says so in the response. Each transfer is its
// own transaction — the alternative is one transaction holding a write lock on
// two hundred apps and every one of their grant rows, which on the estate this
// exists for is a long lock on the app-lookup path. A partial result is
// recoverable by running it again; a timeout halfway through a single giant
// transaction is not obviously recoverable by anything.
func (s *Server) handleTransferUserApps(w http.ResponseWriter, r *http.Request) {
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
	from, err := s.db.GetUser(userID)
	if err != nil || from == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the users with their ids"})
		return
	}

	var req transferAppsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ToID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to_id required", "hint": `send the new owner's id, as "to_id": "<user or group id>"`})
		return
	}

	if req.Storage != "" && req.Storage != "keep_as_admin" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `storage is "keep_as_admin" or absent`, "hint": `send "storage": "keep_as_admin", or leave storage out`})
		return
	}
	opts := database.TransferOptions{KeepStorageAsAdmin: req.Storage == "keep_as_admin"}

	toType, refusal := s.resolveOwnerTarget(req.ToType, req.ToID)
	if refusal != nil {
		refusal.write(w)
		return
	}
	if toType == database.PrincipalTypeUser && req.ToID == userID {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "that is the same person — name a different owner", "hint": "name a different user or a group in to_id"})
		return
	}

	owned, err := s.db.ListAppsOwnedBy(userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	meta := requestMeta(r.Context())
	out := transferAppsResponse{Apps: []string{}}
	for _, app := range owned {
		if err := s.db.TransferOwnershipWith(r.Context(), app.ID, toType, req.ToID, p.ID, meta, opts); err != nil {
			var missing *database.StorageNotAttachableByNewOwnerError
			if errors.As(err, &missing) {
				writeJSON(w, http.StatusConflict, map[string]any{
					"error": fmt.Sprintf("moved %d app%s, then %s declares storage in toft.toml the new owner "+
						"cannot attach — delegate it, or send storage: keep_as_admin to keep it attached as an administrator's",
						out.Transferred, plural(out.Transferred), app.Slug),
					"code": "storage_not_attachable_by_new_owner", "app": app.Slug, "storage": missing.Missing,
					"transferred": out.Transferred, "hint": `send "storage": "keep_as_admin" to move the rest, or let the new owner attach that storage first`,
				})
				return
			}
			log.Printf("transfer-apps: moving %s from %s: %v", app.ID, userID, err)
			// Stop rather than press on. Whatever refused the first app will
			// refuse the rest, and reporting "moved 3 of 200" with a reason
			// beats two hundred identical log lines and a 500.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": fmt.Sprintf("moved %d app%s, then %s could not be transferred",
					out.Transferred, plural(out.Transferred), app.Slug), "hint": "transfer that app on its own with POST /api/v1/apps/{id}/transfer to see why, then send this again to move the rest",
			})
			return
		}
		out.Transferred++
		out.Apps = append(out.Apps, app.Slug)
	}

	writeJSON(w, http.StatusOK, out)
}
