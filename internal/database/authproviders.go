package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sign-in providers and the identities they map onto users.
//
// The split is the point: `users` is the platform's notion of a person and
// everything else keys on `users.id`, while `user_identities` records which
// external accounts prove someone *is* that person. One user, many identities —
// which is what makes adding a second provider a configuration change rather
// than a migration of every table that references a user.
//
// Provider secrets are encrypted at rest and never leave this package in
// plaintext form; the field holding the ciphertext is unexported so it cannot be
// serialized by accident.

// Provider kinds.
const (
	// ProviderKindOIDC is any OpenID Connect provider: endpoints come from the
	// issuer's discovery document, so Okta, Entra, Google, Auth0, Keycloak,
	// Dex and GitLab are all this one kind.
	ProviderKindOIDC = "oidc"
	// ProviderKindGitHub is GitHub's OAuth2, which predates OIDC: no discovery
	// document, no id_token, and a bespoke user API. Its own kind because none
	// of the OIDC machinery applies to it.
	ProviderKindGitHub = "github"
	// ProviderKindPassword is local sign-in: a username and a password this
	// install stores, with no third party in the flow and therefore no client
	// id, no secret and no redirect URI. Exactly one row has this kind — `local`,
	// created by migration 21 — and it exists so an install with no identity
	// provider configured yet still has a door.
	ProviderKindPassword = "password"
)

// ErrProviderExists means the slug is taken. Distinct so the API can answer 409
// rather than a 500 on the primary key violation.
var ErrProviderExists = errors.New("a provider with that id already exists")

// ErrProviderNotFound means no such provider row.
var ErrProviderNotFound = errors.New("provider not found")

