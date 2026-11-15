package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"toft/internal/authprovider"
	"toft/internal/database"
)

// The provider registry: what this install will let someone sign in with.
//
// Two sources, merged here and nowhere else:
//
//	environment  GITHUB_CLIENT_ID / OIDC_* — the operator's, set at deploy
//	             time. Not editable at runtime, because a form cannot change
//	             what a restart controls.
//	database     auth_providers — an administrator's, editable, audited.
//
// The environment wins on a collision, and creating a row with a colliding id
// is refused at the API rather than silently shadowed: an admin who cannot see
// why their edits do nothing is the worst of the three outcomes.
//
// Read straight from the database on every use rather than cached. A sign-in is
// not a hot path, one indexed read is cheaper than the staleness bugs a cache
// invalidation would eventually produce, and an administrator disabling a
// provider expects the next login attempt to honor it.

// errNoSuchProvider means the id names nothing this install offers.
var errNoSuchProvider = errors.New("no such sign-in provider")

// envAuthProviders builds the providers the operator configured. Their secrets
// come from the environment already in plaintext, so nothing here decrypts.
func (s *Server) envAuthProviders() []authprovider.Provider {
	out := []authprovider.Provider{}

	if s.cfg.GitHubClientID != "" && s.cfg.GitHubClientSecret != "" {
		out = append(out, authprovider.Provider{
			ID:           "github",
			Kind:         authprovider.KindGitHub,
			DisplayName:  "GitHub",
			ClientID:     s.cfg.GitHubClientID,
			ClientSecret: s.cfg.GitHubClientSecret,
			DefaultRole:  database.SystemRoleViewer,
			// Environment-configured providers auto-provision. They exist to get
			// the first administrator in, and one that refused to create the
			// account it is there to create would be a bootstrap that cannot
			// bootstrap. Turning it off is a decision for a managed provider,
			// where an admin already exists to make it.
			AutoProvision: true,
			Enabled:       true,
			Managed:       false,
		})
	}

	if s.cfg.OIDCIssuer != "" && s.cfg.OIDCClientID != "" {
		id := s.cfg.OIDCProviderID
		if id == "" {
			id = "sso"
		}
		role := s.cfg.OIDCDefaultRole
		if role == "" {
			role = database.SystemRoleViewer
		}
		out = append(out, authprovider.Provider{
			ID:                  id,
			Kind:                authprovider.KindOIDC,
			DisplayName:         firstNonEmptyStr(s.cfg.OIDCDisplayName, "Single sign-on"),
			ClientID:            s.cfg.OIDCClientID,
			ClientSecret:        s.cfg.OIDCClientSecret,
			Issuer:              s.cfg.OIDCIssuer,
			Scopes:              strings.Fields(s.cfg.OIDCScopes),
			AllowedDomains:      splitDomains(s.cfg.OIDCAllowedDomains),
			AllowedEmails:       splitDomains(s.cfg.OIDCAllowedEmails),
			DefaultRole:         role,
			LinkByVerifiedEmail: false,
			AutoProvision:       true,
			Enabled:             true,
			Managed:             false,
			GroupsClaim:         s.cfg.OIDCGroupsClaim,
			GroupsSeparator:     s.cfg.OIDCGroupsSeparator,
			// The operator's half of the gate. An environment-configured
			// provider is read-only in the admin UI by design, so without this
			// it could not be narrowed by group at all.
			PermittedLoginGroups: splitDomains(s.cfg.OIDCPermittedLoginGroups),
		})
	}
	return out
}

// authProviders returns every provider this install knows about, enabled or
// not, environment-configured ones first.
func (s *Server) authProviders() ([]authprovider.Provider, error) {
	out := s.envAuthProviders()
	fromEnv := map[string]bool{}
	for _, p := range out {
		fromEnv[p.ID] = true
	}

	rows, err := s.db.ListAuthProviders()
	if err != nil {
		return nil, err
	}
	managed := make([]authprovider.Provider, 0, len(rows))
	for i := range rows {
		if fromEnv[rows[i].ID] {
			// Shadowed by the environment. Skipped rather than merged: two
			// definitions of one provider is not a state worth expressing.
			continue
		}
		p, err := s.resolveProvider(&rows[i])
		if err != nil {
			// A provider whose secret will not decrypt is broken, not absent —
			// the key changed, or the row was restored from another install.
			// It stays in the list with no secret, which is what the comment
			// here always claimed and the code did not do: returning the error
			// failed the *whole* list, so one unreadable secret took down the
			// admin page that exists to re-enter it and the login page with it.
			//
			// Fails closed anyway, and by the existing rule rather than a new
			// one: Ready() requires a client secret, so an unreadable provider
			// is not offered on the login page, and every sign-in path resolves
			// the provider again by id and gets this same error. What survives
			// is everything that never needed this key — local password sign-in
			// included, which is the way back in a lost key must not block.
			log.Printf("auth provider %s: %v — listed as broken; its stored secret must be re-entered", rows[i].ID, err)
			managed = append(managed, *providerFromRow(&rows[i], ""))
			continue
		}
		managed = append(managed, *p)
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].DisplayName < managed[j].DisplayName })
	return append(out, managed...), nil
}

// loginProviders returns the providers a login page should offer: the ones that
// are ready (see authprovider.Provider.Ready). A provider with no client
// credentials is a half-finished form, and offering it would send someone to a
// provider that will reject them; a disabled one is absent, including local
// password sign-in, which the login page must not so much as hint at once an
// administrator has switched it off.
func (s *Server) loginProviders() ([]authprovider.Provider, error) {
	all, err := s.authProviders()
	if err != nil {
		return nil, err
	}
	out := make([]authprovider.Provider, 0, len(all))
	for _, p := range all {
		if p.Ready() {
			out = append(out, p)
		}
	}
	return out, nil
}

