package auth

import (
	"context"
	"net/http"
	"time"
)

type contextKey string

const (
	userClaimsKey contextKey = "user_claims"
	principalKey  contextKey = "principal"
)

// Principal represents an authenticated actor (user or service account).
type Principal struct {
	Type         string
	ID           string
	SystemRole   string
	Groups       []string
	OwningUserID string // service accounts only

	// APIKeyID and APIKeyName are set when this principal was authenticated by
	// a personal API key rather than a session cookie. The principal is still
	// the *user* — a personal key carries no identity of its own — but which
	// credential spoke is what the audit log records and what the allow-list
	// gates on.
	APIKeyID   string
	APIKeyName string
	// APIKeyScope is `read` or `full` — which routes the key may call. Like
	// DeployKeyScope it is not a role and it does not narrow *which objects*
	// the principal reaches: a read-scoped key resolves to exactly its owner's
	// role, groups and grants, and reaches every object they do. Authorize
	// therefore does not read it; the route guard in internal/api does.
	//
	// It is the one claim a personal key carries, and it is safe for exactly
	// one reason — it can only ever subtract. A field that granted anything
	// would have to be resolved live like every other claim on this struct.
	APIKeyScope string
	// APIKeyIssuer is set when the key is an exchanged one (`toftx_`, ADR
	// §157): the trusted issuer whose identity token bought it. The principal
	// is still the person; this names who vouched, and the route guard bounds
	// such a key to the deploy surface.
	APIKeyIssuer string

	// DeployKeyID and DeployKeyName are set when this principal *is* a deploy
	// key — `toftd_`, the credential that belongs to nobody (ADR §51). Unlike
	// APIKeyID, which names the credential a *person* spoke through, these
	// describe the whole principal: there is no user behind it, ID is empty,
	// and the audit actor is `deploy_key:{id}`.
	DeployKeyID   string
	DeployKeyName string
	// DeployKeyScope is `read` or `deploy` — which route list bounds this key
	// (ADR §51). It is not a role and it does not narrow *which objects* the
	// key reaches: a deploy key acts as owner on every app whatever its scope,
	// and the scope decides only which routes it may call. Authorize therefore
	// does not read it; the route guard in internal/api does.
	DeployKeyScope string
}

// PrincipalTypeDeployKey is the principal a deploy key authenticates as. It is
// deliberately not "user": nothing may resolve it to a person, and a group
// membership, a lock and a system role are all meaningless for it.
const PrincipalTypeDeployKey = "deploy_key"

func (p *Principal) IsAnonymous() bool { return p == nil || (p.ID == "" && p.DeployKeyID == "") }
func (p *Principal) IsUser() bool      { return p != nil && p.Type == "user" }
func (p *Principal) IsServiceAccount() bool {
	return p != nil && p.Type == "service_account"
}

// IsDeployKey reports whether this request was authenticated by a deploy key.
//
// It is a different *kind* of principal from IsAPIKey, and the difference is
// the whole reason the class exists: a personal key is a person reached through
// a credential a script can hold, so authorization is that person's. A deploy
// key is nobody. Its authorization is possession, bounded entirely by
// deployKeyAllowedRoutes — which is why that list, and not a role, is the
// security boundary here.
func (p *Principal) IsDeployKey() bool { return p != nil && p.DeployKeyID != "" }

// IsAPIKey reports whether this request was authenticated by a personal API
// key. It is not a different kind of principal — it is the same person,
// reached by a credential a script can hold.
func (p *Principal) IsAPIKey() bool { return p != nil && p.APIKeyID != "" }

// IsExchangedKey reports whether the key is a short-lived one a trusted
// issuer's token was exchanged for. Every exchanged key is also IsAPIKey.
func (p *Principal) IsExchangedKey() bool { return p != nil && p.APIKeyIssuer != "" }
func (p *Principal) IsAdmin() bool {
	return p != nil && p.SystemRole == "system_admin"
}

// SessionStates resolves a user's current token_version and lock status in
// one read — implemented by database.DB. Middleware compares the version with
// the one baked into the JWT so that role/group changes and logout revoke
// outstanding sessions; a locked user fails the same check.
type SessionStates interface {
	GetSessionState(userID string) (tokenVersion int, locked bool, err error)
}

// CheckSession reports whether claims are still live: the session must be
// inside the install's absolute ceiling, the token's version must equal the
// user's current one, and the account must not be locked. Any lookup failure
// (user deleted, database down) fails closed. Only user principals carry a
// version; nothing mints service-account JWTs today.
//
// The ceiling is checked here, and here is why here: this is the one function
// every reader of a session cookie already calls — the platform middleware, the
// apps gateway, and `/me`. A cap enforced anywhere less central would be a rule
// with an exception nobody wrote down.
//
// It is checked *independently of `exp`* on purpose. An administrator who
// shortens the lifetime from eight hours to one means now, not "for sessions
// minted after this" — which is the rule the machine-credentials invariant
// already states as "off means absent, and off means now", and the difference
// between a control an assessor can test and one they cannot.
func CheckSession(states SessionStates, claims *Claims, policy SessionPolicy) bool {
	if states == nil || claims == nil {
		return false
	}
	if policy.Expired(claims.SessionStart(), time.Now().UTC()) {
		return false
	}
	current, locked, err := states.GetSessionState(claims.UserID)
	return err == nil && !locked && current == claims.TokenVersion
}

