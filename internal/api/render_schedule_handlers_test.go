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

// The re-render schedule's routes: a read, a pause and a recipient list. The
// schedule itself is toft.toml's [render] (ADR §144).

type renderFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
}

func newRenderFixture(t *testing.T, framework string) *renderFixture {
	t.Helper()
	srv := newTestServer(t)
	f := &renderFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	f.app = &database.App{
		Slug: "reports", Name: "Reports", OwnerID: "owner", Framework: framework,
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated,
	}
	if err := srv.db.CreateApp(f.app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return f
}

// declare is what a deploy of a release with [render] does.
func (f *renderFixture) declare(t *testing.T, expr string) {
	t.Helper()
	next := time.Now().UTC().Add(time.Hour)
	if _, err := f.srv.db.ReconcileManifestSchedules(f.app.ID, nil,
		&database.ManifestSchedule{Schedule: expr, NextRunAt: &next}); err != nil {
		t.Fatal(err)
	}
}

func (f *renderFixture) at(t *testing.T, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/apps/"+f.app.ID+"/render-schedule"+path,
		strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(sessionCookieFor(t, f.srv, who))
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *renderFixture) as(t *testing.T, who, method, body string) *httptest.ResponseRecorder {
	return f.at(t, who, method, "", body)
}

// Absent is null, not 404: "this app has none" and "there is no such app" are
// different facts and the page has to tell them apart.
func TestANotebookWithNoRenderIsNull(t *testing.T) {
	f := newRenderFixture(t, "notebook")
	rec := f.as(t, "owner", http.MethodGet, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("GET = %d %s, want 200 null", rec.Code, rec.Body.String())
	}
}

// An old client's PUT or DELETE is told where the schedule is written now.
func TestTheScheduleIsNotWrittenThroughTheAPI(t *testing.T) {
	f := newRenderFixture(t, "notebook")
	f.declare(t, "0 6 * * 1")
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := f.as(t, "owner", method, `{"schedule":"0 3 * * *"}`)
		if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "[render]") {
			t.Errorf("%s = %d %s, want 405 naming [render]", method, rec.Code, rec.Body.String())
		}
	}
	got, _ := f.srv.db.GetRenderSchedule(f.app.ID)
	if got == nil || got.Schedule != "0 6 * * 1" {
		t.Errorf("the declared schedule moved: %+v", got)
	}
}

// PATCH pauses and resumes, and a resume is computed from now.
func TestARenderCanBePausedAndResumed(t *testing.T) {
	f := newRenderFixture(t, "notebook")
	f.declare(t, "0 6 * * 1")

	rec := f.as(t, "owner", http.MethodPatch, `{"enabled":false}`)
	var got database.RenderSchedule
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Enabled || got.NextRunAt != nil {
		t.Fatalf("pause = %d %s", rec.Code, rec.Body.String())
	}
	rec = f.as(t, "owner", http.MethodPatch, `{"enabled":true}`)
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || !got.Enabled || got.NextRunAt == nil {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body.String())
	}
	// Audited, as a job's pause is.
	var n int
	f.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action IN
		('app.render_schedule.pause', 'app.render_schedule.resume') AND target_id = ?`, f.app.ID).Scan(&n)
	if n != 2 {
		t.Errorf("audit rows = %d, want 2", n)
	}
	// A PATCH carrying anything but enabled says where the rest is written.
	if rec := f.as(t, "owner", http.MethodPatch, `{"schedule":"0 3 * * *"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH schedule = %d, want 400", rec.Code)
	}
}

// Pausing or giving recipients to a schedule the file does not declare is a
// 409 that says what would declare one.
func TestNoRenderDeclaredIsAConflict(t *testing.T) {
	f := newRenderFixture(t, "notebook")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, "", `{"enabled":false}`},
		{http.MethodPut, "/recipients", `{"recipients":[]}`},
	} {
		rec := f.at(t, "owner", c.method, c.path, c.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_render_schedule") {
			t.Errorf("%s %s = %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// Recipients have their own route, and an omitted field is not an empty list.
func TestRecipientsAreSetOnTheirOwnRoute(t *testing.T) {
	f := newRenderFixture(t, "notebook")
	f.declare(t, "0 6 * * 1")

	rec := f.at(t, "owner", http.MethodPut, "/recipients", `{"recipients":["owner"]}`)
	var got database.RenderSchedule
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || len(got.Recipients) != 1 {
		t.Fatalf("PUT recipients = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.at(t, "owner", http.MethodPut, "/recipients", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("omitted recipients = %d, want 400", rec.Code)
	}
	// The list survives the deploy that changes the schedule.
	f.declare(t, "CRON_TZ=Europe/London 0 6 * * 1")
	rec = f.as(t, "owner", http.MethodGet, "")
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Recipients) != 1 {
		t.Errorf("after a schedule change recipients = %+v", got.Recipients)
	}
}

// A viewer cannot read, pause or set recipients: this is `can_edit`, like
// every other configuration of an app.
func TestARenderScheduleNeedsEditRights(t *testing.T) {
	f := newRenderFixture(t, "notebook")
	f.declare(t, "0 6 * * 1")
	seedUser(t, f.srv.db, "stranger", database.SystemRoleViewer)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "", ""},
		{http.MethodPatch, "", `{"enabled":false}`},
		{http.MethodPut, "/recipients", `{"recipients":[]}`},
	} {
		rec := f.at(t, "stranger", c.method, c.path, c.body)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 404 or 403", c.method, c.path, rec.Code)
		}
	}
}

// The restart's routes are the same read and pause, over its own row.
func TestARestartScheduleIsReadAndPaused(t *testing.T) {
	f := newRenderFixture(t, "flask")
	path := "/api/v1/apps/" + f.app.ID + "/restart-schedule"
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sessionCookieFor(t, f.srv, "owner"))
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodGet, ""); strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("GET with none = %s", rec.Body.String())
	}
	if rec := do(http.MethodPatch, `{"enabled":false}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "no_restart_schedule") {
		t.Fatalf("pause with none = %d %s", rec.Code, rec.Body.String())
	}
	next := time.Now().UTC().Add(time.Hour)
	f.srv.db.ReconcileManifestSchedules(f.app.ID, &database.ManifestSchedule{Schedule: "0 4 * * *", NextRunAt: &next}, nil)
	rec := do(http.MethodPatch, `{"enabled":false}`)
	var got database.RestartSchedule
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Enabled {
		t.Fatalf("pause = %d %s", rec.Code, rec.Body.String())
	}
	var n int
	f.srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'app.restart_schedule.pause'`).Scan(&n)
	if n != 1 {
		t.Errorf("pause audit rows = %d", n)
	}
}