// AuthProvider is one configured door into the platform.
type AuthProvider struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	ClientID    string `json:"client_id"`

	Issuer      string `json:"issuer,omitempty"`
	AuthURL     string `json:"auth_url,omitempty"`
	TokenURL    string `json:"token_url,omitempty"`
	UserinfoURL string `json:"userinfo_url,omitempty"`
	Scopes      string `json:"scopes,omitempty"`

	AllowedDomains      string `json:"allowed_domains"`
	DefaultRole         string `json:"default_role"`
	LinkByVerifiedEmail bool   `json:"link_by_verified_email"`
	// AutoProvision permits a successful sign-in with no matching identity to
	// create an account. Off means the provider only authenticates people who
	// already have one.
	AutoProvision bool `json:"auto_provision"`
	// SCIMTokenHash is the hashed credential a directory presents to the SCIM
	// endpoint. Never rendered: the JSON tag is "-" so a provider listing
	// cannot leak it, and SCIMTokenSet is what the UI reads instead.
	SCIMTokenHash  string     `json:"-"`
	SCIMTokenSetAt *time.Time `json:"scim_token_set_at,omitempty"`
	SCIMLastCallAt *time.Time `json:"scim_last_call_at,omitempty"`
	// GroupsClaim names the claim this provider asserts directory group
	// membership in. Empty means `groups`; the column stores empty rather than
	// the literal, so an upgrade writes nothing and a provider created before
	// this existed reads the same as one nobody has configured.
	GroupsClaim string `json:"groups_claim"`
	// PermittedLoginGroups is the comma-separated set of claim values of which
	// a person must hold at least one to sign in through this provider. Empty
	// means every group is permitted, which is what every provider created
	// before this existed reads.
	//
	// Claim values rather than group ids: this gate runs before any mapping is
	// consulted, so it does not depend on `group_claim_mappings` and a value
	// nobody has attached to a group is still usable.
	PermittedLoginGroups string `json:"permitted_login_groups"`
	// AllowedEmails is the comma-separated set of individual addresses
	// permitted to sign in, in union with AllowedDomains.
	AllowedEmails string `json:"allowed_emails"`
	// GroupsSeparator is the delimiter a group claim delivered as one string is
	// split on. Empty means no split — a bare string is a list of one, which is
	// what every provider read before this existed.
	GroupsSeparator string `json:"groups_separator"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// UserCount is how many identities point at this provider — what an admin
	// needs in order to know what disabling or deleting it costs.
	UserCount int `json:"user_count"`

	// SecretSet reports that a client secret is stored, which is the only thing
	// about it any caller may learn.
	SecretSet bool `json:"secret_set"`

	// secret is the encrypted client secret. Unexported deliberately: a struct
	// that cannot marshal the field cannot leak it through a handler that
	// forgets to build a DTO.
	secret string
}

// EncryptedSecret returns the stored ciphertext for the caller that holds the
// decryption key. Named for what it is so no call site mistakes it for a
// usable secret.
func (p *AuthProvider) EncryptedSecret() string { return p.secret }

// SetEncryptedSecret stores already-encrypted ciphertext. Encryption happens in
// the API layer, which owns the key; this package only persists.
func (p *AuthProvider) SetEncryptedSecret(ciphertext string) {
	p.secret = ciphertext
	p.SecretSet = ciphertext != ""
}

// AllowedDomainList splits the stored comma-separated form. Empty means every
// domain the provider authenticates is acceptable.
func (p *AuthProvider) AllowedDomainList() []string {
	return splitAndTrimLower(p.AllowedDomains)
}

// AllowedEmailList splits the stored comma-separated form. Empty means the
// domain list is the whole of the address policy.
func (p *AuthProvider) AllowedEmailList() []string {
	return splitAndTrimLower(p.AllowedEmails)
}

// PermittedLoginGroupList splits the stored comma-separated form. Empty means
// every group this provider asserts is permitted to sign in.
//
// Lowercased on the way out, and matching is done against lowercased claim
// values: Entra emits object GUIDs whose case is not stable across surfaces,
// and no directory in practice distinguishes two groups by case alone.
func (p *AuthProvider) PermittedLoginGroupList() []string {
	return splitAndTrimLower(p.PermittedLoginGroups)
}

// ScopeList splits the stored space-separated form. Empty means the kind's
// defaults apply.
func (p *AuthProvider) ScopeList() []string {
	return strings.Fields(p.Scopes)
}

func splitAndTrimLower(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.ToLower(strings.TrimSpace(part)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

const authProviderColumns = `id, kind, display_name, enabled, client_id, client_secret,
	issuer, auth_url, token_url, userinfo_url, scopes,
	allowed_domains, default_role, link_by_verified_email, auto_provision, groups_claim,
	permitted_login_groups, allowed_emails, groups_separator,
	scim_token_hash, scim_token_set_at, scim_last_call_at,
	COALESCE(created_by, ''), created_at, updated_at`

func scanAuthProvider(scan func(...any) error) (*AuthProvider, error) {
	p := &AuthProvider{}
	err := scan(&p.ID, &p.Kind, &p.DisplayName, &p.Enabled, &p.ClientID, &p.secret,
		&p.Issuer, &p.AuthURL, &p.TokenURL, &p.UserinfoURL, &p.Scopes,
		&p.AllowedDomains, &p.DefaultRole, &p.LinkByVerifiedEmail, &p.AutoProvision, &p.GroupsClaim,
		&p.PermittedLoginGroups, &p.AllowedEmails, &p.GroupsSeparator,
		&p.SCIMTokenHash, &p.SCIMTokenSetAt, &p.SCIMLastCallAt,
		&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.SecretSet = p.secret != ""
	return p, nil
}

// CountIdentitiesByProvider is how many accounts sign in through each provider
// id, whether or not that provider has a row in this table.
//
// The count used to be a subquery on `auth_providers`, which meant it could only
// ever answer for a provider that *is* a row — and an environment-configured one
// is not. It is merged into the list from the registry, so Admin →
// Authentication looked it up in a map built from rows alone and got the zero
// value: an install whose only working provider was `OIDC_ISSUER` reported "0
// accounts" beside it, while four people signed in through it, including the
// administrator reading the page.
//
// Keyed by provider id and read by whoever assembled the list, so the answer no
// longer depends on where the provider was configured.
func (db *DB) CountIdentitiesByProvider() (map[string]int, error) {
	rows, err := db.conn.Query(`SELECT provider_id, COUNT(*) FROM user_identities GROUP BY provider_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ListAuthProviders returns every configured provider, enabled or not, with the
// number of identities attached to each.
func (db *DB) ListAuthProviders() ([]AuthProvider, error) {
	rows, err := db.conn.Query(`SELECT ` + authProviderColumns + `,
		(SELECT COUNT(*) FROM user_identities i WHERE i.provider_id = auth_providers.id)
		FROM auth_providers ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AuthProvider{}
	for rows.Next() {
		var count int
		p, err := scanAuthProvider(func(dest ...any) error {
			return rows.Scan(append(dest, &count)...)
		})
		if err != nil {
			return nil, err
		}
		p.UserCount = count
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetAuthProvider returns one provider, or ErrProviderNotFound.
func (db *DB) GetAuthProvider(id string) (*AuthProvider, error) {
	p, err := scanAuthProvider(db.conn.QueryRow(
		`SELECT `+authProviderColumns+` FROM auth_providers WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProviderNotFound
	}
	return p, err
}