// ssoProviderIDs lists the providers that are ready and are *not* local password
// sign-in — the ways in that survive switching local sign-in off.
//
// This is the input to the guard on disabling it: the question is never "does
// another provider exist" but "is an administrator actually reachable through
// one", and answering it needs both halves — these ids, and whether an unlocked
// admin holds an identity on one (database.HasAdminIdentityOn).
func (s *Server) ssoProviderIDs() ([]string, error) {
	providers, err := s.loginProviders()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		if p.Kind != authprovider.KindPassword {
			out = append(out, p.ID)
		}
	}
	return out, nil
}

// authProvider resolves one provider by id, for the login and callback paths.
// Disabled providers resolve — the handlers refuse them with the reason, which
// is more useful than a 404 on a provider that exists.
func (s *Server) authProvider(id string) (*authprovider.Provider, error) {
	for _, p := range s.envAuthProviders() {
		if p.ID == id {
			return &p, nil
		}
	}
	row, err := s.db.GetAuthProvider(id)
	if errors.Is(err, database.ErrProviderNotFound) {
		return nil, errNoSuchProvider
	}
	if err != nil {
		return nil, err
	}
	return s.resolveProvider(row)
}

// resolveProvider turns a stored row into a usable provider, decrypting the
// client secret. The plaintext exists only in the returned value, for the
// duration of one request.
func (s *Server) resolveProvider(row *database.AuthProvider) (*authprovider.Provider, error) {
	secret := ""
	if enc := row.EncryptedSecret(); enc != "" {
		var err error
		secret, err = s.encryptor.Decrypt(enc)
		if err != nil {
			return nil, fmt.Errorf("decrypting client secret: %w", err)
		}
	}
	return providerFromRow(row, secret), nil
}

// providerFromRow is the one place a stored row becomes a Provider, so the
// broken case below carries every field the working one does — the only
// difference being the secret it could not read.
func providerFromRow(row *database.AuthProvider, secret string) *authprovider.Provider {
	return &authprovider.Provider{
		ID:                   row.ID,
		Kind:                 row.Kind,
		DisplayName:          row.DisplayName,
		ClientID:             row.ClientID,
		ClientSecret:         secret,
		Issuer:               row.Issuer,
		AuthURL:              row.AuthURL,
		TokenURL:             row.TokenURL,
		UserinfoURL:          row.UserinfoURL,
		Scopes:               row.ScopeList(),
		AllowedDomains:       row.AllowedDomainList(),
		AllowedEmails:        row.AllowedEmailList(),
		DefaultRole:          row.DefaultRole,
		LinkByVerifiedEmail:  row.LinkByVerifiedEmail,
		AutoProvision:        row.AutoProvision,
		GroupsClaim:          row.GroupsClaim,
		GroupsSeparator:      row.GroupsSeparator,
		PermittedLoginGroups: row.PermittedLoginGroupList(),
		Enabled:              row.Enabled,
		Managed:              true,
	}
}

// redirectURI is where a provider sends the browser back. Derived from the
// provider id rather than stored, so it cannot drift from the route that
// actually handles the callback — and it must match byte-for-byte what was
// registered at the provider, which is why the id is a permalink.
func (s *Server) redirectURI(providerID string) string {
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/api/v1/auth/" + providerID + "/callback"
}

// redirectURIFor is redirectURI for a sign-in actually in progress: it returns
// the callback on the origin this request arrived at, when that origin is one
// the platform already trusts.
//
// The state and PKCE cookies are set on the origin where sign-in began, and the
// browser only sends them back to that same origin. Pinning the callback to
// BASE_URL therefore breaks every entry point that is not BASE_URL — start at
// http://localhost:8080 through an SSH tunnel and the IdP returns the browser
// to the public hostname, which holds no state cookie, and the flow dies on
// "invalid state - mismatch" after the password has already been accepted.
//
// No new trust is introduced: the candidates are exactly trustedPlatformOrigins,
// so an attacker-supplied Host falls through to BASE_URL rather than steering
// the authorization code anywhere. Both the authorization request and the token
// exchange call this with the same request-bound origin, which OAuth requires
// to be byte-identical. The IdP must have each origin's callback registered —
// docker/dex/config.yaml lists all three for this reason.
func (s *Server) redirectURIFor(r *http.Request, providerID string) string {
	return s.platformBaseFor(r) + "/api/v1/auth/" + providerID + "/callback"
}

// platformBaseFor returns the trusted origin this request arrived at, or
// BASE_URL when the Host matches none of them.
//
// Every browser-facing redirect in the sign-in flow goes through here, not just
// the OAuth callback: sending someone who signed in through a tunnel back to
// BASE_URL lands them on a different origin from the one holding their new
// session cookie, so a successful sign-in presents as a login page.
func (s *Server) platformBaseFor(r *http.Request) string {
	if r != nil && r.Host != "" {
		for _, origin := range s.trustedPlatformOrigins(r) {
			u, err := url.Parse(origin)
			if err != nil || u.Host == "" {
				continue
			}
			if strings.EqualFold(u.Host, r.Host) {
				return strings.TrimSuffix(origin, "/")
			}
		}
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/")
}

func splitDomains(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.ToLower(strings.TrimSpace(part)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
