package autodeploy

import (
	"strings"
	"testing"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/events"
	"toft/internal/gitconn"
	"toft/internal/runtime"
	"toft/internal/sources"
)

// Enforcement point 5, and the transition rule that makes it bearable.
//
// The poller runs every minute forever. A blocked app recorded as a *condition*
// rather than a *transition* would put more than a thousand identical rows a day
// in front of the one that explains the problem — which is the failure the whole
// events package exists to avoid.

func blockedPoller(t *testing.T) (*Poller, *database.DB, *database.App) {
	t.Helper()
	db := dbtest.New(t)

	owner := &database.User{ID: "owner", Login: "owner"}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		ID: "app-blocked", Slug: "blocked", Name: "blocked", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/blocked", Branch: "main",
		Status: database.AppStatusRunning, AutoDeploy: true,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	bld := builder.New(db, runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil), nil, gitconn.NewResolver(db, nil), nil, builder.Options{})
	p := New(db, bld, nil, events.NewRecorder(db, nil, nil), 0, nil)
	return p, db, app
}

func openEvents(t *testing.T, db *database.DB, appID string) []database.AppEvent {
	t.Helper()
	rows, err := db.ListAppEvents(appID, database.AppEventFilter{Limit: 50})
	if err != nil {
		t.Fatalf("ListAppEvents: %v", err)
	}
	out := []database.AppEvent{}
	for _, e := range rows {
		if e.Kind == string(events.KindSourceBlocked) {
			out = append(out, e)
		}
	}
	return out
}

func TestPollerRecordsOneEventPerTransition(t *testing.T) {
	p, db, app := blockedPoller(t)
	if err := db.SetSourcePolicy(sources.ModeAllowlist, true,
		[]database.AllowedGitSource{{Pattern: "gitlab.internal/*"}}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	// Ten passes, as an install would make in ten minutes. Nothing here reaches
	// the network: the policy is checked before ls-remote precisely so that an
	// install which has just blocked a host stops talking to it.
	for i := 0; i < 10; i++ {
		p.check(app)
	}

	rows := openEvents(t, db, app.ID)
	if len(rows) != 1 {
		t.Fatalf("%d source_blocked rows across ten polls, want 1", len(rows))
	}
	if rows[0].Severity != database.EventSeverityWarning {
		t.Errorf("severity = %q, want a warning — the app is serving and an administrator made a decision",
			rows[0].Severity)
	}
	if !strings.Contains(rows[0].Summary, "keeps running") {
		t.Errorf("the summary reads like an outage: %q", rows[0].Summary)
	}

	// The flag is left on, so re-allowing the source resumes deployment without
	// anybody having to remember to switch it back.
	fresh, err := db.GetApp(app.ID)
	if err != nil || fresh == nil {
		t.Fatalf("GetApp: %v", err)
	}
	if !fresh.AutoDeploy {
		t.Error("the poller switched auto-deploy off")
	}
}

func TestPollerClearsTheEventWhenTheSourceIsAllowedAgain(t *testing.T) {
	p, db, app := blockedPoller(t)
	if err := db.SetSourcePolicy(sources.ModeNone, true, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}
	p.check(app)
	if rows := openEvents(t, db, app.ID); len(rows) != 1 || rows[0].ResolvedAt != nil {
		t.Fatalf("expected one open source_blocked row, got %+v", rows)
	}

	// Re-allowed. The next pass clears it — and then goes on to ls-remote, which
	// fails against a repository that does not resolve here; that failure is a
	// different event and is not what this test is about.
	if err := db.SetSourcePolicy(sources.ModeAny, true, nil, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}
	p.check(app)

	rows := openEvents(t, db, app.ID)
	if len(rows) != 1 {
		t.Fatalf("%d source_blocked rows, want the original 1", len(rows))
	}
	if rows[0].ResolvedAt == nil {
		t.Error("the source is allowed again and the event is still open")
	}
}
