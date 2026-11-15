package artifact

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeAzBlob is a small Azure Blob server: enough of the protocol to exercise
// the real SDK client end to end, and the exact peer of fakeS3 beside it.
//
// The fake pins the request shapes this code sends; Azurite proves the other
// half. Both are needed for the same reason stated in s3_test.go — a fake
// written by the same person as the client agrees with the client by
// construction — and the fake is what keeps `go test ./...` meaningful with
// nothing running.
//
// One deliberate cruelty: metadata comes back title-cased, the way real Azure
// returns it. A client that looked its own key up verbatim would find nothing,
// report the artifact as having no digest, and unpack a corrupt download clean.
type fakeAzBlob struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	blocks  map[string][]byte
	// failWith, when set, is returned for every request — used to prove an
	// outage does not read as a missing release.
	failWith int
}

func newFakeAzBlob() *fakeAzBlob {
	return &fakeAzBlob{objects: map[string]fakeObject{}, blocks: map[string][]byte{}}
}

func (f *fakeAzBlob) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failWith != 0 {
		http.Error(w, "backend unavailable", f.failWith)
		return
	}

	// /{account}/{container}/{key...}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	key := ""
	if len(parts) == 3 {
		key = parts[2]
	}
	q := r.URL.Query()

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		switch q.Get("comp") {
		case "block":
			f.blocks[q.Get("blockid")] = body
		case "blocklist":
			f.commitBlocks(w, r, key, body)
		default:
			f.objects[key] = fakeObject{body: body, meta: azHeaderMeta(r.Header)}
			f.created(w)
		}

	case http.MethodGet:
		if q.Get("restype") == "container" && q.Get("comp") == "list" {
			f.list(w, r)
			return
		}
		obj, ok := f.objects[key]
		if !ok {
			f.blobNotFound(w)
			return
		}
		f.writeMeta(w, obj)
		w.WriteHeader(http.StatusOK)
		w.Write(obj.body)

	case http.MethodHead:
		obj, ok := f.objects[key]
		if !ok {
			// A HEAD carries no body, so there is no error code to read —
			// exactly the gap HeadObject has on S3, and the reason mapAzError
			// falls back to the status.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.writeMeta(w, obj)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		if _, ok := f.objects[key]; !ok {
			f.blobNotFound(w)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusAccepted)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeAzBlob) commitBlocks(w http.ResponseWriter, r *http.Request, key string, list []byte) {
	var doc struct {
		Latest      []string `xml:"Latest"`
		Uncommitted []string `xml:"Uncommitted"`
	}
	if err := xml.Unmarshal(list, &doc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body []byte
	for _, id := range append(doc.Latest, doc.Uncommitted...) {
		body = append(body, f.blocks[id]...)
	}
	f.objects[key] = fakeObject{body: body, meta: azHeaderMeta(r.Header)}
	f.created(w)
}

func (f *fakeAzBlob) created(w http.ResponseWriter) {
	w.Header().Set("ETag", `"fake"`)
	w.Header().Set("Last-Modified", "Wed, 06 Aug 2026 12:00:00 GMT")
	w.WriteHeader(http.StatusCreated)
}

// writeMeta title-cases every metadata key on the way out, which is what Azure
// itself does: a blob written with `toft_sha256` is read back as
// `Toft_sha256`.
func (f *fakeAzBlob) writeMeta(w http.ResponseWriter, obj fakeObject) {
	for k, v := range obj.meta {
		w.Header().Set("x-ms-meta-"+strings.ToUpper(k[:1])+k[1:], v)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.body)))
	w.Header().Set("Last-Modified", "Wed, 06 Aug 2026 12:00:00 GMT")
	w.Header().Set("ETag", `"fake"`)
	w.Header().Set("x-ms-blob-type", "BlockBlob")
}

func (f *fakeAzBlob) blobNotFound(w http.ResponseWriter) {
	w.Header().Set("x-ms-error-code", "BlobNotFound")
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
		Message string
	}{Code: "BlobNotFound", Message: "The specified blob does not exist."})
}

func (f *fakeAzBlob) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")

	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	type properties struct {
		ContentLength int64 `xml:"Content-Length"`
	}
	type blob struct {
		XMLName    xml.Name `xml:"Blob"`
		Name       string
		Properties properties
	}
	resp := struct {
		XMLName    xml.Name `xml:"EnumerationResults"`
		Blobs      []blob   `xml:"Blobs>Blob"`
		NextMarker string
	}{}
	for _, k := range keys {
		resp.Blobs = append(resp.Blobs, blob{
			Name:       k,
			Properties: properties{ContentLength: int64(len(f.objects[k].body))},
		})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(resp)
}

func azHeaderMeta(h http.Header) map[string]string {
	meta := map[string]string{}
	const prefix = "X-Ms-Meta-"
	for name, vals := range h {
		if canon := http.CanonicalHeaderKey(name); strings.HasPrefix(canon, prefix) && len(vals) > 0 {
			meta[strings.ToLower(strings.TrimPrefix(canon, prefix))] = vals[0]
		}
	}
	return meta
}

// fakeAzConnectionString points the SDK at the test server with a shared key
// the fake never checks. This is the one place a key is used at all: outside a
// test and Azurite, the ambient identity chain is the whole of the credential.
func fakeAzConnectionString(url string) string {
	key := base64.StdEncoding.EncodeToString([]byte("toft-test-account-key-000000"))
	return fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=%s;BlobEndpoint=%s/devstoreaccount1;",
		key, url)
}

