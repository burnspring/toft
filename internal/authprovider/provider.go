// Package authprovider turns a configured sign-in provider into the two
// operations the platform needs from it: where to send the browser, and what
// came back.
//
// It knows nothing about the database or about HTTP handlers. A Provider is a
// resolved, in-memory value — plaintext client secret included — assembled by
// the caller from either an environment-configured provider or a row an
// administrator created. That is deliberate: the package that talks to identity
// providers should not also be the package that decides who may configure one.
//
// Two kinds, not one per vendor. Everything vendor-specific about an OIDC
// provider is already in its discovery document, so Okta, Entra ID, Google,
// Auth0, Keycloak, Dex and GitLab are one implementation. GitHub is the
// exception because it predates OIDC: no discovery document, no id_token, and a
// user API of its own.
//
// # The id_token is deliberately not used
//
// A confidential client redeeming an authorization code talks to the token
// endpoint directly over TLS, so the response is already authenticated by the
// channel — it did not cross an untrusted boundary the way an id_token handed
// to a browser does. Reading the profile from the userinfo endpoint with the
// access token is the same trust level and costs one request, where verifying
// an id_token means JWKS fetching, key rotation, caching, clock skew and
// algorithm confusion. Fewer things to get wrong, and none of them silent.
package authprovider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Kinds, mirroring database.ProviderKind*. Not imported from there: this
// package must not depend on storage.
const (
	KindOIDC   = "oidc"
	KindGitHub = "github"
	// KindPassword is local sign-in against a password this install stores.
	// There is no provider to talk to, so nothing in this package acts on it —
	// it appears here only because the registry, the login page and the admin
	// surface treat every way in as a provider, and one of the ways in is not an
	// identity provider at all.
	KindPassword = "password"
)

// ErrUnsupportedKind means the kind is not one this package implements.
var ErrUnsupportedKind = errors.New("unsupported provider kind")

// ErrProviderDisabled is what Exchange returns for a provider that is switched
// off: the code may be perfectly good, and the answer is still no.
var ErrProviderDisabled = errors.New("that sign-in provider is disabled")

// Provider is a fully resolved provider, ready to use.
type Provider struct {
	// ID is the slug that appears in the redirect URI. It is a permalink.
	ID          string
	Kind        string
	DisplayName string

	ClientID     string
	ClientSecret string

	// Issuer is the OIDC issuer URL. Endpoints are discovered from it unless
	// the three URLs below are set, which is the escape hatch for a provider
	// whose discovery document is absent or wrong.
	Issuer      string
	AuthURL     string
	TokenURL    string
	UserinfoURL string
	Scopes      []string

	// AllowedDomains restricts which email domains may sign in. Empty means
	// any domain the provider authenticates — correct for a corporate IdP that
	// only holds employees, dangerous for a multi-tenant one.
	AllowedDomains []string
	// AllowedEmails are individual addresses permitted to sign in, in **union**
	// with AllowedDomains and never in intersection: a person matching either
	// list is permitted. Both empty means any address this provider
	// authenticates. It exists because "the domain, plus two contractors" is
	// the request that arrives immediately after a domain gate goes on.
	AllowedEmails []string
	// DefaultRole is the system role a newly provisioned user receives.
	DefaultRole string
	// LinkByVerifiedEmail permits attaching this provider's new identities to
	// an existing user with the same verified address.
	LinkByVerifiedEmail bool
	// AutoProvision permits a successful sign-in with no matching identity to
	// create an account. Off, this provider authenticates only people who
	// already have one, and everybody else is refused with the platform's
	// "no account" message.
	AutoProvision bool

	// Managed is true for a provider an administrator created and can edit.
	// Environment-configured providers are not managed: the operator owns them,
	// and the admin UI shows them read-only rather than pretending a form can
	// change what a restart controls.
	Managed bool

	// Enabled providers are offered on the login page.
	Enabled bool

	// GroupsClaim names the claim carrying directory group membership. Empty
	// means `groups` — see GroupsClaimOr.
	//
	// Per provider rather than per install because two IdPs differ and one of
	// them calls it `roles`. A provider with no group mappings attached to any
	// group never reads it.
	GroupsClaim string

	// GroupsSeparator is the delimiter a claim delivered as a single string is
	// split on. Empty means no split, which is what every provider read before
	// this field existed: a bare string is a list of one.
	//
	// It is a fix as much as a setting. `"data-team,platform"` has always read
	// as one group whose name contains a comma, which matches nothing — harmless
	// while the only consumer of a claim was membership, which grants, and not
	// harmless at all now that a consumer refuses.
	GroupsSeparator string

	// PermittedLoginGroups are claim values of which a person must hold at
	// least one to sign in through this provider. Empty means every group is
	// permitted.
	//
	// Claim values rather than local group ids, because this is an
	// authentication gate and must not depend on an authorization table: a
	// value nobody has attached to a group is still usable, and the gate's
	// failure mode stays independent of the reconciler's.
	//
	// See GroupsPermitted for the decision an absent claim produces, which is
	// the sharpest thing in this package.
	PermittedLoginGroups []string
}

