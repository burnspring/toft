package database_test

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func mkProvider(t *testing.T, db *database.DB, id string, policy func(*database.AuthProvider)) *database.AuthProvider {
	t.Helper()
	p := &database.AuthProvider{
		ID: id, Kind: database.ProviderKindOIDC, DisplayName: strings.ToUpper(id),
		Enabled: true, ClientID: "client-id", Issuer: "https://" + id + ".example.com",
		DefaultRole: database.SystemRoleViewer, AutoProvision: true,
	}
	p.SetEncryptedSecret("ciphertext-" + id)
	if policy != nil {
		policy(p)
	}
	if err := db.CreateAuthProvider(p, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin-1",
	}); err != nil {
		t.Fatalf("CreateAuthProvider: %v", err)
	}
	return p
}

func TestCreateAndListAuthProviders(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)

	providers, err := db.ListAuthProviders()
	if err != nil {
		t.Fatalf("ListAuthProviders: %v", err)
	}
	// Two: the one just created, and the built-in `local` password provider every
	// install has (migration 21).
	if len(providers) != 2 {
		t.Fatalf("got %d providers, want the new one plus the built-in local provider", len(providers))
	}
	var got *database.AuthProvider
	for i := range providers {
		if providers[i].ID == "okta" {
			got = &providers[i]
		}
	}
	if got == nil {
		t.Fatalf("okta missing from %+v", providers)
	}
	if !got.Enabled || got.UserCount != 0 {
		t.Fatalf("unexpected row: %+v", got)
	}
	if !got.SecretSet || got.EncryptedSecret() != "ciphertext-okta" {
		t.Fatalf("secret not round-tripped: set=%v", got.SecretSet)
	}

	// A duplicate id is a 409 at the API, so it must be distinguishable here.
	dup := &database.AuthProvider{
		ID: "okta", Kind: database.ProviderKindOIDC, DisplayName: "Okta again",
		ClientID: "c", Issuer: "https://other.example.com", DefaultRole: database.SystemRoleViewer,
	}
	if err := db.CreateAuthProvider(dup, database.AuditEntry{}); err != database.ErrProviderExists {
		t.Fatalf("duplicate create = %v, want ErrProviderExists", err)
	}
}

// The form never receives the stored secret, so it cannot echo one back — an
// empty secret on update has to mean "keep it" or every unrelated edit would
// silently break sign-in.
func TestUpdateWithNoSecretKeepsTheStoredOne(t *testing.T) {
	db := dbtest.New(t)
	p := mkProvider(t, db, "okta", nil)

	edit := *p
	edit.DisplayName = "Okta (renamed)"
	edit.SetEncryptedSecret("")
	if err := db.UpdateAuthProvider(&edit, database.AuditEntry{ActorID: "admin-1"}); err != nil {
		t.Fatalf("UpdateAuthProvider: %v", err)
	}

	after, err := db.GetAuthProvider("okta")
	if err != nil {
		t.Fatalf("GetAuthProvider: %v", err)
	}
	if after.DisplayName != "Okta (renamed)" {
		t.Errorf("DisplayName = %q", after.DisplayName)
	}
	if after.EncryptedSecret() != "ciphertext-okta" {
		t.Fatalf("secret was cleared by an edit that did not mention it")
	}

	// A supplied secret does replace it.
	edit.SetEncryptedSecret("ciphertext-rotated")
	if err := db.UpdateAuthProvider(&edit, database.AuditEntry{ActorID: "admin-1"}); err != nil {
		t.Fatalf("UpdateAuthProvider (rotate): %v", err)
	}
	after, _ = db.GetAuthProvider("okta")
	if after.EncryptedSecret() != "ciphertext-rotated" {
		t.Fatalf("secret = %q, want the rotated value", after.EncryptedSecret())
	}
}

