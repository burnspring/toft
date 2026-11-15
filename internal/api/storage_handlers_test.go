package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// fakeProvisioner stands in for a cluster. It records what the API asked to be
// bound, which is the half of the handler worth asserting here — whether the
// object is shaped correctly is a kube-package question.
type fakeProvisioner struct {
	mu       sync.Mutex
	ensured  []runtime.StorageResource
	removed  []runtime.StorageResource
	kinds    []string
	ensueErr error

	// destroyed is every DestroyStorage call, with the claims it was given;
	// report is what each answers; stillThere makes VolumesGone say no.
	destroyed  []destroyCall
	report     runtime.DestroyReport
	destroyErr error
	stillThere bool
}

type destroyCall struct {
	res    runtime.StorageResource
	claims []string
}

func (f *fakeProvisioner) DestroyStorage(_ context.Context, res runtime.StorageResource, claims []string) (runtime.DestroyReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, destroyCall{res: res, claims: claims})
	return f.report, f.destroyErr
}

func (f *fakeProvisioner) VolumesGone(context.Context, []string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.stillThere, nil
}

func (f *fakeProvisioner) EnsureStorage(_ context.Context, res runtime.StorageResource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensueErr != nil {
		return f.ensueErr
	}
	f.ensured = append(f.ensured, res)
	return nil
}

func (f *fakeProvisioner) RemoveStorage(_ context.Context, res runtime.StorageResource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, res)
	return nil
}

func (f *fakeProvisioner) StorageKinds() []string {
	if f.kinds == nil {
		return []string{runtime.MountVolume, runtime.MountStore}
	}
	return f.kinds
}

// backingProvisioner is a fakeProvisioner that also answers what can be
// underneath a kind. Deliberately a *second* type: the plain one above stands
// for a backend that cannot answer, and the server has to keep working with it
// — which is the failure mode that made kinds and backings two interfaces
// rather than one.
type backingProvisioner struct {
	fakeProvisioner
	backings map[string][]string
}

func (b *backingProvisioner) StorageBackings(kind string) []string { return b.backings[kind] }

// A backing this install cannot provision answers 422 naming its own setting.
//
// The kind is on offer and this way of backing it is not, so the sentence has
// to be the backing's: an administrator sent to KUBE_STORAGE_CLASS when what is
// missing is the S3 Files class has been sent to the wrong place.
func TestUnprovisionableBackingIsExplained(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &backingProvisioner{
		fakeProvisioner: fakeProvisioner{kinds: []string{runtime.MountVolume}},
		backings:        map[string][]string{runtime.MountVolume: {runtime.BackingEFS}},
	})

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"reports","kind":"volume","backing":"s3files","bucket":"acme-files","prefix":"reports"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "KUBE_S3FILES_STORAGE_CLASS") {
		t.Errorf("the explanation should name the backing's own setting, got: %s", rec.Body.String())
	}

	// A backing the *kind* cannot have is a 400 rather than a 422: that request
	// is malformed, not unprovisionable, and no setting would make it work.
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"wrong","kind":"volume","backing":"mountpoint"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a backing the kind cannot have = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// And the backing the install does offer goes through, defaulted or named.
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if res.Backing != database.StorageBackingEFS {
		t.Errorf("backing = %q, want the one the kind has always had", res.Backing)
	}
}

// rootProvisioner is a backend that provisions under the operator's host mounts
// rather than a cluster's bindings.
type rootProvisioner struct {
	backingProvisioner
	roots []runtime.StorageRoot
	// existing is the slugs whose directory is already on the disk.
	existing map[string]bool
}

func (r *rootProvisioner) Roots() []runtime.StorageRoot { return r.roots }

func (r *rootProvisioner) ExistingDirectory(res runtime.StorageResource) (string, bool, error) {
	return "/srv/" + res.Root + "/" + res.Slug, r.existing[res.Slug], nil
}

