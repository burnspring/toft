package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/authprovider"
	"toft/internal/database"
	"toft/internal/entitlements"
)

// Admin → Authentication.
//
// An administrator adds, edits, enables and removes sign-in providers here. The
// surface is admin-only for an obvious reason: a provider is a door, and whoever
// can add one can add a door they control.
//
// Client secrets go in and never come out. The API accepts a secret, encrypts
// it, and thereafter reports only whether one is stored — so a compromised
// admin session cannot exfiltrate the credential that would let someone stand
// up a look-alike IdP.

// requireProviderAdmin gates every route in this file.
func (s *Server) requireProviderAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	if !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "only an administrator can manage sign-in providers",
			"hint":  "ask an administrator to make this change"})
		return false
	}
	return true
}

// adminProviderDTO is a provider as the admin UI sees it: everything except the
// secret, plus whether one is set and where the provider came from.
type adminProviderDTO struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	KindLabel   string `json:"kind_label"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	ClientID    string `json:"client_id"`
	SecretSet   bool   `json:"secret_set"`
	// SecretUnreadable is a stored client secret this install's keys cannot
	// decrypt — a lost or changed ENCRYPTION_KEY, or a row restored from
	// another install. The provider is broken rather than absent, so it stays
	// listed and the form says the value must be retyped. See credentials.go.
	SecretUnreadable bool `json:"secret_unreadable"`

	Issuer      string `json:"issuer,omitempty"`
	AuthURL     string `json:"auth_url,omitempty"`
	TokenURL    string `json:"token_url,omitempty"`
	UserinfoURL string `json:"userinfo_url,omitempty"`
	Scopes      string `json:"scopes,omitempty"`

	AllowedDomains      string `json:"allowed_domains"`
	DefaultRole         string `json:"default_role"`
	LinkByVerifiedEmail bool   `json:"link_by_verified_email"`
	AutoProvision       bool   `json:"auto_provision"`
	// GroupsClaim names the claim carrying directory group membership. Empty
	// means `groups`, and is what an install that has never mapped a group
	// reads — the field costs an unmapped install nothing.
	GroupsClaim string `json:"groups_claim"`
	// PermittedLoginGroups is the comma-separated set of claim values of which
	// a person must hold one to sign in. Empty means every group is permitted.
	PermittedLoginGroups string `json:"permitted_login_groups"`
	// AllowedEmails are individual addresses permitted alongside the domains,
	// in union with them.
	AllowedEmails string `json:"allowed_emails"`
	// GroupsSeparator is the delimiter a single-string group claim is split on.
	GroupsSeparator string `json:"groups_separator"`

	// Managed is false for an environment-configured provider: the operator owns
	// it, so the UI shows it read-only rather than offering a form that cannot
	// take effect.
	Managed bool `json:"managed"`
	// RedirectURI is what must be registered at the provider. Computed by the
	// server, because a hand-typed one that differs by a trailing slash fails
	// with an error that names the client secret.
	RedirectURI string `json:"redirect_uri"`
	// UserCount is how many identities point at this provider.
	UserCount int `json:"user_count"`
	// LoginReady means it would appear on the login page as things stand.
	LoginReady bool `json:"login_ready"`
}

type adminProvidersResponse struct {
	Providers []adminProviderDTO `json:"providers"`
	// Presets are form-fillers for known vendors, split into enterprise SSO and
	// social login by category. Served from the Go catalog so the frontend does
	// not carry a second copy of the vendor list.
	Presets []authprovider.Preset `json:"presets"`
}

func (s *Server) handleListAuthProviders(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAdmin(w, r) {
		return
	}

	rows, err := s.db.ListAuthProviders()
	if err != nil {
		log.Printf("listing auth providers: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Read off the stored row rather than the resolved provider: a secret that
	// will not decrypt resolves to an empty one, and "no secret set" and "a
	// secret nobody can read" are exactly the two states this page has to stop
	// showing as the same thing.
	unreadable := map[string]bool{}
	for i := range rows {
		unreadable[rows[i].ID] = s.credentialUnreadable(rows[i].EncryptedSecret())
	}

	// Counted from the identities themselves rather than from the rows above.
	// A provider configured in the environment has no row, so a count taken
	// from `auth_providers` could only ever say zero about it — which is what
	// this page said about the provider four people were signing in through.
	counts, err := s.db.CountIdentitiesByProvider()
	if err != nil {
		log.Printf("counting identities per provider: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Built from the merged registry rather than the rows, so an
	// environment-configured provider appears here too — an admin who cannot
	// see it has no way to understand why the login page offers it.
	providers, err := s.authProviders()
	if err != nil {
		log.Printf("listing auth providers: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := make([]adminProviderDTO, 0, len(providers))
	for i := range providers {
		p := &providers[i]
		// Password sign-in has no third party, so there is nothing to register a
		// redirect URI with. Blank rather than a URL that would 404 if anyone
		// pasted it somewhere.
		redirectURI := s.redirectURI(p.ID)
		if p.Kind == authprovider.KindPassword {
			redirectURI = ""
		}
		out = append(out, adminProviderDTO{
			ID: p.ID, Kind: p.Kind, KindLabel: kindLabel(p.Kind), DisplayName: p.DisplayName,
			Enabled: p.Enabled, ClientID: p.ClientID,
			SecretSet:        p.ClientSecret != "" || unreadable[p.ID],
			SecretUnreadable: unreadable[p.ID],
			Issuer:           p.Issuer, AuthURL: p.AuthURL, TokenURL: p.TokenURL, UserinfoURL: p.UserinfoURL,
			Scopes:         strings.Join(p.Scopes, " "),
			AllowedDomains: strings.Join(p.AllowedDomains, ", "),
			DefaultRole:    p.DefaultRole, LinkByVerifiedEmail: p.LinkByVerifiedEmail,
			AutoProvision: p.AutoProvision, GroupsClaim: p.GroupsClaim,
			PermittedLoginGroups: strings.Join(p.PermittedLoginGroups, ", "),
			AllowedEmails:        strings.Join(p.AllowedEmails, ", "),
			GroupsSeparator:      p.GroupsSeparator,
			Managed:              p.Managed, RedirectURI: redirectURI,
			UserCount:  counts[p.ID],
			LoginReady: p.Ready(),
		})
	}

	writeJSON(w, http.StatusOK, adminProvidersResponse{
		Providers: out,
		Presets:   authprovider.Presets(),
	})
}

// providerRequest is the write shape. Pointers where absence must be
// distinguishable from a zero value: a PATCH that omits `enabled` must not
// disable the provider.
type providerRequest struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	DisplayName  string  `json:"display_name"`
	Enabled      *bool   `json:"enabled"`
	ClientID     string  `json:"client_id"`
	ClientSecret *string `json:"client_secret"`

	Issuer      *string `json:"issuer"`
	AuthURL     *string `json:"auth_url"`
	TokenURL    *string `json:"token_url"`
	UserinfoURL *string `json:"userinfo_url"`
	Scopes      *string `json:"scopes"`

	AllowedDomains      *string `json:"allowed_domains"`
	DefaultRole         *string `json:"default_role"`
	LinkByVerifiedEmail *bool   `json:"link_by_verified_email"`
	AutoProvision       *bool   `json:"auto_provision"`
	GroupsClaim         *string `json:"groups_claim"`

	PermittedLoginGroups *string `json:"permitted_login_groups"`
	AllowedEmails        *string `json:"allowed_emails"`
	GroupsSeparator      *string `json:"groups_separator"`
}

func (s *Server) handleCreateAuthProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAdmin(w, r) {
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureIdentityProviders) {
		return
	}
	var body providerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	row := &database.AuthProvider{
		ID:          strings.ToLower(strings.TrimSpace(body.ID)),
		Kind:        strings.TrimSpace(body.Kind),
		DisplayName: strings.TrimSpace(body.DisplayName),
		Enabled:     body.Enabled == nil || *body.Enabled,
		ClientID:    strings.TrimSpace(body.ClientID),
		DefaultRole: database.SystemRoleViewer,
		// Absent means on, matching the column default and the behavior every
		// existing provider has.
		AutoProvision: true,
	}
	applyProviderFields(row, &body)

	// Local password sign-in is not something an administrator adds. There is
	// exactly one, migration 21 created it, and it authenticates exactly one
	// account — a second password provider would be a second local user
	// directory, which is precisely what an identity provider is for.
	if row.Kind == database.ProviderKindPassword || row.ID == database.LocalProviderID {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "local password sign-in already exists as the built-in `" + database.LocalProviderID +
				"` provider and cannot be duplicated. Add an OpenID Connect or GitHub provider instead.",
			"hint": `send "kind": "oidc" or "github" for a new provider; PATCH /api/v1/admin/auth-providers/local changes the built-in one`})
		return
	}

	// An environment-configured provider owns its id. Refused rather than
	// shadowed: a row that exists, validates, and is silently never used is
	// indistinguishable from a bug.
	for _, env := range s.envAuthProviders() {
		if env.ID == row.ID {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "the id " + row.ID + " is configured by this install's environment " +
					"(GITHUB_CLIENT_ID or OIDC_*). Unset that configuration to manage it here, or choose another id.",
				"hint": "choose another id — GET /api/v1/admin/auth-providers lists the ids in use"})
			return
		}
	}

	secret := strings.TrimSpace(derefOr(body.ClientSecret, ""))
	if msg := s.validateProviderRow(row, secret); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "correct the field the sentence names and send it again — nothing was saved"})
		return
	}
	if err := s.encryptProviderSecret(row, secret); err != nil {
		log.Printf("encrypting client secret: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID
	row.CreatedBy = p.ID

	if err := s.db.CreateAuthProvider(row, audit); err != nil {
		if errors.Is(err, database.ErrProviderExists) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "a provider with the id " + row.ID + " already exists",
				"hint":  "choose another id, or PATCH /api/v1/admin/auth-providers/" + row.ID + " to change the one that exists"})
			return
		}
		log.Printf("creating auth provider: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	authprovider.ForgetDiscovery(row.Issuer)

	writeJSON(w, http.StatusCreated, map[string]any{
		"id": row.ID, "redirect_uri": s.redirectURI(row.ID),
	})
}

