package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/builder"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/gitconn"
	"toft/internal/ports"
	"toft/internal/runtime"
)

// The lock is a rule enforced in seven places, and "the list of callers is the
// specification" is only true if something checks the list. These tests are
// that check: each one drives a real request through the real router and
// asserts the refusal, because a lock that holds at the proxy and leaks at the
// deploy endpoint is not a lock.

type lockEnv struct {
	srv    *Server
	router http.Handler
	app    *database.App
}

func newLockEnv(t *testing.T) *lockEnv {
	t.Helper()

	srv := newTestServer(t)
	// A real allocator, so the workload actually listens: the restart at the end
	// of TestUnlockDoesNotStartTheApp goes through Builder.Start, which waits for
	// readiness rather than trusting a nil error.
	appsDir := t.TempDir()
	mock := runtime.NewMockRuntime(ports.New(45500, 45900, func() ([]int, error) { return nil, nil }))
	rts := runtime.NewRuntimes(mock, nil)
	srv.rts = rts
	enc, err := crypto.NewEncryptor("d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0")
	if err != nil {
		t.Fatal(err)
	}
	// Since Restart left the seam, starting a workload is the builder's job —
	// a server without one cannot start anything, which is a 409 rather than
	// the 200 this file's last assertion is about.
	srv.builder = builder.New(srv.db, rts, enc, gitconn.NewResolver(srv.db, enc), nil, builder.Options{AppsDir: appsDir})

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		Slug: "locked-demo", Name: "Locked Demo", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated,
		RepoURL:        "https://github.com/example/demo",
		CurrentRelease: "locked-demo-r1",
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}

	release := filepath.Join(appsDir, app.ID, "releases", "r1")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "index.html"), []byte("<!doctype html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, builder.CurrentLink(appsDir, app.ID)); err != nil {
		t.Fatal(err)
	}

	// A workload actually registered with the backend, not just a row saying
	// `running`. Locking is supposed to *stop* the app, and against a mock with
	// no process the stop fails, the status is never written, and the test would
	// pass or fail for reasons that have nothing to do with the lock.
	if _, err := mock.Deploy(runtime.WorkloadSpec{Slug: app.Slug, Name: app.CurrentRelease}); err != nil {
		t.Fatalf("registering a workload: %v", err)
	}

	return &lockEnv{srv: srv, router: srv.Router(http.NotFoundHandler()), app: app}
}

func (e *lockEnv) do(t *testing.T, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		r.AddCookie(sessionCookieFor(t, e.srv, who))
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)
	return rec
}

// The rule the whole design rests on: locking is not something an app's own
// people can undo. If an owner could unlock, the lock would be exactly as
// strong as `stopped`, and there would have been no reason to build it.
func TestOnlyAnAdminCanLockOrUnlock(t *testing.T) {
	e := newLockEnv(t)
	path := "/api/v1/apps/" + e.app.ID + "/lock"

	if rec := e.do(t, "owner", http.MethodPost, path, `{"message":"mine now"}`); rec.Code == http.StatusOK {
		t.Fatalf("the owner locked their own app: %d %s", rec.Code, rec.Body.String())
	}

	if rec := e.do(t, "root", http.MethodPost, path, `{"message":"Decommissioned."}`); rec.Code != http.StatusOK {
		t.Fatalf("admin lock = %d, body=%s", rec.Code, rec.Body.String())
	}

	// And having been locked by an administrator, the owner cannot lift it.
	if rec := e.do(t, "owner", http.MethodDelete, path, ""); rec.Code == http.StatusOK {
		t.Fatalf("the owner unlocked their own app: %d %s", rec.Code, rec.Body.String())
	}

	app, err := e.srv.db.GetApp(e.app.ID)
	if err != nil || app == nil {
		t.Fatalf("reloading app: %v", err)
	}
	if !app.Locked() {
		t.Fatal("the app came back unlocked after the owner's DELETE")
	}
	if app.LockedMessage != "Decommissioned." {
		t.Errorf("locked_message = %q, want %q", app.LockedMessage, "Decommissioned.")
	}
	// Locking stops the workload — reclaiming what a disused app holds is half
	// the reason to lock one.
	if app.Status != database.AppStatusStopped {
		t.Errorf("status after locking = %q, want %q", app.Status, database.AppStatusStopped)
	}
}

// A non-administrator gets one answer from the lock routes whatever the id:
// the app was looked up before the administrator check, so a real id answered
// 403 and a made-up one 404, which confirmed an unlisted app to a stranger
// (docs/bugs 2026-09-04-api-review #20).
func TestTheLockRoutesDoNotTellANonAdminWhichAppsExist(t *testing.T) {
	e := newLockEnv(t)
	real := "/api/v1/apps/" + e.app.ID + "/lock"
	madeUp := "/api/v1/apps/00000000-0000-4000-8000-000000000000/lock"
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		body := ""
		if method == http.MethodPost {
			body = `{"message":"probe"}`
		}
		got := e.do(t, "owner", method, real, body).Code
		want := e.do(t, "owner", method, madeUp, body).Code
		if got != want || got == http.StatusOK {
			t.Errorf("%s by a non-admin: real app %d, made-up id %d — must be the same refusal", method, got, want)
		}
	}
}

