package database_test

import (
	"context"
	"strings"
	"testing"

	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

const (
	rotateKeyA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	rotateKeyB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func mustEncrypt(t *testing.T, e *crypto.Encryptor, plain string) string {
	t.Helper()
	record, err := e.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return record
}

func newRing(t *testing.T, current string, retired ...string) *crypto.Encryptor {
	t.Helper()
	e, err := crypto.NewKeyring(current, retired)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return e
}

// collect drains the enumeration into plaintext by class, which is how every
// assertion below reads: the sweep must not change what any secret *is*.
func collect(t *testing.T, db *database.DB) []database.EncryptedSecret {
	t.Helper()
	var out []database.EncryptedSecret
	if err := db.EachEncryptedSecret(func(s database.EncryptedSecret) error {
		out = append(out, s)
		return nil
	}); err != nil {
		t.Fatalf("EachEncryptedSecret: %v", err)
	}
	return out
}

// seed writes one secret of every kind the enumeration claims to cover, under
// the given encryptor, and returns what each should decrypt back to.
func seed(t *testing.T, db *database.DB, e *crypto.Encryptor) map[string]string {
	t.Helper()

	app := mkApp(t, db, "rotate")
	want := map[string]string{}

	if err := db.SetEnvVar(app.ID, "API_TOKEN", mustEncrypt(t, e, "env-var-secret"), true); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}
	want["env-var-secret"] = "app environment variable"

	if err := db.SetGitToken(app.ID, mustEncrypt(t, e, "git-token-secret")); err != nil {
		t.Fatalf("SetGitToken: %v", err)
	}
	want["git-token-secret"] = "app git token"

	provider := &database.AuthProvider{
		ID: "oidc-rotate", Kind: "oidc", DisplayName: "Rotate", ClientID: "client",
		Issuer: "https://issuer.example", CreatedBy: database.BuiltinAdminID,
	}
	provider.SetEncryptedSecret(mustEncrypt(t, e, "provider-client-secret"))
	if err := db.CreateAuthProvider(provider, database.AuditEntry{
		ActorType: database.PrincipalTypeOperator, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("CreateAuthProvider: %v", err)
	}
	want["provider-client-secret"] = "auth provider client secret"

	if err := db.SaveConnection(&database.IntegrationConnection{
		ID: "iconn-rotate", Kind: "slack", Name: "Rotate",
		Secret: mustEncrypt(t, e, "integration-credential"),
	}, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	want["integration-credential"] = "integration connection credential"

	// The credential store's composite-keyed rows (security review
	// 2026-11-10, H-2), written as writeCredentialValues writes them.
	if _, err := db.Conn().Exec(`INSERT INTO credentials (id, name, description, created_by, created_at, updated_at)
		VALUES ('cred-rotate', 'Rotate', '', ?, now(), now())`, database.BuiltinAdminID); err != nil {
		t.Fatalf("seeding a credential: %v", err)
	}
	if _, err := db.Conn().Exec(`INSERT INTO credential_values (credential_id, key, value) VALUES ('cred-rotate', 'PASSWORD', ?)`,
		mustEncrypt(t, e, "credential-value")); err != nil {
		t.Fatalf("seeding a credential value: %v", err)
	}
	want["credential-value"] = "attached credential value"

	for key, plain := range map[string]string{
		database.SettingEmailPassword:            "smtp-password",
		database.SettingEmailAPIKey:              "mail-api-key",
		database.SettingTelemetryHeaders:         "telemetry-headers",
		database.SettingDependencyFeedCredential: "feed-credential",
	} {
		if _, err := db.PutSetting(key, mustEncrypt(t, e, plain), database.BuiltinAdminID); err != nil {
			t.Fatalf("PutSetting(%s): %v", key, err)
		}
		want[plain] = key
	}

	return want
}

// The enumeration is the specification: every kind of secret this install
// stores has to come back out of it, or the sweep skips it in silence and the
// startup report calls it readable.
func TestEachEncryptedSecretReachesEveryKind(t *testing.T) {
	db := dbtest.New(t)
	e := newRing(t, rotateKeyA)
	want := seed(t, db, e)

	got := collect(t, db)
	if len(got) != len(want) {
		t.Fatalf("enumeration returned %d secrets, seeded %d", len(got), len(want))
	}

	seen := map[string]bool{}
	for _, s := range got {
		plain, err := e.Decrypt(s.Record)
		if err != nil {
			t.Fatalf("%s (%s): %v", s.Where(), s.Class, err)
		}
		if _, ok := want[plain]; !ok {
			t.Errorf("enumeration returned something unseeded: %q from %s", plain, s.Where())
		}
		if s.Class == "" {
			t.Errorf("%s came back with no class", s.Where())
		}
		if s.Where() == "" {
			t.Errorf("a secret came back with nowhere to write it")
		}
		seen[plain] = true
	}
	for plain, class := range want {
		if !seen[plain] {
			t.Errorf("the enumeration missed the %s", class)
		}
	}
}

// An unset credential is not a record. Reporting one as unreadable would tell
// an install with no relay that its SMTP password is broken.
func TestEachEncryptedSecretSkipsUnsetValues(t *testing.T) {
	db := dbtest.New(t)

	mkApp(t, db, "empty")
	if _, err := db.PutSetting(database.SettingEmailPassword, "", database.BuiltinAdminID); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	// A connection with no credential of its own. Mail's is the ordinary case —
	// the relay is configured on its own page — and every install has one from
	// the migration, so this is a second under a name of its own.
	if err := db.SaveConnection(&database.IntegrationConnection{
		ID: "iconn-empty", Kind: "email", Name: "No credential",
	}, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	if got := collect(t, db); len(got) != 0 {
		for _, s := range got {
			t.Errorf("an unset value was enumerated: %s (%s)", s.Where(), s.Class)
		}
	}
}

// The sweep, end to end: everything written under the retired key comes out
// under the encrypt key, with every plaintext unchanged.
func TestRewriteMovesTheEstateOntoTheEncryptKey(t *testing.T) {
	db := dbtest.New(t)

	old := newRing(t, rotateKeyB)
	want := seed(t, db, old)

	after := newRing(t, rotateKeyA, rotateKeyB)
	for _, s := range collect(t, db) {
		if crypto.RecordKeyID(s.Record) != old.EncryptKeyID() {
			t.Fatalf("%s was not written under the old key", s.Where())
		}
		plain, err := after.Decrypt(s.Record)
		if err != nil {
			t.Fatalf("%s: %v", s.Where(), err)
		}
		changed, err := db.RewriteEncryptedSecret(s, mustEncrypt(t, after, plain))
		if err != nil {
			t.Fatalf("RewriteEncryptedSecret(%s): %v", s.Where(), err)
		}
		if !changed {
			t.Errorf("%s reported no row changed", s.Where())
		}
	}

	// Everything is now under the new key alone — which is the moment the
	// retired key may be dropped, and not one instant before it.
	only := newRing(t, rotateKeyA)
	seen := map[string]bool{}
	for _, s := range collect(t, db) {
		if got := crypto.RecordKeyID(s.Record); got != only.EncryptKeyID() {
			t.Errorf("%s is still under key %s", s.Where(), got)
		}
		plain, err := only.Decrypt(s.Record)
		if err != nil {
			t.Fatalf("%s after the sweep: %v", s.Where(), err)
		}
		if _, ok := want[plain]; !ok {
			t.Errorf("%s changed value: %q", s.Where(), plain)
		}
		seen[plain] = true
	}
	if len(seen) != len(want) {
		t.Errorf("the sweep left %d secrets, started with %d", len(seen), len(want))
	}
}

// A sweep interrupted halfway is the case the whole record format exists for:
// both halves still read, and running it again finishes the job rather than
// starting it over.
func TestSweepIsResumable(t *testing.T) {
	db := dbtest.New(t)

	old := newRing(t, rotateKeyB)
	seed(t, db, old)
	after := newRing(t, rotateKeyA, rotateKeyB)

	all := collect(t, db)
	if len(all) < 4 {
		t.Fatalf("expected several secrets, got %d", len(all))
	}
	half := len(all) / 2
	for _, s := range all[:half] {
		plain, err := after.Decrypt(s.Record)
		if err != nil {
			t.Fatalf("%s: %v", s.Where(), err)
		}
		if _, err := db.RewriteEncryptedSecret(s, mustEncrypt(t, after, plain)); err != nil {
			t.Fatalf("RewriteEncryptedSecret: %v", err)
		}
	}

	// The interrupted state: two keys live at once, and everything readable.
	var pending int
	for _, s := range collect(t, db) {
		if !after.CanRead(s.Record) {
			t.Errorf("%s became unreadable mid-sweep", s.Where())
		}
		if _, err := after.Decrypt(s.Record); err != nil {
			t.Errorf("%s: %v", s.Where(), err)
		}
		if crypto.RecordKeyID(s.Record) != after.EncryptKeyID() {
			pending++
		}
	}
	if pending != len(all)-half {
		t.Errorf("%d records left to move, want %d", pending, len(all)-half)
	}

	// The resume: only what is left moves.
	var moved int
	for _, s := range collect(t, db) {
		if crypto.RecordKeyID(s.Record) == after.EncryptKeyID() {
			continue
		}
		plain, err := after.Decrypt(s.Record)
		if err != nil {
			t.Fatalf("%s: %v", s.Where(), err)
		}
		if _, err := db.RewriteEncryptedSecret(s, mustEncrypt(t, after, plain)); err != nil {
			t.Fatalf("RewriteEncryptedSecret: %v", err)
		}
		moved++
	}
	if moved != len(all)-half {
		t.Errorf("the resume moved %d records, want %d", moved, len(all)-half)
	}
	for _, s := range collect(t, db) {
		if crypto.RecordKeyID(s.Record) != after.EncryptKeyID() {
			t.Errorf("%s survived two passes still under an old key", s.Where())
		}
	}
}

// A credential an administrator changes while the sweep is running must not be
// overwritten with the value the sweep read a moment earlier.
func TestRewriteDoesNotClobberAConcurrentChange(t *testing.T) {
	db := dbtest.New(t)

	old := newRing(t, rotateKeyB)
	seed(t, db, old)
	after := newRing(t, rotateKeyA, rotateKeyB)

	var target database.EncryptedSecret
	for _, s := range collect(t, db) {
		if s.Class == "SMTP password" {
			target = s
		}
	}
	if target.Record == "" {
		t.Fatal("no SMTP password was seeded")
	}

	stale, err := after.Decrypt(target.Record)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	// The administrator saves a new password while the sweep holds the old one.
	if _, err := db.PutSetting(database.SettingEmailPassword, mustEncrypt(t, after, "the-new-token"), database.BuiltinAdminID); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}

	changed, err := db.RewriteEncryptedSecret(target, mustEncrypt(t, after, stale))
	if err != nil {
		t.Fatalf("RewriteEncryptedSecret: %v", err)
	}
	if changed {
		t.Error("the sweep overwrote a credential that was changed while it ran")
	}

	got, err := db.GetSetting(database.SettingEmailPassword)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	plain, err := after.Decrypt(got.Value)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "the-new-token" {
		t.Errorf("the stored password is %q, want the administrator's new one", plain)
	}
}

// The refusal the command is built around: a record under a key nobody holds
// has to be visible as exactly that, before anything is written.
func TestUnreadableRecordsAreVisibleBeforeAnySweep(t *testing.T) {
	db := dbtest.New(t)

	gone := newRing(t, rotateKeyB)
	seed(t, db, gone)

	// The operator dropped ENCRYPTION_KEY_OLD too early.
	ring := newRing(t, rotateKeyA)
	var unreadable int
	for _, s := range collect(t, db) {
		if ring.CanRead(s.Record) {
			t.Errorf("%s reads under a key that did not write it", s.Where())
			continue
		}
		unreadable++
		_, err := ring.Decrypt(s.Record)
		if err == nil {
			t.Errorf("%s decrypted under the wrong key", s.Where())
			continue
		}
		// The error has to name the key, or the operator cannot tell which one
		// to put back.
		if !strings.Contains(err.Error(), crypto.RecordKeyID(s.Record)) {
			t.Errorf("%s: the error does not name the key: %v", s.Where(), err)
		}
	}
	if unreadable == 0 {
		t.Fatal("nothing was seeded")
	}
}

// The wrapped data key is stored so it survives a restart. Without that, a
// managed install would mint a new key on every boot and orphan everything the
// previous one wrote — which is the failure the table exists to prevent.
func TestDataKeyStoreRoundTrip(t *testing.T) {
	db := dbtest.New(t)

	if _, _, ok, err := db.ActiveDEK(); err != nil {
		t.Fatalf("ActiveDEK on an empty store: %v", err)
	} else if ok {
		t.Error("a fresh install already has an active data key")
	}

	if err := db.PutDEK("aaaa1111", []byte("wrapped-one"), "fake-kms://one"); err != nil {
		t.Fatalf("PutDEK: %v", err)
	}
	id, wrapped, ok, err := db.ActiveDEK()
	if err != nil || !ok {
		t.Fatalf("ActiveDEK: %v (ok=%v)", err, ok)
	}
	if id != "aaaa1111" || string(wrapped) != "wrapped-one" {
		t.Errorf("ActiveDEK = %q/%q", id, wrapped)
	}

	// A second generation takes over, and the first stays readable — its
	// records still exist, and dropping the row that opens them would destroy
	// them.
	if err := db.PutDEK("bbbb2222", []byte("wrapped-two"), "fake-kms://one"); err != nil {
		t.Fatalf("PutDEK: %v", err)
	}
	id, _, _, err = db.ActiveDEK()
	if err != nil {
		t.Fatalf("ActiveDEK: %v", err)
	}
	if id != "bbbb2222" {
		t.Errorf("the second generation did not become active: %q", id)
	}
	if w, ok, err := db.DEK("aaaa1111"); err != nil || !ok || string(w) != "wrapped-one" {
		t.Errorf("the retired generation is no longer readable: %q ok=%v err=%v", w, ok, err)
	}
	if _, ok, err := db.DEK("cccc3333"); err != nil || ok {
		t.Errorf("a data key this install never had came back: ok=%v err=%v", ok, err)
	}
}

// The whole managed path against the real store: generate, wrap, store,
// restart, read.
func TestManagedBackendAgainstTheRealStore(t *testing.T) {
	db := dbtest.New(t)

	kek, err := crypto.NewFakeKEK(rotateKeyB)
	if err != nil {
		t.Fatalf("NewFakeKEK: %v", err)
	}

	first, err := crypto.New(context.Background(), "", nil, crypto.Options{KEK: kek, DEKs: db})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if first.Backend() != crypto.BackendManaged {
		t.Fatalf("backend is %q", first.Backend())
	}

	app := mkApp(t, db, "managed")
	if err := db.SetEnvVar(app.ID, "TOKEN", mustEncrypt(t, first, "under-the-data-key"), true); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}

	// A restart reads the wrapped key back rather than minting a new one.
	second, err := crypto.New(context.Background(), "", nil, crypto.Options{KEK: kek, DEKs: db})
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	if second.EncryptKeyID() != first.EncryptKeyID() {
		t.Errorf("the restart minted a new data key: %s then %s", first.EncryptKeyID(), second.EncryptKeyID())
	}

	found := 0
	for _, s := range collect(t, db) {
		found++
		if !second.CanRead(s.Record) {
			t.Errorf("%s is unreadable after the restart", s.Where())
		}
		plain, err := second.Decrypt(s.Record)
		if err != nil {
			t.Fatalf("%s: %v", s.Where(), err)
		}
		if plain != "under-the-data-key" {
			t.Errorf("%s = %q", s.Where(), plain)
		}
	}
	if found != 1 {
		t.Errorf("enumerated %d secrets, seeded 1", found)
	}
}
