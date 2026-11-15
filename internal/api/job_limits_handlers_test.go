package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The job PATCH is an owner's for the pause and an administrator's for the
// limits: an owner pauses (200), an owner's limits are refused with nothing
// applied — not even the pause in the same body — and an administrator sets
// both, audited.
func TestTheJobPatchKeepsLimitsForAnAdministrator(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	app := &database.App{Slug: "reports", Name: "Reports", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	def := declareJob(t, srv.db, app.ID, database.JobDefinition{Name: "nightly", Command: "python n.py",
		Schedule: "0 3 * * *"})

	patch := func(who, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+app.ID+"/jobs/"+def.ID, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(sessionCookieFor(t, srv, who))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := patch("owner", `{"enabled": false}`); rec.Code != http.StatusOK {
		t.Fatalf("owner pause: %d %s", rec.Code, rec.Body)
	}
	rec := patch("owner", `{"enabled": true, "memory_limit_mb": 2048}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "memory_limit_mb") {
		t.Fatalf("owner limits: %d %s, want 403 naming the field", rec.Code, rec.Body)
	}
	got, _ := srv.db.GetJobDefinition(app.ID, def.ID)
	if got.Enabled || got.OverrideMemoryMB != 0 {
		t.Errorf("a refused body was partly applied: enabled=%v memory=%d", got.Enabled, got.OverrideMemoryMB)
	}

	rec = patch("root", `{"memory_limit_gb": 2, "cpu_limit_cores": 0.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin limits: %d %s", rec.Code, rec.Body)
	}
	var back database.JobDefinition
	_ = json.Unmarshal(rec.Body.Bytes(), &back)
	if back.OverrideMemoryMB != 2048 || back.OverrideCPUMillicores != 500 {
		t.Errorf("stored %d MB, %dm; want 2048, 500", back.OverrideMemoryMB, back.OverrideCPUMillicores)
	}
	var n int
	if err := srv.db.Conn().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, "job.resources.change").Scan(&n); err != nil || n != 1 {
		t.Errorf("job.resources.change rows = %d (%v), want 1", n, err)
	}

	// null clears one, and the job list shows where each figure comes from.
	if rec := patch("root", `{"cpu_limit_millicores": null}`); rec.Code != http.StatusOK {
		t.Fatalf("admin clear: %d %s", rec.Code, rec.Body)
	}
	got, _ = srv.db.GetJobDefinition(app.ID, def.ID)
	if got.OverrideCPUMillicores != 0 || got.OverrideMemoryMB != 2048 {
		t.Errorf("after clearing CPU: %d MB, %dm", got.OverrideMemoryMB, got.OverrideCPUMillicores)
	}
	if rec := patch("root", `{"cpu_limit_millicores": 10}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a 10m limit: %d, want 400", rec.Code)
	}

	list := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/jobs")
	var body struct {
		Jobs []struct {
			Limits struct {
				Memory struct {
					Value       int    `json:"value"`
					Source      string `json:"source"`
					Enforcement string `json:"enforcement"`
				} `json:"memory"`
				CPU struct {
					Value  int    `json:"value"`
					Source string `json:"source"`
				} `json:"cpu"`
			} `json:"limits"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || len(body.Jobs) != 1 {
		t.Fatalf("job list: %v %s", err, list.Body)
	}
	l := body.Jobs[0].Limits
	if l.Memory.Value != 2048 || l.Memory.Source != "override" || l.Memory.Enforcement == "" ||
		l.CPU.Source != "install default" {
		t.Errorf("limits = %+v", l)
	}
}
