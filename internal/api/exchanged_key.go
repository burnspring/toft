package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/auth"
	"toft/internal/authprovider"
	"toft/internal/database"
)

// A workbench session's identity, exchanged for a short-lived key (ADR §157).
//
// An administrator names an OIDC issuer and the audience its identity tokens
// must carry (Admin → Sign-in → Trusted issuers). A caller holding such a
// token presents it here, RFC 8693-shaped, and gets back an `toftx_` key for
// the person the token names:
//
//   - **The person must already exist.** The token names them by `sub` on the
//     issuer's sign-in provider, or by a verified `email`; nothing here creates
//     an account.
//   - **The key is the person's, and narrow.** It authenticates exactly as a
//     personal key does — the owner's live role, groups and lock, every
//     request — and the route guard bounds it to the deploy surface, which is
//     deployKeyAllowedRoutes plus the Posit Connect publishing routes.
//   - **It ends on its own.** The issuer's lifetime, one hour to one day, and
//     revocable by whoever holds it and by an administrator. Switching the
//     issuer off or deleting it ends every key it vouched for, on the next
//     request.
//
// Two routes serve the one handler: Toft's own, `POST /api/v1/auth/token-exchange`,
// and rsconnect's, `POST /__api__/v1/oauth/integrations/credentials`, which
// Posit Workbench's rsconnect tries before the token flow.
//
// **No credential mints one.** Both routes ignore a presented key — the first
// sits in the auth group where keys are not read, the second is on neither key
// allow-list — so an exchanged key cannot buy its own successor.

// exchangedKeySecretPrefix identifies a short-lived key an exchange minted.
// Not `toftu_`: the prefix decides the class before anything is looked up, and
// this one must never be mistaken for a long-lived personal key.
const exchangedKeySecretPrefix = "toftx_"

// The RFC 8693 vocabulary the exchange speaks.
const (
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeIDToken       = "urn:ietf:params:oauth:token-type:id_token"
	tokenTypeJWT           = "urn:ietf:params:oauth:token-type:jwt"
	// What rsconnect asks for, and what it is answered with.
	tokenTypeConnectAPIKey = "urn:posit:connect:api-key"
	// Toft's own word for what it issues.
	tokenTypeToftKey = "urn:toft:params:oauth:token-type:api-key"
)

// exchangedKeyStorageRoutes is the storage a session key reaches beyond the
// deploy scope: attaching a resource its owner already manages to an app, and
// the /store data plane under its owner's grants. The deploy key is refused
// both because it belongs to nobody, so there is no answer to "which grants
// does it hold"; an exchanged key is a person's, and reaches exactly what that
// person reaches — an agent session that deploys an app declaring storage, or
// stages its data, needs both. Granting and managing stay off, because those
// decide who may use a store rather than using it.
var exchangedKeyStorageRoutes = []string{
	"GET /api/v1/apps/{appID}/storage",
	"PUT /api/v1/apps/{appID}/storage/{resourceID}",
	"DELETE /api/v1/apps/{appID}/storage/{resourceID}",
	"GET /api/v1/me/storage",
	"GET /api/v1/store",
	"GET /api/v1/store/{slug}/objects",
	"POST /api/v1/store/{slug}/objects",
	"GET /api/v1/store/{slug}/objects/*",
	"DELETE /api/v1/store/{slug}/objects/*",
	"POST /api/v1/store/{slug}/folders",
	"DELETE /api/v1/store/{slug}/folders/*",
}

// exchangedKeyAllowedRoutes is what an `toftx_` key may call: the deploy
// key's deploy scope — create, push, watch, roll back, the environment, the
// jobs a deploy runs — its owner's storage (exchangedKeyStorageRoutes), and
// the Posit Connect publishing surface, because rsconnect is one of the two
// callers. Derived, not hand-written, so it moves with the lists it is made
// of; TestExchangedKeyRoutesAreDeployScoped holds it inside the personal
// key's list.
var exchangedKeyAllowedRoutes = func() map[string]bool {
	out := map[string]bool{}
	for k := range deployKeyAllowedRoutes {
		out[k] = true
	}
	for _, k := range exchangedKeyStorageRoutes {
		out[k] = true
	}
	for k := range apiKeyAllowedRoutes {
		if _, path, ok := strings.Cut(k, " "); ok && strings.HasPrefix(path, connectRoutePrefix+"/") {
			out[k] = true
		}
	}
	return out
}()

func generateExchangedKeySecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return exchangedKeySecretPrefix + hex.EncodeToString(buf), nil
}

// exchangedKeyAuth is apiKeyAuth's half for an `toftx_` key. Its refusals are
// the personal key's, plus the two this class has of its own: revoked, and an
// issuer that no longer vouches.
func (s *Server) exchangedKeyAuth(w http.ResponseWriter, r *http.Request, next http.Handler, secret string) {
	key, err := s.db.ResolveExchangedKey(hashAPIKey(secret))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	refuse := func(msg, code, hint string) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg, "code": code, "hint": hint})
	}
	const again = "exchange a fresh identity token for a new key at POST /api/v1/auth/token-exchange"
	switch {
	case key == nil:
		refuse("invalid API key", "invalid_key", again)
		return
	case !key.Enabled:
		refuse("personal API keys are switched off on this install", "api_keys_disabled",
			"ask an administrator to turn personal API keys back on, or sign in through the browser")
		return
	case key.RevokedAt != nil:
		refuse("this key was revoked", "key_revoked", again)
		return
	case !key.IssuerEnabled:
		refuse("the issuer that vouched for this key ("+key.IssuerName+") is switched off",
			"issuer_disabled", "ask an administrator to turn the trusted issuer back on")
		return
	case !key.ExpiresAt.After(time.Now().UTC()):
		refuse("this key expired at "+key.ExpiresAt.UTC().Format(time.RFC3339), "key_expired", again)
		return
	case key.Locked:
		refuse("the account this API key belongs to is locked", "owner_locked",
			"ask an administrator to unlock the account")
		return
	case key.Pending:
		refuse("the account this API key belongs to is awaiting activation", "owner_pending",
			"ask an administrator to activate the account")
		return
	}
	groups, err := s.db.GetUserGroupIDs(key.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.touchExchangedKey(key.KeyID, clientIP(r))
	ctx := auth.WithPrincipal(r.Context(), &auth.Principal{
		Type:         database.PrincipalTypeUser,
		ID:           key.UserID,
		SystemRole:   key.SystemRole,
		Groups:       groups,
		APIKeyID:     key.KeyID,
		APIKeyName:   key.KeyName,
		APIKeyScope:  key.KeyScope,
		APIKeyIssuer: key.IssuerID,
	})
	next.ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) touchExchangedKey(id, ip string) {
	now := time.Now()
	if v, ok := s.apiKeyTouched.Load(id); ok && now.Sub(v.(time.Time)) < time.Minute {
		return
	}
	s.apiKeyTouched.Store(id, now)
	go func() {
		if err := s.db.TouchExchangedKey(id, now.UTC(), ip); err != nil {
			log.Printf("exchanged-key: recording last use of %s: %v", id, err)
		}
	}()
}

// exchangeRefusal is one way an exchange is refused: RFC 6749's error word,
// which rsconnect and any OAuth client read, beside this API's code and hint.
func exchangeRefusal(w http.ResponseWriter, status int, oauthErr, code, msg, hint string) {
	writeJSON(w, status, map[string]string{
		"error": msg, "code": code, "hint": hint, "oauth_error": oauthErr})
}

