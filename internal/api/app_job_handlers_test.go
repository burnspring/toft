package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/jobs"
	"toft/internal/runtime"
)

// The app-facing job surface, driven through the real router.
//
// Everything here is about one of two claims. Either an app cannot reach past
// itself — no other app's runs, no other person's runs, no forged viewer — or a
// person's identity survives the round trip through the app without the app
// ever holding it.

// --- Harness ---

type appJobFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
	other  *database.App
	token  string // the app's tofta_ secret
	def    *database.JobDefinition
}

// jobBackend is a backend that accepts everything. The dispatcher is not what
// these tests are about; that it exists is what makes jobs "available".
type jobBackend struct{}

func (jobBackend) Start(context.Context, runtime.WorkloadSpec) (string, error) { return "wl-1", nil }
func (jobBackend) Status(context.Context, string) (runtime.JobStatus, error) {
	return runtime.JobStatus{State: runtime.JobRunning}, nil
}
func (jobBackend) Cancel(context.Context, string) error { return nil }

func newAppJobFixture(t *testing.T, mode runtime.Mode) *appJobFixture {
	t.Helper()

	srv := newTestServer(t)
	store, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	srv.artifacts = store
	srv.jobs = jobs.New(srv.db, jobBackend{}, jobs.Options{NodeID: "test",
		Dials: config.NewResolver(&config.Config{AppMemoryLimitMB: 512}, srv.db)})

	// Jobs are an install capability. Both switches on, because every refusal
	// this file cares about is a different one.
	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	settings.IsolatedEnabled, settings.JobsEnabled = true, true
	settings.JobConcurrency = 3
	if err := srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("enable jobs: %v", err)
	}

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "priya", database.SystemRoleViewer)
	seedUser(t, srv.db, "sam", database.SystemRoleViewer)

	f := &appJobFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}

	f.app = &database.App{
		Slug: "recon", Name: "Reconciliation", OwnerID: "owner",
		Framework: "streamlit", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
		// Readable by anyone signed in, because the whole point of
		// viewer_runnable is publishing a button to people who are not owners.
		ExecutionMode: string(mode),
		ReleaseRef:    "artifact:releases/recon/dep-1.tar.gz",
	}
	if err := srv.db.CreateApp(f.app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	f.other = &database.App{
		Slug: "other", Name: "Other", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
		ReleaseRef: "artifact:releases/other/dep-1.tar.gz",
	}
	if err := srv.db.CreateApp(f.other); err != nil {
		t.Fatalf("create other app: %v", err)
	}

	f.def = declareJob(t, srv.db, f.app.ID, database.JobDefinition{
		Name: "reconcile", Command: "python -m reports.reconcile", TimeoutSec: 1800,
	})

	f.token = f.mintToken(t, f.app.ID)
	return f
}

func (f *appJobFixture) mintToken(t *testing.T, appID string) string {
	t.Helper()
	secret := appTokenSecretPrefix + appID + "-secret"
	if err := f.srv.db.CreateAppToken(&database.AppToken{
		ID: "tok-" + appID, AppID: appID, TokenHash: hashAPIKey(secret), Prefix: secret[:8],
	}); err != nil {
		t.Fatalf("create app token: %v", err)
	}
	return secret
}

// actFor mints the blob the proxy would have set, for a live session of `who`.
func (f *appJobFixture) actFor(t *testing.T, who, appID string) string {
	t.Helper()
	version, err := f.srv.db.GetTokenVersion(who)
	if err != nil {
		t.Fatalf("token version for %s: %v", who, err)
	}
	return f.srv.jwt.MintActFor(who, appID, version, time.Now().Add(time.Hour))
}

// call drives one request as the app, optionally relaying an act-for blob.
func (f *appJobFixture) call(t *testing.T, method, path, actForBlob, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+f.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.ContentLength = 0
	}
	if actForBlob != "" {
		req.Header.Set(auth.ActForHeader, actForBlob)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func decodeRun(t *testing.T, rec *httptest.ResponseRecorder) appJobRunResponse {
	t.Helper()
	var out appJobRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return out
}

// --- The subtree admits nothing but an app token ---

// A session cookie, an app key and a personal key are all the wrong direction
// for this surface. The drift test already proves every /api/v1 route is
// classified; this proves the exemption is not a hole.
func TestAppJobRoutesRefuseEveryCredentialButTheAppToken(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)

	paths := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/app/jobs/reconcile/runs"},
		{http.MethodGet, "/api/v1/app/job-runs"},
		{http.MethodGet, "/api/v1/app/job-runs/whatever"},
		{http.MethodGet, "/api/v1/app/job-runs/whatever/logs"},
		{http.MethodGet, "/api/v1/app/job-runs/whatever/output"},
		{http.MethodPost, "/api/v1/app/job-runs/whatever/cancel"},
	}

	credentials := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"a session cookie", func(r *http.Request) {
			r.AddCookie(sessionCookieFor(t, f.srv, "owner"))
		}},
		{"an app key", func(r *http.Request) {
			r.Header.Set("X-API-Key", retiredAppKeyPrefix+"whatever")
		}},
		{"a personal key", func(r *http.Request) {
			r.Header.Set("X-API-Key", userKeySecretPrefix+"whatever")
		}},
		{"nothing at all", func(*http.Request) {}},
	}

	for _, p := range paths {
		for _, c := range credentials {
			req := httptest.NewRequest(p.method, p.path, nil)
			c.apply(req)
			rec := httptest.NewRecorder()
			f.router.ServeHTTP(rec, req)

			// 401, not 403 and certainly not 200: the credential is not merely
			// insufficient, it is not the kind of credential this surface takes.
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: status %d, want 401",
					p.method, p.path, c.name, rec.Code)
			}
		}
	}
}

