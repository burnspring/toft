package api

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"toft/internal/database"
	"toft/internal/hoststore"
	"toft/internal/runtime"
)

// hostStoreEnv is an install with a host root on a temp directory and the host
// data plane on, and the router built after it — the data route is mounted
// only where the plane exists.
func hostStoreEnv(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	roots := []runtime.StorageRoot{{Name: "data", Path: dir}, {Name: "frozen", Path: dir, ReadOnly: true}}
	srv, _, _ := storageTestEnv(t, &rootProvisioner{
		backingProvisioner: backingProvisioner{
			fakeProvisioner: fakeProvisioner{kinds: []string{runtime.MountVolume}},
			backings:        map[string][]string{runtime.MountVolume: {runtime.BackingHost}},
		},
		roots: roots,
	})
	srv.cfg.AppUserPerApp = true
	srv.hostStore = hoststore.New(hoststore.DeriveKey([]byte("test")), srv.cfg.BaseURL, roots, nil)
	return srv, srv.Router(http.NotFoundHandler()), dir
}

// hostResource creates a resource under a root and grants root `full` on it.
// Its gate is moved to this process's own group, the one gid an unprivileged
// test may chown a file to.
func hostResource(t *testing.T, srv *Server, router http.Handler, dir, body string) database.StorageResource {
	t.Helper()
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if err := os.MkdirAll(filepath.Join(dir, res.Slug), 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Conn().Exec(`UPDATE storage_gates SET gid = ? WHERE resource_id = ?`, os.Getgid(), res.ID); err != nil {
		t.Fatal(err)
	}
	return res
}

func grant(t *testing.T, srv *Server, router http.Handler, res database.StorageResource, access string) *httptest.ResponseRecorder {
	t.Helper()
	return storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+res.ID+"/grants", "root",
		fmt.Sprintf(`{"subject_type":"user","subject_id":"root","access":%q}`, access))
}

type signed struct {
	URL, Method string
	Headers     map[string]string
}

// follow sends the signed request the way the CLI does: to the URL it was
// given, with no cookie and no key.
func follow(t *testing.T, router http.Handler, s signed, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(s.Method, u.Path, bytes.NewReader(body))
	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func mintUpload(t *testing.T, srv *Server, router http.Handler, slug, name string, body []byte) (*httptest.ResponseRecorder, signed) {
	t.Helper()
	m, s := md5.Sum(body), sha256.Sum256(body)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/store/"+slug+"/objects", "root",
		fmt.Sprintf(`{"name":%q,"size":%d,"sha256":%q,"md5":%q}`, name, len(body),
			hex.EncodeToString(s[:]), hex.EncodeToString(m[:])))
	var out signed
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// A host-root resource is reached the way a bucket is: grant, mint, follow the
// URL. The bytes land in the resource's directory as the gate's, list back,
// and download — and the URL is the platform's own, at /store-data/.
func TestAHostRootResourceIsReachedLikeABucket(t *testing.T) {
	srv, router, dir := hostStoreEnv(t)
	res := hostResource(t, srv, router, dir, `{"name":"share","kind":"volume","backing":"host","storage_root":"data"}`)
	if rec := grant(t, srv, router, res, "full"); rec.Code != http.StatusOK {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body.String())
	}

	rec, up := mintUpload(t, srv, router, res.Slug, "reports/q1.csv", []byte("a,b\n"))
	if rec.Code != http.StatusOK || !strings.HasPrefix(up.URL, srv.cfg.BaseURL+hoststore.Route) {
		t.Fatalf("mint = %d %s", rec.Code, rec.Body.String())
	}
	if rec := follow(t, router, up, []byte("a,b\n")); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
	}
	info, err := os.Stat(filepath.Join(dir, res.Slug, "reports", "q1.csv"))
	if err != nil {
		t.Fatalf("the file did not land: %v", err)
	}
	if gid := int(info.Sys().(*syscall.Stat_t).Gid); gid != os.Getgid() || info.Mode().Perm() != 0o660 {
		t.Errorf("landed as gid %d mode %v, want the gate's at 0660", gid, info.Mode().Perm())
	}

	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/store/"+res.Slug+"/objects?path=reports/", "root", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"q1.csv"`) {
		t.Errorf("list = %d %s", rec.Code, rec.Body.String())
	}

	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/store/"+res.Slug+"/objects/reports/q1.csv", "root", "")
	var down signed
	_ = json.Unmarshal(rec.Body.Bytes(), &down)
	if got := follow(t, router, down, nil); got.Code != http.StatusOK || got.Body.String() != "a,b\n" {
		t.Errorf("download = %d %q", got.Code, got.Body.String())
	}

	// The token is the credential: the same path with a cookie and an edited
	// token authorizes nothing.
	bad := down
	bad.URL = strings.Replace(down.URL, hoststore.Route, hoststore.Route+"x", 1)
	if got := follow(t, router, bad, nil); got.Code != http.StatusForbidden {
		t.Errorf("an edited token = %d, want 403", got.Code)
	}
}

