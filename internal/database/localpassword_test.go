package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/password"
)

// The built-in administrator, at the storage layer.
//
// The property under test throughout is the one the platform promises: an install
// always has one way in. Migration 21 establishes it; these are the ways it could
// otherwise be lost.

// A fresh install must not ship a credential the internet already knows.
//
// must_change stops the first password minting a session, but it never stopped
// the account being *claimed*: whoever reaches the login page first replaces the
// password and owns the administrator. On a published hostname that is a race
// with strangers, so the password an install starts with is now generated.
func TestFreshInstallGeneratesItsOwnAdminPassword(t *testing.T) {
	db := dbtest.New(t)

	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatalf("GetPasswordCredential = %v, %v", cred, err)
	}
	if password.Verify(cred.Hash, password.Default) {
		t.Error("a fresh install still ships the published default password")
	}
	if !cred.MustChange {
		t.Error("the generated password does not require a change")
	}

	def, err := db.BuiltinAdminHasDefaultPassword()
	if err != nil {
		t.Fatal(err)
	}
	if def {
		t.Error("BuiltinAdminHasDefaultPassword is true on an install that generated one")
	}
}

// An install whose deployment has somewhere safe to keep a password supplies
// one — the Helm chart generates it into the release Secret — and must be able
// to sign in with exactly that.
func TestSuppliedBootstrapPasswordIsUsed(t *testing.T) {
	const supplied = "correct-horse-battery-staple"
	db := dbtest.New(t, database.WithBootstrapAdminPassword(supplied))

	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatalf("GetPasswordCredential = %v, %v", cred, err)
	}
	if !password.Verify(cred.Hash, supplied) {
		t.Error("the supplied bootstrap password does not verify")
	}
	if !cred.MustChange {
		t.Error("a supplied bootstrap password must still be replaced at first sign-in")
	}
}

// The safety property that makes the environment variable acceptable at all.
//
// A value left in the environment that reset the password on every restart
// would be a lockout for whoever changed it and a standing backdoor for whoever
// still knew it. Bootstrap runs on every boot, so this is the case that has to
// hold rather than the exception.
func TestBootstrapNeverResetsAnExistingPassword(t *testing.T) {
	db := dbtest.New(t, database.WithBootstrapAdminPassword("the-first-one"))

	chosen := "a-password-an-admin-chose"
	hash, err := password.Hash(chosen)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetPassword(database.BuiltinAdminID, hash, false,
		database.AuditEntry{ActorType: database.PrincipalTypeOperator, ActorID: "test"}); err != nil {
		t.Fatal(err)
	}

	// A second boot, with the bootstrap value still in the environment.
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}

	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatalf("GetPasswordCredential = %v, %v", cred, err)
	}
	if !password.Verify(cred.Hash, chosen) {
		t.Error("a reboot overwrote the administrator's chosen password with the bootstrap value")
	}
	if cred.MustChange {
		t.Error("a reboot re-armed must_change on a password that had been deliberately set")
	}
}

func TestBuiltinAdminExistsOnAFreshDatabase(t *testing.T) {
	db := dbtest.New(t)

	user, err := db.GetUser(database.BuiltinAdminID)
	if err != nil || user == nil {
		t.Fatalf("GetUser = %v, %v", user, err)
	}
	if user.Login != database.BuiltinAdminLogin || user.SystemRole != database.SystemRoleAdmin {
		t.Errorf("account = %s/%s, want %s/system_admin", user.Login, user.SystemRole, database.BuiltinAdminLogin)
	}

	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatalf("GetPasswordCredential = %v, %v", cred, err)
	}
	if !cred.MustChange {
		t.Error("the initial password does not require a change — a published default that mints sessions")
	}

	// The identity row is what makes the account page able to say how this user
	// signs in, and what makes the provider's user count right.
	identities, err := db.ListUserIdentities(database.BuiltinAdminID)
	if err != nil {
		t.Fatalf("ListUserIdentities: %v", err)
	}
	if len(identities) != 1 || identities[0].ProviderID != database.LocalProviderID {
		t.Errorf("identities = %+v, want one on the local provider", identities)
	}
}

