package api

import (
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"toft/internal/auth"
	"toft/internal/database"
)

// userKeySecretPrefix identifies a personal API key. It deliberately does not
// begin with retiredAppKeyPrefix ("toft_"), so a removed app key can be refused
// by name and neither can be mistaken for the other by a prefix check.
const userKeySecretPrefix = "toftu_"

// userAPIKeyFromRequest extracts a presented personal API key, or "". Both
// header forms are accepted, matching the apps gateway: a client that already
// knows how to talk to a deployed app should not have to learn a second
// convention to talk to the platform.
func userAPIKeyFromRequest(r *http.Request) string {
	return presentedKeyWithPrefix(r, userKeySecretPrefix)
}

// exchangedKeyFromRequest extracts a presented exchanged key (`toftx_`), or "".
func exchangedKeyFromRequest(r *http.Request) string {
	return presentedKeyWithPrefix(r, exchangedKeySecretPrefix)
}

func presentedKeyWithPrefix(r *http.Request, prefix string) string {
	if k := r.Header.Get("X-API-Key"); strings.HasPrefix(k, prefix) {
		return k
	}
	if tok := bearerToken(r); strings.HasPrefix(tok, prefix) {
		return tok
	}
	return ""
}

// bearerToken is the token of a Bearer Authorization header, or "". The scheme
// is matched case-insensitively, as RFC 9110 says it must be and as every IdP
// picks its own capitalisation for SCIM: `bearer toftu_…` used to be recognised
// by nothing at the gateway and so went through to the app as an unrecognised
// header (code review 2026-11-11, A-1).
func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// stripPresentedKey removes a credential the gateway has read from the request
// before it is forwarded, so no Toft secret reaches an app's code.
func stripPresentedKey(r *http.Request, prefix string) {
	if strings.HasPrefix(r.Header.Get("X-API-Key"), prefix) {
		r.Header.Del("X-API-Key")
	}
	if strings.HasPrefix(bearerToken(r), prefix) {
		r.Header.Del("Authorization")
	}
}

// unreadCredentialPresented reports a request that carries a bearer token or
// an X-API-Key that is neither a personal key nor a deploy key. Only the
// Bearer scheme counts: another scheme — Basic from a proxy in front of the
// install — is not a credential of ours, and refusing it would take a whole
// browser session down with it.
func unreadCredentialPresented(r *http.Request) bool {
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" &&
		!strings.HasPrefix(k, deployKeySecretPrefix) {
		return true
	}
	token := bearerToken(r)
	return token != "" && !strings.HasPrefix(token, deployKeySecretPrefix)
}