// handleTokenExchange mints an `toftx_` key for the person a trusted issuer's
// identity token names.
func (s *Server) handleTokenExchange(w http.ResponseWriter, r *http.Request) {
	// An exchanged key is a personal key's narrower sibling, so it is under
	// the same switch — off means absent, as for `toft login`.
	if s.personalKeysAbsent(w) {
		return
	}
	form, err := readExchangeForm(r)
	if err != nil {
		exchangeRefusal(w, http.StatusBadRequest, "invalid_request", "invalid_body",
			"the exchange is a form: grant_type, subject_token_type and subject_token",
			"send application/x-www-form-urlencoded with grant_type="+grantTypeTokenExchange)
		return
	}
	if form.Get("grant_type") != grantTypeTokenExchange {
		exchangeRefusal(w, http.StatusBadRequest, "unsupported_grant_type", "unsupported_grant_type",
			"this route exchanges tokens and nothing else", "send grant_type="+grantTypeTokenExchange)
		return
	}
	switch form.Get("subject_token_type") {
	case tokenTypeIDToken, tokenTypeJWT:
	default:
		exchangeRefusal(w, http.StatusBadRequest, "invalid_request", "unsupported_token_type",
			"only an OIDC identity token can be exchanged", "send subject_token_type="+tokenTypeIDToken)
		return
	}
	issuedType := tokenTypeToftKey
	switch form.Get("requested_token_type") {
	case "", tokenTypeToftKey:
	case tokenTypeConnectAPIKey:
		issuedType = tokenTypeConnectAPIKey
	default:
		exchangeRefusal(w, http.StatusBadRequest, "invalid_request", "unsupported_token_type",
			"this install issues API keys and nothing else",
			"leave requested_token_type out, or send "+tokenTypeToftKey)
		return
	}
	raw := strings.TrimSpace(form.Get("subject_token"))
	if raw == "" {
		exchangeRefusal(w, http.StatusBadRequest, "invalid_request", "token_required",
			"subject_token is required", "send the identity token as subject_token")
		return
	}

	issuer, verified, err := s.verifyAgainstTrustedIssuers(r.Context(), raw)
	if err != nil {
		log.Printf("token-exchange: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if issuer == nil {
		exchangeRefusal(w, http.StatusUnauthorized, "invalid_grant", "issuer_not_trusted",
			"this install does not trust that token — its issuer and audience are not a trusted issuer here, "+
				"or its signature or expiry did not check out",
			"an administrator names trusted issuers on Admin → Sign-in")
		return
	}

	// A `sub` issuer vouches for identities on one sign-in provider; with
	// that provider gone the identities it names are nobody's, and the schema
	// comment's "a vanished provider leaves the issuer inert" has to be true
	// here, since DeleteAuthProvider keeps user_identities (A-4).
	//
	// And a disabled provider is closed here as at sign-in and SCIM: disabling
	// is the reversible half of removal, so it stops the exchange too.
	if issuer.SubjectClaim == database.SubjectClaimSub {
		exists, enabled := s.providerState(issuer.ProviderID)
		if !exists {
			exchangeRefusal(w, http.StatusForbidden, "invalid_grant", "provider_gone",
				"the sign-in provider "+issuer.ProviderID+" that "+issuer.DisplayName+" vouches for identities on no longer exists on this install",
				"an administrator can point the trusted issuer at another provider, or disable it, on Admin → Sign-in")
			return
		}
		if !enabled {
			exchangeRefusal(w, http.StatusForbidden, "invalid_grant", "provider_disabled",
				"the sign-in provider "+issuer.ProviderID+" that "+issuer.DisplayName+" vouches for identities on is disabled on this install",
				"an administrator can enable the provider again on Admin → Sign-in")
			return
		}
	}
	subject := verified.Subject
	if issuer.SubjectClaim == database.SubjectClaimEmail {
		if verified.Email == "" || !verified.EmailVerified {
			exchangeRefusal(w, http.StatusForbidden, "invalid_grant", "email_not_verified",
				"the token carries no verified email, which is how "+issuer.DisplayName+" names a person here",
				"the issuer must include email and email_verified=true in its identity tokens")
			return
		}
		subject = verified.Email
	}
	userID, err := s.db.FindUserForIssuer(issuer, subject)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if userID == "" {
		exchangeRefusal(w, http.StatusForbidden, "invalid_grant", "no_account",
			"nobody on this install is the person that token names",
			"sign in to this install once through the browser, so the account exists, then try again")
		return
	}
	user, err := s.db.GetUser(userID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if user.LockedAt != nil {
		exchangeRefusal(w, http.StatusForbidden, "invalid_grant", "owner_locked",
			"the account that token names is locked", "ask an administrator to unlock the account")
		return
	}
	// An unactivated account is refused at every sign-in path, this one too:
	// the first browser sign-in creates it and refuses the session, and the
	// exchange must not then hand it a key (security review 2026-11-10, A-1).
	if user.PendingActivation() {
		exchangeRefusal(w, http.StatusForbidden, "invalid_grant", "owner_pending",
			"the account that token names is awaiting an administrator's activation",
			"ask an administrator to activate the account")
		return
	}
	secret, err := generateExchangedKeySecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	lifetime := time.Duration(issuer.KeyLifetimeMinutes) * time.Minute
	key := &database.ExchangedKey{
		ID:           uuid.New().String(),
		UserID:       userID,
		IssuerID:     issuer.ID,
		KeyPrefix:    keyDisplayPrefix(secret),
		TokenSubject: subject,
		ExpiresAt:    time.Now().UTC().Add(lifetime),
	}
	audit := auditEntryFor(r)
	audit.ActorType, audit.ActorID = database.PrincipalTypeUser, userID
	audit.Action, audit.TargetType, audit.TargetID = "exchanged_key.create", "exchanged_key", key.ID
	audit.After = jsonValStr(map[string]any{
		"issuer_id": issuer.ID, "issuer": issuer.DisplayName, "subject": subject,
		"expires_at": key.ExpiresAt.Format(time.RFC3339),
	})
	if err := s.db.CreateExchangedKeyTx(key, hashAPIKey(secret), audit, maxExchangedKeysPerIssuer); err != nil {
		log.Printf("token-exchange: storing the key: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":      secret,
		"issued_token_type": issuedType,
		"token_type":        "Bearer",
		"expires_in":        int(lifetime.Seconds()),
		"expires_at":        key.ExpiresAt.Format(time.RFC3339),
		"key_id":            key.ID,
	})
}

// verifyAgainstTrustedIssuers finds the trusted row a token was issued for.
// The issuer is read unverified only to choose which rows to try; a row is
// returned only once the token has verified against it — signature, issuer,
// that row's audience, expiry.
func (s *Server) verifyAgainstTrustedIssuers(ctx context.Context, raw string) (*database.TrustedIssuer, *authprovider.VerifiedToken, error) {
	iss := authprovider.PeekIssuer(raw)
	if iss == "" {
		return nil, nil, nil
	}
	rows, err := s.db.EnabledTrustedIssuersFor(iss)
	if err != nil {
		return nil, nil, err
	}
	for i := range rows {
		v, err := authprovider.VerifyIDToken(ctx, raw, rows[i].Issuer, rows[i].Audience)
		if err != nil {
			log.Printf("token-exchange: %s (%s): %v", rows[i].DisplayName, rows[i].ID, err)
			continue
		}
		return &rows[i], v, nil
	}
	return nil, nil, nil
}

// readExchangeForm reads the exchange's form body, and also accepts the same
// fields as JSON — a caller written in a language whose HTTP client makes a
// form awkward should not have to learn one.
func readExchangeForm(r *http.Request) (url.Values, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<10)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]string
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			return nil, err
		}
		v := url.Values{}
		for k, val := range m {
			v.Set(k, val)
		}
		return v, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.PostForm, nil
}