// Deleting a provider is a configuration change, not a decision about people:
// the users it provisioned keep their accounts, and re-adding it under the same
// id restores their sign-in.
func TestDeleteAuthProviderKeepsItsUsers(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)

	user, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{DefaultRole: database.SystemRoleViewer, AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}

	if err := db.DeleteAuthProvider("okta", database.AuditEntry{ActorID: "admin-1"}); err != nil {
		t.Fatalf("DeleteAuthProvider: %v", err)
	}
	if _, err := db.GetAuthProvider("okta"); err != database.ErrProviderNotFound {
		t.Fatalf("GetAuthProvider after delete = %v, want ErrProviderNotFound", err)
	}
	if u, err := db.GetUser(user.ID); err != nil || u == nil {
		t.Fatalf("the provider's user was deleted with it (err=%v)", err)
	}

	// Re-adding it resolves the same person rather than minting a second
	// account.
	mkProvider(t, db, "okta", nil)
	again, outcome, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if again.ID != user.ID || outcome != database.SignInExisting {
		t.Fatalf("re-added provider produced %s (%s), want the original user %s", again.ID, outcome, user.ID)
	}
}

func TestResolveIdentityProvisionsThenReuses(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)

	first, outcome, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{DefaultRole: database.SystemRolePublisher, AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if outcome != database.SignInProvisioned {
		t.Fatalf("outcome = %s, want provisioned", outcome)
	}
	// The provider's default role, and only that. Signing in first used to make
	// someone system_admin, which on a corporate IdP is a race between everybody
	// the IdP authenticates — the built-in local administrator is the bootstrap
	// now, so provisioning grants exactly what the provider says it grants.
	if first.SystemRole != database.SystemRolePublisher {
		t.Fatalf("provisioned role = %s, want the provider's default (publisher)", first.SystemRole)
	}

	// The subject is the identity: a renamed handle and a changed address still
	// resolve to the same person.
	second, outcome, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jane-doe", Email: "jane.doe@acme.com",
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if second.ID != first.ID || outcome != database.SignInExisting {
		t.Fatalf("second sign-in produced %s (%s), want %s existing", second.ID, outcome, first.ID)
	}
	if second.Login != "jane-doe" || second.Email != "jane.doe@acme.com" {
		t.Errorf("profile not refreshed for a single-identity user: %+v", second)
	}

	// A second account under the same provider is a different subject.
	other, outcome, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-2", Login: "asmith", Email: "asmith@acme.com",
	}, database.SignInPolicy{DefaultRole: database.SystemRolePublisher, AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if other.ID == first.ID || outcome != database.SignInProvisioned {
		t.Fatal("a different subject must be a different user")
	}
	// Only the *first* account is promoted; the provider's policy applies after.
	if other.SystemRole != database.SystemRolePublisher {
		t.Fatalf("role = %s, want the provider's default (publisher)", other.SystemRole)
	}
}

