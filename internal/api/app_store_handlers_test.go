package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// An app reaching a store through its mapping, over signed URLs.

type appStoreFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
	other  *database.App
	res    *database.StorageResource
	token  string
	otherT string
}

func newAppStoreFixture(t *testing.T) *appStoreFixture {
	t.Helper()
	srv := storeScopeServer(t, newCountingObjectStore(t))
	f := &appStoreFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	for _, a := range []**database.App{&f.app, &f.other} {
		slug := "recon"
		if a == &f.other {
			slug = "other"
		}
		*a = &database.App{Slug: slug, Name: slug, OwnerID: "owner", Framework: "flask",
			Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
		if err := srv.db.CreateApp(*a); err != nil {
			t.Fatalf("create app: %v", err)
		}
	}
	f.res = seedStore(t, srv.db, "uploads")
	data := &appDataFixture{srv: srv}
	f.token = data.mintToken(t, f.app.ID, "tok-store")
	f.otherT = data.mintToken(t, f.other.ID, "tok-other")
	return f
}

func (f *appStoreFixture) mapAt(t *testing.T, access, folder string) {
	t.Helper()
	m := &database.StorageMapping{ResourceID: f.res.ID, AppID: f.app.ID,
		Path: "/data/uploads/" + folder, Access: access, Folder: folder}
	if err := f.srv.db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
}

func (f *appStoreFixture) call(t *testing.T, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

// A zero-byte upload's request: the empty body's digests, in hex.
const emptyUpload = `{"name":"%s","size":0,` +
	`"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",` +
	`"md5":"d41d8cd98f00b204e9800998ecf8427e"}`

func uploadBody(name string) string { return strings.Replace(emptyUpload, "%s", name, 1) }

// **A test whose failure would be silent.** The app's URL is bounded by its
// mapping exactly as a person's is by a grant: `write` signs If-None-Match, and
// a rung the mapping does not hold mints nothing. Drift upward here is an
// escalation every other test passes through — the URL simply works.
func TestAnAppsURLIsNeverMoreGenerousThanItsMapping(t *testing.T) {
	for _, tc := range []struct {
		access              string
		put, del, ifNoneHit bool
	}{
		{runtime.AccessRead, false, false, false},
		{runtime.AccessWrite, true, false, true},
		{runtime.AccessFull, true, true, false},
	} {
		t.Run(tc.access, func(t *testing.T) {
			f := newAppStoreFixture(t)
			f.mapAt(t, tc.access, "")

			if w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects/a.csv", ""); w.Code != http.StatusOK {
				t.Errorf("GET at %s = %d: %s", tc.access, w.Code, w.Body.String())
			}
			w := f.call(t, f.token, http.MethodPost, "/api/v1/app/store/uploads/objects", uploadBody("a.csv"))
			if got := w.Code == http.StatusOK; got != tc.put {
				t.Fatalf("PUT at %s = %d, want allowed=%v: %s", tc.access, w.Code, tc.put, w.Body.String())
			}
			if tc.put {
				var signed storeSignedResponse
				_ = json.Unmarshal(w.Body.Bytes(), &signed)
				if got := signed.Headers["If-None-Match"] == "*"; got != tc.ifNoneHit {
					t.Errorf("PUT at %s signed If-None-Match=%v, want %v", tc.access, got, tc.ifNoneHit)
				}
				u, _ := url.Parse(signed.URL)
				if !strings.HasSuffix(u.Path, "/acme-data/team/uploads/a.csv") {
					t.Errorf("signed path %q is not the key under the store's prefix", u.Path)
				}
			}
			w = f.call(t, f.token, http.MethodDelete, "/api/v1/app/store/uploads/objects/a.csv", "")
			if got := w.Code == http.StatusOK; got != tc.del {
				t.Errorf("DELETE at %s = %d, want allowed=%v", tc.access, w.Code, tc.del)
			}
			// A refusal names the level the verb needs: the hint is what an
			// agent acts on (drills, 2026-10-28 evening #1).
			if !tc.del {
				var body map[string]string
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if body["code"] != "insufficient_access" || !strings.Contains(body["hint"], `"full"`) {
					t.Errorf("DELETE refusal at %s = %v, want insufficient_access with a hint naming full", tc.access, body)
				}
			}
		})
	}
}

// **A test whose failure would be silent.** These routes are for an app acting
// as itself. A person's session, a personal key and an app key are the wrong
// direction, and a route that admitted one would hand a store to a caller its
// grants never mentioned.
func TestAppStoreRoutesAdmitOnlyTheAppToken(t *testing.T) {
	f := newAppStoreFixture(t)
	f.mapAt(t, runtime.AccessFull, "")
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		path := "/api/v1/app/store/uploads/objects"
		if method == http.MethodDelete {
			path += "/a.csv"
		}
		req := httptest.NewRequest(method, path, strings.NewReader(uploadBody("a.csv")))
		req.AddCookie(sessionCookieFor(t, f.srv, "owner"))
		w := httptest.NewRecorder()
		f.router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with a session = %d, want 401", method, w.Code)
		}
		for _, bearer := range []string{"toft_somekey", "toftu_somekey"} {
			req := httptest.NewRequest(method, path, strings.NewReader(uploadBody("a.csv")))
			req.Header.Set("Authorization", "Bearer "+bearer)
			w := httptest.NewRecorder()
			f.router.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s with %s = %d, want 401", method, bearer[:6], w.Code)
			}
		}
	}
}