// --- Act-for ---

func TestActForRefusesAForgery(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)

	forged := []struct{ name, blob string }{
		{"nonsense", "not-a-token"},
		{"a payload with no signature", "a1.cHJpeWE.YXBw.1.99999999999"},
		{"a tampered signature", f.actFor(t, "priya", f.app.ID) + "x"},
	}
	for _, tc := range forged {
		// Signed by nobody, so it must not become a run in anybody's name.
		rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", tc.blob, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 — body %s", tc.name, rec.Code, rec.Body.String())
		}
	}

	// A blob for a *different* app is the case that matters most: it is the one
	// an attacker actually has, because any app they can view mints one for
	// them. Relaying it to a second app must buy nothing.
	stolen := f.actFor(t, "priya", f.other.ID)
	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", stolen, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("a blob minted for another app: status %d, want 403 — body %s",
			rec.Code, rec.Body.String())
	}
}

// The blob is a narrowed capability of one session, so it dies when that
// session does — which is what makes binding it to the session's own expiry
// rather than a short clock of its own safe.
func TestActForDiesWithItsSession(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	blob := f.actFor(t, "priya", f.app.ID)

	if rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", blob, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("a live session should start a run: %d %s", rec.Code, rec.Body.String())
	}

	// A logout, a demotion, a group change or a lock — all of them land here.
	if err := f.srv.db.BumpTokenVersion("priya"); err != nil {
		t.Fatalf("bump token version: %v", err)
	}

	rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs?scope=viewer", blob, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401 after the session was revoked — body %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "act_for_expired") {
		t.Errorf("the refusal should name the expiry so an app can say 'sign in again': %s",
			rec.Body.String())
	}
}

// No blob is not an error. It is the app acting as itself, which is a
// first-class case — and the run it produces has no actor, so nothing can later
// render it as a person.
func TestAnAbsentActForYieldsAnAppAttributedRun(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)

	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 — body %s", rec.Code, rec.Body.String())
	}
	got := decodeRun(t, rec)
	if got.StartedBy != database.JobStartedByApp {
		t.Errorf("started_by = %q, want app — nobody is relayed", got.StartedBy)
	}
	if got.StatusURL != "/apps/"+f.app.Slug+"/__toft/runs/"+got.ID || got.LogsURL == "" {
		t.Errorf("status_url = %q logs_url = %q", got.StatusURL, got.LogsURL)
	}

	run, err := f.srv.db.GetAppJobRun(f.app.ID, got.ID, "")
	if err != nil || run == nil {
		t.Fatalf("reading the run back: %v", err)
	}
	if run.ActorID != "" {
		t.Errorf("actor_id = %q, want empty — nobody started this", run.ActorID)
	}
	if run.StartedBy != database.JobStartedByApp {
		t.Errorf("started_by = %q, want %q", run.StartedBy, database.JobStartedByApp)
	}
}

// --- A viewer's start ---

// The platform does not gate a viewer's start by role: the app's own button is
// the gate, and a viewer can only reach this route through the app. What the
// platform does is resolve the person itself and put the run in their name.
func TestAViewersStartIsAttributedAndNotGatedByRole(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	declareJob(t, f.srv.db, f.app.ID, database.JobDefinition{Name: "rebuild-index", Command: "python -m ops.rebuild"})

	blob := f.actFor(t, "priya", f.app.ID)
	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/rebuild-index/runs", blob, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a viewer's start through the app: %d %s, want 202", rec.Code, rec.Body.String())
	}
	got := decodeRun(t, rec)
	if got.StartedBy != database.JobStartedByViewer {
		t.Errorf("started_by = %q, want viewer", got.StartedBy)
	}
	run, _ := f.srv.db.GetAppJobRun(f.app.ID, got.ID, "")
	if run == nil || run.ActorID != "priya" {
		t.Errorf("run = %+v, want it in priya's name", run)
	}
}

