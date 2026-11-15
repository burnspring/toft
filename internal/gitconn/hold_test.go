package gitconn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func seedConnection(t *testing.T, db *database.DB, enc interface {
	Encrypt(string) (string, error)
}, secret string, enabled bool) *database.GitConnection {
	t.Helper()
	ciphertext, err := enc.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	conn := &database.GitConnection{
		ID: "conn-1", Kind: database.GitConnGitHub, DisplayName: "Acme GitHub",
		BaseURL: "https://github.com", AuthMethod: database.GitAuthToken,
		Enabled: enabled, Coverage: "acme/*",
	}
	conn.SetEncryptedCloneSecret(ciphertext)
	if err := db.CreateGitConnection(conn, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("CreateGitConnection: %v", err)
	}
	return conn
}

// The connection is the credential, and the app carries none.
func TestAConnectionAnswersTheCloneCredential(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	seedConnection(t, db, enc, "github_pat_live", true)
	r := NewResolver(db, enc)

	cred, err := r.CloneCredential(context.Background(), &database.App{
		Slug: "reports", RepoURL: "https://github.com/acme/reports", GitConnectionID: "conn-1",
	})
	if err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	if cred.Token != "github_pat_live" {
		t.Errorf("token = %q", cred.Token)
	}
	if cred.Source != SourceConnection {
		t.Errorf("source = %q, want %q", cred.Source, SourceConnection)
	}
	if cred.Connection != "Acme GitHub" {
		t.Errorf("connection = %q, want the display name a person would recognise", cred.Connection)
	}
}

// **The connection wins over the app's own token.** An administrator's
// install-wide decision outranks a token one owner pasted, and the alternative
// — a stale personal token quietly beating a working connection — is the
// failure connections exist to end.
func TestTheConnectionWinsOverAPastedToken(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	seedConnection(t, db, enc, "the-connections", true)
	stale, err := enc.Encrypt("the-owners")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	r := NewResolver(db, enc)

	cred, err := r.CloneCredential(context.Background(), &database.App{
		Slug: "both", RepoURL: "https://github.com/acme/both",
		GitConnectionID: "conn-1", GitToken: stale,
	})
	if err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	if cred.Token != "the-connections" {
		t.Errorf("the app's own token won: %q", cred.Token)
	}
}

// Test 7. **A disabled connection refuses on the next resolve**, with no
// restart and no cache flush called by hand — the row is read live and the
// resolver caches no `enabled`. That is what makes "off means now" survive a
// credential shared by an estate.
func TestADisabledConnectionRefusesOnTheNextResolve(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := seedConnection(t, db, enc, "github_pat_live", true)
	r := NewResolver(db, enc)
	app := &database.App{
		Slug: "reports", RepoURL: "https://github.com/acme/reports", GitConnectionID: conn.ID,
	}

	if _, err := r.CloneCredential(context.Background(), app); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	conn.Enabled = false
	if err := db.UpdateGitConnection(conn, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("UpdateGitConnection: %v", err)
	}

	cred, err := r.CloneCredential(context.Background(), app)
	if err == nil {
		t.Fatal("a disabled connection still handed out a credential")
	}
	if cred.Token != "" {
		t.Error("a token came back beside the refusal")
	}
	var unreadable *Unreadable
	if !errors.As(err, &unreadable) {
		t.Fatalf("error is %T, want *Unreadable", err)
	}
	// The owner is told about the connection and about an administrator, never
	// about a field on their own settings page.
	if !strings.Contains(unreadable.Summary, "Acme GitHub") {
		t.Errorf("the refusal does not name the connection: %q", unreadable.Summary)
	}
	if strings.Contains(unreadable.Summary, "app's settings") {
		t.Errorf("the owner is sent to a field they cannot use: %q", unreadable.Summary)
	}
	// And it says what did *not* happen, because that is the question somebody
	// reading it at 3am actually has.
	if !strings.Contains(unreadable.Summary, "Nothing running has stopped") {
		t.Errorf("the refusal does not say what still works: %q", unreadable.Summary)
	}
	if unreadable.Cred.Source != SourceConnection {
		t.Errorf("the refusal lost its provenance: %+v", unreadable.Cred)
	}
}

// A connection whose credential will not open sends the owner to an
// administrator, not to a token field. This is the poller's old sentence
// generalised: the same failure, phrased for whoever can actually fix it.
func TestAnUnreadableConnectionCredentialNamesTheAdministrator(t *testing.T) {
	db := dbtest.New(t)
	written := testEncryptor(t, testKey)
	seedConnection(t, db, written, "github_pat_live", true)

	rotated := testEncryptor(t, strings.Repeat("ab", 32))
	r := NewResolver(db, rotated)

	_, err := r.CloneCredential(context.Background(), &database.App{
		Slug: "reports", RepoURL: "https://github.com/acme/reports", GitConnectionID: "conn-1",
	})
	var unreadable *Unreadable
	if !errors.As(err, &unreadable) {
		t.Fatalf("error is %T, want *Unreadable", err)
	}
	if strings.Contains(unreadable.Summary, "app's settings") {
		t.Errorf("the owner is told to re-enter a token they do not have: %q", unreadable.Summary)
	}
	if !strings.Contains(unreadable.Summary, "Admin → Sources") {
		t.Errorf("the refusal does not say where the fix is: %q", unreadable.Summary)
	}
}

// A connection deleted out from under an app — which the removal refusal should
// have caught — falls back to whatever the app has of its own rather than
// failing every deploy on a row that is not there.
func TestAMissingConnectionFallsBackRatherThanFailing(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	own, err := enc.Encrypt("the-owners")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	r := NewResolver(db, enc)

	cred, err := r.CloneCredential(context.Background(), &database.App{
		Slug: "orphan", RepoURL: "https://github.com/acme/orphan",
		GitConnectionID: "gone", GitToken: own,
	})
	if err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	if cred.Source != SourceAppToken || cred.Token != "the-owners" {
		t.Errorf("cred = %+v, want the app's own token", cred)
	}
}