// The mapping is resolved for the token's own app and read on every request:
// another app's mapping is not this app's, an ambiguous slug is named rather than
// guessed, and a mapping removed refuses the very next mint.
func TestAnAppReachesOnlyItsOwnMappingAsItStandsNow(t *testing.T) {
	f := newAppStoreFixture(t)
	f.mapAt(t, runtime.AccessWrite, "in")

	if w := f.call(t, f.otherT, http.MethodGet, "/api/v1/app/store/uploads/objects", ""); w.Code != http.StatusNotFound {
		t.Errorf("another app = %d, want 404: %s", w.Code, w.Body.String())
	}

	// The folder is part of the key, and the caller's name is under it.
	w := f.call(t, f.token, http.MethodPost, "/api/v1/app/store/uploads/objects", uploadBody("x.csv"))
	if w.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", w.Code, w.Body.String())
	}
	var signed storeSignedResponse
	_ = json.Unmarshal(w.Body.Bytes(), &signed)
	if u, _ := url.Parse(signed.URL); !strings.HasSuffix(u.Path, "/acme-data/team/uploads/in/x.csv") {
		t.Errorf("signed path %q is not under the mapping's folder", u.Path)
	}
	if w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects?path=../payroll", ""); w.Code != http.StatusBadRequest {
		t.Errorf("a traversal out of the folder = %d, want 400", w.Code)
	}

	// Two mappings of one slug: no folder is a 409 naming both; a folder picks one.
	f.mapAt(t, runtime.AccessRead, "")
	if w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects", ""); w.Code != http.StatusConflict {
		t.Errorf("ambiguous = %d, want 409: %s", w.Code, w.Body.String())
	}
	if w := f.call(t, f.token, http.MethodPost, "/api/v1/app/store/uploads/objects?folder=", uploadBody("y.csv")); w.Code != http.StatusForbidden {
		t.Errorf("the read mapping took an upload: %d", w.Code)
	}

	folder := "in"
	if _, err := f.srv.db.DeleteStorageMapping(f.res.ID, f.app.ID, &folder, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageMapping: %v", err)
	}
	if w := f.call(t, f.token, http.MethodPost, "/api/v1/app/store/uploads/objects?folder=in", uploadBody("z.csv")); w.Code != http.StatusNotFound {
		t.Errorf("a removed mapping minted: %d", w.Code)
	}
}

// A volume is reached at its path; the store routes do not answer for one.
func TestAVolumeMappingTakesNoStoreRoute(t *testing.T) {
	f := newAppStoreFixture(t)
	vol := &database.StorageResource{Name: "cache", Slug: "cache", Kind: database.StorageKindVolume}
	if err := f.srv.db.CreateStorageResource(vol, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	m := &database.StorageMapping{ResourceID: vol.ID, AppID: f.app.ID, Path: "/data/cache", Access: runtime.AccessWrite}
	if err := f.srv.db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
	w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/cache/objects", "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not_a_store") {
		t.Errorf("volume = %d %s, want 404 not_a_store", w.Code, w.Body.String())
	}
}

