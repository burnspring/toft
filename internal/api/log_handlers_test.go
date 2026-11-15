package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/applog"
	"toft/internal/builder"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/gitconn"
	"toft/internal/runtime"

	"github.com/gorilla/websocket"
)

// logTestEnv is a server wired to a real database and the mock runtime, with an
// app owned by "owner" and one deployment whose build log is a marker string we
// can grep responses for.
type logTestEnv struct {
	srv    *Server
	router http.Handler
	app    *database.App
	deploy *database.Deployment
}

// buildLogMarker stands in for the secrets a real build log carries — the raw
// stdout of npm/uv, which runs with every decrypted app secret in its
// environment. No response to an unauthorized caller may contain it.
const buildLogMarker = "SECRET_FROM_BUILD_ENV=hunter2"

func newLogTestEnv(t *testing.T) *logTestEnv {
	t.Helper()

	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)
	seedUser(t, srv.db, "reader", database.SystemRoleViewer)

	app := &database.App{
		Slug: "logtest", Name: "Log Test", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	deploy := &database.Deployment{AppID: app.ID, Status: "success", BuildLog: buildLogMarker}
	if err := srv.db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	// Register the process so the mock runtime will answer Logs. Port 0 means
	// no listener is opened — this test never proxies.
	if _, err := srv.rts.Shared().Deploy(runtime.WorkloadSpec{
		AppID: app.ID, Slug: app.Slug, Name: app.ProcessName(), Framework: app.Framework,
	}); err != nil {
		t.Fatalf("register mock process: %v", err)
	}

	return &logTestEnv{srv: srv, router: srv.Router(http.NotFoundHandler()), app: app, deploy: deploy}
}

// as issues a request through the real router as the named user, or anonymously
// when who is empty.
func (e *logTestEnv) as(t *testing.T, who, path string) *httptest.ResponseRecorder {
	t.Helper()
	return getAs(t, e.srv, e.router, who, path)
}

// logEndpoints are the five handlers that shipped with no authorization at all.
func (e *logTestEnv) logEndpoints() map[string]string {
	return map[string]string{
		"runtime logs":     fmt.Sprintf("/api/v1/apps/%s/logs", e.app.ID),
		"runtime stream":   fmt.Sprintf("/api/v1/apps/%s/logs/stream", e.app.ID),
		"deployment list":  fmt.Sprintf("/api/v1/apps/%s/deployments", e.app.ID),
		"build log":        fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs", e.app.ID, e.deploy.ID),
		"build log stream": fmt.Sprintf("/api/v1/apps/%s/deployments/%s/stream", e.app.ID, e.deploy.ID),
		// The file forms are the same reads, and must refuse the same callers.
		"runtime log file": fmt.Sprintf("/api/v1/apps/%s/logs?download=1", e.app.ID),
		"build log file":   fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs?download=1", e.app.ID, e.deploy.ID),
	}
}

// An unauthenticated caller could read every app's build and runtime logs with
// one curl. This is finding 0.1 and is the regression most likely to return.
func TestLogEndpointsRejectAnonymousCallers(t *testing.T) {
	e := newLogTestEnv(t)

	for name, path := range e.logEndpoints() {
		t.Run(name, func(t *testing.T) {
			rec := e.as(t, "", path)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if body := rec.Body.String(); strings.Contains(body, buildLogMarker) {
				t.Errorf("build log leaked to an anonymous caller: %q", body)
			}
		})
	}
}

// A viewer can read a public app, so it must get 403 rather than 404 — but it
// must still not get logs. Logs are gated on ActionEditContent, not ActionRead.
func TestLogEndpointsRejectReadOnlyViewers(t *testing.T) {
	e := newLogTestEnv(t)

	for name, path := range e.logEndpoints() {
		t.Run(name, func(t *testing.T) {
			rec := e.as(t, "reader", path)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if strings.Contains(rec.Body.String(), buildLogMarker) {
				t.Error("build log leaked to a viewer")
			}
		})
	}
}

