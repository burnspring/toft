package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/auth"
	"toft/internal/database"
)

const (
	// maxAPIKeysPerUser bounds credential sprawl per person, mirroring the
	// per-app cap. Ten is enough for one token per pipeline and few enough
	// that the list stays a thing someone reads rather than scrolls.
	maxAPIKeysPerUser = 10

	// maxExchangedKeysPerIssuer bounds one person's live exchanged keys from
	// one trusted issuer. Past it the oldest is revoked, never the exchange
	// refused (CreateExchangedKeyTx).
	maxExchangedKeysPerIssuer = 10

	// defaultKeyExpiryDays is what a caller gets when they say nothing. A key
	// that never expires is offered but not assumed: most automation outlives
	// the person who remembers minting it, and the default should be the
	// choice you would want to have made.
	defaultKeyExpiryDays = 90
	maxKeyExpiryDays     = 3650

	// keyUsageDefaultDays and keyUsageMaxDays bound the reporting window. A
	// year, so month-over-month is answerable at all — the 90-day ceiling on
	// app usage predates anyone asking that question.
	keyUsageDefaultDays = 30
	keyUsageMaxDays     = 365
)

// signInKeyExpiry is when a key created by a sign-in flow expires: `toft login`
// and RStudio's Add account, the two ways a person hands a machine a credential
// by approving it in the browser.
//
// **Never, unless the install caps it.** These keys stand in for a publishing
// account the person set up once, and a key that lapses on a fixed date fails a
// deploy months later with nobody near the terminal that made it — Posit
// Connect's own tokens for the same flow do not expire either. Revocation is
// what ends one: deleting the key, locking the person, or switching personal
// keys off. An administrator who wants an end date sets
// `api_key_max_lifetime_days`, and these keys take it.
func (s *Server) signInKeyExpiry() (*time.Time, error) {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		return nil, err
	}
	if settings.APIKeyMaxLifetimeDays <= 0 {
		return nil, nil
	}
	t := time.Now().UTC().AddDate(0, 0, settings.APIKeyMaxLifetimeDays)
	return &t, nil
}

// generateUserAPIKeySecret mints a personal key. 24 random bytes is 192 bits;
// a SHA-256 of it needs no password-hashing work factor, because there is no
// low-entropy secret to grind.
func generateUserAPIKeySecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return userKeySecretPrefix + hex.EncodeToString(buf), nil
}

// keyDisplayPrefix is the leading fragment stored for identification: enough
// to match a key against the CI job holding it, far too little to use.
func keyDisplayPrefix(secret string) string {
	if len(secret) < 12 {
		return secret
	}
	return secret[:12]
}

// userKeyDTO adds the computed state to the stored row, so the UI never
// re-derives "is this expired" from a timestamp and a clock it does not share.
type userKeyDTO struct {
	database.UserAPIKey
	State string `json:"state"`
}

func userKeyDTOs(keys []database.UserAPIKey) []userKeyDTO {
	now := time.Now().UTC()
	out := make([]userKeyDTO, 0, len(keys))
	for _, k := range keys {
		key := k
		out = append(out, userKeyDTO{UserAPIKey: key, State: key.State(now)})
	}
	return out
}

// requirePersonalKeys gates every personal-key route on the install-wide
// switch. Off means **absent**, not refused: 404, so the capability looks like
// something this install does not offer rather than something it offers and
// denies. Returns the caller when the capability is on and they are signed in.
func (s *Server) requirePersonalKeys(w http.ResponseWriter, r *http.Request) *auth.Principal {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil
	}
	if !settings.APIKeysEnabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "ask an administrator to turn personal API keys on for this install"})
		return nil
	}
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}
	return p
}

func (s *Server) handleListMyAPIKeys(w http.ResponseWriter, r *http.Request) {
	p := s.requirePersonalKeys(w, r)
	if p == nil {
		return
	}
	keys, err := s.db.ListUserAPIKeys(p.ID, keyUsageDefaultDays)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"keys": userKeyDTOs(keys),
		// The install's cap, so the create form offers only what it would
		// accept. 0 is no cap.
		"max_lifetime_days": settings.APIKeyMaxLifetimeDays,
		// The window the per-key request counts were taken over, so the column
		// header can say what it is counting rather than an unqualified number.
		"days": keyUsageDefaultDays,
	})
}

