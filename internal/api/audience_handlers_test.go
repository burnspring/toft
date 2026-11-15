package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
)

// /system/audience (PRD test 14): administrator-only, ISO-week buckets, null
// where no sketch was recorded, the recorded figures still read while the
// switch is off (BB-43), and a window bounded by the access record's retention.

func audienceGet(t *testing.T, srv *Server, who, query string) (int, map[string]any) {
	t.Helper()
	rr := getThroughRouterAs(t, srv, who, "/api/v1/system/audience"+query)
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return rr.Code, body
}

func TestSystemAudienceIsAdministratorOnly(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)
	if code, _ := audienceGet(t, srv, "pub", ""); code != http.StatusForbidden {
		t.Errorf("a publisher got %d, want 403", code)
	}
	if rr := getThroughRouter(t, srv, "/api/v1/system/audience"); rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous got %d, want 401", rr.Code)
	}
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	if code, _ := audienceGet(t, srv, "root", ""); code != http.StatusOK {
		t.Errorf("an administrator got %d, want 200", code)
	}
}

func TestSystemAudienceBucketsAndTheSwitch(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	sun := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // a Sunday
	mon := sun.AddDate(0, 0, 1)
	if err := srv.db.BumpAppUsage([]database.UsageBump{
		{AppID: "a", UserID: "u1", Day: sun, Requests: 4, PageLoads: 2},
		{AppID: "a", UserID: "", Day: mon, Requests: 9, PageLoads: 3},
	}); err != nil {
		t.Fatal(err)
	}
	sk := database.NewAudienceSketch()
	sk.Insert([]byte("browser"))
	b, _ := sk.MarshalBinary()
	if err := srv.db.MergeAudienceSketches([]database.AudienceSketch{{AppID: "a", Day: mon, Sketch: b}}); err != nil {
		t.Fatal(err)
	}
	q := "?since=2026-11-01&until=2026-11-02&grain=week"

	// Off: the switch says so, and the week a sketch was recorded still reads
	// — off stops counting, not the reading of what was counted (BB-43).
	code, body := audienceGet(t, srv, "root", q)
	if code != http.StatusOK {
		t.Fatalf("got %d: %v", code, body)
	}
	if body["audience_measurement"] != false {
		t.Errorf("switch off and audience_measurement = %v", body["audience_measurement"])
	}
	if offB := body["buckets"].([]any); len(offB) != 2 || offB[1].(map[string]any)["anonymous_browsers"] != float64(1) {
		t.Errorf("switch off hid the recorded week: %v", offB)
	}
	if offT := body["totals"].(map[string]any); offT["anonymous_browsers"] != float64(1) {
		t.Errorf("switch off hid the recorded total: %v", offT)
	}

	audienceOn(t, srv, true)
	code, body = audienceGet(t, srv, "root", q)
	if code != http.StatusOK {
		t.Fatalf("got %d: %v", code, body)
	}
	buckets := body["buckets"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("buckets = %v, want two ISO weeks", buckets)
	}
	first, second := buckets[0].(map[string]any), buckets[1].(map[string]any)
	if first["start"] != "2026-10-26" || second["start"] != "2026-11-02" {
		t.Errorf("week starts = %v, %v; want Mondays 2026-10-26 and 2026-11-02", first["start"], second["start"])
	}
	if v, ok := first["anonymous_browsers"]; !ok || v != nil {
		t.Errorf("a week with no sketch reported %v (present %v), want null", v, ok)
	}
	if second["anonymous_browsers"] != float64(1) || second["page_loads"] != float64(3) {
		t.Errorf("second week = %v, want 1 browser and 3 page loads", second)
	}
	if first["people"] != float64(1) {
		t.Errorf("first week people = %v, want 1", first["people"])
	}
	totals := body["totals"].(map[string]any)
	if totals["anonymous_browsers"] != float64(1) || totals["page_loads"] != float64(5) || totals["people"] != float64(1) {
		t.Errorf("totals = %v", totals)
	}
}

