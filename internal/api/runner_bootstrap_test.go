package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/telemetry/telemetryharness"
)

// An isolated workload is handed its app's outbound credential when it
// bootstraps, because that is the only moment the platform can give it one.
//
// The secret is hashed on the way in and cannot be re-derived, so a token
// minted at deploy is unreachable by the time a pod starts — which is why an
// isolated app had no platform identity at all, and why a woken or restored one
// lost the identity it had. See ADR §26 and migration 00041.

// presigningStore is a local store that can also presign, which is the one
// capability bootstrap requires and the one a LocalStore deliberately refuses.
// Faking it here keeps these tests about the token rather than about S3.
type presigningStore struct{ *artifact.LocalStore }

func (presigningStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "https://example.invalid/release.tar.gz", nil
}

func (presigningStore) PresignPut(context.Context, string, time.Duration) (string, error) {
	return "https://example.invalid/output", nil
}

// identityRuntime is an isolated backend that verifies workload identities the
// way the real one does — by answering with facts the platform holds about a
// pod, never with a claim the caller made. The token here stands in for a
// projected ServiceAccount token; what matters to these tests is that the app
// id and workload name come back from the *verifier* rather than the request.
type identityRuntime struct {
	runtime.ServiceRuntime
	appID string
}

func (r identityRuntime) VerifyWorkloadIdentity(_ context.Context, token string) (runtime.WorkloadIdentity, error) {
	// "pod:<workload>:<runID>" — the shape a pod's labels would have given.
	parts := strings.SplitN(token, ":", 3)
	if len(parts) != 3 || parts[0] != "pod" {
		return runtime.WorkloadIdentity{}, errors.New("not a workload of this install")
	}
	return runtime.WorkloadIdentity{AppID: r.appID, WorkloadName: parts[1], RunID: parts[2]}, nil
}

type bootstrapFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
}

func newBootstrapFixture(t *testing.T) *bootstrapFixture {
	t.Helper()
	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "reports", Name: "Reports", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated,
		// An isolated workload fetches its release, so bootstrap refuses an app
		// that has no published artifact before it gets as far as the token.
		ReleaseRef: "artifact:releases/reports/dep-1.tar.gz",
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	local, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	srv.artifacts = presigningStore{local}
	// Bootstrap authenticates against the isolated backend rather than against
	// a token the platform minted, so a Server with no cluster has nothing to
	// verify with and answers 401 — which is what TestBootstrapNeedsACluster
	// pins.
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), identityRuntime{appID: app.ID})
	return &bootstrapFixture{srv: srv, router: srv.Router(http.NotFoundHandler()), app: app}
}