// Options are what Middleware needs beyond authentication itself: the install's
// session policy, and the way back to the browser.
//
// A struct rather than two more positional parameters, and split this way
// because the two halves belong to different packages. This one owns the
// *decision* — it holds the claims and the policy. `internal/api` owns the
// *cookie*: the paths, the Secure flag and the two-origin split are facts about
// how this install is served, and none of them belongs in a package that
// validates tokens.
type Options struct {
	// Policy reads the install's session policy, live. Nil falls back to
	// DefaultSessionPolicy — which is for tests; a server always supplies one.
	//
	// Read per request rather than baked into the token at mint, so a tightened
	// lifetime applies to sessions already outstanding. The caller is expected
	// to cache: this runs on every authenticated request.
	Policy func() SessionPolicy

	// Renew writes a re-minted session back to the browser. Nil means this
	// mount does not renew — which is also what an install with the idle
	// timeout off gets, since ShouldRenew never says yes.
	Renew func(w http.ResponseWriter, s Session)
	// HostCookie reports whether a browser's session is read from
	// HostSessionCookie (SessionTokenFor): secure cookies on an install where
	// another host could toss a plain one — two hosts, or an app on a hostname
	// of its own. Asked per request, because the second can change at runtime.
	HostCookie func() bool
}

func (o Options) policy() SessionPolicy {
	if o.Policy == nil {
		return DefaultSessionPolicy()
	}
	return o.Policy()
}

// Middleware parses the session cookie; if present & valid it populates the
// Principal in context. Missing/invalid tokens yield anonymous requests rather
// than 401 — individual handlers decide via authorize(). A token whose
// version no longer matches the user's current token_version — or whose user
// is locked, or whose session is past the install's absolute ceiling — is
// treated as invalid (one indexed read per authenticated request).
//
// It is also where a session is renewed. The idle clock lives in the browser's
// own cookie and nothing is written down, so being used *is* the renewal: a
// request that arrives more than a quarter of the way into the idle window
// re-mints and re-sets the cookie, and one that does not arrive lets it lapse.
func Middleware(jwtSvc *JWTService, states SessionStates, opts Options) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			// An explicit credential beats an ambient one. If an earlier
			// middleware authenticated an API key, the cookie riding along on
			// the same request says nothing: a script that presents a token is
			// asking to act as that token, and silently preferring a browser
			// session that happens to be attached would make the audit trail
			// name the wrong credential.
			if GetPrincipal(ctx) != nil {
				next.ServeHTTP(w, r)
				return
			}
			if token := SessionTokenFor(r, opts.HostCookie != nil && opts.HostCookie()); token != "" {
				policy := opts.policy()
				claims, err := jwtSvc.Validate(token)
				if err == nil && CheckSession(states, claims, policy) {
					p := &Principal{
						Type:         claims.PrincipalType,
						ID:           claims.UserID,
						SystemRole:   claims.SystemRole,
						Groups:       claims.Groups,
						OwningUserID: claims.OwningUserID,
					}
					if p.Type == "" {
						p.Type = "user"
					}
					ctx = context.WithValue(ctx, userClaimsKey, claims)
					ctx = context.WithValue(ctx, principalKey, p)
					RenewIfDue(jwtSvc, policy, opts.Renew, w, claims)
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RenewIfDue re-mints and re-sets the session cookie when this request is far
// enough into the idle window to be worth it.
//
// One function rather than two, because the platform middleware and the apps
// gateway both do this and they must not drift: time spent inside a deployed
// app is time the person was present, and a rule that only the platform origin
// applied would sign somebody out of Toft while they were using it.
//
// A failure to re-mint is deliberately silent and non-fatal. The request has
// already been authorized; the only consequence is that the cookie keeps the
// expiry it arrived with, and the next request tries again.
//
// The claims already in the request context are *not* replaced. The only fields
// renewal changes are `iat` and `exp`, and nothing downstream reads either —
// the act-for blob is minted against `auth_time`, which renewal never moves.
func RenewIfDue(jwtSvc *JWTService, policy SessionPolicy, set func(http.ResponseWriter, Session), w http.ResponseWriter, claims *Claims) {
	if set == nil || jwtSvc == nil || !policy.ShouldRenew(claims, time.Now().UTC()) {
		return
	}
	renewed, err := jwtSvc.Renew(claims, policy)
	if err != nil {
		return
	}
	set(w, renewed)
}

func GetClaims(ctx context.Context) *Claims {
	claims, _ := ctx.Value(userClaimsKey).(*Claims)
	return claims
}

func GetPrincipal(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey).(*Principal)
	return p
}

// WithPrincipal attaches an already-authenticated principal. It exists for the
// API-key path, which authenticates against a credential table rather than a
// JWT and so cannot go through Middleware — everything downstream still reads
// the principal through GetPrincipal and cannot tell the two apart, which is
// the point.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}
