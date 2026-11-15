package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
)

// handleAppToken is POST /api/v1/app/tokens/{kind}: the attached connection's
// own short-lived access token, so the app can call Graph or Drive with the
// provider's SDK and hold no secret.
//
// Four refusals stand between an app and the token, in order: the integration
// gates (appint.Check, which also refuses while the connection's issue_tokens
// switch is off), and for Graph the token's own roles, read on every mint
// (graph.CheckFileRoles). What is handed out reaches the connection's whole
// grant; the folder in the answer is a hint, not a fence.
//
// **The token is never written down.** The ledger row records the kind, the
// action, the connection's scope and the expiry, and nothing token-shaped.
func (s *Server) handleAppToken(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	appID := token.Token.AppID
	// A kind that issues no tokens has no token action, so Check answers it
	// as an unknown capability.
	capability := chi.URLParam(r, "kind") + "." + appint.ActionToken

	decision, err := appint.Check(s.db, appID, token.AppStatus, capability)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !decision.Allowed {
		if decision.Code == "unknown_capability" {
			decision.Hint = appint.UnknownTokenKindHint()
		}
		writeJSON(w, decision.Status, decisionBody(decision))
		return
	}
	kind := appint.Lookup(appint.IntegrationOf(capability))
	issuer, ok := s.transports[kind.Name].(integrations.Issuer)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "app integrations are not available on this install"})
		return
	}

	attachment, err := s.db.ResolveAttachment(appID, kind.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	target, err := s.connectionTarget(attachment.ConnectionID, attachment.Destination)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The row is written before the mint, as a send's is before its attempt.
	row := &database.IntegrationSend{
		ID:           uuid.NewString(),
		AppID:        appID,
		Kind:         kind.Name,
		Action:       appint.ActionToken,
		Status:       database.SendQueued,
		Destination:  target.Connection.ScopeLabel,
		DeploymentID: token.Token.DeploymentID,
	}
	if row.Destination == "" {
		row.Destination = target.Connection.ScopeID
	}
	if err := s.db.InsertSend(row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	issued, err := issuer.Token(r.Context(), target)
	var wide *integrations.TokenTooWideError
	switch {
	case errors.As(err, &wide):
		_ = s.db.CompleteSend(row.ID, database.SendBlocked, err.Error())
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "code": "token_too_wide", "roles": wide.Roles, "hint": "ask an administrator to remove the extra permissions from the app registration, or to give this connection one of its own"})
		return
	case err != nil:
		_ = s.db.CompleteSend(row.ID, database.SendFailed, err.Error())
		slog.Warn("an app token request failed", "app", token.AppSlug, "kind", kind.Name,
			"error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": err.Error(), "code": "token_failed"})
		return
	}
	expires := issued.ExpiresAt.UTC().Truncate(time.Second)
	if err := s.db.SetSendSubject(row.ID, "expires "+expires.Format(time.RFC3339)); err != nil {
		slog.Warn("recording a token's expiry failed", "send", row.ID, "error", err)
	}
	_ = s.db.CompleteSend(row.ID, database.SendSent, "")

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": issued.Value,
		"token_type":   "Bearer",
		"expires_at":   expires,
		"resource":     issued.Resource,
		"folder":       issued.Folder,
	})
}
