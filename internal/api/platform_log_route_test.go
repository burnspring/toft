package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"toft/internal/applog"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The route, end to end, against a real database and the real router.
//
// The merge is unit-tested on its own; what this covers is everything around
// it — that the sink reaches the store, that the store reaches the handler,
// that authorization is unchanged, and that the answer's shape is what a
// client reads. `newLogTestEnv` gives an app owned by "owner" with the mock
// runtime registered.
func newPlatformLogEnv(t *testing.T) (*logTestEnv, *applog.Sink) {
	t.Helper()
	e := newLogTestEnv(t)
	sink := applog.New(e.srv.db)
	t.Cleanup(sink.Close)
	e.srv.platformLog = sink
	return e, sink
}

func readLogs(t *testing.T, e *logTestEnv, who string) logsResponse {
	t.Helper()
	rec := e.as(t, who, fmt.Sprintf("/api/v1/apps/%s/logs", e.app.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET logs: %d — %s", rec.Code, rec.Body.String())
	}
	var got logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the tail: %v", err)
	}
	return got
}

// A stopped app's pane is no longer empty. This is the whole feature.
func TestAStoppedAppAnswersWithThePlatformsAccount(t *testing.T) {
	e, sink := newPlatformLogEnv(t)

	sink.For(e.app.ID, e.app.ProcessName(), "").Say(applog.StreamStdout, "==> Starting release abc123")
	sink.For(e.app.ID, e.app.ProcessName(), "").Say(applog.StreamStderr, "The platform has stopped restarting this app after 5 attempts.")
	sink.Flush()

	if err := e.srv.db.UpdateAppStatus(e.app.ID, database.AppStatusStopped); err != nil {
		t.Fatalf("stopping the app: %v", err)
	}

	got := readLogs(t, e, "owner")
	if len(got.Lines) != 2 {
		t.Fatalf("a stopped app answered %d lines; the pointer in every crash event lands here", len(got.Lines))
	}
	if got.Reason != runtime.LogAbsentStopped {
		t.Errorf("reason is %q; the word travels with the lines now", got.Reason)
	}
	for _, l := range got.Lines {
		if l.Source != runtime.LogSourcePlatform {
			t.Errorf("a stored line came back as %q", l.Source)
		}
		if l.At == nil {
			t.Error("a stored line lost its timestamp; the store is the half that has one")
		}
	}
	if !strings.Contains(got.Logs, "stopped restarting") {
		t.Error("the flat blob is missing the platform's lines; `toft logs` reads exactly that")
	}
}

// A locked app answers with the word alone. A lock is an administrator taking
// the app away, and the copy beside it promises nothing it printed is kept.
func TestALockedAppStillAnswersNothing(t *testing.T) {
	e, sink := newPlatformLogEnv(t)

	sink.For(e.app.ID, e.app.ProcessName(), "").Say(applog.StreamStdout, "==> Starting release abc123")
	sink.Flush()

	// A seeded account, because `apps.locked_by` is a foreign key onto users:
	// the database refuses a lock by nobody-in-particular, which is what the
	// column is for.
	admin := seedUser(t, e.srv.db, "locking-admin", database.SystemRoleAdmin)
	if err := e.srv.db.SetAppLock(e.app.ID, admin.ID, "Taken away pending review", time.Now().UTC(), e.app.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("locking the app: %v", err)
	}

	got := readLogs(t, e, "owner")
	if len(got.Lines) != 0 {
		t.Fatalf("a locked app answered %d lines: %+v", len(got.Lines), got.Lines)
	}
	if got.Reason != runtime.LogAbsentLocked {
		t.Errorf("reason is %q, want locked", got.Reason)
	}
}

// The gate is the one that already existed. A new source of rows must not
// arrive behind a weaker one.
func TestThePlatformLogIsBehindTheSameGate(t *testing.T) {
	e, sink := newPlatformLogEnv(t)
	sink.For(e.app.ID, e.app.ProcessName(), "").Say(applog.StreamStdout, "==> Starting release abc123")
	sink.Flush()

	for _, who := range []string{"", "stranger", "reader"} {
		name := who
		if name == "" {
			name = "anonymous"
		}
		t.Run(name, func(t *testing.T) {
			rec := e.as(t, who, fmt.Sprintf("/api/v1/apps/%s/logs", e.app.ID))
			if rec.Code == http.StatusOK {
				t.Fatalf("%s read the platform's log: %s", name, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "abc123") {
				t.Errorf("%s was refused and told what the log says anyway", name)
			}
		})
	}
}

// One app's narration never reaches another app's pane. The store is queried
// by the app id in the URL, which is the IDOR rule this route already keeps.
func TestOneAppsNarrationStaysOnThatApp(t *testing.T) {
	e, sink := newPlatformLogEnv(t)

	other := &database.App{
		Slug: "other", Name: "Other", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusStopped,
	}
	if err := e.srv.db.CreateApp(other); err != nil {
		t.Fatalf("create app: %v", err)
	}
	sink.For(other.ID, other.ProcessName(), "").Say(applog.StreamStdout, "==> the other app's business")
	sink.Flush()

	got := readLogs(t, e, "owner")
	if strings.Contains(got.Logs, "other app's business") {
		t.Fatal("one app's pane carried another app's narration")
	}
}

// An install with no sink answers exactly what it answered before.
func TestWithNoSinkTheRouteIsUnchanged(t *testing.T) {
	e := newLogTestEnv(t)
	e.srv.platformLog = nil

	if err := e.srv.db.UpdateAppStatus(e.app.ID, database.AppStatusStopped); err != nil {
		t.Fatalf("stopping the app: %v", err)
	}
	got := readLogs(t, e, "owner")
	if len(got.Lines) != 0 || got.Reason != runtime.LogAbsentStopped {
		t.Fatalf("absent configuration changed the answer: %+v", got)
	}
	if got.Lines == nil {
		t.Error("Lines is nil; `[]` is what tells toft the log is empty")
	}
}
