package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
	"toft/internal/version"
)

type healthResponse struct {
	System  *runtime.SystemMetrics `json:"system"`
	Server  *serverInfo            `json:"server"`
	Summary map[string]int         `json:"summary"`
	Apps    []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"apps"`
}

// newHealthTestEnv builds an estate of three apps with different owners and
// visibilities, so "which apps does this caller see" is actually meaningful.
func newHealthTestEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()

	srv := newTestServer(t)
	srv.rts = runtime.NewRuntimes(runtime.NewMockRuntime(nil), nil)

	seedUser(t, srv.db, "alice", database.SystemRolePublisher)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for _, a := range []*database.App{
		{Slug: "alice-public", Name: "Alice Public", OwnerID: "alice", Visibility: database.VisibilityPublic},
		{Slug: "bob-secret", Name: "Bob Secret", OwnerID: "bob", Visibility: database.VisibilityPrivate},
		{Slug: "bob-internal", Name: "Bob Internal", OwnerID: "bob", Visibility: database.VisibilityAuthenticated},
	} {
		a.Framework = "flask"
		a.Status = database.AppStatusRunning
		if err := srv.db.CreateApp(a); err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
	}

	return srv, srv.Router(http.NotFoundHandler())
}

func decodeHealth(t *testing.T, body []byte) healthResponse {
	t.Helper()
	var h healthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("decode health: %v (body %q)", err, body)
	}
	return h
}

// /system/health returned every app, including private ones, to anyone who
// asked — no session required. Finding 0.2.
func TestSystemHealthRejectsAnonymousCallers(t *testing.T) {
	srv, router := newHealthTestEnv(t)

	rec := getAs(t, srv, router, "", "/api/v1/system/health")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "bob-secret") {
		t.Errorf("private app leaked to an anonymous caller: %q", body)
	}
}

// A publisher sees the apps they could already list via GET /apps — public,
// authenticated, and their own — and nothing else.
func TestSystemHealthScopesAppsToTheCaller(t *testing.T) {
	srv, router := newHealthTestEnv(t)
	rungOff(t, srv)

	rec := getAs(t, srv, router, "alice", "/api/v1/system/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	h := decodeHealth(t, rec.Body.Bytes())

	seen := map[string]bool{}
	for _, a := range h.Apps {
		seen[a.Slug] = true
	}
	if !seen["alice-public"] {
		t.Error("alice cannot see her own app")
	}
	if !seen["bob-internal"] {
		t.Error("alice cannot see an authenticated-visibility app")
	}
	if seen["bob-secret"] {
		t.Error("alice can see another publisher's private app")
	}
	if h.Summary["total"] != len(h.Apps) {
		t.Errorf("summary total = %d but %d apps returned; the count still leaks the estate size",
			h.Summary["total"], len(h.Apps))
	}
}

// Host CPU/memory/disk is infrastructure detail and is admin-only.
func TestSystemHealthGatesHostMetricsOnAdmin(t *testing.T) {
	srv, router := newHealthTestEnv(t)

	rec := getAs(t, srv, router, "alice", "/api/v1/system/health")
	if h := decodeHealth(t, rec.Body.Bytes()); h.System != nil {
		t.Errorf("host metrics returned to a non-admin: %+v", h.System)
	}

	rec = getAs(t, srv, router, "root", "/api/v1/system/health")
	if h := decodeHealth(t, rec.Body.Bytes()); h.System == nil {
		t.Error("host metrics withheld from an admin")
	}
}

// Hostname, OS, and versions are infrastructure detail like the host metrics:
// present for admins, absent for everyone else.
func TestSystemHealthGatesServerInfoOnAdmin(t *testing.T) {
	srv, router := newHealthTestEnv(t)

	rec := getAs(t, srv, router, "alice", "/api/v1/system/health")
	if h := decodeHealth(t, rec.Body.Bytes()); h.Server != nil {
		t.Errorf("server info returned to a non-admin: %+v", h.Server)
	}

	rec = getAs(t, srv, router, "root", "/api/v1/system/health")
	h := decodeHealth(t, rec.Body.Bytes())
	if h.Server == nil {
		t.Fatal("server info withheld from an admin")
	}
	if h.Server.GoVersion == "" || h.Server.OS == "" {
		t.Errorf("server info incomplete: %+v", h.Server)
	}
	// The published version is compiled in, so it is reported verbatim — a test
	// build has no VCS stamp and must still say "0.1.0", not "dev".
	if h.Server.Version != version.Version {
		t.Errorf("version = %q, want the compiled-in %q", h.Server.Version, version.Version)
	}
	if h.Server.StartedAt.IsZero() {
		t.Error("started_at is zero")
	}
	if len(h.Server.Runtimes) == 0 {
		t.Error("no runtime versions reported")
	}
}

func TestSystemHealthGivesAdminsTheWholeEstate(t *testing.T) {
	srv, router := newHealthTestEnv(t)

	rec := getAs(t, srv, router, "root", "/api/v1/system/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	h := decodeHealth(t, rec.Body.Bytes())
	if len(h.Apps) != 3 {
		t.Errorf("admin saw %d apps, want all 3", len(h.Apps))
	}
}

// A failed app is not a crashed one. The summary folded `failed` into
// `crashed`, so Admin home read "1 app crashed" over an app that had never
// started, and sent whoever believed it to an estate filter holding nothing —
// the estate list has had a tab for each word for as long as both have existed.
func TestSystemHealthCountsFailedApartFromCrashed(t *testing.T) {
	srv, router := newHealthTestEnv(t)

	for slug, status := range map[string]string{
		"alice-public": database.AppStatusCrashed,
		"bob-secret":   database.AppStatusFailed,
	} {
		app, err := srv.db.GetAppBySlug(slug)
		if err != nil {
			t.Fatalf("get %s: %v", slug, err)
		}
		if err := srv.db.UpdateAppStatus(app.ID, status); err != nil {
			t.Fatalf("set %s %s: %v", slug, status, err)
		}
	}

	rec := getAs(t, srv, router, "root", "/api/v1/system/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	h := decodeHealth(t, rec.Body.Bytes())
	if h.Summary["crashed"] != 1 {
		t.Errorf("crashed = %d, want 1 — the failed app must not be counted here", h.Summary["crashed"])
	}
	if h.Summary["failed"] != 1 {
		t.Errorf("failed = %d, want 1", h.Summary["failed"])
	}
}

// The list rung shows an app without opening it, so a caller who can only list
// an app is not handed its live CPU, memory, uptime and restarts.
func TestSystemHealthGivesReadingsOnlyForAppsTheCallerCanRead(t *testing.T) {
	srv, router := newHealthTestEnv(t)
	if _, err := srv.db.PutSetting(database.SettingAppsListableToAuthenticated, "true", ""); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"alice-public", "bob-secret"} {
		app, err := srv.db.GetAppBySlug(slug)
		if err != nil || app == nil {
			t.Fatalf("GetAppBySlug(%s): %v", slug, err)
		}
		if _, err := srv.rts.Shared().Deploy(runtime.WorkloadSpec{
			AppID: app.ID, Slug: app.Slug, Name: app.ProcessName(), Framework: app.Framework,
		}); err != nil {
			t.Fatalf("register mock process: %v", err)
		}
	}
	readings := func(who string) map[string]bool {
		rec := getAs(t, srv, router, who, "/api/v1/system/health")
		var h struct {
			Apps []struct {
				Slug    string          `json:"slug"`
				Metrics json.RawMessage `json:"metrics"`
			} `json:"apps"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := map[string]bool{}
		for _, a := range h.Apps {
			got[a.Slug] = len(a.Metrics) > 0
		}
		return got
	}

	alice := readings("alice")
	if has, listed := alice["bob-secret"]; !listed {
		t.Fatal("the list rung did not list bob-secret; the test asserts nothing")
	} else if has {
		t.Error("alice, who can only list bob-secret, was handed its live readings")
	}
	if !alice["alice-public"] {
		t.Error("alice got no readings for her own app")
	}
	if !readings("root")["bob-secret"] {
		t.Error("an administrator got no readings for bob-secret")
	}
}

