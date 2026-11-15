package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/builder"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/gitconn"
	"toft/internal/ports"
	"toft/internal/runtime"
)

// The restart button, after Restart was deleted from the seam.
//
// These drive the real router, because the three things worth pinning are all
// properties of the handler rather than of the builder underneath it: the
// status is written from what was *observed*, the wait is bounded and says so
// with a 202, and two people pressing the button collapse into one start.

type restartEnv struct {
	srv    *Server
	router http.Handler
	app    *database.App
	mock   *runtime.MockRuntime
}

// What the environment lets a start do.
//
// The three are kept apart because two of them used to be one: "no release on
// disk" was how these tests made a start fail, and a start that finds no
// release is no longer a failure — it is a refusal, with nothing started and
// nothing crashed. A test about what a *crash* writes has to crash.
type startable int

const (
	// startWorks — a release on disk and a port to bind.
	startWorks startable = iota
	// startFails — a release on disk and no port left, so the backend refuses.
	// A genuine failure of a start that was going to happen.
	startFails
	// noRelease — nothing on disk. The start is refused before anything runs.
	noRelease
)

// newRestartEnv stands up a server whose builder can actually start something:
// a real release directory under a temp APPS_DIR, and a mock backend with a
// real allocator so the workload listens and `waitReady` can succeed.
func newRestartEnv(t *testing.T, mode startable) *restartEnv {
	t.Helper()

	srv := newTestServer(t)
	appsDir := t.TempDir()

	// One port in range, and something already holds it: the allocator has
	// nothing to hand out, so Deploy fails. Failing at the backend rather than
	// before it is the point — this is what a start that broke looks like.
	alloc := ports.New(45000, 45400, func() ([]int, error) { return nil, nil })
	if mode == startFails {
		alloc = ports.New(45000, 45000, func() ([]int, error) { return []int{45000}, nil })
	}
	mock := runtime.NewMockRuntime(alloc)
	rts := runtime.NewRuntimes(mock, nil)
	srv.rts = rts

	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatal(err)
	}
	srv.builder = builder.New(srv.db, rts, enc, gitconn.NewResolver(srv.db, enc), nil, builder.Options{AppsDir: appsDir})
	// The rule is "the handler stops waiting before the start does", and a
	// shortened hold proves that sentence as well as 25 real seconds would.
	srv.wakeHold = 300 * time.Millisecond

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "restart-demo", Name: "Restart Demo", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated,
		RepoURL: "https://github.com/example/demo",
		// A live release, because a start is of *something*: without it the
		// handler is exercising the no-release refusal rather than the restart.
		CurrentRelease: "restart-demo-r1",
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}

	if mode != noRelease {
		// A release on disk is what makes a shared-mode start possible at all;
		// without it Start refuses with errNoRelease, which is a 409 rather
		// than anything the backend was ever asked about.
		release := filepath.Join(appsDir, app.ID, "releases", "r1")
		if err := os.MkdirAll(release, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(release, "index.html"),
			[]byte("<!doctype html><title>demo</title>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(release, builder.CurrentLink(appsDir, app.ID)); err != nil {
			t.Fatal(err)
		}
	}

	return &restartEnv{srv: srv, router: srv.Router(http.NotFoundHandler()), app: app, mock: mock}
}