// Every mutating path an owner has. Each is refused with 409 and the
// administrator's own words, not a bare "conflict" and not a 403 — the owner
// has the permission, the app has the state.
func TestLockRefusesEveryMutatingPath(t *testing.T) {
	e := newLockEnv(t)
	lockPath := "/api/v1/apps/" + e.app.ID + "/lock"
	if rec := e.do(t, "root", http.MethodPost, lockPath, `{"message":"Retired in March."}`); rec.Code != http.StatusOK {
		t.Fatalf("locking: %d %s", rec.Code, rec.Body.String())
	}

	base := "/api/v1/apps/" + e.app.ID
	for _, tc := range []struct{ name, method, path, body string }{
		{"restart", http.MethodPost, base + "/restart", ""},
		{"deploy", http.MethodPost, base + "/deploy", `{}`},
		// A rollback changes what is served and starts a workload, which is
		// the whole of what a lock withholds — and it was not on the list the
		// enforcement point calls its specification. An owner refused a deploy
		// could reach the same outcome by making an older build current.
		//
		// The deployment id is deliberately one that does not exist: the check
		// sits above the release resolution, exactly where deploy's does, so a
		// locked app answers 409 before it answers anything about a build.
		{"rollback", http.MethodPost, base + "/deployments/dep-does-not-exist/rollback", ""},
		// A job is another entrypoint of the same code with the same secrets: an
		// owner of a locked app must not start, re-run or resume one.
		{"start a job", http.MethodPost, base + "/jobs/nightly/runs", `{}`},
		{"resume a job", http.MethodPatch, base + "/jobs/nope", `{"enabled":true}`},
		{"re-run a run", http.MethodPost, base + "/job-runs/nope/rerun", ""},
		// Visibility is reach itself, and the environment is what the app runs
		// with when the lock lifts: an owner made a locked app public on
		// trytoft.dev, and nothing refused it.
		{"make it public", http.MethodPatch, base, `{"visibility":"public"}`},
		{"change its environment", http.MethodPatch, base + "/env", `{"set":{"A":{"value":"1"}}}`},
		// Grants, approvals and a transfer are reach the app gains the moment
		// the lock lifts; the destination is what it runs with; a test send is
		// a message sent under the lock; a data reset erases what the lock
		// promises to leave alone (security review 2026-11-15, LSA-2). Each id
		// is made up: the check sits above every lookup.
		{"grant a group", http.MethodPut, base + "/grants/group/grp-does-not-exist", `{"role":"viewer"}`},
		{"approve an access request", http.MethodPost, base + "/access-requests/req-does-not-exist/approve", ""},
		{"transfer it", http.MethodPost, base + "/transfer", `{"new_owner_type":"group","new_owner_id":"grp-does-not-exist"}`},
		{"choose a destination", http.MethodPut, base + "/integrations/slack/destination", `{"id":"C123"}`},
		{"send a test message", http.MethodPost, base + "/integrations/test", `{"kind":"slack"}`},
		{"reset its data", http.MethodDelete, base + "/data", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, "owner", tc.method, tc.path, tc.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s on a locked app = %d, want 409; body=%s", tc.name, rec.Code, rec.Body.String())
			}
			var out refusalBody
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decoding refusal: %v", err)
			}
			if out["code"] != "app_locked" {
				t.Errorf("code = %q, want app_locked", out["code"])
			}
			// The refusal carries the reason. An owner told only "conflict"
			// files a ticket; one told "Retired in March" does not.
			if !strings.Contains(out["error"], "Retired in March.") {
				t.Errorf("refusal did not carry the administrator's message: %q", out["error"])
			}
		})
	}
}