// handleCreateMyAPIKey mints a key and returns the secret exactly once. Only
// its SHA-256 is stored, so there is no path — for anyone, at any privilege —
// that shows it again.
func (s *Server) handleCreateMyAPIKey(w http.ResponseWriter, r *http.Request) {
	p := s.requirePersonalKeys(w, r)
	if p == nil {
		return
	}

	var req struct {
		Name string `json:"name"`
		// ExpiresInDays: nil means the default; 0 means never. The pointer is
		// what tells those apart — an omitted field and an explicit "never"
		// are different intentions.
		ExpiresInDays *int `json:"expires_in_days"`
		// Scope: absent means `full`, which is what every client that predates
		// this field asks for and what it has always received. A plain string
		// rather than a pointer, because unlike the expiry there is no third
		// intention to express — "" and "omitted" mean the same thing here.
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name must be 1-64 characters", "hint": "pass name as 1 to 64 characters, like laptop or ci"})
		return
	}

	scope := req.Scope
	if scope == "" {
		scope = database.UserKeyScopeFull
	}
	// Validated against the same predicate the database's CHECK mirrors, so the
	// vocabulary cannot fork between the mint, the guard and the column.
	if !database.ValidUserKeyScope(scope) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "scope must be \"read\" or \"full\"", "hint": "pass scope as read or full, or leave it out for full"})
		return
	}

	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	limit := settings.APIKeyMaxLifetimeDays

	days := defaultKeyExpiryDays
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	} else if limit > 0 && days > limit {
		// Saying nothing asks for the default, and the default must not be
		// the one choice the install refuses.
		days = limit
	}
	if days < 0 || days > maxKeyExpiryDays {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "expires_in_days must be between 0 (never) and 3650", "hint": "pass expires_in_days as a number from 0 to 3650, where 0 means the key never expires"})
		return
	}
	// Refused rather than shortened: somebody who asked for a key that never
	// expires and was handed one that does would find out on the day it
	// stopped working.
	if limit > 0 && (days == 0 || days > limit) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this install allows personal API keys to last at most " +
				strconv.Itoa(limit) + " days", "hint": "pass expires_in_days as a number no larger than the limit the sentence names"})
		return
	}
	var expiresAt *time.Time
	if days > 0 {
		t := time.Now().UTC().AddDate(0, 0, days)
		expiresAt = &t
	}

	n, err := s.db.CountUserAPIKeys(p.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if n >= maxAPIKeysPerUser {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "you can have at most 10 API keys — revoke one first",
			"code":  "key_limit_reached"})
		return
	}

	secret, err := generateUserAPIKeySecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	key := database.UserAPIKey{
		ID:        uuid.New().String(),
		UserID:    p.ID,
		Name:      name,
		KeyHash:   hashAPIKey(secret),
		KeyPrefix: keyDisplayPrefix(secret),
		Scope:     scope,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}

	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	audit.Action = "user.api_key_create"
	audit.TargetType = "user"
	audit.TargetID = p.ID
	// The prefix and the expiry, never the secret or its hash: an audit blob is
	// read by more people than the credential ever should be.
	audit.After = jsonValStr(map[string]interface{}{
		"key_id": key.ID, "name": key.Name, "key_prefix": key.KeyPrefix,
		"expires_at": key.ExpiresAt,
		// The scope joins the blob rather than becoming a second action. What
		// happened is still a mint, and a `user.api_key_create_read` would
		// split "which keys were minted" across two audit queries forever.
		"scope": key.Scope,
	})

	if err := s.db.CreateUserAPIKeyTx(&key, audit); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "you already have a key with that name", "code": "name_taken", "hint": "choose another name; GET /api/v1/me/api-keys lists the names you have used"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"key":    userKeyDTO{UserAPIKey: key, State: key.State(time.Now().UTC())},
		"secret": secret,
	})
}

// keyPatch is the shared body for renaming and for disabling/enabling, on both
// credential classes. Disabled is a pointer so that "enable" is distinguishable
// from "do not touch".
type keyPatch struct {
	Name     *string `json:"name"`
	Disabled *bool   `json:"disabled"`
}

func decodeKeyPatch(w http.ResponseWriter, r *http.Request) (*keyPatch, bool) {
	var req keyPatch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return nil, false
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || len(n) > 64 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name must be 1-64 characters", "hint": "pass name as 1 to 64 characters, like laptop or ci"})
			return nil, false
		}
		req.Name = &n
	}
	if req.Name == nil && req.Disabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to change", "hint": "send name, disabled or both, like {\"disabled\": true}"})
		return nil, false
	}
	return &req, true
}

// keyPatchAction names the change for the audit log. Disabling a credential
// and renaming one are different events and must not share a verb.
func keyPatchAction(prefix string, p *keyPatch) string {
	switch {
	case p.Disabled != nil && *p.Disabled:
		return prefix + "_disable"
	case p.Disabled != nil:
		return prefix + "_enable"
	default:
		return prefix + "_rename"
	}
}