// A dev script that wipes the user table, an operator tidying up, a partial
// restore: the account has to come back, or the install has no way in and nothing
// says so.
func TestBuiltinAdminIsRestoredOnBoot(t *testing.T) {
	db := dbtest.New(t)

	if _, err := db.Conn().Exec(`DELETE FROM users WHERE id = ?`, database.BuiltinAdminID); err != nil {
		t.Fatalf("deleting the built-in administrator: %v", err)
	}
	// The cascade takes the credential with it, which is what would otherwise make
	// the next boot fail on a foreign key rather than heal.
	if cred, err := db.GetPasswordCredential(database.BuiltinAdminID); err != nil || cred != nil {
		t.Fatalf("credential after delete = %v, %v, want gone", cred, err)
	}

	if err := db.Bootstrap(); err != nil {
		t.Fatalf("second boot: %v", err)
	}

	user, err := db.GetUser(database.BuiltinAdminID)
	if err != nil || user == nil {
		t.Fatalf("the account was not restored: %v, %v", user, err)
	}
	// A restored account gets a password on the same terms as a fresh install's:
	// generated, printed once, and replaceable-only. Restoring it with the
	// published default would re-open the claim race on an install that is
	// already published — which is precisely when this path runs.
	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatalf("the restored account has no password: %v, %v", cred, err)
	}
	if !cred.MustChange {
		t.Error("the restored password does not require a change")
	}
	if password.Verify(cred.Hash, password.Default) {
		t.Error("the restored account has the published default password")
	}
}

// Boot must be idempotent: it runs on every start, including on installs where an
// administrator has renamed the account or changed its password.
func TestBootDoesNotUndoAdministratorChanges(t *testing.T) {
	db := dbtest.New(t)

	hash, err := password.Hash("a-password-of-my-own-choosing")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := db.SetPassword(database.BuiltinAdminID, hash, false, database.AuditEntry{
		ActorType: database.PrincipalTypeOperator, ActorID: "test",
	}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := db.Conn().Exec(`UPDATE users SET login = 'renamed' WHERE id = ?`,
		database.BuiltinAdminID); err != nil {
		t.Fatalf("renaming: %v", err)
	}

	if err := db.Bootstrap(); err != nil {
		t.Fatalf("second boot: %v", err)
	}

	user, err := db.GetUser(database.BuiltinAdminID)
	if err != nil || user == nil {
		t.Fatalf("GetUser: %v, %v", user, err)
	}
	if user.Login != "renamed" {
		t.Errorf("login = %q, want the administrator's rename to survive", user.Login)
	}
	if isDefault, _ := db.BuiltinAdminHasDefaultPassword(); isDefault {
		t.Error("boot reset a password an administrator had chosen")
	}
}

// Changing a credential must kill the sessions minted with the old one.
func TestSetPasswordRevokesSessions(t *testing.T) {
	db := dbtest.New(t)

	before, err := db.GetTokenVersion(database.BuiltinAdminID)
	if err != nil {
		t.Fatalf("GetTokenVersion: %v", err)
	}

	hash, err := password.Hash("a-password-of-my-own-choosing")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := db.SetPassword(database.BuiltinAdminID, hash, false, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	after, err := db.GetTokenVersion(database.BuiltinAdminID)
	if err != nil {
		t.Fatalf("GetTokenVersion: %v", err)
	}
	if after <= before {
		t.Errorf("token version %d -> %d, want a bump so old sessions die", before, after)
	}

	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatalf("GetPasswordCredential: %v, %v", cred, err)
	}
	if cred.MustChange {
		t.Error("must_change survived a deliberate password change")
	}
}

func TestPasswordLockout(t *testing.T) {
	db := dbtest.New(t)

	for i := 1; i < 10; i++ {
		lockedUntil, err := db.RecordPasswordFailure(database.BuiltinAdminID)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if lockedUntil != nil {
			t.Fatalf("locked out after only %d failures", i)
		}
	}

	lockedUntil, err := db.RecordPasswordFailure(database.BuiltinAdminID)
	if err != nil {
		t.Fatalf("tenth failure: %v", err)
	}
	if lockedUntil == nil {
		t.Fatal("ten failures did not lock the account")
	}
	if !lockedUntil.After(time.Now().UTC()) {
		t.Errorf("locked_until = %s, want a future time", lockedUntil)
	}

	cred, err := db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatalf("GetPasswordCredential: %v, %v", cred, err)
	}
	if !cred.Throttled(time.Now().UTC()) {
		t.Error("the credential does not report itself throttled")
	}

	// A correct password clears it; waiting out one lockout does not hand back a
	// fresh budget of ten.
	if err := db.ClearPasswordFailures(database.BuiltinAdminID); err != nil {
		t.Fatalf("ClearPasswordFailures: %v", err)
	}
	if cred, _ := db.GetPasswordCredential(database.BuiltinAdminID); cred.Throttled(time.Now().UTC()) {
		t.Error("the lockout survived a successful sign-in")
	}
}

