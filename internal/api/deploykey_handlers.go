package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/database"
)

// defaultDeployKeyDays is the expiry offered when the caller names none.
//
// Ninety days rather than never, because the whole argument for this class is
// that a credential should outlive a person — not that it should outlive
// everything. "Never" stays available, and the admin page shows it as such.
const defaultDeployKeyDays = 90

// maxDeployKeys bounds credential sprawl.
// Install-scoped, so the number is small: a pipeline needs one, a second
// pipeline needs a second, and thirty is somebody using them as per-app keys.
const maxDeployKeys = 30

func generateDeployKeySecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return deployKeySecretPrefix + hex.EncodeToString(buf), nil
}

// deployKeyDTO carries the computed state beside the row, so the UI reads the
// same vocabulary it reads for the other two credential classes.
type deployKeyDTO struct {
	database.DeployKey
	State string `json:"state"`
	// MinterLeft is why an administrator should look at this row: the account
	// that minted it is gone. Not a cascade and not a refusal — the key still
	// works, deliberately — but it is the prompt to rotate.
	MinterLeft bool `json:"minter_left"`
}

// liveDeployKeys counts the rows the cap is about: everything that has not been
// revoked. See the refusal in handleCreateDeployKey for why it is not len().
func liveDeployKeys(keys []database.DeployKey) int {
	n := 0
	for i := range keys {
		if keys[i].RevokedAt == nil {
			n++
		}
	}
	return n
}

func deployKeyDTOs(keys []database.DeployKey) []deployKeyDTO {
	now := time.Now().UTC()
	out := make([]deployKeyDTO, 0, len(keys))
	for _, k := range keys {
		out = append(out, deployKeyDTO{
			DeployKey:  k,
			State:      k.State(now),
			MinterLeft: k.CreatedBy == "",
		})
	}
	return out
}

// Every route in this file is behind the package's existing requireAdmin.
// Minting a credential that acts as owner on every app is an administrator's
// decision and nobody else's — and, pointedly, not a decision a credential can
// make: these routes are in apiKeyDeniedRoutes and absent from
// deployKeyAllowedRoutes, so neither a personal key nor a deploy key can reach
// them. No key mints a key.

func (s *Server) handleListDeployKeys(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	keys, err := s.db.ListDeployKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, deployKeyDTOs(keys))
}

type createDeployKeyRequest struct {
	Name string `json:"name"`
	// ExpiresInDays: absent means the default, 0 means never. A pointer
	// because those are different requests and an int cannot tell them apart.
	ExpiresInDays *int `json:"expires_in_days"`
	// Scope: absent means `deploy`, which is what the class meant before there
	// were two. Chosen here and nowhere else — there is no PATCH that touches
	// it, because a scope that could be widened in place is a scope that is not
	// a boundary. Changing one is minting the other and revoking this.
	Scope string `json:"scope"`
	// MayAttach lets deploys carrying this key add storage toft.toml declares,
	// on the app owner's delegation alone. Deploy scope only.
	MayAttach bool `json:"may_attach"`
}

// mayAttachOnReadKey is the CHECK's sentence: a read key deploys nothing, so
// letting it attach would mean nothing.
const mayAttachOnReadKey = "only a deploy-scoped key may attach storage — a read key deploys nothing"

