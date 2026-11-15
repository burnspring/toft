package artifact

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

// These tests run the Azure Blob artifact store against a *real* server rather
// than the fake in azblob_test.go, and the argument is verbatim the one
// s3_integration_test.go makes about MinIO: the fake pins the request shapes
// this code sends, and only a real server proves the other half — that actual
// responses, error codes and metadata handling are read correctly. A fake
// written by the same person as the client agrees with the client by
// construction, which is exactly the failure this closes.
//
// Azurite is Microsoft's own emulator, and its existence is the reason the
// Azure object store was worth building before the ECS backend: ECS has been
// designed and unbuilt since ADR §26 with nothing to resolve its behaviour
// against.
//
//	docker compose --profile azure up -d azurite
//	export TEST_AZURITE_ENDPOINT=http://127.0.0.1:10000/devstoreaccount1
//	go test ./internal/artifact/
//
// Skipped when TEST_AZURITE_ENDPOINT is unset, following the TEST_S3_ENDPOINT
// pattern beside it, so `go test ./...` stays green with nothing running.
//
// **What Azurite cannot test is a mount's behaviour under load or a real
// account's error codes**, which is why the `blob` store's mount half stays
// credentials-gated exactly as EFS is. It *can* answer what the driver asks for:
// blobfuse2's `--subdirectory` containment was settled here rather than against
// a real account, because the property is what the client requests and the
// observable is its outgoing requests (ADR §84).
const azuriteEndpointEnv = "TEST_AZURITE_ENDPOINT"

// azuriteAccountKey is Azurite's well-known development key, published in
// Microsoft's own documentation. It is not a credential: it reaches an emulator
// and nothing else.
const azuriteAccountKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

func azuriteConnectionString(endpoint string) string {
	return fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=%s;BlobEndpoint=%s;",
		azuriteAccountKey, strings.TrimSuffix(endpoint, "/"))
}

// liveAzBlobStore returns a store backed by a container dedicated to this test,
// and removes the container when the test ends. A container per test keeps
// Expire's prefix sweeps from seeing another test's blobs.
func liveAzBlobStore(t *testing.T) (*AzBlobStore, string) {
	t.Helper()
	conn, name := liveAzContainer(t)

	store, err := NewAzBlobStore(AzBlobConfig{
		Container:        name,
		Prefix:           "install-a",
		ConnectionString: conn,
	})
	if err != nil {
		t.Fatalf("NewAzBlobStore: %v", err)
	}
	return store, name
}

// liveAzContainer creates a container named after the test and returns the
// connection string that reaches it.
func liveAzContainer(t *testing.T) (conn, name string) {
	t.Helper()

	endpoint := os.Getenv(azuriteEndpointEnv)
	if endpoint == "" {
		t.Skipf("%s not set — skipping live Azure Blob test (see file docs)", azuriteEndpointEnv)
	}
	conn = azuriteConnectionString(endpoint)

	// A container name derived from the test name: lowercase, no underscores,
	// three to sixty-three characters, which is Azure's rule.
	name = "toft-test-" + strings.ToLower(strings.NewReplacer("_", "-", "/", "-").Replace(t.Name()))
	if len(name) > 63 {
		name = name[:63]
	}

	svc, err := service.NewClientFromConnectionString(conn, nil)
	if err != nil {
		t.Fatalf("azurite connection string: %v", err)
	}
	ctx := context.Background()
	if _, err := svc.NewContainerClient(name).Create(ctx, nil); err != nil &&
		!strings.Contains(err.Error(), "ContainerAlreadyExists") {
		t.Fatalf("creating test container %s at %s: %v", name, endpoint, err)
	}
	t.Cleanup(func() {
		svc.NewContainerClient(name).Delete(context.Background(), nil)
	})
	return conn, name
}