// bootstrap calls the endpoint as a workload and returns the response.
func (f *bootstrapFixture) bootstrap(t *testing.T, workload string, runID string) runner.BootstrapResponse {
	t.Helper()
	token := "pod:" + workload + ":" + runID
	body, _ := json.Marshal(runner.BootstrapRequest{
		Version: f.srv.cfg.RunnerVersion, WorkloadName: workload,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/bootstrap", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap %s: %d %s", workload, rec.Code, rec.Body.String())
	}
	var resp runner.BootstrapResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

func (f *bootstrapFixture) resolves(t *testing.T, secret string) bool {
	t.Helper()
	got, err := f.srv.db.ResolveAppToken(hashAPIKey(secret))
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	return got != nil
}

func TestAServiceWorkloadIsHandedItsAppsToken(t *testing.T) {
	f := newBootstrapFixture(t)

	resp := f.bootstrap(t, "reports-abc123", "")

	if resp.AppToken == "" {
		t.Fatal("an isolated service workload got no app token — it has no platform identity")
	}
	if !strings.HasPrefix(resp.AppToken, appTokenSecretPrefix) {
		t.Errorf("token %q is not an tofta_ credential", resp.AppToken)
	}
	if !f.resolves(t, resp.AppToken) {
		t.Error("the token handed over does not authenticate")
	}
	// The app's own environment is what this response already carried; the
	// token joins it rather than displacing it.
	if resp.AppID != f.app.ID {
		t.Errorf("app id = %q", resp.AppID)
	}
}

// A job gets none, and the reason is ADR §27 D1: the argument that a scheduled
// task must be an HTTP trigger rests on a job workload holding no app token.
func TestAJobWorkloadIsNotHandedAnAppToken(t *testing.T) {
	f := newBootstrapFixture(t)
	run := &database.JobRun{AppID: f.app.ID, JobName: "nightly"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, "reports-job-1", run.ID)

	if resp.AppToken != "" {
		t.Error("a job workload was handed an app token; §27 D1 rests on it holding none")
	}
	if resp.RunID != run.ID {
		t.Errorf("run id = %q, want the job's", resp.RunID)
	}
}

// A run's bootstrap carries what its job declared to run — a single binary's
// args when toft.toml gave args — and somewhere to write its one output. Params
// are a run's only input, so there is no input URL.
func TestARunIsBootstrappedWithItsArgsAndAnOutputURL(t *testing.T) {
	f := newBootstrapFixture(t)
	def := declareJob(t, f.srv.db, f.app.ID, database.JobDefinition{Name: "archive",
		Args: []string{"job", "archive"}, TimeoutSec: 600})
	run := &database.JobRun{AppID: f.app.ID, DefinitionID: def.ID, JobName: "archive",
		StartedBy: database.JobStartedByCLI, RerunOf: ""}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, "reports-job-1", run.ID)

	if len(resp.Args) != 2 || resp.Args[1] != "archive" || resp.Command != "" {
		t.Errorf("args = %v command = %q, want the declared args", resp.Args, resp.Command)
	}
	if resp.Trigger != database.JobStartedByCLI || resp.TimeoutSeconds != 600 {
		t.Errorf("trigger = %q timeout = %d", resp.Trigger, resp.TimeoutSeconds)
	}
	if resp.OutputURL == "" {
		t.Error("no output url — a job decides whether to write an output, and the place has to exist first")
	}
}

// With telemetry on, the store is wrapped in the decorator, which hides the
// presigner from a bare type assertion. Bootstrap must still find it: an S3
// install with telemetry on otherwise answers every isolated pod 409.
func TestBootstrapPresignsThroughTheTelemetryDecorator(t *testing.T) {
	telemetryharness.Enable(t)
	f := newBootstrapFixture(t)
	f.srv.artifacts = artifact.Instrumented(f.srv.artifacts, "test")
	def := declareJob(t, f.srv.db, f.app.ID, database.JobDefinition{Name: "archive",
		Args: []string{"job", "archive"}, TimeoutSec: 600})
	run := &database.JobRun{AppID: f.app.ID, DefinitionID: def.ID, JobName: "archive",
		StartedBy: database.JobStartedByCLI}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, "reports-job-1", run.ID)

	if resp.ArtifactURL == "" {
		t.Error("no artifact url through the decorator")
	}
	if resp.OutputURL == "" {
		t.Error("no output url through the decorator")
	}
}

// Starting a workload again supersedes its own previous token — the process
// holding that one is gone — and leaves a different release's alone, which is
// the case a rolling deploy depends on.
func TestAWorkloadStartSupersedesOnlyItsOwnPreviousToken(t *testing.T) {
	f := newBootstrapFixture(t)

	first := f.bootstrap(t, "reports-abc123", "")
	other := f.bootstrap(t, "reports-def456", "")
	if !f.resolves(t, first.AppToken) || !f.resolves(t, other.AppToken) {
		t.Fatal("both workloads' tokens should start out live")
	}

	// The same workload starts again: a pod recreated, an app woken, a crash
	// restored.
	second := f.bootstrap(t, "reports-abc123", "")

	if f.resolves(t, first.AppToken) {
		t.Error("the previous start's token is still live; it outlived the process holding it")
	}
	if !f.resolves(t, second.AppToken) {
		t.Error("the new start's token does not work")
	}
	if !f.resolves(t, other.AppToken) {
		t.Error("another release's token was revoked — a deploy would break the outgoing app")
	}
}

// The platform verifies rather than mints, so an install with no cluster has
// nothing to verify against — and a caller there is not a workload of this
// install. Absent, not dormant.
func TestBootstrapNeedsACluster(t *testing.T) {
	f := newBootstrapFixture(t)
	f.srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	body, _ := json.Marshal(runner.BootstrapRequest{
		Version: f.srv.cfg.RunnerVersion, WorkloadName: "reports-abc123",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/bootstrap", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer pod:reports-abc123:")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bootstrap against an install with no isolated backend: %d %s", rec.Code, rec.Body.String())
	}
}