// The same address at two providers is two accounts unless linking is turned on
// deliberately — and even then only for an address the provider says it verified.
func TestResolveIdentityLinking(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)
	mkProvider(t, db, "google", nil)

	original, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}

	// Linking off: a separate account, which is the safe default.
	separate, outcome, err := db.ResolveIdentity("google", database.IdentityProfile{
		Subject: "g-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{LinkByVerifiedEmail: false, AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if separate.ID == original.ID || outcome != database.SignInProvisioned {
		t.Fatal("linking is off, so the second provider must not attach to the existing account")
	}

	// Unverified, with linking on: still refused. An unverified address is a
	// claim, not a credential.
	db2 := dbtest.New(t)
	mkProvider(t, db2, "okta", nil)
	mkProvider(t, db2, "google", nil)
	orig2, _, _ := db2.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	unverified, outcome, err := db2.ResolveIdentity("google", database.IdentityProfile{
		Subject: "g-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: false,
	}, database.SignInPolicy{LinkByVerifiedEmail: true, AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if unverified.ID == orig2.ID || outcome != database.SignInProvisioned {
		t.Fatal("an unverified email must not be able to claim an existing account")
	}

	// Verified, with linking on: attached, and the canonical profile is left
	// alone now that the user has two identities.
	db3 := dbtest.New(t)
	mkProvider(t, db3, "okta", nil)
	mkProvider(t, db3, "google", nil)
	orig3, _, _ := db3.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	linked, outcome, err := db3.ResolveIdentity("google", database.IdentityProfile{
		Subject: "g-1", Login: "google-handle", Email: "JDoe@Acme.com", EmailVerified: true,
	}, database.SignInPolicy{LinkByVerifiedEmail: true, AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if linked.ID != orig3.ID || outcome != database.SignInLinked {
		t.Fatalf("verified linking produced %s (%s), want %s linked", linked.ID, outcome, orig3.ID)
	}
	if linked.Login != "jdoe" {
		t.Errorf("Login = %q — a second provider must not rewrite the profile the first established", linked.Login)
	}

	identities, err := db3.ListUserIdentities(orig3.ID)
	if err != nil {
		t.Fatalf("ListUserIdentities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("got %d identities, want 2", len(identities))
	}
	for _, i := range identities {
		if i.LastLoginAt == nil {
			t.Errorf("identity %s/%s has no last_login_at", i.ProviderID, i.Subject)
		}
	}
}

// Provisioning and linking change who can act as a user, so they are audited
// like any other authorization-relevant mutation. A routine sign-in is not.
func TestIdentityEventsAreAudited(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)
	before, _ := db.CountAuditEntries()

	profile := database.IdentityProfile{Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com"}
	if _, _, err := db.ResolveIdentity("okta", profile, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{}); err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	afterProvision, _ := db.CountAuditEntries()
	if afterProvision != before+1 {
		t.Fatalf("audit rows went %d → %d, want one entry for the provisioning", before, afterProvision)
	}

	if _, _, err := db.ResolveIdentity("okta", profile, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{}); err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	afterLogin, _ := db.CountAuditEntries()
	if afterLogin != afterProvision {
		t.Fatalf("a routine sign-in wrote %d audit rows — the audit log is not a session log",
			afterLogin-afterProvision)
	}
}

// Auto-provisioning off means the provider authenticates people who already
// have an account and nobody else: no user row, no identity row, and an error
// the callback can turn into an explanation.
func TestResolveIdentityWithoutAutoProvisioning(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", func(p *database.AuthProvider) { p.AutoProvision = false })

	_, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{DefaultRole: database.SystemRoleViewer}, database.AuditEntry{})
	if err != database.ErrNoAccount {
		t.Fatalf("ResolveIdentity = %v, want ErrNoAccount", err)
	}

	// The refusal must leave nothing behind — a half-created account is worse
	// than a refused one, because nobody knows to clean it up. The built-in
	// administrator and its identity are excluded: they predate the sign-in.
	for _, q := range []string{
		`SELECT COUNT(*) FROM users WHERE id <> $1`,
		`SELECT COUNT(*) FROM user_identities WHERE user_id <> $1`,
	} {
		var n int
		if err := db.Conn().QueryRow(q, database.BuiltinAdminID).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}

	// An account that already exists still signs in: the switch governs
	// creation, not admission.
	if _, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{}); err != nil {
		t.Fatalf("provisioning the account first: %v", err)
	}
	user, outcome, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{}, database.AuditEntry{})
	if err != nil || outcome != database.SignInExisting || user == nil {
		t.Fatalf("known identity with provisioning off: user=%v outcome=%s err=%v", user, outcome, err)
	}
}

// Linking is not provisioning: the account already exists and somebody created
// it deliberately, so attaching a second identity to it is still allowed.
func TestLinkingWorksWithoutAutoProvisioning(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)
	mkProvider(t, db, "google", func(p *database.AuthProvider) { p.AutoProvision = false })

	original, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}

	linked, outcome, err := db.ResolveIdentity("google", database.IdentityProfile{
		Subject: "g-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{LinkByVerifiedEmail: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if outcome != database.SignInLinked || linked.ID != original.ID {
		t.Fatalf("outcome = %s onto %s, want linked onto %s", outcome, linked.ID, original.ID)
	}
}

// A provider that returns no subject has told us nothing about who signed in.
func TestResolveIdentityRefusesAnEmptySubject(t *testing.T) {
	db := dbtest.New(t)
	if _, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{}); err == nil {
		t.Fatal("ResolveIdentity accepted a profile with no subject")
	}
}