// --- Idempotency ---

// The naive call has to be correct without a key, because Streamlit guarantees
// the double-fire: a rerun re-executes the script top to bottom, so a start()
// in a button branch fires again on the next widget change.
func TestARepeatedStartCoalescesAndSaysSo(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	blob := f.actFor(t, "priya", f.app.ID)
	body := `{"params":{"month":"2026-03","ledgers":["a","b"]}}`

	first := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", blob, body))
	if first.Deduplicated {
		t.Error("the first start is not a duplicate")
	}

	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", blob, body)
	// 200 rather than 202: nothing new was accepted for processing.
	if rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200 on a dedup hit — body %s", rec.Code, rec.Body.String())
	}
	second := decodeRun(t, rec)
	if second.ID != first.ID {
		t.Errorf("got run %s, want the one already in flight (%s)", second.ID, first.ID)
	}
	if !second.Deduplicated {
		t.Error("deduplication must be reported — an app that appears to ignore a click " +
			"is indistinguishable from a broken one")
	}

	// Key order is not meaningful in JSON and a dict serialized twice can come
	// out either way round, so the same parameters spelled differently must
	// still coalesce.
	reordered := `{"params":{"ledgers":["a","b"],"month":"2026-03"}}`
	if got := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", blob, reordered)); got.ID != first.ID {
		t.Errorf("reordered params produced run %s, want %s — the key is canonical", got.ID, first.ID)
	}

	// Different parameters are a different request, and so is a different
	// person: the key includes the resolved user, which is why two people
	// asking for the same report do not collapse onto one run.
	if got := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", blob,
		`{"params":{"month":"2026-04"}}`)); got.ID == first.ID {
		t.Error("different parameters must start a new run")
	}
	samBlob := f.actFor(t, "sam", f.app.ID)
	if got := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", samBlob, body)); got.ID == first.ID {
		t.Error("a different person's identical request must be their own run")
	}

	// An explicit key overrides the derivation.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/app/jobs/reconcile/runs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.ActForHeader, blob)
	req.Header.Set("Idempotency-Key", "mine")
	rec = httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if got := decodeRun(t, rec); got.ID == first.ID {
		t.Error("an explicit key must override the derived one")
	}
}

// --- The queue ---

// Reading the app's own runs spends a bucket of its own: a few wait()s polling
// every two seconds spent the sixty a minute, and every start the app then made
// was refused rate_limited for as long as they polled (jobs-regression #14).
func TestPollingRunsDoesNotSpendTheAppsBucket(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	run := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", `{}`))
	for i := 0; i < appTokenBurst*3; i++ {
		if rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs/"+run.ID, "", ""); rec.Code != http.StatusOK {
			t.Fatalf("poll %d = %d, want 200 — the run reads are on the app's bucket", i, rec.Code)
		}
	}
	if rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", `{"params":{"n":2}}`); rec.Code == http.StatusTooManyRequests {
		t.Error("polling a run spent the bucket the app's starts are on")
	}
}