// Only accounts that actually have a password are reachable this way. An SSO
// user's login must not become something to guess against.
func TestFindPasswordUser(t *testing.T) {
	db := dbtest.New(t)

	if err := db.UpsertUser(&database.User{
		ID: "sso-only", Login: "jdoe", Email: "jdoe@acme.com",
	}); err != nil {
		t.Fatalf("seeding an SSO user: %v", err)
	}

	for _, identifier := range []string{
		database.BuiltinAdminLogin,
		"ADMIN",
		database.BuiltinAdminEmail,
	} {
		user, cred, err := db.FindPasswordUser(identifier)
		if err != nil {
			t.Fatalf("FindPasswordUser(%q): %v", identifier, err)
		}
		if user == nil || cred == nil || user.ID != database.BuiltinAdminID {
			t.Errorf("FindPasswordUser(%q) = %v, want the built-in administrator", identifier, user)
		}
	}

	for _, identifier := range []string{"jdoe", "jdoe@acme.com", "nobody", "", "  "} {
		user, cred, err := db.FindPasswordUser(identifier)
		if err != nil {
			t.Fatalf("FindPasswordUser(%q): %v", identifier, err)
		}
		if user != nil || cred != nil {
			t.Errorf("FindPasswordUser(%q) = %v, want nothing", identifier, user)
		}
	}
}

// The question behind "may local sign-in be switched off": not whether another
// provider exists, but whether an administrator can actually get in through one.
func TestHasAdminIdentityOn(t *testing.T) {
	db := dbtest.New(t)

	if ok, err := db.HasAdminIdentityOn(nil); err != nil || ok {
		t.Errorf("HasAdminIdentityOn(nil) = %v, %v, want false", ok, err)
	}
	if ok, err := db.HasAdminIdentityOn([]string{"okta"}); err != nil || ok {
		t.Errorf("with no identities = %v, %v, want false", ok, err)
	}

	// A non-admin on the provider is not an answer.
	if err := db.UpsertUser(&database.User{ID: "viewer-1", Login: "viewer"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := db.Conn().Exec(`
		INSERT INTO user_identities (provider_id, subject, user_id) VALUES ('okta', 'sub-v', 'viewer-1')`); err != nil {
		t.Fatalf("linking the viewer: %v", err)
	}
	if ok, _ := db.HasAdminIdentityOn([]string{"okta"}); ok {
		t.Error("a viewer counted as a way back in")
	}

	if err := db.UpsertUser(&database.User{ID: "admin-1", Login: "boss"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := db.UpdateUserRole("admin-1", database.SystemRoleAdmin); err != nil {
		t.Fatalf("promoting: %v", err)
	}
	if _, err := db.Conn().Exec(`
		INSERT INTO user_identities (provider_id, subject, user_id) VALUES ('okta', 'sub-a', 'admin-1')`); err != nil {
		t.Fatalf("linking the admin: %v", err)
	}
	if ok, err := db.HasAdminIdentityOn([]string{"okta"}); err != nil || !ok {
		t.Errorf("with an admin on okta = %v, %v, want true", ok, err)
	}

	// A locked administrator cannot sign in, so they are not a way back either.
	if _, err := db.Conn().Exec(`UPDATE users SET locked_at = NOW() WHERE id = 'admin-1'`); err != nil {
		t.Fatalf("locking: %v", err)
	}
	if ok, _ := db.HasAdminIdentityOn([]string{"okta"}); ok {
		t.Error("a locked administrator counted as a way back in")
	}
}
