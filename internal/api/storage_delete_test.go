package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// Deleting a storage resource deletes its data (ADR §12a), and only the
// resource delete may.

// The silent failure: a second caller of DestroyStorage — a cleanup path, a
// sweep written months from now for something harmless — would delete data,
// and nothing would notice until someone looked for a file. The one call is in
// dispatchStorageDeletion, which only the delete handler and the reconciler
// that finishes its work reach.
func TestOnlyTheResourceDeleteDestroysStorage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`\.DestroyStorage\(`)
	var sites []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for range call.FindAllIndex(src, -1) {
			sites = append(sites, f)
		}
	}
	if len(sites) != 1 || sites[0] != "storage_handlers.go" {
		t.Fatalf("DestroyStorage is called from %v; want exactly once, in dispatchStorageDeletion", sites)
	}
	src, _ := os.ReadFile("storage_handlers.go")
	callers := regexp.MustCompile(`s\.dispatchStorageDeletion\(`).FindAllIndex(src, -1)
	recon, _ := os.ReadFile("storage_reconcile.go")
	callers = append(callers, regexp.MustCompile(`s\.dispatchStorageDeletion\(`).FindAllIndex(recon, -1)...)
	if len(callers) != 2 {
		t.Errorf("dispatchStorageDeletion has %d callers; want the delete handler and the reconciler", len(callers))
	}
}

// The whole of a delete whose volumes the cluster has not finished: 202, the
// slug refused with its reason, the reconciler finishing once the volumes are
// gone, and the slug free again.
func TestADeleteHoldsItsSlugUntilTheClusterHasFinished(t *testing.T) {
	prov := &fakeProvisioner{report: runtime.DestroyReport{RemovesData: true, Pending: []string{"pv-1"}}, stillThere: true}
	srv, router, app := storageTestEnv(t, prov)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume","partitioned":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	// A folder mapping binds the volume's one claim (K8-D1), and detaching it
	// releases nothing: the folder is a directory of the volume.
	rec = storageRequest(t, srv, router, http.MethodPut, "/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/teams","access":"full","folder":"teams"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("map = %d: %s", rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodDelete,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID+"?folder=teams", "root", "")
	if rec.Code >= 300 {
		t.Fatalf("unmap = %d: %s", rec.Code, rec.Body.String())
	}
	for _, r := range prov.removed {
		if r.Folder == "teams" {
			t.Error("detaching the last app released the volume's folder claim")
		}
	}
	claims, err := srv.db.ListStorageClaims(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(claims, ",") != "lpv-share" {
		t.Errorf("recorded claims = %v, want only the volume's one claim", claims)
	}

	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID, "root", "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"state":"deleting"`) {
		t.Fatalf("delete = %d %s, want 202 deleting", rec.Code, rec.Body.String())
	}
	if len(prov.destroyed) != 1 || strings.Join(prov.destroyed[0].claims, ",") != strings.Join(claims, ",") {
		t.Errorf("destroy got %+v, want every recorded claim", prov.destroyed)
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "storage_deleting") {
		t.Fatalf("create while deleting = %d %s, want 409 storage_deleting", rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "root", "")
	if !strings.Contains(rec.Body.String(), `"deleting":[{"slug":"share"`) {
		t.Errorf("the list does not show the delete in flight: %s", rec.Body.String())
	}

	// Still there: the reconciler leaves it alone.
	srv.finishStorageDeletionsOnce()
	if all, _ := srv.db.ListStorageDeletions(); len(all) != 1 {
		t.Fatalf("finished while the volume was still there: %v", all)
	}
	prov.stillThere = false
	srv.finishStorageDeletionsOnce()
	if all, _ := srv.db.ListStorageDeletions(); len(all) != 0 {
		t.Fatalf("not finished once the volume was gone: %v", all)
	}
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create once finished = %d: %s", rec.Code, rec.Body.String())
	}
}

// A destroy the cluster refused is not a failed delete: the row is gone, the
// tombstone keeps the work, and the reconciler runs it again.
func TestADestroyCutShortIsFinishedByTheReconciler(t *testing.T) {
	prov := &fakeProvisioner{destroyErr: errTransient}
	srv, router, _ := storageTestEnv(t, prov)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID, "root", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete = %d %s, want 202", rec.Code, rec.Body.String())
	}
	prov.destroyErr = nil
	srv.finishStorageDeletionsOnce()
	if len(prov.destroyed) != 2 {
		t.Errorf("destroy ran %d times, want the reconciler's retry", len(prov.destroyed))
	}
	if all, _ := srv.db.ListStorageDeletions(); len(all) != 0 {
		t.Errorf("the retried destroy left a tombstone: %v", all)
	}
}

