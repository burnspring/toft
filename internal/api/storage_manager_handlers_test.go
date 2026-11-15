package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/runtime"
)

// errStorageTest is a cluster refusing a binding, with a string a delegate must
// never see.
var errStorageTest = errors.New("csi driver: efs-ap quota exhausted on fs-0abc")

func hasText(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func delegateEnv(t *testing.T) (*Server, http.Handler, *database.App, *database.StorageResource) {
	t.Helper()
	srv, router, app := storageTestEnv(t, &fakeProvisioner{})
	seedUser(t, srv.db, "delegate", database.SystemRolePublisher)

	res := &database.StorageResource{
		Name: "Reports", Slug: "reports", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/reports",
	}
	if err := srv.db.CreateStorageResource(res, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	return srv, router, app, res
}

func delegate(t *testing.T, srv *Server, resourceID, subjectID, level string) {
	t.Helper()
	m := &database.StorageManager{
		ResourceID: resourceID, SubjectType: database.StoreSubjectUser,
		SubjectID: subjectID, MaxAccess: level,
	}
	if err := srv.db.PutStorageManager(m, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageManager: %v", err)
	}
}

// Both halves, and neither alone.
//
// A delegate needs management on the resource *and* ActionEditContent on the
// app. Missing either is a 404 rather than a 403 — the caller learns nothing
// about a resource they cannot reach or an app they cannot edit.
func TestAttachingNeedsBothHalves(t *testing.T) {
	srv, router, app, res := delegateEnv(t)
	path := "/api/v1/apps/" + app.ID + "/storage/" + res.ID
	body := `{"path":"/data/reports","access":"read"}`

	// Neither half: a publisher who is not the owner and manages nothing.
	if rec := storageRequest(t, srv, router, http.MethodPut, path, "stranger", body); rec.Code != http.StatusNotFound {
		t.Errorf("stranger with neither half = %d, want 404", rec.Code)
	}

	// The app half only. The owner may edit their app and manages nothing.
	if rec := storageRequest(t, srv, router, http.MethodPut, path, "owner", body); rec.Code != http.StatusNotFound {
		t.Errorf("owner with no delegation = %d, want 404", rec.Code)
	}

	// The resource half only. The delegate manages it and cannot edit the app.
	delegate(t, srv, res.ID, "delegate", runtime.AccessWrite)
	if rec := storageRequest(t, srv, router, http.MethodPut, path, "delegate", body); rec.Code != http.StatusNotFound {
		t.Errorf("delegate who cannot edit the app = %d, want 404", rec.Code)
	}

	// Both halves. The owner, now delegated.
	delegate(t, srv, res.ID, "owner", runtime.AccessWrite)
	rec := storageRequest(t, srv, router, http.MethodPut, path, "owner", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner with both halves = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	mappings, err := srv.db.ListStorageMappingsForApp(app.ID)
	if err != nil {
		t.Fatalf("ListStorageMappingsForApp: %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("mappings = %+v, want one", mappings)
	}
	// Stamped to the delegation it resolved through, which is what a later
	// revocation collects by.
	if want := "user:owner"; mappings[0].Authority != want {
		t.Errorf("authority = %q, want %q", mappings[0].Authority, want)
	}
}

// A delegate cannot attach above their ceiling, and the refusal names it.
func TestAttachingIsCappedByTheCeiling(t *testing.T) {
	srv, router, app, res := delegateEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessWrite)
	path := "/api/v1/apps/" + app.ID + "/storage/" + res.ID

	rec := storageRequest(t, srv, router, http.MethodPut, path, "owner",
		`{"path":"/data/reports","access":"full"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("attaching above the ceiling = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !hasText(body, "write") {
		t.Errorf("the refusal should name the ceiling, got: %s", body)
	}

	// At the ceiling it lands.
	if rec := storageRequest(t, srv, router, http.MethodPut, path, "owner",
		`{"path":"/data/reports","access":"write"}`); rec.Code != http.StatusOK {
		t.Fatalf("attaching at the ceiling = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// An administrator keeps today's behaviour: any resource, any app, no
// delegation required, and the mapping is stamped `admin`.
func TestAnAdministratorNeedsNoDelegation(t *testing.T) {
	srv, router, app, res := delegateEnv(t)
	path := "/api/v1/apps/" + app.ID + "/storage/" + res.ID

	rec := storageRequest(t, srv, router, http.MethodPut, path, "root",
		`{"path":"/data/reports","access":"full"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("administrator attaching = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	mappings, _ := srv.db.ListStorageMappingsForApp(app.ID)
	if len(mappings) != 1 || mappings[0].Authority != database.AuthorityAdmin {
		t.Fatalf("mappings = %+v, want one stamped admin", mappings)
	}
}

// GET /me/storage is the delegate's whole view, and it carries no bucket and no
// prefix — those are the containment boundary and belong to an administrator.
func TestMyStorageCarriesNoBucketOrPrefix(t *testing.T) {
	srv, router, _, res := delegateEnv(t)

	rec := storageRequest(t, srv, router, http.MethodGet, "/api/v1/me/storage", "owner", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/storage = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !hasText(body, `"resources":[]`) {
		t.Errorf("a caller managing nothing should see an empty list, got: %s", body)
	}

	delegate(t, srv, res.ID, "owner", runtime.AccessWrite)
	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/me/storage", "owner", "")
	body := rec.Body.String()
	if !hasText(body, `"slug":"reports"`) || !hasText(body, `"max_access":"write"`) {
		t.Fatalf("the listing should carry the resource and the ceiling, got: %s", body)
	}
	for _, secret := range []string{"acme-data", "team/reports", `"bucket"`, `"prefix"`} {
		if hasText(body, secret) {
			t.Errorf("the listing leaked %q, which is the containment boundary: %s", secret, body)
		}
	}
}

// Revoking removes the mappings the delegation wrote and answers with the apps
// that are being restarted — the list the administrator needs in order to tell
// the right people.
func TestRevokingAnswersWithTheAppsItRestarts(t *testing.T) {
	srv, router, app, res := delegateEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessWrite)

	attach := "/api/v1/apps/" + app.ID + "/storage/" + res.ID
	if rec := storageRequest(t, srv, router, http.MethodPut, attach, "owner",
		`{"path":"/data/reports","access":"write"}`); rec.Code != http.StatusOK {
		t.Fatalf("attach = %d: %s", rec.Code, rec.Body.String())
	}

	revoke := "/api/v1/admin/storage/" + res.ID + "/managers/user:owner"
	rec := storageRequest(t, srv, router, http.MethodDelete, revoke, "root", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Restarting []database.StorageManagedApp `json:"restarting"`
		Note       string                       `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Restarting) != 1 || out.Restarting[0].ID != app.ID {
		t.Fatalf("restarting = %+v, want the one app", out.Restarting)
	}
	if !hasText(out.Note, "current release") || !hasText(out.Note, "Demo") {
		t.Errorf("the note should name the app and say what it is restarted onto, got: %q", out.Note)
	}

	mappings, _ := srv.db.ListStorageMappingsForApp(app.ID)
	if len(mappings) != 0 {
		t.Errorf("mappings survived the revocation: %+v", mappings)
	}

	// And a second revoke is a 404 rather than a second silent success.
	if rec := storageRequest(t, srv, router, http.MethodDelete, revoke, "root", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second revoke = %d, want 404", rec.Code)
	}
}

// Delegating is administrator-only, and the manager routes say so.
func TestOnlyAdministratorsDelegate(t *testing.T) {
	srv, router, _, res := delegateEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)

	base := "/api/v1/admin/storage/" + res.ID + "/managers"
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPut, base, `{"subject_type":"user","subject_id":"stranger","max_access":"full"}`},
		{http.MethodDelete, base + "/user:owner", ""},
	} {
		// The owner manages the resource at `full` and still may not delegate:
		// using is operation, deciding who may is authorization.
		rec := storageRequest(t, srv, router, tc.method, tc.path, "owner", tc.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a delegate = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}

// The delegation routes 404 on an install with no storage backend, like every
// other storage route. Absent, not dormant.
func TestDelegationRoutes404WithoutABackend(t *testing.T) {
	srv, router, app := storageTestEnv(t, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/storage/x/managers"},
		{http.MethodPut, "/api/v1/admin/storage/x/managers"},
		{http.MethodDelete, "/api/v1/admin/storage/x/managers/user:y"},
		{http.MethodGet, "/api/v1/me/storage"},
		{http.MethodPut, "/api/v1/apps/" + app.ID + "/storage/x"},
		{http.MethodDelete, "/api/v1/apps/" + app.ID + "/storage/x"},
	} {
		rec := storageRequest(t, srv, router, tc.method, tc.path, "root", "{}")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// A cluster failure on a delegate's attach writes no row and never shows the
// delegate the driver's error string — an app owner is not an operator, and a
// CSI message is an install's internals.
func TestADelegatesAttachHidesTheClusterError(t *testing.T) {
	srv, router, app := storageTestEnv(t, &fakeProvisioner{ensueErr: errStorageTest})
	seedUser(t, srv.db, "delegate", database.SystemRolePublisher)
	res := &database.StorageResource{
		Name: "Reports", Slug: "reports", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/reports",
	}
	// Created directly, since the create route would fail on the same binding.
	if err := srv.db.CreateStorageResource(res, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	delegate(t, srv, res.ID, "owner", runtime.AccessWrite)

	rec := storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/apps/"+app.ID+"/storage/"+res.ID, "owner",
		`{"path":"/data/reports","access":"write"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("attach with a refusing cluster = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); hasText(body, errStorageTest.Error()) {
		t.Errorf("the delegate was shown the cluster error: %s", body)
	}
	mappings, _ := srv.db.ListStorageMappingsForApp(app.ID)
	if len(mappings) != 0 {
		t.Errorf("a row was written despite the binding failing: %+v", mappings)
	}
}

// A delegation body with no max_access names the field — and, when the
// grants' `access` was sent instead, says so — rather than quoting "" as a
// level (D-02).
func TestADelegationWithoutMaxAccessNamesTheField(t *testing.T) {
	srv, router, _, res := delegateEnv(t)
	for _, path := range []string{"/api/v1/admin/storage/" + res.ID + "/managers", "/api/v1/admin/storage/defaults"} {
		rec := storageRequest(t, srv, router, http.MethodPut, path, "root",
			`{"subject_type":"user","subject_id":"delegate","access":"read"}`)
		body := rec.Body.String()
		if rec.Code != http.StatusBadRequest || !hasText(body, "needs max_access") ||
			!hasText(body, "grant") || hasText(body, `\"\" is not one`) || !hasText(body, `"hint"`) {
			t.Errorf("PUT %s with access = %d %s", path, rec.Code, body)
		}
	}
}

// An attach is what a locked app would run with when the lock lifts, so it is
// refused as PATCH /env is — but only to a caller who could attach at all: one
// with no delegation still gets the 404 and learns nothing about the lock.
// Detaching narrows and stays open (security review 2026-11-15, LSA-2).
func TestAttachingToALockedAppIsRefused(t *testing.T) {
	srv, router, app, res := delegateEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessWrite)
	if err := srv.db.SetAppLock(app.ID, "root", "Retired in March.", time.Now(), app.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("SetAppLock: %v", err)
	}
	path := "/api/v1/apps/" + app.ID + "/storage/" + res.ID
	body := `{"path":"/data/reports","access":"read"}`

	if rec := storageRequest(t, srv, router, http.MethodPut, path, "stranger", body); rec.Code != http.StatusNotFound {
		t.Errorf("stranger attaching to a locked app = %d, want 404", rec.Code)
	}
	rec := storageRequest(t, srv, router, http.MethodPut, path, "owner", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("attaching to a locked app = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding refusal: %v", err)
	}
	if msg, _ := out["error"].(string); out["code"] != "app_locked" || !hasText(msg, "Retired in March.") {
		t.Errorf("refusal = %+v, want app_locked carrying the administrator's message", out)
	}
	if mappings, _ := srv.db.ListStorageMappingsForApp(app.ID); len(mappings) != 0 {
		t.Errorf("a refused attach left mappings: %+v", mappings)
	}

	if rec := storageRequest(t, srv, router, http.MethodDelete, path, "owner", ""); rec.Code != http.StatusOK && rec.Code != http.StatusNotFound && rec.Code != http.StatusNoContent {
		t.Errorf("detaching from a locked app = %d, want it left open: %s", rec.Code, rec.Body.String())
	}
}
