package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
)

func deletePath(appID, deployID string) string {
	return fmt.Sprintf("/api/v1/apps/%s/deployments/%s", appID, deployID)
}

func bulkDeletePath(appID string) string {
	return fmt.Sprintf("/api/v1/apps/%s/deployments", appID)
}

func (e *rollbackEnv) delete(t *testing.T, who, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// makeCurrent points the app at a deployment the way a finished build does:
// the process name records which row deployed, the release reference records
// which release is executing. A rollback is the case where they disagree.
func (e *rollbackEnv) makeCurrent(t *testing.T, rowID, releaseID string) {
	t.Helper()

	e.app.CurrentRelease = builder.ReleaseProcessName(e.app.Slug, rowID)
	e.app.ReleaseRef = "local:" + builder.ReleaseDir(e.srv.cfg.AppsDir, e.app.ID, releaseID)
	if err := e.srv.db.UpdateApp(e.app); err != nil {
		t.Fatalf("update app: %v", err)
	}
}

func TestDeleteDeploymentGuards(t *testing.T) {
	e := newRollbackEnv(t)
	rungOff(t, e.srv)
	e.makeCurrent(t, e.goodDeploy.ID, e.goodDeploy.ID)

	building := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusBuilding, Branch: "upload"}
	if err := e.srv.db.CreateDeployment(building, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	cases := []struct {
		name string
		who  string
		path string
		want int
	}{
		{"anonymous is rejected", "", deletePath(e.app.ID, e.failedDeploy.ID), http.StatusUnauthorized},
		// 404, not 403: the app is private and unreadable to the caller.
		{"stranger cannot see the app", "stranger", deletePath(e.app.ID, e.failedDeploy.ID), http.StatusNotFound},
		// Authorization was evaluated against the app in the path, so the
		// deployment has to belong to it — otherwise this is an IDOR.
		{"deployment of another app is not found", "owner", deletePath(e.app.ID, e.otherDeploy.ID), http.StatusNotFound},
		{"unknown deployment is not found", "owner", deletePath(e.app.ID, "no-such-deployment"), http.StatusNotFound},
		// Deleting the release out from under the app that is serving it.
		{"current build is protected", "owner", deletePath(e.app.ID, e.goodDeploy.ID), http.StatusConflict},
		// Its release directory is being written right now.
		{"in-flight build is protected", "owner", deletePath(e.app.ID, building.ID), http.StatusConflict},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.delete(t, tc.who, tc.path)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// A rollback leaves the app serving the source deployment's release while a
// newer row records the deploy. Both rows are protected: deleting the source
// would take the running code, deleting the rollback row would take the record
// of what is running.
func TestDeleteDeploymentProtectsBothHalvesOfARollback(t *testing.T) {
	e := newRollbackEnv(t)

	rollbackRow := &database.Deployment{
		AppID: e.app.ID, Status: database.DeployStatusSuccess,
		Branch: "upload", TriggeredBy: database.DeployTriggerRollback,
	}
	if err := e.srv.db.CreateDeployment(rollbackRow, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	// The rollback row is current; the release underneath it is goodDeploy's.
	e.makeCurrent(t, rollbackRow.ID, e.goodDeploy.ID)

	for _, tc := range []struct{ name, deployID string }{
		{"the row that records the deploy", rollbackRow.ID},
		{"the release actually being served", e.goodDeploy.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.delete(t, "owner", deletePath(e.app.ID, tc.deployID))
			if rec.Code != http.StatusConflict {
				t.Errorf("status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// The happy path: the row goes, and so does the release directory it owned.
func TestDeleteDeploymentRemovesTheReleaseAndTheRow(t *testing.T) {
	e := newRollbackEnv(t)
	e.makeCurrent(t, e.goodDeploy.ID, e.goodDeploy.ID)

	dir := builder.ReleaseDir(e.srv.cfg.AppsDir, e.app.ID, e.failedDeploy.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	rec := e.delete(t, "owner", deletePath(e.app.ID, e.failedDeploy.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body deleteReleasesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", body.Deleted)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("release directory survived deletion (err=%v)", err)
	}
	gone, err := e.srv.db.GetDeployment(e.failedDeploy.ID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if gone != nil {
		t.Error("deployment row survived deletion")
	}
}

// The bulk form clears everything the single form would allow, and nothing it
// would refuse.
func TestDeleteNonCurrentDeploymentsSparesTheLiveBuild(t *testing.T) {
	e := newRollbackEnv(t)
	e.makeCurrent(t, e.goodDeploy.ID, e.goodDeploy.ID)

	rec := e.delete(t, "owner", bulkDeletePath(e.app.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	remaining, err := e.srv.db.ListDeployments(e.app.ID)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != e.goodDeploy.ID {
		t.Fatalf("remaining deployments = %+v, want only the current build", remaining)
	}

	// The other app's history is not this app's to sweep.
	others, err := e.srv.db.ListDeployments(e.otherApp.ID)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(others) != 1 {
		t.Errorf("another app's deployments = %d, want 1", len(others))
	}
}

// The listing is where a client learns which build is current, which one is
// being served, and what may be deleted — because after a rollback it cannot
// work any of that out from the rows alone.
func TestListDeploymentsMarksTheLiveBuild(t *testing.T) {
	e := newRollbackEnv(t)

	rollbackRow := &database.Deployment{
		AppID: e.app.ID, Status: database.DeployStatusSuccess,
		Branch: "upload", TriggeredBy: database.DeployTriggerRollback,
	}
	if err := e.srv.db.CreateDeployment(rollbackRow, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	e.makeCurrent(t, rollbackRow.ID, e.goodDeploy.ID)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/apps/%s/deployments", e.app.ID), nil)
	req.AddCookie(sessionCookieFor(t, e.srv, "owner"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var views []deploymentView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	byID := map[string]deploymentView{}
	for _, v := range views {
		byID[v.ID] = v
	}

	if v := byID[rollbackRow.ID]; !v.Current || v.Serving || v.ProtectedReason == "" {
		t.Errorf("rollback row: current=%v serving=%v protected=%q — want current, not serving, protected",
			v.Current, v.Serving, v.ProtectedReason)
	}
	if v := byID[e.goodDeploy.ID]; v.Current || !v.Serving || v.ProtectedReason == "" {
		t.Errorf("source row: current=%v serving=%v protected=%q — want serving, not current, protected",
			v.Current, v.Serving, v.ProtectedReason)
	}
	if v := byID[e.failedDeploy.ID]; v.ProtectedReason != "" {
		t.Errorf("failed build reported protected: %q", v.ProtectedReason)
	}
}

// A rollback that is queued protects the release it is returning to. The
// source rode only on the in-memory build job, so "Delete old releases"
// pressed between the 202 and the worker removed it, and the rollback failed
// with "that release no longer exists". The pending row carries the source
// now and the deletion reads it.
func TestDeleteDeploymentProtectsAQueuedRollbacksSource(t *testing.T) {
	e := newRollbackEnv(t)
	e.makeCurrent(t, e.goodDeploy.ID, e.goodDeploy.ID)

	// An older release to return to, and a rollback to it that is still
	// waiting for the worker.
	older := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusSuccess, Branch: "main"}
	if err := e.srv.db.CreateDeployment(older, nil); err != nil {
		t.Fatal(err)
	}
	queued := &database.Deployment{
		AppID: e.app.ID, Status: database.DeployStatusPending, Branch: "main",
		TriggeredBy: database.DeployTriggerRollback, SourceDeployID: older.ID,
	}
	if err := e.srv.db.CreateDeployment(queued, nil); err != nil {
		t.Fatal(err)
	}

	rec := e.delete(t, "owner", deletePath(e.app.ID, older.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting the source of a queued rollback = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	still, err := e.srv.db.GetDeployment(older.ID)
	if err != nil || still == nil {
		t.Fatalf("the release a rollback is returning to was deleted (err=%v)", err)
	}
}

// A queued job retry waits with its first attempt's release. Pruning it failed
// every remaining attempt as infrastructure_error before any code ran (trytoft
// drills rerun #1), so a release an unfinished run is pinned to is protected —
// by the single delete and by the sweep alike.
func TestDeleteDeploymentProtectsAReleaseAJobRunIsPinnedTo(t *testing.T) {
	e := newRollbackEnv(t)
	e.makeCurrent(t, e.goodDeploy.ID, e.goodDeploy.ID)

	older := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusSuccess, Branch: "main"}
	if err := e.srv.db.CreateDeployment(older, nil); err != nil {
		t.Fatal(err)
	}
	run := &database.JobRun{AppID: e.app.ID, JobName: "refresh",
		ReleaseRef: "artifact:releases/" + e.app.ID + "/" + older.ID + ".tar.gz"}
	if err := e.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	rec := e.delete(t, "owner", deletePath(e.app.ID, older.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a release a queued run is pinned to = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if rec := e.delete(t, "owner", bulkDeletePath(e.app.ID)); rec.Code != http.StatusOK {
		t.Fatalf("sweep = %d: %s", rec.Code, rec.Body.String())
	}
	if still, err := e.srv.db.GetDeployment(older.ID); err != nil || still == nil {
		t.Fatalf("the sweep deleted the release a queued run is pinned to (err=%v)", err)
	}
}

// The deletion's audit row names the principal as what it is: a deploy key is
// recorded as the key, not as a user whose id is the key's.
func TestReleaseDeletionAuditNamesTheKey(t *testing.T) {
	e := newRollbackEnv(t)
	p := &auth.Principal{ID: "key-1", DeployKeyID: "key-1"}
	r := httptest.NewRequest(http.MethodDelete, deletePath(e.app.ID, e.failedDeploy.ID), nil)
	r = r.WithContext(auth.WithPrincipal(context.Background(), p))
	e.srv.auditRelease(r, e.app.ID, e.failedDeploy, 0, 0)

	var actorType, actorID string
	if err := e.srv.db.Conn().QueryRow(`
		SELECT actor_type, COALESCE(actor_id,'') FROM audit_log
		WHERE target_id = ? AND action = 'app.release_deleted'`, e.app.ID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if actorType != auth.PrincipalTypeDeployKey || actorID != "key-1" {
		t.Fatalf("audit actor = (%s, %s), want (deploy_key, key-1)", actorType, actorID)
	}
}

// D8: the refusal and the prune say what protects a job-pinned release and what
// frees it. The refusal said "deploy or promote another build first", which does
// nothing for a release a run is pinned to; the prune said only how many it
// deleted, so the survivor read as the serving release alone.
func TestAJobPinnedReleaseIsNamedWithItsOwnRemedy(t *testing.T) {
	e := newRollbackEnv(t)
	e.makeCurrent(t, e.goodDeploy.ID, e.goodDeploy.ID)

	older := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusSuccess, Branch: "main"}
	if err := e.srv.db.CreateDeployment(older, nil); err != nil {
		t.Fatal(err)
	}
	run := &database.JobRun{AppID: e.app.ID, JobName: "refresh",
		ReleaseRef: "artifact:releases/" + e.app.ID + "/" + older.ID + ".tar.gz"}
	if err := e.srv.db.EnqueueJobRun(run, 0); err != nil {
		t.Fatal(err)
	}

	rec := e.delete(t, "owner", deletePath(e.app.ID, older.ID))
	var refused struct {
		Error, Hint string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil || rec.Code != http.StatusConflict {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	for field, text := range map[string]string{"error": refused.Error, "hint": refused.Hint} {
		if strings.Contains(text, "deploy or promote") || !strings.Contains(text, "toft job wait") {
			t.Errorf("%s = %q, want the run's remedy, not a deploy's", field, text)
		}
	}

	rec = e.delete(t, "owner", bulkDeletePath(e.app.ID))
	var pruned deleteReleasesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pruned); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("prune = %d: %s", rec.Code, rec.Body.String())
	}
	kept := map[string]keptRelease{}
	for _, k := range pruned.Kept {
		kept[k.ID] = k
	}
	if k := kept[older.ID]; !strings.Contains(k.Reason, "job run") || !strings.Contains(k.Remedy, "toft job wait") {
		t.Errorf("kept[%s] = %+v, want the job-pinned reason and its remedy (all kept: %+v)", older.ID, k, pruned.Kept)
	}
	if k := kept[e.goodDeploy.ID]; !strings.Contains(k.Reason, "serving") {
		t.Errorf("kept[%s] = %+v, want the serving release named", e.goodDeploy.ID, k)
	}
}
