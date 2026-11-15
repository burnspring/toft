package artifact

import (
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is a small S3-compatible server: enough of the protocol to exercise
// the real SDK client end to end. There is no local emulator for the isolated
// backends, but the artifact store's wire behaviour *can* be pinned here, and
// it is the part most likely to be wrong — metadata round-tripping, 404
// mapping, and pagination.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	// failWith, when set, is returned for every request — used to prove an
	// outage does not read as a missing release.
	failWith int
	// uploads are multipart uploads in progress, by id; calls names every
	// multipart call in order, for the test that asserts the shape.
	uploads map[string]*fakeUpload
	calls   []string
}

type fakeUpload struct {
	key   string
	meta  map[string]string
	parts map[int][]byte
}

type fakeObject struct {
	body []byte
	meta map[string]string
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string]fakeObject{}} }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failWith != 0 {
		http.Error(w, "backend unavailable", f.failWith)
		return
	}

	// Path-style addressing: /{bucket}/{key...}
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}

	if f.multipart(w, r, key) {
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		meta := map[string]string{}
		for name, vals := range r.Header {
			const prefix = "X-Amz-Meta-"
			if strings.HasPrefix(http.CanonicalHeaderKey(name), prefix) && len(vals) > 0 {
				meta[strings.ToLower(strings.TrimPrefix(http.CanonicalHeaderKey(name), prefix))] = vals[0]
			}
		}
		f.objects[key] = fakeObject{body: body, meta: meta}
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)
			return
		}
		obj, ok := f.objects[key]
		if !ok {
			f.notFound(w)
			return
		}
		f.writeMeta(w, obj)
		w.WriteHeader(http.StatusOK)
		w.Write(obj.body)

	case http.MethodHead:
		obj, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.writeMeta(w, obj)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// multipart answers the four multipart calls, and reports whether r was one.
func (f *fakeS3) multipart(w http.ResponseWriter, r *http.Request, key string) bool {
	q := r.URL.Query()
	_, create := q["uploads"]
	id := q.Get("uploadId")
	if !create && id == "" {
		return false
	}
	if f.uploads == nil {
		f.uploads = map[string]*fakeUpload{}
	}
	w.Header().Set("Content-Type", "application/xml")
	switch {
	case create && r.Method == http.MethodPost:
		meta := map[string]string{}
		for name, vals := range r.Header {
			const prefix = "X-Amz-Meta-"
			if strings.HasPrefix(http.CanonicalHeaderKey(name), prefix) && len(vals) > 0 {
				meta[strings.ToLower(strings.TrimPrefix(http.CanonicalHeaderKey(name), prefix))] = vals[0]
			}
		}
		id = fmt.Sprintf("upload-%d", len(f.uploads)+1)
		f.uploads[id] = &fakeUpload{key: key, meta: meta, parts: map[int][]byte{}}
		f.calls = append(f.calls, "create")
		xml.NewEncoder(w).Encode(struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			Bucket   string
			Key      string
			UploadId string
		}{Bucket: "toft-artifacts", Key: key, UploadId: id})
	case r.Method == http.MethodPut:
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		body, _ := io.ReadAll(r.Body)
		f.uploads[id].parts[n] = body
		f.calls = append(f.calls, fmt.Sprintf("part %d", n))
		w.Header().Set("ETag", fmt.Sprintf(`"part-%d"`, n))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost:
		u := f.uploads[id]
		var body []byte
		for i := 1; i <= len(u.parts); i++ {
			body = append(body, u.parts[i]...)
		}
		f.objects[u.key] = fakeObject{body: body, meta: u.meta}
		delete(f.uploads, id)
		f.calls = append(f.calls, "complete")
		xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
			Key     string
			ETag    string
		}{Key: u.key, ETag: `"fake-mp"`})
	case r.Method == http.MethodDelete:
		delete(f.uploads, id)
		f.calls = append(f.calls, "abort")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	return true
}

func (f *fakeS3) writeMeta(w http.ResponseWriter, obj fakeObject) {
	for k, v := range obj.meta {
		w.Header().Set("x-amz-meta-"+k, v)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.body)))
	w.Header().Set("Last-Modified", "Wed, 06 Aug 2026 12:00:00 GMT")
	w.Header().Set("ETag", `"fake"`)
}

func (f *fakeS3) notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
		Message string
	}{Code: "NoSuchKey", Message: "The specified key does not exist."})
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	// A delimiter rolls every key sharing a segment up into one CommonPrefix and
	// drops it from Contents, which is the behaviour a folder view is built on.
	// The fake implements it because the alternative is proving the browser's
	// only listing mode against MinIO alone.
	delimiter := r.URL.Query().Get("delimiter")

	var keys []string
	seen := map[string]bool{}
	var prefixes []string
	for k := range f.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				cp := k[:len(prefix)+i+len(delimiter)]
				if !seen[cp] {
					seen[cp] = true
					prefixes = append(prefixes, cp)
				}
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(prefixes)

	type content struct {
		Key  string
		Size int64
	}
	type commonPrefix struct {
		Prefix string
	}
	resp := struct {
		XMLName        xml.Name `xml:"ListBucketResult"`
		IsTruncated    bool
		Contents       []content
		CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
	}{}
	for _, k := range keys {
		resp.Contents = append(resp.Contents, content{Key: k, Size: int64(len(f.objects[k].body))})
	}
	for _, cp := range prefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, commonPrefix{Prefix: cp})
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(resp)
}