func newTestAzBlobStore(t *testing.T, srv *httptest.Server, prefix string) *AzBlobStore {
	t.Helper()
	// One attempt. The retry ladder is right for an install and is eight
	// seconds of backoff in a test that is deliberately talking to a dead
	// server — and what that test is proving is the error mapping, not the
	// number of tries.
	// Negative, not zero: the SDK reads zero as "unset" and applies its default
	// of three, which is the trap this comment exists to keep somebody from
	// walking back into.
	prev := azMaxRetries
	azMaxRetries = -1
	t.Cleanup(func() { azMaxRetries = prev })

	store, err := NewAzBlobStore(AzBlobConfig{
		Container:        "toft-artifacts",
		Prefix:           prefix,
		ConnectionString: fakeAzConnectionString(srv.URL),
	})
	if err != nil {
		t.Fatalf("new azblob store: %v", err)
	}
	return store
}

func TestAzBlobRoundTrip(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "install-a")
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

	dest := filepath.Join(t.TempDir(), "restored")
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatalf("get: %v", err)
	}
	got := readTree(t, dest)
	if got["app.py"] != "print('hi')" || got["venv/lib/site/mod.py"] != "x = 1" {
		t.Errorf("restored tree wrong: %v", got)
	}
}

// The digest must survive the round trip as blob metadata, *through Azure's
// title-casing*. Without it Get has nothing to verify against and ErrIntegrity
// becomes unreachable — the failure would then surface as a mysteriously broken
// app instead of a refused download.
func TestAzBlobStoresTheDigestAsMetadata(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "")
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")

	desc, err := store.Put(ctx, key, writeTree(t, map[string]string{"a": "b"}))
	if err != nil {
		t.Fatal(err)
	}

	stat, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.SHA256 != desc.SHA256 {
		t.Errorf("digest did not survive: stored %q, read back %q", desc.SHA256, stat.SHA256)
	}
	if stat.Size != desc.Size {
		t.Errorf("size = %d, want %d", stat.Size, desc.Size)
	}
}

func TestAzBlobCorruptObjectIsRefused(t *testing.T) {
	fake := newFakeAzBlob()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "")
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")

	if _, err := store.Put(ctx, key, writeTree(t, map[string]string{"a.py": strings.Repeat("x", 5000)})); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	obj := fake.objects[key]
	obj.body[len(obj.body)/2] ^= 0xff
	fake.objects[key] = obj
	fake.mu.Unlock()

	err := store.Get(ctx, key, filepath.Join(t.TempDir(), "d"))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
}

func TestAzBlobMissingKeyIsNotFound(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "")
	ctx := context.Background()

	if err := store.Get(ctx, ReleaseKey("app-1", "nope"), t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
	if _, err := store.Stat(ctx, ReleaseKey("app-1", "nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat err = %v, want ErrNotFound", err)
	}
}

// The distinction that matters most, and the reason mapAzError's not-found list
// is short and explicit: an outage must not read as "the release is gone", or
// the reconciler would conclude every app needed a redeploy at the exact moment
// the store came back.
func TestAzBlobOutageIsUnreachableNotNotFound(t *testing.T) {
	fake := newFakeAzBlob()
	fake.failWith = http.StatusInternalServerError
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "")
	err := store.Get(context.Background(), ReleaseKey("app-1", "deploy-1"), t.TempDir())

	if errors.Is(err, ErrNotFound) {
		t.Fatalf("an outage must never read as a missing release: %v", err)
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

// Deleting an absent blob is not an error. Azure, unlike S3, says 404 — so
// pruning would fail on its second run without the carve-out in Delete.
func TestAzBlobDeleteIsIdempotent(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "")
	if err := store.Delete(context.Background(), ReleaseKey("app-1", "never-existed")); err != nil {
		t.Fatalf("deleting an absent blob: %v", err)
	}
}

func TestAzBlobExpireKeepsExactlyTheNamedReleases(t *testing.T) {
	srv := httptest.NewServer(newFakeAzBlob())
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "install-a")
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

// The install prefix must not leak into the keys a caller sees, or Expire's
// keep set would never match what Put returned.
func TestAzBlobPrefixIsInternal(t *testing.T) {
	fake := newFakeAzBlob()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestAzBlobStore(t, srv, "install-a")
	key := ReleaseKey("app-1", "deploy-1")

	desc, err := store.Put(context.Background(), key, writeTree(t, map[string]string{"a": "b"}))
	if err != nil {
		t.Fatal(err)
	}
	if desc.Key != key {
		t.Errorf("descriptor key = %q, want the caller's %q", desc.Key, key)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.objects["install-a/"+key]; !ok {
		var stored []string
		for k := range fake.objects {
			stored = append(stored, k)
		}
		t.Errorf("blob not stored under the install prefix: %v", stored)
	}
}

func TestAzBlobStoreNeedsAContainerAndAnAccount(t *testing.T) {
	if _, err := NewAzBlobStore(AzBlobConfig{}); err == nil {
		t.Error("expected an error with no container configured")
	}
	if _, err := NewAzBlobStore(AzBlobConfig{Container: "c"}); err == nil {
		t.Error("expected an error with no storage account and no connection string")
	}
}