// GET /apps had the same leak as /system/health: every listed app got its live
// readings attached, including the ones the caller holds only at the list rung.
func TestListAppsGivesReadingsOnlyForAppsTheCallerCanRead(t *testing.T) {
	srv, router := newHealthTestEnv(t)
	if _, err := srv.db.PutSetting(database.SettingAppsListableToAuthenticated, "true", ""); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"alice-public", "bob-secret"} {
		app, err := srv.db.GetAppBySlug(slug)
		if err != nil || app == nil {
			t.Fatalf("GetAppBySlug(%s): %v", slug, err)
		}
		if _, err := srv.rts.Shared().Deploy(runtime.WorkloadSpec{
			AppID: app.ID, Slug: app.Slug, Name: app.ProcessName(), Framework: app.Framework,
		}); err != nil {
			t.Fatalf("register mock process: %v", err)
		}
	}
	readings := func(who string) map[string]bool {
		rec := getAs(t, srv, router, who, "/api/v1/apps")
		var list []struct {
			Slug    string          `json:"slug"`
			Metrics json.RawMessage `json:"metrics"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
		}
		got := map[string]bool{}
		for _, a := range list {
			got[a.Slug] = len(a.Metrics) > 0 && string(a.Metrics) != "null"
		}
		return got
	}

	alice := readings("alice")
	if has, listed := alice["bob-secret"]; !listed {
		t.Fatal("the list rung did not list bob-secret; the test asserts nothing")
	} else if has {
		t.Error("alice, who can only list bob-secret, was handed its live readings")
	}
	if !alice["alice-public"] {
		t.Error("alice got no readings for her own app")
	}
	if !readings("root")["bob-secret"] {
		t.Error("an administrator got no readings for bob-secret")
	}
}