// GroupDecision is what the permitted-groups gate concluded. Three values
// rather than a bool because the two refusals have different remedies and the
// audit log has to be able to say which — a claim that arrived and matched
// nothing is fixed in this form, and a claim that never arrived is fixed at the
// IdP.
type GroupDecision int

const (
	// GroupsAdmitted is either a match or an empty policy.
	GroupsAdmitted GroupDecision = iota
	// GroupsNotPermitted is a claim that arrived and held none of the
	// permitted values. Present-and-empty lands here too: a provider saying
	// "this person is in no groups" has asserted something, and it is honoured.
	GroupsNotPermitted
	// GroupsClaimAbsent is a claim that never arrived at all.
	GroupsClaimAbsent
)

// GroupsPermitted decides whether a profile may sign in through this provider.
//
// # An absent claim refuses, and that is the decision worth arguing with
//
// Admitting on absence would make the setting decorative: any provider
// misconfiguration — a dropped scope, an Entra overage, an Okta app configured
// to omit the claim — would silently open the install to everybody, which is
// the exact opposite of what an administrator who filled the field in asked
// for. An authentication gate that fails open is not a gate.
//
// The cost is real and is said plainly rather than softened: **a provider that
// stops asserting groups locks out every one of its users at once, including
// administrators who sign in through it.** What makes that a trade rather than
// a recklessness is an invariant this product already holds — there is always
// exactly one way in that needs no provider, and the built-in local
// administrator is reachable when every provider is refusing. Failing closed is
// only defensible in a product with that door, and this one has it.
//
// Matching is case-insensitive on both sides. Entra emits object GUIDs whose
// case is not stable across surfaces, and no directory in practice
// distinguishes two groups by case alone.
func (p *Provider) GroupsPermitted(profile *Profile) GroupDecision {
	if len(p.PermittedLoginGroups) == 0 {
		return GroupsAdmitted
	}
	if profile == nil || !profile.GroupsAsserted {
		return GroupsClaimAbsent
	}
	permitted := make(map[string]bool, len(p.PermittedLoginGroups))
	for _, g := range p.PermittedLoginGroups {
		if v := strings.ToLower(strings.TrimSpace(g)); v != "" {
			permitted[v] = true
		}
	}
	if len(permitted) == 0 {
		// A list of nothing but blanks is not a policy. Treated as unset
		// rather than as a lockout nobody typed.
		return GroupsAdmitted
	}
	for _, held := range profile.Groups {
		if permitted[strings.ToLower(strings.TrimSpace(held))] {
			return GroupsAdmitted
		}
	}
	return GroupsNotPermitted
}

