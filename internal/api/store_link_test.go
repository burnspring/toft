package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// Durable storage links: a name at the platform's address, opened by store
// grants alone (store_link_handlers.go).

// headBucket answers HEAD for the keys it holds and 404 for the rest — the one
// question a link's open asks the store before it mints.
type headBucket struct {
	mu   sync.Mutex
	keys map[string]bool
	srv  *httptest.Server
}

func newHeadBucket(t *testing.T, keys ...string) *headBucket {
	t.Helper()
	b := &headBucket{keys: map[string]bool{}}
	for _, k := range keys {
		b.keys[k] = true
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		// Path style: /<bucket>/<key>.
		key := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		if r.Method == http.MethodHead && len(key) == 2 && b.keys[key[1]] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

type linkFixture struct {
	srv    *Server
	router http.Handler
	res    *database.StorageResource
	app    *database.App
	token  string
}

func newLinkFixture(t *testing.T, keys ...string) *linkFixture {
	t.Helper()
	bucket := newHeadBucket(t, keys...)
	db := dbtest.New(t)
	srv := newTestServerWithConfig(t, db, &config.Config{
		BaseURL: "http://localhost:8080", StoreUploads: true,
	})
	signer, err := artifact.NewS3Signer(context.Background(), artifact.S3SignerConfig{
		Endpoint: bucket.srv.URL, Region: "us-east-1", UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Signer: %v", err)
	}
	srv.storeSigner = signer
	srv.storage = &fakeProvisioner{}
	f := &linkFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}
	seedUser(t, db, "owner", database.SystemRolePublisher)
	seedUser(t, db, "analyst", database.SystemRoleViewer)
	seedUser(t, db, "stranger", database.SystemRoleViewer)
	f.res = seedStore(t, db, "reports")
	f.app = &database.App{Slug: "nightly", Name: "nightly", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := db.CreateApp(f.app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	f.token = (&appDataFixture{srv: srv}).mintToken(t, f.app.ID, "tok-link")
	return f
}

func (f *linkFixture) open(t *testing.T, who, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, f.srv, who))
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *linkFixture) mapAt(t *testing.T, access, folder string) {
	t.Helper()
	m := &database.StorageMapping{ResourceID: f.res.ID, AppID: f.app.ID,
		Path: "/data/reports/" + folder, Access: access, Folder: folder}
	if err := f.srv.db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
}

func (f *linkFixture) searchActivity(t *testing.T, q string) []string {
	t.Helper()
	entries, err := f.srv.db.ListActivity(database.ActivityFilter{Query: q, Source: "audit", Limit: 50})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	return actions
}

// A grant holder is redirected to a signed GET, and the open is the person's
// own store.presign row, marked as arriving by link and found by the file's
// name.
func TestAStorageLinkRedirectsAGrantHolderAndIsAudited(t *testing.T) {
	f := newLinkFixture(t, "team/reports/2026-11/summary.pdf")
	grantFolder(t, f.srv.db, f.res.ID, "analyst", "", runtime.AccessRead)

	w := f.open(t, "analyst", "/files/reports/2026-11/summary.pdf")
	if w.Code != http.StatusFound {
		t.Fatalf("open = %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "team/reports/2026-11/summary.pdf") || !strings.Contains(loc, "X-Amz-Signature") {
		t.Errorf("Location %q is not a signed GET of the object", loc)
	}
	// BB-67: the address the click lands on is short-lived, so a revoke is
	// not outlived by an hour-long bearer URL in the browser's history.
	if !strings.Contains(loc, "X-Amz-Expires=60&") && !strings.HasSuffix(loc, "X-Amz-Expires=60") {
		t.Errorf("Location %q does not expire in the link-open TTL (60 s)", loc)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a redirect carrying a credential", cc)
	}
	var actorID, after string
	if err := f.srv.db.Conn().QueryRow(
		`SELECT actor_id, after::text FROM audit_log WHERE action = 'store.presign'`).Scan(&actorID, &after); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if actorID != "analyst" || !strings.Contains(after, `"via": "link"`) {
		t.Errorf("audit = %s %s, want the reader as actor and via link", actorID, after)
	}
	if got := f.searchActivity(t, "summary.pdf"); len(got) != 1 || got[0] != "store.presign" {
		t.Errorf("searching the file's name found %v, want the one open", got)
	}
}

// A reader with no grant and a link to a file that is not there get the same
// answer, byte for byte — and both refusals are audited and found by name.
func TestAnUnreadableFileAndAMissingOneAreTheSameCard(t *testing.T) {
	f := newLinkFixture(t, "team/reports/secret.pdf")
	grantFolder(t, f.srv.db, f.res.ID, "analyst", "", runtime.AccessRead)

	unreadable := f.open(t, "stranger", "/files/reports/secret.pdf")
	missing := f.open(t, "analyst", "/files/reports/nothing-here.pdf")
	noStore := f.open(t, "analyst", "/files/nosuchstore/secret.pdf")
	for name, w := range map[string]*httptest.ResponseRecorder{"unreadable": unreadable, "missing": missing, "no such store": noStore} {
		if w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", name, w.Code)
		}
	}
	if unreadable.Body.String() != missing.Body.String() || missing.Body.String() != noStore.Body.String() {
		t.Error("the refusals differ: an unreadable file can be told from a missing one")
	}
	if strings.Contains(unreadable.Body.String(), "secret") || strings.Contains(unreadable.Body.String(), "reports") {
		t.Error("the card names the file or the store")
	}
	if csp := unreadable.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("the card's CSP is %q", csp)
	}
	if got := f.searchActivity(t, "secret.pdf"); len(got) != 2 {
		t.Errorf("searching the refused file's name found %v, want both refused opens", got)
	}
	for _, a := range f.searchActivity(t, "nothing-here") {
		if a != "store.link.refused" {
			t.Errorf("a missing file's open was audited as %s", a)
		}
	}
}

// Nobody signed in is sent to sign in, and back to the link.
func TestAStorageLinkSendsAnAnonymousReaderToSignIn(t *testing.T) {
	f := newLinkFixture(t, "team/reports/a b.pdf")
	w := f.open(t, "", "/files/reports/a%20b.pdf")
	if w.Code != http.StatusFound {
		t.Fatalf("anonymous open = %d, want a sign-in redirect", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Path != "/login" || loc.Query().Get("redirect") != "http://localhost:8080/files/reports/a%20b.pdf" {
		t.Errorf("redirect = %s", w.Header().Get("Location"))
	}
}

// The grant is read on every open: revoking it refuses the next one, and a
// folder grant reaches only its folder.
func TestAStorageLinkFollowsTheGrantAsItStandsNow(t *testing.T) {
	f := newLinkFixture(t, "team/reports/finance/q1.csv", "team/reports/hr/people.csv")
	grantFolder(t, f.srv.db, f.res.ID, "analyst", "finance", runtime.AccessRead)

	if w := f.open(t, "analyst", "/files/reports/finance/q1.csv"); w.Code != http.StatusFound {
		t.Fatalf("inside the folder = %d", w.Code)
	}
	if w := f.open(t, "analyst", "/files/reports/hr/people.csv"); w.Code != http.StatusNotFound {
		t.Errorf("outside the folder = %d, want 404", w.Code)
	}

	if err := f.srv.db.DeleteStoreGrant(f.res.ID, database.StoreSubjectUser, "analyst", "finance", database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if w := f.open(t, "analyst", "/files/reports/finance/q1.csv"); w.Code != http.StatusNotFound {
		t.Errorf("after revoking = %d, want 404", w.Code)
	}
}

// An app builds the link through its mapping: the name is under the mapping's
// folder, the address is the platform's, and the build is audited as the app.
func TestAnAppBuildsALinkThroughItsMapping(t *testing.T) {
	f := newLinkFixture(t)
	f.mapAt(t, runtime.AccessWrite, "out")

	w := (&appStoreFixture{srv: f.srv, router: f.router}).call(t, f.token, http.MethodGet,
		"/api/v1/app/store/reports/link/2026-11/q1%20summary.pdf", "")
	if w.Code != http.StatusOK {
		t.Fatalf("link = %d: %s", w.Code, w.Body.String())
	}
	var got struct{ Link, Name string }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := "http://localhost:8080/files/reports/out/2026-11/q1%20summary.pdf"; got.Link != want {
		t.Errorf("link = %q, want %q", got.Link, want)
	}
	var actorType string
	if err := f.srv.db.Conn().QueryRow(`SELECT actor_type FROM audit_log WHERE action = 'store.link'`).Scan(&actorType); err != nil {
		t.Fatalf("no store.link row: %v", err)
	}
	if actorType != database.PrincipalTypeApp {
		t.Errorf("store.link actor = %s, want app", actorType)
	}

	// A name that escapes, and a store the app is not mapped to, are refused.
	for path, want := range map[string]int{
		"/api/v1/app/store/reports/link/..%2F..%2Fetc": http.StatusBadRequest,
		"/api/v1/app/store/elsewhere/link/x.pdf":       http.StatusNotFound,
	} {
		if w := (&appStoreFixture{srv: f.srv, router: f.router}).call(t, f.token, http.MethodGet, path, ""); w.Code != want {
			t.Errorf("%s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
	}
}

// A job's run token builds a link — the case the feature exists for.
func TestARunTokenBuildsALink(t *testing.T) {
	f := newLinkFixture(t)
	f.mapAt(t, runtime.AccessFull, "")
	run := &database.JobRun{AppID: f.app.ID, JobName: "report"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='running' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	tok, err := f.srv.jwt.MintRunToken(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	w := (&appStoreFixture{srv: f.srv, router: f.router}).call(t, tok, http.MethodGet, "/api/v1/app/store/reports/link/x.pdf", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/files/reports/x.pdf") {
		t.Errorf("a run token building a link = %d: %s", w.Code, w.Body.String())
	}
}

func TestStorageLinkEncodingIsPerSegment(t *testing.T) {
	for name, want := range map[string]string{
		"a.pdf":              "https://t.example/files/s/a.pdf",
		"2026/q1 report.csv": "https://t.example/files/s/2026/q1%20report.csv",
		"a#b?c%d.txt":        "https://t.example/files/s/a%23b%3Fc%25d.txt",
	} {
		if got := storageLinkFor("https://t.example/", "s", name); got != want {
			t.Errorf("storageLinkFor(%q) = %q, want %q", name, got, want)
		}
	}
}