func TestSystemAudienceWindowIsBoundedByRetention(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	old := time.Now().UTC().AddDate(0, 0, -401).Format(dayLayout)
	if code, _ := audienceGet(t, srv, "root", "?since="+old); code != http.StatusBadRequest {
		t.Errorf("a window past 400 days got %d, want 400", code)
	}
	if _, err := srv.db.PutSetting(database.SettingAccessRecordRetentionDays, "365", ""); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().AddDate(0, 0, -380).Format(dayLayout)
	code, body := audienceGet(t, srv, "root", "?since="+since)
	if code != http.StatusBadRequest || !strings.Contains(fmtAny(body["error"]), "365 days") {
		t.Errorf("a window past a 365-day retention got %d %v, want 400 naming it", code, body)
	}
	if code, _ := audienceGet(t, srv, "root", "?grain=year"); code != http.StatusBadRequest {
		t.Errorf("grain=year got %d, want 400", code)
	}
}

// The per-app read: page_loads always, anonymous_browsers wherever a sketch was
// recorded — with the switch off too (BB-43) — and never for an app nobody
// counted.
func TestAppUsageCarriesPageLoadsAndBrowsersWhenOn(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := &database.App{Slug: "demo", Name: "Demo", OwnerID: "owner", Status: database.AppStatusRunning}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC()
	if err := srv.db.BumpAppUsage([]database.UsageBump{{AppID: app.ID, UserID: "", Day: today, Requests: 5, PageLoads: 2}}); err != nil {
		t.Fatal(err)
	}
	sk := database.NewAudienceSketch()
	sk.Insert([]byte("x"))
	b, _ := sk.MarshalBinary()
	if err := srv.db.MergeAudienceSketches([]database.AudienceSketch{{AppID: app.ID, Day: today, Sketch: b}}); err != nil {
		t.Fatal(err)
	}
	other := &database.App{Slug: "uncounted", Name: "Uncounted", OwnerID: "owner", Status: database.AppStatusRunning}
	if err := srv.db.CreateApp(other); err != nil {
		t.Fatal(err)
	}
	if rr := getThroughRouterAs(t, srv, "owner", "/api/v1/apps/"+other.ID+"/usage"); strings.Contains(rr.Body.String(), "anonymous_browsers") {
		t.Errorf("an app with no sketch names anonymous browsers: %s", rr.Body.String())
	}
	read := func() string {
		rr := getThroughRouterAs(t, srv, "owner", "/api/v1/apps/"+app.ID+"/usage")
		if rr.Code != http.StatusOK {
			t.Fatalf("usage = %d %s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	off := read()
	if !strings.Contains(off, `"page_loads":2`) || !strings.Contains(off, `"anonymous_browsers":1`) ||
		!strings.Contains(off, `"audience_measurement":false`) {
		t.Errorf("switch off: %s, want the recorded browser and the switch named off", off)
	}
	audienceOn(t, srv, true)
	if on := read(); !strings.Contains(on, `"anonymous_browsers":1`) {
		t.Errorf("switch on: %s", on)
	}
}

func fmtAny(v any) string { s, _ := v.(string); return s }

// A far-future `until` is clamped to today: one bucket per day up to it was
// millions of buckets before any SQL (security review 2026-11-14, PB-9).
func TestSystemAudienceClampsAFutureUntilToToday(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	code, body := audienceGet(t, srv, "root", "?grain=day&until=9999-12-31")
	if code != http.StatusOK {
		t.Fatalf("got %d: %v", code, body)
	}
	today := time.Now().UTC().Format(dayLayout)
	if body["until"] != today {
		t.Errorf("until = %v, want today %s", body["until"], today)
	}
	if buckets, _ := body["buckets"].([]any); len(buckets) > 84 {
		t.Errorf("%d buckets, want at most 84", len(buckets))
	}
}
