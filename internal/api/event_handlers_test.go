package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
)

func seedEvent(t *testing.T, db *database.DB, appID, kind, summary string) *database.AppEvent {
	t.Helper()
	e := &database.AppEvent{
		AppID: appID, Kind: kind, Severity: database.EventSeverityError,
		Source: database.EventSourceAutodeploy, Summary: summary,
		Detail: "git stderr goes here", Fingerprint: "auth",
	}
	if err := db.InsertAppEvent(e); err != nil {
		t.Fatalf("InsertAppEvent: %v", err)
	}
	return e
}

// An app's events carry build-log tails and git's own stderr, so they are
// gated like the logs they excerpt: someone who may only run the app cannot
// read them, and gets a 404 rather than being told the app exists.
func TestAppEventsAreGatedLikeLogs(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	rungOff(t, srv)
	seedUser(t, srv.db, "onlooker", database.SystemRolePublisher)
	seedEvent(t, srv.db, app.ID, "git_auth_failed", "the token expired")

	if rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/events"); rec.Code != http.StatusOK {
		t.Fatalf("owner GET events = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := getAs(t, srv, router, "onlooker", "/api/v1/apps/"+app.ID+"/events"); rec.Code != http.StatusNotFound {
		t.Errorf("a stranger GET events = %d, want 404", rec.Code)
	}
	if rec := getAs(t, srv, router, "", "/api/v1/apps/"+app.ID+"/events"); rec.Code == http.StatusOK {
		t.Errorf("anonymous GET events = %d, want a refusal", rec.Code)
	}
}

// The list is scoped by the app id in the path. Another app's events must not
// appear however the query is dressed up.
func TestAppEventsAreScopedByPathID(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	other := &database.App{Slug: "other", Name: "Other", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateAppTx(other, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create second app: %v", err)
	}
	seedEvent(t, srv.db, app.ID, "git_auth_failed", "mine")
	seedEvent(t, srv.db, other.ID, "git_auth_failed", "theirs")

	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/events?limit=100")
	var got []database.AppEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v — body %s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0].Summary != "mine" {
		t.Fatalf("got %+v, want only this app's event", got)
	}
}

// An app with no events answers with an empty list, not null — a client
// rendering a table should not have to special-case it.
func TestAppEventsEmptyIsAList(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/events")
	if body := rec.Body.String(); body != "[]\n" && body != "[]" {
		t.Fatalf("empty events body = %q, want []", body)
	}
}