// Runs past the app's runs-at-once limit queue rather than being refused; the
// refusal comes only when the queue itself is at the install's cap, and it is
// retryable with a hint.
func TestAFullQueueRefusesAndSaysWhatToDo(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	if _, err := f.srv.db.PutSetting(database.SettingMaxQueuedRuns, "3", database.BuiltinAdminID); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"params":{"n":%d}}`, i)
		rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("run %d: status %d — body %s", i, rec.Code, rec.Body.String())
		}
		if got := decodeRun(t, rec); got.QueuePosition != i+1 || got.Status != database.JobRunQueued {
			t.Errorf("run %d: %s at position %d, want queued at %d", i, got.Status, got.QueuePosition, i+1)
		}
	}

	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", `{"params":{"n":99}}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 — body %s", rec.Code, rec.Body.String())
	}
	var refusal struct {
		Code       string `json:"code"`
		Retryable  bool   `json:"retryable"`
		RetryAfter int    `json:"retry_after"`
		Hint       string `json:"hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}
	if refusal.Code != "queue_full" || !refusal.Retryable || refusal.RetryAfter == 0 || refusal.Hint == "" {
		t.Errorf("refusal = %+v", refusal)
	}
}

// --- Cancel ---

func TestCancelBelongsToWhoeverStartedTheRun(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	priya := f.actFor(t, "priya", f.app.ID)
	sam := f.actFor(t, "sam", f.app.ID)

	hers := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", priya, `{"params":{"n":1}}`))

	// Somebody else, through the same app, must not be able to kill her work.
	// 404 rather than 403: whose run it is is not a fact to confirm.
	if rec := f.call(t, http.MethodPost, "/api/v1/app/job-runs/"+hers.ID+"/cancel", sam, ""); rec.Code != http.StatusNotFound {
		t.Errorf("another viewer cancelling: status %d, want 404", rec.Code)
	}
	// Nor may the app cancel it on nobody's behalf.
	if rec := f.call(t, http.MethodPost, "/api/v1/app/job-runs/"+hers.ID+"/cancel", "", ""); rec.Code != http.StatusForbidden {
		t.Errorf("the app cancelling a person's run: status %d, want 403", rec.Code)
	}
	// She may.
	if rec := f.call(t, http.MethodPost, "/api/v1/app/job-runs/"+hers.ID+"/cancel", priya, ""); rec.Code != http.StatusNoContent {
		t.Errorf("the starter cancelling: status %d, want 204", rec.Code)
	}
	// Finished, it is a no-op the app may ask for like any other (BB-021,
	// second 2026-11-03 bash): not act_for_required.
	if rec := f.call(t, http.MethodPost, "/api/v1/app/job-runs/"+hers.ID+"/cancel", "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("the app cancelling a person's finished run: status %d, want 204", rec.Code)
	}

	// A run with no actor is the app's own, and the app token alone may cancel it.
	its := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", `{"params":{"n":2}}`))
	if rec := f.call(t, http.MethodPost, "/api/v1/app/job-runs/"+its.ID+"/cancel", "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("the app cancelling its own run: status %d, want 204", rec.Code)
	}
}

// --- scope=viewer ---

// The app gets that person's runs and still never learns who they are.
func TestViewerScopeReturnsOnlyThatPersonsRuns(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	priya := f.actFor(t, "priya", f.app.ID)
	sam := f.actFor(t, "sam", f.app.ID)

	hers := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", priya, `{"params":{"n":1}}`))
	his := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", sam, `{"params":{"n":2}}`))
	itsOwn := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", `{"params":{"n":3}}`))

	list := func(blob string) []appJobRunResponse {
		t.Helper()
		rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs?scope=viewer", blob, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d — body %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Runs []appJobRunResponse `json:"runs"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Runs
	}

	got := list(priya)
	if len(got) != 1 || got[0].ID != hers.ID {
		t.Fatalf("priya's runs = %+v, want exactly %s", got, hers.ID)
	}

	// A second login by the same person — a new session, a new blob — still
	// finds the run she started on the first one. This is the whole "start it
	// on a laptop, check it from a phone" claim, and it works because the run
	// is a durable row keyed on the *user*, not on a session id.
	second := f.srv.jwt.MintActFor("priya", f.app.ID, mustTokenVersion(t, f, "priya"), time.Now().Add(2*time.Hour))
	if got := list(second); len(got) != 1 || got[0].ID != hers.ID {
		t.Errorf("a second login sees %+v, want %s", got, hers.ID)
	}

	// Nothing of Sam's, and nothing of the app's own.
	for _, r := range list(priya) {
		if r.ID == his.ID || r.ID == itsOwn.ID {
			t.Errorf("viewer scope leaked run %s", r.ID)
		}
	}

	// Without the header there is no viewer to scope to, and answering with
	// *every* run would be the silent widening this is arranged to prevent.
	if rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs?scope=viewer", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("scope=viewer with no act-for header: status %d, want 400", rec.Code)
	}

	// Authorization stays live. Lose read on the app and the list is empty,
	// because the filter runs against a principal the platform resolved itself.
	f.app.Visibility = database.VisibilityPrivate
	if err := f.srv.db.UpdateApp(f.app); err != nil {
		t.Fatalf("make the app private: %v", err)
	}
	if got := list(priya); len(got) != 0 {
		t.Errorf("she lost read on the app but still sees %+v", got)
	}
}

func mustTokenVersion(t *testing.T, f *appJobFixture, who string) int {
	t.Helper()
	v, err := f.srv.db.GetTokenVersion(who)
	if err != nil {
		t.Fatalf("token version: %v", err)
	}
	return v
}

// --- Output ---

// The two failure modes are told apart deliberately: a succeeded run with a
// download that 404s is worse than one that says its artifact is gone.
func TestOutputDistinguishesAbsentFromExpired(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	run := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", ""))

	// Nothing written yet.
	rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs/"+run.ID+"/output", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("a run with no output: status %d, want 404 — body %s", rec.Code, rec.Body.String())
	}

	// Recorded, but the store has nothing under the key — an artifact that
	// expired with its row, or one a lifecycle rule collected.
	//
	// The local store standing in here cannot presign, so this leg asserts the
	// refusal it *does* give: its own message, naming the remedy, rather than a
	// 500 that would send an operator hunting for a fault in a working install.
	key := artifact.RunOutputKey(f.app.ID, run.ID, "output")
	if err := f.srv.db.SetJobRunOutputKey(f.app.ID, run.ID, key); err != nil {
		t.Fatalf("record output key: %v", err)
	}
	rec = f.call(t, http.MethodGet, "/api/v1/app/job-runs/"+run.ID+"/output", "", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a store that cannot presign: status %d, want 422 — body %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ARTIFACT_STORE=s3") {
		t.Errorf("the refusal should name the remedy: %s", rec.Body.String())
	}
}