// A credential the cluster does not recognise gets the same opaque refusal as
// every other failure here.
func TestBootstrapRefusesAnUnverifiableCaller(t *testing.T) {
	f := newBootstrapFixture(t)

	body, _ := json.Marshal(runner.BootstrapRequest{
		Version: f.srv.cfg.RunnerVersion, WorkloadName: "reports-abc123",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/bootstrap", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer whatever-app-code-found-lying-around")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unverifiable caller got %d %s", rec.Code, rec.Body.String())
	}
}

// The deadlock this pins: a pod bootstraps in the middle of its own deploy,
// while the builder is still waiting for it to become ready — and the flip that
// writes `apps.release_ref` runs *after* that wait. An app deploying isolated
// for the first time therefore has an empty reference at exactly the moment its
// pod asks, and reading the app row refused it the artifact the build had just
// published. Readiness could then never pass, so the flip never ran, so the
// next attempt failed identically.
func TestAPodGetsTheReleaseItWasStartedForNotTheAppsCurrentOne(t *testing.T) {
	f := newBootstrapFixture(t)

	// Never flipped: this app has no serving release at all.
	f.app.ReleaseRef = ""
	if err := f.srv.db.UpdateApp(f.app); err != nil {
		t.Fatalf("clearing the release ref: %v", err)
	}

	dep := &database.Deployment{
		AppID:         f.app.ID,
		Status:        database.DeployStatusBuilding,
		ExecutionMode: string(runtime.ModeIsolated),
	}
	if err := f.srv.db.CreateDeployment(dep, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	// What the builder records the moment the publish succeeds — before the
	// start, and therefore already true when the pod asks.
	packed := int64(145 << 20)
	if err := f.srv.db.SetDeploymentSizes(dep.ID, nil, &packed); err != nil {
		t.Fatalf("recording the artifact size: %v", err)
	}

	resp := f.bootstrap(t, builder.ReleaseProcessName(f.app.Slug, dep.ID), "")

	if resp.ArtifactURL == "" {
		t.Fatal("the pod was refused the release its own deploy had just published")
	}
}

// The other half of the same rule: a workload whose artifact was never
// published must not be handed a URL derived from its name alone, or a pod
// would fetch a key the store has nothing at.
func TestAWorkloadWithNoPublishedArtifactFallsBackToTheApp(t *testing.T) {
	f := newBootstrapFixture(t)

	dep := &database.Deployment{
		AppID:         f.app.ID,
		Status:        database.DeployStatusBuilding,
		ExecutionMode: string(runtime.ModeIsolated),
	}
	if err := f.srv.db.CreateDeployment(dep, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	// No SetDeploymentSizes: nothing was published for this deployment. The
	// app's own reference is what remains, and it is what the pod gets.
	resp := f.bootstrap(t, builder.ReleaseProcessName(f.app.Slug, dep.ID), "")

	if resp.ArtifactURL == "" {
		t.Fatal("the app's own release should still have been served")
	}
}

// --- The exchange ---

// exchange calls the refresh route as a workload and returns the recorder.
func (f *bootstrapFixture) exchange(t *testing.T, identityWorkload, runID, askFor string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(runner.TokenRequest{
		Version: f.srv.cfg.RunnerVersion, WorkloadName: askFor,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/token", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer pod:"+identityWorkload+":"+runID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// A service workload outlives its run token — it has no run to be bounded by,
// so it gets an hour and keeps running. Before the exchange the runner had no
// way back: it bootstraps once, so from that minute on every consumption report
// and every stage mark answered 401 for the life of the pod.
func TestAServiceExchangesItsRunTokenWithoutRebootstrapping(t *testing.T) {
	f := newBootstrapFixture(t)

	boot := f.bootstrap(t, "reports-abc123", "")
	if boot.AppToken == "" {
		t.Fatal("no app token to protect")
	}

	rec := f.exchange(t, "reports-abc123", "", "reports-abc123")
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: %d %s", rec.Code, rec.Body.String())
	}
	var resp runner.TokenResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RunToken == "" {
		t.Fatal("the exchange returned no token")
	}

	// It is a run token for this app and this workload, and nothing wider.
	claims, err := f.srv.jwt.ValidateRunToken(resp.RunToken, auth.AudienceRun)
	if err != nil {
		t.Fatalf("the exchanged token is not a run token: %v", err)
	}
	if claims.AppID != f.app.ID || claims.WorkloadName != "reports-abc123" {
		t.Errorf("claims = %+v", claims)
	}
	if claims.RunID != "" {
		t.Errorf("a service's token names a run: %q", claims.RunID)
	}

	// The whole reason this is not a second bootstrap: the app token the
	// running process holds in its environment must survive. Minting a fresh
	// one would supersede it and break a live app to refresh a credential it
	// was not using.
	if !f.resolves(t, boot.AppToken) {
		t.Error("the exchange superseded the app token the running process holds")
	}
}

// A run's token is bounded by the run, deliberately, so that a token found
// after the run cannot be replayed. Exchanging one would undo exactly that.
func TestARunCannotExchangeItsToken(t *testing.T) {
	f := newBootstrapFixture(t)

	// No bootstrap first: the refusal is on the identity naming a run, and it
	// lands before anything about the run is looked up.
	rec := f.exchange(t, "reports-job-1", "run-1", "reports-job-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("exchange for a run: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// The identity names the workload it was issued to, exactly as at bootstrap.
func TestTheExchangeRefusesAnotherWorkloadsName(t *testing.T) {
	f := newBootstrapFixture(t)

	rec := f.exchange(t, "reports-abc123", "", "reports-someone-else")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("exchange for another workload: %d, want 401", rec.Code)
	}
}

// The exchange takes the credential the *cluster* issued — the same one
// bootstrap takes — and nothing else. A run token presented here is not a way
// to mint another one.
func TestTheExchangeRefusesARunToken(t *testing.T) {
	f := newBootstrapFixture(t)
	boot := f.bootstrap(t, "reports-abc123", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/token", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+boot.RunToken)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("exchange with a run token: %d, want 401", rec.Code)
	}
}

// keyEchoStore presigns by echoing the key, so a test can see *which* release a
// workload was handed rather than only that it was handed one.
type keyEchoStore struct{ *artifact.LocalStore }

func (keyEchoStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://example.invalid/" + key, nil
}

func (keyEchoStore) PresignPut(context.Context, string, time.Duration) (string, error) {
	return "https://example.invalid/output", nil
}

// A job runs the code it was queued against. The run snapshots its release at
// enqueue; bootstrap used to ignore that and resolve the app's current release,
// because a job's workload is named after its run and not a deployment — so a
// deploy that landed while the run was queued silently changed what it executed.
func TestJobBootstrapUsesQueuedRelease(t *testing.T) {
	f := newBootstrapFixture(t)
	local, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.artifacts = keyEchoStore{local}
	const queued = "releases/reports/dep-1.tar.gz"
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Put(context.Background(), queued, tree); err != nil {
		t.Fatalf("storing the queued release: %v", err)
	}

	run := &database.JobRun{AppID: f.app.ID, JobName: "nightly", ReleaseRef: "artifact:" + queued}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	// A deploy lands while the run waits.
	f.app.ReleaseRef = "artifact:releases/reports/dep-2.tar.gz"
	if err := f.srv.db.UpdateApp(f.app); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, run.ID, run.ID)

	if !strings.HasSuffix(resp.ArtifactURL, queued) {
		t.Errorf("artifact url = %q, want the queued release %s", resp.ArtifactURL, queued)
	}
}

// The other half: a queued release that retention has since deleted is refused
// by name, never quietly replaced by the current one.
func TestJobBootstrapRefusesAQueuedReleaseThatIsGone(t *testing.T) {
	f := newBootstrapFixture(t)
	run := &database.JobRun{AppID: f.app.ID, JobName: "nightly",
		ReleaseRef: "artifact:releases/reports/dep-0.tar.gz"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(runner.BootstrapRequest{Version: f.srv.cfg.RunnerVersion, WorkloadName: run.ID})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/bootstrap", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer pod:"+run.ID+":"+run.ID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no longer stored") {
		t.Errorf("bootstrap = %d %s, want 409 naming the missing release", rec.Code, rec.Body.String())
	}
}

// A run's instant reaches the workload in its schedule's own zone, so one string
// answers both which day and in whose calendar.
func TestBootstrapHandsTheRunItsInstantInTheSchedulesZone(t *testing.T) {
	f := newBootstrapFixture(t)
	def := declareJob(t, f.srv.db, f.app.ID, database.JobDefinition{Name: "nightly", Command: "true",
		Schedule: "CRON_TZ=Europe/London 0 3 * * *", TimeoutSec: 600})
	at := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	run := &database.JobRun{AppID: f.app.ID, DefinitionID: def.ID, JobName: "nightly", ScheduledFor: &at}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, run.ID, run.ID)

	if resp.ScheduledFor != "2026-10-13T03:00:00+01:00" {
		t.Errorf("scheduled_for = %q, want the London wall-clock time", resp.ScheduledFor)
	}

	// An instant a person named with an offset reaches the job as named, not
	// in the schedule's zone or UTC (BB-045, fourth 2026-11-03 black-box list).
	named, _ := time.Parse(time.RFC3339, "2026-11-03T09:00:00-05:00")
	byHand := &database.JobRun{AppID: f.app.ID, DefinitionID: def.ID, JobName: "nightly",
		StartedBy: database.JobStartedByCLI, ScheduledFor: &named}
	if err := f.srv.db.EnqueueJobRun(byHand, 0); err != nil {
		t.Fatal(err)
	}
	if got := f.bootstrap(t, byHand.ID, byHand.ID).ScheduledFor; got != "2026-11-03T09:00:00-05:00" {
		t.Errorf("a named instant reached the job as %q, want the text as named", got)
	}
}

// postBootstrap presents a raw bearer to the bootstrap route.
func (f *bootstrapFixture) postBootstrap(t *testing.T, bearer, workload string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(runner.BootstrapRequest{Version: f.srv.cfg.RunnerVersion, WorkloadName: workload})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/bootstrap", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// A host run proves itself with the credential the platform wrote into its run
// directory, once: a second presentation — anything else on the machine that
// read the file after the runner did — opens nothing. Its release is on this
// machine, so it is handed no URL to fetch one from.
func TestAHostRunBootstrapsOnceAndIsHandedNoArtifact(t *testing.T) {
	f := newBootstrapFixture(t)
	f.srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil) // no cluster at all
	run := &database.JobRun{AppID: f.app.ID, JobName: "nightly", ExecutionMode: "shared"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	token, err := f.srv.jwt.MintHostBootstrapToken(f.app.ID, run.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	first := f.postBootstrap(t, token, run.ID)
	if first.Code != http.StatusOK {
		t.Fatalf("first bootstrap = %d %s", first.Code, first.Body.String())
	}
	var resp runner.BootstrapResponse
	json.Unmarshal(first.Body.Bytes(), &resp)
	if resp.RunID != run.ID || resp.ArtifactURL != "" || resp.RunToken == "" {
		t.Errorf("response = run %q, artifact %q, token set %v; want the run, no artifact, a token",
			resp.RunID, resp.ArtifactURL, resp.RunToken != "")
	}

	if again := f.postBootstrap(t, token, run.ID); again.Code != http.StatusUnauthorized {
		t.Errorf("a second presentation = %d, want 401", again.Code)
	}
}

// A run token — what app code holds — is not a bootstrap credential, host or
// otherwise.
func TestARunTokenCannotBootstrapAsAHost(t *testing.T) {
	f := newBootstrapFixture(t)
	f.srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	run := &database.JobRun{AppID: f.app.ID, JobName: "nightly"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	tok, _ := f.srv.jwt.MintRunToken(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
	if rec := f.postBootstrap(t, tok, run.ID); rec.Code != http.StatusUnauthorized {
		t.Errorf("bootstrap with a run token = %d, want 401", rec.Code)
	}
}

// A run's token outlives its deadline by runTokenGrace: what the run printed
// before the platform stopped it is shipped after the stop, and a token that
// died at the deadline refused it — a timed-out run kept no output at all.
func TestARunsTokenOutlivesItsDeadlineByTheGrace(t *testing.T) {
	f := newBootstrapFixture(t)
	def := declareJob(t, f.srv.db, f.app.ID, database.JobDefinition{Name: "quick", Command: "true",
		TimeoutSec: 5})
	run := &database.JobRun{AppID: f.app.ID, DefinitionID: def.ID, JobName: "quick"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, run.ID, run.ID)
	claims, err := f.srv.jwt.ValidateRunToken(resp.RunToken, auth.AudienceRun)
	if err != nil {
		t.Fatal(err)
	}
	life := time.Until(claims.ExpiresAt.Time)
	if want := 5*time.Second + runTokenGrace; life < want-5*time.Second || life > want+time.Second {
		t.Errorf("run token lives %v, want about %v (timeout + grace)", life, want)
	}
	if resp.TimeoutSeconds != 5 {
		t.Errorf("timeout_seconds = %d, want the run's own 5", resp.TimeoutSeconds)
	}
}

// An isolated workload takes its environment from the bootstrap, so an attached
// credential has to be in it — it was not, and on EKS a credential never
// reached an isolated app while the same app in shared mode had it (ENV.14).
func TestABootstrapCarriesTheAppsAttachedCredentials(t *testing.T) {
	f := newBootstrapFixture(t)
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "owner"}
	cred, err := f.srv.db.CreateCredential(context.Background(), "warehouse", "",
		map[string]string{"WAREHOUSE_PASSWORD": "rotated"}, f.srv.encryptor.Encrypt, audit)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := f.srv.db.AttachCredential(context.Background(), f.app.ID, cred.ID, audit); err != nil {
		t.Fatalf("AttachCredential: %v", err)
	}

	// A run bootstraps through the same environment read, before the
	// service/run branch.
	resp := f.bootstrap(t, "reports-abc123", "")
	if got := resp.Env["WAREHOUSE_PASSWORD"]; got != "rotated" {
		t.Errorf("bootstrap env WAREHOUSE_PASSWORD = %q, want the credential's value", got)
	}
}

// A workload is told what its own release's build detected, not what the app
// row says. The app row is rewritten at the next build's detect and never
// reverted by a failed one, so a pod of the serving release restarted
// mid-build — or after a failed build — read the new release's framework and
// entrypoint and ran the wrong program (ADR §127).
func TestBootstrapAnswersDetectionFromTheWorkloadsOwnDeployment(t *testing.T) {
	f := newBootstrapFixture(t)
	serving := &database.Deployment{AppID: f.app.ID, Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(serving, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.db.SetDeploymentDetection(serving.ID, "flask", "app.py", ""); err != nil {
		t.Fatal(err)
	}
	// A later build detected something else and wrote it to the app row.
	if err := f.srv.db.SetAppDetection(f.app.ID, "streamlit", "main.py", "streamlit run x"); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, builder.ReleaseProcessName(f.app.Slug, serving.ID), "")
	if resp.Framework != "flask" || resp.Entrypoint != "app.py" || resp.StartCmd != "" {
		t.Errorf("serving workload told %q/%q/%q, want its own build's flask/app.py", resp.Framework, resp.Entrypoint, resp.StartCmd)
	}
}

// A deployment made before the columns existed records nothing, and its
// workload is answered from the app row, as every workload was before.
func TestBootstrapFallsBackToTheAppRowForAnUndescribedDeployment(t *testing.T) {
	f := newBootstrapFixture(t)
	old := &database.Deployment{AppID: f.app.ID, Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(old, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.db.SetAppDetection(f.app.ID, "fastapi", "main.py", ""); err != nil {
		t.Fatal(err)
	}

	resp := f.bootstrap(t, builder.ReleaseProcessName(f.app.Slug, old.ID), "")
	if resp.Framework != "fastapi" || resp.Entrypoint != "main.py" {
		t.Errorf("undescribed deployment told %q/%q, want the app row's fastapi/main.py", resp.Framework, resp.Entrypoint)
	}
}

// agentRuntime is a shared backend that vouches for its workloads the way the
// shared-mode agent does: a token verifies while the process it was issued to
// is alive, and never after.
type agentRuntime struct {
	runtime.ServiceRuntime
	appID string
	live  map[string]string // token -> workload
}

func (r agentRuntime) VerifyWorkloadIdentity(_ context.Context, token string) (runtime.WorkloadIdentity, error) {
	w, ok := r.live[token]
	if !ok {
		return runtime.WorkloadIdentity{}, errors.New("not a live workload")
	}
	return runtime.WorkloadIdentity{AppID: r.appID, WorkloadName: w}, nil
}

func (f *bootstrapFixture) bootstrapWith(t *testing.T, token, workload string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(runner.BootstrapRequest{Version: f.srv.cfg.RunnerVersion, WorkloadName: workload})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/bootstrap", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// A shared service placed by the agent bootstraps on the identity the agent
// issued it (ADR §130): it is handed its environment, its app
// token and its role key, and no address — its address is its placer's, and the
// pod's 0.0.0.0:8080 would put it on every interface (D2).
func TestAHostServiceBootstrapsOnTheAgentsIdentity(t *testing.T) {
	f := newBootstrapFixture(t)
	live := map[string]string{"agent-token": "reports-abc123"}
	f.srv.rts = runtime.NewRuntimes(agentRuntime{runtime.NewMockRuntime(nil), f.app.ID, live}, identityRuntime{appID: f.app.ID})
	f.router = f.srv.Router(http.NotFoundHandler())

	rec := f.bootstrapWith(t, "agent-token", "reports-abc123")
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", rec.Code, rec.Body.String())
	}
	var resp runner.BootstrapResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Port != 0 || resp.BindAddress != "" {
		t.Errorf("a host service was handed an address: %d %q", resp.Port, resp.BindAddress)
	}
	if resp.AppToken == "" || !f.resolves(t, resp.AppToken) {
		t.Error("a host service got no working app token at bootstrap")
	}
	if resp.RoleKey == "" || resp.RunToken == "" {
		t.Errorf("role key %q, run token %q", resp.RoleKey, resp.RunToken)
	}
	if resp.ArtifactURL != "" {
		t.Error("a host service was sent an artifact to fetch; its release is on this machine")
	}
}

// ADR §130, the platform's half: a token presented after its process
// exited is refused — the agent no longer vouches for it, and nothing else
// will.
func TestBootstrapRefusesADeadHostServiceToken(t *testing.T) {
	f := newBootstrapFixture(t)
	live := map[string]string{}
	f.srv.rts = runtime.NewRuntimes(agentRuntime{runtime.NewMockRuntime(nil), f.app.ID, live}, identityRuntime{appID: f.app.ID})
	f.router = f.srv.Router(http.NotFoundHandler())

	if rec := f.bootstrapWith(t, "agent-token-of-an-exited-process", "reports-abc123"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a dead host service's token: %d, want 401", rec.Code)
	}
	// And the token exchange, the other route the identity opens.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runner/token", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer agent-token-of-an-exited-process")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a dead host service's token exchange: %d, want 401", rec.Code)
	}
}

// A runner that leaves workload_name out of the body is keyed by its verified
// identity's name: its role key is the one the gateway signs with for that
// workload, and its second start retires the first start's app token.
func TestABootstrapWithNoBodyNameIsKeyedByTheIdentity(t *testing.T) {
	f := newBootstrapFixture(t)
	live := map[string]string{"agent-token": "reports-abc123"}
	f.srv.rts = runtime.NewRuntimes(agentRuntime{runtime.NewMockRuntime(nil), f.app.ID, live}, identityRuntime{appID: f.app.ID})
	f.router = f.srv.Router(http.NotFoundHandler())

	start := func() runner.BootstrapResponse {
		rec := f.bootstrapWith(t, "agent-token", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("bootstrap: %d %s", rec.Code, rec.Body.String())
		}
		var resp runner.BootstrapResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp
	}
	first := start()
	if want := f.srv.jwt.RoleKey(f.app.ID, "reports-abc123"); first.RoleKey != want {
		t.Error("the role key was derived from the empty body name, not the identity's")
	}
	second := start()
	if f.resolves(t, first.AppToken) {
		t.Error("the previous start's token is still live; it was minted under no workload")
	}
	if !f.resolves(t, second.AppToken) {
		t.Error("the new start's token does not work")
	}
}

// Every read of a run gives scheduled_for in its schedule's zone, as the job's
// TOFT_SCHEDULED_FOR does — a run for 15:00Z on a New York job read back as
// 15:00:00Z beside the job's 10:00:00-05:00 (D-08). An offset a person named
// is kept as named.
func TestARunIsReadInItsSchedulesZone(t *testing.T) {
	f := newBootstrapFixture(t)
	def := declareJob(t, f.srv.db, f.app.ID, database.JobDefinition{Name: "nightly", Command: "true",
		Schedule: "CRON_TZ=America/New_York 0 10 * * *", TimeoutSec: 600})
	at := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	run := &database.JobRun{AppID: f.app.ID, DefinitionID: def.ID, JobName: "nightly",
		StartedBy: database.JobStartedByCLI, ScheduledFor: &at}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	read, _ := f.srv.db.GetAppJobRun(f.app.ID, run.ID, "")
	f.srv.decorateRun(f.app, read)
	if got, _ := json.Marshal(personRun(read)); !strings.Contains(string(got), `"scheduled_for":"2026-11-02T10:00:00-05:00"`) {
		t.Errorf("person read = %s", got)
	}
	again, _ := f.srv.db.GetAppJobRun(f.app.ID, run.ID, "")
	if got, _ := json.Marshal(f.srv.appRunResponse(again, false)); !strings.Contains(string(got), `"scheduled_for":"2026-11-02T10:00:00-05:00"`) {
		t.Errorf("app read = %s", got)
	}

	named, _ := time.Parse(time.RFC3339, "2026-11-03T09:00:00+05:30")
	byHand := &database.JobRun{AppID: f.app.ID, DefinitionID: def.ID, JobName: "nightly",
		StartedBy: database.JobStartedByCLI, ScheduledFor: &named}
	if err := f.srv.db.EnqueueJobRun(byHand, 0); err != nil {
		t.Fatal(err)
	}
	read, _ = f.srv.db.GetAppJobRun(f.app.ID, byHand.ID, "")
	f.srv.decorateRun(f.app, read)
	if got, _ := json.Marshal(personRun(read)); !strings.Contains(string(got), `"scheduled_for":"2026-11-03T09:00:00+05:30"`) {
		t.Errorf("named read = %s", got)
	}
}

// A run mints its output URL at the moment it writes, with its run token: a
// long run's bootstrap URL may have expired by then (AS-4). The key is the
// run's own, and a finished run, a service token and a run token for another
// app's run are refused.
func TestARunMintsItsOutputURLAtWriteTime(t *testing.T) {
	f := newBootstrapFixture(t)
	keys := &keyRecordingStore{LocalStore: f.srv.artifacts.(presigningStore).LocalStore}
	f.srv.artifacts = artifact.Instrumented(keys, "test")
	run := &database.JobRun{AppID: f.app.ID, JobName: "report"}
	if err := f.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='running' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	post := func(p auth.RunTokenParams) *httptest.ResponseRecorder {
		tok, err := f.srv.jwt.MintRunToken(p)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, runner.OutputURLPath, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}

	rec := post(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("output-url = %d %s", rec.Code, rec.Body.String())
	}
	var resp runner.OutputURLResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.URL == "" || resp.Headers[runner.OutputBlobTypeHeader] != "BlockBlob" {
		t.Errorf("response = %+v, want a URL and the blob-type header", resp)
	}
	if want := artifact.RunOutputKey(f.app.ID, run.ID, "output"); keys.put != want || keys.ttl != artifact.OutputURLTTL {
		t.Errorf("signed %q for %s, want %q for %s", keys.put, keys.ttl, want, artifact.OutputURLTTL)
	}

	if rec := post(auth.RunTokenParams{AppID: "another-app", RunID: run.ID, WorkloadName: run.ID}); rec.Code != http.StatusNotFound {
		t.Errorf("another app's token for this run = %d, want 404", rec.Code)
	}
	if rec := post(auth.RunTokenParams{AppID: f.app.ID, WorkloadName: "reports-web"}); rec.Code != http.StatusBadRequest {
		t.Errorf("a service token = %d, want 400", rec.Code)
	}

	if _, err := f.srv.db.Conn().Exec(`UPDATE job_runs SET status='succeeded' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if rec := post(auth.RunTokenParams{AppID: f.app.ID, RunID: run.ID, WorkloadName: run.ID}); rec.Code != http.StatusConflict {
		t.Errorf("a finished run = %d, want 409", rec.Code)
	}
}

type keyRecordingStore struct {
	*artifact.LocalStore
	put string
	ttl time.Duration
}

func (*keyRecordingStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "https://example.invalid/release.tar.gz", nil
}

func (k *keyRecordingStore) PresignPut(_ context.Context, key string, ttl time.Duration) (string, error) {
	k.put, k.ttl = key, ttl
	return "https://example.invalid/output", nil
}