func (e *restartEnv) restart(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/apps/"+e.app.ID+"/restart", nil)
	r.AddCookie(sessionCookieFor(t, e.srv, "owner"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)
	return rec
}

func (e *restartEnv) status(t *testing.T) string {
	t.Helper()
	fresh, err := e.srv.db.GetApp(e.app.ID)
	if err != nil || fresh == nil {
		t.Fatalf("re-reading the app: %v", err)
	}
	return fresh.Status
}

// `running` means a readiness probe answered, not that a call returned nil.
// This is the half that changes shared mode too, where the process manager
// returns long before the app is listening.
func TestRestartWritesRunningOnlyWhenReadinessAnswered(t *testing.T) {
	e := newRestartEnv(t, startWorks)

	if rec := e.restart(t); rec.Code != http.StatusOK {
		t.Fatalf("restart = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := e.status(t); got != database.AppStatusRunning {
		t.Errorf("status after a successful restart = %q, want running", got)
	}
}

// A release the platform cannot find is a refusal, and it is not a crash.
//
// It used to be a 500 whose body was "release directory missing — redeploy the
// app", and it wrote `crashed` — blaming the app for the state of the disk.
// Nothing was started, so nothing crashed, and the caller is told the one thing
// that fixes it.
func TestRestartRefusesWhenTheReleaseIsGone(t *testing.T) {
	e := newRestartEnv(t, noRelease)
	was := e.status(t)

	rec := e.restart(t)
	if rec.Code != http.StatusConflict {
		t.Fatalf("restart = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var out struct{ Error, Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the refusal: %v (%s)", err, rec.Body.String())
	}
	if out.Code != "no_release" {
		t.Errorf("code = %q, want no_release", out.Code)
	}
	if got := e.status(t); got != was {
		t.Errorf("status after a refused restart = %q, want it unchanged at %q", got, was)
	}
}

// A start that never comes up leaves a status the dashboard and the gateway
// interstitial tell the truth about. The old handler returned 500 and left the
// row saying `running` with nothing running.
func TestRestartWritesCrashedWhenTheStartFails(t *testing.T) {
	e := newRestartEnv(t, startFails) // the backend has no port to give it

	rec := e.restart(t)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("restart = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := e.status(t); got != database.AppStatusCrashed {
		t.Errorf("status after a failed restart = %q, want crashed — a row saying "+
			"running with nothing running is the thing this writes against", got)
	}
}

// Two people pressing Restart at the same moment are one start.
//
// The wake group is what collapses them, and it is the same group the proxy's
// wake uses — so a restart and a wake of one app cannot race each other either.
func TestConcurrentRestartsCollapseToOneStart(t *testing.T) {
	e := newRestartEnv(t, startWorks)

	// The mock counts starts as restarts on the process it already knows.
	before := e.mockRestarts(t)

	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.restart(t).Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK && c != http.StatusAccepted {
			t.Errorf("caller %d got %d, want 200 or 202", i, c)
		}
	}

	// One start, not four. Four would be four bounces of somebody's serving app
	// from one impatient double-click.
	if got := e.mockRestarts(t) - before; got > 1 {
		t.Errorf("four concurrent restarts produced %d starts, want 1", got)
	}
}

func (e *restartEnv) mockRestarts(t *testing.T) int {
	t.Helper()
	all, err := e.mock.List()
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, m := range all {
		total += m.Restarts
	}
	return total
}

// Past the hold the answer is 202 with the start still running, because an
// isolated start is a pod schedule and an image pull and `readinessTimeout` is
// 90 seconds — a handler that blocks that long is not a design.
//
// Driven by a start that outlives a deliberately shortened hold rather than by
// waiting 25 real seconds: the rule is "the handler stops waiting before the
// start does", and the shorter number proves the same sentence.
func TestRestartAnswers202WhenTheStartOutlivesTheHold(t *testing.T) {
	e := newRestartEnv(t, startWorks)

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		// Occupy the wake group with a start that does not finish, so the
		// handler's own wait is what expires.
		_ = e.srv.wakes.Do(t.Context(), e.app.ID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	// The handler joins the in-flight call and gives up on its own deadline.
	done := make(chan int, 1)
	go func() { done <- e.restart(t).Code }()

	select {
	case code := <-done:
		if code != http.StatusAccepted {
			t.Fatalf("restart while a start is in flight = %d, want 202", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never gave up — the hold is not bounded")
	}
}

// Nothing about the 202 may imply the app is back.
func TestThe202SaysTheStartIsStillRunning(t *testing.T) {
	e := newRestartEnv(t, startWorks)

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = e.srv.wakes.Do(t.Context(), e.app.ID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	rec := e.restart(t)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "restarting" {
		t.Errorf("status = %q, want %q", body["status"], "restarting")
	}
	// And the status column is untouched on this path: the closure owns it and
	// has not observed anything yet.
	if got := e.status(t); got != database.AppStatusRunning {
		t.Errorf("the 202 path wrote %q; it must write nothing about the outcome", got)
	}
}

// --- What has been saved but is not running ---

// The count is of variables that actually *changed*, not of rows submitted.
//
// The settings form resubmits every row on every save, so a counter bumped per
// write would report five un-applied changes for a save that changed nothing —
// and a notice that cries wolf is one people learn to scroll past.
func TestUnappliedEnvCountsChangesNotWrites(t *testing.T) {
	e := newRestartEnv(t, startWorks)

	// A start, so the app is level: nothing pending.
	if rec := e.restart(t); rec.Code != http.StatusOK {
		t.Fatalf("initial start: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.unapplied(t); got != 0 {
		t.Fatalf("un-applied after a start = %d, want 0", got)
	}

	e.saveEnv(t, map[string]string{"A": "1", "B": "2"}, nil)
	if got := e.unapplied(t); got != 2 {
		t.Errorf("after adding two variables = %d, want 2", got)
	}

	// The same values again. Nothing changed, so nothing is pending beyond what
	// already was.
	e.saveEnv(t, map[string]string{"A": "1", "B": "2"}, nil)
	if got := e.unapplied(t); got != 2 {
		t.Errorf("after resubmitting the same values = %d, want the same 2", got)
	}

	// One edit and one removal are two more changes.
	e.saveEnv(t, map[string]string{"A": "changed"}, []string{"B"})
	if got := e.unapplied(t); got != 4 {
		t.Errorf("after one edit and one removal = %d, want 4", got)
	}

	// And a start clears it, because the process now carries them.
	if rec := e.restart(t); rec.Code != http.StatusOK {
		t.Fatalf("restart: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.unapplied(t); got != 0 {
		t.Errorf("after a restart = %d, want 0 — the running process has them now", got)
	}
}

// A start that failed readiness is not carrying anything, so the changes are
// still un-applied. Clearing them on a nil error would tell somebody their
// variables were live on a process that never came up.
func TestAFailedStartDoesNotMarkTheEnvironmentApplied(t *testing.T) {
	e := newRestartEnv(t, startFails) // the backend has no port to give it

	e.saveEnv(t, map[string]string{"A": "1"}, nil)
	if got := e.unapplied(t); got != 1 {
		t.Fatalf("after a save = %d, want 1", got)
	}
	if rec := e.restart(t); rec.Code != http.StatusInternalServerError {
		t.Fatalf("restart = %d, want 500", rec.Code)
	}
	if got := e.unapplied(t); got != 1 {
		t.Errorf("after a failed start = %d, want the change still pending", got)
	}
}

func (e *restartEnv) saveEnv(t *testing.T, vars map[string]string, unset []string) {
	t.Helper()
	set := map[string]any{}
	for k, v := range vars {
		set[k] = map[string]any{"value": v, "sensitive": false}
	}
	body, err := json.Marshal(map[string]any{"set": set, "unset": unset})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+e.app.ID+"/env", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(sessionCookieFor(t, e.srv, "owner"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("saving env vars = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// unapplied reads the number the app's own page renders, through the route
// that renders it — a direct database read would not prove the field reaches
// the client, which is the half that was missing before.
func (e *restartEnv) unapplied(t *testing.T) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+e.app.ID, nil)
	r.AddCookie(sessionCookieFor(t, e.srv, "owner"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("reading the app = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Unapplied int `json:"unapplied_env_changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Unapplied
}

// The answer says which of the two the route did — a start of a stopped app,
// a bounce of a running one — the same judgement its audit row records, so
// `toft start` is not left printing "restarted" (docs/bugs 2026-10-28 D20).
// `status` stays "restarted" for both, for clients that predate `action`.
func TestRestartAnswerNamesWhetherItStartedOrRestarted(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	for _, tc := range []struct{ from, want string }{
		{database.AppStatusStopped, "start"},
		{database.AppStatusRunning, "restart"},
	} {
		if err := e.srv.db.UpdateAppStatus(e.app.ID, tc.from); err != nil {
			t.Fatal(err)
		}
		rec := e.restart(t)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusOK || body["status"] != "restarted" || body["action"] != tc.want {
			t.Errorf("from %s: %d %v, want status restarted, action %s", tc.from, rec.Code, body, tc.want)
		}
		if body["note"] != "" {
			t.Errorf("a static app's restart carried a note: %q", body["note"])
		}
	}
}

// A static app has no process of its own, so waking one from sleep started
// nothing: the answer is a restart, beside the note that nothing changed, and
// `toft restart` prints "unchanged" rather than "started" over that note
// (drills 2026-10-29 #3). A stopped static app was not being served, so
// bringing it back is a start, and says nothing about having changed nothing.
func TestAStaticAppAsleepRestartsAndAStoppedOneStarts(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	if err := e.srv.db.SetAppDetection(e.app.ID, "static", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		from, want string
		note       bool
	}{
		{database.AppStatusSleeping, "restart", true},
		{database.AppStatusStopped, "start", false},
		{database.AppStatusRunning, "restart", true},
	} {
		if err := e.srv.db.UpdateAppStatus(e.app.ID, tc.from); err != nil {
			t.Fatal(err)
		}
		rec := e.restart(t)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusOK || body["action"] != tc.want {
			t.Errorf("from %s: %d %v, want action %s", tc.from, rec.Code, body, tc.want)
		}
		if got := body["note"] != ""; got != tc.note {
			t.Errorf("from %s: note %q, want a note: %v", tc.from, body["note"], tc.note)
		}
	}
}

// A document's restart serves the pages its last render made, so the answer
// says the page changes at the next deploy rather than a bare "restarted" to
// somebody who changed a variable for it (docs/bugs 2026-10-28 D11).
func TestRestartOfADocumentSaysThePageChangesAtTheNextDeploy(t *testing.T) {
	for _, fw := range []string{"notebook", "quarto", "rmarkdown"} {
		if note := restartNote(fw); !strings.Contains(note, "next deploy") {
			t.Errorf("%s: note = %q, want it to name the next deploy", fw, note)
		}
	}
	// A static app's restart changed nothing, and says so in the words the
	// CLI used when the install said nothing.
	if note := restartNote("static"); !strings.Contains(note, "the restart changed nothing") {
		t.Errorf("static: note = %q, want it to say the restart changed nothing", note)
	}
	for _, fw := range []string{"", "flask", "shiny"} {
		if note := restartNote(fw); note != "" {
			t.Errorf("%s: note = %q, want none", fw, note)
		}
	}
}

// A person's restart forgives the app — the supervisor's fresh five attempts
// and the health sweep's tolerance — after restartApp was lifted out of the
// handler (ADR §145). A scheduled one does not, which is the
// other half of the same rule, tested beside the scheduler.
func TestAPersonsRestartStillForgives(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	var forgiven []string
	e.srv.forgiven = func(id string) { forgiven = append(forgiven, id) }

	if rec := e.restart(t); rec.Code != http.StatusOK {
		t.Fatalf("restart = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(forgiven) != 1 || forgiven[0] != e.app.ID {
		t.Errorf("forgiven = %v, want the app once", forgiven)
	}
}

// stopOnDeploy is the mock backend with one hook: something that happens to
// the app row while its workload is being placed.
type stopOnDeploy struct {
	*runtime.MockRuntime
	onDeploy func()
}

func (s stopOnDeploy) Deploy(spec runtime.WorkloadSpec) (runtime.Endpoint, error) {
	if s.onDeploy != nil {
		s.onDeploy()
	}
	return s.MockRuntime.Deploy(spec)
}

// A restart that waited behind a start while a deploy flipped finds the release
// moved: nothing failed, the flip wrote the status, and the restart writes no
// `crashed` over it (security review 2026-11-14, DB-4).
func TestRestartSupersededByADeployWritesNothing(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	stale := *e.app

	moved, err := e.srv.db.GetApp(e.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved.CurrentRelease = "restart-demo-r2"
	moved.RunningMode = "shared"
	if err := e.srv.db.SetAppRelease(moved); err != nil {
		t.Fatal(err)
	}

	if _, err := e.srv.restartApp(context.Background(), &stale, restartOpts{Asked: time.Now(), ActorType: "system"}); err != nil {
		t.Fatalf("a superseded restart = %v, want nil — nothing failed", err)
	}
	if got := e.status(t); got != database.AppStatusRunning {
		t.Errorf("status = %q, want the flip's running to stand", got)
	}
}

// A Stop that lands while the restart is placing its workload stands, and the
// workload the restart placed is stopped again rather than left live under a
// `stopped` row (DB-4 step 5, DB-U1).
func TestRestartLeavesAStopMadeDuringItsStart(t *testing.T) {
	e := newRestartEnv(t, startWorks)
	hooked := stopOnDeploy{MockRuntime: e.mock, onDeploy: func() {
		e.srv.db.UpdateAppStatus(e.app.ID, database.AppStatusStopped)
	}}
	rts := runtime.NewRuntimes(hooked, nil)
	e.srv.rts = rts
	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatal(err)
	}
	e.srv.builder = builder.New(e.srv.db, rts, enc, gitconn.NewResolver(e.srv.db, enc), nil,
		builder.Options{AppsDir: e.srv.builder.AppsDir()})

	if _, err := e.srv.restartApp(context.Background(), e.app, restartOpts{Asked: time.Now(), ActorType: "system"}); err != nil {
		t.Fatalf("restart = %v", err)
	}
	if got := e.status(t); got != database.AppStatusStopped {
		t.Errorf("status = %q, want the Stop made during the start to stand", got)
	}
	m, err := e.mock.Metrics(e.app.ProcessName())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.WorkloadIsUp(m.Status) {
		t.Errorf("workload is %q under a stopped row — the start's workload was left live", m.Status)
	}
}