// CreateAuthProvider writes the row and its audit entry in one transaction.
// defaultRoleOrViewer keeps the column's promise. The INSERT names
// `default_role`, so the column's own DEFAULT never applies, and a caller that
// left the field alone stored the empty string — which is not a rung on the
// ladder and not the provisioning policy either.
//
// Viewer is the conservative reading and is what the column has always
// defaulted to: a provider that has not said what it hands out hands out the
// least. The fourth value, `pending`, is a provisioning policy rather than a
// role and is never reached by an absence — an install has to choose it.
func defaultRoleOrViewer(role string) string {
	switch role {
	case SystemRoleAdmin, SystemRolePublisher, SystemRoleViewer, ProvisionPending:
		return role
	default:
		return SystemRoleViewer
	}
}

func (db *DB) CreateAuthProvider(p *AuthProvider, audit AuditEntry) error {
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt

	exists, err := db.authProviderExists(p.ID)
	if err != nil {
		return err
	}
	if exists {
		return ErrProviderExists
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO auth_providers (id, kind, display_name, enabled, client_id, client_secret,
			issuer, auth_url, token_url, userinfo_url, scopes,
			allowed_domains, default_role, link_by_verified_email, auto_provision, groups_claim,
			permitted_login_groups, allowed_emails, groups_separator,
			created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Kind, p.DisplayName, p.Enabled, p.ClientID, p.secret,
		p.Issuer, p.AuthURL, p.TokenURL, p.UserinfoURL, p.Scopes,
		p.AllowedDomains, defaultRoleOrViewer(p.DefaultRole), p.LinkByVerifiedEmail, p.AutoProvision, p.GroupsClaim,
		p.PermittedLoginGroups, p.AllowedEmails, p.GroupsSeparator,
		nullIfEmpty(p.CreatedBy), p.CreatedAt, p.UpdatedAt); err != nil {
		return err
	}

	audit.Action = "auth_provider.create"
	audit.TargetType = "auth_provider"
	audit.TargetID = p.ID
	audit.After = jsonStr(p)
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateAuthProvider rewrites every mutable column. The id is not one of them:
// it is baked into the redirect URI registered at the provider, so changing it
// would break sign-in without touching anything visible.
//
// An empty secret means "keep the stored one" — the API never receives the
// current secret back from a form, so an empty field cannot be allowed to mean
// "clear it".
func (db *DB) UpdateAuthProvider(p *AuthProvider, audit AuditEntry) error {
	before, err := db.GetAuthProvider(p.ID)
	if err != nil {
		return err
	}
	if p.secret == "" {
		p.secret = before.secret
	}
	p.CreatedAt = before.CreatedAt
	p.CreatedBy = before.CreatedBy
	p.UpdatedAt = time.Now().UTC()
	p.SecretSet = p.secret != ""

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE auth_providers SET kind = ?, display_name = ?, enabled = ?, client_id = ?,
			client_secret = ?, issuer = ?, auth_url = ?, token_url = ?, userinfo_url = ?,
			scopes = ?, allowed_domains = ?, default_role = ?, link_by_verified_email = ?,
			auto_provision = ?, groups_claim = ?, permitted_login_groups = ?,
			allowed_emails = ?, groups_separator = ?, updated_at = ?
		WHERE id = ?`,
		p.Kind, p.DisplayName, p.Enabled, p.ClientID, p.secret,
		p.Issuer, p.AuthURL, p.TokenURL, p.UserinfoURL, p.Scopes,
		p.AllowedDomains, p.DefaultRole, p.LinkByVerifiedEmail, p.AutoProvision, p.GroupsClaim,
		p.PermittedLoginGroups, p.AllowedEmails, p.GroupsSeparator,
		p.UpdatedAt, p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProviderNotFound
	}

	audit.Action = "auth_provider.update"
	audit.TargetType = "auth_provider"
	audit.TargetID = p.ID
	audit.Before = jsonStr(before)
	audit.After = jsonStr(p)
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}

	// A second row when the door opened or closed, and deliberately in
	// addition rather than instead.
	//
	// `auth_provider.update` is the diff and stays the diff; a save that
	// renamed a provider and a save that took it off the login page are the
	// same action name with the answer buried in two JSON blobs. Whether people
	// can sign in through this provider is the fact an incident is
	// reconstructed from, and it should be findable by action alone — which is
	// what local sign-in has always written for the same flag, under the same
	// name (see SetAuthProviderEnabled).
	if before.Enabled != p.Enabled {
		flip := audit
		flip.Action = "auth_provider.enabled"
		flip.TargetType = "auth_provider"
		flip.TargetID = p.ID
		flip.Before = jsonStr(map[string]any{"enabled": before.Enabled})
		flip.After = jsonStr(map[string]any{"enabled": p.Enabled})
		if err := writeAuditTx(tx, flip); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteAuthProvider removes the provider row and leaves every identity that
// used it in place.
//
// Deliberately not a cascade and deliberately not blocked by existing users.
// Deleting the row is a configuration change, not a decision about people: the
// users it provisioned keep their apps, their grants and their history, and
// re-adding a provider under the same id restores their sign-in exactly as it
// was — less the group memberships its directory wrote, which are removed
// below and come back only through mappings made again. Blocking the delete
// instead would strand an admin who mistyped an issuer, and cascading would
// quietly delete the platform's users.
func (db *DB) DeleteAuthProvider(id string, audit AuditEntry) error {
	before, err := db.GetAuthProvider(id)
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM auth_providers WHERE id = ?`, id); err != nil {
		return err
	}

	// What the provider's directory decided does go with it, though: its
	// claim mappings and the memberships it wrote are authorization, and
	// neither table carries a foreign key that would take them. Left behind,
	// a person the directory put in a group kept that group's access through
	// any other identity they hold, and a later provider re-created under this
	// id inherited the mappings (DB1-8). Removed exactly as unmapping removes
	// them — sessions dropped, one audit row per membership.
	type membership struct{ groupID, userID string }
	rows, err := tx.Query(`
		SELECT group_id, user_id FROM group_members
		WHERE source=? AND source_provider_id=? ORDER BY group_id, user_id`, sourceDirectory, id)
	if err != nil {
		return err
	}
	var removed []membership
	for rows.Next() {
		var m membership
		if err := rows.Scan(&m.groupID, &m.userID); err != nil {
			rows.Close()
			return err
		}
		removed = append(removed, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM group_members WHERE source=? AND source_provider_id=?`,
		sourceDirectory, id); err != nil {
		return err
	}
	mappings, err := tx.Exec(`DELETE FROM group_claim_mappings WHERE provider_id=?`, id)
	if err != nil {
		return err
	}
	bumped := map[string]bool{}
	for _, m := range removed {
		if !bumped[m.userID] {
			bumped[m.userID] = true
			if _, err := tx.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id=?`, m.userID); err != nil {
				return err
			}
		}
		if err := writeAuditTx(tx, AuditEntry{
			ActorType: audit.ActorType, ActorID: audit.ActorID,
			Action: "group.member_remove", TargetType: "group", TargetID: m.groupID,
			Before: jsonStr(map[string]any{
				"user_id": m.userID, "source": sourceDirectory, "provider_id": id,
			}),
			After:     jsonStr(map[string]any{"reason": "sign-in provider deleted"}),
			RequestID: audit.RequestID, IP: audit.IP, UserAgent: audit.UserAgent,
		}); err != nil {
			return err
		}
	}
	unmapped, _ := mappings.RowsAffected()

	audit.Action = "auth_provider.delete"
	audit.After = jsonStr(map[string]any{
		"claim_mappings_removed": unmapped, "memberships_removed": len(removed),
	})
	audit.TargetType = "auth_provider"
	audit.TargetID = id
	audit.Before = jsonStr(before)
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) authProviderExists(id string) (bool, error) {
	var one int
	err := db.conn.QueryRow(`SELECT 1 FROM auth_providers WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// --- Identities ---

// Identity is one external account attached to a user.
type Identity struct {
	ProviderID  string     `json:"provider_id"`
	Subject     string     `json:"subject"`
	UserID      string     `json:"user_id"`
	Email       string     `json:"email"`
	Login       string     `json:"login"`
	Name        string     `json:"name"`
	AvatarURL   string     `json:"avatar_url"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// IdentityProfile is what a provider asserted about an account at sign-in.
type IdentityProfile struct {
	Subject string
	Login   string
	// Name is refreshed on every sign-in: people change their names, and the
	// subject — not the name — is what identifies the account.
	Name          string
	Email         string
	EmailVerified bool
	AvatarURL     string
}

// SignInPolicy is the provider's provisioning policy, passed in rather than
// re-read here so the caller (which may be serving an environment-configured
// provider with no row) is the single source of it.
type SignInPolicy struct {
	// DefaultRole is the system role a newly provisioned user receives, or
	// ProvisionPending — the one value that is not a role. Pending creates the
	// account as a viewer and stamps `pending_activation_at`, which refuses
	// every sign-in it makes until an administrator activates it and chooses
	// the role. It applies only to an account this policy *creates*: linking
	// attaches an identity to an account that already exists and was already
	// let in, and re-deciding that on a second provider's first sign-in would
	// lock somebody out of an account they have been using.
	DefaultRole string
	// LinkByVerifiedEmail permits attaching an unknown identity to an existing
	// user with the same address, and only when the provider says the address
	// is verified.
	LinkByVerifiedEmail bool
	// AutoProvision permits creating a user for an identity that matched
	// nothing. Off, the sign-in fails with ErrNoAccount and writes no rows —
	// authentication succeeded, authorization to exist here did not.
	AutoProvision bool
}

// ErrNoAccount means the identity authenticated but has no account here and the
// provider does not create one. Distinct from an internal error so the callback
// can tell the person what happened — it is a policy outcome, not a failure.
var ErrNoAccount = errors.New("no account for this identity and auto-provisioning is off")

// SignInOutcome says what resolving an identity actually did, so the caller can
// audit it and the UI can say "welcome" rather than "welcome back".
type SignInOutcome string

const (
	// SignInExisting matched a known identity.
	SignInExisting SignInOutcome = "existing"
	// SignInLinked attached a new identity to an existing user by verified email.
	SignInLinked SignInOutcome = "linked"
	// SignInProvisioned created a new user.
	SignInProvisioned SignInOutcome = "provisioned"
)

// ResolveIdentity maps a provider's assertion onto a platform user, creating or
// linking as policy allows, and returns the persisted user.
//
// One transaction, because the three writes it may perform (a user, an
// identity, an audit row) are one fact: an identity pointing at a user that was
// never created is a login that can never succeed and never be diagnosed.
//
// Order of resolution:
//
//  1. Known identity → that user. The subject is the identity, so a changed
//     email or renamed handle resolves to the same person.
//  2. Unknown identity, policy permits linking, provider asserts the email is
//     verified, and exactly one user holds that address → link.
//  3. Otherwise → a new user, or ErrNoAccount when the provider does not
//     auto-provision.
//
// The profile is written back to the users row only while this is the person's
// *only* identity. With two, the canonical profile stops being any one
// provider's to overwrite, and a second sign-in must not rewrite the name and
// avatar the first one established.
func (db *DB) ResolveIdentity(providerID string, profile IdentityProfile, policy SignInPolicy, audit AuditEntry) (*User, SignInOutcome, error) {
	if profile.Subject == "" {
		return nil, "", fmt.Errorf("provider %s returned no subject", providerID)
	}
	now := time.Now().UTC()

	tx, err := db.conn.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()

	outcome := SignInExisting
	var userID string
	err = tx.QueryRow(`SELECT user_id FROM user_identities WHERE provider_id = ? AND subject = ?`,
		providerID, profile.Subject).Scan(&userID)
	switch {
	case err == nil:
		// Known identity.
	case errors.Is(err, sql.ErrNoRows):
		userID, outcome, err = provisionOrLink(tx, providerID, profile, policy, now)
		if err != nil {
			return nil, "", err
		}
	default:
		return nil, "", err
	}

	if _, err := tx.Exec(`
		INSERT INTO user_identities (provider_id, subject, user_id, email, email_verified, login, name, avatar_url, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider_id, subject) DO UPDATE SET
			email = excluded.email, email_verified = excluded.email_verified,
			login = excluded.login, name = excluded.name,
			avatar_url = excluded.avatar_url, last_login_at = excluded.last_login_at`,
		providerID, profile.Subject, userID, profile.Email, profile.EmailVerified, profile.Login, profile.Name,
		profile.AvatarURL, now, now); err != nil {
		return nil, "", err
	}

	// Refresh the canonical profile only for a single-identity user — see the
	// doc comment. Counted after the upsert above, so a first-ever sign-in
	// counts itself.
	var identities int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM user_identities WHERE user_id = ?`, userID).
		Scan(&identities); err != nil {
		return nil, "", err
	}
	if identities == 1 {
		// system_role is deliberately absent: the provider says who you are,
		// Toft says what you may do, and a sign-in must never re-decide
		// a role an administrator set.
		if _, err := tx.Exec(`UPDATE users SET login = ?, name = ?, email = ?, avatar_url = ? WHERE id = ?`,
			profile.Login, profile.Name, profile.Email, profile.AvatarURL, userID); err != nil {
			return nil, "", err
		}
	}

	if outcome != SignInExisting {
		// A provisioning or linking event changes who can act as this user, so
		// it is audited like any other authorization-relevant mutation, in the
		// same transaction. A routine sign-in is not: it would bury the audit
		// log in noise, and the platform is not a session log.
		audit.ActorType = PrincipalTypeUser
		audit.ActorID = userID
		audit.Action = "identity." + string(outcome)
		audit.TargetType = "user"
		audit.TargetID = userID
		audit.After = jsonStr(map[string]string{
			"provider": providerID, "subject": profile.Subject, "email": profile.Email,
		})
		if err := writeAuditTx(tx, audit); err != nil {
			return nil, "", err
		}
	}

	user, err := scanUserTx(tx, userID)
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	return user, outcome, nil
}

func provisionOrLink(tx *Tx, providerID string, profile IdentityProfile, policy SignInPolicy, now time.Time) (string, SignInOutcome, error) {
	// Linking is the narrow case and every condition is load-bearing: the
	// policy must permit it, the *provider* must assert the address is
	// verified (an unverified address is a claim, not a credential), and the
	// address must identify exactly one user — two matches is ambiguous and
	// resolving it by picking one would attach the identity to the wrong
	// person.
	if policy.LinkByVerifiedEmail && profile.EmailVerified && profile.Email != "" {
		// The existing side must have proved the address as well: a user whose
		// address came only from an unverified assertion is not a link target.
		// A user with no identity at all was created by an administrator or an
		// import, whose word for the address is not a provider's assertion, and
		// stays linkable. A `local` identity never counts as the proving side:
		// no provider verified it, and migration 00053 marked every identity
		// that existed before it verified — the built-in administrator's among
		// them — so on an upgraded install it would read as proof (DB1-6).
		rows, err := tx.Query(`
			SELECT u.id FROM users u
			WHERE LOWER(u.email) = LOWER(?)
			  AND (NOT EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = u.id)
			       OR EXISTS (SELECT 1 FROM user_identities i
			                  WHERE i.user_id = u.id AND i.email_verified AND i.provider_id <> ?
			                    AND LOWER(i.email) = LOWER(?)))
			LIMIT 2`, profile.Email, LocalProviderID, profile.Email)
		if err != nil {
			return "", "", err
		}
		var matches []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return "", "", err
			}
			matches = append(matches, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", "", err
		}
		if len(matches) == 1 {
			return matches[0], SignInLinked, nil
		}
	}

	// Nothing to attach to, and this provider does not create accounts. The
	// transaction rolls back, so a refused sign-in leaves no user row, no
	// identity row and no half-account for an admin to clean up later.
	//
	// Honored even on an install with no users at all: a switch that quietly
	// suspends itself in some state is worse than one that means what it says,
	// and the default is on, so reaching this on an empty install takes a
	// deliberate act.
	if !policy.AutoProvision {
		return "", "", ErrNoAccount
	}

	role := policy.DefaultRole
	if role == "" {
		role = SystemRoleViewer
	}
	// `pending` is the provider's answer to "what may a new account do", not a
	// role — so it never reaches system_role. The account is created as a
	// viewer, which is the floor, and stamped as unactivated; every sign-in it
	// makes is refused until an administrator activates it and says which role
	// it actually holds. Doing it this way rather than storing "pending" in
	// system_role is what keeps every reader of that column — the JWT, the
	// authorizer, the ladder in internal/auth — free of a value meaning "no
	// role at all".
	var pendingAt any
	if role == ProvisionPending {
		role = SystemRoleViewer
		pendingAt = now
	}
	// No promotion here, deliberately. The first user in an empty install used to
	// become system_admin, on the reasoning that someone has to be able to reach
	// the admin surface. But "first" means whoever loads the URL first, which
	// against a corporate IdP is a race between everybody the IdP authenticates,
	// and it left an operator no way to configure SSO before the race started.
	// The built-in local administrator (migration 21) is the bootstrap now, so a
	// provider sign-in only ever grants the provider's default role.

	// A platform user id is opaque and internally generated. It used to be the
	// GitHub numeric id, which made the provider's namespace the platform's
	// primary key — two providers would eventually hand out the same one.
	userID := uuid.New().String()
	if _, err := tx.Exec(`
		INSERT INTO users (id, login, name, email, avatar_url, system_role, created_at, pending_activation_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, profile.Login, profile.Name, profile.Email, profile.AvatarURL, role, now, pendingAt); err != nil {
		return "", "", err
	}
	return userID, SignInProvisioned, nil
}

func scanUserTx(tx *Tx, id string) (*User, error) {
	u := &User{}
	err := tx.QueryRow(
		`SELECT id, login, name, email, avatar_url, system_role, locked_at, created_at, pending_activation_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Login, &u.Name, &u.Email, &u.AvatarURL, &u.SystemRole, &u.LockedAt, &u.CreatedAt, &u.PendingActivationAt)
	return u, err
}

// ListUserIdentities returns the external accounts attached to one user, for
// the account page and for an admin asking which logins reach it.
func (db *DB) ListUserIdentities(userID string) ([]Identity, error) {
	rows, err := db.conn.Query(`
		SELECT provider_id, subject, user_id, email, login, name, avatar_url, created_at, last_login_at
		FROM user_identities WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Identity{}
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.ProviderID, &i.Subject, &i.UserID, &i.Email, &i.Login, &i.Name,
			&i.AvatarURL, &i.CreatedAt, &i.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// nullIfEmpty maps Go's "" onto the SQL NULL that a nullable column uses for
// "not set".
//
// It matters most for the authorship columns — created_by, granted_by,
// added_by, attached_by, enabled_by, updated_by, actor_id, requester_id,
// approver_id — which are foreign keys to users(id) with ON DELETE SET NULL.
// NULL is how they spell "no user": deleted, or the system, or never set. Go
// has no nullable string, so every one of them arrives here as "" when there is
// nobody to name, and writing that "" straight through fails the foreign key,
// because the empty string is not anybody's id.
//
// The read direction is COALESCE(col, ”) in the query, so scan targets stay
// plain strings and nothing above this package has to know.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- The SCIM feed's credential ---

// ErrNoSCIMToken is every provider that has never had one, which is every
// provider on an install that has not configured a feed.
var ErrNoSCIMToken = errors.New("no SCIM token is configured for any provider")

// SCIMProviderByTokenHash resolves the provider a SCIM token authenticates.
//
// **Presence of a hash is the switch**: an install with no token has no feed,
// and a lookup that matches nothing is the only refusal this needs. The token
// names its provider, which is what lets every write it causes say which
// directory asserted it — and what makes provider isolation structural rather
// than checked, since a token for one directory can only ever resolve rows
// scoped to that directory.
//
// Empty hashes never match: the column defaults to ” on every existing row,
// and a caller presenting an empty bearer must not authenticate as the first
// provider that has not configured one.
func (db *DB) SCIMProviderByTokenHash(hash string) (*AuthProvider, error) {
	if hash == "" {
		return nil, ErrNoSCIMToken
	}
	p, err := scanAuthProvider(db.conn.QueryRow(
		`SELECT `+authProviderColumns+` FROM auth_providers WHERE scim_token_hash = ? AND scim_token_hash <> ''`,
		hash).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSCIMToken
	}
	return p, err
}

// SetSCIMToken stores a token's hash, or clears it when hash is empty.
//
// Rotation is minting again: the previous hash is overwritten in the same
// statement, so the old token stops working the moment the new one exists.
// There is no window in which both are valid, which is the right trade for a
// credential that can lock every account on the install — an administrator
// pasting a new token into an IdP console is a smaller inconvenience than two
// live secrets nobody is tracking.
func (db *DB) SetSCIMToken(ctx context.Context, providerID, hash string, audit AuditEntry) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var setAt any
	action := "auth_provider.scim_token_revoke"
	if hash != "" {
		setAt = time.Now().UTC()
		action = "auth_provider.scim_token_set"
	}
	res, err := tx.Exec(
		`UPDATE auth_providers SET scim_token_hash = ?, scim_token_set_at = ?, updated_at = NOW() WHERE id = ?`,
		hash, setAt, providerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProviderNotFound
	}

	audit.Action = action
	audit.TargetType = "auth_provider"
	audit.TargetID = providerID
	// The token itself never reaches the audit log, in any form. What is
	// recorded is that one now exists, which is the fact somebody reviewing
	// access needs.
	audit.After = jsonStr(map[string]bool{"scim_token_set": hash != ""})
	if err := writeAuditTx(tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

// TouchSCIMCall records that a feed called, throttled to once a minute.
//
// It is what a system check reads to say "a token is configured and has never
// been used", which is the shape of every SCIM misconfiguration and is
// invisible from this side without it. Throttled because an IdP syncing a large
// directory calls constantly and this must not become a write per request —
// the same bargain `touchUserAPIKey` already makes.
func (db *DB) TouchSCIMCall(providerID string) error {
	_, err := db.conn.Exec(
		`UPDATE auth_providers SET scim_last_call_at = NOW()
		 WHERE id = ? AND (scim_last_call_at IS NULL OR scim_last_call_at < NOW() - INTERVAL '1 minute')`,
		providerID)
	return err
}

// --- Resolving a directory's view of a person ---

// SCIMUser is an account as one directory sees it: the platform's user row, plus
// the identity that directory holds on it.
//
// Two rows rather than one because the identity is per provider. The same person
// signs in through two directories with two logins, and a feed authenticated as
// one of them must see its own.
type SCIMUser struct {
	User       *User
	Login      string
	ExternalID string
}

// UserByProviderLogin resolves a directory's `userName` to an account.
//
// **The join is `user_identities(provider_id, login)`, not `users.login`.** The
// second is one canonical value chosen by whichever provider provisioned the
// account first, through a five-way fallback in `authprovider.oidcProfile` —
// on a two-provider install it can be the *other* directory's spelling of a
// name. The identity row is what that directory actually asserted.
//
// Narrowing on provider_id is also what makes provider isolation structural: a
// token issued to one directory cannot resolve a person only another one knows,
// and nothing separate has to check for it.
//
// Returns (nil, nil) for no match, which is a filter finding nothing rather
// than an error.
//
// A login is whatever the IdP asserted and is not unique per provider, so two
// accounts answering one userName is refused with ErrAmbiguousSCIMLogin rather
// than resolved to whichever row came first: the deactivation that follows
// would land on a bystander and leave the person it meant with access (DB1-7).
func (db *DB) UserByProviderLogin(providerID, login string) (*SCIMUser, error) {
	var n int
	if err := db.conn.QueryRow(`
		SELECT COUNT(DISTINCT user_id) FROM user_identities WHERE provider_id = ? AND LOWER(login) = LOWER(?)`,
		providerID, login).Scan(&n); err != nil {
		return nil, err
	}
	if n > 1 {
		return nil, ErrAmbiguousSCIMLogin
	}
	return db.scimUserWhere(`i.provider_id = ? AND LOWER(i.login) = LOWER(?)`, providerID, login)
}

// ErrAmbiguousSCIMLogin is a directory's userName that more than one account
// holds under that directory.
var ErrAmbiguousSCIMLogin = errors.New("more than one account holds this userName under this directory")

// UserByProviderAndID resolves a `/Users/{id}` read, scoped to the directory
// that asked. An id belonging to somebody that directory has no identity for is
// not found — the same isolation the login lookup gets, by the same clause.
func (db *DB) UserByProviderAndID(providerID, userID string) (*SCIMUser, error) {
	return db.scimUserWhere(`i.provider_id = ? AND i.user_id = ?`, providerID, userID)
}

func (db *DB) scimUserWhere(where string, args ...any) (*SCIMUser, error) {
	out := &SCIMUser{User: &User{}}
	err := db.conn.QueryRow(`
		SELECT u.id, u.login, u.name, u.email, u.avatar_url, u.system_role, u.locked_at, u.created_at,
		       i.login, i.external_id
		FROM users u JOIN user_identities i ON i.user_id = u.id
		WHERE `+where+` LIMIT 1`, args...).
		Scan(&out.User.ID, &out.User.Login, &out.User.Name, &out.User.Email, &out.User.AvatarURL,
			&out.User.SystemRole, &out.User.LockedAt, &out.User.CreatedAt,
			&out.Login, &out.ExternalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordExternalID stores the directory's own identifier for a person, once.
//
// Learned rather than required: it is blank on every account that exists today
// and can only arrive on a call that already matched on a login. Written only
// when it changes, so a feed syncing constantly does not write constantly.
func (db *DB) RecordExternalID(providerID, userID, externalID string) error {
	if externalID == "" {
		return nil
	}
	_, err := db.conn.Exec(
		`UPDATE user_identities SET external_id = ?
		 WHERE provider_id = ? AND user_id = ? AND external_id <> ?`,
		externalID, providerID, userID, externalID)
	return err
}