// A publisher with no grant on a private app must be told it does not exist,
// per the documented enumeration defence.
func TestLogEndpointsReturn404OnUnreadableApps(t *testing.T) {
	e := newLogTestEnv(t)
	rungOff(t, e.srv)

	e.app.Visibility = database.VisibilityPrivate
	if err := e.srv.db.UpdateApp(e.app); err != nil {
		t.Fatalf("update visibility: %v", err)
	}

	for name, path := range e.logEndpoints() {
		t.Run(name, func(t *testing.T) {
			rec := e.as(t, "stranger", path)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (403 would confirm the app exists)", rec.Code)
			}
			if strings.Contains(rec.Body.String(), buildLogMarker) {
				t.Error("build log leaked to a stranger")
			}
		})
	}
}

func TestLogEndpointsAllowTheOwner(t *testing.T) {
	e := newLogTestEnv(t)

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs", e.app.ID, e.deploy.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), buildLogMarker) {
		t.Errorf("owner did not receive the build log: %q", rec.Body.String())
	}

	rec = e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/deployments", e.app.ID))
	if rec.Code != http.StatusOK {
		t.Errorf("deployment list: owner got %d, want 200", rec.Code)
	}
}

// One route serves a build's log, and the list is not it.
//
// `deploymentView` embeds the deployment row, whose `BuildLog` is tagged
// `json:"build_log"` — so the list carried the full text of every retained
// build, to a page that refetched it every three seconds while a deploy was in
// flight. The suppression is a wire change (ADR §87) and this is what holds it:
// the field, and the bytes, both gone from the list and both still on the route
// beside it.
func TestDeploymentListCarriesNoBuildLog(t *testing.T) {
	e := newLogTestEnv(t)

	// A **failed** build, because `listableRelease` drops a successful one whose
	// release is not on disk — and this fixture writes no release, so the list
	// would otherwise be empty and assert nothing. A failed build never owned a
	// release and is always listed; it is also the build whose log anybody
	// actually goes looking for.
	failed := &database.Deployment{
		AppID: e.app.ID, Status: "failed", BuildLog: buildLogMarker,
	}
	if err := e.srv.db.CreateDeployment(failed, nil); err != nil {
		t.Fatalf("create failed deployment: %v", err)
	}

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/deployments", e.app.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("deployment list: owner got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), failed.ID) {
		t.Fatalf("the list does not hold the build under test, so it asserts nothing: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "build_log") {
		t.Errorf("the deployment list still names build_log: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), buildLogMarker) {
		t.Errorf("the deployment list still carries the build log: %q", rec.Body.String())
	}

	// The single-build read is the same shape, and answers the same way.
	rec = e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/deployments/%s", e.app.ID, failed.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("deployment detail: owner got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), buildLogMarker) {
		t.Errorf("the single-build read still carries the build log: %q", rec.Body.String())
	}

	// And the route that is the log still is one.
	rec = e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs", e.app.ID, failed.ID))
	if !strings.Contains(rec.Body.String(), buildLogMarker) {
		t.Errorf("the dedicated route no longer serves the build log: %q", rec.Body.String())
	}
}

// The deployment handlers authorized against the app in the URL but read the
// deployment by ID alone, so an owner could read any other app's build log by
// quoting its deployment ID against their own app. Second IDOR in 0.1.
func TestDeploymentLogsRejectDeploymentFromAnotherApp(t *testing.T) {
	e := newLogTestEnv(t)

	victim := &database.App{
		Slug: "victim", Name: "Victim", OwnerID: "stranger",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := e.srv.db.CreateApp(victim); err != nil {
		t.Fatalf("create victim app: %v", err)
	}
	victimDeploy := &database.Deployment{
		AppID: victim.ID, Status: "success", BuildLog: "VICTIM_SECRET=abc123",
	}
	if err := e.srv.db.CreateDeployment(victimDeploy, nil); err != nil {
		t.Fatalf("create victim deployment: %v", err)
	}

	// "owner" owns e.app but has no grant on victim. Quote the victim's
	// deployment ID against the app they do own.
	for name, path := range map[string]string{
		"build log":        fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs", e.app.ID, victimDeploy.ID),
		"build log stream": fmt.Sprintf("/api/v1/apps/%s/deployments/%s/stream", e.app.ID, victimDeploy.ID),
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.as(t, "owner", path)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "VICTIM_SECRET") {
				t.Errorf("cross-app build log leaked: %q", rec.Body.String())
			}
		})
	}
}