// The count an administrator reads beside a provider has to work for a provider
// that is not a row.
//
// It used to be a subquery on `auth_providers`, which can only answer for a
// provider that exists there — and an environment-configured one does not. It is
// merged into the admin list from the registry, so the page looked its count up
// in a map built from rows and got the zero value: "0 accounts" beside the only
// provider anybody was signing in through, on this box including the
// administrator reading it.
func TestIdentitiesAreCountedForAProviderWithNoRow(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)

	for _, sub := range []string{"sub-1", "sub-2"} {
		if _, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
			Subject: sub, Login: sub, Email: sub + "@acme.com",
		}, database.SignInPolicy{DefaultRole: database.SystemRoleViewer, AutoProvision: true},
			database.AuditEntry{}); err != nil {
			t.Fatalf("ResolveIdentity(okta, %s): %v", sub, err)
		}
	}
	// The environment-configured case: an identity whose provider id has no row
	// in auth_providers at all. `dex` is exactly this shape on a box configured
	// with OIDC_ISSUER.
	if _, _, err := db.ResolveIdentity("dex", database.IdentityProfile{
		Subject: "sub-3", Login: "sub-3", Email: "sub-3@acme.com",
	}, database.SignInPolicy{DefaultRole: database.SystemRoleViewer, AutoProvision: true},
		database.AuditEntry{}); err != nil {
		t.Fatalf("ResolveIdentity(dex): %v", err)
	}

	counts, err := db.CountIdentitiesByProvider()
	if err != nil {
		t.Fatalf("CountIdentitiesByProvider: %v", err)
	}
	if counts["okta"] != 2 {
		t.Fatalf("okta = %d, want 2", counts["okta"])
	}
	if counts["dex"] != 1 {
		t.Fatalf("dex = %d, want 1 — a provider configured in the environment has "+
			"no auth_providers row, and the count may not depend on one", counts["dex"])
	}
}

// A provider whose policy is `pending` creates the account and does not let it
// in. Three properties, and each one is a way this could go wrong: the account
// exists (or there is no queue), it holds the floor role rather than the word
// "pending" (or every reader of system_role has to learn a fourth value), and
// it is stamped as unactivated (or it is simply a viewer who can sign in).
func TestPendingPolicyProvisionsAnUnactivatedAccount(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)

	user, outcome, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{DefaultRole: database.ProvisionPending, AutoProvision: true},
		database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if outcome != database.SignInProvisioned {
		t.Fatalf("outcome = %s, want provisioned — the row has to exist for an administrator to act on", outcome)
	}
	if !user.PendingActivation() {
		t.Fatal("the account was created without pending_activation_at, so it could sign straight in")
	}
	if user.SystemRole != database.SystemRoleViewer {
		t.Fatalf("system_role = %q, want viewer — `pending` is a provisioning policy and never a role",
			user.SystemRole)
	}

	// And it stays pending across sign-ins. There is no "already told them"
	// state, so the second attempt has to be refused by the same fact as the
	// first.
	again, outcome, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com",
	}, database.SignInPolicy{DefaultRole: database.ProvisionPending, AutoProvision: true},
		database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if outcome != database.SignInExisting || !again.PendingActivation() {
		t.Fatalf("second sign-in: outcome %s, pending %v — want existing and still pending",
			outcome, again.PendingActivation())
	}

	// Read back through the ordinary path too: the queue and the account record
	// both go through GetUser, and a column the scan forgot would make every
	// one of them say the account is fine.
	fetched, err := db.GetUser(user.ID)
	if err != nil || fetched == nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !fetched.PendingActivation() {
		t.Error("GetUser does not carry pending_activation_at")
	}
}

// Linking is not provisioning. An account that already exists was already let
// in, and a second provider's first sign-in must not put it back in the queue —
// that would lock somebody out of an account they have been using for a year.
func TestPendingPolicyDoesNotReachALinkedAccount(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)
	mkProvider(t, db, "google", nil)

	original, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
		Subject: "sub-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if original.PendingActivation() {
		t.Fatal("an ordinary policy must not stamp an account as pending")
	}

	linked, outcome, err := db.ResolveIdentity("google", database.IdentityProfile{
		Subject: "g-1", Login: "jdoe", Email: "jdoe@acme.com", EmailVerified: true,
	}, database.SignInPolicy{
		DefaultRole: database.ProvisionPending, LinkByVerifiedEmail: true, AutoProvision: true,
	}, database.AuditEntry{})
	if err != nil {
		t.Fatalf("ResolveIdentity: %v", err)
	}
	if outcome != database.SignInLinked || linked.ID != original.ID {
		t.Fatalf("outcome %s for %s, want linked to %s", outcome, linked.ID, original.ID)
	}
	if linked.PendingActivation() {
		t.Error("linking put an existing, working account into the activation queue")
	}
}