func (s *Server) handleUpdateAuthProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAdmin(w, r) {
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureIdentityProviders) {
		return
	}
	id := chi.URLParam(r, "providerID")

	row, err := s.db.GetAuthProvider(id)
	if errors.Is(err, database.ErrProviderNotFound) {
		// An environment-configured provider is not editable, and saying so is
		// more useful than a bare 404 on an id the page is displaying.
		for _, env := range s.envAuthProviders() {
			if env.ID == id {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": env.DisplayName + " is configured by this install's environment and cannot be edited here",
					"hint":  "change it where this install is configured — the GITHUB_CLIENT_ID or OIDC_* variables — and restart it"})
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/auth-providers lists the providers by id"})
		return
	}
	if err != nil {
		log.Printf("reading auth provider %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	var body providerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	// Local password sign-in shares the row and the route but almost none of the
	// semantics: no client credentials, no issuer, and an enabled flag that only
	// travels one way through this API.
	if row.Kind == database.ProviderKindPassword {
		s.updateLocalProvider(w, r, row, &body)
		return
	}

	// A provider's kind is what it is. Changing it would turn an OIDC or
	// GitHub row into a second password-kind provider — a second login form,
	// a row no route can edit back, and one providerLockoutRefusal would then
	// treat as the guaranteed way in.
	if k := strings.TrimSpace(body.Kind); k != "" && k != row.Kind {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "a provider's kind cannot be changed — this one is " + row.Kind +
				". Add a new provider of the kind you want and remove this one.",
			"hint": "create one of the kind you want with POST /api/v1/admin/auth-providers, then delete this one"})
		return
	}
	previousIssuer := row.Issuer
	if body.DisplayName != "" {
		row.DisplayName = strings.TrimSpace(body.DisplayName)
	}
	if body.ClientID != "" {
		row.ClientID = strings.TrimSpace(body.ClientID)
	}
	wasEnabled := row.Enabled
	if body.Enabled != nil {
		row.Enabled = *body.Enabled
	}
	endpointsBefore := providerEndpointHosts(row)
	applyProviderFields(row, &body)

	// Before validation, because this refusal is about the install and the
	// others are about the form: an administrator taking down the last door
	// should be told that, not that their issuer is malformed.
	if wasEnabled && !row.Enabled {
		if msg := s.providerLockoutRefusal(row.ID, row.DisplayName); msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "sign in as an administrator through another enabled provider, then try again"})
			return
		}
	}

	// An empty secret means "keep the stored one" — the form never receives the
	// current secret, so it cannot echo it back, and treating empty as "clear"
	// would silently break sign-in on every unrelated edit.
	newSecret := strings.TrimSpace(derefOr(body.ClientSecret, ""))
	// Moving the provider's endpoints keeps nothing: the stored secret is
	// write-only through this API, and carrying it to a new issuer or token
	// endpoint handed it to whoever runs that host in two requests (code
	// review 2026-11-11, R1-1). A moved provider re-enters its secret.
	if newSecret == "" && row.EncryptedSecret() != "" && endpointsBefore != providerEndpointHosts(row) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "the issuer or an endpoint moved to another host; re-enter the client secret in the same request, because the stored one is never carried to a host it was not entered for",
			"hint":  "send client_secret with the new issuer, auth_url, token_url or userinfo_url"})
		return
	}
	if msg := s.validateProviderRow(row, firstNonEmptyStr(newSecret, row.EncryptedSecret())); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "correct the field the sentence names and send it again — nothing was saved"})
		return
	}
	if newSecret != "" {
		if err := s.encryptProviderSecret(row, newSecret); err != nil {
			log.Printf("encrypting client secret: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	} else {
		row.SetEncryptedSecret("")
	}

	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID

	if err := s.db.UpdateAuthProvider(row, audit); err != nil {
		log.Printf("updating auth provider %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// A corrected issuer must take effect on the next sign-in, not when the
	// discovery cache happens to expire.
	authprovider.ForgetDiscovery(previousIssuer)
	authprovider.ForgetDiscovery(row.Issuer)

	// `login_ready` comes back, because *switched on* and *on the login page*
	// are two different facts and the switch reports the wrong one on its own.
	// A provider with no client secret can be enabled and stay off the door;
	// the row in the table says so and the toast beside it used to say "Okta
	// enabled". It is the same `Ready()` the listing serializes.
	// The same two facts the listing serializes, computed the same way: what
	// this row *is* rather than what the request asked for. Enabled and
	// unreadable — an install whose key can no longer decrypt the secret — is
	// the state the row already labels "Secret unreadable", and it is the one
	// that must not be reported as success.
	unreadable := s.credentialUnreadable(row.EncryptedSecret())
	ready := row.Enabled &&
		(row.Kind == authprovider.KindPassword ||
			(row.ClientID != "" && row.EncryptedSecret() != "" && !unreadable))
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                row.ID,
		"login_ready":       ready,
		"secret_unreadable": unreadable,
	})
}

// updateLocalProvider is the write path for local password sign-in.
//
// Only two things about it are editable, and only one of them is interesting:
//
//	display name  cosmetic, like any provider's
//	enabled       may be set to false here, and never to true
//
// The asymmetry is the design. Switching local sign-in off is an ordinary
// administrative act with SSO working, so the UI does it. Switching it back on
// hands out a way in that bypasses the identity provider, so it takes a shell on
// the host — `toft local-admin enable`. A door that a stolen admin session
// can reopen was never shut, and this way the recovery path and the attack path
// are not the same request.
func (s *Server) updateLocalProvider(w http.ResponseWriter, r *http.Request, row *database.AuthProvider, body *providerRequest) {
	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID

	if body.Enabled != nil && *body.Enabled != row.Enabled {
		if *body.Enabled {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "local password sign-in cannot be switched on from here — it is a way in that " +
					"bypasses your identity provider, so it takes a shell on the host: run " +
					"`toft local-admin enable` on the server.",
				"hint": "run `toft local-admin enable` in a shell on the server"})
			return
		}
		if msg := s.localDisableRefusal(); msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "sign in as an administrator through another enabled provider, then try again"})
			return
		}
		if err := s.db.SetAuthProviderEnabled(row.ID, false, audit); err != nil {
			log.Printf("disabling local sign-in: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		log.Printf("local sign-in disabled by %s — re-enable with `toft local-admin enable`", p.ID)
	}

	if name := strings.TrimSpace(body.DisplayName); name != "" && name != row.DisplayName {
		row.DisplayName = name
		if err := s.db.UpdateAuthProvider(row, audit); err != nil {
			log.Printf("renaming local sign-in: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": row.ID})
}

// localDisableRefusal returns why local sign-in must stay on, or "".
//
// The test is not "is another provider configured" but "can an administrator
// actually get in through one" — a provider added five minutes ago that nobody
// has signed in through is not a way back. Getting this wrong locks an install
// out of its own admin surface, and while `toft local-admin enable` would
// recover it, needing a shell to undo a checkbox is not a good afternoon.
func (s *Server) localDisableRefusal() string {
	ids, err := s.ssoProviderIDs()
	if err != nil {
		log.Printf("checking for another way in: %v", err)
		return "could not verify that another sign-in provider is working — try again"
	}
	if len(ids) == 0 {
		return "local sign-in is the only way into this install. Add and enable an OpenID Connect " +
			"or GitHub provider, sign in through it as an administrator, then switch this off."
	}
	ok, err := s.db.HasAdminIdentityOn(ids)
	if err != nil {
		log.Printf("checking for an administrator on another provider: %v", err)
		return "could not verify that another sign-in provider is working — try again"
	}
	if !ok {
		return "no administrator has signed in through another provider yet, so switching this off " +
			"would leave nobody able to administer this install. Sign in as an administrator through " +
			"your identity provider first — then this can be switched off."
	}
	return ""
}

// providerLockoutRefusal returns why this identity provider must stay on the
// login page, or "".
//
// The other half of localDisableRefusal, and it was missing for as long as that
// one has existed. The pair guards one condition from two sides: an install
// running on a single identity provider with local sign-in already off could be
// switched off — or deleted — from the admin page, and the sentence that says
// what to do about it is `toft local-admin enable` on the host. Guarding
// only local sign-in guarded the door nobody closes.
//
// The test is the same one, for the same reason: not "is another provider
// configured" but "can an administrator actually get in through one". The
// difference is the short circuit — local password sign-in *is* the guarantee,
// because the built-in administrator is seeded by the schema and re-created if
// it is deleted, so an install still showing the password form always has a way
// back and this refusal has nothing to say.
func (s *Server) providerLockoutRefusal(id, displayName string) string {
	providers, err := s.loginProviders()
	if err != nil {
		log.Printf("checking for another way in: %v", err)
		return "could not verify that another way in would remain — try again"
	}
	remaining := make([]string, 0, len(providers))
	for _, p := range providers {
		if p.ID == id {
			continue
		}
		if p.Kind == authprovider.KindPassword {
			return ""
		}
		remaining = append(remaining, p.ID)
	}
	if len(remaining) == 0 {
		return displayName + " is the only way into this install, and local password sign-in is " +
			"off. Add and enable another provider and sign in through it as an administrator, or " +
			"turn local sign-in back on with `toft local-admin enable` on the server, before " +
			"taking this one down."
	}
	ok, err := s.db.HasAdminIdentityOn(remaining)
	if err != nil {
		log.Printf("checking for an administrator on another provider: %v", err)
		return "could not verify that another way in would remain — try again"
	}
	if !ok {
		return "no administrator has signed in through any other provider yet, so taking " +
			displayName + " down would leave nobody able to administer this install. Sign in as an " +
			"administrator through one of the others first."
	}
	return ""
}

func (s *Server) handleDeleteAuthProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "providerID")

	// The built-in local provider is not deletable. Deleting it would take away
	// the platform's guarantee that one way in always exists — and unlike
	// disabling it, nothing short of a migration would put it back.
	if id == database.LocalProviderID {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "local password sign-in is built in and cannot be removed. Switch it off instead, " +
				"once an administrator can sign in through another provider.",
			"hint": `send {"enabled": false} to PATCH /api/v1/admin/auth-providers/local instead`})
		return
	}

	// A delete closes the door as surely as the switch does, and more
	// permanently: the same guard, or the guard is one the delete walks past.
	// Read before the row goes, since the answer depends on it still being
	// there.
	if row, err := s.db.GetAuthProvider(id); err == nil && row != nil && row.Enabled {
		if msg := s.providerLockoutRefusal(id, row.DisplayName); msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "sign in as an administrator through another enabled provider, then try again"})
			return
		}
	}

	p := auth.GetPrincipal(r.Context())
	audit := auditEntryFor(r)
	audit.ActorType = database.PrincipalTypeUser
	audit.ActorID = p.ID

	if err := s.db.DeleteAuthProvider(id, audit); err != nil {
		if errors.Is(err, database.ErrProviderNotFound) {
			// The same guard the update handler has, in the same place and with
			// the same sentence. An environment-configured provider is not a
			// row, so there is nothing here to delete — and answering a bare
			// 404 for an id this same API had just listed teaches whoever reads
			// it that the list is unreliable.
			//
			// 403 and not 404, and the house rule does not apply: *404 over 403*
			// protects the existence of something the caller may not see, and
			// this provider is on the list they just fetched, on the page they
			// are looking at, behind a route only a provider administrator
			// reaches. There is nothing to conceal.
			for _, env := range s.envAuthProviders() {
				if env.ID == id {
					writeJSON(w, http.StatusForbidden, map[string]string{
						"error": env.DisplayName + " is configured by this install's environment and cannot be edited here",
						"hint":  "change it where this install is configured — the GITHUB_CLIENT_ID or OIDC_* variables — and restart it"})
					return
				}
			}
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/auth-providers lists the providers by id"})
			return
		}
		log.Printf("deleting auth provider %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDiscoverIssuer probes an issuer and reports what it found.
//
// The point is to fail at the form instead of at someone's first sign-in: a
// wrong issuer, a tenant with no discovery document, or an unreachable IdP all
// look identical from a login button.
func (s *Server) handleDiscoverIssuer(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAdmin(w, r) {
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureIdentityProviders) {
		return
	}
	var body struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	issuer := strings.TrimSpace(body.Issuer)
	if issuer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "an issuer URL is required", "hint": `send the issuer, as {"issuer": "https://login.example.com"}`})
		return
	}

	// Always a live fetch: the admin is asking what is there *now*, and
	// answering from a cache is how a corrected issuer appears to still be
	// broken.
	authprovider.ForgetDiscovery(issuer)
	doc, err := authprovider.Discover(r.Context(), issuer)
	if err != nil {
		// The admin typed the target, so they get the real error — this is the
		// diagnostic, and a summarized version would defeat it.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "hint": "check the issuer — its /.well-known/openid-configuration address should answer with JSON from this server"})
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// applyProviderFields copies the optional fields present in the request.
func applyProviderFields(row *database.AuthProvider, body *providerRequest) {
	if body.Issuer != nil {
		row.Issuer = strings.TrimSpace(*body.Issuer)
	}
	if body.AuthURL != nil {
		row.AuthURL = strings.TrimSpace(*body.AuthURL)
	}
	if body.TokenURL != nil {
		row.TokenURL = strings.TrimSpace(*body.TokenURL)
	}
	if body.UserinfoURL != nil {
		row.UserinfoURL = strings.TrimSpace(*body.UserinfoURL)
	}
	if body.Scopes != nil {
		row.Scopes = strings.Join(strings.Fields(*body.Scopes), " ")
	}
	if body.AllowedDomains != nil {
		row.AllowedDomains = strings.Join(splitDomains(*body.AllowedDomains), ",")
	}
	if body.DefaultRole != nil && *body.DefaultRole != "" {
		row.DefaultRole = *body.DefaultRole
	}
	if body.LinkByVerifiedEmail != nil {
		row.LinkByVerifiedEmail = *body.LinkByVerifiedEmail
	}
	if body.GroupsClaim != nil {
		row.GroupsClaim = strings.TrimSpace(*body.GroupsClaim)
	}
	if body.PermittedLoginGroups != nil {
		// Normalized through the same splitter the domains use, so the stored
		// form is what the reader expects and a list typed with stray spaces
		// or a trailing comma stores clean.
		row.PermittedLoginGroups = strings.Join(splitDomains(*body.PermittedLoginGroups), ",")
	}
	if body.AllowedEmails != nil {
		row.AllowedEmails = strings.Join(splitDomains(*body.AllowedEmails), ",")
	}
	if body.GroupsSeparator != nil {
		// Not trimmed. A separator may legitimately *be* a space, and trimming
		// would turn that into "no separator" — silently, on a form somebody
		// filled in on purpose.
		row.GroupsSeparator = *body.GroupsSeparator
	}
	if body.AutoProvision != nil {
		row.AutoProvision = *body.AutoProvision
	}
}

// validateProviderRow returns a human-readable reason the provider would not
// work, or "".
func (s *Server) validateProviderRow(row *database.AuthProvider, secret string) string {
	// Validated through the same type that performs sign-in, so the form cannot
	// accept a shape the flow rejects.
	p := authprovider.Provider{
		ID: row.ID, Kind: row.Kind, DisplayName: row.DisplayName,
		ClientID: row.ClientID, Issuer: row.Issuer,
		AuthURL: row.AuthURL, TokenURL: row.TokenURL, UserinfoURL: row.UserinfoURL,
		// Carried so Validate can refuse a group gate on a kind that asserts no
		// group claim. The struct is deliberately built field by field rather
		// than resolved, and a field the validator needs has to be added here
		// or the check silently passes on every provider.
		PermittedLoginGroups: row.PermittedLoginGroupList(),
	}
	if err := p.Validate(); err != nil {
		return err.Error()
	}
	if secret == "" {
		return "a client secret is required"
	}
	switch row.DefaultRole {
	case database.SystemRoleViewer, database.SystemRolePublisher:
	case database.ProvisionPending:
		// Not a role, and accepted here because it is chosen from the same
		// control: the accounts this provider creates wait for an
		// administrator, who picks the role when they let each one in.
	case database.SystemRoleAdmin:
		// Anyone who can sign in would become an administrator, which turns
		// "add a provider" into "hand the platform to whoever the IdP
		// authenticates". Promotion stays a deliberate, audited act.
		return "the default role cannot be system_admin — promote individual users instead"
	default:
		return "the default role must be viewer, publisher, or pending activation"
	}
	return ""
}

// encryptProviderSecret stores a client secret as ciphertext. The secret is
// trimmed by both callers before it arrives: a secret pasted out of an IdP's
// console carries a trailing newline often enough, and the provider's answer to
// one is `incorrect_client_credentials` — a failure that names the credential
// but not the whitespace, at the far end of a sign-in the administrator has to
// walk through a browser to reach.
func (s *Server) encryptProviderSecret(row *database.AuthProvider, secret string) error {
	if secret == "" {
		row.SetEncryptedSecret("")
		return nil
	}
	ciphertext, err := s.encryptor.Encrypt(secret)
	if err != nil {
		return err
	}
	row.SetEncryptedSecret(ciphertext)
	return nil
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}

// --- The SCIM feed's token ---

// handleMintSCIMToken issues the credential a directory presents to
// `/scim/v2`, and returns it exactly once.
//
// Shown once and stored hashed, like every other machine credential here. A
// token that could be read back is a token an administrator does not have to
// look after, and this one can lock every account on the install.
//
// Minting again **rotates**: the previous hash is overwritten in the same
// statement, so the old token stops working the moment the new one exists.
// There is no window in which both are valid, which is the right trade for this
// credential — an administrator pasting a new token into an IdP console is a
// smaller inconvenience than two live secrets nobody is tracking.
func (s *Server) handleMintSCIMToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAdmin(w, r) {
		return
	}
	if s.refusedUnlicensed(w, r, entitlements.FeatureIdentityProviders) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	providerID := chi.URLParam(r, "providerID")

	row, err := s.db.GetAuthProvider(providerID)
	if err != nil || row == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found", "hint": "GET /api/v1/admin/auth-providers lists the providers by id"})
		return
	}
	// A password provider authenticates one local account and has no directory
	// behind it, so there is nothing a feed could be a feed *of*.
	if row.Kind == database.ProviderKindPassword {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "local sign-in has no directory behind it, so it cannot have a SCIM feed",
			"hint":  "mint the SCIM token on the provider your directory signs people in through",
		})
		return
	}

	secret, err := randomToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	sum := sha256.Sum256([]byte(secret))
	meta := requestMeta(r.Context())
	if err := s.db.SetSCIMToken(r.Context(), providerID, hex.EncodeToString(sum[:]), database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		log.Printf("minting SCIM token for %s: %v", providerID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"token": secret,
		// The address to paste into the IdP, worked out from this install's own
		// base rather than typed by hand into two places.
		"base_url": strings.TrimSuffix(s.cfg.BaseURL, "/") + "/scim/v2",
	})
}

// handleRevokeSCIMToken clears the token. Off means absent, and off means now:
// the next call from the directory is a 401, with no grace period.
func (s *Server) handleRevokeSCIMToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	if err := s.db.SetSCIMToken(r.Context(), chi.URLParam(r, "providerID"), "", database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}); err != nil {
		if errors.Is(err, database.ErrProviderNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "provider not found", "hint": "GET /api/v1/admin/auth-providers lists the providers by id"})
			return
		}
		log.Printf("revoking SCIM token: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// randomToken mints 32 bytes of entropy, hex-encoded. No prefix: this is not
// one of the three key classes and must not be spelled as though it were.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// providerEndpointHosts is the set of hosts a provider's secret is sent to or
// trusted from, as one comparable string (R1-1).
func providerEndpointHosts(row *database.AuthProvider) string {
	host := func(raw string) string {
		if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
			return strings.ToLower(u.Host)
		}
		return strings.ToLower(raw)
	}
	return host(row.Issuer) + "|" + host(row.AuthURL) + "|" + host(row.TokenURL) + "|" + host(row.UserinfoURL)
}
