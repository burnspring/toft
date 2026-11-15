package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

type presenceBody struct {
	WindowSeconds int    `json:"window_seconds"`
	Node          string `json:"node"`
	Count         int    `json:"count"`
	Users         []struct {
		ID      string `json:"id"`
		Login   string `json:"login"`
		AppID   string `json:"app_id"`
		AppName string `json:"app_name"`
		InApp   bool   `json:"in_app"`
	} `json:"users"`
}

type processesBody struct {
	Node      string `json:"node"`
	Count     int    `json:"count"`
	Degraded  string `json:"degraded"`
	Sleeping  int    `json:"sleeping"`
	Stopped   int    `json:"stopped"`
	Processes []struct {
		AppID         string  `json:"app_id"`
		AppName       string  `json:"app_name"`
		Release       string  `json:"release"`
		Mode          string  `json:"mode"`
		OwnerLogin    string  `json:"owner_login"`
		Status        string  `json:"status"`
		ProcessStatus string  `json:"process_status"`
		CPU           float64 `json:"cpu"`
		Uptime        int64   `json:"uptime"`
		Restarts      int     `json:"restarts"`
		Host          string  `json:"host"`
	} `json:"processes"`
}

// Both endpoints describe the whole estate — who is signed in, and every app
// that is running regardless of who may read it. That is precisely the
// enumeration a non-admin must not get.
func TestPresenceAndProcesses_AdminOnly(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "publisher", database.SystemRolePublisher)

	for _, path := range []string{"/api/v1/system/presence", "/api/v1/system/processes"} {
		if rec := getAs(t, srv, router, "", path); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous on %s: %d, want 401", path, rec.Code)
		}
		if rec := getAs(t, srv, router, "publisher", path); rec.Code != http.StatusForbidden {
			t.Errorf("publisher on %s: %d, want 403", path, rec.Code)
		}
		if rec := getAs(t, srv, router, "admin", path); rec.Code != http.StatusOK {
			t.Errorf("admin on %s: %d, want 200 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// Presence is fed by the platform API too, not only the apps gateway: the
// admin's own request is what puts them in the answer.
func TestPresence_CountsPlatformActivity(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "publisher", database.SystemRolePublisher)

	// A publisher browsing Toft itself — no app involved.
	if rec := getAs(t, srv, router, "publisher", "/api/v1/apps"); rec.Code != http.StatusOK {
		t.Fatalf("publisher listing apps: %d", rec.Code)
	}

	rec := getAs(t, srv, router, "admin", "/api/v1/system/presence")
	if rec.Code != http.StatusOK {
		t.Fatalf("presence: %d body=%s", rec.Code, rec.Body.String())
	}
	var body presenceBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.WindowSeconds != 300 {
		t.Errorf("default window = %d, want 300", body.WindowSeconds)
	}
	if body.Node == "" {
		t.Error("no node in the answer; presence is per-node and has to say whose view it is")
	}

	online := map[string]bool{}
	for _, u := range body.Users {
		online[u.Login] = true
		if u.AppID != "" {
			t.Errorf("%s reports app %q without having opened one", u.Login, u.AppID)
		}
	}
	if !online["publisher"] {
		t.Error("publisher browsed the platform but is not online")
	}
	if !online["admin"] {
		t.Error("the admin's own request did not register as presence")
	}
	if body.Count != len(body.Users) {
		t.Errorf("count = %d with %d users", body.Count, len(body.Users))
	}
}

// A window outside what the tracker remembers is refused rather than answered
// from a shorter memory.
func TestPresence_RejectsOutOfRangeWindow(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)

	for _, q := range []string{"?window=5", "?window=99999", "?window=abc"} {
		rec := getAs(t, srv, router, "admin", "/api/v1/system/presence"+q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("window %s: %d, want 400", q, rec.Code)
		}
	}
	if rec := getAs(t, srv, router, "admin", "/api/v1/system/presence?window=1800"); rec.Code != http.StatusOK {
		t.Errorf("window=1800 (retention): %d, want 200", rec.Code)
	}
}

