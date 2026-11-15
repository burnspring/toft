package toft

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A store reached over the platform's signed URLs. One fake answers both
// halves — the mint and the object store — so what reaches the object store is
// recorded too: the signed headers verbatim, and no Toft token.
type storeFake struct {
	mu   sync.Mutex
	seen []*http.Request
	body map[string][]byte
	put  int
	get  int
}

func newStoreFake(t *testing.T) (*storeFake, *httptest.Server) {
	t.Helper()
	f := &storeFake{body: map[string][]byte{}, put: http.StatusOK}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r)
		f.body[r.Method+" "+r.URL.Path] = data
		f.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/link/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"link": "https://toft.example/files" + strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1/app/store")})
		case strings.HasPrefix(r.URL.Path, "/api/v1/app/store/"):
			method := map[string]string{http.MethodPost: http.MethodPut, http.MethodDelete: http.MethodDelete}[r.Method]
			if method == "" {
				method = http.MethodGet
			}
			headers := map[string]string{}
			if r.Method == http.MethodPost {
				headers["If-None-Match"] = "*"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"url": srv.URL + "/bucket/obj", "method": method, "headers": headers})
		case r.URL.Path == "/bucket/obj" && r.Method == http.MethodPut:
			w.WriteHeader(f.put)
		case r.URL.Path == "/bucket/obj" && f.get != 0:
			w.WriteHeader(f.get)
		case r.URL.Path == "/bucket/obj":
			_, _ = w.Write([]byte("the bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	SetTransport(srv.URL, "tofta_test")
	t.Cleanup(func() { SetTransport("", "") })
	return f, srv
}

func unmountedStore() Mount {
	return Mount{Name: "uploads", Kind: "store", Access: "write", Folder: "in"}
}

func TestAnUnmountedStoreRefusesPut(t *testing.T) {
	m := unmountedStore()
	if m.Mounted() {
		t.Fatal("a store with no path reported mounted")
	}
	src := filepath.Join(t.TempDir(), "a.csv")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	if _, err := m.Put(src, ""); err == nil || !strings.Contains(err.Error(), "Upload") {
		t.Errorf("Put = %v, want a refusal naming Upload", err)
	}
}

func TestUploadSignsThenSendsWithoutTheToken(t *testing.T) {
	f, _ := newStoreFake(t)
	src := filepath.Join(t.TempDir(), "report.csv")
	_ = os.WriteFile(src, []byte("a,b\n"), 0o644)

	name, err := unmountedStore().Upload(context.Background(), src, "")
	if err != nil || name != "report.csv" {
		t.Fatalf("Upload = %q, %v", name, err)
	}
	mint, put := f.seen[0], f.seen[1]
	if mint.URL.Query().Get("folder") != "in" || !mint.URL.Query().Has("folder") {
		t.Errorf("mint query %q does not name the folder", mint.URL.RawQuery)
	}
	var sent map[string]any
	_ = json.Unmarshal(f.body["POST /api/v1/app/store/uploads/objects"], &sent)
	sum := sha256.Sum256([]byte("a,b\n"))
	if sent["sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %v", sent["sha256"])
	}
	if put.Header.Get("If-None-Match") != "*" || put.Header.Get("Authorization") != "" {
		t.Errorf("PUT headers %v: want the signed header and no token", put.Header)
	}
	if string(f.body["PUT /bucket/obj"]) != "a,b\n" {
		t.Errorf("PUT body %q", f.body["PUT /bucket/obj"])
	}
}

func TestADownloadOfAMissingObjectIsNotFound(t *testing.T) {
	f, _ := newStoreFake(t)
	f.get = http.StatusNotFound
	_, err := unmountedStore().Download(context.Background(), "sales.db")
	var e *Error
	if !errors.As(err, &e) || e.Code() != "not_found" || e.Retryable() ||
		!strings.Contains(e.Error(), `"sales.db"`) {
		t.Errorf("Download of a missing object = %v", err)
	}
}

func TestWriteOntoAnExistingNameSaysSo(t *testing.T) {
	f, _ := newStoreFake(t)
	f.put = http.StatusPreconditionFailed
	src := filepath.Join(t.TempDir(), "report.csv")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	if _, err := unmountedStore().Upload(context.Background(), src, ""); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("Upload onto an existing name = %v", err)
	}
	// Started at `full` and lowered since: the store signs at today's level, so
	// the refusal names `write` and never the level the app started with.
	started := unmountedStore()
	started.Access = "full"
	if _, err := started.Upload(context.Background(), src, ""); err == nil ||
		!strings.Contains(err.Error(), "`write`") || strings.Contains(err.Error(), "full") {
		t.Errorf("Upload onto an existing name after a lowered mapping = %v", err)
	}
}

