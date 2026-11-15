package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/gitconn"
	"toft/internal/idle"
	"toft/internal/ports"
	"toft/internal/runtime"
)

// newWakeServer builds a server whose builder and proxy share one mock backend,
// which is what makes a wake observable: the reaper stops a workload the mock
// still knows about, and the wake has to restart that same one.
//
// The apps directory is returned because a wake is a **start** since ADR §4, and
// a start assembles the whole description from the app row and the release on
// disk. It used to be able to skip that whenever the backend still knew the
// workload — which is precisely the branch that brought back the environment PM2
// had saved at process creation — so the fixture now has to leave behind what a
// deploy would have.
func newWakeServer(t *testing.T) (*Server, *runtime.MockRuntime, string) {
	t.Helper()

	db := dbtest.New(t)
	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	// A real allocator, because these tests need the mock to actually listen:
	// a wake is only proven by a request the woken workload answers.
	mock := runtime.NewMockRuntime(ports.New(39100, 39199, db.UsedPorts))
	rts := runtime.NewRuntimes(mock, nil)
	appsDir := t.TempDir()
	bld := builder.New(db, rts, enc, gitconn.NewResolver(db, enc), nil, builder.Options{AppsDir: appsDir})

	cfg := &config.Config{BaseURL: "http://localhost:8080"}
	srv := NewServer(cfg, db, auth.NewJWTService("test-secret-key"), rts, bld, enc)
	return srv, mock, appsDir
}

// sleepingApp deploys a workload on the mock backend, then puts the app row in
// the state the reaper leaves behind: process stopped, address still held,
// status `sleeping`.
func sleepingApp(t *testing.T, srv *Server, mock *runtime.MockRuntime, appsDir, slug string) *database.App {
	t.Helper()

	if err := srv.db.UpsertUser(&database.User{
		ID: "u1", Login: "u1", SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	release := slug + "-rel"
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: "u1",
		Framework: "flask", Visibility: database.VisibilityPublic,
		Status: database.AppStatusRunning, CurrentRelease: release,
		RunningMode: string(runtime.ModeShared),
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// What a deploy leaves on disk. The wake reads it — release directory,
	// framework, entrypoint — because it builds the workload's description
	// rather than asking the backend to bounce a name it remembers.
	releaseDir := filepath.Join(appsDir, app.ID, "releases", "r1")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "app.py"), []byte("# flask app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(releaseDir, builder.CurrentLink(appsDir, app.ID)); err != nil {
		t.Fatal(err)
	}

	ep, err := mock.Deploy(runtime.WorkloadSpec{AppID: app.ID, Slug: slug, Name: release})
	if err != nil {
		t.Fatalf("deploy mock workload: %v", err)
	}
	if err := srv.db.SetAppEndpoint(app.ID, ep.Host, ep.Port); err != nil {
		t.Fatalf("record endpoint: %v", err)
	}
	if err := mock.Stop(release); err != nil {
		t.Fatalf("stop mock workload: %v", err)
	}
	if err := srv.db.UpdateAppStatus(app.ID, database.AppStatusSleeping); err != nil {
		t.Fatalf("mark sleeping: %v", err)
	}

	fresh, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatalf("re-read app: %v", err)
	}
	return fresh
}

// proxyExisting serves one request through the real proxy handler for an app
// row that already exists. Unlike proxyTo it creates nothing, so a test can set
// the app up first and then look at what the request did to it. It never calls
// t.Fatal, so it is safe from a goroutine.
func proxyExisting(t *testing.T, srv *Server, app *database.App, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", app.Slug)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	srv.handleAppProxy(rec, req)
	return rec
}

