package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
)

type viewersBody struct {
	Days    int `json:"days"`
	Viewers []struct {
		Kind        string `json:"kind"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Requests    int64  `json:"requests"`
		Days        int    `json:"days"`
		FirstDay    string `json:"first_day"`
		LastDay     string `json:"last_day"`
	} `json:"viewers"`
}

func seedUsage(t *testing.T, db *database.DB, appID, userID string, daysAgo int, n int64) {
	t.Helper()
	day := time.Now().UTC().AddDate(0, 0, -daysAgo)
	if err := db.BumpAppUsage([]database.UsageBump{{AppID: appID, UserID: userID, Day: day, Requests: n}}); err != nil {
		t.Fatalf("bump usage: %v", err)
	}
}

// The viewer breakdown names people, so it is gated harder than the totals it
// breaks down: a plain reader of the app must not get it.
func TestUsageViewers_NeedsManageGrants(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "reader", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	app := &database.App{
		Slug: "metrics-app", Name: "Metrics App", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	rec := putGrantAs(t, srv, router, "owner", app.ID, "user", "reader", database.ObjectRoleViewer)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("grant viewer: %d body=%s", rec.Code, rec.Body.String())
	}

	// The reader can see the aggregate totals...
	if rec := getAs(t, srv, router, "reader", "/api/v1/apps/"+app.ID+"/usage"); rec.Code != http.StatusOK {
		t.Fatalf("reader on /usage: %d, want 200", rec.Code)
	}
	// ...but not who produced them.
	if rec := getAs(t, srv, router, "reader", "/api/v1/apps/"+app.ID+"/usage/viewers"); rec.Code != http.StatusForbidden {
		t.Fatalf("reader on /usage/viewers: %d, want 403", rec.Code)
	}
	// Someone who cannot read the app is not told who views it — and what they
	// are told about the app *existing* is the install's decision, not this
	// route's.
	//
	// With the install-wide `list` rung on, which is the default since
	// 2026-08-31, everybody signed in holds the rung on every app: they have
	// already been told this app exists, by name, in their own list. So the
	// refusal is the rung's door rather than a 404 — a 404 here would teach
	// them the list they are looking at is unreliable, which is the reasoning
	// *404 over 403*'s exception already carries.
	if rec := getAs(t, srv, router, "stranger", "/api/v1/apps/"+app.ID+"/usage/viewers"); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger on /usage/viewers: %d, want 403 — the install lists every app to "+
			"everybody signed in, so the refusal is the rung's", rec.Code)
	}

	// And with it off, or on an app its owner withdrew, the rule is back
	// unchanged: nobody is told the app is there at all. Asserted here rather
	// than left to the rung's own tests, because this is the route that would
	// leak *who uses* an app, and the two answers have to be the same shape as
	// the one above rather than a special case of it.
	if _, err := srv.db.PutSetting(database.SettingAppsListableToAuthenticated, "false", ""); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	if rec := getAs(t, srv, router, "stranger", "/api/v1/apps/"+app.ID+"/usage/viewers"); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger on /usage/viewers with the rung off: %d, want 404", rec.Code)
	}
	if _, err := srv.db.PutSetting(database.SettingAppsListableToAuthenticated, "true", ""); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	if err := srv.db.SetAppListable(app.ID, true, false,
		database.PrincipalTypeUser, "owner", "", "", "", ""); err != nil {
		t.Fatalf("SetAppListable: %v", err)
	}
	if rec := getAs(t, srv, router, "stranger", "/api/v1/apps/"+app.ID+"/usage/viewers"); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger on /usage/viewers for an app its owner withdrew: %d, want 404 — the "+
			"column is what puts 404-over-403 back within the owner's reach", rec.Code)
	}
	if err := srv.db.SetAppListable(app.ID, false, true,
		database.PrincipalTypeUser, "owner", "", "", "", ""); err != nil {
		t.Fatalf("SetAppListable: %v", err)
	}
	if rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/usage/viewers"); rec.Code != http.StatusOK {
		t.Fatalf("owner on /usage/viewers: %d, want 200", rec.Code)
	}
}

// Anonymous traffic and API-key traffic are part of what the app served, so
// they appear as their own rows rather than being silently dropped — otherwise
// the breakdown would not add up to the totals shown beside it.
func TestUsageViewers_ClassifiesEveryKindOfTraffic(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "viewer-one", database.SystemRolePublisher)

	app := &database.App{
		Slug: "public-app", Name: "Public App", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	seedUsage(t, srv.db, app.ID, "viewer-one", 0, 5)
	seedUsage(t, srv.db, app.ID, "viewer-one", 3, 5)
	seedUsage(t, srv.db, app.ID, "", 0, 100)                           // anonymous
	seedUsage(t, srv.db, app.ID, database.UsageKeyPrefix+"gone", 1, 7) // key since deleted
	seedUsage(t, srv.db, app.ID, "deleted-user-id", 2, 3)              // account since removed

	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/usage/viewers?days=30")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner read: %d body=%s", rec.Code, rec.Body.String())
	}
	var body viewersBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Viewers) != 4 {
		t.Fatalf("got %d rows, want 4: %+v", len(body.Viewers), body.Viewers)
	}

	// Busiest first, and the anonymous pool is the busiest here.
	if body.Viewers[0].Kind != "anonymous" || body.Viewers[0].Requests != 100 {
		t.Fatalf("first row = %+v, want the anonymous pool with 100 requests", body.Viewers[0])
	}
	for i := 1; i < len(body.Viewers); i++ {
		if body.Viewers[i].Requests > body.Viewers[i-1].Requests {
			t.Fatalf("rows are not sorted by requests descending: %+v", body.Viewers)
		}
	}

	byKind := map[string]struct {
		name string
		reqs int64
		days int
	}{}
	for _, v := range body.Viewers {
		byKind[v.Kind] = struct {
			name string
			reqs int64
			days int
		}{v.DisplayName, v.Requests, v.Days}
	}

	// A user's two days of traffic collapse into one row that keeps both.
	if got := byKind["user"]; got.name != "viewer-one" || got.reqs != 10 || got.days != 2 {
		t.Fatalf("user row = %+v, want viewer-one with 10 requests over 2 days", got)
	}
	if got := byKind["api_key"]; got.name != "Deleted API key" || got.reqs != 7 {
		t.Fatalf("api_key row = %+v", got)
	}
	if got := byKind["removed"]; got.name != "Removed account" || got.reqs != 3 {
		t.Fatalf("removed row = %+v", got)
	}
}

// Active-user counts are an operator's number and exclude anything that is not
// a person.
func TestActiveUsers_AdminOnly_ExcludesAnonymousAndKeys(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)

	app := &database.App{
		Slug: "counted", Name: "Counted", OwnerID: "root",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	seedUsage(t, srv.db, app.ID, "bob", 0, 4)                          // today
	seedUsage(t, srv.db, app.ID, "root", 10, 2)                        // this month only
	seedUsage(t, srv.db, app.ID, "", 0, 900)                           // never counted
	seedUsage(t, srv.db, app.ID, database.UsageKeyPrefix+"k1", 0, 900) // never counted

	if rec := getAs(t, srv, router, "bob", "/api/v1/system/active-users"); rec.Code != http.StatusForbidden {
		t.Fatalf("publisher: %d, want 403", rec.Code)
	}

	rec := getAs(t, srv, router, "root", "/api/v1/system/active-users")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: %d body=%s", rec.Code, rec.Body.String())
	}
	var counts database.ActiveUserCounts
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if counts.Today != 1 {
		t.Fatalf("today = %d, want 1 (bob only)", counts.Today)
	}
	if counts.Week != 1 {
		t.Fatalf("week = %d, want 1", counts.Week)
	}
	if counts.Month != 2 || counts.Year != 2 {
		t.Fatalf("month/year = %d/%d, want 2/2", counts.Month, counts.Year)
	}
}

// A personal key's traffic names the person. The proxy records the key, not
// the owner — so a nightly cron is traffic and not a daily active human — and
// the breakdown is where the row resolves back. It used to join only the
// app's own keys, so a live personal key read as "Deleted API key".
func TestUsageViewers_APersonalKeyNamesItsOwner(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "alice", database.SystemRolePublisher)
	app := &database.App{
		Slug: "reports", Name: "Reports", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	key := &database.UserAPIKey{ID: "pk-1", UserID: "alice", Name: "nightly", KeyHash: "h", KeyPrefix: "toftu_x", Scope: database.UserKeyScopeFull}
	if err := srv.db.CreateUserAPIKeyTx(key, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, srv.db, app.ID, database.UsageKeyPrefix+"pk-1", 0, 4)

	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/usage/viewers?days=30")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner read: %d body=%s", rec.Code, rec.Body.String())
	}
	var body viewersBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Viewers) != 1 || body.Viewers[0].Kind != "api_key" {
		t.Fatalf("rows = %+v, want one api_key row", body.Viewers)
	}
	if got := body.Viewers[0].DisplayName; !strings.Contains(got, "alice") || !strings.Contains(got, "nightly") {
		t.Errorf("display name = %q, want the person and the key's name", got)
	}
}

// A deleted person's key traffic is still named. The key cascaded away with
// the account, so the name comes from the records that outlived both: the
// owner the traffic was written with, the login on user.delete, the key's
// name on its user.api_key_create.
func TestUsageViewers_ADeletedPersonsKeyTrafficIsStillNamed(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{Slug: "reports", Name: "Reports", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning, Visibility: database.VisibilityPublic}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	for _, e := range []database.AuditEntry{
		{ActorType: "user", ActorID: "gone", Action: "user.api_key_create", TargetType: "user", TargetID: "gone",
			After: `{"key_id": "pk-gone", "name": "nightly"}`},
		{ActorType: "user", ActorID: "owner", Action: "user.delete", TargetType: "user", TargetID: "gone",
			Before: `{"login": "priya"}`},
	} {
		if err := srv.db.WriteAuditRow(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.db.BumpAppUsage([]database.UsageBump{{AppID: app.ID, UserID: database.UsageKeyPrefix + "pk-gone",
		KeyOwnerID: "gone", Day: time.Now().UTC(), Requests: 3}}); err != nil {
		t.Fatal(err)
	}

	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/usage/viewers")
	var body viewersBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Viewers) != 1 || body.Viewers[0].DisplayName != `priya (key "nightly")` {
		t.Errorf("viewers = %+v, want priya's key, named", body.Viewers)
	}
}

// The window: dates inclusive, the old `days` still answers, the two forms
// are never mixed, and the file is the day grain with a formula neutralised.
func TestUsageViewers_ARangeAndItsFile(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "alice", database.SystemRolePublisher)
	if _, err := srv.db.Conn().Exec(`UPDATE users SET login = '=HYPERLINK("x")' WHERE id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	app := &database.App{Slug: "reports", Name: "Reports", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning, Visibility: database.VisibilityPublic}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, srv.db, app.ID, "alice", 200, 5) // outside 90 days, inside a year
	seedUsage(t, srv.db, app.ID, "alice", 1, 2)

	day := func(ago int) string { return time.Now().UTC().AddDate(0, 0, -ago).Format("2006-01-02") }
	base := "/api/v1/apps/" + app.ID + "/usage/viewers"

	rec := getAs(t, srv, router, "owner", base+"?since="+day(365)+"&until="+day(0))
	var body viewersBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("a year's range: %d %s", rec.Code, rec.Body.String())
	}
	if len(body.Viewers) != 1 || body.Viewers[0].Requests != 7 || body.Viewers[0].Days != 2 {
		t.Errorf("a year = %+v, want alice with 7 requests over 2 days", body.Viewers)
	}
	rec = getAs(t, srv, router, "owner", base+"?since="+day(1)+"&until="+day(1))
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Viewers) != 1 || body.Viewers[0].Requests != 2 {
		t.Errorf("one day, inclusive = %+v, want 2 requests", body.Viewers)
	}

	for _, q := range []string{"?days=30&since=" + day(5), "?since=2026-1-1", "?since=" + day(0) + "&until=" + day(3), "?until=" + day(0)} {
		if rec := getAs(t, srv, router, "owner", base+q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, base+"?since="+day(365), nil)
	req.Header.Set("Accept", "text/csv")
	req.AddCookie(sessionCookieFor(t, srv, "owner"))
	file := httptest.NewRecorder()
	router.ServeHTTP(file, req)
	lines := strings.Split(strings.TrimSpace(file.Body.String()), "\n")
	if file.Code != http.StatusOK || len(lines) != 3 || !strings.HasPrefix(lines[0], "day,kind,user_id") {
		t.Fatalf("the file: %d %q", file.Code, file.Body.String())
	}
	if !strings.Contains(lines[1], day(200)) || !strings.Contains(file.Body.String(), `'=HYPERLINK`) {
		t.Errorf("the file is not the day grain, or a formula survived: %q", file.Body.String())
	}
}