func TestURLAndDownloadAreSignedGets(t *testing.T) {
	f, srv := newStoreFake(t)
	link, err := unmountedStore().URL(context.Background(), "2026/report.csv", true)
	if err != nil || link != srv.URL+"/bucket/obj" {
		t.Fatalf("URL = %q, %v", link, err)
	}
	if got := f.seen[0]; got.URL.Path != "/api/v1/app/store/uploads/objects/2026/report.csv" ||
		got.URL.Query().Get("disposition") != "attachment" {
		t.Errorf("mint = %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	data, err := unmountedStore().Download(context.Background(), "2026/report.csv")
	if err != nil || string(data) != "the bytes" {
		t.Errorf("Download = %q, %v", data, err)
	}
}

func TestLinkAsksThePlatformAndCarriesTheFolder(t *testing.T) {
	f, _ := newStoreFake(t)
	link, err := unmountedStore().Link(context.Background(), "2026/q1 report.csv")
	if err != nil || link != "https://toft.example/files/uploads/link/2026/q1%20report.csv" {
		t.Fatalf("Link = %q, %v", link, err)
	}
	if got := f.seen[0]; got.URL.Query().Get("folder") != "in" || got.Method != http.MethodGet {
		t.Errorf("link request = %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	for _, name := range []string{"../escape", "/abs", "a//b"} {
		if _, err := unmountedStore().Link(context.Background(), name); err == nil {
			t.Errorf("Link(%q) was not refused", name)
		}
	}
	if len(f.seen) != 1 {
		t.Errorf("a refused name reached the platform: %d requests", len(f.seen))
	}
}

func TestANameCannotLeaveTheStore(t *testing.T) {
	f, _ := newStoreFake(t)
	for _, name := range []string{"../escape", "/abs", "a//b"} {
		if _, err := unmountedStore().URL(context.Background(), name, false); err == nil {
			t.Errorf("URL(%q) was signed", name)
		}
	}
	vol := Mount{Name: "cache", Kind: "volume", Path: "/data/cache"}
	if _, err := vol.List(context.Background(), "", ""); err == nil {
		t.Error("a volume was listed over URLs")
	}
	if len(f.seen) != 0 {
		t.Errorf("%d requests reached the platform", len(f.seen))
	}
}

// A resource mapped at two folders is named once in not_mapped's list — it read
// "Mapped here: drills-drop, drills-drop, …" (drills on trytoft, 2026-10-23).
func TestNotMappedNamesEachResourceOnce(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"drop","kind":"store","path":"","access":"read"},`+
		`{"name":"drop","kind":"store","path":"","access":"write","folder":"backup"},`+
		`{"name":"cache","kind":"volume","path":"/data/cache","access":"full"}]`)
	_, err := Storage("nope")
	if err == nil || !strings.HasSuffix(err.Error(), "Mapped to this app: cache, drop") {
		t.Errorf("err = %v, want each name once", err)
	}
}

// Store("x") was told to call toft.StorageIn, which is not what a store wants
// (drills D17): the refusal names the accessor the caller used.
func TestAmbiguousMountNamesTheAccessorUsed(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"drop","kind":"store","path":"","access":"read"},`+
		`{"name":"drop","kind":"store","path":"","access":"write","folder":"backup"}]`)
	if _, err := Store("drop"); err == nil || !strings.Contains(err.Error(), `toft.StoreIn("drop", `) {
		t.Errorf("Store's refusal = %v, want it to name toft.StoreIn", err)
	}
	if _, err := Storage("drop"); err == nil || !strings.Contains(err.Error(), `toft.StorageIn("drop", `) {
		t.Errorf("Storage's refusal = %v, want it to name toft.StorageIn", err)
	}
}

// The SDK's own refusals carry a code and are not retryable — a wrong name or a
// missing mapping is not a blip, and a statusless error would read as one.
func TestSDKRefusalsAreCoded(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"drop","kind":"store","path":"","access":"read"},`+
		`{"name":"drop","kind":"store","path":"","access":"write","folder":"backup"}]`)
	for code, call := range map[string]func() error{
		"ambiguous_mount":     func() error { _, err := Storage("drop"); return err },
		"not_mapped":          func() error { _, err := Storage("nope"); return err },
		"not_mapped (folder)": func() error { _, err := StoreIn("drop", "elsewhere"); return err },
		"invalid_name":        func() error { _, err := Storage(""); return err },
		"invalid_name (path)": func() error { m, _ := StoreIn("drop", ""); _, err := m.Put("/etc/hostname", "../x"); return err },
		"not_mounted":         func() error { m, _ := StoreIn("drop", ""); _, err := m.Put("/etc/hostname", "x"); return err },
	} {
		want, _, _ := strings.Cut(code, " ")
		var e *Error
		if err := call(); !errors.As(err, &e) || e.Code() != want || e.Retryable() {
			t.Errorf("%s: got %#v", code, err)
			continue
		}
		// Each one has a next step, so it names one (drills 2026-10-28 #10).
		if e.Hint() == "" {
			t.Errorf("%s: no hint on %q", code, e.Error())
		}
	}
	if _, err := Store("drop"); err == nil || !strings.Contains(err.(*Error).Hint(), `toft.StoreIn("drop", `) {
		t.Errorf("ambiguous_mount's hint = %v, want it to name toft.StoreIn", err)
	}
}