// An install that provisions under host roots needs a root named, whatever the
// backing.
//
// The backing is only half the question and it is the half that is easy to
// miss: there is no cluster here to bind a claim in, so a resource created with
// no root is one nothing can ever mount — the create would succeed and the
// first mapping would fail at the app's next deploy, which is exactly the
// distance this refusal exists to close.
func TestAnInstallOnHostRootsNeedsARootNamed(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &rootProvisioner{
		backingProvisioner: backingProvisioner{
			fakeProvisioner: fakeProvisioner{kinds: []string{runtime.MountVolume}},
			backings: map[string][]string{
				runtime.MountVolume: {runtime.BackingHost, runtime.BackingS3Files},
			},
		},
		roots: []runtime.StorageRoot{{Name: "reports", Path: "/srv/reports"}, {Name: "scratch", Path: "/srv/scratch"}},
	})

	// Even the backing that could in principle live elsewhere: on this install
	// it cannot. Two roots are a choice, so it is asked; one would be taken
	// (TestTheDocumentedStorageBodiesWorkOnAOneRootInstall).
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"nightly","kind":"volume","backing":"s3files","bucket":"acme-files","prefix":"nightly"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with no root = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reports") {
		t.Errorf("the refusal should name the roots that exist, got: %s", rec.Body.String())
	}

	// A root nobody declared is a 422 rather than a 400: the request is
	// well-formed and this install simply has no such mount.
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"elsewhere","kind":"volume","backing":"host","storage_root":"nope"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("create naming an undeclared root = %d, want 422: %s", rec.Code, rec.Body.String())
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"nightly","kind":"volume","backing":"host","storage_root":"reports"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if res.Root != "reports" {
		t.Errorf("root = %q, want the one it was created under", res.Root)
	}
	// And the listing hands the form the roots, as drives, so it can ask for
	// one at all.
	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "root", "")
	if !strings.Contains(rec.Body.String(), `"key":"root:reports"`) {
		t.Errorf("the listing carries no root drive: %s", rec.Body.String())
	}
}

// On an install with no cluster, a store is offered when the platform can sign
// for one — and it is the one resource there that needs no root. The root rule
// used to ask for one on any create, and the schema refuses a store that has
// one, so on an install with both roots and a signer a store could not be
// created at all.
func TestASharedInstallWithASignerCreatesAStoreWithNoRoot(t *testing.T) {
	shared := runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "efs", Path: t.TempDir()}}, t.TempDir(), runtime.BackingMountpoint)
	srv, router, _ := storageTestEnv(t, shared)

	rec := storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "root", "")
	if !strings.Contains(rec.Body.String(), `"kinds":["volume","store"]`) {
		t.Errorf("listing should offer both kinds: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"stores_mounted":false`) {
		t.Errorf("a shared install should say its stores are not mounted: %s", rec.Body.String())
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"uploads","kind":"store","bucket":"example-app-uploads","prefix":"files"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create store = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if res.Backing != database.StorageBackingMountpoint || res.Root != "" {
		t.Errorf("store = backing %q root %q, want mountpoint and no root", res.Backing, res.Root)
	}

	// A root on a store is said, not stored — the schema would refuse it.
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"rooted","kind":"store","bucket":"b","prefix":"p","storage_root":"efs"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("store with a root = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// And a volume still lives under one here: the one root the install
	// declares, when it names none (BB-046, fourth 2026-11-03 black-box list).
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume","backing":"host"}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"storage_root":"efs"`) {
		t.Errorf("volume with no root = %d, want 201 under efs: %s", rec.Code, rec.Body.String())
	}
}

// With no signer a shared install offers no store, and says what would.
func TestASharedInstallWithoutASignerExplainsTheStore(t *testing.T) {
	shared := runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "efs", Path: t.TempDir()}}, t.TempDir(), "")
	srv, router, _ := storageTestEnv(t, shared)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"uploads","kind":"store","bucket":"b","prefix":"p"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "STORE_UPLOADS") {
		t.Errorf("create store = %d %s, want 422 naming STORE_UPLOADS", rec.Code, rec.Body.String())
	}
}