// Every mint is an audit row with the app as its actor — "the app's code asked"
// is a different fact from "its owner did".
func TestAnAppsMintIsAuditedAsTheApp(t *testing.T) {
	f := newAppStoreFixture(t)
	f.mapAt(t, runtime.AccessWrite, "in")
	if w := f.call(t, f.token, http.MethodPost, "/api/v1/app/store/uploads/objects", uploadBody("x.csv")); w.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", w.Code, w.Body.String())
	}
	var actorType, actorID, after string
	if err := f.srv.db.Conn().QueryRow(
		`SELECT actor_type, actor_id, after::text FROM audit_log WHERE action = 'store.presign'`).
		Scan(&actorType, &actorID, &after); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if actorType != database.PrincipalTypeApp || actorID != f.app.ID {
		t.Errorf("actor = %s/%s, want app/%s", actorType, actorID, f.app.ID)
	}
	for _, want := range []string{`"folder": "in"`, `team/uploads/in/x.csv`, `"ttl": "15m0s"`} {
		if !strings.Contains(after, want) {
			t.Errorf("audit after %s lacks %s", after, want)
		}
	}
}

// The store routes spend a bucket of their own: an app walking a tree must not
// starve its own notifications, and its notifications must not cap the walk.
func TestTheStoreRoutesHaveTheirOwnBucket(t *testing.T) {
	f := newAppStoreFixture(t)
	f.mapAt(t, runtime.AccessRead, "")
	for i := 0; i < appTokenBurst+5; i++ {
		if w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects/a.csv", ""); w.Code != http.StatusOK {
			t.Fatalf("store call %d = %d, want 200 — the store routes are sharing the app's bucket", i, w.Code)
		}
	}
	if w := f.call(t, f.token, http.MethodGet, "/api/v1/app/self", ""); w.Code == http.StatusTooManyRequests {
		t.Error("the store calls spent the app's own bucket")
	}
}

// A spent bucket says how long until it has a token again -- a second at the
// app rate -- not the minute the rate is written in. The SDKs honour
// Retry-After, and the flat 60 this used to send made an app that spent its
// burst in one page view sleep through the gateway's own timeout.
func TestASpentBucketSaysWhenItRefills(t *testing.T) {
	f := newAppStoreFixture(t)
	var last int
	var retry string
	for i := 0; i < appTokenBurst+5; i++ {
		w := f.call(t, f.token, http.MethodGet, "/api/v1/app/self", "")
		last, retry = w.Code, w.Header().Get("Retry-After")
		if last == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("the app bucket never ran out (last %d)", last)
	}
	if retry != "1" {
		t.Errorf("Retry-After = %q, want 1 -- the bucket refills one token a second", retry)
	}
	for perMinute, want := range map[int]string{60: "1", 600: "1", 30: "2", 7: "9", 0: "60"} {
		if got := retryAfterFor(perMinute); got != want {
			t.Errorf("retryAfterFor(%d) = %q, want %q", perMinute, got, want)
		}
	}
}

// The per-token maps are swept: a refilled bucket and a stale touch go, a
// bucket still holding back its caller stays. Keyed by token and by run, they
// otherwise grew for the life of the process.
func TestAppTokenStateIsSwept(t *testing.T) {
	s := &Server{}
	s.allowAppTokenAt("run:idle", 60, 10)
	for i := 0; i < 10; i++ {
		s.allowAppTokenAt("run:busy", 1, 10) // emptied, refills a token a minute
	}
	s.appTokenTouched.Store("old-token", time.Now().Add(-2*time.Minute))

	// A minute on, the idle bucket has refilled and the busy one has not.
	s.sweepAppTokenState(time.Now().Add(appTokenSweepEvery + time.Second))
	if _, ok := s.appTokenLimiters.Load("run:idle"); ok {
		t.Error("a refilled bucket was kept")
	}
	if _, ok := s.appTokenLimiters.Load("run:busy"); !ok {
		t.Error("a bucket still limiting its caller was dropped")
	}
	if _, ok := s.appTokenTouched.Load("old-token"); ok {
		t.Error("a touch older than its throttle window was kept")
	}
}

