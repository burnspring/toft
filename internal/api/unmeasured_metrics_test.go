package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// Two backends that disagree about what they can measure, which is the whole
// subject: the shared one reports a figure, the isolated one reports that it
// took no reading. Both embed the mock for the rest of ServiceRuntime — only
// the two metrics calls are the point.
type measuringRuntime struct {
	*runtime.MockRuntime
	name string
	cpu  float64
}

func (m *measuringRuntime) Metrics(string) (*runtime.AppMetrics, error) {
	return &runtime.AppMetrics{Status: "online", Uptime: 60, CPU: runtime.Measured(m.cpu), Memory: runtime.Measured(128)}, nil
}

func (m *measuringRuntime) List() (map[string]*runtime.AppMetrics, error) {
	one, _ := m.Metrics(m.name)
	return map[string]*runtime.AppMetrics{m.name: one}, nil
}

// unmeasuredRuntime is kube's shape: a pod's status, restarts and uptime, and
// nothing about CPU or memory.
type unmeasuredRuntime struct {
	*runtime.MockRuntime
	name string
}

func (u *unmeasuredRuntime) Metrics(string) (*runtime.AppMetrics, error) {
	return &runtime.AppMetrics{Status: "online", Uptime: 90, Restarts: 2}, nil
}

func (u *unmeasuredRuntime) List() (map[string]*runtime.AppMetrics, error) {
	one, _ := u.Metrics(u.name)
	return map[string]*runtime.AppMetrics{u.name: one}, nil
}

// erroredRuntime is the third shape, and the one that produced false zeros on
// a real box: a backend that *tried* to measure and could not. A workload PM2
// listed as `online` with no process behind it reaches the seam as `errored`
// with no reading at all — where it used to arrive as `online` at 0.0% and 0 MB,
// indistinguishable at every layer above from an app sitting idle.
type erroredRuntime struct {
	*runtime.MockRuntime
	name string
}

func (e *erroredRuntime) Metrics(string) (*runtime.AppMetrics, error) {
	return &runtime.AppMetrics{Status: "errored", Uptime: 300, Restarts: 0}, nil
}

func (e *erroredRuntime) List() (map[string]*runtime.AppMetrics, error) {
	one, _ := e.Metrics(e.name)
	return map[string]*runtime.AppMetrics{e.name: one}, nil
}

// newUnmeasuredEnv builds one shared app whose backend measures and one
// isolated app whose backend does not. The shared app is deliberately reported
// at 0.0% — the case the sort has to keep telling apart from no reading at all.
func newUnmeasuredEnv(t *testing.T) (*Server, http.Handler, *database.App, *database.App) {
	t.Helper()

	srv := newTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	shared := &database.App{Slug: "idle-shared", Name: "Idle Shared", OwnerID: "root", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate, RunningMode: string(runtime.ModeShared)}
	isolated := &database.App{Slug: "pod-app", Name: "Pod App", OwnerID: "root", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate, RunningMode: string(runtime.ModeIsolated)}
	for _, a := range []*database.App{shared, isolated} {
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
	}

	srv.rts = runtime.NewRuntimes(
		&measuringRuntime{MockRuntime: runtime.NewMockRuntime(nil), name: shared.ProcessName(), cpu: 0.0},
		&unmeasuredRuntime{MockRuntime: runtime.NewMockRuntime(nil), name: isolated.ProcessName()},
	)
	return srv, srv.Router(http.NotFoundHandler()), shared, isolated
}

