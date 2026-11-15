package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// envSetRequest is one key of PATCH /apps/{id}/env's `set`. Value absent
// changes only the flag; Sensitive absent keeps a stored row's flag and makes
// a new row visible (database.EnvSet).
type envSetRequest struct {
	Value     *string `json:"value"`
	Sensitive *bool   `json:"sensitive"`
}

// UpdateAppEnvRequest is the body of PATCH /apps/{id}/env. A key it does not
// mention is untouched, which is the whole of its difference from the
// whole-map field it replaced: a value is always the value (there is no
// "keep" encoding, because keep is not mentioning it), a writer needs no read
// first, and two writers of different keys cannot lose each other's edit.
type UpdateAppEnvRequest struct {
	Set         map[string]envSetRequest `json:"set"`
	Unset       []string                 `json:"unset"`
	OnlyMissing bool                     `json:"only_missing"`
}

// handleUpdateAppEnv is the one way to change an existing app's environment
// (ADR §14). ActionEditContent, the lock refused as on PATCH /apps/{id}, and on
// the deploy scope's list, so a pipeline can rotate a secret with the key it
// ships with (ADR §51). The answer names keys, never a value, and the route
// never restarts the app: that is its own command.
func (s *Server) handleUpdateAppEnv(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	if s.refuseIfLocked(w, app) {
		return
	}

	var req UpdateAppEnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	for _, k := range req.Unset {
		if _, both := req.Set[k]; both {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": k + " is both set and unset in one request", "code": "set_and_unset", "name": k, "hint": "name each variable in set or in unset, not both"})
			return
		}
	}

	// The name rule, with stored names exempt: a variable saved before names
	// were checked must stay editable. Unset is not checked — removing a
	// name the platform would refuse today is the cure, not the problem.
	existing, err := s.db.GetEnvVars(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	stored := make(map[string]database.EnvVar, len(existing))
	for _, v := range existing {
		stored[v.Key] = v
	}
	if refuseEnvNames(w, req.Set, stored) {
		return
	}

	set := make(map[string]database.EnvSet, len(req.Set))
	for k, v := range req.Set {
		set[k] = database.EnvSet{Value: v.Value, Sensitive: v.Sensitive}
	}
	meta := requestMeta(r.Context())
	actorType, actorID := auditActor(p)
	res, err := s.db.ApplyEnvChanges(app.ID, set, req.Unset, req.OnlyMissing, s.encryptor, database.AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action: "app.env_change", TargetType: "app", TargetID: app.ID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	})
	var fromCred *database.EnvKeyFromCredentialError
	var refused *database.EnvChangeError
	switch {
	case errors.As(err, &fromCred):
		// The credential's name, which is on the app's own page anyway, and
		// nothing else about it.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fromCred.Key + " is supplied by the " + fromCred.Credential + " credential attached to this app. " +
				"Ask an administrator to change it, or use a different name.",
			"code": "key_from_credential", "name": fromCred.Key, "credential": fromCred.Credential, "hint": "use a different name, or ask an administrator to change the credential",
		})
		return
	case errors.As(err, &refused):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": refused.Message, "code": refused.Code, "name": refused.Key, "hint": "correct the variable the sentence names and send it again — nothing was saved"})
		return
	case err != nil:
		log.Printf("env: applying changes to %s: %v", app.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update environment variables"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"set": res.Set, "unchanged": res.Unchanged, "kept": res.Kept, "cleared": res.Cleared,
		"unset": res.Unset, "not_present": res.NotPresent,
	})
}