// Profile is what a provider asserted about an account, normalized.
type Profile struct {
	// Subject is the provider's stable identifier for the account — `sub` for
	// OIDC, the numeric id for GitHub. Never the email: most providers let an
	// address be reassigned, so an email is an attribute and not an identity.
	Subject string
	Login   string
	// Name is what the person is called, as the provider asserts it — the
	// OIDC `name` claim, GitHub's `name`. Empty when the provider does not
	// say; nothing invents one.
	Name  string
	Email string
	// EmailVerified is the provider's own assertion. It gates email-based
	// account linking, so a provider that omits it gets `false` — absent is
	// treated as unverified, never as verified.
	EmailVerified bool
	// EmailUnverified is the stronger statement: the provider **said** the
	// address is not verified (email_verified=false, a GitHub address with
	// verified=false). Absent is the weaker case — several IdPs never send the
	// claim — and is not this. The allowed-domain gate refuses on this one, so
	// a provider that vouches for nothing is not locked out and one that says
	// "unverified" is not believed.
	EmailUnverified bool
	AvatarURL       string
	// Groups is the group claim, resolved to a list of values. Empty and absent
	// are different things, and GroupsAsserted is which.
	Groups []string
	// GroupsAsserted is whether the provider said anything about groups at all.
	//
	// It cannot be inferred from len(Groups): Entra omits the claim entirely
	// past its overage limit and Okta can be configured to omit it, so a
	// reconciler reading absence as "in no groups" would strip every directory
	// membership on the install. Absent reconciles nothing; present-and-empty
	// removes.
	GroupsAsserted bool
}

// DefaultScopes are the scopes a kind needs when none are configured.
func DefaultScopes(kind string) []string {
	switch kind {
	case KindGitHub:
		return []string{"read:user", "user:email"}
	default:
		// openid is what makes it an OIDC request at all; profile and email are
		// what make the userinfo response contain a person.
		return []string{"openid", "profile", "email"}
	}
}

// Validate reports whether the provider is configured well enough to attempt a
// sign-in. Called on admin writes so a broken provider is refused at the form
// rather than discovered by the first person who tries to log in.
func (p *Provider) Validate() error {
	if p.ID == "" {
		return errors.New("an id is required")
	}
	if !validSlug(p.ID) {
		return errors.New("the id may contain only lowercase letters, digits and hyphens")
	}
	if p.DisplayName == "" {
		return errors.New("a display name is required")
	}
	// Password sign-in has no client registration to get wrong: no third party,
	// so no client id, no secret and no endpoints. Checked before the client id
	// requirement below, which every OAuth kind does have.
	if p.Kind == KindPassword {
		return nil
	}
	if p.ClientID == "" {
		return errors.New("a client id is required")
	}
	switch p.Kind {
	case KindGitHub:
		// A permitted-groups list here can never be satisfied, and the result
		// would be a total lockout rather than a narrowing. GitHub predates
		// OIDC: there is no id_token and no group claim, so githubProfile sets
		// neither Groups nor GroupsAsserted, every sign-in reaches the gate as
		// GroupsClaimAbsent, and the gate fails closed by design.
		//
		// Refused here rather than warned because this one is *certain* — it is
		// a property of the kind and not of somebody's IdP configuration, so
		// there is no correct setup it could be mistaking for a broken one.
		if len(p.PermittedLoginGroups) > 0 {
			return errors.New("GitHub asserts no group claim, so a permitted login group would refuse every sign-in — use allowed email domains or addresses instead")
		}
		return nil
	case KindOIDC:
		if p.Issuer == "" && (p.AuthURL == "" || p.TokenURL == "" || p.UserinfoURL == "") {
			return errors.New("an issuer URL is required (or all three endpoint URLs, for a provider with no discovery document)")
		}
		for _, u := range []string{p.Issuer, p.AuthURL, p.TokenURL, p.UserinfoURL} {
			if u == "" {
				continue
			}
			if err := checkHTTPSURL(u); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedKind, p.Kind)
	}
}

// UsesPKCE reports whether an authorization request should carry a code
// challenge. OIDC providers get one; GitHub's OAuth2 ignores it.
func (p *Provider) UsesPKCE() bool { return p.Kind == KindOIDC }