func decodeRows(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp struct {
		Processes []map[string]any `json:"processes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode processes: %v (body %q)", err, body)
	}
	return resp.Processes
}

// The wire half of the fix: a workload nobody measured carries no cpu or
// memory key at all, so a consumer gets undefined rather than a zero it would
// render as an idle machine.
func TestProcesses_UnmeasuredFiguresAreAbsentNotZero(t *testing.T) {
	srv, router, _, _ := newUnmeasuredEnv(t)

	rec := getAs(t, srv, router, "root", "/api/v1/system/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("processes: %d body=%s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec.Body.Bytes())
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (body %s)", len(rows), rec.Body.String())
	}

	byName := map[string]map[string]any{}
	for _, r := range rows {
		byName[r["app_name"].(string)] = r
	}

	measured := byName["Idle Shared"]
	if cpu, ok := measured["cpu"]; !ok || cpu.(float64) != 0.0 {
		t.Errorf("measured row: cpu = %v (present=%v), want 0 present — a real reading of zero is still a reading", cpu, ok)
	}
	if _, ok := measured["memory"]; !ok {
		t.Error("measured row: memory key missing")
	}

	unmeasured := byName["Pod App"]
	if v, ok := unmeasured["cpu"]; ok {
		t.Errorf("unmeasured row carries a cpu key (%v); it must be absent", v)
	}
	if v, ok := unmeasured["memory"]; ok {
		t.Errorf("unmeasured row carries a memory key (%v); it must be absent", v)
	}
	// The rest of the row is still answerable and still there.
	if unmeasured["process_status"] != "online" || unmeasured["restarts"].(float64) != 2 {
		t.Errorf("unmeasured row lost the fields the backend *can* answer: %v", unmeasured)
	}
}

// D4. Busiest-first put every isolated app at the bottom already, but as the
// claim "least busy" rather than as an absent answer — and a measured 0.0%
// sorted level with it. The unmeasured row goes below the measured zero.
func TestProcesses_UnmeasuredSortsBelowAMeasuredZero(t *testing.T) {
	srv, router, _, _ := newUnmeasuredEnv(t)

	rec := getAs(t, srv, router, "root", "/api/v1/system/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("processes: %d body=%s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec.Body.Bytes())

	if got := rows[0]["app_name"]; got != "Idle Shared" {
		t.Errorf("first row = %v, want the measured app even at 0.0%%", got)
	}
	if got := rows[1]["app_name"]; got != "Pod App" {
		t.Errorf("second row = %v, want the unmeasured app last", got)
	}
}

// The app's own page reads the same absence: the metrics object is present —
// status, uptime and restarts are all real — with the two figures missing.
func TestAppDetail_UnmeasuredFiguresAreAbsent(t *testing.T) {
	srv, router, shared, isolated := newUnmeasuredEnv(t)

	var detail struct {
		Metrics map[string]any `json:"metrics"`
	}

	rec := getAs(t, srv, router, "root", "/api/v1/apps/"+isolated.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("app detail: %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Metrics == nil {
		t.Fatal("isolated app has no metrics object at all; it has three answerable fields")
	}
	if _, ok := detail.Metrics["cpu"]; ok {
		t.Error("isolated app detail carries a cpu key; it must be absent")
	}
	if _, ok := detail.Metrics["memory"]; ok {
		t.Error("isolated app detail carries a memory key; it must be absent")
	}
	if detail.Metrics["status"] != "online" {
		t.Errorf("status = %v, want online", detail.Metrics["status"])
	}

	// And the measuring backend is unchanged by any of this.
	rec = getAs(t, srv, router, "root", "/api/v1/apps/"+shared.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("app detail (shared): %d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if _, ok := detail.Metrics["cpu"]; !ok {
		t.Error("shared app lost its cpu key")
	}
	if detail.Metrics["memory"].(float64) != 128 {
		t.Errorf("memory = %v, want 128", detail.Metrics["memory"])
	}
}

// The case that produced the false zeros: a backend that tried to take a
// reading and could not. It is neither the isolated backend's "I do not
// measure this" nor a genuine idle app, and it must render as absence rather
// than as either — nothing about a wrong number is visible from outside, which
// is why this is asserted on the wire.
func TestProcesses_AReadingThatFailedIsAbsentToo(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	idle := &database.App{Slug: "idle-shared", Name: "Idle Shared", OwnerID: "root", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate, RunningMode: string(runtime.ModeShared)}
	if err := srv.db.CreateApp(idle); err != nil {
		t.Fatalf("create app: %v", err)
	}

	srv.rts = runtime.NewRuntimes(
		&erroredRuntime{MockRuntime: runtime.NewMockRuntime(nil), name: idle.ProcessName()},
		nil,
	)
	router := srv.Router(http.NotFoundHandler())

	rec := getAs(t, srv, router, "root", "/api/v1/system/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("processes: %d body=%s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec.Body.Bytes())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (body %s)", len(rows), rec.Body.String())
	}
	row := rows[0]

	if v, ok := row["cpu"]; ok {
		t.Errorf("a reading that failed carries a cpu key (%v); 0.0%% would claim the app is idle", v)
	}
	if v, ok := row["memory"]; ok {
		t.Errorf("a reading that failed carries a memory key (%v)", v)
	}
	// The word is the point of the row. `errored` is the shared vocabulary's
	// answer for a workload with no process, and the UI already renders it.
	if row["process_status"] != "errored" {
		t.Errorf("process_status = %v, want errored — the status is answerable even when the figures are not", row["process_status"])
	}
}