func (s *Server) handleUpdateMyAPIKey(w http.ResponseWriter, r *http.Request) {
	p := s.requirePersonalKeys(w, r)
	if p == nil {
		return
	}
	// Scoped by owner: a key id out of a URL never names a key on its own.
	key, err := s.db.GetUserAPIKey(p.ID, chi.URLParam(r, "keyID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found", "hint": "GET /api/v1/me/api-keys lists your keys and their ids"})
		return
	}
	patch, ok := decodeKeyPatch(w, r)
	if !ok {
		return
	}

	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	audit.Action = keyPatchAction("user.api_key", patch)
	audit.TargetType = "user"
	audit.TargetID = p.ID
	audit.Before = jsonValStr(map[string]interface{}{
		"key_id": key.ID, "name": key.Name, "state": key.State(time.Now().UTC())})
	audit.After = jsonValStr(map[string]interface{}{
		"key_id": key.ID, "name": patch.Name, "disabled": patch.Disabled})

	if err := s.db.UpdateUserAPIKeyTx(key.ID, patch.Name, patch.Disabled, audit); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "you already have a key with that name", "code": "name_taken", "hint": "choose another name; GET /api/v1/me/api-keys lists the names you have used"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	updated, err := s.db.GetUserAPIKey(p.ID, key.ID)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, userKeyDTO{UserAPIKey: *updated, State: updated.State(time.Now().UTC())})
}

func (s *Server) handleDeleteMyAPIKey(w http.ResponseWriter, r *http.Request) {
	p := s.requirePersonalKeys(w, r)
	if p == nil {
		return
	}
	key, err := s.db.GetUserAPIKey(p.ID, chi.URLParam(r, "keyID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found", "hint": "GET /api/v1/me/api-keys lists your keys and their ids"})
		return
	}

	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	audit.Action = "user.api_key_revoke"
	audit.TargetType = "user"
	audit.TargetID = p.ID
	audit.Before = jsonValStr(map[string]string{
		"key_id": key.ID, "name": key.Name, "key_prefix": key.KeyPrefix})

	if err := s.db.DeleteUserAPIKeyTx(key.ID, audit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// keyUsageWindow parses the shared ?days= and ?bucket= parameters. Week and
// month are rollups of the same daily rows, which is why switching bucket can
// never change the total beside the chart.
func keyUsageWindow(w http.ResponseWriter, r *http.Request) (days int, bucket string, ok bool) {
	days = keyUsageDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > keyUsageMaxDays {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be 1-365", "hint": "pass days as a whole number from 1 to 365"})
			return 0, "", false
		}
		days = n
	}
	bucket = r.URL.Query().Get("bucket")
	switch bucket {
	case "", "day":
		bucket = "day"
	case "week", "month":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bucket must be day, week or month", "hint": "pass bucket as day, week or month, or leave it out for day"})
		return 0, "", false
	}
	return days, bucket, true
}

// writeKeyUsage answers the series, the window total and what the key spent
// its calls on. Shared by both credential classes: the question is the same
// and so is the table underneath.
func (s *Server) writeKeyUsage(w http.ResponseWriter, keyID, bucket string, days int) {
	series, err := s.db.GetAPIKeyUsage(keyID, bucket, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	totals, err := s.db.GetAPIKeyUsageTotals(keyID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	targets, err := s.db.GetAPIKeyUsageTargets(keyID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"days": days, "bucket": bucket,
		"series": series, "totals": totals, "targets": targets,
	})
}

func (s *Server) handleMyAPIKeyUsage(w http.ResponseWriter, r *http.Request) {
	p := s.requirePersonalKeys(w, r)
	if p == nil {
		return
	}
	key, err := s.db.GetUserAPIKey(p.ID, chi.URLParam(r, "keyID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found", "hint": "GET /api/v1/me/api-keys lists your keys and their ids"})
		return
	}
	days, bucket, ok := keyUsageWindow(w, r)
	if !ok {
		return
	}
	s.writeKeyUsage(w, key.ID, bucket, days)
}

// --- Install-wide administration ---

// requireKeyAdmin gates the inventory. 404 rather than 403 to everyone else,
// matching /admin/audit and /system/settings: how many credentials exist on
// this install, and whose they are, is not something a non-administrator is
// owed an answer about — including the answer "you may not know".
//
// Unlike the personal routes, this one does *not* 404 when the capability is
// switched off. Turning the switch off is exactly when an administrator most
// needs to see what is out there and revoke it.
func (s *Server) requireKeyAdmin(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return nil
	}
	return p
}