// A backend that cannot answer the backings question must not turn into a
// backend that offers everything.
//
// This is the bug the two interfaces exist to prevent: widening the kinds
// interface to carry StorageBackings silently un-implemented it for every fake
// that only had StorageKinds, and an install that offered one kind began
// offering both. A capability check that fails open is the one shape this must
// never have.
func TestABackendThatCannotAnswerBackingsStillGatesKinds(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &fakeProvisioner{kinds: []string{runtime.MountStore}})

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 — the kinds gate stopped holding: %s", rec.Code, rec.Body.String())
	}
}

func storageTestEnv(t *testing.T, provisioner runtime.StorageProvisioner) (*Server, http.Handler, *database.App) {
	t.Helper()

	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	srv.storage = provisioner

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	app := &database.App{
		Slug: "demo", Name: "Demo", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return srv, srv.Router(http.NotFoundHandler()), app
}

func storageRequest(t *testing.T, srv *Server, router http.Handler, method, path, who, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Absent configuration means the capability is not offered — not that it exists
// and fails. An install with no isolated backend must not have a storage API at
// all, so nothing in the UI can appear for something that cannot work here.
func TestStorageRoutes404WithoutABackend(t *testing.T) {
	srv, router, app := storageTestEnv(t, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/storage"},
		{http.MethodPost, "/api/v1/admin/storage"},
		{http.MethodDelete, "/api/v1/admin/storage/x"},
		{http.MethodGet, "/api/v1/admin/storage/x/mappings"},
		{http.MethodPut, "/api/v1/admin/storage/x/mappings/y"},
		{http.MethodGet, "/api/v1/apps/" + app.ID + "/storage"},
	} {
		// As an administrator, so a 404 can only mean "not offered here".
		rec := storageRequest(t, srv, router, tc.method, tc.path, "root", "{}")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// Provisioning is admin-only. An app owner may see what their app has and
// nothing more: a volume is a real cost and a durable grant that outlives every
// release the app will ever have.
func TestOnlyAdminsManageStorage(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &fakeProvisioner{})

	rec := storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "owner", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("owner listing resources = %d, want 403", rec.Code)
	}
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "owner",
		`{"name":"cache","kind":"volume"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("owner creating a resource = %d, want 403", rec.Code)
	}
	// And anonymously it is 401, never a silent success.
	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous listing = %d, want 401", rec.Code)
	}
}

// The row is written before the binding, and the binding is what the cluster is
// asked for. A row with no binding is visible and repairable; a binding with no
// row is an orphan on a shared filesystem that nobody finds until the quota
// runs out.
func TestCreatingAResourceBindsIt(t *testing.T) {
	prov := &fakeProvisioner{}
	srv, router, _ := storageTestEnv(t, prov)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"shared-cache","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	var res database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Slug != "shared-cache" {
		t.Errorf("slug = %q, want a slugified name", res.Slug)
	}
	if len(prov.ensured) != 1 || prov.ensured[0].Slug != "shared-cache" {
		t.Fatalf("ensured = %+v", prov.ensured)
	}
}

// A store with no prefix would mount a bucket root, giving every app mapped to
// any store in that bucket access to all of it. The prefix *is* the
// containment, so its absence is refused rather than defaulted.
func TestStoreNeedsABucketAndAPrefix(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &fakeProvisioner{})

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"everything","kind":"store","bucket":"acme-data"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without a prefix = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bucket root") {
		t.Errorf("the refusal should say why, got: %s", rec.Body.String())
	}

	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"datasets","kind":"store","prefix":"sales"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("create without a bucket = %d, want 400", rec.Code)
	}
}

// A kind this install cannot provision answers 422 with the setting that would
// change that — the request is well-formed and the kind is real, so it is not
// a 400, and an admin should read a decision rather than a malfunction.
func TestUnprovisionableKindIsExplained(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &fakeProvisioner{kinds: []string{runtime.MountStore}})

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "KUBE_STORAGE_CLASS") {
		t.Errorf("the explanation should name the setting, got: %s", rec.Body.String())
	}
}

// Mapping ensures the binding for that level exists before the app is told it
// has one — a mapping whose claim is absent produces a pod stuck Pending with
// an event nobody reads.
func TestMappingBindsTheAccessLevelFirst(t *testing.T) {
	prov := &fakeProvisioner{}
	srv, router, app := storageTestEnv(t, prov)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"datasets","kind":"store","bucket":"acme-data","prefix":"sales"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	rec = storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/sets","access":"full"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("map = %d: %s", rec.Code, rec.Body.String())
	}

	var bound bool
	for _, e := range prov.ensured {
		if e.Slug == "datasets" && e.Access == runtime.AccessFull {
			bound = true
		}
	}
	if !bound {
		t.Errorf("the full-access binding was never ensured: %+v", prov.ensured)
	}

	// And the response says when it takes effect, because the admin has just
	// changed what an app can reach and nothing about the running app moved.
	if !strings.Contains(rec.Body.String(), "next deploy") {
		t.Errorf("the response should say when it applies, got: %s", rec.Body.String())
	}
}

// A static app has no process and no job to mount anything into. Accepted, the
// mapping said "takes effect on the next deployment", which never came, and
// held one of the app's five places (the trytoft drills).
func TestAStaticAppIsRefusedAMapping(t *testing.T) {
	srv, router, app := storageTestEnv(t, &fakeProvisioner{})
	app.Framework = "static"
	if err := srv.db.UpdateApp(app); err != nil {
		t.Fatal(err)
	}

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	rec = storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/cache","access":"full"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "static_app") {
		t.Fatalf("map = %d: %s, want 409 static_app", rec.Code, rec.Body.String())
	}
	if got, _ := srv.db.ListStorageMappingsForApp(app.ID); len(got) != 0 {
		t.Errorf("a refused mapping was stored: %+v", got)
	}
}

// An access level the kind does not offer is refused with the levels that would
// have worked. Volumes have no overwrite or delete flags to gate.
func TestAccessLevelMustSuitTheKind(t *testing.T) {
	srv, router, app := storageTestEnv(t, &fakeProvisioner{})

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	rec = storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/cache","access":"write"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("map at write = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "read, full") {
		t.Errorf("the refusal should list what works, got: %s", rec.Body.String())
	}

	// `overwrite` was merged into `full` and is refused, never aliased: a
	// request for less must not be granted more. The refusal says what to send.
	rec = storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/cache","access":"overwrite"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("map at overwrite = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "merged into full") {
		t.Errorf("the refusal should name full, got: %s", rec.Body.String())
	}
}

// A path inside the runner's own tree would shadow the release the app is about
// to exec out of. Refused at the API, not discovered at container creation.
func TestReservedMountPathsAreRefused(t *testing.T) {
	srv, router, app := storageTestEnv(t, &fakeProvisioner{})

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)

	for _, path := range []string{"/workspace", "data/cache", "/", "/data/../etc"} {
		rec := storageRequest(t, srv, router, http.MethodPut,
			"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
			`{"path":"`+path+`","access":"read"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("mapping at %q = %d, want 400", path, rec.Code)
		}
	}
}

// The per-app view is readable by anyone who can read the app, and by nobody
// else — the same 404-over-403 rule every unreadable object follows.
func TestAppStorageFollowsAppReadability(t *testing.T) {
	prov := &fakeProvisioner{}
	srv, router, app := storageTestEnv(t, prov)
	rungOff(t, srv)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)
	storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/cache","access":"full"}`)

	// The owner sees it.
	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/apps/"+app.ID+"/storage", "owner", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner = %d: %s", rec.Code, rec.Body.String())
	}
	var got appStorageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Mappings) != 1 || got.Mappings[0].Path != "/data/cache" {
		t.Fatalf("mappings = %+v", got.Mappings)
	}
	// The app is shared, so nothing is mounted yet and the note says what to do.
	if !got.Pending {
		t.Error("a shared app with mappings has none of them mounted; want pending")
	}
	if !strings.Contains(got.Note, "isolated mode") {
		t.Errorf("the note should name the requirement, got: %q", got.Note)
	}

	// A stranger cannot read the app, so they cannot read its storage either.
	rec = storageRequest(t, srv, router, http.MethodGet, "/api/v1/apps/"+app.ID+"/storage", "stranger", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("stranger = %d, want 404", rec.Code)
	}
}

