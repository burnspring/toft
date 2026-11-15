package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// rootsEnv is a server whose storage declares the given roots, with one host
// resource under each named root, its directory made where `made` says.
func rootsEnv(t *testing.T, roots []runtime.StorageRoot, resources map[string]string, made bool) *Server {
	t.Helper()
	srv, _, _ := storageTestEnv(t, runtime.NewSharedStorage(roots, t.TempDir(), ""))
	for slug, root := range resources {
		res := &database.StorageResource{Slug: slug, Kind: database.StorageKindVolume,
			Backing: database.StorageBackingHost, Root: root}
		if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
			t.Fatalf("CreateStorageResource: %v", err)
		}
		if !made {
			continue
		}
		for _, r := range roots {
			if r.Name == root {
				if err := os.MkdirAll(filepath.Join(r.Path, slug), 0o755); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return srv
}

func storageRootsRow(t *testing.T, srv *Server) systemCheck {
	t.Helper()
	rp := &checkReport{}
	srv.checkStorageRoots(rp)
	if len(rp.checks) != 1 {
		t.Fatalf("expected one row, got %d", len(rp.checks))
	}
	return rp.checks[0]
}

func withMountPoint(t *testing.T, answer bool) {
	t.Helper()
	was := isMountPoint
	isMountPoint = func(string) bool { return answer }
	t.Cleanup(func() { isMountPoint = was })
}

// The bug this replaced: the root's *name* was stat'ed as a path, so a healthy
// install with storage failed this row every time.
func TestStorageRootsResolvesTheRootByName(t *testing.T) {
	withMountPoint(t, true)
	dir := t.TempDir()
	srv := rootsEnv(t, []runtime.StorageRoot{{Name: "efs", Path: dir}},
		map[string]string{"sales-data": "efs"}, true)

	row := storageRootsRow(t, srv)
	if row.Status != checkPass {
		t.Fatalf("a mounted, present root = %s (%s), want pass", row.Status, row.Detail)
	}
	if _, err := os.Stat(filepath.Join(dir, ".toft-check")); !os.IsNotExist(err) {
		t.Error("the write probe was left behind in the root")
	}
}

func TestStorageRootsFailsWhenAResourceDirectoryIsGone(t *testing.T) {
	withMountPoint(t, true)
	srv := rootsEnv(t, []runtime.StorageRoot{{Name: "efs", Path: t.TempDir()}},
		map[string]string{"sales-data": "efs"}, false)

	row := storageRootsRow(t, srv)
	if row.Status != checkFail || !strings.Contains(row.Detail, "sales-data") {
		t.Errorf("a missing resource directory = %s (%s), want fail naming it", row.Status, row.Detail)
	}
}

func TestStorageRootsFailsWhenARootIsNoLongerDeclared(t *testing.T) {
	withMountPoint(t, true)
	srv := rootsEnv(t, []runtime.StorageRoot{{Name: "efs", Path: t.TempDir()}},
		map[string]string{"sales-data": "efs"}, true)
	// The operator dropped it from SHARED_STORAGE_ROOTS after the resource was made.
	srv.storage = runtime.NewSharedStorage([]runtime.StorageRoot{{Name: "other", Path: t.TempDir()}}, t.TempDir(), "")

	row := storageRootsRow(t, srv)
	if row.Status != checkFail || !strings.Contains(row.Detail, "SHARED_STORAGE_ROOTS") {
		t.Errorf("an undeclared root = %s (%s), want fail naming the variable", row.Status, row.Detail)
	}
}

// An empty directory where a mount should be passes stat and write, and is the
// failure the row exists for. A warn: a plain directory is a legitimate root.
func TestStorageRootsWarnsWhenNothingIsMounted(t *testing.T) {
	withMountPoint(t, false)
	srv := rootsEnv(t, []runtime.StorageRoot{{Name: "efs", Path: t.TempDir()}},
		map[string]string{"sales-data": "efs"}, true)

	row := storageRootsRow(t, srv)
	if row.Status != checkWarn || !strings.Contains(row.Detail, "Nothing is mounted") {
		t.Errorf("an unmounted root = %s (%s), want warn", row.Status, row.Detail)
	}
}

// A `:ro` root refuses a write by design, so it is never write-probed.
func TestStorageRootsDoesNotWriteToAReadOnlyRoot(t *testing.T) {
	withMountPoint(t, true)
	dir := t.TempDir()
	srv := rootsEnv(t, []runtime.StorageRoot{{Name: "archive", Path: dir, ReadOnly: true}},
		map[string]string{"old-reports": "archive"}, true)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	row := storageRootsRow(t, srv)
	if row.Status != checkPass {
		t.Errorf("a read-only root = %s (%s), want pass", row.Status, row.Detail)
	}
}

// The real device test: a temp directory is not a mount point, and the
// file system root always is.
func TestIsMountPoint(t *testing.T) {
	if !isMountPoint("/") {
		t.Error("/ should be a mount point")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "plain")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if isMountPoint(sub) {
		t.Error("a plain subdirectory reported as a mount point")
	}
}

// Deleting a resource whose root is no longer declared removes the row and
// cannot remove the directory, and the answer says so rather than claiming the
// files are gone.
func TestDeletingOnAnUndeclaredRootSaysWhatItLeft(t *testing.T) {
	dir := t.TempDir()
	srv := rootsEnv(t, []runtime.StorageRoot{{Name: "efs", Path: dir}},
		map[string]string{"sales-data": "efs"}, true)
	all, err := srv.db.ListStorageResources()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListStorageResources: %v %v", all, err)
	}
	res := all[0]
	srv.storage = runtime.NewSharedStorage([]runtime.StorageRoot{{Name: "oms", Path: dir}}, t.TempDir(), "")

	router := srv.Router(http.NotFoundHandler())
	rec := storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID, "root", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"left_behind"`) {
		t.Fatalf("delete = %d %s, want 200 naming what was left", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "sales-data")); err != nil {
		t.Errorf("the directory should still be there: %v", err)
	}
}
