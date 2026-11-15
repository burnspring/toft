package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// GET /api/v1/app/self — the mode an app is told it is running in.
//
// The whole point of these is the deploy window. `apps.running_mode` is written
// at the flip, which is after the readiness probe, and the probe is usually the
// new workload's first request — so an app that reads its mode at startup reads
// the row describing the release it is replacing. On a first deploy that column
// is empty, which reads as `shared`, and an isolated app was told `shared` for
// the life of the process.

type selfModeFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
}

func newSelfModeFixture(t *testing.T, executionMode, runningMode string) *selfModeFixture {
	t.Helper()
	srv := newTestServer(t)
	f := &selfModeFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	f.app = &database.App{
		Slug: "recon", Name: "recon", OwnerID: "owner",
		Framework: "streamlit", Status: database.AppStatusRunning,
		Visibility:    database.VisibilityAuthenticated,
		ExecutionMode: executionMode,
		RunningMode:   runningMode,
	}
	if err := srv.db.CreateApp(f.app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if runningMode != "" {
		// CreateApp does not carry it; the flip is what writes it.
		if err := srv.db.UpdateApp(f.app); err != nil {
			t.Fatalf("recording the running mode: %v", err)
		}
	}
	return f
}

// deploy records a release in one mode and returns the workload name the
// builder would have started it under.
func (f *selfModeFixture) deploy(t *testing.T, mode string) string {
	t.Helper()
	d := &database.Deployment{
		AppID: f.app.ID, Status: database.DeployStatusBuilding, ExecutionMode: mode,
	}
	if err := f.srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	return builder.ReleaseProcessName(f.app.Slug, d.ID)
}

// mint issues the app token one workload start holds. An empty workload is a
// token minted before workloads were named, which still has to answer.
func (f *selfModeFixture) mint(t *testing.T, id, workload string) string {
	t.Helper()
	secret := appTokenSecretPrefix + id + "-secret"
	if err := f.srv.db.CreateAppToken(&database.AppToken{
		ID: id, AppID: f.app.ID, Workload: workload,
		TokenHash: hashAPIKey(secret), Prefix: secret[:8],
	}); err != nil {
		t.Fatalf("create app token: %v", err)
	}
	return secret
}

func (f *selfModeFixture) mode(t *testing.T, token string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/app/self", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.ContentLength = 0
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return out.Mode
}

// The bug, exactly: a first isolated deploy, asked before the flip.
func TestAnIsolatedWorkloadIsNotToldItIsSharedBeforeTheFlip(t *testing.T) {
	f := newSelfModeFixture(t, string(runtime.ModeIsolated), "")
	workload := f.deploy(t, string(runtime.ModeIsolated))
	token := f.mint(t, "tok-new", workload)

	if got := f.mode(t, token); got != string(runtime.ModeIsolated) {
		t.Errorf("mode = %q, want isolated — the row has not been flipped yet, so the "+
			"answer has to come from this workload's own deployment", got)
	}
}

// A release still serving in the mode it was started under, while a deploy of
// the other mode is in flight. Both tokens are live; each gets its own answer.
func TestEachWorkloadIsToldItsOwnModeDuringAModeChange(t *testing.T) {
	f := newSelfModeFixture(t, string(runtime.ModeIsolated), string(runtime.ModeShared))
	old := f.mint(t, "tok-old", f.deploy(t, string(runtime.ModeShared)))
	incoming := f.mint(t, "tok-in", f.deploy(t, string(runtime.ModeIsolated)))

	if got := f.mode(t, old); got != string(runtime.ModeShared) {
		t.Errorf("the outgoing release's mode = %q, want shared", got)
	}
	if got := f.mode(t, incoming); got != string(runtime.ModeIsolated) {
		t.Errorf("the incoming release's mode = %q, want isolated", got)
	}
}

// A token whose workload names no deployment — one minted before the naming
// rule, or whose row has been pruned — falls back to the app, which is the
// reading every other caller of RunningModeOrShared takes.
func TestATokenWithNoWorkloadFallsBackToTheApp(t *testing.T) {
	f := newSelfModeFixture(t, string(runtime.ModeIsolated), string(runtime.ModeIsolated))
	token := f.mint(t, "tok-anon", "")

	if got := f.mode(t, token); got != string(runtime.ModeIsolated) {
		t.Errorf("mode = %q, want isolated from the app row", got)
	}
}
