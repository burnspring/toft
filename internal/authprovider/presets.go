package authprovider

// The preset catalog: what the admin UI offers as a starting point.
//
// A preset is *only* a form filler. Nothing here is required to add a provider,
// nothing here is consulted at sign-in, and a provider not listed here is not
// second-class — any OIDC issuer works, because everything vendor-specific is
// in the issuer's own discovery document. The catalog exists so an admin adding
// Okta does not have to remember the shape of an Okta issuer URL.
//
// It lives in Go and is served to the frontend rather than duplicated there:
// two copies of a vendor list drift, and the one that drifts is the one nobody
// is looking at.
//
// Two categories, and the split is a product statement. Enterprise SSO is the
// feature — one corporate IdP, every employee, provisioning policy attached.
// Social login is a convenience for small internal installs, presented as a
// secondary option, and each one still has to be given its own client
// credentials and switched on before it appears to anyone.

// Preset categories.
const (
	CategoryEnterprise = "enterprise"
	CategorySocial     = "social"
)

// Preset is a known provider's shape.
type Preset struct {
	// ID is the suggested slug. It ends up in the redirect URI.
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Category string `json:"category"`
	// Issuer is fixed for a single-tenant vendor. Empty when the issuer depends
	// on the customer's own tenant, in which case IssuerTemplate shows the
	// shape with the parts to replace in braces.
	Issuer         string `json:"issuer,omitempty"`
	IssuerTemplate string `json:"issuer_template,omitempty"`
	// Scopes overrides the kind's defaults where a vendor needs it.
	Scopes []string `json:"scopes,omitempty"`
	// MultiTenant marks a provider that authenticates accounts from outside the
	// organization. For these the admin UI insists on an email domain
	// restriction, because "anyone with a Google account" is otherwise the
	// access policy.
	MultiTenant bool `json:"multi_tenant,omitempty"`
	// Hint is one line of setup guidance, shown next to the form.
	Hint string `json:"hint,omitempty"`
	// DocsURL is where the vendor documents creating the OAuth client.
	DocsURL string `json:"docs_url,omitempty"`
}

var presets = []Preset{
	// --- Enterprise SSO ---
	{
		ID: "sso", Name: "OpenID Connect (any provider)", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "https://{your-idp-host}",
		Hint:           "Works with any OIDC-compliant provider. Paste the issuer URL and the endpoints are discovered from it.",
	},
	{
		ID: "okta", Name: "Okta", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "https://{your-org}.okta.com/oauth2/default",
		Hint:           "Create a Web application in Okta. Use the custom authorization server's issuer, not the org URL alone.",
		DocsURL:        "https://developer.okta.com/docs/guides/implement-oauth-for-okta/main/",
	},
	{
		ID: "entra", Name: "Microsoft Entra ID", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "https://login.microsoftonline.com/{tenant-id}/v2.0",
		Hint:           "Register an application in Entra ID and add a Web redirect URI. Use your tenant id, not `common`, so only your directory can sign in.",
		DocsURL:        "https://learn.microsoft.com/entra/identity-platform/v2-protocols-oidc",
	},
	{
		ID: "auth0", Name: "Auth0", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "https://{your-tenant}.auth0.com/",
		Hint:           "Create a Regular Web Application. The issuer includes the trailing slash.",
		DocsURL:        "https://auth0.com/docs/get-started/applications",
	},
	{
		ID: "keycloak", Name: "Keycloak", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "https://{host}/realms/{realm}",
		Hint:           "Create a confidential client with the standard flow enabled, then copy its secret from the Credentials tab.",
		DocsURL:        "https://www.keycloak.org/securing-apps/oidc-layers",
	},
	{
		ID: "ping", Name: "PingFederate / PingOne", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "https://auth.pingone.com/{environment-id}/as",
		Hint:           "Create a Web App with the authorization code grant.",
	},
	{
		ID: "onelogin", Name: "OneLogin", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "https://{your-org}.onelogin.com/oidc/2",
		Hint:           "Add an OpenID Connect (Web) app and set the token endpoint to client secret POST or Basic.",
	},
	{
		ID: "jumpcloud", Name: "JumpCloud", Kind: KindOIDC, Category: CategoryEnterprise,
		Issuer: "https://oauth.id.jumpcloud.com",
		Hint:   "Create an SSO application of type OIDC.",
	},
	{
		ID: "dex", Name: "Dex (local development)", Kind: KindOIDC, Category: CategoryEnterprise,
		IssuerTemplate: "http://localhost:5556/dex",
		Hint:           "The OIDC provider in docker-compose. `scripts/dev.sh start --oauth` brings it up with a static user roster.",
		DocsURL:        "https://dexidp.io/docs/getting-started/",
	},

	// --- Social / developer accounts ---
	//
	// Every one of these authenticates accounts the organization does not
	// control, which is why they all carry MultiTenant.
	{
		ID: "google", Name: "Google", Kind: KindOIDC, Category: CategorySocial,
		Issuer: "https://accounts.google.com", MultiTenant: true,
		Hint:    "Create an OAuth client ID of type Web application. Restrict to your Workspace domain below — otherwise any Google account can sign in.",
		DocsURL: "https://developers.google.com/identity/openid-connect/openid-connect",
	},
	{
		ID: "github", Name: "GitHub", Kind: KindGitHub, Category: CategorySocial,
		MultiTenant: true,
		Hint:        "Register an OAuth App under your organization's developer settings. GitHub predates OIDC, so there is no issuer to configure.",
		DocsURL:     "https://docs.github.com/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app",
	},
	{
		ID: "microsoft", Name: "Microsoft account", Kind: KindOIDC, Category: CategorySocial,
		Issuer: "https://login.microsoftonline.com/common/v2.0", MultiTenant: true,
		Hint: "Personal and any-organization Microsoft accounts. For employees only, use Microsoft Entra ID above with your tenant id.",
	},
	{
		ID: "gitlab", Name: "GitLab", Kind: KindOIDC, Category: CategorySocial,
		Issuer: "https://gitlab.com", MultiTenant: true,
		Scopes: []string{"openid", "profile", "email"},
		Hint:   "Add an Application under user or group settings. Self-managed GitLab works too — set the issuer to your instance URL.",
	},
	{
		ID: "slack", Name: "Slack", Kind: KindOIDC, Category: CategorySocial,
		Issuer: "https://slack.com", MultiTenant: true,
		Scopes: []string{"openid", "profile", "email"},
		Hint:   "Enable Sign in with Slack on a Slack app. Restrict to your workspace's email domain below.",
	},
}

// Presets returns a copy of the catalog so callers cannot mutate the source.
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}

// PresetByID finds a preset, or nil.
func PresetByID(id string) *Preset {
	for i := range presets {
		if presets[i].ID == id {
			p := presets[i]
			return &p
		}
	}
	return nil
}