// The merged feed is the administrator's, and 404s to everyone else — the
// existence and shape of an install-wide audit trail is not something a
// non-administrator is owed.
func TestActivityFeedIsAdminOnly(t *testing.T) {
	srv, router, _ := modeTestEnv(t)

	if rec := getAs(t, srv, router, "root", "/api/v1/admin/audit"); rec.Code != http.StatusOK {
		t.Fatalf("admin GET audit = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := getAs(t, srv, router, "owner", "/api/v1/admin/audit"); rec.Code != http.StatusNotFound {
		t.Errorf("non-admin GET audit = %d, want 404", rec.Code)
	}
	if rec := getAs(t, srv, router, "", "/api/v1/admin/audit"); rec.Code == http.StatusOK {
		t.Error("anonymous GET audit succeeded")
	}
}

// **A filter the feed does not read is refused, not ignored** (drills rerun
// 2026-10-22 #9). A misspelled parameter answered every row, which looks
// exactly like a filter that matched them all; it is a 400 naming what the
// feed takes, and every parameter the feed reads still answers.
func TestActivityFeedRefusesAParameterItDoesNotRead(t *testing.T) {
	srv, router, _ := modeTestEnv(t)

	rec := getAs(t, srv, router, "root", "/api/v1/admin/audit?catgory=access")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a misspelled parameter = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "unknown_parameter" || !strings.Contains(body["error"].(string), "category") {
		t.Errorf("refusal = %v, want unknown_parameter naming what the feed takes", body)
	}

	ok := "/api/v1/admin/audit?q=app.&category=all&severity=info,warning&limit=5&before_id=x" +
		"&since=2026-01-01T00:00:00Z&until=2030-01-01T00:00:00Z&before=2030-01-01T00:00:00Z" +
		"&action=app.*&actor=root"
	if rec := getAs(t, srv, router, "root", ok); rec.Code != http.StatusOK {
		t.Errorf("every parameter the feed reads = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// A value the feed cannot match is refused like a parameter it does not read:
// each of these would otherwise answer an empty feed, which on an audit trail
// reads as "this never happened".
func TestActivityFeedRefusesAValueItCannotMatch(t *testing.T) {
	srv, router, _ := modeTestEnv(t)
	for _, qs := range []string{"severity=errors", "severity=warning,", "action=Store%20Presign", "action=*"} {
		rec := getAs(t, srv, router, "root", "/api/v1/admin/audit?"+qs)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_parameter") {
			t.Errorf("?%s = %d %s, want 400 invalid_parameter", qs, rec.Code, rec.Body.String())
		}
	}
}

// One person's acts, one action, and what went wrong — each a parameter, and
// a feed narrowed to an actor names them, since the page may hold only the id.
func TestActivityFeedNarrowsByActorActionAndSeverity(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	for _, e := range []database.AuditEntry{
		{ActorType: "user", ActorID: "root", Action: "app.stop", TargetType: "app", TargetID: app.ID},
		{ActorType: "user", ActorID: "root", Action: "deploy_source_refused", TargetType: "app", TargetID: app.ID},
		{ActorType: "anonymous", Action: ActionSigninFailed},
	} {
		if err := srv.db.WriteAuditRow(e); err != nil {
			t.Fatal(err)
		}
	}
	read := func(qs string) (actions []string, actorLabel string) {
		t.Helper()
		rec := getAs(t, srv, router, "root", "/api/v1/admin/audit?limit=100&"+qs)
		if rec.Code != http.StatusOK {
			t.Fatalf("?%s = %d: %s", qs, rec.Code, rec.Body.String())
		}
		var page struct {
			Entries    []database.ActivityEntry `json:"entries"`
			ActorLabel string                   `json:"actor_label"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			actions = append(actions, e.Action)
		}
		sort.Strings(actions)
		return actions, page.ActorLabel
	}

	problems, _ := read("severity=warning,error")
	if got := strings.Join(problems, " "); got != "deploy_source_refused session.signin_failed" {
		t.Errorf("severity=warning,error = %q, want the refusal and the failed sign-in", got)
	}
	one, _ := read("action=session.signin_failed")
	if got := strings.Join(one, " "); got != "session.signin_failed" {
		t.Errorf("action = %q, want the one action", got)
	}
	mine, label := read("actor=root&action=app.stop")
	if got := strings.Join(mine, " "); got != "app.stop" {
		t.Errorf("actor and action = %q, want app.stop", got)
	}
	if label == "" {
		t.Error("a feed narrowed to an actor does not name them")
	}
	for _, a := range func() []string { all, _ := read("actor=root"); return all }() {
		if a == ActionSigninFailed {
			t.Error("an anonymous sign-in failure is in root's acts")
		}
	}
}

// A window that will not parse is refused, not dropped: a dropped `since`
// answered with the whole log, and a dropped `before` restarted a pager.
func TestActivityFeedRefusesAMalformedWindow(t *testing.T) {
	srv, router, _ := modeTestEnv(t)
	for _, name := range []string{"since", "until", "before"} {
		rec := getAs(t, srv, router, "root", "/api/v1/admin/audit?"+name+"=2026-01-01")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s=2026-01-01 = %d, want 400", name, rec.Code)
			continue
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["code"] != "invalid_parameter" || !strings.Contains(body["error"], "`"+name+"`") {
			t.Errorf("?%s refusal = %v, want invalid_parameter naming it", name, body)
		}
	}
}

// The feed merges both halves and hands back a cursor only when there may be
// another page.
func TestActivityFeedMergesAndPages(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	for i := 0; i < 3; i++ {
		seedEvent(t, srv.db, app.ID, "app_crashed", "it stopped")
	}

	rec := getAs(t, srv, router, "root", "/api/v1/admin/audit?limit=2")
	var page struct {
		Entries      []database.ActivityEntry `json:"entries"`
		NextBefore   string                   `json:"next_before"`
		NextBeforeID string                   `json:"next_before_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(page.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(page.Entries))
	}
	if page.NextBefore == "" || page.NextBeforeID == "" {
		t.Fatal("a full page carried no cursor, so nothing can page past it")
	}

	// CreateAppTx writes an object_grant.create audit row, so both halves are
	// represented and the feed is genuinely merged.
	rec = getAs(t, srv, router, "root", "/api/v1/admin/audit?limit=100")
	page.Entries, page.NextBefore, page.NextBeforeID = nil, "", ""
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawEvent, sawAudit bool
	for _, e := range page.Entries {
		switch e.Source {
		case "event":
			sawEvent = true
		case "audit":
			sawAudit = true
		}
	}
	if !sawEvent || !sawAudit {
		t.Fatalf("feed is not merged: events=%v audit=%v", sawEvent, sawAudit)
	}
	if page.NextBefore != "" {
		t.Error("a partial page carried a cursor, which pages into nothing")
	}
}

// The category chips are server-side families of audit actions. "system" is
// the whole app_events half, and an unknown one is refused rather than
// silently returning everything.
func TestActivityFeedCategories(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedEvent(t, srv.db, app.ID, "app_crashed", "it stopped")

	decode := func(path string) []database.ActivityEntry {
		t.Helper()
		rec := getAs(t, srv, router, "root", path)
		var page struct {
			Entries []database.ActivityEntry `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode %s: %v — %s", path, err, rec.Body.String())
		}
		return page.Entries
	}

	system := decode("/api/v1/admin/audit?category=system")
	if len(system) == 0 {
		t.Fatal("category=system returned nothing")
	}
	for _, e := range system {
		if e.Source != "event" {
			t.Fatalf("category=system returned an %s row", e.Source)
		}
	}

	access := decode("/api/v1/admin/audit?category=access")
	for _, e := range access {
		if e.Source != "audit" {
			t.Fatalf("an action category returned an %s row", e.Source)
		}
	}

	if rec := getAs(t, srv, router, "root", "/api/v1/admin/audit?category=nonsense"); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown category = %d, want 400", rec.Code)
	}
}

// Unlike the sleep exemption, failure email belongs to the app's own people:
// anyone who may edit the app may silence it, and a viewer may not.
func TestNotifyFailuresIsTheAppsOwnSwitch(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	if rec := patchAppAs(t, srv, router, "owner", app.ID, `{"notify_failures":false}`); rec.Code != http.StatusOK {
		t.Fatalf("owner PATCH notify_failures = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	stored, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.NotifyFailures {
		t.Fatal("the switch did not turn off")
	}

	// An administrator may too — they can edit any app.
	if rec := patchAppAs(t, srv, router, "root", app.ID, `{"notify_failures":true}`); rec.Code != http.StatusOK {
		t.Errorf("admin PATCH notify_failures = %d, want 200", rec.Code)
	}

	// Somebody with no grant on the app may not.
	if rec := patchAppAs(t, srv, router, "stranger", app.ID, `{"notify_failures":false}`); rec.Code == http.StatusOK {
		t.Error("a stranger changed the app's notification setting")
	}
}

// Turning it off is recorded, because "nobody was told" is exactly the kind of
// thing worth being able to explain afterwards.
func TestNotifyFailuresChangeIsAudited(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	if rec := patchAppAs(t, srv, router, "owner", app.ID, `{"notify_failures":false}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d", rec.Code)
	}

	rows, err := srv.db.ListActivity(database.ActivityFilter{Limit: 50})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	for _, e := range rows {
		if e.Action == "app.notify_failures_change" {
			return
		}
	}
	t.Fatalf("no app.notify_failures_change row; got %+v", rows)
}

// The whole feature has to work with no relay at all: events are still
// recorded and still readable, and nothing errors.
func TestEventsWorkWithoutAnyMailer(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	srv.mailer = nil

	seedEvent(t, srv.db, app.ID, "app_crashed", "it stopped")
	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events with no mailer = %d", rec.Code)
	}
	var got []database.AppEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events with no mailer, want 1", len(got))
	}
}

// Filters narrow rather than widen, and a malformed timestamp is ignored
// rather than emptying the list.
func TestAppEventFilters(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedEvent(t, srv.db, app.ID, "git_auth_failed", "token")
	warn := &database.AppEvent{
		AppID: app.ID, Kind: "git_fetch_failed", Severity: database.EventSeverityWarning,
		Source: database.EventSourceAutodeploy, Summary: "network", OccurredAt: time.Now().UTC(),
	}
	if err := srv.db.InsertAppEvent(warn); err != nil {
		t.Fatal(err)
	}

	decode := func(query string) []database.AppEvent {
		t.Helper()
		rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/events"+query)
		var got []database.AppEvent
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v — %s", query, err, rec.Body.String())
		}
		return got
	}

	if got := decode("?kind=git_auth_failed"); len(got) != 1 || got[0].Kind != "git_auth_failed" {
		t.Errorf("kind filter returned %+v", got)
	}
	if got := decode("?severity=warning"); len(got) != 1 || got[0].Severity != "warning" {
		t.Errorf("severity filter returned %+v", got)
	}
	if got := decode("?since=not-a-time"); len(got) != 2 {
		t.Errorf("a malformed since emptied the list: %+v", got)
	}
}

// One app's history, by id: a request decided about it is in it even though
// the row's target is the request, another app's row is not, a deleted app's
// id still answers, and a malformed one is refused.
func TestActivityFeedNarrowsToOneApp(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	for _, e := range []database.AuditEntry{
		{ActorType: "user", ActorID: "root", Action: "access_request.create",
			TargetType: database.AccessReqTypeObject, TargetID: "req-1", AppID: app.ID},
		{ActorType: "user", ActorID: "root", Action: "app.stop", TargetType: "app",
			TargetID: "11111111-1111-1111-1111-111111111111"},
		{ActorType: "system", Action: database.ActionAppScopeStarted, TargetType: "platform", TargetID: "audit_log"},
	} {
		if err := srv.db.WriteAuditRow(e); err != nil {
			t.Fatal(err)
		}
	}

	rec := getAs(t, srv, router, "root", "/api/v1/admin/audit?limit=100&app="+app.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("?app = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Entries         []database.ActivityEntry `json:"entries"`
		AppScopeStarted string                   `json:"app_scope_started"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var sawRequest bool
	for _, e := range page.Entries {
		if e.AppID != app.ID && e.Source == "audit" {
			t.Errorf("%s about %q in %s's history", e.Action, e.AppID, app.ID)
		}
		if e.Action == "access_request.create" {
			sawRequest = true
		}
	}
	if !sawRequest {
		t.Error("the access request, whose target is the request, is missing from the app's history")
	}
	if page.AppScopeStarted == "" {
		t.Error("no app_scope_started on a narrowed read while the cutover row exists")
	}

	gone := getAs(t, srv, router, "root", "/api/v1/admin/audit?app=11111111-1111-1111-1111-111111111111")
	if gone.Code != http.StatusOK || !strings.Contains(gone.Body.String(), "app.stop") {
		t.Errorf("a deleted app's history = %d %s, want 200 with its row", gone.Code, gone.Body.String())
	}
	if bad := getAs(t, srv, router, "root", "/api/v1/admin/audit?app=reports"); bad.Code != http.StatusBadRequest {
		t.Errorf("?app=<slug> = %d, want 400", bad.Code)
	}
}