func (s *Server) handleListAllAPIKeys(w http.ResponseWriter, r *http.Request) {
	if s.requireKeyAdmin(w, r) == nil {
		return
	}
	days, _, ok := keyUsageWindow(w, r)
	if !ok {
		return
	}
	keys, err := s.db.ListAllUserAPIKeys(days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"keys": userKeyDTOs(keys), "days": days})
}

func (s *Server) handleAdminUpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	p := s.requireKeyAdmin(w, r)
	if p == nil {
		return
	}
	key, err := s.db.GetAnyUserAPIKey(chi.URLParam(r, "keyID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found", "hint": "GET /api/v1/admin/api-keys lists every key and its id"})
		return
	}
	patch, ok := decodeKeyPatch(w, r)
	if !ok {
		return
	}

	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	audit.Action = keyPatchAction("admin.user_api_key", patch)
	// Targeted at the key's owner, not the administrator: the question this
	// row answers later is "what happened to alice's credentials".
	audit.TargetType = "user"
	audit.TargetID = key.UserID
	audit.Before = jsonValStr(map[string]interface{}{
		"key_id": key.ID, "name": key.Name, "state": key.State(time.Now().UTC())})
	audit.After = jsonValStr(map[string]interface{}{
		"key_id": key.ID, "name": patch.Name, "disabled": patch.Disabled})

	if err := s.db.UpdateUserAPIKeyTx(key.ID, patch.Name, patch.Disabled, audit); err != nil {
		// The same unique violation the owner's own rename maps to 409: the
		// name is taken among that person's keys.
		if strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "that person already has a key with that name", "code": "name_taken", "hint": "choose another name; GET /api/v1/users/{id}/api-keys lists that person's keys"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	updated, err := s.db.GetAnyUserAPIKey(key.ID)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, userKeyDTO{UserAPIKey: *updated, State: updated.State(time.Now().UTC())})
}

func (s *Server) handleAdminDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	p := s.requireKeyAdmin(w, r)
	if p == nil {
		return
	}
	key, err := s.db.GetAnyUserAPIKey(chi.URLParam(r, "keyID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found", "hint": "GET /api/v1/admin/api-keys lists every key and its id"})
		return
	}

	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	audit.Action = "admin.user_api_key_revoke"
	audit.TargetType = "user"
	audit.TargetID = key.UserID
	audit.Before = jsonValStr(map[string]string{
		"key_id": key.ID, "name": key.Name, "key_prefix": key.KeyPrefix})

	if err := s.db.DeleteUserAPIKeyTx(key.ID, audit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- One account's credentials, on the account record ---
//
// The install-wide inventory answers "find this leaked key"; these two answer
// "what does this account actually do", which is the question the account
// record exists for and the one the dormancy clock only summarises. A person
// whose clock is fresh because of a key should be one click from seeing which
// key, when, and from where — otherwise "last used: 2 days ago, via API key"
// is a fact an administrator can read but not act on.
//
// Admin-gated through requireKeyAdmin, so like /admin/api-keys they keep
// working while personal keys are switched off: an administrator deciding
// whether an account is safe to lock needs to see the credentials that would
// stop working, and off is when that decision gets made.

// handleListUserAPIKeys — GET /users/{id}/api-keys.
func (s *Server) handleListUserAPIKeys(w http.ResponseWriter, r *http.Request) {
	if s.requireKeyAdmin(w, r) == nil {
		return
	}
	userID := chi.URLParam(r, "id")
	// 404 for an id that names nobody, matching GET /users/{id}: an empty key
	// list and a user who does not exist are different answers, and this route
	// must not turn the second into the first.
	target, err := s.db.GetUser(userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the accounts and their ids"})
		return
	}
	days, _, ok := keyUsageWindow(w, r)
	if !ok {
		return
	}
	keys, err := s.db.ListUserAPIKeys(userID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"keys": userKeyDTOs(keys), "days": days})
}

// handleUserAPIKeyUsage — GET /users/{id}/api-keys/{keyID}/usage.
//
// GetUserAPIKey, not GetAnyUserAPIKey: the key id is scoped by the owner id in
// the URL even though the caller is an administrator, so a key id pasted under
// the wrong account is a 404 rather than someone else's traffic drawn on this
// person's record.
func (s *Server) handleUserAPIKeyUsage(w http.ResponseWriter, r *http.Request) {
	if s.requireKeyAdmin(w, r) == nil {
		return
	}
	key, err := s.db.GetUserAPIKey(chi.URLParam(r, "id"), chi.URLParam(r, "keyID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found", "hint": "GET /api/v1/users/{id}/api-keys lists that person's keys and their ids"})
		return
	}
	days, bucket, ok := keyUsageWindow(w, r)
	if !ok {
		return
	}
	s.writeKeyUsage(w, key.ID, bucket, days)
}
