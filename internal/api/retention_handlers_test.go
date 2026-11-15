package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

// The two halves of release retention, from the API's side: what an owner may
// set on their own app, and what only an administrator may.

// A number above the platform maximum is stored as asked rather than refused
// or clamped. This is the requirement stated as "if app settings conflict it
// is shown, not rejected", and it is also what makes the conflict recoverable:
// raise the cap and the owner's figure applies again, with nobody re-entering
// it.
func TestOverCapKeepIsStoredAndReportedNotRejected(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	if err := srv.db.SetPlatformSettings(
		&database.PlatformSettings{MaxVersions: 5}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("setting the cap: %v", err)
	}

	rec := patchAppAs(t, srv, router, "owner", app.ID, `{"releases_kept":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH releases_kept = %d, want 200 — an over-cap figure must not be refused: %s",
			rec.Code, rec.Body.String())
	}

	stored, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ReleasesKept != 10 {
		t.Errorf("stored releases_kept = %d, want the 10 the owner asked for", stored.ReleasesKept)
	}

	policy := appPolicyOf(t, srv, router, "owner", app.ID)
	if !policy.KeepConflict {
		t.Error("the conflict was not reported, so the settings page has nothing to show")
	}
	if policy.EffectiveKeep != 5 {
		t.Errorf("effective keep = %d, want the platform maximum of 5", policy.EffectiveKeep)
	}
	if policy.RequestedKeep != 10 {
		t.Errorf("requested keep = %d, want 10", policy.RequestedKeep)
	}
}

// The sleep exemption is the administrator's, like execution mode. An owner
// who could exempt their own app would make idle sleep advisory.
func TestOnlyAnAdminMayChangeTheSleepExemption(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	if rec := patchAppAs(t, srv, router, "owner", app.ID, `{"never_sleep":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("owner PATCH never_sleep = %d, want 403", rec.Code)
	}
	if stored, _ := srv.db.GetApp(app.ID); stored.NeverSleep {
		t.Fatal("a refused request still exempted the app")
	}

	if rec := patchAppAs(t, srv, router, "root", app.ID, `{"never_sleep":true}`); rec.Code != http.StatusOK {
		t.Fatalf("admin PATCH never_sleep = %d, want 200", rec.Code)
	}
	stored, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.NeverSleep {
		t.Error("the administrator's exemption was not stored")
	}
}

// Requirement three: the owner sees what the administrator decided, and the
// reason, without being able to change it. A read-only setting nobody can see
// is indistinguishable from the platform misbehaving.
func TestOwnerSeesTheAdminSetPolicy(t *testing.T) {
	srv, router, app := modeTestEnv(t)

	if err := srv.db.SetPlatformSettings(
		&database.PlatformSettings{MaxVersions: 5, IdleSleepMinutes: 30}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("setting policy: %v", err)
	}
	if rec := patchAppAs(t, srv, router, "root", app.ID, `{"never_sleep":true}`); rec.Code != http.StatusOK {
		t.Fatalf("exempting the app: %d", rec.Code)
	}

	policy := appPolicyOf(t, srv, router, "owner", app.ID)
	if policy.MaxVersions != 5 {
		t.Errorf("max versions = %d, want 5 — the owner cannot see the cap they are held to", policy.MaxVersions)
	}
	if policy.IdleSleepMinutes != 30 {
		t.Errorf("idle sleep minutes = %d, want 30", policy.IdleSleepMinutes)
	}
	if policy.Sleeps {
		t.Error("the app is exempt but its policy says it sleeps")
	}
	// Which exemption it is, not merely that there is one: an owner who
	// disables their last schedule needs to know the app will start sleeping,
	// and one held awake by an administrator needs to know they cannot change
	// it.
	if policy.SleepExemption != "admin" {
		t.Errorf("sleep exemption = %q, want %q", policy.SleepExemption, "admin")
	}
}

// The platform's own settings document stays admin-only. The owner's view is
// their app's policy, not the shape of the install — see handleGetSettings.
func TestPolicyDoesNotOpenPlatformSettingsToOwners(t *testing.T) {
	srv, router, _ := modeTestEnv(t)

	rec := getAs(t, srv, router, "owner", "/api/v1/system/settings")
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /system/settings as an owner = %d, want 404", rec.Code)
	}
}