// Ready reports whether this provider could sign someone in as things stand:
// enabled, and configured far enough to try.
//
// One definition, used by both the login page and the admin surface, so a
// provider can never be offered on one and reported unusable on the other.
// Password sign-in needs no client credentials; for every other kind a missing
// client id or secret is a half-finished form, and offering it would send
// someone to a provider that will reject them.
func (p *Provider) Ready() bool {
	if !p.Enabled {
		return false
	}
	if p.Kind == KindPassword {
		return true
	}
	return p.ClientID != "" && p.ClientSecret != ""
}

// scopes resolves configured scopes against the kind's defaults.
func (p *Provider) scopes() []string {
	if len(p.Scopes) > 0 {
		return p.Scopes
	}
	return DefaultScopes(p.Kind)
}

// Endpoints is where a provider's three operations live.
type Endpoints struct {
	AuthURL     string
	TokenURL    string
	UserinfoURL string
}

// Resolve returns the provider's endpoints, discovering them if needed.
//
// Discovery results are cached per issuer (see discovery.go), so a login does
// not become two round trips to the IdP; an explicit override skips discovery
// entirely.
func (p *Provider) Resolve(ctx context.Context) (Endpoints, error) {
	switch p.Kind {
	case KindGitHub:
		return githubEndpoints, nil
	case KindOIDC:
		ep := Endpoints{AuthURL: p.AuthURL, TokenURL: p.TokenURL, UserinfoURL: p.UserinfoURL}
		if ep.AuthURL != "" && ep.TokenURL != "" && ep.UserinfoURL != "" {
			return ep, nil
		}
		doc, err := Discover(ctx, p.Issuer)
		if err != nil {
			return Endpoints{}, err
		}
		// A partial override wins over the discovered value for that one
		// endpoint, which is how a provider with one wrong entry in an
		// otherwise correct document is worked around.
		if ep.AuthURL == "" {
			ep.AuthURL = doc.AuthorizationEndpoint
		}
		if ep.TokenURL == "" {
			ep.TokenURL = doc.TokenEndpoint
		}
		if ep.UserinfoURL == "" {
			ep.UserinfoURL = doc.UserinfoEndpoint
		}
		if ep.AuthURL == "" || ep.TokenURL == "" || ep.UserinfoURL == "" {
			return Endpoints{}, fmt.Errorf("provider %s: discovery document from %s is missing an authorization, token or userinfo endpoint",
				p.ID, p.Issuer)
		}
		return ep, nil
	default:
		return Endpoints{}, fmt.Errorf("%w: %q", ErrUnsupportedKind, p.Kind)
	}
}

func (p *Provider) oauthConfig(ep Endpoints, redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		RedirectURL:  redirectURI,
		Scopes:       p.scopes(),
		Endpoint: oauth2.Endpoint{
			AuthURL:  ep.AuthURL,
			TokenURL: ep.TokenURL,
		},
	}
}

// AuthURLFor returns where to send the browser. The verifier is the PKCE code
// verifier the caller stashed for the callback; it is ignored for kinds that
// do not use PKCE.
func (p *Provider) AuthURLFor(ctx context.Context, state, redirectURI, verifier string) (string, error) {
	ep, err := p.Resolve(ctx)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{}
	if p.UsesPKCE() && verifier != "" {
		opts = append(opts, oauth2.S256ChallengeOption(verifier))
	}
	return p.oauthConfig(ep, redirectURI).AuthCodeURL(state, opts...), nil
}