// handleExchangedKeyRevoke is RFC 7009's revocation: whoever holds the key
// may end it, presenting it as `token`. Answered 200 whether or not the token
// was a live key, as the RFC asks, so the route says nothing about which
// strings are keys.
//
// It ends a personal key (toftu_) the same way. A key may not list, mint or
// delete keys (apiKeyDeniedRoutes), which left a key that a program holds —
// Garth's for a session, a CI job's — no way to end itself: revoking only
// takes away, the presented key is the only one it can reach (there is no id
// to aim with), and a thief who ends a stolen key has locked themselves out.
// A disabled or expired key, or one whose owner is locked, is ended too.
func (s *Server) handleExchangedKeyRevoke(w http.ResponseWriter, r *http.Request) {
	form, err := readExchangeForm(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body",
			"hint": "send application/x-www-form-urlencoded with token=<the toftx_ or toftu_ key>"})
		return
	}
	secret := strings.TrimSpace(form.Get("token"))
	switch {
	case strings.HasPrefix(secret, exchangedKeySecretPrefix):
		key, err := s.db.GetExchangedKeyByHash(hashAPIKey(secret))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if key != nil {
			audit := auditEntryFor(r)
			audit.ActorType, audit.ActorID = database.PrincipalTypeUser, key.UserID
			audit.APIKeyID = key.ID
			audit.Action, audit.TargetType, audit.TargetID = "exchanged_key.revoke", "exchanged_key", key.ID
			audit.After = jsonValStr(map[string]any{"by": "holder", "issuer": key.IssuerName})
			if _, err := s.db.RevokeExchangedKeyTx(key.ID, audit); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}
	case strings.HasPrefix(secret, userKeySecretPrefix):
		key, err := s.db.ResolveUserAPIKey(hashAPIKey(secret))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if key != nil {
			// The owner's own revocation, as on Settings → API keys, marked
			// as the holder's.
			audit := auditEntryFor(r)
			audit.ActorType, audit.ActorID = database.PrincipalTypeUser, key.UserID
			audit.APIKeyID = key.KeyID
			audit.Action, audit.TargetType, audit.TargetID = "user.api_key_revoke", "user", key.UserID
			audit.Before = jsonValStr(map[string]string{"key_id": key.KeyID, "name": key.KeyName})
			audit.After = jsonValStr(map[string]any{"by": "holder"})
			if err := s.db.DeleteUserAPIKeyTx(key.KeyID, audit); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// --- the administrator's half -------------------------------------------
//
// Every route below is behind requireAdmin and in apiKeyDeniedRoutes: naming
// who may vouch for a person is changing who may do what, which no key does.

type trustedIssuerRequest struct {
	DisplayName        *string `json:"display_name"`
	Issuer             *string `json:"issuer"`
	Audience           *string `json:"audience"`
	SubjectClaim       *string `json:"subject_claim"`
	ProviderID         *string `json:"provider_id"`
	KeyLifetimeMinutes *int    `json:"key_lifetime_minutes"`
	Enabled            *bool   `json:"enabled"`
}

// apply writes the request onto t and returns the refusal, or "".
func (req trustedIssuerRequest) apply(t *database.TrustedIssuer, providerExists func(string) bool) (msg, hint string) {
	if req.DisplayName != nil {
		t.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Issuer != nil {
		t.Issuer = strings.TrimSuffix(strings.TrimSpace(*req.Issuer), "/")
	}
	if req.Audience != nil {
		t.Audience = strings.TrimSpace(*req.Audience)
	}
	if req.SubjectClaim != nil {
		t.SubjectClaim = strings.TrimSpace(*req.SubjectClaim)
	}
	if req.ProviderID != nil {
		t.ProviderID = strings.TrimSpace(*req.ProviderID)
	}
	if req.KeyLifetimeMinutes != nil {
		t.KeyLifetimeMinutes = *req.KeyLifetimeMinutes
	}
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	}
	switch {
	case t.DisplayName == "":
		return "display_name is required", `name the caller, as "display_name": "Posit Workbench"`
	case t.Audience == "":
		return "audience is required", "the client id the issuer puts in aud on the tokens it gives this caller"
	case t.SubjectClaim != database.SubjectClaimSub && t.SubjectClaim != database.SubjectClaimEmail:
		return "subject_claim is sub or email", `send "subject_claim": "sub" (an identity on provider_id) or "email"`
	case t.SubjectClaim == database.SubjectClaimSub && t.ProviderID == "":
		return "a sub names an identity on a sign-in provider: provider_id is required",
			"GET /api/v1/admin/auth-providers lists them; or send \"subject_claim\": \"email\""
	// Checked when the provider is being named, not on every edit: a
	// provider deleted after the fact left the issuer with no working off
	// switch, because `{"enabled": false}` was refused for the very reason
	// it was sent (A-4). The exchange refuses such an issuer on its own.
	case t.SubjectClaim == database.SubjectClaimSub && (req.ProviderID != nil || req.SubjectClaim != nil) && !providerExists(t.ProviderID):
		return "there is no sign-in provider " + t.ProviderID, "GET /api/v1/admin/auth-providers lists them"
	case t.KeyLifetimeMinutes < database.ExchangedKeyMinMinutes || t.KeyLifetimeMinutes > database.ExchangedKeyMaxMinutes:
		return "key_lifetime_minutes must be between 60 and 1440", "an hour to a day; 480 is the default"
	}
	if err := checkIssuerURL(t.Issuer); err != "" {
		return err, "the issuer URL exactly as the tokens' iss claim carries it, https"
	}
	return "", ""
}

func checkIssuerURL(raw string) string {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" {
		return "issuer must be the issuer's URL"
	}
	if u.Scheme == "https" {
		return ""
	}
	if u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") {
		return ""
	}
	return "issuer must be https (plain http only on loopback, for local development)"
}

