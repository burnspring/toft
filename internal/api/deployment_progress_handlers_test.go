package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deploysteps"
	"toft/internal/gitconn"
	"toft/internal/runtime"
)

// progressEnv is a server with a *real* builder, which the shared test server
// deliberately has not got. Both routes under test reach for one — the stream
// subscribes to it — and a nil builder would be a panic rather than a test.
type progressEnv struct {
	srv      *Server
	router   http.Handler
	builder  *builder.Builder
	app      *database.App // owned by "owner", private
	otherApp *database.App // owned by "stranger", private
	live     *database.Deployment
	done     *database.Deployment
	foreign  *database.Deployment // belongs to otherApp
}

func newProgressEnv(t *testing.T) *progressEnv {
	t.Helper()

	db := dbtest.New(t)
	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	rts := runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)
	b := builder.New(db, rts, enc, gitconn.NewResolver(db, enc), nil, builder.Options{AppsDir: t.TempDir()})
	srv := NewServer(&config.Config{BaseURL: "http://localhost:8080"}, db,
		auth.NewJWTService("test-secret-key"), rts, b, enc)
	srv.cfg.AppsDir = t.TempDir()

	seedUser(t, db, "owner", database.SystemRolePublisher)
	seedUser(t, db, "stranger", database.SystemRolePublisher)

	app := &database.App{
		Slug: "watched", Name: "Watched", OwnerID: "owner",
		RepoURL: "https://github.com/example/watched", Status: database.AppStatusBuilding,
		Visibility: database.VisibilityPrivate,
	}
	otherApp := &database.App{
		Slug: "unwatched", Name: "Unwatched", OwnerID: "stranger",
		RepoURL: "https://github.com/example/unwatched", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	for _, a := range []*database.App{app, otherApp} {
		if err := db.CreateApp(a); err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
	}

	mkDeploy := func(appID, status, step string) *database.Deployment {
		d := &database.Deployment{AppID: appID, Status: status, Branch: "main"}
		if err := db.CreateDeployment(d, nil); err != nil {
			t.Fatalf("create deployment: %v", err)
		}
		if step != "" {
			if err := db.SetDeploymentStep(d.ID, step); err != nil {
				t.Fatalf("set step: %v", err)
			}
		}
		return d
	}

	return &progressEnv{
		srv: srv, router: srv.Router(http.NotFoundHandler()), builder: b,
		app: app, otherApp: otherApp,
		live:    mkDeploy(app.ID, database.DeployStatusBuilding, string(deploysteps.Scan)),
		done:    mkDeploy(app.ID, database.DeployStatusFailed, string(deploysteps.Build)),
		foreign: mkDeploy(otherApp.ID, database.DeployStatusBuilding, string(deploysteps.Fetch)),
	}
}