// On a partitioned resource a write lands in a mapped folder or nowhere: a
// file beside the folders would be root's, and no app's to open.
func TestAPartitionedResourceTakesWritesOnlyInsideAFolder(t *testing.T) {
	srv, router, dir := hostStoreEnv(t)
	res := hostResource(t, srv, router, dir,
		`{"name":"teams","kind":"volume","backing":"host","storage_root":"data","partitioned":true}`)
	app := &database.App{Slug: "sales", Name: "Sales", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if rec := storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID,
		"root", `{"path":"/data/sales","access":"full","folder":"teams/sales"}`); rec.Code != http.StatusOK {
		t.Fatalf("map = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := srv.db.Conn().Exec(`UPDATE storage_gates SET gid = ? WHERE resource_id = ?`, os.Getgid(), res.ID); err != nil {
		t.Fatal(err)
	}
	grant(t, srv, router, res, "full")

	if rec, _ := mintUpload(t, srv, router, res.Slug, "loose.txt", []byte("x")); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), `"folders":["teams/sales"]`) {
		t.Errorf("a write beside the folders = %d %s, want 403 naming the mapped folder", rec.Code, rec.Body.String())
	}
	rec, up := mintUpload(t, srv, router, res.Slug, "teams/sales/in.txt", []byte("x"))
	if rec.Code != http.StatusOK {
		t.Fatalf("a write into the folder = %d %s", rec.Code, rec.Body.String())
	}
	if got := follow(t, router, up, []byte("x")); got.Code != http.StatusOK {
		t.Errorf("upload = %d %s", got.Code, got.Body.String())
	}
}

// A grant above read under a read-only mount is refused at the grant, and with
// the data plane absent a host-root grant is refused naming why.
func TestAHostGrantIsRefusedWhereItCouldNeverWork(t *testing.T) {
	srv, router, dir := hostStoreEnv(t)
	frozen := hostResource(t, srv, router, dir, `{"name":"frozen","kind":"volume","backing":"host","storage_root":"frozen"}`)
	if rec := grant(t, srv, router, frozen, "write"); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "read-only") {
		t.Errorf("write on a read-only root = %d %s, want 422", rec.Code, rec.Body.String())
	}
	if rec := grant(t, srv, router, frozen, "read"); rec.Code != http.StatusOK {
		t.Errorf("read on a read-only root = %d %s", rec.Code, rec.Body.String())
	}

	srv.hostStore, srv.hostStoreAbsent = nil, "the kernel lets apps hard-link"
	router = srv.Router(http.NotFoundHandler())
	if rec := grant(t, srv, router, frozen, "read"); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "hard-link") {
		t.Errorf("a grant with no data plane = %d %s, want 422 naming why", rec.Code, rec.Body.String())
	}
}
