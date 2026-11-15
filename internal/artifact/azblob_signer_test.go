package artifact

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

func newTestAzBlobSigner(t *testing.T, srv *httptest.Server) *AzBlobSigner {
	t.Helper()
	signer, err := NewAzBlobSigner(AzBlobSignerConfig{
		ConnectionString: fakeAzConnectionString(srv.URL),
	})
	if err != nil {
		t.Fatalf("NewAzBlobSigner: %v", err)
	}
	return signer
}

// signedPermissions pulls the `sp` field out of a minted URL. It is the
// permission set the *service* will enforce, which on this backend is where the
// ladder's rungs live — not in a header the client is asked to send.
func signedPermissions(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing the signed URL: %v", err)
	}
	return u.Query().Get("sp")
}

// The D4 table, asserted as strings.
//
// This is the ladder's *third* expression, and the reason it can be a table
// here rather than an oracle comparison is that the other two are about flags
// and headers: an S3 presigned PUT carries its condition inside the signature,
// and a SAS cannot — it signs the permission set instead. So the mapping from
// rung to permission letters is the thing that has to be pinned, and a change
// to it that nobody notices is a permission escalation.
//
// `c` without `w` is the whole of the `write` rung: Put Blob against a blob
// that already exists needs Write, so a create-only SAS is refused by Azure
// rather than by a client's good manners.
func TestAzBlobSASPermissionsPerRung(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()
	signer := newTestAzBlobSigner(t, srv)
	ctx := context.Background()

	t.Run("read is r", func(t *testing.T) {
		got, err := signer.PresignGet(ctx, "c", "k.csv", time.Minute, "")
		if err != nil {
			t.Fatal(err)
		}
		if perms := signedPermissions(t, got.URL); perms != "r" {
			t.Errorf("GET signed sp=%q, want %q", perms, "r")
		}
		if got.Method != "GET" {
			t.Errorf("method = %q", got.Method)
		}
	})

	t.Run("write is c and not w", func(t *testing.T) {
		got, err := signer.PresignPut(ctx, "c", "k.csv", time.Minute, PutConditions{IfNoneMatch: true})
		if err != nil {
			t.Fatal(err)
		}
		perms := signedPermissions(t, got.URL)
		if perms != "c" {
			t.Errorf("the write rung signed sp=%q, want %q — `c` alone is what makes it "+
				"create-only, and adding `w` silently promotes it to full", perms, "c")
		}
	})

	t.Run("full's PUT is c and w", func(t *testing.T) {
		got, err := signer.PresignPut(ctx, "c", "k.csv", time.Minute, PutConditions{})
		if err != nil {
			t.Fatal(err)
		}
		// The SDK emits the letters in its own canonical order; compare as a
		// set so this test is about the permissions and not about the spelling.
		if perms := signedPermissions(t, got.URL); !sameLetters(perms, "cw") {
			t.Errorf("the full rung's PUT signed sp=%q, want the set %q", perms, "cw")
		}
	})

	t.Run("delete is d", func(t *testing.T) {
		got, err := signer.PresignDelete(ctx, "c", "k.csv", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if perms := signedPermissions(t, got.URL); perms != "d" {
			t.Errorf("DELETE signed sp=%q, want %q", perms, "d")
		}
		if got.Method != "DELETE" {
			t.Errorf("method = %q", got.Method)
		}
	})
}

func sameLetters(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, r := range want {
		if !strings.ContainsRune(got, r) {
			return false
		}
	}
	return true
}

// The disposition is a *signed* field of the SAS (`rscd`), not a header the
// client sends. Being signed is what makes it safe to derive from a request at
// all — a caller cannot append one to a URL the platform already minted, and
// cannot strip one either.
func TestAzBlobDispositionIsSigned(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()
	signer := newTestAzBlobSigner(t, srv)
	ctx := context.Background()

	with, err := signer.PresignGet(ctx, "c", "reports/q1.csv", time.Minute, "Q1 report.csv")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(with.URL)
	if err != nil {
		t.Fatal(err)
	}
	rscd := u.Query().Get("rscd")
	if !strings.HasPrefix(rscd, "attachment;") {
		t.Errorf("rscd = %q, want an attachment disposition", rscd)
	}
	if !strings.Contains(rscd, `filename="Q1 report.csv"`) {
		t.Errorf("rscd does not carry the filename: %q", rscd)
	}
	// The signature has to cover it. Two URLs differing only in rscd must have
	// different signatures, or the field is decoration.
	if u.Query().Get("sig") == "" {
		t.Fatal("no signature on the URL at all")
	}

	without, err := signer.PresignGet(ctx, "c", "reports/q1.csv", time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := url.Parse(without.URL)
	if plain.Query().Get("rscd") != "" {
		t.Errorf("rscd present with no filename asked for: %q", plain.Query().Get("rscd"))
	}
	if plain.Query().Get("sig") == u.Query().Get("sig") {
		t.Error("the same signature with and without rscd — the disposition is not covered by it, " +
			"which would let a caller add or remove one on a URL the platform minted")
	}
}

// A PUT must carry x-ms-blob-type, which the Put Blob REST API requires, and
// Content-MD5 when the caller computed one.
//
// The digest is deliberately NOT presented as a condition of the URL here: a
// SAS does not sign request headers, so this is the *server's* check on a body
// the client volunteered. Documented, not caught.
func TestAzBlobPutCarriesTheBlobTypeAndDigest(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()
	signer := newTestAzBlobSigner(t, srv)

	got, err := signer.PresignPut(context.Background(), "c", "k.csv", time.Minute, PutConditions{
		ContentMD5:     "1B2M2Y8AsgTpgAmY7PhCfg==",
		ChecksumSHA256: "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
		ContentLength:  1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Headers["x-ms-blob-type"] != "BlockBlob" {
		t.Errorf("no x-ms-blob-type header; Put Blob refuses without one: %v", got.Headers)
	}
	if got.Headers["Content-MD5"] != "1B2M2Y8AsgTpgAmY7PhCfg==" {
		t.Errorf("Content-MD5 = %q", got.Headers["Content-MD5"])
	}
	// x-amz-checksum-sha256 has no Azure equivalent and must not be invented as
	// a header nothing reads — a client sending it would get a 400, and a
	// reader of the response would learn nothing about the bytes.
	for name := range got.Headers {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-") {
			t.Errorf("an S3 header leaked onto an Azure URL: %q", name)
		}
	}
}

// The window is signed and it starts in the past. A URL minted on a host whose
// clock runs a minute ahead of Azure's would otherwise be refused as not yet
// valid, which reads to whoever is holding it as a permission problem.
func TestAzBlobSASWindowToleratesClockSkew(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()
	signer := newTestAzBlobSigner(t, srv)

	got, err := signer.PresignGet(context.Background(), "c", "k", 10*time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(got.URL)
	start, err := time.Parse("2006-01-02T15:04:05Z", u.Query().Get("st"))
	if err != nil {
		t.Fatalf("start time %q: %v", u.Query().Get("st"), err)
	}
	if !start.Before(time.Now().UTC()) {
		t.Errorf("the SAS window starts at %s, which is not in the past", start)
	}
	expiry, err := time.Parse("2006-01-02T15:04:05Z", u.Query().Get("se"))
	if err != nil {
		t.Fatalf("expiry %q: %v", u.Query().Get("se"), err)
	}
	if !expiry.After(time.Now().UTC()) {
		t.Errorf("the SAS expired at %s, before it was handed out", expiry)
	}
}

// The URL addresses one blob under one container, and a key with separators
// stays a path rather than becoming one escaped segment.
func TestAzBlobSignedURLAddressesOneBlob(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()
	signer := newTestAzBlobSigner(t, srv)

	got, err := signer.PresignGet(context.Background(), "acme-data", "team/reports/q1 2026.csv", time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/devstoreaccount1/acme-data/team/reports/q1 2026.csv" {
		t.Errorf("path = %q", u.Path)
	}
	if strings.Contains(u.RawPath+u.EscapedPath(), " ") {
		t.Errorf("the space was not escaped: %q", u.EscapedPath())
	}
}

// A connection string with no account key cannot sign anything, and saying so
// at construction is better than minting URLs that verify nowhere.
func TestAzBlobSignerNeedsSomethingToSignWith(t *testing.T) {
	if _, err := NewAzBlobSigner(AzBlobSignerConfig{
		ConnectionString: "DefaultEndpointsProtocol=http;AccountName=x;BlobEndpoint=http://127.0.0.1:1/x;",
	}); err == nil {
		t.Error("a connection string with no AccountKey was accepted")
	}
}

// The artifact store presigns for isolated workloads: a read SAS for the
// release and a create+write SAS for the output, each on the store's own
// container and prefixed key.
func TestAzBlobStorePresignsOneBlobUnderItsPrefix(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()
	store := newTestAzBlobStore(t, srv, "install-a")
	ctx := context.Background()

	get, err := store.PresignGet(ctx, "releases/app/dep.tar.gz", PresignTTL)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	u, _ := url.Parse(get)
	if u.Path != "/devstoreaccount1/toft-artifacts/install-a/releases/app/dep.tar.gz" {
		t.Errorf("GET path = %q", u.Path)
	}
	if sp := signedPermissions(t, get); sp != "r" {
		t.Errorf("GET sp = %q, want r", sp)
	}

	put, err := store.PresignPut(ctx, RunOutputKey("app", "run", "output"), time.Hour)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if sp := signedPermissions(t, put); sp != "cw" {
		t.Errorf("PUT sp = %q, want cw", sp)
	}
}

// A user-delegation SAS cannot outlive its key, so a cached key is reused only
// when it outlives the URL about to be signed. A key with two hours left
// serves a one-hour URL and is refreshed for a six-hour one.
func TestAzBlobDelegationKeyOutlivesTheURLSignedWithIt(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	srv.Close() // a refresh must fail, so reuse is observable
	prev := azMaxRetries
	azMaxRetries = -1
	t.Cleanup(func() { azMaxRetries = prev })
	signer := newTestAzBlobSigner(t, srv)
	signer.delegation = &service.UserDelegationCredential{}
	signer.delegationTo = time.Now().Add(2 * time.Hour)

	if got, err := signer.userDelegation(context.Background(), time.Hour); err != nil || got != signer.delegation {
		t.Fatalf("a key with 2h left was not reused for a 1h URL: %v", err)
	}
	if _, err := signer.userDelegation(context.Background(), PresignTTL); err == nil {
		t.Fatal("a key with 2h left was reused for a 6h URL, which would die with it")
	}
	if _, err := signer.userDelegation(context.Background(), azDelegationWindow); err == nil {
		t.Fatal("a URL longer than any delegation key was signed")
	}
}