// providerExists asks the merged registry — the environment's providers and
// the table's, the same list the login page and the admin page show — not the
// table alone, which refused a `sub` issuer naming an OIDC_* provider that
// GET /admin/auth-providers lists (code review 2026-11-11, A-3).
func (s *Server) providerExists(id string) bool {
	exists, _ := s.providerState(id)
	return exists
}

// providerState is providerExists plus whether the provider is enabled.
func (s *Server) providerState(id string) (exists, enabled bool) {
	list, err := s.authProviders()
	if err != nil {
		return false, false
	}
	for _, p := range list {
		if p.ID == id {
			return true, p.Enabled
		}
	}
	return false, false
}

func (s *Server) handleListTrustedIssuers(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	list, err := s.db.ListTrustedIssuers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateTrustedIssuer(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	var req trustedIssuerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	t := &database.TrustedIssuer{
		ID: uuid.New().String(), SubjectClaim: database.SubjectClaimSub,
		KeyLifetimeMinutes: database.ExchangedKeyDefaultMinutes, Enabled: true, CreatedBy: p.ID,
	}
	if msg, hint := req.apply(t, s.providerExists); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": "invalid_issuer", "hint": hint})
		return
	}
	audit := auditEntryFor(r)
	audit.ActorType, audit.ActorID = database.PrincipalTypeUser, p.ID
	audit.Action, audit.TargetType, audit.TargetID = "trusted_issuer.create", "trusted_issuer", t.ID
	audit.After = jsonValStr(trustedIssuerAudit(t))
	if err := s.db.CreateTrustedIssuerTx(t, audit); err != nil {
		if err == database.ErrTrustedIssuerExists {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "issuer_exists",
				"hint": "edit the existing row: GET /api/v1/admin/trusted-issuers lists them"})
			return
		}
		log.Printf("trusted-issuer: create: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleUpdateTrustedIssuer(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	t, err := s.db.GetTrustedIssuer(chi.URLParam(r, "issuerID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if t == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found",
			"hint": "GET /api/v1/admin/trusted-issuers lists them with their ids"})
		return
	}
	var req trustedIssuerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	before := trustedIssuerAudit(t)
	oldIssuer := t.Issuer
	if msg, hint := req.apply(t, s.providerExists); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": "invalid_issuer", "hint": hint})
		return
	}
	audit := auditEntryFor(r)
	audit.ActorType, audit.ActorID = database.PrincipalTypeUser, p.ID
	audit.Action, audit.TargetType, audit.TargetID = "trusted_issuer.update", "trusted_issuer", t.ID
	audit.Before, audit.After = jsonValStr(before), jsonValStr(trustedIssuerAudit(t))
	if err := s.db.UpdateTrustedIssuerTx(t, audit); err != nil {
		if err == database.ErrTrustedIssuerExists {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "issuer_exists",
				"hint": "another row already trusts that issuer for that audience"})
			return
		}
		log.Printf("trusted-issuer: update: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	authprovider.ForgetJWKS(oldIssuer)
	authprovider.ForgetJWKS(t.Issuer)
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTrustedIssuer(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	t, err := s.db.GetTrustedIssuer(chi.URLParam(r, "issuerID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if t == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found",
			"hint": "GET /api/v1/admin/trusted-issuers lists them with their ids"})
		return
	}
	audit := auditEntryFor(r)
	audit.ActorType, audit.ActorID = database.PrincipalTypeUser, p.ID
	audit.Action, audit.TargetType, audit.TargetID = "trusted_issuer.delete", "trusted_issuer", t.ID
	audit.Before = jsonValStr(trustedIssuerAudit(t))
	if err := s.db.DeleteTrustedIssuerTx(t.ID, audit); err != nil {
		log.Printf("trusted-issuer: delete: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	authprovider.ForgetJWKS(t.Issuer)
	w.WriteHeader(http.StatusNoContent)
}

func trustedIssuerAudit(t *database.TrustedIssuer) map[string]any {
	return map[string]any{
		"display_name": t.DisplayName, "issuer": t.Issuer, "audience": t.Audience,
		"subject_claim": t.SubjectClaim, "provider_id": t.ProviderID,
		"key_lifetime_minutes": t.KeyLifetimeMinutes, "enabled": t.Enabled,
	}
}

func (s *Server) handleListExchangedKeys(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	list, err := s.db.ListActiveExchangedKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRevokeExchangedKey(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	key, err := s.db.GetExchangedKey(chi.URLParam(r, "keyID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if key == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found",
			"hint": "GET /api/v1/admin/exchanged-keys lists the live ones"})
		return
	}
	audit := auditEntryFor(r)
	audit.ActorType, audit.ActorID = database.PrincipalTypeUser, p.ID
	audit.Action, audit.TargetType, audit.TargetID = "exchanged_key.revoke", "exchanged_key", key.ID
	audit.After = jsonValStr(map[string]any{"by": "administrator", "owner": key.UserLogin, "issuer": key.IssuerName})
	if _, err := s.db.RevokeExchangedKeyTx(key.ID, audit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
