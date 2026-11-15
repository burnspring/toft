package authprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDC discovery: one GET, cached.
//
// Discovery is what makes "support any OIDC provider" a single implementation
// rather than a vendor list. The issuer is the only thing an administrator has
// to know, because everything else — endpoints, supported scopes, signing keys
// — is published at a well-known path beneath it.

// DiscoveryDoc is the subset of the provider metadata document this platform
// uses. Deliberately a subset: fields we do not act on would only invite code
// that pretends to honor them.
type DiscoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	// ScopesSupported and the two below are surfaced to the admin UI so a
	// discovery probe can show what was actually found, which is the fastest
	// way to tell a wrong issuer from a wrong client secret.
	ScopesSupported []string `json:"scopes_supported,omitempty"`
	EndSessionURL   string   `json:"end_session_endpoint,omitempty"`
	JWKSURI         string   `json:"jwks_uri,omitempty"`
}

const discoveryTTL = 30 * time.Minute

type cachedDoc struct {
	doc     *DiscoveryDoc
	fetched time.Time
}

var (
	discoveryMu    sync.Mutex
	discoveryCache = map[string]cachedDoc{}
)

// Discover fetches (or returns a cached) discovery document for an issuer.
//
// Cached for half an hour: an IdP's endpoints change on the order of never, and
// a cache miss on every sign-in would put a third party's availability directly
// in the platform's login path.
func Discover(ctx context.Context, issuer string) (*DiscoveryDoc, error) {
	if issuer == "" {
		return nil, fmt.Errorf("no issuer configured")
	}
	key := strings.TrimSuffix(issuer, "/")

	discoveryMu.Lock()
	if hit, ok := discoveryCache[key]; ok && time.Since(hit.fetched) < discoveryTTL {
		discoveryMu.Unlock()
		return hit.doc, nil
	}
	discoveryMu.Unlock()

	doc, err := fetchDiscovery(ctx, key)
	if err != nil {
		return nil, err
	}

	discoveryMu.Lock()
	discoveryCache[key] = cachedDoc{doc: doc, fetched: time.Now()}
	discoveryMu.Unlock()
	return doc, nil
}

// ForgetDiscovery drops a cached document. Called when an administrator edits a
// provider, so a corrected issuer takes effect on the next sign-in instead of
// half an hour later.
func ForgetDiscovery(issuer string) {
	discoveryMu.Lock()
	delete(discoveryCache, strings.TrimSuffix(issuer, "/"))
	discoveryMu.Unlock()
}

func fetchDiscovery(ctx context.Context, issuer string) (*DiscoveryDoc, error) {
	if err := checkHTTPSURL(issuer); err != nil {
		return nil, err
	}
	url := issuer + "/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d — check the issuer URL (it must be the issuer itself, not the authorization endpoint)",
			url, resp.StatusCode)
	}

	var doc DiscoveryDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", url, err)
	}

	// The issuer identifier in the document must match what was configured. A
	// mismatch means the URL is a proxy, a tenant alias, or the wrong tenant
	// entirely, and every one of those makes the tokens belong to a different
	// authority than the administrator thinks.
	//
	// Azure AD is the standing exception: its multi-tenant `common` endpoint
	// publishes a templated issuer, by design.
	if doc.Issuer != "" && strings.TrimSuffix(doc.Issuer, "/") != issuer && !strings.Contains(doc.Issuer, "{tenantid}") {
		return nil, fmt.Errorf("issuer mismatch: configured %s but the document at %s declares %s",
			issuer, url, doc.Issuer)
	}
	return &doc, nil
}