func (s *Server) handleCreateDeployKey(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}

	var req createDeployKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required", "hint": `send a name, as "name": "ci-deploy"`})
		return
	}

	existing, err := s.db.ListDeployKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// The cap counts what revoking can remove, which is the only reading that
	// makes its own refusal true. ListDeployKeys returns revoked rows too —
	// deliberately, because the audit trail resolves `deploy_key:{id}` to a
	// name — and counting those meant an install that had minted and revoked
	// its way to thirty refused every new key while telling the operator to
	// revoke one, with every key it held already revoked and no route that
	// removes a row. There was no way out of that state through the API at
	// all. A disabled or expired key still counts: disabled is reversible, and
	// both still hold their name.
	if liveDeployKeys(existing) >= maxDeployKeys {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this install already has the maximum number of deploy keys — revoke one first",
			"code":  "key_limit_reached"})
		return
	}

	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		scope = database.DeployKeyScopeDeploy
	}
	if !database.ValidDeployKeyScope(scope) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a deploy key is scoped read or deploy",
			"code":  "invalid_scope", "hint": `send "scope": "deploy", or "scope": "read" for a key that only reads`,
		})
		return
	}

	if req.MayAttach && scope != database.DeployKeyScopeDeploy {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": mayAttachOnReadKey, "code": "may_attach_needs_deploy_scope", "hint": `send "scope": "deploy" with may_attach, or leave may_attach out`})
		return
	}

	days := defaultDeployKeyDays
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	}
	// The siblings' range and the siblings' sentence: -30 used to mint a key
	// that never expired, and 100000 one centuries out.
	if days < 0 || days > maxKeyExpiryDays {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "expires_in_days must be between 0 (never) and 3650", "hint": "pass expires_in_days as a whole number from 0 (never expires) to 3650"})
		return
	}
	var expires *time.Time
	if days > 0 {
		t := time.Now().UTC().AddDate(0, 0, days)
		expires = &t
	}

	secret, err := generateDeployKeySecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	key := &database.DeployKey{
		ID:        uuid.New().String(),
		Name:      name,
		KeyHash:   hashAPIKey(secret),
		KeyPrefix: secret[:len(deployKeySecretPrefix)+6],
		Scope:     scope,
		CreatedBy: p.ID,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expires,
		MayAttach: req.MayAttach,
	}
	meta := requestMeta(r.Context())
	if err := s.db.CreateDeployKey(key, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		Action: "deploy_key.create", TargetType: "deploy_key", TargetID: key.ID,
		After:     jsonValStr(map[string]any{"name": name, "scope": scope, "may_attach": req.MayAttach}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		// A name already taken is an ordinary refusal, and the same one the
		// other two credential classes answer. `deploy_keys.name` is UNIQUE
		// because the class is install-wide and owned by nobody, so the name is
		// all anybody has to identify a key by — but a unique violation
		// answered as 500 tells the caller the install is broken when what
		// happened is that they picked a name.
		if strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "a deploy key with that name already exists", "code": "name_taken",
				"hint": "choose another name; GET /api/v1/deploy-keys lists the names in use, a revoked key's included"})
			return
		}
		log.Printf("creating deploy key: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create the key"})
		return
	}

	// The secret, exactly once. Every later read serves the prefix and nothing
	// more, because only its hash was stored.
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"key":    deployKeyDTOs([]database.DeployKey{*key})[0],
		"secret": secret,
	})
}

// deployKeyPatchRequest carries the two things about a key that may change:
// whether it works, and whether it may attach storage. Deliberately not the
// scope: see createDeployKeyRequest.
type deployKeyPatchRequest struct {
	Disabled  *bool `json:"disabled"`
	MayAttach *bool `json:"may_attach"`
}

func (s *Server) handleUpdateDeployKey(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	id := chi.URLParam(r, "keyID")
	key, err := s.db.GetDeployKey(id)
	if err != nil || key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/deploy-keys lists the deploy keys with their ids"})
		return
	}
	var req deployKeyPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Disabled == nil && req.MayAttach == nil) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "disabled or may_attach required", "hint": `send "disabled": true or false, or "may_attach": true or false`})
		return
	}
	if req.MayAttach != nil && *req.MayAttach && key.Scope != database.DeployKeyScopeDeploy {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": mayAttachOnReadKey, "code": "may_attach_needs_deploy_scope", "hint": "mint a deploy-scoped key with POST /api/v1/deploy-keys to attach storage"})
		return
	}
	// Revoked is final. Re-enabling one would make "revoked" mean "disabled
	// with a stronger word", and the whole value of the state is that it does
	// not.
	if key.RevokedAt != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this key has been revoked, which cannot be undone — mint a new one", "hint": "mint a new key with POST /api/v1/deploy-keys"})
		return
	}
	if req.Disabled != nil {
		if err := s.db.SetDeployKeyDisabled(r.Context(), id, *req.Disabled, p.ID, requestMeta(r.Context())); err != nil {
			log.Printf("updating deploy key %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	if req.MayAttach != nil {
		if err := s.db.SetDeployKeyMayAttach(r.Context(), id, *req.MayAttach, p.ID, requestMeta(r.Context())); err != nil {
			log.Printf("setting may_attach on deploy key %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	updated, _ := s.db.GetDeployKey(id)
	writeJSON(w, http.StatusOK, deployKeyDTOs([]database.DeployKey{*updated})[0])
}

func (s *Server) handleRevokeDeployKey(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	id := chi.URLParam(r, "keyID")
	key, err := s.db.GetDeployKey(id)
	if err != nil || key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/deploy-keys lists the deploy keys with their ids"})
		return
	}
	if err := s.db.RevokeDeployKey(r.Context(), id, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("revoking deploy key %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