// appPolicyOf reads the policy block off GET /apps/{id}.
func appPolicyOf(t *testing.T, srv *Server, router http.Handler, who, appID string) appPolicyResponse {
	t.Helper()

	rec := getAs(t, srv, router, who, "/api/v1/apps/"+appID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET app as %s = %d: %s", who, rec.Code, rec.Body.String())
	}

	var body struct {
		Policy appPolicyResponse `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the app response: %v", err)
	}
	return body.Policy
}

// A version whose release has been reclaimed is not listed. It cannot be made
// current, downloaded, or rolled back to — every action the row offers ends in
// 410 — so listing it describes history rather than versions.
func TestReclaimedVersionsAreNotListed(t *testing.T) {
	e := newRollbackEnv(t)

	// newRollbackEnv gives every deployment a row and no release directory, so
	// goodDeploy is exactly the orphan case: successful, unprotected, gone.
	views := listDeployments(t, e)
	if _, ok := views[e.goodDeploy.ID]; ok {
		t.Error("a successful build with no release left is still listed")
	}

	// A failed build keeps its row: it never owned a release, and that row is
	// the only record of why the deploy failed. The auto-deploy poller's
	// blocked-push rows are these.
	if _, ok := views[e.failedDeploy.ID]; !ok {
		t.Error("a failed build was hidden — its row is the explanation")
	}
}

// The row recording the running deploy is never dropped, even though it owns no
// release directory of its own. A promote serves its *source's* release, so
// "no directory" and "release deleted" look identical from the filesystem —
// protection is what tells them apart.
func TestTheLiveRowSurvivesTheReclaimedFilter(t *testing.T) {
	e := newRollbackEnv(t)

	rollbackRow := &database.Deployment{
		AppID: e.app.ID, Status: database.DeployStatusSuccess,
		Branch: "upload", TriggeredBy: database.DeployTriggerRollback,
	}
	if err := e.srv.db.CreateDeployment(rollbackRow, nil); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	e.makeCurrent(t, rollbackRow.ID, e.goodDeploy.ID)

	views := listDeployments(t, e)
	if _, ok := views[rollbackRow.ID]; !ok {
		t.Fatal("the row recording the running deploy was filtered out as reclaimed")
	}
	if _, ok := views[e.goodDeploy.ID]; !ok {
		t.Error("the row whose release is actually being served was filtered out")
	}
}

func listDeployments(t *testing.T, e *rollbackEnv) map[string]deploymentView {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/"+e.app.ID+"/deployments", nil)
	req.AddCookie(sessionCookieFor(t, e.srv, "owner"))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("listing deployments = %d: %s", rec.Code, rec.Body.String())
	}

	var views []deploymentView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	byID := map[string]deploymentView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	return byID
}

// A new app stores releases_kept 0 ("no opinion") and keeps three. Its read
// says so in effective_releases_kept, on the full view and the brief one the
// CLI reads, so `releases_kept: 0` is not taken for "keeps none" (BB-025).
func TestANewAppReportsTheReleasesItKeeps(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	for _, path := range []string{"/api/v1/apps/" + app.ID, "/api/v1/apps/" + app.ID + "?view=brief"} {
		rec := getAs(t, srv, router, "owner", path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		var body struct {
			App map[string]any `json:"app"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if got := body.App["releases_kept"]; got != float64(0) {
			t.Errorf("%s releases_kept = %v, want the stored 0", path, got)
		}
		if got := body.App["effective_releases_kept"]; got != float64(database.DefaultReleasesKept) {
			t.Errorf("%s effective_releases_kept = %v, want %d", path, got, database.DefaultReleasesKept)
		}
	}
}

// The policy an app's page prints is the one for the mode it runs in, and the
// reason it is not slept names the mode where that is the reason. The order is
// the reaper's: a mode that is off is answered before the exemption.
func TestSleepPolicyReadsTheAppsOwnMode(t *testing.T) {
	shared := &database.App{RunningMode: string(runtime.ModeShared)}
	isolated := &database.App{RunningMode: string(runtime.ModeIsolated)}
	exempt := &database.App{RunningMode: string(runtime.ModeIsolated), NeverSleep: true}

	for _, tc := range []struct {
		name             string
		app              *database.App
		shared, isolated int
		sleeps           bool
		why              string
	}{
		{"nothing on", isolated, 0, 0, false, "disabled"},
		{"shared on, isolated app", isolated, 30, 0, false, "isolated"},
		{"isolated on, isolated app", isolated, 0, 60, true, ""},
		{"isolated on, shared app", shared, 0, 60, false, "disabled"},
		{"both on, shared app", shared, 30, 60, true, ""},
		{"isolated on, exempted", exempt, 0, 60, false, "admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := database.PlatformSettings{IdleSleepMinutes: tc.shared, IsolatedIdleSleepMinutes: tc.isolated}
			sleeps, why := (&Server{}).sleepPolicy(tc.app, settings)
			if sleeps != tc.sleeps || why != tc.why {
				t.Errorf("sleepPolicy = (%v, %q), want (%v, %q)", sleeps, why, tc.sleeps, tc.why)
			}
		})
	}
}