// oidcProfile reads the userinfo endpoint and normalizes it.
//
// groupsClaim names the claim carrying group membership, empty meaning
// `groups`. It is read here *and* from the id_token (see claimsFromIDToken),
// because providers disagree about where it lives and the union is one map
// merge — Okta and Keycloak commonly put it in userinfo, and Entra does not put
// it there at all.
func oidcProfile(ctx context.Context, client *http.Client, userinfoURL, groupsClaim, groupsSeparator string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo endpoint returned %d", resp.StatusCode)
	}

	// email_verified is specified as a boolean but is shipped as a string by
	// enough providers that a strict bool decode fails real sign-ins. Decoded
	// loosely, then interpreted strictly: anything that is not affirmatively
	// true is false, because this value gates account linking.
	var raw struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		EmailVerified     any    `json:"email_verified"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Nickname          string `json:"nickname"`
		Picture           string `json:"picture"`
	}
	// Read once and decoded twice: the struct above for the fields this product
	// has always known, and a bare map for the group claim, whose *name* is an
	// administrator's setting and cannot be a struct tag.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUserinfoBytes))
	if err != nil {
		return nil, fmt.Errorf("reading userinfo: %w", err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decoding userinfo: %w", err)
	}
	if raw.Sub == "" {
		return nil, fmt.Errorf("userinfo response contained no subject")
	}

	p := &Profile{
		Subject:       raw.Sub,
		Login:         firstNonEmpty(raw.PreferredUsername, raw.Nickname, raw.Name, localPart(raw.Email), raw.Sub),
		Name:          raw.Name,
		Email:         raw.Email,
		EmailVerified: truthy(raw.EmailVerified),
		// Said false, as opposed to not said at all.
		EmailUnverified: raw.EmailVerified != nil && !truthy(raw.EmailVerified),
		AvatarURL:       raw.Picture,
	}

	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err == nil {
		if groups, ok := claimStrings(claims, GroupsClaimOr(groupsClaim), groupsSeparator); ok {
			p.Groups, p.GroupsAsserted = groups, true
		}
	}
	return p, nil
}

// maxUserinfoBytes bounds a userinfo response. A person in a great many groups
// produces a large one, and this is on the sign-in path.
const maxUserinfoBytes = 1 << 20

// GroupsClaimOr resolves a provider's configured claim name.
//
// Empty means `groups`, and the column stores empty rather than the literal so
// that an upgrade writes nothing and a provider created before this existed is
// indistinguishable from one whose administrator never touched the field.
func GroupsClaimOr(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return "groups"
	}
	return strings.TrimSpace(configured)
}

// claimStrings reads a claim that may be a list or a single value, and reports
// whether it was **present at all**.
//
// The second return is the whole point and is not len(values) > 0. Microsoft
// Entra omits the groups claim entirely once a person is in more groups than it
// will assert — 150 for SAML, 200 for JWT — and sends an overage indicator
// instead; Okta can be configured to omit it per application. A caller that read
// absence as "this person is in no groups" would remove every directory
// membership on the install, for everybody, on a sign-in nobody caused.
//
// A present claim of the wrong shape — a number, an object — reads as absent
// rather than empty, for the same reason: it is a provider this product does not
// understand, not an assertion that somebody is in nothing.
// The separator is applied to every string it meets, in the list arm as well as
// the bare-string one. A list whose elements are themselves delimited is odd,
// but an administrator who set a separator has said what their directory does,
// and honouring it in one arm and not the other would be the surprising rule.
func claimStrings(claims map[string]any, name, separator string) ([]string, bool) {
	v, ok := claims[name]
	if !ok || v == nil {
		return nil, false
	}
	// Split one claim value on the configured separator. Empty separator means
	// no split, which is exactly what every provider read before the field
	// existed: a bare string is a list of one.
	expand := func(dst []string, s string) []string {
		if separator == "" {
			if s = strings.TrimSpace(s); s != "" {
				dst = append(dst, s)
			}
			return dst
		}
		for _, part := range strings.Split(s, separator) {
			if part = strings.TrimSpace(part); part != "" {
				dst = append(dst, part)
			}
		}
		return dst
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = expand(out, s)
		}
		return out, true
	case []string:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = expand(out, e)
		}
		return out, true
	case string:
		// A single-valued claim. Some providers send one group as a bare
		// string rather than a list of one, and some send the whole list that
		// way with a delimiter between them — which is what `separator` is for.
		return expand(nil, t), true
	default:
		return nil, false
	}
}

// claimsFromIDToken reads the claims out of an id_token **without verifying its
// signature**, and that is a decision rather than an omission.
//
// OIDC Core §3.1.3.7 permits a client that received an id_token directly from
// the token endpoint, over a TLS channel it authenticated, to skip signature
// validation — the transport is the assurance. This platform already trusts that
// exact channel for the access token it then presents to userinfo, so refusing
// to read a claim from the same response would be a consistency nobody benefits
// from, bought with JWKS fetching, key caching and rotation handling on the
// sign-in path.
//
// What keeps that safe is the narrow use: **the id_token is read for claims and
// never for identity.** `Subject` keeps coming from userinfo, so the seam where
// an account is resolved is untouched and this cannot become an authentication
// bypass. Anything unparseable is absent, never empty.
func claimsFromIDToken(raw string) map[string]any {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "True" || t == "1"
	default:
		return false
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func localPart(email string) string {
	if at := strings.LastIndex(email, "@"); at > 0 {
		return email[:at]
	}
	return ""
}