// Exchange redeems the authorization code and returns the account's profile.
//
// The redirect URI must be byte-identical to the one sent to the authorization
// endpoint — providers compare it, and a mismatch is the most common cause of
// an `invalid_grant` that looks like a wrong secret.
//
// A disabled provider never completes an exchange. The handlers refuse one
// before they get here, but the exchange is the one call that mints an account
// from a code, so the rule lives on the object rather than on every caller
// remembering it.
func (p *Provider) Exchange(ctx context.Context, code, redirectURI, verifier string) (*Profile, error) {
	if !p.Enabled {
		return nil, ErrProviderDisabled
	}
	ep, err := p.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	cfg := p.oauthConfig(ep, redirectURI)

	// The token POST goes through the bounded client too. Without it oauth2
	// falls back to http.DefaultClient, which has no timeout, and the callback
	// request carries no deadline: a token endpoint that accepts and never
	// answers would hold the sign-in open for as long as the browser waits.
	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

	opts := []oauth2.AuthCodeOption{}
	if p.UsesPKCE() && verifier != "" {
		opts = append(opts, oauth2.VerifierOption(verifier))
	}
	token, err := cfg.Exchange(ctx, code, opts...)
	if err != nil {
		return nil, fmt.Errorf("exchanging authorization code: %w", err)
	}

	client := cfg.Client(ctx, token)
	client.Timeout = httpTimeout
	switch p.Kind {
	case KindGitHub:
		// GitHub asserts no group claim. Reading its teams API would be a
		// directory query, which this design refuses: everything here is a
		// claim the person brought.
		return githubProfile(ctx, client)
	default:
		profile, err := oidcProfile(ctx, client, ep.UserinfoURL, p.GroupsClaim, p.GroupsSeparator)
		if err != nil {
			return nil, err
		}
		// The id_token is the second source and usually the only one that has
		// it: Entra does not return groups from userinfo. Userinfo wins where
		// both answer, on the ground that it was fetched with the access token
		// rather than parsed out of a bearer assertion.
		if !profile.GroupsAsserted {
			if raw, ok := token.Extra("id_token").(string); ok && raw != "" {
				if claims := claimsFromIDToken(raw); claims != nil {
					if groups, ok := claimStrings(claims, GroupsClaimOr(p.GroupsClaim), p.GroupsSeparator); ok {
						profile.Groups, profile.GroupsAsserted = groups, true
					}
				}
			}
		}
		return profile, nil
	}
}

// RestrictsEmail reports whether this provider admits only some addresses.
func (p *Provider) RestrictsEmail() bool {
	return len(p.AllowedDomains) > 0 || len(p.AllowedEmails) > 0
}

// EmailAllowed applies the domain policy. Reported separately from Exchange so
// the caller can answer with the reason rather than a generic failure — "your
// account is not in an allowed domain" is actionable and "authentication
// failed" is not.
func (p *Provider) EmailAllowed(email string) bool {
	if len(p.AllowedDomains) == 0 && len(p.AllowedEmails) == 0 {
		return true
	}
	addr := strings.ToLower(strings.TrimSpace(email))
	// The address list first, because it is the more specific answer and it is
	// how "the domain, plus two contractors" is expressed. A **union**: a
	// person matching either list is permitted, which is the only rule that
	// leaves both fields usable at once.
	for _, allowed := range p.AllowedEmails {
		if addr == strings.ToLower(strings.TrimSpace(allowed)) {
			return true
		}
	}
	if len(p.AllowedDomains) == 0 {
		// Addresses were listed and this one is not among them. Nothing else
		// to consult.
		return false
	}
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		// A domain policy with no address to check fails closed: the policy
		// exists precisely to keep unknown accounts out.
		return false
	}
	domain := addr[at+1:]
	for _, allowed := range p.AllowedDomains {
		if domain == strings.ToLower(strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

const httpTimeout = 15 * time.Second

// httpClient is used for discovery, the code exchange and userinfo. A bounded
// timeout because all three are on the sign-in path: an IdP that hangs must fail the login rather than
// hold a platform request open indefinitely.
var httpClient = &http.Client{Timeout: httpTimeout}

func validSlug(s string) bool {
	if len(s) > 40 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(s)-1:
		default:
			return false
		}
	}
	return s != ""
}

// checkHTTPSURL refuses anything that is not TLS, with a carve-out for loopback
// so a local Dex or Keycloak can be developed against. Client secrets and
// authorization codes cross these URLs; over plain http on a real network they
// are readable.
func checkHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || net.ParseIP(host).IsLoopback() {
			return nil
		}
		return fmt.Errorf("%s must use https (plain http is permitted only for loopback, for local development)", raw)
	default:
		return fmt.Errorf("%q must be an http(s) URL", raw)
	}
}