// The round trip against a real server. The fake proves what is sent; this
// proves what comes back is understood — including that Azure returns metadata
// keys title-cased, which a client reading its own spelling back would miss and
// report as an artifact stored without a digest.
func TestAzBlobLiveRoundTrip(t *testing.T) {
	store, _ := liveAzBlobStore(t)
	ctx := context.Background()

	src := writeTree(t, map[string]string{
		"app.py":               "print('hi')",
		"venv/lib/site/mod.py": "x = 1",
	})
	key := ReleaseKey("app-1", "deploy-1")

	desc, err := store.Put(ctx, key, src)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if len(desc.SHA256) != 64 {
		t.Fatalf("no digest recorded: %+v", desc)
	}

	stat, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.SHA256 != desc.SHA256 {
		t.Fatalf("the digest did not survive a real server: stored %q, read back %q",
			desc.SHA256, stat.SHA256)
	}
	if stat.Size != desc.Size {
		t.Errorf("size = %d, want %d", stat.Size, desc.Size)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatalf("get: %v", err)
	}
	got := readTree(t, dest)
	if got["app.py"] != "print('hi')" || got["venv/lib/site/mod.py"] != "x = 1" {
		t.Errorf("restored tree wrong: %v", got)
	}
}

// A real server's not-found, which is the one the fake can only imitate. Both
// the GET (an error code in the body) and the HEAD (a bare 404 with none) have
// to land on ErrNotFound.
func TestAzBlobLiveMissingKeyIsNotFound(t *testing.T) {
	store, _ := liveAzBlobStore(t)
	ctx := context.Background()

	if err := store.Get(ctx, ReleaseKey("app-1", "nope"), t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
	if _, err := store.Stat(ctx, ReleaseKey("app-1", "nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat err = %v, want ErrNotFound", err)
	}
	// And a delete of nothing is still not an error, which is what makes
	// pruning idempotent on a backend that answers 404 where S3 shrugs.
	if err := store.Delete(ctx, ReleaseKey("app-1", "nope")); err != nil {
		t.Errorf("Delete of an absent blob: %v", err)
	}
}

// Listing and pruning against a real server: the paginator's shape, and that
// the install prefix is stripped correctly on the way back out. Getting the
// second wrong would make every keep-set comparison fail and prune everything.
func TestAzBlobLiveExpireKeepsExactlyTheNamedReleases(t *testing.T) {
	store, _ := liveAzBlobStore(t)
	ctx := context.Background()
	src := writeTree(t, map[string]string{"app.py": "x"})

	var keys []string
	for _, id := range []string{"d1", "d2", "d3"} {
		k := ReleaseKey("app-1", id)
		if _, err := store.Put(ctx, k, src); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	other := ReleaseKey("app-2", "d1")
	if _, err := store.Put(ctx, other, src); err != nil {
		t.Fatal(err)
	}

	removed, err := store.Expire(ctx, "app-1", keys[2:])
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d, want 2", removed)
	}
	if _, err := store.Stat(ctx, keys[2]); err != nil {
		t.Errorf("kept release was removed: %v", err)
	}
	if _, err := store.Stat(ctx, other); err != nil {
		t.Errorf("another app's release was expired: %v", err)
	}
}

// --- The signer, against Azurite ---

func liveAzBlobSigner(t *testing.T) (*AzBlobSigner, string) {
	t.Helper()
	conn, name := liveAzContainer(t)

	signer, err := NewAzBlobSigner(AzBlobSignerConfig{ConnectionString: conn})
	if err != nil {
		t.Fatalf("NewAzBlobSigner: %v", err)
	}
	return signer, name
}

// A signed URL a real HTTP client actually executes — the half a fake cannot
// prove. Read and write, and the read is scoped to the one blob it names.
func TestAzBlobLivePresignedURLsWorkAndAreScoped(t *testing.T) {
	signer, containerName := liveAzBlobSigner(t)
	ctx := context.Background()

	body := []byte("name,count\nreports,3\n")
	req, err := signer.PresignPut(ctx, containerName, "grants/report.csv", time.Minute, PutConditions{
		ContentMD5:    azMD5(body),
		ContentLength: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if code, msg := putSigned(t, req, body); code != http.StatusCreated {
		t.Fatalf("presigned PUT: %d %s", code, msg)
	}

	req, err = signer.PresignGet(ctx, containerName, "grants/report.csv", time.Minute, "")
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	code, got := putSigned(t, req, nil)
	if code != http.StatusOK {
		t.Fatalf("presigned GET: %d %s", code, got)
	}
	if got != string(body) {
		t.Errorf("read back %q, want %q", got, string(body))
	}

	// The URL addresses one blob. The same signature against a sibling must be
	// refused, or a grant on one object is a grant on the container.
	sibling := strings.Replace(req.URL, "grants/report.csv", "grants/secret.csv", 1)
	resp, err := http.Get(sibling)
	if err != nil {
		t.Fatalf("sibling GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("a URL signed for one blob served a different one")
	}
}

// The `write` rung, on the wire, against a real server.
//
// This is the Azure half of TestWriteRungRefusesAnOverwrite, and it proves the
// thing D4 turns on: `c` without `w` is refused by the *service* when the blob
// already exists. On S3 the same rung is a signed If-None-Match; here it is a
// permission, because a SAS does not sign request headers and a condition the
// client could decline is not a condition.
func TestAzBlobLiveWriteRungRefusesAnOverwrite(t *testing.T) {
	signer, containerName := liveAzBlobSigner(t)
	ctx := context.Background()

	body := []byte("first")
	overwrite := PutConditions{ContentMD5: azMD5(body), ContentLength: int64(len(body))}

	// `overwrite` writes it the first time.
	req, err := signer.PresignPut(ctx, containerName, "grants/once", time.Minute, overwrite)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if code, msg := putSigned(t, req, body); code != http.StatusCreated {
		t.Fatalf("seeding: %d %s", code, msg)
	}

	// `write` refuses the second.
	conditional := overwrite
	conditional.IfNoneMatch = true
	req, err = signer.PresignPut(ctx, containerName, "grants/once", time.Minute, conditional)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	code, msg := putSigned(t, req, body)
	if code == http.StatusCreated || code == http.StatusOK {
		t.Errorf("overwriting under a `write` grant succeeded (%d) — the rung is not holding", code)
	}
	if code != http.StatusForbidden {
		t.Logf("refused with %d: %s", code, msg)
	}

	// And the same rung still creates something new, or `write` would be `read`.
	req, err = signer.PresignPut(ctx, containerName, "grants/twice", time.Minute, conditional)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if code, msg := putSigned(t, req, body); code != http.StatusCreated {
		t.Errorf("a `write` grant could not create a new blob: %d %s", code, msg)
	}
}

// The disposition is signed, so the server has to honour it and a caller cannot
// substitute one. `rscd` is a service SAS field, which is the same guarantee
// response-content-disposition gets under SigV4.
func TestAzBlobLiveDispositionIsHonoured(t *testing.T) {
	signer, containerName := liveAzBlobSigner(t)
	ctx := context.Background()

	body := []byte("a,b\n1,2\n")
	req, err := signer.PresignPut(ctx, containerName, "grants/data.csv", time.Minute, PutConditions{
		ContentMD5: azMD5(body), ContentLength: int64(len(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := putSigned(t, req, body); code != http.StatusCreated {
		t.Fatalf("seeding: %d %s", code, msg)
	}

	req, err = signer.PresignGet(ctx, containerName, "grants/data.csv", time.Minute, "Q1 report.csv")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(req.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, `filename="Q1 report.csv"`) {
		t.Errorf("Content-Disposition = %q — the signed rscd was not honoured", got)
	}
}

// Listing with the "/" delimiter is what the store browser draws, and it is a
// different Azure operation from the flat listing a CLI wants. Both, plus a
// cursor, because a folder view that silently returned one page and called it
// the contents is the failure the delimiter exists to avoid.
func TestAzBlobLiveListingWithDelimiterAndCursor(t *testing.T) {
	signer, containerName := liveAzBlobSigner(t)
	ctx := context.Background()

	seed := func(key string) {
		body := []byte(key)
		req, err := signer.PresignPut(ctx, containerName, key, time.Minute, PutConditions{
			ContentMD5: azMD5(body), ContentLength: int64(len(body)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if code, msg := putSigned(t, req, body); code != http.StatusCreated {
			t.Fatalf("seeding %s: %d %s", key, code, msg)
		}
	}
	seed("team/top.csv")
	seed("team/reports/q1.csv")
	seed("team/reports/q2.csv")
	seed("team/archive/old.csv")

	// One level, with folders.
	page, err := signer.ListObjects(ctx, containerName, "team/", "", "/", 100)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(page.Objects) != 1 || page.Objects[0].Name != "top.csv" {
		t.Errorf("objects = %+v, want just top.csv", page.Objects)
	}
	wantPrefixes := map[string]bool{"archive/": true, "reports/": true}
	for _, p := range page.Prefixes {
		if !wantPrefixes[p] {
			t.Errorf("unexpected folder %q", p)
		}
		delete(wantPrefixes, p)
	}
	for p := range wantPrefixes {
		t.Errorf("folder %q was not listed", p)
	}

	// Flat, which is what a CLI wants, and paged — a cursor that skipped or
	// repeated would make a listing quietly lie.
	seen := map[string]bool{}
	cursor := ""
	for range 10 {
		page, err := signer.ListObjects(ctx, containerName, "team/", cursor, "", 2)
		if err != nil {
			t.Fatalf("ListObjects: %v", err)
		}
		for _, o := range page.Objects {
			if seen[o.Name] {
				t.Errorf("%q listed twice across pages", o.Name)
			}
			seen[o.Name] = true
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	for _, want := range []string{"top.csv", "reports/q1.csv", "reports/q2.csv", "archive/old.csv"} {
		if !seen[want] {
			t.Errorf("%q was never listed", want)
		}
	}
}

func azMD5(body []byte) string {
	sum := md5.Sum(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// The isolated-workload path against a real server: the release a store Put is
// read back through the store's own PresignGet byte for byte, and its
// PresignPut accepts an output PUT sent exactly the way every SDK's
// write_output sends it — a bare body with `x-ms-blob-type: BlockBlob`.
func TestAzBlobLiveStorePresignsReleaseAndOutput(t *testing.T) {
	store, _ := liveAzBlobStore(t)
	ctx := context.Background()

	src := writeTree(t, map[string]string{"app.py": "print('hi')"})
	if _, err := store.Put(ctx, "releases/app/dep.tar.gz", src); err != nil {
		t.Fatalf("Put: %v", err)
	}
	getURL, err := store.PresignGet(ctx, "releases/app/dep.tar.gz", PresignTTL)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	fetched, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned release GET: %d %s", resp.StatusCode, fetched)
	}
	stored := filepath.Join(t.TempDir(), "via-sdk")
	if err := store.Get(ctx, "releases/app/dep.tar.gz", stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	desc, err := store.Stat(ctx, "releases/app/dep.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(fetched)) != desc.Size {
		t.Errorf("presigned GET read %d bytes, the blob is %d", len(fetched), desc.Size)
	}

	key := RunOutputKey("app", "run-1", "output")
	putURL, err := store.PresignPut(ctx, key, time.Hour)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	body := []byte(`{"rows": 3}`)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("output PUT: %v", err)
	}
	put.Body.Close()
	if put.StatusCode != http.StatusCreated {
		t.Fatalf("output PUT = %d, want 201", put.StatusCode)
	}
	readURL, _ := store.PresignGet(ctx, key, time.Minute)
	resp, err = http.Get(readURL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != string(body) {
		t.Errorf("output read back %q, want %q", got, body)
	}
}
