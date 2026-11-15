package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/builder"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/gitconn"
	"toft/internal/ports"
	"toft/internal/runtime"
)

// An app that has never deployed: a row, no release, no workload anywhere.
func newNeverDeployedEnv(t *testing.T) (*Server, http.Handler, *database.App) {
	t.Helper()

	srv := newTestServer(t)
	mock := runtime.NewMockRuntime(ports.New(45900, 46200, func() ([]int, error) { return nil, nil }))
	rts := runtime.NewRuntimes(mock, nil)
	srv.rts = rts
	enc, err := crypto.NewEncryptor(strings.Repeat("d0", 32))
	if err != nil {
		t.Fatal(err)
	}
	srv.builder = builder.New(srv.db, rts, enc, gitconn.NewResolver(srv.db, enc), nil,
		builder.Options{AppsDir: t.TempDir()})

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{
		Slug: "never-deployed", Name: "Never Deployed", OwnerID: "owner",
		Status: database.AppStatusBuilding, Visibility: database.VisibilityAuthenticated,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return srv, srv.Router(http.NotFoundHandler()), app
}

// Stopping an app with no workload is not a failure: the state the caller asked
// for is the state it is in.
//
// It was a 500 carrying `pm2 stop: [PM2][ERROR] Process or Namespace ... not
// found` — a backend's own wire format read out to somebody who has no reason
// to know which backend runs their app, and, worse, a server error for a state
// that is perfectly ordinary. e2e APP.8.
func TestStoppingAnAppWithNoWorkloadIsNotAServerError(t *testing.T) {
	srv, router, app := newNeverDeployedEnv(t)

	rec := sendAs(t, srv, router, "owner", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/stop", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stopping a never-deployed app: %d (%s), want 200",
			rec.Code, rec.Body.String())
	}
	body := strings.ToLower(rec.Body.String())
	for _, backend := range []string{"pm2", "kube", "pod", "namespace"} {
		if strings.Contains(body, backend) {
			t.Errorf("the answer names a backend (%q): %s", backend, rec.Body.String())
		}
	}

	// The row is still there and the owner's intent is recorded.
	after, err := srv.db.GetApp(app.ID)
	if err != nil || after == nil {
		t.Fatalf("re-reading the app: %v", err)
	}
	if after.Status != database.AppStatusStopped {
		t.Errorf("status = %q, want %q", after.Status, database.AppStatusStopped)
	}
	if after.LockedAt != nil {
		t.Error("stopping wrote a lock — `stopped` is not a lock")
	}
}

// And restarting one is a refusal the caller can act on.
//
// It was a 500 whose message was "release directory missing — redeploy the app",
// said to somebody who had not deployed it once. e2e APP.8.
func TestRestartingAnAppWithNoReleaseIsARefusalAndNotAServerError(t *testing.T) {
	srv, router, app := newNeverDeployedEnv(t)

	rec := sendAs(t, srv, router, "owner", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/restart", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("restarting an app with no release: %d (%s), want 409",
			rec.Code, rec.Body.String())
	}
	var out struct{ Error, Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the refusal: %v (%s)", err, rec.Body.String())
	}
	if out.Code != "no_release" {
		t.Errorf("code = %q, want no_release", out.Code)
	}
	if out.Error == "" {
		t.Error("the refusal carries no sentence")
	}

	// And it did not blame the app for the state of the disk.
	after, _ := srv.db.GetApp(app.ID)
	if after.Status == database.AppStatusCrashed {
		t.Error("the app was marked crashed; nothing was started and nothing crashed")
	}
}

// The seam's half of the same rule: a backend asked to stop a name it does not
// hold says so as an absence, in the word the other backend uses.
func TestStoppingAnUnknownWorkloadIsAnAbsence(t *testing.T) {
	mock := runtime.NewMockRuntime(nil)
	err := mock.Stop("nothing-holds-this-name")
	if got := runtime.AbsenceReason(err); got != runtime.LogAbsentNoWorkload {
		t.Fatalf("AbsenceReason = %q, want %q (err = %v)",
			got, runtime.LogAbsentNoWorkload, err)
	}
}