func newTestS3Store(t *testing.T, srv *httptest.Server, prefix string) *S3Store {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	// Keep the SDK from probing IMDS or a shared config file on the test host.
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))

	store, err := NewS3Store(context.Background(), S3Config{
		Bucket:       "toft-artifacts",
		Prefix:       prefix,
		Endpoint:     srv.URL,
		Region:       "us-east-1",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("new s3 store: %v", err)
	}
	return store
}

func TestS3RoundTrip(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestS3Store(t, srv, "install-a")
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

// The digest must survive the round trip as object metadata. Without it Get
// has nothing to verify against and ErrIntegrity becomes unreachable — the
// failure would then surface as a mysteriously broken app instead.
func TestS3StoresTheDigestAsMetadata(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestS3Store(t, srv, "")
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

// Corruption in transit must be caught by the digest rather than unpacked.
func TestS3CorruptObjectIsRefused(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestS3Store(t, srv, "")
	ctx := context.Background()
	key := ReleaseKey("app-1", "deploy-1")

	if _, err := store.Put(ctx, key, writeTree(t, map[string]string{"a.py": strings.Repeat("x", 5000)})); err != nil {
		t.Fatal(err)
	}

	// Corrupt the stored bytes without touching the recorded digest.
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

func TestS3MissingKeyIsNotFound(t *testing.T) {
	srv := httptest.NewServer(newFakeS3())
	defer srv.Close()

	store := newTestS3Store(t, srv, "")
	ctx := context.Background()

	if err := store.Get(ctx, ReleaseKey("app-1", "nope"), t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
	if _, err := store.Stat(ctx, ReleaseKey("app-1", "nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat err = %v, want ErrNotFound", err)
	}
}

// The distinction that matters most: an outage must not read as "the release
// is gone". If it did, the reconciler would conclude every app needed a
// redeploy at the exact moment the store came back.
func TestS3OutageIsUnreachableNotNotFound(t *testing.T) {
	fake := newFakeS3()
	fake.failWith = http.StatusInternalServerError
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestS3Store(t, srv, "")
	err := store.Get(context.Background(), ReleaseKey("app-1", "deploy-1"), t.TempDir())

	if errors.Is(err, ErrNotFound) {
		t.Fatalf("an outage must never read as a missing release: %v", err)
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

func TestS3ExpireKeepsExactlyTheNamedReleases(t *testing.T) {
	srv := httptest.NewServer(newFakeS3())
	defer srv.Close()

	store := newTestS3Store(t, srv, "install-a")
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
func TestS3PrefixIsInternal(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	store := newTestS3Store(t, srv, "install-a")
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
		t.Errorf("object not stored under the install prefix: %v", stored)
	}
}

func TestS3StoreNeedsABucket(t *testing.T) {
	if _, err := NewS3Store(context.Background(), S3Config{}); err == nil {
		t.Error("expected an error with no bucket configured")
	}
}

// A staged release past the single-PUT threshold goes up as a multipart
// upload — S3 refuses one PutObject over 5 GiB, and a release may unpack to 8
// GiB — and the completed object carries the digest as a single PUT's does
// (AS-14).
func TestS3UploadPastTheThresholdIsMultipart(t *testing.T) {
	wasT, wasP := s3MultipartThreshold, s3PartSize
	s3MultipartThreshold, s3PartSize = 6<<20, 5<<20
	t.Cleanup(func() { s3MultipartThreshold, s3PartSize = wasT, wasP })

	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	store := newTestS3Store(t, srv, "install-a")
	ctx := context.Background()

	// Random, so gzip cannot shrink it under the threshold.
	src := t.TempDir()
	noise := make([]byte, 12<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "blob.bin"), noise, 0o644); err != nil {
		t.Fatal(err)
	}
	staged, err := store.Stage(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Remove()
	key := ReleaseKey("app", "dep")
	if _, err := store.Upload(ctx, key, staged); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if got := strings.Join(fake.calls, ","); got != "create,part 1,part 2,part 3,complete" {
		t.Errorf("multipart calls = %q", got)
	}
	desc, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if desc.SHA256 != staged.SHA256 || desc.Size != staged.Size {
		t.Errorf("Stat = %+v, want digest %s and size %d", desc, staged.SHA256, staged.Size)
	}
	dest := t.TempDir()
	if err := store.Get(ctx, key, dest); err != nil {
		t.Fatalf("Get of the multipart object: %v", err)
	}
}
