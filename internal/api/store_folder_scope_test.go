package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"toft/internal/artifact"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/kube"
	"toft/internal/runtime"
)

// countingObjectStore stands in for the bucket and, more usefully, counts.
//
// The assertion this exists for is a *negative* one — that a listing never
// happened — and a fake that returned plausible objects would pass whether or
// not the request went out.
type countingObjectStore struct {
	hits atomic.Int64
	srv  *httptest.Server
}

func newCountingObjectStore(t *testing.T) *countingObjectStore {
	t.Helper()
	c := &countingObjectStore{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`+
			`<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func storeScopeServer(t *testing.T, bucket *countingObjectStore) *Server {
	t.Helper()
	db := dbtest.New(t)
	srv := newTestServerWithConfig(t, db, &config.Config{
		BaseURL:      "http://localhost:8080",
		StoreUploads: true,
	})
	signer, err := artifact.NewS3Signer(context.Background(), artifact.S3SignerConfig{
		Endpoint: bucket.srv.URL, Region: "us-east-1", UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Signer: %v", err)
	}
	srv.storeSigner = signer
	// Uploads are offered only where there is both a signer and a storage
	// backend, which is the install shape these routes exist on.
	srv.storage = &fakeProvisioner{}
	return srv
}

func seedStore(t *testing.T, db *database.DB, slug string) *database.StorageResource {
	t.Helper()
	res := &database.StorageResource{
		Name: slug, Slug: slug, Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/" + slug,
	}
	if err := db.CreateStorageResource(res, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	return res
}

func grantFolder(t *testing.T, db *database.DB, resourceID, userID, folder, access string) {
	t.Helper()
	g := &database.StoreGrant{
		ResourceID: resourceID, SubjectType: database.StoreSubjectUser,
		SubjectID: userID, Access: access, Folder: folder,
	}
	if err := db.PutStoreGrant(g, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStoreGrant: %v", err)
	}
}

// A folder nobody granted you is not a row you can see, and the way that is
// true is that nothing was listed.
//
// **This is the test whose failure would be silent.** A regression that turned
// the root back into a real listing would render a working page — folders,
// names, everything — and the only thing wrong with it would be that a person
// granted `analytics/` now knows there is a `payroll-2026/`. Counting the
// requests to the object store is the only assertion that catches it, because
// the response body of the broken version looks better than the correct one.
func TestStoreRootIsSynthesizedForAFolderGrant(t *testing.T) {
	bucket := newCountingObjectStore(t)
	srv := storeScopeServer(t, bucket)
	seedUser(t, srv.db, "analyst", database.SystemRoleViewer)
	res := seedStore(t, srv.db, "reports")
	grantFolder(t, srv.db, res.ID, "analyst", "analytics", runtime.AccessRead)
	grantFolder(t, srv.db, res.ID, "analyst", "finance/q1", runtime.AccessFull)

	w := getAs(t, srv, srv.Router(http.NotFoundHandler()), "analyst", "/api/v1/store/reports/objects")

	if w.Code != http.StatusOK {
		t.Fatalf("listing the root = %d, want 200: %s", w.Code, w.Body.String())
	}
	var page artifact.ObjectPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding the listing: %v", err)
	}
	want := []string{"analytics/", "finance/q1/"}
	if len(page.Prefixes) != len(want) {
		t.Fatalf("prefixes = %v, want %v", page.Prefixes, want)
	}
	for i, p := range want {
		if page.Prefixes[i] != p {
			t.Errorf("prefix %d = %q, want %q", i, page.Prefixes[i], p)
		}
	}
	if len(page.Objects) != 0 {
		t.Errorf("objects = %v, want none — the root of a folder grant holds no objects of its own", page.Objects)
	}
	if n := bucket.hits.Load(); n != 0 {
		t.Errorf("the object store was asked %d time(s); a synthesized root must ask it none, "+
			"or a sibling folder is one listing away from being disclosed", n)
	}
}

// A caller who reaches the root gets the real listing, which is the other half:
// the synthetic answer is for somebody who cannot see everything, and must not
// become the answer for everybody.
func TestStoreRootIsRealForARootGrant(t *testing.T) {
	bucket := newCountingObjectStore(t)
	srv := storeScopeServer(t, bucket)
	seedUser(t, srv.db, "owner", database.SystemRoleViewer)
	res := seedStore(t, srv.db, "reports")
	grantFolder(t, srv.db, res.ID, "owner", "", runtime.AccessRead)

	w := getAs(t, srv, srv.Router(http.NotFoundHandler()), "owner", "/api/v1/store/reports/objects")

	if w.Code != http.StatusOK {
		t.Fatalf("listing the root = %d, want 200: %s", w.Code, w.Body.String())
	}
	if n := bucket.hits.Load(); n != 1 {
		t.Errorf("the object store was asked %d time(s), want 1 — a root grant lists the store itself", n)
	}
}

// A path outside every granted folder is refused, and the refusal says which
// folders the caller does hold.
//
// 403 rather than 404, because the caller already holds a scope on this store
// and therefore already knows it exists — and rather than an empty listing,
// which would teach them that `marketing/` exists and happens to be empty.
func TestListingOutsideAGrantedFolderIsRefused(t *testing.T) {
	bucket := newCountingObjectStore(t)
	srv := storeScopeServer(t, bucket)
	seedUser(t, srv.db, "analyst", database.SystemRoleViewer)
	res := seedStore(t, srv.db, "reports")
	grantFolder(t, srv.db, res.ID, "analyst", "analytics", runtime.AccessRead)

	w := getAs(t, srv, srv.Router(http.NotFoundHandler()), "analyst",
		"/api/v1/store/reports/objects?path=marketing/")

	if w.Code != http.StatusForbidden {
		t.Fatalf("listing outside the grant = %d, want 403: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error   string   `json:"error"`
		Folders []string `json:"folders"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}
	if len(body.Folders) != 1 || body.Folders[0] != "analytics" {
		t.Errorf("folders = %v, want [analytics] — the refusal names what they do hold", body.Folders)
	}
	if n := bucket.hits.Load(); n != 0 {
		t.Errorf("a refused listing asked the object store %d time(s), want 0", n)
	}
}

// A presign is resolved against the key, not the store.
func TestPresignResolvesTheLevelPerKey(t *testing.T) {
	bucket := newCountingObjectStore(t)
	srv := storeScopeServer(t, bucket)
	seedUser(t, srv.db, "analyst", database.SystemRoleViewer)
	res := seedStore(t, srv.db, "reports")
	grantFolder(t, srv.db, res.ID, "analyst", "analytics", runtime.AccessFull)

	router := srv.Router(http.NotFoundHandler())
	for _, tc := range []struct {
		name, key string
		want      int
	}{
		{"inside the granted folder", "analytics/2026.csv", http.StatusOK},
		{"a sibling folder", "marketing/2026.csv", http.StatusForbidden},
		{"a folder whose name merely starts the same way", "analytics-archive/x", http.StatusForbidden},
		{"an object beside the folder", "analytics", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := getAs(t, srv, router, "analyst", "/api/v1/store/reports/objects/"+tc.key)
			if w.Code != tc.want {
				t.Errorf("presigning %q = %d, want %d: %s", tc.key, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// The folder is expressed twice, and this is the test that keeps the two
// expressions the same expression.
//
// Once as the mount's prefix (kube.StorePrefix), once as the covering test a
// grant resolves through (StoreScopes.LevelForKey). Drift between them is not a
// failure anybody sees: a key the grant covers but the mount's prefix excludes
// is a URL the platform signs for an object no app of that mapping could ever
// read, and — the direction that matters — a key the grant covers that lands
// *outside* the folder is a person reaching a directory nobody granted them.
//
// So the oracle here is the other implementation, never a copied constant.
func TestGrantCoverageMatchesTheMountPrefix(t *testing.T) {
	res := database.StorageResource{
		Slug: "reports", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/reports",
	}
	const folder = "analytics"
	scopes := database.StoreScopes{
		Resource: res,
		Scopes:   []database.StoreScope{{Folder: folder, Access: runtime.AccessFull}},
	}
	mountPrefix := kube.StorePrefix(runtime.StorageResource{
		Slug: res.Slug, Kind: runtime.MountStore,
		Bucket: res.Bucket, Prefix: res.Prefix, Access: runtime.AccessFull, Folder: folder,
	})

	for _, name := range []string{
		"analytics/2026.csv",
		"analytics/deep/nested/file.parquet",
		"analytics-archive/2026.csv",
		"analytics",
		"analyticsx/y",
		"marketing/2026.csv",
		"team/reports/analytics/confused.csv",
	} {
		covered := scopes.LevelForKey(name) != ""
		inMount := strings.HasPrefix(storeKeyFor(res, name), mountPrefix)
		if covered != inMount {
			t.Errorf("%q: the grant %s it and the mount %s it — the two expressions of one folder disagree",
				name,
				map[bool]string{true: "covers", false: "does not cover"}[covered],
				map[bool]string{true: "contains", false: "does not contain"}[inMount])
		}
	}
}

// A binding is released when the last mapping of its folder goes, and not
// before.
//
// The failure this guards is the quiet one: a mapping moved from one folder to
// another, or a second app mounting the same folder, and a sweep that counted
// nothing releasing a PersistentVolume somebody's running pod is holding.
func TestSweepReleasesOnlyUnreferencedFolders(t *testing.T) {
	prov := &fakeProvisioner{}
	srv, router, app := storageTestEnv(t, prov)

	second := &database.App{
		Slug: "second", Name: "Second", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusStopped,
		Visibility: database.VisibilityPrivate, ExecutionMode: string(runtime.ModeIsolated),
	}
	if err := srv.db.CreateApp(second); err != nil {
		t.Fatalf("create second app: %v", err)
	}
	res := seedStore(t, srv.db, "shared")

	// Both apps mount the same folder; one of them also mounts another.
	attach := func(appID, path, folder string) {
		t.Helper()
		body := fmt.Sprintf(`{"path":%q,"access":"read","folder":%q}`, path, folder)
		w := storageRequest(t, srv, router, http.MethodPut,
			"/api/v1/admin/storage/"+res.ID+"/mappings/"+appID, "root", body)
		if w.Code != http.StatusOK {
			t.Fatalf("attaching %s at %s: %d %s", folder, path, w.Code, w.Body.String())
		}
	}
	attach(app.ID, "/data/a", "analytics")
	attach(second.ID, "/data/a", "analytics")
	attach(app.ID, "/data/f", "finance")

	prov.mu.Lock()
	prov.removed = nil
	prov.mu.Unlock()

	// Detaching the first app takes both of its mappings. `analytics` is still
	// the second app's, so only `finance` may be released.
	w := storageRequest(t, srv, router, http.MethodDelete,
		"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", "")
	if w.Code != http.StatusOK {
		t.Fatalf("detaching = %d, want 200: %s", w.Code, w.Body.String())
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	released := map[string]bool{}
	for _, r := range prov.removed {
		released[r.Folder] = true
	}
	if !released["finance"] {
		t.Errorf("finance was not released, and nothing mounts it any more")
	}
	if released["analytics"] {
		t.Errorf("analytics was released while another app still mounts it")
	}
	if released[""] {
		t.Errorf("the resource's own bindings were released by a detach; they belong to its deletion")
	}
}

// One validator, three callers, and the same strings refused by all of them.
//
// **This is the test whose failure is silent and worst.** A folder is validated
// on the mapping route, on the grant route and as a listing path, and the rule
// they share — no `..`, no empty segment, no backslash, no leading slash, no
// control character — is `ValidateStorePath` delegating to
// `ValidateStoreObjectName`. A second copy of that list anywhere is how a
// traversal comes to be refused by one caller and permitted by another, and the
// permitted one is a mount rooted outside its own resource, or a URL signed for
// a key beside somebody else's store. Nothing errors when that happens; it just
// works, for the wrong person.
func TestEveryFolderCallerRefusesTheSameStrings(t *testing.T) {
	prov := &fakeProvisioner{}
	srv, router, app := storageTestEnv(t, prov)
	signer, err := artifact.NewS3Signer(context.Background(), artifact.S3SignerConfig{
		Endpoint: newCountingObjectStore(t).srv.URL, Region: "us-east-1", UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Signer: %v", err)
	}
	srv.storeSigner = signer
	res := seedStore(t, srv.db, "validated")
	grantFolder(t, srv.db, res.ID, "root", "", runtime.AccessFull)

	for _, folder := range []string{
		"../escape",
		"analytics/../../escape",
		"analytics//double",
		"/leading",
		"back\\slash",
		"control\x01char",
		strings.Repeat("a", 4096),
	} {
		t.Run(folder, func(t *testing.T) {
			mapping := storageRequest(t, srv, router, http.MethodPut,
				"/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root",
				fmt.Sprintf(`{"path":"/data/x","access":"read","folder":%q}`, folder))
			if mapping.Code != http.StatusBadRequest {
				t.Errorf("the mapping route accepted it: %d %s", mapping.Code, mapping.Body.String())
			}

			grant := storageRequest(t, srv, router, http.MethodPut,
				"/api/v1/admin/storage/"+res.ID+"/grants", "root",
				fmt.Sprintf(`{"subject_type":"user","subject_id":"owner","access":"read","folder":%q}`, folder))
			if grant.Code != http.StatusBadRequest {
				t.Errorf("the grant route accepted it: %d %s", grant.Code, grant.Body.String())
			}

			listing := getAs(t, srv, router, "root",
				"/api/v1/store/validated/objects?path="+url.QueryEscape(folder))
			if listing.Code != http.StatusBadRequest {
				t.Errorf("the listing route accepted it: %d %s", listing.Code, listing.Body.String())
			}
		})
	}
}

// A folder marker is built by the folder routes alone, from a path, and a grant
// on a folder covers that folder's own marker but nothing beside or above it.
//
// The object routes keep refusing a trailing slash — otherwise a marker could be
// fetched or deleted as if it were a file, and the one place a slash-terminated
// key is minted would stop being one place.
func TestFolderMarkersAreMintedFromAPath(t *testing.T) {
	bucket := newCountingObjectStore(t)
	srv := storeScopeServer(t, bucket)
	seedUser(t, srv.db, "analyst", database.SystemRoleViewer)
	seedUser(t, srv.db, "reader", database.SystemRoleViewer)
	res := seedStore(t, srv.db, "reports")
	grantFolder(t, srv.db, res.ID, "analyst", "analytics", runtime.AccessWrite)
	grantFolder(t, srv.db, res.ID, "reader", "", runtime.AccessRead)
	router := srv.Router(http.NotFoundHandler())

	for _, tc := range []struct {
		name, who, path string
		want            int
	}{
		{"a subfolder of the grant", "analyst", "analytics/2026", http.StatusOK},
		{"the granted folder itself", "analyst", "analytics/", http.StatusOK},
		{"a sibling", "analyst", "marketing", http.StatusForbidden},
		{"the folder above", "analyst", "", http.StatusBadRequest},
		{"a traversal", "analyst", "analytics/../payroll", http.StatusBadRequest},
		{"a leading slash", "analyst", "/analytics/x", http.StatusBadRequest},
		{"read is not enough", "reader", "anything", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"path": tc.path})
			w := sendAs(t, srv, router, tc.who, http.MethodPost, "/api/v1/store/reports/folders", string(body))
			if w.Code != tc.want {
				t.Fatalf("creating %q = %d, want %d: %s", tc.path, w.Code, tc.want, w.Body.String())
			}
			if w.Code != http.StatusOK {
				return
			}
			var signed storeSignedResponse
			if err := json.Unmarshal(w.Body.Bytes(), &signed); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			wantKey := strings.TrimSuffix(tc.path, "/") + "/"
			if signed.Key != wantKey || signed.Method != http.MethodPut {
				t.Errorf("signed %s %q, want PUT %q", signed.Method, signed.Key, wantKey)
			}
			u, _ := url.Parse(signed.URL)
			if !strings.HasSuffix(u.Path, "/acme-data/team/reports/"+wantKey) {
				t.Errorf("URL path %q is not the marker under the store's prefix", u.Path)
			}
			// write is new-objects-only, for a marker exactly as for a file.
			if signed.Headers["If-None-Match"] != "*" {
				t.Errorf("headers %v carry no If-None-Match at `write`", signed.Headers)
			}
		})
	}

	if w := sendAs(t, srv, router, "analyst", http.MethodDelete,
		"/api/v1/store/reports/folders/analytics/2026", ""); w.Code != http.StatusForbidden {
		t.Errorf("deleting a marker at `write` = %d, want 403: %s", w.Code, w.Body.String())
	}
	grantFolder(t, srv.db, res.ID, "analyst", "analytics", runtime.AccessFull)
	if w := sendAs(t, srv, router, "analyst", http.MethodDelete,
		"/api/v1/store/reports/folders/analytics/2026/", ""); w.Code != http.StatusOK {
		t.Errorf("deleting a marker at `full` = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := getAs(t, srv, router, "analyst", "/api/v1/store/reports/objects/analytics/2026/"); w.Code != http.StatusBadRequest {
		t.Errorf("a marker addressed through the object route = %d, want 400", w.Code)
	}
	if n := bucket.hits.Load(); n != 0 {
		t.Errorf("minting asked the object store %d time(s), want 0", n)
	}
}

// An empty file is part of a tree, so a zero size is accepted — and its URL
// still carries the empty body's MD5, which is what binds it on S3.
func TestAnEmptyFileCanBeUploaded(t *testing.T) {
	bucket := newCountingObjectStore(t)
	srv := storeScopeServer(t, bucket)
	seedUser(t, srv.db, "owner", database.SystemRoleViewer)
	res := seedStore(t, srv.db, "reports")
	grantFolder(t, srv.db, res.ID, "owner", "", runtime.AccessFull)
	body := `{"name":"pkg/__init__.py","size":0,` +
		`"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",` +
		`"md5":"d41d8cd98f00b204e9800998ecf8427e"}`
	w := sendAs(t, srv, srv.Router(http.NotFoundHandler()), "owner", http.MethodPost, "/api/v1/store/reports/objects", body)
	if w.Code != http.StatusOK {
		t.Fatalf("uploading an empty file = %d, want 200: %s", w.Code, w.Body.String())
	}
	var signed storeSignedResponse
	_ = json.Unmarshal(w.Body.Bytes(), &signed)
	if signed.Headers["Content-Md5"] == "" && signed.Headers["Content-MD5"] == "" {
		t.Errorf("headers %v carry no Content-MD5 — nothing would bind the empty body", signed.Headers)
	}
	if w := sendAs(t, srv, srv.Router(http.NotFoundHandler()), "owner", http.MethodPost,
		"/api/v1/store/reports/objects", strings.Replace(body, `"size":0`, `"size":-1`, 1)); w.Code != http.StatusBadRequest {
		t.Errorf("a negative size = %d, want 400", w.Code)
	}
}