// ?lines=99999999 reached `pm2 logs --lines` unbounded.
func TestLogLinesParameterIsClamped(t *testing.T) {
	e := newLogTestEnv(t)

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"absurdly large", "?lines=99999999", maxLogLines},
		{"negative", "?lines=-5", 1},
		{"zero", "?lines=0", 1},
		{"unset", "", defaultLogLines},
		{"in range", "?lines=250", 250},
		{"not a number", "?lines=abc", defaultLogLines},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/logs%s", e.app.ID, tc.query))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			var body struct {
				Logs string `json:"logs"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// The mock runtime emits exactly the number of lines it was asked
			// for, so the response length is the clamped value.
			if got := len(strings.Split(body.Logs, "\n")); got != tc.want {
				t.Errorf("runtime produced %d lines, want %d", got, tc.want)
			}
		})
	}
}

// The log socket reads. gorilla/websocket learns that the peer has gone only
// on a read or a write, and this socket used to do neither until the app
// printed a line — so a closed tab on a quiet or stopped app held the
// goroutine, the follow and the narration subscription until it did, one
// subscription per visit. A stopped shared-mode app is the case: the socket
// holds on the narration alone, and the subscription must go when the client
// does.
func TestTheLogSocketLetsGoWhenTheClientCloses(t *testing.T) {
	srv := newTestServer(t)
	store := &capturingStore{}
	sink := applog.New(store)
	defer sink.Close()
	srv.platformLog = sink

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "quiet", Name: "Quiet", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
		ExecutionMode: string(runtime.ModeShared),
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	const platformOrigin = "http://platform.test"
	srv.cfg.BaseURL = platformOrigin
	ts := httptest.NewServer(srv.Router(http.NotFoundHandler()))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/apps/" + app.ID + "/logs/stream"
	cookie := sessionCookieFor(t, srv, "owner")
	h := http.Header{}
	h.Set("Cookie", cookie.Name+"="+cookie.Value)
	h.Set("Origin", platformOrigin)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Subscribed: the socket is holding on the narration alone.
	deadline := time.Now().Add(5 * time.Second)
	for sink.Listeners(app.ID) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sink.Listeners(app.ID); got != 1 {
		t.Fatalf("listeners while the pane is open = %d, want 1", got)
	}

	// The tab closes. Nothing is printed — the app is stopped — and the
	// subscription must still go.
	conn.Close()
	deadline = time.Now().Add(5 * time.Second)
	for sink.Listeners(app.ID) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sink.Listeners(app.ID); got != 0 {
		t.Fatalf("listeners after the client closed = %d, want 0 — the socket never read", got)
	}
}

// A download is the whole log, as a file: every line the row holds, the
// terminal's escapes removed, named for the app and the build. The pane used to
// save only what its own socket had received.
func TestBuildLogDownloadIsTheWholeStoredLog(t *testing.T) {
	e := newLogTestEnv(t)

	var sb strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "\x1b[32mline %d\x1b[0m\n", i)
	}
	d := &database.Deployment{AppID: e.app.ID, Status: "failed", CommitSHA: "abcdef1234567", BuildLog: sb.String()}
	if err := e.srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs?download=1", e.app.ID, d.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="logtest-build-abcdef1.log"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	body := rec.Body.String()
	if strings.Contains(body, "\x1b") {
		t.Errorf("the file still carries terminal escapes")
	}
	if got := strings.Count(body, "\n"); got != 400 {
		t.Errorf("the file has %d lines, want all 400", got)
	}
	if !strings.HasPrefix(body, "line 0\n") || !strings.HasSuffix(body, "line 399\n") {
		t.Errorf("the file is not the whole log, first to last: %q…", body[:40])
	}
}

// View raw is the same text shown in the tab rather than saved, and it is
// refused a script context: it is a build's output served from the platform's
// own origin.
func TestBuildLogRawIsInlineAndSandboxed(t *testing.T) {
	e := newLogTestEnv(t)

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs?raw=1", e.app.ID, e.deploy.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline", cd)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox" {
		t.Errorf("Content-Security-Policy = %q, want sandbox", csp)
	}
	if !strings.Contains(rec.Body.String(), buildLogMarker) {
		t.Errorf("raw view does not carry the log: %q", rec.Body.String())
	}
}

// The run log's file is not the pane's tail: it asks the backend for as much
// as a download may hold.
func TestRunLogDownloadIsNotTheTail(t *testing.T) {
	e := newLogTestEnv(t)

	rec := e.as(t, "owner", fmt.Sprintf("/api/v1/apps/%s/logs?download=1&lines=20", e.app.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="logtest-logs.txt"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	// The mock runtime emits exactly the number of lines it was asked for.
	if got := strings.Count(rec.Body.String(), "\n"); got != maxDownloadLines {
		t.Errorf("the file has %d lines, want %d", got, maxDownloadLines)
	}
}

// A shared workload's platform line is in two places now — the runner writes it
// to the agent's file and ships the same record to the store — and the tail
// shows it once, as an isolated workload's has always been shown.
func TestASharedTailShowsARunnersPlatformLineOnce(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	later := at.Add(time.Second)
	stored := []runtime.LogLine{
		{At: &at, Source: runtime.LogSourcePlatform, Stream: "stdout", Line: "==> Starting release x"},
		{At: &later, Source: runtime.LogSourcePlatform, Stream: "stdout", Line: "the health watcher said"},
	}
	backend := []runtime.LogLine{
		{At: &at, Source: runtime.LogSourcePlatform, Stream: "stdout", Line: "==> Starting release x"},
		{At: &later, Source: runtime.LogSourceApp, Stream: "stdout", Line: "serving"},
	}
	got := mergeLogs(&database.App{RunningMode: string(runtime.ModeShared)}, stored, backend).Lines
	n := 0
	for _, l := range got {
		if l.Line == "==> Starting release x" {
			n++
		}
	}
	if n != 1 || len(got) != 3 {
		t.Errorf("merged = %+v, want the runner's line once and the stored-only line kept", got)
	}
}

// And the live pane: whichever route delivers a platform line first sends it.
func TestTheLiveLogSendsAPlatformLineOnce(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	l := runtime.LogLine{At: &at, Source: runtime.LogSourcePlatform, Stream: "stdout", Line: "==> Started"}
	app := runtime.LogLine{At: &at, Source: runtime.LogSourceApp, Stream: "stdout", Line: "==> Started"}
	var r recentLines
	if !r.fresh(l) || r.fresh(l) {
		t.Error("a platform line was sent twice, or not at all")
	}
	if !r.fresh(app) || !r.fresh(app) {
		t.Error("an app line was deduped; an app may print the same line twice and mean it")
	}
}

// A start that failed deleted its workload and the output with it; the
// builder's tail of that output, kept in the failure, is the app's lines in
// `toft logs` while that deploy is the newest (BB-003).
func TestAFailedStartsOutputIsInTheAppLog(t *testing.T) {
	e := newLogTestEnv(t)
	// The crash's shape: the failed start's workload is gone, nothing else runs.
	if err := e.srv.rts.Shared().Delete(e.app.ProcessName()); err != nil {
		t.Fatal(err)
	}
	failed := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusFailed}
	if err := e.srv.db.CreateDeployment(failed, nil); err != nil {
		t.Fatal(err)
	}
	msg := "app did not become ready: the app exited before it was ready: it is crash-looping\n" +
		"    Last lines of its log:\n" +
		"    Traceback (most recent call last):\n" +
		"    SystemExit: boom at import"
	if err := e.srv.db.SetDeploymentFailure(failed.ID, "not_ready", "", msg); err != nil {
		t.Fatal(err)
	}

	read := func() []runtime.LogLine {
		rec := e.as(t, "owner", "/api/v1/apps/"+e.app.ID+"/logs?source=app&lines=50")
		if rec.Code != http.StatusOK {
			t.Fatalf("logs = %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Lines []runtime.LogLine `json:"lines"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Lines
	}
	got := read()
	var texts []string
	for _, l := range got {
		if l.Source != runtime.LogSourceApp {
			t.Errorf("--source app returned a %s line %q", l.Source, l.Line)
		}
		texts = append(texts, l.Line)
	}
	if !strings.Contains(strings.Join(texts, "\n"), "Traceback (most recent call last):\nSystemExit: boom at import") {
		t.Fatalf("app lines = %q, want the failed start's traceback", texts)
	}

	// A later deploy is the newer story.
	time.Sleep(10 * time.Millisecond)
	if err := e.srv.db.CreateDeployment(&database.Deployment{AppID: e.app.ID, Status: "success"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, l := range read() {
		if strings.Contains(l.Line, "boom at import") {
			t.Fatalf("the failed start's output outlived the deploy after it: %+v", l)
		}
	}
}

func TestFailedStartOutputReadsOnlyTheIndentedTail(t *testing.T) {
	msg := "app did not become ready: it stopped\n    Last lines of its log:\n    one\n      two\nnot indented"
	if got := failedStartOutput(msg); strings.Join(got, "|") != "one|  two" {
		t.Errorf("tail = %q", got)
	}
	if got := failedStartOutput("app did not become ready: timed out"); got != nil {
		t.Errorf("a message with no tail gave %q", got)
	}
}

// BB-038 (fourth 2026-11-03 black-box list): a failed start whose output the
// deployment kept line by line reads back with each line's own time and
// stream — a print to stdout is stdout — rather than the failure's one stamp
// and stderr on every line.
func TestAFailedStartsKeptOutputKeepsEachLinesTimeAndStream(t *testing.T) {
	e := newLogTestEnv(t)
	if err := e.srv.rts.Shared().Delete(e.app.ProcessName()); err != nil {
		t.Fatal(err)
	}
	failed := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusFailed}
	if err := e.srv.db.CreateDeployment(failed, nil); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 11, 3, 15, 18, 10, 0, time.UTC)
	second := first.Add(time.Second)
	if err := e.srv.db.SetDeploymentFailure(failed.ID, "not_ready", "",
		"app did not become ready: it stopped\n    Last lines of its log:\n    about to crash\n    boom"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]map[string]any{
		{"at": first, "stream": "stdout", "line": "about to crash"},
		{"at": second, "stream": "stderr", "line": "boom"},
	})
	if err := e.srv.db.SetDeploymentFailureOutput(failed.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	rec := e.as(t, "owner", "/api/v1/apps/"+e.app.ID+"/logs?source=app&lines=50")
	var body struct {
		Lines []runtime.LogLine `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	seen := map[string]runtime.LogLine{}
	for _, l := range body.Lines {
		seen[l.Line] = l
	}
	crash, boom := seen["about to crash"], seen["boom"]
	if crash.Stream != "stdout" || crash.At == nil || !crash.At.Equal(first) {
		t.Errorf("stdout line = %+v", crash)
	}
	if boom.Stream != "stderr" || boom.At == nil || !boom.At.Equal(second) {
		t.Errorf("stderr line = %+v", boom)
	}
}

// A build-log socket opened on a deploy that is already over ends: it sends the
// stored log and the terminator and returns, where it used to wait on a channel
// nothing would ever write to again — every console watching a failed build
// held its handler for the life of the process (code review 2026-11-11, F-9).
func TestTheDeployLogSocketEndsOnAFinishedDeploy(t *testing.T) {
	srv := newTestServer(t)
	rts := runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	srv.rts = rts
	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatal(err)
	}
	srv.builder = builder.New(srv.db, rts, enc, gitconn.NewResolver(srv.db, enc), nil, builder.Options{AppsDir: t.TempDir()})
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "over", Name: "Over", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	deploy := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusFailed,
		StartedAt: &now, FinishedAt: &now, BuildLog: "==> Cloning\nERROR: it broke\n"}
	if err := srv.db.CreateDeployment(deploy, &database.DeployRecord{ActorType: "user", ActorID: "owner", Via: "upload"}); err != nil {
		t.Fatal(err)
	}
	const platformOrigin = "http://platform.test"
	srv.cfg.BaseURL = platformOrigin
	ts := httptest.NewServer(srv.Router(http.NotFoundHandler()))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/apps/" + app.ID + "/deployments/" + deploy.ID + "/stream"
	cookie := sessionCookieFor(t, srv, "owner")
	h := http.Header{}
	h.Set("Cookie", cookie.Name+"="+cookie.Value)
	h.Set("Origin", platformOrigin)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []string
	var readErr error
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		got = append(got, string(msg))
		if string(msg) == "DONE" {
			break
		}
	}
	if len(got) == 0 || got[len(got)-1] != "DONE" {
		t.Fatalf("messages = %q (read error %v), want the stored log then DONE", got, readErr)
	}
	if !strings.Contains(strings.Join(got, ""), "it broke") {
		t.Errorf("the stored log was not sent first: %q", got)
	}
}
