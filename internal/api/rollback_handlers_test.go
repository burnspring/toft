package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// rollbackEnv is a server with two private apps owned by different users, each
// with a successful deployment. The rollback and re-upload guards must hold
// before anything reaches the build queue — newTestServer wires no builder, so
// any test that got past them would hang, not pass.
type rollbackEnv struct {
	srv          *Server
	router       http.Handler
	app          *database.App // owned by "owner"
	otherApp     *database.App // owned by "stranger"
	goodDeploy   *database.Deployment
	failedDeploy *database.Deployment
	otherDeploy  *database.Deployment // successful, but belongs to otherApp
}

func newRollbackEnv(t *testing.T) *rollbackEnv {
	t.Helper()

	srv := newTestServer(t)
	srv.cfg.AppsDir = t.TempDir() // no release dirs exist on disk

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	app := &database.App{
		Slug: "rollme", Name: "Roll Me", OwnerID: "owner",
		RepoURL: "upload://rollme.zip", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	otherApp := &database.App{
		Slug: "other", Name: "Other", OwnerID: "stranger",
		RepoURL: "https://github.com/example/other", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	for _, a := range []*database.App{app, otherApp} {
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
	}

	mkDeploy := func(appID, status string) *database.Deployment {
		d := &database.Deployment{AppID: appID, Status: status, Branch: "upload"}
		if err := srv.db.CreateDeployment(d, nil); err != nil {
			t.Fatalf("create deployment: %v", err)
		}
		return d
	}

	return &rollbackEnv{
		srv:          srv,
		router:       srv.Router(http.NotFoundHandler()),
		app:          app,
		otherApp:     otherApp,
		goodDeploy:   mkDeploy(app.ID, database.DeployStatusSuccess),
		failedDeploy: mkDeploy(app.ID, database.DeployStatusFailed),
		otherDeploy:  mkDeploy(otherApp.ID, database.DeployStatusSuccess),
	}
}

func (e *rollbackEnv) post(t *testing.T, who, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func rollbackPath(appID, deployID string) string {
	return fmt.Sprintf("/api/v1/apps/%s/deployments/%s/rollback", appID, deployID)
}

func TestRollbackGuards(t *testing.T) {
	e := newRollbackEnv(t)
	rungOff(t, e.srv)

	cases := []struct {
		name string
		who  string
		path string
		want int
	}{
		{"anonymous is rejected", "", rollbackPath(e.app.ID, e.goodDeploy.ID), http.StatusUnauthorized},
		// 404, not 403: the app is private and unreadable to the caller.
		{"stranger cannot see the app", "stranger", rollbackPath(e.app.ID, e.goodDeploy.ID), http.StatusNotFound},
		// The deployment ID must be pinned to the app in the URL — authorizing
		// against one app and rolling back another's deployment is an IDOR.
		{"deployment of another app is not found", "owner", rollbackPath(e.app.ID, e.otherDeploy.ID), http.StatusNotFound},
		{"failed deployment cannot be made current", "owner", rollbackPath(e.app.ID, e.failedDeploy.ID), http.StatusConflict},
		// The deployment row exists but its release dir is gone from disk.
		{"pruned release is gone", "owner", rollbackPath(e.app.ID, e.goodDeploy.ID), http.StatusGone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.post(t, tc.who, tc.path)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestUploadVersionGuards(t *testing.T) {
	e := newRollbackEnv(t)

	t.Run("anonymous is rejected", func(t *testing.T) {
		rec := e.post(t, "", fmt.Sprintf("/api/v1/apps/%s/upload", e.app.ID))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	// A git-sourced app **accepts** an upload now (ADR §42 D1) — this asserts
	// that it gets as far as reading the body and refuses *that*, rather than
	// refusing the app. The two are told apart by the row: a bad body is a
	// failed deploy with a reason on it, and the old refusal left nothing.
	t.Run("a git-sourced app is refused for its body, not for its source", func(t *testing.T) {
		before, err := e.srv.db.ListDeployments(e.otherApp.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		rec := e.post(t, "stranger", fmt.Sprintf("/api/v1/apps/%s/upload", e.otherApp.ID))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
		after, err := e.srv.db.ListDeployments(e.otherApp.ID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(after) != len(before)+1 {
			t.Errorf("wrote %d rows, want one — the upload started and then failed",
				len(after)-len(before))
		}
	})
}

// A rollback to a release that a building rollback is already returning to is
// refused: two rollbacks to one release start one workload name, and the
// second restarted the serving workload in place (security review
// 2026-11-14, PB-1). HasPendingDeployment counts pending rows only, so this
// guard is what stops the second.
func TestRollbackRefusedWhileOneToTheSameReleaseBuilds(t *testing.T) {
	e := newRollbackEnv(t)
	rungOff(t, e.srv)

	inFlight := &database.Deployment{
		AppID: e.app.ID, Status: database.DeployStatusBuilding, Branch: "upload",
		TriggeredBy: database.DeployTriggerRollback, SourceDeployID: e.goodDeploy.ID,
	}
	if err := e.srv.db.CreateDeployment(inFlight, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	rec := e.post(t, "owner", rollbackPath(e.app.ID, e.goodDeploy.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"deploy_queued"`) {
		t.Errorf("body = %s, want code deploy_queued", rec.Body.String())
	}
}