// Deleting a mapped resource is refused, and unmapping first makes it work. The
// refusal is what stands between an administrator and a live app losing its
// filesystem mid-read.
func TestDeletingAMappedResourceIsRefused(t *testing.T) {
	prov := &fakeProvisioner{}
	srv, router, app := storageTestEnv(t, prov)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume"}`)
	var res database.StorageResource
	json.Unmarshal(rec.Body.Bytes(), &res)
	storageRequest(t, srv, router, http.MethodPut,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
		`{"path":"/data/cache","access":"full"}`)

	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID, "root", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete while mapped = %d, want 409", rec.Code)
	}
	if len(prov.removed) != 0 {
		t.Error("a refused delete must not have released the binding")
	}

	// **200 with a count, not 204.** A detach that names no folder takes every
	// folder that app mounts of this resource — which is what the route meant
	// before folders and what a caller written against it intended — so it has
	// to say how many went. An empty 204 would leave the one ambiguity in this
	// route silent.
	rec = storageRequest(t, srv, router, http.MethodDelete,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unmap = %d: %s", rec.Code, rec.Body.String())
	}
	var unmapped struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &unmapped); err != nil || unmapped.Removed != 1 {
		t.Errorf("unmap said %s, want one mapping removed", rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodDelete, "/api/v1/admin/storage/"+res.ID, "root", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"deleted"`) {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	// A volume's delete destroys it — DestroyStorage, with its recorded claims —
	// rather than only releasing a binding.
	if len(prov.destroyed) != 1 {
		t.Errorf("the volume should have been destroyed once, got %+v", prov.destroyed)
	}
}

// A name is what apps and the CLI type, so its format is checked rather than a
// slug derived from it: the string the administrator enters is the string
// that comes back, or the create is refused with the rule.
func TestStorageNamesFollowTheNamingSchema(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &fakeProvisioner{})

	for _, tc := range []struct {
		name string
		code int
	}{
		{"sales-data", http.StatusCreated},
		{"s3-reports-2026", http.StatusCreated},
		{"Sales data", http.StatusBadRequest},
		{"sales_data", http.StatusBadRequest},
		{"2026-data", http.StatusBadRequest},
		{"ab", http.StatusBadRequest},
		{"sales--data", http.StatusBadRequest},
		{"sales-", http.StatusBadRequest},
		{strings.Repeat("n", database.MaxStorageNameLen+1), http.StatusBadRequest},
	} {
		rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
			`{"name":"`+tc.name+`","kind":"volume"}`)
		if rec.Code != tc.code {
			t.Errorf("name %q = %d, want %d: %s", tc.name, rec.Code, tc.code, rec.Body.String())
			continue
		}
		if tc.code != http.StatusCreated {
			continue
		}
		var res database.StorageResource
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Name != tc.name || res.Slug != tc.name {
			t.Errorf("name %q came back as name %q slug %q, want both unchanged", tc.name, res.Name, res.Slug)
		}
	}

	// A slug is accepted only as the same string: one resource, one name.
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"one-name","slug":"another-name","kind":"volume"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a slug unlike the name = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	longDesc := strings.Repeat("d", maxStorageDescriptionLen+1)
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"cache","kind":"volume","description":"`+longDesc+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a description over the cap = %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

// The name is fixed at create: apps look it up by it and every binding is
// named after it. The description is what moves, and echoing the current name
// back is harmless.
func TestAStorageNameCannotBeChanged(t *testing.T) {
	srv, router, _ := storageTestEnv(t, &fakeProvisioner{})
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"sales-data","kind":"volume"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var res database.StorageResource
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/admin/storage/" + res.ID

	for _, body := range []string{`{"name":"other-data"}`, `{"slug":"other-data"}`} {
		rec = storageRequest(t, srv, router, http.MethodPatch, url, "root", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot be changed") {
			t.Errorf("PATCH %s = %d %s, want 400 saying the name is fixed", body, rec.Code, rec.Body.String())
		}
	}
	rec = storageRequest(t, srv, router, http.MethodPatch, url, "root",
		`{"name":"sales-data","description":"Quarterly sales extracts"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Quarterly sales extracts") {
		t.Errorf("PATCH description = %d %s, want 200 with the new description", rec.Code, rec.Body.String())
	}
}

// classProvisioner is a cluster's storage, as far as the create cares: it
// declares classes.
type classProvisioner struct {
	fakeProvisioner
	classes []runtime.DeclaredClass
}

func (c *classProvisioner) DeclaredClasses() []runtime.DeclaredClass { return c.classes }

// StorageBackings is the real backend's rule: what the declared classes serve.
func (c *classProvisioner) StorageBackings(kind string) []string {
	if kind != runtime.MountVolume {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, cl := range c.classes {
		if !seen[cl.Backing] {
			seen[cl.Backing] = true
			out = append(out, cl.Backing)
		}
	}
	return out
}

// A resource may name only a class the install declares, under a backing that
// class serves. Any string used to be accepted, and a class that does not exist
// is a claim that waits for a provisioner forever.
func TestACreateNamesOnlyADeclaredClass(t *testing.T) {
	prov := &classProvisioner{classes: []runtime.DeclaredClass{
		{Name: "efs-toft", Backing: runtime.BackingEFS, Default: true},
		{Name: "s3files-toft", Backing: runtime.BackingS3Files, Default: true},
	}}
	srv, router, _ := storageTestEnv(t, prov)

	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"name":"named-class","kind":"volume","backing":"efs","storage_class":"efs-toft"}`, http.StatusCreated},
		{`{"name":"no-class","kind":"volume","backing":"efs"}`, http.StatusCreated},
		{`{"name":"made-up","kind":"volume","backing":"efs","storage_class":"gp3"}`, http.StatusUnprocessableEntity},
		{`{"name":"wrong-backing","kind":"volume","backing":"efs","storage_class":"s3files-toft"}`, http.StatusUnprocessableEntity},
	} {
		rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root", tc.body)
		if rec.Code != tc.code {
			t.Errorf("%s = %d, want %d: %s", tc.body, rec.Code, tc.code, rec.Body.String())
		}
	}

	// A backend with no classes at all — a host install — has none to name.
	srv2, router2, _ := storageTestEnv(t, &fakeProvisioner{})
	rec := storageRequest(t, srv2, router2, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"on-a-host","kind":"volume","storage_class":"efs-toft"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a class on a backend with none = %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

// On an install whose storage roots reach shared mode, a shared app mounts its
// resources and deploys fine — so the note must not say its deploy will fail.
// A drills run met that warning on every mapping and then watched three
// shared-mode deploys succeed.
func TestStorageNoteKnowsSharedRoots(t *testing.T) {
	app := &database.App{ExecutionMode: string(runtime.ModeShared)}
	if got := storageTakesEffectNote(app, true); strings.Contains(got, "will fail") || !strings.Contains(got, "deployment") {
		t.Errorf("shared roots offered: %q", got)
	}
	if got := storageTakesEffectNote(app, false); !strings.Contains(got, "isolated mode") {
		t.Errorf("no shared roots: the note should name isolated mode, got %q", got)
	}
}

// A job run is a workload described when it starts, so it reads a new mapping
// at its next run with no deploy between — the note says so wherever it says
// the app waits for a deployment (docs/bugs 2026-10-28 D12).
func TestStorageNoteSaysAJobPicksItUpAtItsNextRun(t *testing.T) {
	isolated := &database.App{ExecutionMode: string(runtime.ModeIsolated), RunningMode: string(runtime.ModeIsolated)}
	moving := &database.App{ExecutionMode: string(runtime.ModeIsolated)}
	shared := &database.App{ExecutionMode: string(runtime.ModeShared)}
	for name, got := range map[string]string{
		"isolated":     storageTakesEffectNote(isolated, false),
		"moving":       storageTakesEffectNote(moving, false),
		"shared roots": storageTakesEffectNote(shared, true),
	} {
		if !strings.Contains(got, "deployment") || !strings.Contains(got, "job's next run") {
			t.Errorf("%s: %q, want a deployment and a job's next run", name, got)
		}
	}
}
