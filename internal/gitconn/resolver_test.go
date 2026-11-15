package gitconn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"toft/internal/crypto"
	"toft/internal/database"
)

const testKey = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"

func testEncryptor(t *testing.T, key string) *crypto.Encryptor {
	t.Helper()
	enc, err := crypto.NewEncryptor(key)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// The three answers, and the provenance that comes with each. The struct is
// what the call sites hold; a resolver that returned a bare string would make
// every message downstream guess.
func TestCloneCredentialCarriesItsProvenance(t *testing.T) {
	enc := testEncryptor(t, testKey)
	r := NewResolver(nil, enc)

	stored, err := enc.Encrypt("ghp_deadbeef")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	cred, err := r.CloneCredential(context.Background(),
		&database.App{RepoURL: "https://github.com/acme/reports", GitToken: stored})
	if err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	if cred.Token != "ghp_deadbeef" {
		t.Errorf("token = %q", cred.Token)
	}
	if cred.Source != SourceAppToken {
		t.Errorf("source = %q, want %q", cred.Source, SourceAppToken)
	}
	if cred.Connection != "" {
		t.Errorf("an app token named a connection: %q", cred.Connection)
	}

	cred, err = r.CloneCredential(context.Background(),
		&database.App{RepoURL: "https://github.com/acme/public"})
	if err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	if cred.Token != "" || cred.Source != SourceNone {
		t.Errorf("an app with no credential resolved to %+v", cred)
	}
}

// A rotated ENCRYPTION_KEY is indistinguishable from a revoked token to the
// app's owner, and both call sites used to say something different about it —
// the poller an event with a sentence, the builder a build log with Go's
// cipher error in it. The resolver phrases it once.
func TestAnUnreadableCredentialIsExplainedOnce(t *testing.T) {
	written := testEncryptor(t, testKey)
	stored, err := written.Encrypt("ghp_deadbeef")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	rotated := testEncryptor(t, strings.Repeat("ab", 32))
	r := NewResolver(nil, rotated)

	cred, err := r.CloneCredential(context.Background(),
		&database.App{RepoURL: "https://github.com/acme/reports", GitToken: stored})
	if err == nil {
		t.Fatal("a credential that will not decrypt resolved without an error")
	}
	if cred.Token != "" {
		t.Error("a token came back beside the error")
	}

	var unreadable *Unreadable
	if !errors.As(err, &unreadable) {
		t.Fatalf("error is %T, want *Unreadable", err)
	}
	if unreadable.Cred.Source != SourceAppToken {
		t.Errorf("the failure lost its provenance: %+v", unreadable.Cred)
	}
	if !strings.Contains(unreadable.Summary, "https://github.com/acme/reports") {
		t.Errorf("the summary does not name the repository: %q", unreadable.Summary)
	}
	if !strings.Contains(unreadable.Summary, "app's settings") {
		t.Errorf("a pasted token's summary does not say where to re-enter it: %q", unreadable.Summary)
	}
	// The cipher's own words belong in the event's detail, never in the
	// sentence a person reads.
	if unreadable.Err == nil {
		t.Error("the decryption failure was dropped")
	}
	if strings.Contains(unreadable.Summary, "cipher") {
		t.Errorf("the summary carries Go's error: %q", unreadable.Summary)
	}
}