// The 302-and-410 half of the same rule, against a real object store.
//
// It needs one: the presigned GET is the whole mechanism, and the local store
// cannot presign — which is consistent, because an install with a local store
// has no isolated backend and therefore no jobs at all. Skipped without MinIO,
// following the pattern the artifact package already uses.
func TestOutputRedirectsToASignedURLAndReportsExpiry(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set; start MinIO with docker compose --profile s3 up -d minio minio-init")
	}

	f := newAppJobFixture(t, runtime.ModeShared)
	store := liveArtifactStore(t, endpoint)
	f.srv.artifacts = store

	run := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", ""))
	key := artifact.RunOutputKey(f.app.ID, run.ID, "output")

	// A run whose object is genuinely there.
	if err := store.PutBlob(context.Background(), key, []byte("rows,1204\n")); err != nil {
		t.Fatalf("put output: %v", err)
	}
	if err := f.srv.db.SetJobRunOutputKey(f.app.ID, run.ID, key); err != nil {
		t.Fatalf("record output key: %v", err)
	}

	rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs/"+run.ID+"/output", "", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302 — body %s", rec.Code, rec.Body.String())
	}
	// A redirect to the store, not bytes through the platform: Toft is not
	// in the data path for a release or a payload either.
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "X-Amz-Signature") {
		t.Errorf("Location = %q, want a presigned URL", loc)
	}
	body, err := http.Get(loc)
	if err != nil {
		t.Fatalf("following the redirect: %v", err)
	}
	defer body.Body.Close()
	got, _ := io.ReadAll(body.Body)
	if string(got) != "rows,1204\n" {
		t.Errorf("the signed URL served %q, want the run's output", got)
	}

	// And once the object is gone, *expired* rather than a redirect to a 404.
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatalf("delete output: %v", err)
	}
	rec = f.call(t, http.MethodGet, "/api/v1/app/job-runs/"+run.ID+"/output", "", "")
	if rec.Code != http.StatusGone {
		t.Errorf("status %d, want 410 — body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("410 should say *expired* in so many words: %s", rec.Body.String())
	}
}

// liveArtifactStore points the S3 store at a bucket of this test's own.
//
// Credentials reach the store through the ambient AWS environment, exactly as
// they do on ECS via a task role — which is also why the shared-credentials and
// config files are pointed at empty temporary paths: a developer's own ~/.aws
// must not decide what this test talks to.
func liveArtifactStore(t *testing.T, endpoint string) *artifact.S3Store {
	t.Helper()

	access := envOrDefault("TEST_S3_ACCESS_KEY", "toft")
	secret := envOrDefault("TEST_S3_SECRET_KEY", "toft-dev-secret")
	t.Setenv("AWS_ACCESS_KEY_ID", access)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))

	store, err := artifact.NewS3Store(context.Background(), artifact.S3Config{
		// The bucket docker-compose provisions; a prefix of this test's own
		// keeps its objects out of everything else's way.
		Bucket:   envOrDefault("TEST_S3_BUCKET", "toft-releases"),
		Prefix:   "app-job-test-" + strings.ToLower(t.Name()),
		Endpoint: endpoint,
		Region:   "us-east-1",
		// Required by MinIO and by most self-hosted S3-compatible servers,
		// which cannot do virtual-host addressing.
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3 store: %v", err)
	}
	return store
}

func envOrDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// --- The IDOR rule, restated for a subtree that takes a run id ---

// These routes take an ID from the URL, so the rule that anything doing so
// scopes its query by it is back in force. One app's token must not reach
// another app's run through any of them.
func TestAnAppCannotReadAnotherAppsRun(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)

	// A run belonging to the *other* app, made directly so the surface under
	// test is only the reading half.
	foreign := &database.JobRun{
		AppID: f.other.ID, JobName: "whatever", StartedBy: database.JobStartedByApp,
		ExecutionMode: string(runtime.ModeIsolated),
	}
	if err := f.srv.db.EnqueueJobRun(foreign, 0); err != nil {
		t.Fatalf("seed the other app's run: %v", err)
	}

	for _, path := range []string{
		"/api/v1/app/job-runs/" + foreign.ID,
		"/api/v1/app/job-runs/" + foreign.ID + "/logs",
		"/api/v1/app/job-runs/" + foreign.ID + "/output",
	} {
		if rec := f.call(t, http.MethodGet, path, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
	}
	rec := f.call(t, http.MethodPost, "/api/v1/app/job-runs/"+foreign.ID+"/cancel", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("cancelling another app's run: status %d, want 404", rec.Code)
	}
	// And it is still there.
	if got, _ := f.srv.db.GetAppJobRun(f.other.ID, foreign.ID, ""); got == nil || got.Status != database.JobRunQueued {
		t.Error("the other app's run was disturbed")
	}
}