// The point of the whole feature: a request for a sleeping app serves the app,
// not an apology.
func TestProxy_WakesASleepingApp(t *testing.T) {
	srv, mock, appsDir := newWakeServer(t)
	app := sleepingApp(t, srv, mock, appsDir, "sleepy")

	rec := proxyExisting(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/sleepy/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Mock runtime") {
		t.Fatalf("the woken app did not serve the request: %s", rec.Body.String())
	}

	after, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatalf("re-read app: %v", err)
	}
	if after.Status != database.AppStatusRunning {
		t.Fatalf("status after wake = %q, want %q", after.Status, database.AppStatusRunning)
	}
	// A wake restarts; it never rebuilds and never re-addresses an app whose
	// workload the backend still holds.
	if after.Port != app.Port {
		t.Fatalf("port moved on wake: %d → %d", app.Port, after.Port)
	}
	if m, err := mock.Metrics(app.CurrentRelease); err != nil || m.Status != "online" {
		t.Fatalf("workload after wake = %+v, err=%v; want online", m, err)
	}
}

// A stopped app is a person's decision and stays down — the same request that
// wakes a sleeping app must not resurrect one someone switched off.
func TestProxy_DoesNotWakeAStoppedApp(t *testing.T) {
	srv, mock, appsDir := newWakeServer(t)
	app := sleepingApp(t, srv, mock, appsDir, "offline")
	if err := srv.db.UpdateAppStatus(app.ID, database.AppStatusStopped); err != nil {
		t.Fatalf("mark stopped: %v", err)
	}
	app.Status = database.AppStatusStopped

	rec := proxyExisting(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/offline/", nil))

	if !strings.Contains(rec.Body.String(), "stopped by its owner") {
		t.Fatalf("a stopped app did not get its own interstitial: %s", rec.Body.String())
	}
	if m, _ := mock.Metrics(app.CurrentRelease); m != nil && m.Status == "online" {
		t.Fatal("a stopped app was restarted by a visitor")
	}
	after, _ := srv.db.GetApp(app.ID)
	if after.Status != database.AppStatusStopped {
		t.Fatalf("status = %q, want it left stopped", after.Status)
	}
}

// The check that actually matters, and the one a unit test of the tracker
// alone cannot make: the proxy must hold the app awake for the *whole life of
// the response*, not just its arrival. The proxy streams (FlushInterval: -1)
// so that SSE and long downloads work, and a reaper that looked only at arrival
// times would stop the process underneath one.
func TestProxy_StreamingResponseHoldsTheAppAwake(t *testing.T) {
	srv, _, _ := newWakeServer(t)
	tracker := idle.New()
	WithIdle(tracker)(srv)

	streaming := make(chan struct{})
	release := make(chan struct{})
	be := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(streaming)
		<-release
	})

	app := &database.App{
		Slug: "stream", Name: "stream", OwnerID: "u1",
		Framework: "flask", Port: database.Port(be.port),
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.UpsertUser(&database.User{
		ID: "u1", Login: "u1", SystemRole: database.SystemRolePublisher,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		proxyExisting(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/stream/", nil))
	}()

	<-streaming
	// A whole day later, with the response still open, the app is not idle.
	if tracker.IdleSince(app.ID, time.Now().Add(24*time.Hour), time.Minute) {
		t.Error("an app streaming a response was reported idle — the reaper would kill it mid-response")
	}

	close(release)
	<-done

	// And the count unwinds, so the app can eventually sleep.
	if !tracker.IdleSince(app.ID, time.Now().Add(24*time.Hour), time.Minute) {
		t.Error("in-flight count did not return to zero after the response finished")
	}
}

// The burst a link produces — page plus every asset — must restart the app
// once, not once per request.
func TestProxy_ConcurrentRequestsWakeOnce(t *testing.T) {
	srv, mock, appsDir := newWakeServer(t)
	app := sleepingApp(t, srv, mock, appsDir, "burst")

	const callers = 8
	done := make(chan int, callers)
	for i := 0; i < callers; i++ {
		go func() {
			rec := proxyExisting(t, srv, app, httptest.NewRequest(http.MethodGet, "/apps/burst/", nil))
			done <- rec.Code
		}()
	}
	for i := 0; i < callers; i++ {
		if code := <-done; code != http.StatusOK {
			t.Errorf("concurrent request %d = %d, want 200", i, code)
		}
	}

	m, err := mock.Metrics(app.CurrentRelease)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.Restarts != 1 {
		t.Fatalf("restarts = %d, want exactly 1 — the wake was not shared", m.Restarts)
	}
}
