package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/deploysteps"
)

// uploadRequest is a multipart POST of one file, as the browser and the CLI
// both send it.
func uploadRequest(t *testing.T, path, filename string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	form.Close()

	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req
}

// zipBytes is the smallest archive that extracts to something.
func zipBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("index.js")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := f.Write([]byte("console.log('hi')\n")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func (e *rollbackEnv) upload(t *testing.T, who string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// The deploy exists before the bytes do, so an upload that is refused after the
// row is created is a failed deployment with a reason on it — not a bare 400
// that leaves the app looking like nothing was ever attempted.
func TestRejectedUploadIsAFailedDeployment(t *testing.T) {
	e := newRollbackEnv(t)
	before, err := e.srv.db.ListDeployments(e.app.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.app.ID)
	rec := e.upload(t, "owner", uploadRequest(t, path, "notes.txt", []byte("not an archive")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}

	after, err := e.srv.db.ListDeployments(e.app.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("deployments went from %d to %d, want one more", len(before), len(after))
	}
	d := after[0]
	if d.Status != database.DeployStatusFailed {
		t.Errorf("status = %q, want failed", d.Status)
	}
	// It died deciding what it had been sent, which is the receive step — the
	// unpack never began.
	if d.CurrentStep != "receive" || d.Phase != "upload" {
		t.Errorf("failed at step %q in phase %q, want receive/upload", d.CurrentStep, d.Phase)
	}
	if d.FinishedAt == nil {
		t.Error("a failed upload has finished; the row should say when")
	}
	if d.BuildLog == "" {
		t.Error("the row is the only account of why this failed, and it is empty")
	}
	// Both halves of why. An empty reason is not "no reason" — every surface is
	// required to read it as *unclassified*, meaning a row written before the
	// column existed, so a failure that leaves it empty is indistinguishable
	// from one recorded by a much older platform.
	if d.FailureReason != string(deploysteps.ReasonSourceUnavailable) {
		t.Errorf("failure_reason = %q, want %q", d.FailureReason,
			deploysteps.ReasonSourceUnavailable)
	}
	if d.FailureMessage == "" {
		t.Error("failure_message is empty; the key is what a program branches " +
			"on and this is what a person reads")
	}
}

// The other step this phase can die at. `receive` is deciding what was sent;
// `unpack` is the archive refusing to expand — and the two are the only steps
// with a reasonOf entry that nothing outside this file can reach.
func TestFailedUnpackRecordsWhy(t *testing.T) {
	e := newRollbackEnv(t)

	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.app.ID)
	// A name that says zip so the suffix check passes, and bytes that are not
	// one so the extract is what fails.
	rec := e.upload(t, "owner", uploadRequest(t, path, "app.zip", []byte("PK not really")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}

	after, err := e.srv.db.ListDeployments(e.app.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) == 0 {
		t.Fatal("an archive that will not unpack still got as far as being a deploy")
	}
	d := after[0]
	if d.CurrentStep != "unpack" {
		t.Errorf("failed at step %q, want unpack", d.CurrentStep)
	}
	if d.FailureReason != string(deploysteps.ReasonSourceUnavailable) {
		t.Errorf("failure_reason = %q, want %q", d.FailureReason,
			deploysteps.ReasonSourceUnavailable)
	}
	if d.FailureMessage == "" {
		t.Error("failure_message is empty")
	}
}

// The reason on the row and the reason in the terminal frame are the same
// answer, because the row is written first and the frame is assembled from it.
// A watcher that reconnects the instant the upload ends must not be told
// something the row will then contradict.
func TestUploadFailureFrameAgreesWithTheRow(t *testing.T) {
	e := newRollbackEnv(t)

	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.app.ID)
	rec := e.upload(t, "owner", uploadRequest(t, path, "notes.txt", []byte("not an archive")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	after, err := e.srv.db.ListDeployments(e.app.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) == 0 {
		t.Fatal("no deployment row")
	}
	d := after[0]

	frame := builder.SnapshotProgress(&d)
	if frame.Reason != d.FailureReason {
		t.Errorf("the snapshot says reason %q and the row says %q", frame.Reason,
			d.FailureReason)
	}
	if frame.Message != d.FailureMessage {
		t.Errorf("the snapshot says message %q and the row says %q", frame.Message,
			d.FailureMessage)
	}
	if frame.Status != database.DeployStatusFailed {
		t.Errorf("snapshot status = %q, want failed", frame.Status)
	}
}

// A refusal that happens before the row — this install has stopped taking
// uploads — still leaves no trace. A deploy that never started is not a deploy
// that failed.
func TestRefusedUploadLeavesNoDeployment(t *testing.T) {
	e := newRollbackEnv(t)
	uploadsOff(t, e.srv)
	before, err := e.srv.db.ListDeployments(e.otherApp.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// otherApp deploys from git, so refusing the upload strands nothing and the
	// switch is in the path.
	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.otherApp.ID)
	rec := e.upload(t, "stranger", uploadRequest(t, path, "app.zip", zipBytes(t)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}

	after, err := e.srv.db.ListDeployments(e.otherApp.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused upload wrote %d new rows, want none", len(after)-len(before))
	}
}

// uploadsOff is the install that has stopped accepting code as an archive.
func uploadsOff(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.db.SetSourcePolicy("any", false, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("switching uploads off: %v", err)
	}
}

// A laptop pushing at an app whose source is a repository. It is accepted — the
// point of `toft deploy` — and the app is not converted by it: repo_url stands,
// auto-deploy stands, and the deployment says `upload` rather than a branch so
// the divergence is legible everywhere a branch is shown.
func TestUploadToAGitBackedApp(t *testing.T) {
	e := newRollbackEnv(t)

	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.otherApp.ID)
	rec := e.upload(t, "stranger", uploadRequest(t, path, "app.zip", zipBytes(t)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}

	var answered database.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answered.Branch != "upload" {
		t.Errorf("branch = %q, want upload — the row is what makes the divergence visible", answered.Branch)
	}

	app, err := e.srv.db.GetApp(e.otherApp.ID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if app.RepoURL != e.otherApp.RepoURL {
		t.Errorf("repo_url = %q, want it untouched (%q) — an upload is a deployment, not a conversion",
			app.RepoURL, e.otherApp.RepoURL)
	}
}

// A redeploy whose toft.toml the build would refuse is refused at the upload,
// with the code toft check gives, and the row records it as manifest_invalid —
// before, it was queued, scanned and detected, then failed manifest_invalid
// with no code (jobs-regression #15).
func TestARedeployWithARemovedKeyIsRefusedAtTheUpload(t *testing.T) {
	e := newRollbackEnv(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"index.js":  "console.log('hi')\n",
		"toft.toml": "[jobs.work]\ncommand = \"node index.js\"\nretries = 2\n",
	} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.otherApp.ID)
	rec := e.upload(t, "stranger", uploadRequest(t, path, "app.zip", buf.Bytes()))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "removed_key" || body["hint"] == nil || body["deployment_id"] == nil || body["reason"] != "manifest_invalid" {
		t.Fatalf("body = %s, want removed_key with its hint, the deployment and reason manifest_invalid", rec.Body.String())
	}
	d, err := e.srv.db.GetDeployment(body["deployment_id"].(string))
	if err != nil || d == nil || d.Status != database.DeployStatusFailed || d.FailureReason != "manifest_invalid" {
		t.Errorf("deployment = %+v, %v; want failed, manifest_invalid", d, err)
	}
	// The code rides the row beside the reason, as check names it.
	if d != nil && d.FailureCode != "removed_key" {
		t.Errorf("failure_code = %q, want removed_key", d.FailureCode)
	}
}

// A redeploy declaring a job over the install's ceiling is refused at the
// upload with limit_exceeds_ceiling, the code toft check names, rather than
// queued for a build that fails it at detect as manifest_invalid (docs/bugs
// 2026-10-28 D6).
func TestARedeployOverTheJobCeilingIsRefusedAtTheUpload(t *testing.T) {
	e := newRollbackEnv(t)
	settings, err := e.srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.MaxJobTimeoutSecs = 900
	if err := e.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"index.js":  "console.log('hi')\n",
		"toft.toml": "[jobs.work]\ncommand = \"node index.js\"\ntimeout = \"50m\"\n",
	} {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.otherApp.ID)
	rec := e.upload(t, "stranger", uploadRequest(t, path, "app.zip", buf.Bytes()))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "limit_exceeds_ceiling" || body["hint"] == nil {
		t.Fatalf("body = %s, want limit_exceeds_ceiling with its hint", rec.Body.String())
	}
}

// The other half of C3's rule: an app created from an archive has no other way
// to ship a fix to its own running code, so the switch is not consulted on its
// version bumps. A settings change never takes down running code.
func TestUploadsOffStillLetsAnUploadedAppRedeploy(t *testing.T) {
	e := newRollbackEnv(t)
	uploadsOff(t, e.srv)

	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.app.ID)
	rec := e.upload(t, "owner", uploadRequest(t, path, "app.zip", zipBytes(t)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
}

// The accepted case: both upload steps are recorded, with their timings, and
// the row is handed to the queue sitting in the phase the builder continues
// from.
func TestAcceptedUploadRecordsItsSteps(t *testing.T) {
	e := newRollbackEnv(t)

	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.app.ID)
	rec := e.upload(t, "owner", uploadRequest(t, path, "app.zip", zipBytes(t)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}

	// The answer describes the deploy the caller just started, steps included.
	// A CLI reads this before it can subscribe to anything, and the same struct
	// is what goes to the build queue — a StepLog that wrote only to the
	// database would leave both of them describing a deploy that had not begun,
	// and the build worker would then write over the upload's timings instead
	// of appending to them.
	var answered database.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answered.CurrentStep != "unpack" || answered.Phase != "upload" {
		t.Errorf("answered with step %q in phase %q, want unpack/upload", answered.CurrentStep, answered.Phase)
	}
	if !strings.Contains(answered.StepTimings, `"step":"receive"`) {
		t.Errorf("the answer's timings are %q, which the build would write over", answered.StepTimings)
	}

	deployments, err := e.srv.db.ListDeployments(e.app.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	d := deployments[0]
	if d.Status != database.DeployStatusPending {
		t.Errorf("status = %q, want pending — the build has not run", d.Status)
	}
	if d.CurrentStep != "unpack" || d.Phase != "upload" {
		t.Errorf("handed over at step %q in phase %q, want unpack/upload", d.CurrentStep, d.Phase)
	}
	// Both steps are closed and timed, so the builder's first close appends to
	// them instead of writing over them.
	for _, step := range []string{`"step":"receive"`, `"step":"unpack"`} {
		if !bytes.Contains([]byte(d.StepTimings), []byte(step)) {
			t.Errorf("timings %q do not record %s", d.StepTimings, step)
		}
	}
}

// The route takes both containers, which is the half of the change a unit test
// of the unpacker cannot see: the suffix check happens before the body is read,
// and a name the platform does not know is refused there.
func TestUploadRouteTakesATarball(t *testing.T) {
	e := newRollbackEnv(t)
	path := fmt.Sprintf("/api/v1/apps/%s/upload", e.app.ID)

	tgz := tarGzOf(t, []tarEntry{{name: "index.js", body: "console.log('hi')\n"}})
	raw := make([]byte, tgz.Size())
	if _, err := tgz.ReadAt(raw, 0); err != nil {
		t.Fatalf("read tarball: %v", err)
	}

	rec := e.upload(t, "owner", uploadRequest(t, path, "app.tar.gz", raw))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}

	// And an extension neither reader knows is refused, in the words both
	// routes share.
	e2 := newRollbackEnv(t)
	rec = e2.upload(t, "owner", uploadRequest(t, fmt.Sprintf("/api/v1/apps/%s/upload", e2.app.ID),
		"app.rar", raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ".tar.gz") {
		t.Errorf("the refusal does not name what is accepted: %s", rec.Body.String())
	}
}