// An unverified address from one provider must not become the account a
// verified sign-in at another is linked to (account pre-hijack).
func TestAnUnverifiedAddressIsNotALinkTarget(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "weak", nil)
	mkProvider(t, db, "strong", nil)
	attacker, _, err := db.ResolveIdentity("weak", database.IdentityProfile{
		Subject: "attacker", Login: "mallory", Email: "victim@corp.example", EmailVerified: false,
	}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatal(err)
	}
	victim, outcome, err := db.ResolveIdentity("strong", database.IdentityProfile{
		Subject: "victim", Login: "victim", Email: "victim@corp.example", EmailVerified: true,
	}, database.SignInPolicy{LinkByVerifiedEmail: true, AutoProvision: true}, database.AuditEntry{})
	if err != nil {
		t.Fatal(err)
	}
	if victim.ID == attacker.ID || outcome == database.SignInLinked {
		t.Fatalf("the victim was linked onto the account seeded from an unverified assertion (%s)", outcome)
	}
}

// DB1-6: migration 00053 marked every identity that existed before it
// verified, the built-in administrator's local one among them, so on an
// upgraded install a provider asserting that address as verified linked onto
// the administrator. A local identity is never the proving side.
func TestALocalIdentityIsNeverALinkTarget(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "google", nil)
	if _, err := db.Conn().Exec(`UPDATE user_identities SET email_verified = true, email = 'admin@toft.local'
		WHERE provider_id = 'local' AND user_id = 'builtin-admin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn().Exec(`UPDATE users SET email = 'admin@toft.local' WHERE id = 'builtin-admin'`); err != nil {
		t.Fatal(err)
	}
	user, outcome, err := db.ResolveIdentity("google", database.IdentityProfile{
		Subject: "g-admin", Login: "admin", Email: "admin@toft.local", EmailVerified: true,
	}, database.SignInPolicy{LinkByVerifiedEmail: true, AutoProvision: true}, database.AuditEntry{})
	if err == nil && user != nil && user.ID == "builtin-admin" {
		t.Fatalf("a provider's verified address linked onto the built-in administrator (%s)", outcome)
	}
}

// DB1-7: a login is not unique per provider; two accounts holding one under
// the same directory is refused rather than resolved to an arbitrary one.
func TestAnAmbiguousSCIMLoginIsRefused(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "okta", nil)
	for _, sub := range []string{"sub-a", "sub-b"} {
		if _, _, err := db.ResolveIdentity("okta", database.IdentityProfile{
			Subject: sub, Login: "jdoe-" + sub, Email: sub + "@acme.com",
		}, database.SignInPolicy{AutoProvision: true}, database.AuditEntry{}); err != nil {
			t.Fatal(err)
		}
	}
	if u, err := db.UserByProviderLogin("okta", "jdoe-sub-a"); err != nil || u == nil {
		t.Fatalf("a unique login: %v, %v", u, err)
	}
	// RFC 7643 makes userName case-insensitive (security review 2026-11-14,
	// SI-11).
	if u, err := db.UserByProviderLogin("okta", "JDoe-Sub-A"); err != nil || u == nil {
		t.Fatalf("a unique login in another case: %v, %v", u, err)
	}
	// And the ambiguity rule holds over the case-folded match.
	if _, err := db.Conn().Exec(`UPDATE user_identities SET login = CASE WHEN login = 'jdoe-sub-a' THEN 'JDoe' ELSE 'jdoe' END
		WHERE provider_id = 'okta'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserByProviderLogin("okta", "jdoe"); err != database.ErrAmbiguousSCIMLogin {
		t.Fatalf("two accounts under one login in two cases = %v, want ErrAmbiguousSCIMLogin", err)
	}
	if _, err := db.Conn().Exec(`UPDATE user_identities SET login = 'jdoe' WHERE provider_id = 'okta'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserByProviderLogin("okta", "jdoe"); err != database.ErrAmbiguousSCIMLogin {
		t.Fatalf("two accounts under one login = %v, want ErrAmbiguousSCIMLogin", err)
	}
}