// --- Both modes ---

// The capability is identical in both execution modes; only where the *app's*
// service runs differs, and that was never app-visible. A run is an isolated
// workload either way.
func TestStartingAJobBehavesTheSameInBothModes(t *testing.T) {
	for _, mode := range []runtime.Mode{runtime.ModeShared, runtime.ModeIsolated} {
		t.Run(string(mode), func(t *testing.T) {
			f := newAppJobFixture(t, mode)
			blob := f.actFor(t, "priya", f.app.ID)

			rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", blob, "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status %d — body %s", rec.Code, rec.Body.String())
			}
			got := decodeRun(t, rec)
			if got.StartedBy != database.JobStartedByViewer || got.Status != database.JobRunQueued {
				t.Errorf("run = %+v, want a queued run attributed to the viewer", got)
			}

			run, err := f.srv.db.GetAppJobRun(f.app.ID, got.ID, "")
			if err != nil || run == nil {
				t.Fatalf("reading the run back: %v", err)
			}
			if runtime.Mode(run.ExecutionMode) != runtime.ModeIsolated {
				t.Errorf("run execution mode = %q, want isolated in both modes", run.ExecutionMode)
			}

			// And the same answer from mine().
			list := f.call(t, http.MethodGet, "/api/v1/app/job-runs?scope=viewer", blob, "")
			if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), got.ID) {
				t.Errorf("mine() = %d %s, want the run just started", list.Code, list.Body.String())
			}
		})
	}
}

// A route that takes no app id cannot have an IDOR, which is why no handler in
// this subtree calls auth.Authorize. That is a rule the router has to keep, not
// a fact about today's handlers.
func TestNoAppFacingRouteTakesAnAppID(t *testing.T) {
	router := routerForWalk(t)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/app/") {
			return nil
		}
		if strings.Contains(route, "{appID}") || strings.Contains(route, "{appId}") {
			t.Errorf("%s %s takes an app id — the token is the app", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
}

// appRun's own comment promised the viewer scope, and only cancel applied it:
// get, logs and output handed one person another's run — status, result, and
// a redirect to the output. The scope is now in the query, so a run somebody
// else started is 404 on every reader, and the starter still reads it.
func TestARelayedViewerReadsOnlyTheirOwnRuns(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	priya := f.actFor(t, "priya", f.app.ID)
	sam := f.actFor(t, "sam", f.app.ID)

	hers := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", priya, `{"params":{"n":1}}`))

	for _, path := range []string{
		"/api/v1/app/job-runs/" + hers.ID,
		"/api/v1/app/job-runs/" + hers.ID + "/logs",
		"/api/v1/app/job-runs/" + hers.ID + "/output",
	} {
		// /output is 404 `no_output` for a run that wrote nothing, so the
		// refusal is told apart by its code, not its status.
		if rec := f.call(t, http.MethodGet, path, sam, ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("GET %s as another viewer: status %d, want 404 not_found — %s", path, rec.Code, rec.Body.String())
		}
		if rec := f.call(t, http.MethodGet, path, priya, ""); strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("GET %s as the starter: not_found — the scope refused its owner", path)
		}
		// The app token alone, with no viewer relayed, reads the app's runs.
		if rec := f.call(t, http.MethodGet, path, "", ""); strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("GET %s as the app: not_found — an unrelayed read is the app's own", path)
		}
	}
}

// `?job=` on the app-scoped list is a SQL predicate, not a filter over the
// page: twenty newer runs of another job must not crowd out the one asked for.
func TestTheJobFilterOnTheAppScopedListRunsBeforeTheLimit(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	declareJob(t, f.srv.db, f.app.ID, database.JobDefinition{
		Name: "other", Command: "python -m other", TimeoutSec: 60,
	})

	want := decodeRun(t, f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", `{"params":{"n":1}}`))
	for i := 0; i < 2; i++ {
		// Distinct params, or the queue deduplicates them into one run.
		if rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/other/runs", "", fmt.Sprintf(`{"params":{"n":%d}}`, 10+i)); rec.Code != http.StatusAccepted {
			t.Fatalf("enqueue other: %d %s", rec.Code, rec.Body.String())
		}
	}

	rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs?job=reconcile&limit=2", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d — %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Runs []appJobRunResponse `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Runs) != 1 || out.Runs[0].ID != want.ID {
		t.Fatalf("job=reconcile&limit=2 = %+v, want exactly %s — the filter ran after the LIMIT", out.Runs, want.ID)
	}
}