// apiKeyAuth authenticates a personal API key on the platform API.
//
// Three properties matter and each is load-bearing:
//
//   - **It never falls back to the cookie.** A presented-but-unusable key is a
//     401, exactly as at the apps gateway. Falling back would mean a script
//     with a revoked token kept working from a developer's browser session and
//     nobody found out until the token was needed somewhere else.
//   - **It resolves the owner live.** Nothing is baked into the credential, so
//     a demotion, a lock or a revoked grant applies on this request rather than
//     whenever a token would have expired. users.token_version is not consulted
//     — that counter invalidates JWTs carrying stale claims, and this carries
//     none.
//   - **A refusal a caller can act on says why.** Whoever presented a hash that
//     matched already holds the key, so "disabled" and "expired" tell them
//     nothing they could not infer, and save an afternoon of guessing. A key
//     that resolves to nothing gets a flat "invalid" — that one can be an
//     attack.
func (s *Server) apiKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if x := exchangedKeyFromRequest(r); x != "" {
			s.exchangedKeyAuth(w, r, next, x)
			return
		}
		secret := userAPIKeyFromRequest(r)
		// A request rsconnect signed carries no secret: connectAPIRewrite
		// verified its signature and left the hash of the personal key the
		// token was approved as. From here the two are one credential, and
		// every check below is made once for both.
		var hash string
		if secret != "" {
			hash = hashAPIKey(secret)
		} else if h, ok := connectSignedKeyHash(r.Context()); ok {
			hash = h
		}
		if hash == "" {
			// A removed app key, said by name: a bare 401 would read as "your
			// key is wrong" when the class it belongs to no longer exists.
			if retiredAppKeyFromRequest(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": retiredAppKeyMessage,
					"code":  "app_key_removed",
					"hint":  "use a personal key (toftu_…) of an account granted on the app — `toft login` mints one",
				})
				return
			}
			// A credential presented in a form no authenticator reads is a
			// 401, never anonymous: `Bearer garbage` answered GET /apps with
			// the public list, so a broken TOFT_TOKEN in CI read as an empty
			// install and exited 0 (BB-033, fourth 2026-11-03 black-box list).
			// A deploy key is the next middleware's; no credential at all
			// stays anonymous.
			if unreadCredentialPresented(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{
					"error": "the credential presented is not a key this install issued — " +
						"a personal key starts toftu_, a deploy key toftd_ and an exchanged key toftx_",
					"code": "invalid_key"})
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		key, err := s.db.ResolveUserAPIKey(hash)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if key == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "invalid API key", "code": "invalid_key"})
			return
		}
		if !key.Enabled {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "personal API keys are switched off on this install",
				"code":  "api_keys_disabled", "hint": "ask an administrator to turn personal API keys back on, or sign in through the browser"})
			return
		}
		if key.DisabledAt != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "this API key is disabled", "code": "key_disabled"})
			return
		}
		if key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now().UTC()) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "this API key expired on " + key.ExpiresAt.Format("2006-01-02"),
				"code":  "key_expired"})
			return
		}
		if key.Locked {
			// A personal key *is* the person, so a locked account takes its
			// keys with it.
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "the account this API key belongs to is locked", "code": "owner_locked"})
			return
		}
		if key.Pending {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "the account this API key belongs to is awaiting activation", "code": "owner_pending",
				"hint": "ask an administrator to activate the account, then use the key again"})
			return
		}

		groups, err := s.db.GetUserGroupIDs(key.UserID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		s.touchUserAPIKey(key.KeyID, clientIP(r))

		ctx := auth.WithPrincipal(r.Context(), &auth.Principal{
			Type:       database.PrincipalTypeUser,
			ID:         key.UserID,
			SystemRole: key.SystemRole,
			Groups:     groups,
			APIKeyID:   key.KeyID,
			APIKeyName: key.KeyName,
			// Read in the same query as the role and the capability switch, so
			// a scope cannot be a request behind the row it came from.
			APIKeyScope: key.KeyScope,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// apiKeyUsage counts a key's platform API traffic, split by outcome.
//
// It wraps the handler rather than preceding it because the status is the
// point: "this token made 4,000 calls" and "this token made 4,000 calls and
// 3,900 of them 404ed" are different facts, and only the second one explains
// why a nightly job has been quietly doing nothing since March.
//
// The counter is in memory (internal/usage) and flushes on an interval, so
// this adds no database write to the request path.
func (s *Server) apiKeyUsage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.GetPrincipal(r.Context())
		// An exchanged key lives hours and is counted by nothing: its use is
		// in the audit log under its id, and a per-key traffic history for a
		// credential that is gone by tomorrow answers nobody's question.
		if !p.IsAPIKey() || p.IsExchangedKey() {
			next.ServeHTTP(w, r)
			return
		}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // handler wrote a body without WriteHeader
		}
		// appID is empty: this is the platform API. Gateway traffic is counted
		// in appProxyGuard, where the app is known.
		s.usage.RecordKey(p.APIKeyID, database.KeyKindUser, "", status)
	})
}

// touchUserAPIKey records last use, throttled per key. It shares the Server's
// throttle map with the app-key path: the two id spaces are UUIDs and cannot
// collide, and the budget being enforced — one write per key per minute — is
// the same budget.
func (s *Server) touchUserAPIKey(id, ip string) {
	now := time.Now()
	if v, ok := s.apiKeyTouched.Load(id); ok && now.Sub(v.(time.Time)) < time.Minute {
		return
	}
	s.apiKeyTouched.Store(id, now)
	go func() {
		if err := s.db.TouchUserAPIKey(id, now.UTC(), ip); err != nil {
			log.Printf("api-key: recording last use of %s: %v", id, err)
		}
	}()
}

// clientIP is the peer address without its port. realIPMiddleware has already
// replaced RemoteAddr with the forwarded client when — and only when — the
// request arrived through a trusted proxy.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