// Unlocking restores permission, not the process. Starting a workload as a side
// effect of a permission change is how an administrator gets a surprise, and a
// release locked away for three months may not be one anybody wants running.
func TestUnlockDoesNotStartTheApp(t *testing.T) {
	e := newLockEnv(t)
	path := "/api/v1/apps/" + e.app.ID + "/lock"

	if rec := e.do(t, "root", http.MethodPost, path, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("locking: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "root", http.MethodDelete, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("unlocking: %d %s", rec.Code, rec.Body.String())
	}

	app, err := e.srv.db.GetApp(e.app.ID)
	if err != nil || app == nil {
		t.Fatalf("reloading app: %v", err)
	}
	if app.Locked() {
		t.Fatal("still locked after DELETE")
	}
	// Locking stopped it; unlocking must leave it exactly as it found it.
	if app.Status != database.AppStatusStopped {
		t.Errorf("unlocking changed the status to %q — it must leave the app %q for somebody to start",
			app.Status, database.AppStatusStopped)
	}
	// The message is forgotten with the lock, so the next administrator's
	// decision does not silently inherit the previous one's wording.
	if app.LockedMessage != "" || app.LockedBy != "" {
		t.Errorf("unlock left %q by %q behind", app.LockedMessage, app.LockedBy)
	}

	// And with the lock gone, the owner's restart works again.
	if rec := e.do(t, "owner", http.MethodPost, "/api/v1/apps/"+e.app.ID+"/restart", ""); rec.Code != http.StatusOK {
		t.Fatalf("restart after unlocking = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// Both lock actions are audited. A platform decision about somebody else's app
// that leaves no record is the thing audit_log exists to prevent.
func TestLockIsAudited(t *testing.T) {
	e := newLockEnv(t)
	path := "/api/v1/apps/" + e.app.ID + "/lock"

	if rec := e.do(t, "root", http.MethodPost, path, `{"message":"Retired."}`); rec.Code != http.StatusOK {
		t.Fatalf("locking: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "root", http.MethodDelete, path, ""); rec.Code != http.StatusOK {
		t.Fatalf("unlocking: %d %s", rec.Code, rec.Body.String())
	}

	// Straight at the table: audit_log deliberately has very few readers and
	// none of them is a general-purpose list, so a test asserting a row exists
	// asks the database rather than growing the reader surface to suit itself.
	rows, err := e.srv.db.Conn().Query(
		`SELECT action FROM audit_log WHERE target_type = 'app' AND target_id = ?`, e.app.ID)
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scanning audit row: %v", err)
		}
		seen[action] = true
	}
	for _, want := range []string{"app.lock", "app.unlock"} {
		if !seen[want] {
			t.Errorf("no %s row in the audit log", want)
		}
	}
}

// **Who placed a lock is said only while there is one** (drills rerun
// 2026-10-22 #9). The column is NOT NULL DEFAULT 'admin', so an unlocked app —
// one just unlocked, or one never locked — read back `locked_source: "admin"`
// beside a null `locked_at`: a source with no lock, which every surface that
// reads the pair has to second-guess.
func TestAnUnlockedAppNamesNoLockSource(t *testing.T) {
	e := newLockEnv(t)
	path := "/api/v1/apps/" + e.app.ID + "/lock"
	source := func(body []byte) (string, bool) {
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
		if app, ok := got["app"].(map[string]any); ok {
			got = app
		}
		v, ok := got["locked_source"].(string)
		return v, ok
	}

	if rec := e.do(t, "root", http.MethodPost, path, `{"message":"hold"}`); rec.Code != http.StatusOK {
		t.Fatalf("locking: %d %s", rec.Code, rec.Body.String())
	}
	if app, _ := e.srv.db.GetApp(e.app.ID); app == nil || app.LockedSource != database.AppLockSourceAdmin {
		t.Fatalf("a locked app's source = %+v, want admin", app)
	}
	rec := e.do(t, "root", http.MethodDelete, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unlocking: %d %s", rec.Code, rec.Body.String())
	}
	if v, ok := source(rec.Body.Bytes()); ok {
		t.Errorf("the unlock's answer still names locked_source %q", v)
	}
	rec = e.do(t, "root", http.MethodGet, "/api/v1/apps/"+e.app.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reading the app: %d %s", rec.Code, rec.Body.String())
	}
	if v, ok := source(rec.Body.Bytes()); ok {
		t.Errorf("an unlocked app reads back locked_source %q", v)
	}
	// LockSource() keeps its default for code that asks.
	if app, _ := e.srv.db.GetApp(e.app.ID); app.LockSource() != database.AppLockSourceAdmin {
		t.Errorf("LockSource() = %q, want the admin default", app.LockSource())
	}
}

// A lock the dependency policy placed is the one a deploy answers: deploy is
// admitted (the build decides whether the release lifts it), and every other
// path still refuses — rollback above all, because it serves the older release
// that was flagged — with the way out in the refusal.
func TestADependencyLockAdmitsADeployAndNothingElse(t *testing.T) {
	e := newLockEnv(t)
	if _, err := e.srv.db.LockAppForDependency(e.app.ID, e.app.Slug, "Locked for MAL-1.",
		nil, time.Now().UTC()); err != nil {
		t.Fatalf("LockAppForDependency: %v", err)
	}
	base := "/api/v1/apps/" + e.app.ID

	if rec := e.do(t, "owner", http.MethodPost, base+"/deploy", `{}`); rec.Code == http.StatusConflict {
		t.Fatalf("deploy under a dependency lock = 409, want it admitted; body=%s", rec.Body.String())
	}

	for _, tc := range []struct{ name, method, path, body string }{
		{"restart", http.MethodPost, base + "/restart", ""},
		{"rollback", http.MethodPost, base + "/deployments/dep-does-not-exist/rollback", ""},
		{"make it public", http.MethodPatch, base, `{"visibility":"public"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(t, "owner", tc.method, tc.path, tc.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("%s under a dependency lock = %d, want 409; body=%s", tc.name, rec.Code, rec.Body.String())
			}
			var out refusalBody
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decoding refusal: %v", err)
			}
			if out["source"] != database.AppLockSourceDependency ||
				!strings.Contains(out["error"], "Deploy a release that no longer carries the package") {
				t.Errorf("the refusal should name the way out: %+v", out)
			}
		})
	}
}