// declareJob adds one job to an app's declared set the way a deploy would: the
// app's toft.toml now holds its existing jobs and this one.
func declareJob(t *testing.T, db *database.DB, appID string, d database.JobDefinition) *database.JobDefinition {
	t.Helper()
	defs, err := db.ListJobDefinitions(appID)
	if err != nil {
		t.Fatal(err)
	}
	kept := defs[:0]
	for _, existing := range defs {
		if existing.Name != d.Name {
			kept = append(kept, existing)
		}
	}
	if err := db.ReconcileManifestJobs(appID, append(kept, d)); err != nil {
		t.Fatalf("declaring %s: %v", d.Name, err)
	}
	got, err := db.GetJobDefinitionByName(appID, d.Name)
	if err != nil || got == nil {
		t.Fatalf("reading %s back: %v", d.Name, err)
	}
	return got
}

// An anonymous visitor to a public app carries an act-for header that names
// nobody, so a start in their name is refused viewer_anonymous — with a hint
// naming the call that does it as the app, which is what app code can act on
// (drills D5, 2026-10-28: the refusal had none, and a public page answered 500).
func TestAnAnonymousViewersStartIsRefusedWithTheWayRound(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	blob := f.srv.jwt.MintAnonymousActFor(f.app.ID, time.Now().Add(time.Hour))

	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", blob, "")
	var body struct {
		Code, Hint string
		Retryable  *bool
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body.Code != "viewer_anonymous" {
		t.Fatalf("anonymous start = %d %s, want 403 viewer_anonymous", rec.Code, rec.Body.String())
	}
	if !strings.Contains(body.Hint, "as the app") || !strings.Contains(body.Hint, "toft.jobs.start") {
		t.Errorf("viewer_anonymous hint = %q, want the start-as-the-app way round", body.Hint)
	}
	if body.Retryable == nil || *body.Retryable {
		t.Errorf("viewer_anonymous retryable = %v, want false", body.Retryable)
	}

	// Listing their runs is not a start, and its hint says list (BB-022,
	// second 2026-11-03 bash).
	rec = f.call(t, http.MethodGet, "/api/v1/app/job-runs?scope=viewer", blob, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body.Code != "viewer_anonymous" ||
		!strings.Contains(body.Hint, "toft.jobs.list") || strings.Contains(body.Hint, "start it") {
		t.Errorf("anonymous list = %d, hint %q", rec.Code, body.Hint)
	}

	// The app's runs through that viewer are refused too: a viewer's list is a
	// viewer call (drills 2026-11-03). Without the header it is the app's.
	rec = f.call(t, http.MethodGet, "/api/v1/app/job-runs?job=reconcile", blob, "")
	body.Code = ""
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body.Code != "viewer_anonymous" {
		t.Errorf("anonymous viewer's list of the app's runs = %d %s, want 403 viewer_anonymous",
			rec.Code, rec.Body.String())
	}
	if rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs?job=reconcile", "", ""); rec.Code != http.StatusOK {
		t.Errorf("the app's own list = %d, want 200", rec.Code)
	}
}

// no_such_job's hint on an app that is not static is the [jobs] advice alone:
// the static-app exception is chosen by the app's framework, never hedged in
// every app's hint (drills D14, 2026-10-28; TestNoSuchJobHintFollowsTheFramework
// is the static half).
func TestNoSuchJobsHintIsTheDeclarationForAServerApp(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)

	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/never-declared/runs", "", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_such_job") {
		t.Fatalf("undeclared job = %d %s, want 404 no_such_job", rec.Code, rec.Body.String())
	}
	var body struct{ Hint string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Hint, "[jobs.never-declared]") || strings.Contains(body.Hint, "static") {
		t.Errorf("no_such_job hint = %q, want the declaration and no static hedge", body.Hint)
	}
}

