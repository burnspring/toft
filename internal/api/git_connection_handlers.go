package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/gitconn"
	"toft/internal/safefetch"
)

// Admin → Sources, the connections half.
//
// An administrator connects the organization's git host once here, and apps
// stop carrying credentials of their own. It sits beside the source policy
// because the two answer one question between them — *where may code come
// from, and how do we reach it* — and it is admin-only for the same reason the
// sign-in providers are: whoever can add a connection can add a credential the
// install clones through.
//
// Secrets go in and never come out. The API accepts them, encrypts them, and
// thereafter reports only whether one is stored. The one secret that comes out
// is the webhook secret the platform minted itself, and only in the response
// that minted it: POST …/webhook-secret, which turns the webhook on.

// requireConnectionAdmin gates every route in this file.
func (s *Server) requireConnectionAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	if !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "only an administrator can manage connected git hosts", "hint": "ask an administrator to add or change the connection"})
		return false
	}
	return true
}

// adminGitConnectionDTO is a connection as the admin page sees it: everything
// except the three ciphertexts, plus what the page has to say about the
// credential's provenance, reach and expiry.
type adminGitConnectionDTO struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	BaseURL     string `json:"base_url"`
	AuthMethod  string `json:"auth_method"`
	Enabled     bool   `json:"enabled"`

	AppID          string `json:"app_id,omitempty"`
	InstallationID string `json:"installation_id,omitempty"`
	ClientID       string `json:"client_id,omitempty"`

	CloneSecretSet   bool `json:"clone_secret_set"`
	ClientSecretSet  bool `json:"client_secret_set"`
	WebhookSecretSet bool `json:"webhook_secret_set"`
	// CloneSecretUnreadable is a stored credential this install's keys cannot
	// open — a lost or rotated ENCRYPTION_KEY. The connection is broken rather
	// than absent, so it stays listed and the form says the value must be
	// retyped. Same distinction the sign-in providers already draw.
	CloneSecretUnreadable bool `json:"clone_secret_unreadable"`

	// Reach is the one sentence the page says about what the credential can
	// see. It is derived from the token's own prefix, so it costs nobody a
	// question and cannot go stale against the stored value.
	Reach               string     `json:"reach"`
	CredentialKind      string     `json:"credential_kind,omitempty"`
	CredentialOwner     string     `json:"credential_owner,omitempty"`
	CredentialExpiresAt *time.Time `json:"credential_expires_at,omitempty"`

	Coverage []string `json:"coverage"`
	// Picker says whether a developer choosing this connection gets a
	// repository list or a URL field. It is on the admin DTO because the page
	// states what the auth method costs; the developer's route carries the same
	// flag and never the method.
	Picker bool `json:"picker"`

	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	LastOKAt    *time.Time `json:"last_ok_at,omitempty"`

	AppCount  int       `json:"app_count"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// WebhookURL is what an administrator registers at the host. Computed by
	// the server for the same reason a redirect URI is: a hand-typed one that
	// differs by a trailing slash fails with an error about the secret.
	WebhookURL string `json:"webhook_url"`
}

func (s *Server) gitConnectionDTO(c *database.GitConnection) adminGitConnectionDTO {
	return adminGitConnectionDTO{
		ID:                    c.ID,
		Kind:                  c.Kind,
		DisplayName:           c.DisplayName,
		BaseURL:               c.BaseURL,
		AuthMethod:            c.AuthMethod,
		Enabled:               c.Enabled,
		AppID:                 c.GitHubAppID,
		InstallationID:        c.InstallationID,
		ClientID:              c.ClientID,
		CloneSecretSet:        c.CloneSecretSet,
		ClientSecretSet:       c.ClientSecretSet,
		WebhookSecretSet:      c.WebhookSecretSet,
		CloneSecretUnreadable: s.credentialUnreadable(c.EncryptedCloneSecret()),
		Reach:                 gitconn.ReachSentence(c),
		CredentialKind:        c.CredentialKind,
		CredentialOwner:       c.CredentialOwner,
		CredentialExpiresAt:   c.CredentialExpiresAt,
		Coverage:              c.CoverageList(),
		Picker:                c.HasPicker(),
		LastError:             c.LastError,
		LastErrorAt:           c.LastErrorAt,
		LastOKAt:              c.LastOKAt,
		AppCount:              c.AppCount,
		CreatedBy:             c.CreatedBy,
		CreatedAt:             c.CreatedAt,
		UpdatedAt:             c.UpdatedAt,
		WebhookURL:            strings.TrimRight(s.cfg.BaseURL, "/") + "/api/v1/git/hooks/" + c.ID,
	}
}

// gitConnectionRequest is the write shape. Secrets are pointers so that absent
// and empty are different: absent keeps what is stored, and there is no way to
// spell "clear it" — a connection with no credential is a connection to
// remove.
type gitConnectionRequest struct {
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	BaseURL     string `json:"base_url"`
	AuthMethod  string `json:"auth_method"`
	Enabled     *bool  `json:"enabled"`

	AppID          string  `json:"app_id"`
	InstallationID string  `json:"installation_id"`
	ClientID       string  `json:"client_id"`
	ClientSecret   *string `json:"client_secret"`
	CloneSecret    *string `json:"clone_secret"`

	CredentialOwner     string       `json:"credential_owner"`
	CredentialExpiresAt optionalTime `json:"credential_expires_at"`

	Coverage []string `json:"coverage"`
}

// optionalTime is a PATCH field with merge-patch semantics: absent keeps the
// stored value, an explicit null clears it, a time sets it. A bare *time.Time
// decodes null and absent alike, so clearing a connection's expiry kept the
// old date while the dialog said Saved.
type optionalTime struct {
	Set   bool
	Value *time.Time
}

func (o *optionalTime) UnmarshalJSON(data []byte) error {
	o.Set = true
	if string(data) == "null" {
		o.Value = nil
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(data, &t); err != nil {
		return err
	}
	o.Value = &t
	return nil
}

func (s *Server) handleListGitConnections(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	rows, err := s.db.ListGitConnections()
	if err != nil {
		log.Printf("listing git connections: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]adminGitConnectionDTO, 0, len(rows))
	for i := range rows {
		out = append(out, s.gitConnectionDTO(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (s *Server) handleCreateGitConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	// The license gates *configuration*, never operation. Creating a
	// connection needs one; cloning through one that already exists consults
	// no entitlement at all, so an install whose license lapses keeps
	// deploying every app it already connected.
	if s.refusedUnlicensed(w, r, entitlements.FeatureTrustedSources) {
		return
	}
	var body gitConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	row := &database.GitConnection{
		ID:          uuid.NewString(),
		Kind:        strings.TrimSpace(body.Kind),
		DisplayName: strings.TrimSpace(body.DisplayName),
		AuthMethod:  strings.TrimSpace(body.AuthMethod),
		Enabled:     body.Enabled == nil || *body.Enabled,
	}
	applyGitConnectionFields(row, &body)
	row.BaseURL = defaultBaseURL(row.Kind, strings.TrimSpace(body.BaseURL))

	if msg := validateGitConnection(row, derefOr(body.CloneSecret, "")); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "correct the field the sentence names and send it again — nothing was saved"})
		return
	}
	// Described, not refused (Q10). The prefix says which kind of token this
	// is, so the page can state its reach without asking anybody — and D17's
	// coverage bounds it either way.
	row.CredentialKind = gitconn.ClassifyCredential(row.Kind, row.AuthMethod, derefOr(body.CloneSecret, ""))

	if err := s.encryptGitConnectionSecrets(row, &body); err != nil {
		log.Printf("encrypting git connection secrets: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// **No webhook secret.** The webhook is an accelerator an organization
	// may not be able to use at all — most do not let a git host call in — so
	// a connection starts without one and polls, and an administrator turns it
	// on with POST …/webhook-secret, which mints the secret and shows it once.
	row.SetEncryptedWebhookSecret("")

	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	row.CreatedBy = p.ID

	if err := s.db.CreateGitConnection(row, audit); err != nil {
		log.Printf("creating git connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.gitConn.Forget(row.ID)

	// A connection is a credential this install now holds for cloning, which is
	// exactly the change worth somebody noticing. After the write, and it can
	// neither fail the create nor delay it.
	covers := "whatever the App is installed on"
	if len(row.CoverageList()) > 0 {
		covers = strings.Join(row.CoverageList(), ", ")
	}
	s.notifySourceReachWidened(
		"A git host was connected to this install",
		row.DisplayName+" ("+row.BaseURL+") was connected by "+p.ID+
			". Apps may now be pointed at "+covers+" and will clone with this install's own "+
			"credential rather than one of their own.")

	writeJSON(w, http.StatusCreated, s.gitConnectionDTO(row))
}

// handleDisableGitConnectionWebhook turns the connection's webhook off by
// clearing its secret: no delivery verifies after the write, and the poller is
// the only way a push deploys, as on a connection that never had one. Not
// license-gated, for the rotate's reason.
func (s *Server) handleDisableGitConnectionWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "connectionID")
	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	err := s.db.ClearGitConnectionWebhookSecret(id, audit)
	if errors.Is(err, database.ErrGitConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/admin/git-connections lists the connections with their ids"})
		return
	}
	if err != nil {
		log.Printf("turning off the webhook of git connection %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	row, err := s.db.GetGitConnection(id)
	if err != nil {
		log.Printf("reading git connection %s after turning off its webhook: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.gitConnectionDTO(row))
}

// gitConnectionWithSecret is a connection plus its freshly minted webhook
// secret, returned by the webhook-secret route and by nothing else.
type gitConnectionWithSecret struct {
	adminGitConnectionDTO
	WebhookSecret string `json:"webhook_secret"`
}

// handleRotateGitConnectionWebhookSecret turns the connection's webhook on, or
// replaces its secret: it mints one and returns it once, the way an API key is
// shown, and never reads it back after — a secret that could be fetched again
// is a credential with a second reader for the life of the install. An old
// secret stops verifying with the write.
// **Not license-gated**, on the disable's rule: it is an incident verb, and a
// credential the platform will not let you replace is worse than one it will
// not let you edit.
func (s *Server) handleRotateGitConnectionWebhookSecret(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "connectionID")
	secret, err := newWebhookSecret()
	if err != nil {
		log.Printf("minting a webhook secret: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	encrypted, err := s.encryptor.Encrypt(secret)
	if err != nil {
		log.Printf("encrypting the webhook secret: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID

	err = s.db.RotateGitConnectionWebhookSecret(id, encrypted, audit)
	if errors.Is(err, database.ErrGitConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/admin/git-connections lists the connections with their ids"})
		return
	}
	if err != nil {
		log.Printf("rotating the webhook secret of git connection %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	row, err := s.db.GetGitConnection(id)
	if err != nil {
		log.Printf("reading git connection %s after rotating: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, gitConnectionWithSecret{s.gitConnectionDTO(row), secret})
}

func (s *Server) handleUpdateGitConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "connectionID")
	existing, err := s.db.GetGitConnection(id)
	if errors.Is(err, database.ErrGitConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/admin/git-connections lists the connections with their ids"})
		return
	}
	if err != nil {
		log.Printf("reading git connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	var body gitConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	// **Disabling is never refused and never license-gated.** It is the verb an
	// administrator reaches for in an incident, and a credential the platform
	// will not let you turn off is worse than one it will not let you edit.
	//
	// The carve-out is *only* the disable, and `onlyEnabledChanged` enumerates
	// every other field rather than the two or three worth remembering.
	// Coverage is a security boundary, so a request that flipped `enabled` and
	// widened coverage in the same body would otherwise carry a boundary change
	// past the gate on the back of the one field the gate lets through.
	disablingOnly := body.Enabled != nil && !*body.Enabled && existing.Enabled &&
		onlyEnabledChanged(&body)
	if !disablingOnly && s.refusedUnlicensed(w, r, entitlements.FeatureTrustedSources) {
		return
	}

	row := *existing
	if body.Enabled != nil {
		row.Enabled = *body.Enabled
	}
	if strings.TrimSpace(body.DisplayName) != "" {
		row.DisplayName = strings.TrimSpace(body.DisplayName)
	}
	if strings.TrimSpace(body.AuthMethod) != "" {
		row.AuthMethod = strings.TrimSpace(body.AuthMethod)
	}
	if strings.TrimSpace(body.BaseURL) != "" {
		row.BaseURL = strings.TrimSpace(body.BaseURL)
	}
	if body.Coverage != nil {
		row.Coverage = strings.Join(trimmedLines(body.Coverage), "\n")
	}
	if strings.TrimSpace(body.AppID) != "" {
		row.GitHubAppID = strings.TrimSpace(body.AppID)
	}
	if strings.TrimSpace(body.InstallationID) != "" {
		row.InstallationID = strings.TrimSpace(body.InstallationID)
	}
	if strings.TrimSpace(body.ClientID) != "" {
		row.ClientID = strings.TrimSpace(body.ClientID)
	}
	if strings.TrimSpace(body.CredentialOwner) != "" {
		row.CredentialOwner = strings.TrimSpace(body.CredentialOwner)
	}
	if body.CredentialExpiresAt.Set {
		row.CredentialExpiresAt = body.CredentialExpiresAt.Value
	}

	// **A credential goes only to the host it was entered for.** Moving the
	// base URL to another host would send the stored clone token, an App JWT
	// and the OAuth client secret there, so the move re-enters them; the
	// store drops the people's picker tokens in the same write (security
	// review 2026-11-14, DG-9). The sign-in providers refuse the same change.
	if !database.SameGitHost(existing.BaseURL, row.BaseURL) {
		missing := []string{}
		if derefOr(body.CloneSecret, "") == "" {
			missing = append(missing, "clone_secret")
		}
		if existing.ClientSecretSet && derefOr(body.ClientSecret, "") == "" {
			missing = append(missing, "client_secret")
		}
		if len(missing) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Moving " + existing.DisplayName + " to another host needs its credentials entered " +
					"again, so the stored ones are never sent there: send " + strings.Join(missing, " and ") +
					" with the new base URL.",
				"hint": "nothing was saved"})
			return
		}
	}
	if msg := validateGitConnection(&row, derefOr(body.CloneSecret, "")); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "correct the field the sentence names and send it again — nothing was saved"})
		return
	}
	if v := derefOr(body.CloneSecret, ""); v != "" {
		row.CredentialKind = gitconn.ClassifyCredential(row.Kind, row.AuthMethod, v)
	}
	// Clear the ciphertexts so the database layer's "empty means keep" applies
	// to exactly the fields this request did not carry.
	row.SetEncryptedCloneSecret("")
	row.SetEncryptedClientSecret("")
	row.SetEncryptedWebhookSecret("")
	if err := s.encryptGitConnectionSecrets(&row, &body); err != nil {
		log.Printf("encrypting git connection secrets: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID

	if err := s.db.UpdateGitConnection(&row, audit); err != nil {
		log.Printf("updating git connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// **The write is what drops the cache.** A disable takes effect on the next
	// mint and the next poll, with nothing to restart and no flush to remember.
	s.gitConn.Forget(row.ID)

	writeJSON(w, http.StatusOK, s.gitConnectionDTO(&row))
}

func (s *Server) handleDeleteGitConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "connectionID")

	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID

	err := s.db.DeleteGitConnection(id, audit)
	switch {
	case errors.Is(err, database.ErrGitConnectionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/admin/git-connections lists the connections with their ids"})
		return
	case errors.Is(err, database.ErrGitConnectionInUse):
		// The refusal names every app, because "something is using this" is not
		// a refusal an administrator can act on. Disable is the verb that is
		// never refused, and it is on the same page.
		apps, listErr := s.db.AppsUsingGitConnection(id)
		if listErr != nil {
			log.Printf("listing apps on git connection %s: %v", id, listErr)
		}
		names := make([]string, 0, len(apps))
		for _, a := range apps {
			names = append(names, a.Name)
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "This connection cannot be removed while " + countedApps(len(apps)) +
				" through it: " + strings.Join(names, ", ") +
				". Disable it instead, or move those apps first.",
			"code": "connection_in_use",
			"apps": apps, "hint": `disable it with PATCH /api/v1/admin/git-connections/{id} and "enabled": false, or move the apps listed in "apps" first`,
		})
		return
	case err != nil:
		log.Printf("deleting git connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.gitConn.Forget(id)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestGitConnection makes one authenticated call to the host and reports
// what happened.
//
// **Not license-gated, and that is the same rule the create is gated by, read
// the other way.** A license gates configuration and never operation: creating
// a connection needs one, cloning through an existing one consults no
// entitlement, and finding out *why* an existing one stopped cloning is the
// operation half. An install whose license has lapsed keeps deploying every app
// it already connected, so it must keep being able to diagnose them.
//
// The result is written to the connection's health, which is what makes this
// button worth more than a toast: the row on this page already renders
// `last_error` and `last_ok_at`, and until now the only thing that ever wrote
// them was a webhook delivery arriving. An install with no inbound reachability
// had a health field nothing could fill.
//
// It answers 200 with `ok: false` rather than an error status when the host
// refuses. The diagnostic succeeded — it found out the credential is wrong,
// which is exactly what was asked — and a 502 here would have the browser's
// error path swallow the sentence that says which field to go and fix.
func (s *Server) handleTestGitConnection(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "connectionID")
	conn, err := s.db.GetGitConnection(id)
	if errors.Is(err, database.ErrGitConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/admin/git-connections lists the connections with their ids"})
		return
	}
	if err != nil {
		log.Printf("reading git connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if s.gitConn == nil {
		// The resolver is what holds the host clients and the encryptor, so
		// without one there is nothing to test *with*. Said rather than
		// panicked, on the mailer's rule: an absent capability explains itself.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this install was started without git connection support"})
		return
	}

	// Bounded here as well as by the host client's own timeout, so a host that
	// accepts a connection and then says nothing cannot hold an admin request
	// open for as long as it likes.
	ctx, cancel := context.WithTimeout(r.Context(), gitConnectionTestTimeout)
	defer cancel()
	result := s.gitConn.Check(ctx, conn)

	// Recorded on the row, so the page and the toast say the same thing and a
	// pass clears a stale failure. A success writes `last_ok_at`; a failure
	// writes the same sentence the administrator is about to read.
	failure := ""
	if !result.OK {
		failure = result.Summary
	}
	if _, err := s.db.RecordGitConnectionResult(id, failure); err != nil {
		// The test is the answer and it has already been obtained. Failing the
		// request because the health column would not update would throw away
		// the one thing the administrator asked for.
		log.Printf("recording the result of testing git connection %s: %v", id, err)
	}

	writeJSON(w, http.StatusOK, result)
}

// handleListGitConnectionApps is what the page reads to show the dependants
// *before* an administrator tries to remove a connection.
func (s *Server) handleListGitConnectionApps(w http.ResponseWriter, r *http.Request) {
	if !s.requireConnectionAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "connectionID")
	if _, err := s.db.GetGitConnection(id); errors.Is(err, database.ErrGitConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/admin/git-connections lists the connections with their ids"})
		return
	}
	apps, err := s.db.AppsUsingGitConnection(id)
	if err != nil {
		log.Printf("listing apps on git connection %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

// --- helpers ---

// gitConnectionTestTimeout bounds one test. Longer than the host client's own
// per-request timeout because the App method makes two calls, and short enough
// that an administrator pressing the button is not left wondering.
const gitConnectionTestTimeout = 45 * time.Second

// onlyEnabledChanged reports that this request carries the `enabled` field and
// nothing else.
//
// Written as an enumeration of every other field on purpose: the alternative is
// a list of the ones somebody thought were dangerous, which is a list that goes
// out of date the first time the request shape gains a field. A field added
// without a line here makes the license carve-out wider, silently — so the
// compiler cannot catch it, but a reader adding a field to the struct above has
// this immediately below to update.
func onlyEnabledChanged(body *gitConnectionRequest) bool {
	return body.CloneSecret == nil &&
		body.ClientSecret == nil &&
		!body.CredentialExpiresAt.Set &&
		body.Coverage == nil &&
		strings.TrimSpace(body.Kind) == "" &&
		strings.TrimSpace(body.DisplayName) == "" &&
		strings.TrimSpace(body.BaseURL) == "" &&
		strings.TrimSpace(body.AuthMethod) == "" &&
		strings.TrimSpace(body.AppID) == "" &&
		strings.TrimSpace(body.InstallationID) == "" &&
		strings.TrimSpace(body.ClientID) == "" &&
		strings.TrimSpace(body.CredentialOwner) == ""
}

func applyGitConnectionFields(row *database.GitConnection, body *gitConnectionRequest) {
	row.GitHubAppID = strings.TrimSpace(body.AppID)
	row.InstallationID = strings.TrimSpace(body.InstallationID)
	row.ClientID = strings.TrimSpace(body.ClientID)
	row.CredentialOwner = strings.TrimSpace(body.CredentialOwner)
	row.CredentialExpiresAt = body.CredentialExpiresAt.Value
	row.Coverage = strings.Join(trimmedLines(body.Coverage), "\n")
}

func (s *Server) encryptGitConnectionSecrets(row *database.GitConnection, body *gitConnectionRequest) error {
	for _, pair := range []struct {
		value *string
		set   func(string)
	}{
		{body.CloneSecret, row.SetEncryptedCloneSecret},
		{body.ClientSecret, row.SetEncryptedClientSecret},
	} {
		if pair.value == nil || *pair.value == "" {
			continue
		}
		encrypted, err := s.encryptor.Encrypt(*pair.value)
		if err != nil {
			return err
		}
		pair.set(encrypted)
	}
	return nil
}

// defaultBaseURL fills in the public host by kind. It is a **value in the row**
// and never a constant: a GitHub Enterprise Server is the ordinary case for the
// organizations this feature is for, and a clone is not a login, so baking the
// endpoint in costs more here than it did for sign-in.
func defaultBaseURL(kind, given string) string {
	if given != "" {
		return strings.TrimRight(given, "/")
	}
	switch kind {
	case database.GitConnGitLab:
		return "https://gitlab.com"
	default:
		return "https://github.com"
	}
}

func validateGitConnection(row *database.GitConnection, cloneSecret string) string {
	switch row.Kind {
	case database.GitConnGitHub, database.GitConnGitLab:
	default:
		return "kind must be `github` or `gitlab`"
	}
	switch row.AuthMethod {
	case database.GitAuthApp:
		if row.Kind != database.GitConnGitHub {
			return "the `app` method is GitHub's; a GitLab connection authenticates with a group access token"
		}
	case database.GitAuthToken:
	default:
		return "auth_method must be `app` or `token`"
	}
	if row.DisplayName == "" {
		return "a connection needs a name an administrator will recognise"
	}
	// https only, no credentials in the authority, and a real host. A
	// self-managed host on plain HTTP is refused here rather than at the first
	// clone.
	if err := safefetch.CheckURL(row.BaseURL); err != nil {
		return "the host address " + err.Error()
	}
	if !row.CloneSecretSet && cloneSecret == "" {
		if row.AuthMethod == database.GitAuthApp {
			return "an App-backed connection needs the App's private key"
		}
		return "a token-backed connection needs an access token"
	}
	if row.AuthMethod == database.GitAuthApp && row.GitHubAppID == "" {
		return "an App-backed connection needs the App id"
	}
	// **Coverage is the boundary on what a developer may attach**, and a token
	// connection has no host to ask. An empty pattern list would mean the
	// credential covers nothing, which is a connection nobody can use, or
	// everything, which is a boundary that is not one.
	if row.AuthMethod == database.GitAuthToken && len(row.CoverageList()) == 0 {
		return "a token-backed connection needs the repositories it covers — `acme/*`, or named repositories, one per line"
	}
	return ""
}

func trimmedLines(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func countedApps(n int) string {
	if n == 1 {
		return "1 app deploys"
	}
	return strconv.Itoa(n) + " apps deploy"
}

// newWebhookSecret mints the HMAC key a delivery is verified against.
func newWebhookSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
