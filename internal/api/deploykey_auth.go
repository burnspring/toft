package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
)

// deployKeySecretPrefix identifies a deploy key.
//
// Distinct from "toftu_" (personal key), "tofta_" (an app's outbound token) and
// the retired "toft_" (app key), and — as with those — not a prefix of any of
// them, so a prefix check can never mistake one class for another. The
// credentials point in different directions and the type of a token has to be legible
// from the token itself.
const deployKeySecretPrefix = "toftd_"

// deployKeyFromRequest extracts a presented deploy key, or "". Both header
// forms, matching every other credential this platform takes.
func deployKeyFromRequest(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); strings.HasPrefix(k, deployKeySecretPrefix) {
		return k
	}
	if tok := bearerToken(r); strings.HasPrefix(tok, deployKeySecretPrefix) {
		return tok
	}
	return ""
}

// deployKeyAuth authenticates a deploy key on the platform API.
//
// It has the same three properties as apiKeyAuth and for the same reasons — it
// never falls back to the cookie, it resolves state live, and a refusal
// somebody could act on says which refusal it is — with one difference that
// matters: there is nobody to resolve. The principal it installs has no user
// id, no groups and no system role, because a deploy key belongs to nobody.
// Everything it may do comes from deployKeyAllowedRoutes.
func (s *Server) deployKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := deployKeyFromRequest(r)
		if secret == "" {
			next.ServeHTTP(w, r)
			return
		}

		key, err := s.db.ResolveDeployKey(hashAPIKey(secret))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if key == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "invalid deploy key", "code": "invalid_key"})
			return
		}
		switch key.State(time.Now().UTC()) {
		case database.KeyStateRevoked:
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "this deploy key has been revoked", "code": "key_revoked"})
			return
		case database.KeyStateDisabled:
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "this deploy key is disabled", "code": "key_disabled"})
			return
		case database.KeyStateExpired:
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "this deploy key expired on " + key.ExpiresAt.Format("2006-01-02"),
				"code":  "key_expired"})
			return
		}

		s.touchDeployKey(key.ID)

		ctx := auth.WithPrincipal(r.Context(), &auth.Principal{
			Type:           auth.PrincipalTypeDeployKey,
			DeployKeyID:    key.ID,
			DeployKeyName:  key.Name,
			DeployKeyScope: key.Scope,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// deployKeyRoutesForScope picks the list that bounds a key.
//
// One function rather than a conditional at the guard, because the same
// question is asked by the tests that assert the chain and by the refusal that
// has to name the scope. An unrecognised scope resolves to the narrow list:
// this is an allow-list that fails closed, and a row carrying a word nobody has
// implemented yet must not be the widest thing on the install.
func deployKeyRoutesForScope(scope string) map[string]bool {
	if scope == database.DeployKeyScopeDeploy {
		return deployKeyAllowedRoutes
	}
	return deployKeyReadRoutes
}

// deployKeyRouteGuard is the security boundary for this credential class.
//
// A deploy key acts as owner on every app (auth.Authorize step 1a), so this
// list is not a convenience — it is the whole fence. It is a strict subset of
// apiKeyAllowedRoutes, and the sentence that governs it is the same one that
// governs a personal key, for the same reason: it may never manage a credential
// (its own included) or change who may do what. A stolen CI token that could
// mint its replacement or grant itself a role turns a bounded incident into an
// unbounded one.
//
// The scope picks which of the two lists applies, and the refusal says which
// one refused. "This key is read-scoped" and "no deploy key may do that" are
// different problems with different remedies — mint the other scope, versus
// stop asking — and a caller who cannot tell them apart spends an afternoon on
// the wrong one. That is the same reasoning as the disabled/expired/revoked
// split above: whoever presented a matching hash already holds the credential,
// so naming the reason leaks nothing.
func (s *Server) deployKeyRouteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.GetPrincipal(r.Context())
		if !p.IsDeployKey() {
			next.ServeHTTP(w, r)
			return
		}
		// A path that matches no route is not this credential's problem —
		// nothing here decides it, because it reaches no handler whoever asks.
		// It goes to the router's 404 rather than to a sentence about what a
		// deploy key may do. See routeMatch: a nil mux still fails closed.
		pattern, resolved := s.routeMatch(r)
		if s.mux != nil && !resolved {
			next.ServeHTTP(w, r)
			return
		}
		route := r.Method + " " + pattern
		if deployKeyRoutesForScope(p.DeployKeyScope)[route] {
			next.ServeHTTP(w, r)
			return
		}
		if p.DeployKeyScope == database.DeployKeyScopeRead && deployKeyAllowedRoutes[route] {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this deploy key is read-scoped — it may list apps and groups, read " +
					"deployment history and download a release, and it may not create, deploy, " +
					"roll back, restart or stop anything. A deploy-scoped key can do this",
				"code": "deploy_key_read_scope", "hint": "use a deploy-scoped key, or a personal key from toft login",
			})
			return
		}
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "a deploy key may create, deploy, roll back, restart and stop apps, set their " +
				"environment, run their jobs and read their history, and nothing else — it cannot " +
				"delete apps, manage credentials or change who may do what",
			"code": "deploy_key_not_permitted", "hint": "use a personal key from toft login for this",
		})
	})
}

// touchDeployKey records last use, throttled per key, off the request path.
// Same budget and same reason as touchUserAPIKey: one write per key per minute,
// and never a database write inside the request the *Data path* invariant
// protects.
func (s *Server) touchDeployKey(id string) {
	now := time.Now()
	if v, ok := s.apiKeyTouched.Load(id); ok && now.Sub(v.(time.Time)) < time.Minute {
		return
	}
	s.apiKeyTouched.Store(id, now)
	go func() {
		if err := s.db.TouchDeployKey(id, now.UTC()); err != nil {
			log.Printf("deploy-key: recording last use of %s: %v", id, err)
		}
	}()
}

// auditActor is who to record for an audited write.
//
// A person normally, and for a deploy key the key itself: `deploy_key` /
// {id}. That is not a hole in "every audit row names the individual who acted"
// — it is the reason the class exists. A shared human account would have put a
// person's name on every pipeline action forever, which is the attribution
// collapse ADR §51 rejected the service-account design over. A credential that
// belongs to nobody says so in the log, and `deploy_keys.name` is what turns
// the id back into "the nightly promotion pipeline".
//
// One function, because an audit row that quietly attributes a robot's action
// to whoever last touched the code is worse than one that says "unknown".
func auditActor(p *auth.Principal) (actorType, actorID string) {
	if p == nil {
		return database.PrincipalTypeUser, ""
	}
	if p.IsDeployKey() {
		return auth.PrincipalTypeDeployKey, p.DeployKeyID
	}
	return database.PrincipalTypeUser, p.ID
}