func (e *progressEnv) get(t *testing.T, who, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func deployPath(appID, deployID string) string {
	return fmt.Sprintf("/api/v1/apps/%s/deployments/%s", appID, deployID)
}

// The phase is derived from the step wherever a deployment is read, so a client
// never has to carry the step → phase table to know which stage a build is in.
func TestGetDeploymentReportsItsPhase(t *testing.T) {
	e := newProgressEnv(t)

	rec := e.get(t, "owner", deployPath(e.app.ID, e.live.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var got struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Phase       string `json:"phase"`
		CurrentStep string `json:"current_step"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != e.live.ID {
		t.Errorf("id = %q, want %q", got.ID, e.live.ID)
	}
	if got.CurrentStep != "scan" || got.Phase != "scan" {
		t.Errorf("step %q reported in phase %q, want the scan step in the scan phase", got.CurrentStep, got.Phase)
	}
}

// The same guards the rest of the deployment routes keep: a caller who cannot
// read the app is told it does not exist, and a deployment ID is only ever
// resolved against the app in the URL it arrived on.
func TestDeploymentProgressGuards(t *testing.T) {
	e := newProgressEnv(t)
	rungOff(t, e.srv)

	cases := []struct {
		name string
		who  string
		path string
		want int
	}{
		{"anonymous is rejected", "", deployPath(e.app.ID, e.live.ID), http.StatusUnauthorized},
		{"stranger cannot see the app", "stranger", deployPath(e.app.ID, e.live.ID), http.StatusNotFound},
		{"another app's deployment is not found", "owner", deployPath(e.app.ID, e.foreign.ID), http.StatusNotFound},
		{"anonymous cannot stream", "", deployPath(e.app.ID, e.live.ID) + "/progress", http.StatusUnauthorized},
		{"stranger cannot stream", "stranger", deployPath(e.app.ID, e.live.ID) + "/progress", http.StatusNotFound},
		{"another app's stream is not found", "owner", deployPath(e.app.ID, e.foreign.ID) + "/progress", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := e.get(t, tc.who, tc.path); rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// A deploy that was already over when the watcher arrived still gets a frame
// and then the end of the stream. This is the reconnecting-CLI case, and a
// stream that just hung there would be the worst possible answer to it.
func TestProgressStreamOfAFinishedDeployEndsImmediately(t *testing.T) {
	e := newProgressEnv(t)

	rec := e.get(t, "owner", deployPath(e.app.ID, e.done.ID)+"/progress")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q, want text/event-stream", ct)
	}
	frames := progressFrames(t, rec.Body.String())
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want the single terminal one: %s", len(frames), rec.Body.String())
	}
	if frames[0].Status != database.DeployStatusFailed || frames[0].Phase != "deploy" {
		t.Errorf("frame = %+v, want a failed deploy-phase snapshot", frames[0])
	}
}

// The live case, over a real socket rather than a recorder: frames have to
// arrive as they happen, which is the whole point and is exactly what a
// buffered response would hide. A watcher receives its snapshot, then every
// step change, then a terminal frame that ends the stream with nobody closing
// the connection.
func TestProgressStreamFollowsADeployToItsEnd(t *testing.T) {
	e := newProgressEnv(t)

	ts := httptest.NewServer(e.router)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+deployPath(e.app.ID, e.live.ID)+"/progress", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.AddCookie(sessionCookieFor(t, e.srv, "owner"))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	frames := newFrameReader(t, resp.Body)

	// Receiving the snapshot is also the proof that the watcher is subscribed:
	// the handler subscribes first, and a frame published before that would be
	// dropped rather than queued.
	snapshot := frames.next()
	if snapshot.Step != "scan" || snapshot.Phase != "scan" {
		t.Errorf("snapshot = %+v, want where the deploy already was", snapshot)
	}

	e.builder.PublishProgress(e.live.ID, database.DeployStatusBuilding, deploysteps.Build, "")
	if got := frames.next(); got.Phase != "deploy" || got.Status != database.DeployStatusBuilding {
		t.Errorf("frame = %+v, want the build step, still running", got)
	}

	e.builder.PublishProgress(e.live.ID, database.DeployStatusSuccess, deploysteps.Flip, "")
	last := frames.next()
	if !last.Terminal() {
		t.Fatalf("frame = %+v, want a terminal one", last)
	}

	// The server ends the stream itself once it has nothing left to say — the
	// terminal frame is the client's signal, and nothing follows it but EOF.
	for {
		line, err := frames.reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading to the end of the stream: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			t.Fatalf("the stream kept talking after its terminal frame: %s", line)
		}
	}
}

// frameReader pulls one SSE data frame at a time off a live stream, and fails
// the test rather than hanging if one does not arrive.
type frameReader struct {
	t      *testing.T
	reader *bufio.Reader
}

func newFrameReader(t *testing.T, body io.Reader) *frameReader {
	return &frameReader{t: t, reader: bufio.NewReader(body)}
}

func (f *frameReader) next() builder.ProgressEvent {
	f.t.Helper()
	for {
		line, err := f.reader.ReadString('\n')
		if err != nil {
			f.t.Fatalf("reading the stream: %v", err)
		}
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "data: ")
		if !ok {
			continue // the event: line, a blank separator, or a heartbeat
		}
		var e builder.ProgressEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			f.t.Fatalf("decode frame %q: %v", data, err)
		}
		return e
	}
}

func progressFrames(t *testing.T, body string) []builder.ProgressEvent {
	t.Helper()
	var out []builder.ProgressEvent
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var e builder.ProgressEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatalf("decode frame %q: %v", data, err)
		}
		out = append(out, e)
	}
	return out
}
