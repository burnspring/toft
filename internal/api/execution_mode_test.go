package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// patchAppAs sends a PATCH through the real router as the named user.
func patchAppAs(t *testing.T, srv *Server, router http.Handler, who, appID, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+appID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func modeTestEnv(t *testing.T) (*Server, http.Handler, *database.App) {
	t.Helper()

	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		Slug: "demo", Name: "Demo", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}
	return srv, srv.Router(http.NotFoundHandler()), app
}

// Every app is shared by default, in every topology. Density is what the
// common case — many small internal tools — should get without anyone
// choosing it.
func TestAppsDefaultToSharedMode(t *testing.T) {
	srv, _, app := modeTestEnv(t)

	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution_mode = %q, want %q", got.ExecutionMode, runtime.ModeShared)
	}
}

// Promotion on an install that cannot offer isolated mode is refused with an
// explanation, not a generic error. The distinction matters: an operator
// should read a decision about their install, not a malfunction.
func TestPromotionIsRefusedWithAnExplanation(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	rec := patchAppAs(t, srv, router, "root", app.ID, `{"execution_mode":"isolated"}`)

	// 422, not 400: the request is well-formed and the mode is real; the
	// install simply does not offer it.
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
	}

	var body refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg := body["error"]
	for _, want := range []string{"cluster", "Isolated mode"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the explanation should mention %q, got: %q", want, msg)
		}
	}

	// And nothing was written.
	got, _ := srv.db.GetApp(app.ID)
	if got.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("a refused promotion changed the app to %q", got.ExecutionMode)
	}
}

// Mode is the operator's density decision: promoting an app spends elastic
// capacity the operator pays for, so an app owner cannot do it to their own
// app.
func TestOnlyAnAdminCanChangeExecutionMode(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	rec := patchAppAs(t, srv, router, "owner", app.ID, `{"execution_mode":"shared"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body: %s", rec.Code, rec.Body.String())
	}
}

// The API's vocabulary is modes, never backends. Accepting "fargate" would put
// an infrastructure choice into a developer-facing field and into app rows.
func TestBackendNamesAreRejectedAsModes(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	for _, name := range []string{"fargate", "pod", "kubernetes", "pm2"} {
		t.Run(name, func(t *testing.T) {
			rec := patchAppAs(t, srv, router, "root", app.ID, `{"execution_mode":"`+name+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %q. body: %s", rec.Code, name, rec.Body.String())
			}
		})
	}
}

// Setting the mode an app already has is a no-op that succeeds — correcting a
// density decision must be cheap, and that includes correcting it back.
func TestSettingSharedModeSucceeds(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	rec := patchAppAs(t, srv, router, "root", app.ID, `{"execution_mode":"shared"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got, _ := srv.db.GetApp(app.ID)
	if got.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution_mode = %q", got.ExecutionMode)
	}
}

// An unrelated PATCH must not disturb the mode.
func TestOtherUpdatesLeaveTheModeAlone(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	rec := patchAppAs(t, srv, router, "owner", app.ID, `{"name":"Renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	got, _ := srv.db.GetApp(app.ID)
	if got.Name != "Renamed" {
		t.Errorf("name = %q", got.Name)
	}
	if got.ExecutionMode != string(runtime.ModeShared) {
		t.Errorf("execution_mode changed to %q on an unrelated update", got.ExecutionMode)
	}
}

// A handler that writes twice validates everything before it writes once. The
// memory ceiling used to be written and audited before the slug was checked,
// so a PATCH carrying both a memory limit and a slug somebody else holds
// answered 409 with the limit already changed underneath the caller.
func TestARefusedPatchWritesNothing(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	taken := &database.App{
		Slug: "taken", Name: "Taken", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateAppTx(taken, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}

	rec := patchAppAs(t, srv, router, "root", app.ID, `{"memory_limit_mb":4096,"slug":"taken"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for the taken slug. body: %s", rec.Code, rec.Body.String())
	}
	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryLimitMB == 4096 {
		t.Error("the memory limit was written by a request that was refused")
	}
	var audited int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'app.memory_limit.change' AND target_id = ?`, app.ID).
		Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 0 {
		t.Error("a refused request audited a memory limit change")
	}
}