// The process list is the running estate: what the backend reports, plus
// anything recorded as running that it has never heard of. A stopped app that is
// known to be stopped is not a process.
func TestProcesses_ListsRunningAndMissingWorkloads(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	owner := seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	running := &database.App{
		Slug: "live-app", Name: "Live App", OwnerID: owner.ID,
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	stopped := &database.App{
		Slug: "idle-app", Name: "Idle App", OwnerID: owner.ID,
		Framework: "express", Status: database.AppStatusStopped,
		Visibility: database.VisibilityPrivate,
	}
	for _, a := range []*database.App{running, stopped} {
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
	}

	rec := getAs(t, srv, router, "admin", "/api/v1/system/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("processes: %d body=%s", rec.Code, rec.Body.String())
	}
	var body processesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || len(body.Processes) != 1 {
		t.Fatalf("got %d processes, want only the running app: %+v", len(body.Processes), body.Processes)
	}

	row := body.Processes[0]
	if row.AppID != running.ID || row.AppName != "Live App" {
		t.Errorf("row = %s/%s, want the running app", row.AppID, row.AppName)
	}
	if row.OwnerLogin != "owner" {
		t.Errorf("owner_login = %q, want owner", row.OwnerLogin)
	}
	if row.Mode != "shared" {
		t.Errorf("mode = %q, want shared", row.Mode)
	}
	if row.Host == "" {
		t.Error("a shared workload runs on this host and should name it")
	}
	// The mock runtime knows nothing of an app that was never deployed, so the
	// recorded status and the backend's answer disagree — which is the case this
	// page exists to make visible.
	if row.ProcessStatus != "missing" {
		t.Errorf("process_status = %q, want missing for an app the backend never started", row.ProcessStatus)
	}
	if row.Status != database.AppStatusRunning {
		t.Errorf("status = %q, want the recorded status preserved", row.Status)
	}
}

// The case a mock that dropped its stopped entries could never show, and the
// reason a page headed "Running workloads" listed fifty of them on a real box:
// `pm2 stop` keeps its entry so a later start finds the port it held, so in
// shared mode every app anybody ever stopped — and every app the idle reaper
// ever slept — answered List() forever, with no pid, no readings, and the old
// start time still ticking up as an uptime. The isolated backend deletes the
// pod, so the same page meant two different things on the two backends.
//
// Nothing here is a policy this page applies. Both apps are down; the question
// is only whether the list believes the backend's word or the existence of its
// bookkeeping.
func TestProcesses_ABackendThatKeepsStoppedEntriesListsNone(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	owner := seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	asleep := &database.App{
		Slug: "slept-app", Name: "Slept App", OwnerID: owner.ID,
		Framework: "flask", Status: database.AppStatusSleeping,
		Visibility: database.VisibilityPrivate,
	}
	halted := &database.App{
		Slug: "halted-app", Name: "Halted App", OwnerID: owner.ID,
		Framework: "express", Status: database.AppStatusStopped,
		Visibility: database.VisibilityPrivate,
	}
	live := &database.App{
		Slug: "live-app", Name: "Live App", OwnerID: owner.ID,
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	for _, a := range []*database.App{asleep, halted, live} {
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
	}

	// Deployed, then stopped — which is what the reaper and the Stop button
	// both do, and what leaves the entry behind.
	rt, err := srv.rts.For(runtime.ModeShared)
	if err != nil {
		t.Fatalf("shared runtime: %v", err)
	}
	for _, a := range []*database.App{asleep, halted, live} {
		if _, err := rt.Deploy(runtime.WorkloadSpec{AppID: a.ID, Slug: a.Slug, Name: a.ProcessName()}); err != nil {
			t.Fatalf("deploy %s: %v", a.Slug, err)
		}
	}
	for _, a := range []*database.App{asleep, halted} {
		if err := rt.Stop(a.ProcessName()); err != nil {
			t.Fatalf("stop %s: %v", a.Slug, err)
		}
	}

	rec := getAs(t, srv, router, "admin", "/api/v1/system/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("processes: %d body=%s", rec.Code, rec.Body.String())
	}
	var body processesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Count != 1 || len(body.Processes) != 1 {
		t.Fatalf("got %d rows, want only the live workload: %+v", len(body.Processes), body.Processes)
	}
	if body.Processes[0].AppID != live.ID {
		t.Errorf("row = %s, want the running app", body.Processes[0].AppName)
	}
	// Counted, not listed. A page that simply dropped them would read as an
	// app having vanished from the estate.
	if body.Sleeping != 1 {
		t.Errorf("sleeping = %d, want 1", body.Sleeping)
	}
	if body.Stopped != 1 {
		t.Errorf("stopped = %d, want 1", body.Stopped)
	}
}

// The one row that is not up and still belongs: the app is recorded running
// and there is no process behind it. That disagreement is why this page exists,
// and it must survive the filter whichever way the backend words it — a name
// no backend holds at all, or an entry the backend itself calls stopped.
func TestProcesses_RecordedRunningWithNoProcessStillLists(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	app := &database.App{
		Slug: "drifted-app", Name: "Drifted App", OwnerID: "admin",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	rt, err := srv.rts.For(runtime.ModeShared)
	if err != nil {
		t.Fatalf("shared runtime: %v", err)
	}
	if _, err := rt.Deploy(runtime.WorkloadSpec{AppID: app.ID, Slug: app.Slug, Name: app.ProcessName()}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if err := rt.Stop(app.ProcessName()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	rec := getAs(t, srv, router, "admin", "/api/v1/system/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("processes: %d body=%s", rec.Code, rec.Body.String())
	}
	var body processesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Processes) != 1 {
		t.Fatalf("got %d rows, want the drifted app: %+v", len(body.Processes), body.Processes)
	}
	row := body.Processes[0]
	if row.ProcessStatus != "stopped" {
		t.Errorf("process_status = %q, want the backend's own word", row.ProcessStatus)
	}
	if row.Status != database.AppStatusRunning {
		t.Errorf("status = %q, want the recorded status preserved", row.Status)
	}
	// The figure that used to grow every ten seconds for a workload that had
	// not existed in days.
	if row.Uptime != 0 {
		t.Errorf("uptime = %d, want 0 — nothing is up to have been up for a while", row.Uptime)
	}
	// Counted nowhere: it is on the list, so it is not something the list left
	// out.
	if body.Sleeping != 0 || body.Stopped != 0 {
		t.Errorf("sleeping/stopped = %d/%d, want 0/0", body.Sleeping, body.Stopped)
	}
}

// An app the health watcher has called crashed whose workload the backend is
// still restarting — held back between starts of a loop — is on the page. It
// is the workload somebody opened Admin → Processes to find, and before the
// agent backed off nothing was ever caught in that state long enough to miss.
func TestProcesses_ACrashLoopIsListedWhileCalledCrashed(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	app := &database.App{
		Slug: "flapping-app", Name: "Flapping App", OwnerID: "admin",
		Framework: "flask", Status: database.AppStatusCrashed,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	rt, err := srv.rts.For(runtime.ModeShared)
	if err != nil {
		t.Fatalf("shared runtime: %v", err)
	}
	if _, err := rt.Deploy(runtime.WorkloadSpec{AppID: app.ID, Slug: app.Slug, Name: app.ProcessName()}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	mock, ok := rt.(*runtime.MockRuntime)
	if !ok {
		t.Fatalf("shared runtime is %T, want the mock", rt)
	}
	mock.CrashLoop(app.ProcessName(), 6)

	rec := getAs(t, srv, router, "admin", "/api/v1/system/processes")
	if rec.Code != http.StatusOK {
		t.Fatalf("processes: %d body=%s", rec.Code, rec.Body.String())
	}
	var body processesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Processes) != 1 {
		t.Fatalf("got %d rows, want the looping app: %+v", len(body.Processes), body.Processes)
	}
	row := body.Processes[0]
	if row.Restarts != 6 || row.ProcessStatus != runtime.StatusErrored || row.Status != database.AppStatusCrashed {
		t.Errorf("row = restarts %d, process_status %q, status %q; want 6, errored, crashed",
			row.Restarts, row.ProcessStatus, row.Status)
	}
}

// The agent's figures are an administrator's, and absent — not an error —
// where the shared runtime is not the agent (here, the mock).
func TestSystemAgentIsAdminOnlyAndAbsentWithoutAnAgent(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "publisher", database.SystemRolePublisher)

	if rec := getAs(t, srv, router, "publisher", "/api/v1/system/agent"); rec.Code != http.StatusForbidden {
		t.Errorf("publisher: %d, want 403", rec.Code)
	}
	rec := getAs(t, srv, router, "admin", "/api/v1/system/agent")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not run by the agent") {
		t.Errorf("admin on a mock install: %d %s, want 404 saying why", rec.Code, rec.Body.String())
	}
}