// An install with stores mounted but no signer tells the app why a signed-URL
// call has nothing behind it, by name -- a bare 404 on a store the app can see
// in mounts() read as "not mapped" (the eks drills, 2026-10-22).
func TestNoStoreSignerIsANamedAbsenceToAnApp(t *testing.T) {
	f := newAppStoreFixture(t)
	f.mapAt(t, runtime.AccessRead, "")
	f.srv.storeSigner, f.srv.hostStore = nil, nil
	w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects/a.csv", "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"store_uploads_off"`) {
		t.Errorf("no signer = %d %s, want 404 store_uploads_off", w.Code, w.Body.String())
	}
}

// An escaped slash makes chi route on RawPath and hand the wildcard back still
// escaped, so `..%2F..%2Fetc%2Fpasswd` once reached the validator as one
// segment and was signed as a literal key. Decoded first, it is the traversal
// it spells and is refused; an escaped ordinary name still signs the decoded key.
func TestAnEscapedSlashIsDecodedBeforeTheNameIsValidated(t *testing.T) {
	f := newAppStoreFixture(t)
	f.mapAt(t, runtime.AccessFull, "")

	for _, bad := range []string{"..%2F..%2Fetc%2Fpasswd", "a%2F..%2F..%2Fb", "%2Fetc%2Fpasswd"} {
		if w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects/"+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400: %s", bad, w.Code, w.Body.String())
		}
	}
	w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects/dir%2Fa.csv", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET dir%%2Fa.csv = %d: %s", w.Code, w.Body.String())
	}
	var signed storeSignedResponse
	_ = json.Unmarshal(w.Body.Bytes(), &signed)
	u, _ := url.Parse(signed.URL)
	if !strings.HasSuffix(u.Path, "/acme-data/team/uploads/dir/a.csv") {
		t.Errorf("signed path %q is not the decoded key under the store's prefix", u.Path)
	}
}

// An upload signs the name as sent, exactly as every other verb takes it: a
// name trimmed only on upload wrote "q1.csv" for "q1.csv " while the SDK
// returned the untrimmed name, which then addressed nothing. A control
// character is a 400 on upload as on every other verb.
func TestUploadSignsTheNameAsSent(t *testing.T) {
	f := newAppStoreFixture(t)
	f.mapAt(t, runtime.AccessFull, "")

	signedPath := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("= %d: %s", w.Code, w.Body.String())
		}
		var signed storeSignedResponse
		_ = json.Unmarshal(w.Body.Bytes(), &signed)
		u, _ := url.Parse(signed.URL)
		return u.Path
	}
	put := signedPath(f.call(t, f.token, http.MethodPost, "/api/v1/app/store/uploads/objects", uploadBody("q1.csv ")))
	if !strings.HasSuffix(put, "/acme-data/team/uploads/q1.csv ") {
		t.Errorf("upload signed %q, want the key with its trailing space", put)
	}
	get := signedPath(f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects/q1.csv%20", ""))
	if get != put {
		t.Errorf("GET signed %q, upload signed %q: one name, two keys", get, put)
	}
	if w := f.call(t, f.token, http.MethodPost, "/api/v1/app/store/uploads/objects", uploadBody(`q1.csv\n`)); w.Code != http.StatusBadRequest {
		t.Errorf("upload of a name ending in a newline = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// A store declared in toft.toml is reachable by URL at the flip and not
// before: the route follows the table, which is the serving release's, and an
// app token cannot say which release holds it (D21). The refusal says so.
func TestAStoreDeclaredInTheManifestIsReachableAtTheFlip(t *testing.T) {
	f := newAppStoreFixture(t)
	if err := f.srv.db.PutStorageManager(&database.StorageManager{ResourceID: f.res.ID,
		SubjectType: database.StoreSubjectUser, SubjectID: "owner", MaxAccess: "full"},
		database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageManager: %v", err)
	}
	p, err := f.srv.PlanManifestStorage(builder.StoragePlanRequest{App: f.app,
		Deployer: builder.Deployer{Type: database.DeployerUser, ID: "owner"},
		Entries:  []builder.StorageEntry{{Name: "uploads", Access: "read", Path: "/storage/uploads"}}})
	if err != nil || len(p.Add) != 1 {
		t.Fatalf("plan = %+v, %v", p, err)
	}

	w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects", "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "reachable at the end of the deploy that adds it") {
		t.Fatalf("before the flip = %d %s", w.Code, w.Body.String())
	}
	if err := f.srv.ReconcileManifestStorage(f.app, p, builder.Deployer{Type: database.DeployerUser, ID: "owner"}); err != nil {
		t.Fatalf("flip: %v", err)
	}
	if w := f.call(t, f.token, http.MethodGet, "/api/v1/app/store/uploads/objects", ""); w.Code != http.StatusOK {
		t.Fatalf("after the flip = %d %s", w.Code, w.Body.String())
	}
}