var errTransient = &transientErr{}

type transientErr struct{}

func (*transientErr) Error() string { return "the API server timed out" }

// AP-4: a pass that flipped a volume and then failed is remembered. The next
// pass — which finds that claim released and reports nothing pending — must
// not free the slug while the first pass's volume is still being deleted.
func TestAVolumeFlippedByACutShortPassStillHoldsTheSlug(t *testing.T) {
	prov := &fakeProvisioner{destroyErr: errTransient, report: runtime.DestroyReport{Pending: []string{"pv-1"}}, stillThere: true}
	srv, router, _ := storageTestEnv(t, prov)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID, "root", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete = %d %s, want 202", rec.Code, rec.Body.String())
	}
	prov.destroyErr, prov.report = nil, runtime.DestroyReport{}
	srv.finishStorageDeletionsOnce()
	if len(prov.destroyed) != 2 {
		t.Fatalf("destroy ran %d times, want the reconciler's retry", len(prov.destroyed))
	}
	all, _ := srv.db.ListStorageDeletions()
	if len(all) != 1 || strings.Join(all[0].Volumes, ",") != "pv-1" {
		t.Fatalf("tombstone = %+v, want it kept with pv-1", all)
	}
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"share","kind":"volume"}`)
	if rec.Code == http.StatusCreated {
		t.Fatal("a create was accepted while the first pass's volume was still being deleted")
	}
}

// DL-11: on the chart's EFS class every access point is rooted at
// /lpv-<slug>, so a delete that kept the directory and then freed the name
// handed the old files to the next volume of that name. The finished delete
// now holds the name, the create refuses it as storage_directory_kept, and an
// administrator releases it.
func TestAKeptVolumeDirectoryHoldsItsName(t *testing.T) {
	prov := &fakeProvisioner{report: runtime.DestroyReport{
		RemovesData: true,
		Pending:     []string{"pv-1"},
		LeftBehind:  []runtime.LeftBehind{{What: "/lpv-reports (volume pv-1)", Reason: "the class keeps a deleted volume's files"}},
	}, stillThere: true}
	srv, router, _ := storageTestEnv(t, prov)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"reports","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)
	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID, "root", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete = %d %s, want 202", rec.Code, rec.Body.String())
	}

	// Releasing a name still being deleted is not this route's to do.
	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/deleting/reports", "root", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("release while deleting = %d %s, want 404", rec.Code, rec.Body.String())
	}

	prov.stillThere = false
	srv.finishStorageDeletionsOnce()
	all, _ := srv.db.ListStorageDeletions()
	if len(all) != 1 || !all[0].Kept {
		t.Fatalf("tombstone after the volumes went = %+v, want one kept row", all)
	}
	// Finished means finished: another pass changes nothing and asks nothing.
	srv.finishStorageDeletionsOnce()
	if all, _ := srv.db.ListStorageDeletions(); len(all) != 1 {
		t.Fatalf("a second pass changed the kept row: %+v", all)
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"reports","kind":"volume"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "storage_directory_kept") ||
		!strings.Contains(rec.Body.String(), "/lpv-reports") {
		t.Fatalf("create over a kept directory = %d %s, want 409 storage_directory_kept naming /lpv-reports",
			rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "root", "")
	if !strings.Contains(rec.Body.String(), `"kept":true`) {
		t.Errorf("the list does not mark the held name as kept: %s", rec.Body.String())
	}

	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/deleting/reports", "root", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("release = %d %s, want 204", rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"reports","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create once released = %d: %s", rec.Code, rec.Body.String())
	}
}