// startWithKey drives one app start carrying an explicit Idempotency-Key.
func (f *appJobFixture) startWithKey(t *testing.T, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/app/jobs/reconcile/runs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// A replayed key says so, carries its queue position while queued, and a key
// reused for different work is refused rather than answered with the old run
// (BB-010, BB-023).
func TestAnIdempotencyKeyNamesOnePieceOfWork(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)

	first := f.startWithKey(t, "k-1", `{"params":{"month":"2026-03"}}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first start: %d %s", first.Code, first.Body.String())
	}
	if !strings.Contains(first.Body.String(), `"deduplicated":false`) {
		t.Errorf("a fresh start must say deduplicated:false, got %s", first.Body.String())
	}

	replay := f.startWithKey(t, "k-1", `{"params": {"month": "2026-03"}}`)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	got := decodeRun(t, replay)
	if !got.Deduplicated || got.ID != decodeRun(t, first).ID {
		t.Fatalf("a replay must return the same run marked deduplicated, got %+v", got)
	}
	if got.Status == database.JobRunQueued && got.QueuePosition < 1 {
		t.Errorf("a deduplicated start still queued must carry its queue position, got %+v", got)
	}

	reused := f.startWithKey(t, "k-1", `{"params":{"month":"2026-04"}}`)
	if reused.Code != http.StatusConflict {
		t.Fatalf("the same key with different params must be 409, got %d %s", reused.Code, reused.Body.String())
	}
	if body := decodeData(t, reused); body["code"] != "idempotency_key_reused" {
		t.Errorf("want code idempotency_key_reused, got %v", body)
	}
}

// An explicit key the app's code reuses for a second viewer names the first
// viewer's run, and there is no cross-person coalescing (ADR §26): the second
// start is refused, and adds no run (security review 2026-11-14, SI-7).
func TestAnIdempotencyKeyIsNotSharedBetweenViewers(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	start := func(who string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/app/jobs/reconcile/runs", strings.NewReader(`{"params":{"month":"2026-11"}}`))
		req.Header.Set("Authorization", "Bearer "+f.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "export-2026-11")
		req.Header.Set(auth.ActForHeader, f.actFor(t, who, f.app.ID))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}
	if rec := start("priya"); rec.Code != http.StatusAccepted {
		t.Fatalf("priya's start: %d %s", rec.Code, rec.Body.String())
	}
	if rec := start("priya"); rec.Code != http.StatusOK {
		t.Fatalf("priya's replay: %d %s, want 200 deduplicated", rec.Code, rec.Body.String())
	}
	var before int
	f.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM job_runs WHERE app_id = $1`, f.app.ID).Scan(&before)
	rec := start("sam")
	if rec.Code != http.StatusConflict {
		t.Fatalf("sam's start under priya's key: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if body := decodeData(t, rec); body["code"] != "idempotency_key_reused" {
		t.Errorf("want code idempotency_key_reused, got %v", body)
	}
	var after int
	f.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM job_runs WHERE app_id = $1`, f.app.ID).Scan(&after)
	if after != before {
		t.Errorf("the refused start added %d run(s)", after-before)
	}
}

// A claimed run reads as queued, so it carries a position too (BB-023).
func TestAClaimedRunCarriesAQueuePosition(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	run := decodeRun(t, f.startWithKey(t, "k-claim", `{}`))
	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status = $1 WHERE id = $2`, database.JobRunClaimed, run.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := decodeRun(t, f.startWithKey(t, "k-claim", `{}`))
	if got.Status != database.JobRunQueued || got.QueuePosition != 1 {
		t.Fatalf("a claimed run must read as queued at position 1, got status %q position %d", got.Status, got.QueuePosition)
	}
}

// Params over the cap are refused naming the cap, not as a malformed body
// (BB-022).
func TestParamsOverTheCapAreRefusedByName(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	big := `{"params":{"blob":"` + strings.Repeat("x", maxStartBodyBytes) + `"}}`
	rec := f.call(t, http.MethodPost, "/api/v1/app/jobs/reconcile/runs", "", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d %s", rec.Code, rec.Body.String())
	}
	body := decodeData(t, rec)
	if body["code"] != "params_too_large" || !strings.Contains(body["error"].(string), "64 KB") {
		t.Fatalf("want params_too_large naming 64 KB, got %v", body)
	}
}

// A status filter no run can have is refused, not answered with an empty list.
func TestAnUnknownRunStatusFilterIsRefused(t *testing.T) {
	f := newAppJobFixture(t, runtime.ModeShared)
	if rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs?status=bogus", "", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.call(t, http.MethodGet, "/api/v1/app/job-runs?status=queued", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("a real status: %d %s", rec.Code, rec.Body.String())
	}
}

// BB-030 (fourth 2026-11-03 black-box list): rerun_of, scheduled_for and
// failure_reason are present as null when empty, as the SDKs' Run objects
// document them, and carry their value when set.
func TestEmptyRunFieldsArePresentAsNull(t *testing.T) {
	raw, err := json.Marshal(appJobRunResponse{ID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"rerun_of", "scheduled_for", "failure_reason"} {
		if v, ok := m[k]; !ok || string(v) != "null" {
			t.Errorf("%s = %s (present %v), want null", k, v, ok)
		}
	}
	raw, _ = json.Marshal(appJobRunResponse{RerunOf: "r0", FailureReason: "exit 3"})
	var back appJobRunResponse
	if err := json.Unmarshal(raw, &back); err != nil || back.RerunOf != "r0" || back.FailureReason != "exit 3" {
		t.Errorf("round trip = %+v, %v", back, err)
	}
}
