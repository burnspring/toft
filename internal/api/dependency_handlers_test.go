package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/advisory"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/deps"
	"toft/internal/depsweep"
)

type depFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
	deploy *database.Deployment
}

func newDepFixture(t *testing.T, mode string) *depFixture {
	t.Helper()

	srv := newTestServer(t)
	srv.deps = &deps.Resolver{Store: srv.db}
	srv.depSweeper = depsweep.NewSweeper(srv.db, srv.deps, nil)
	f := &depFixture{srv: srv, router: srv.Router(http.NotFoundHandler())}

	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "viewer", database.SystemRoleViewer)
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)

	at := time.Now().UTC()
	if err := srv.db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-1", []advisory.Record{{
		ID: "GHSA-crit", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
		Summary: "Prototype pollution", URL: "https://example.com/a",
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemNPM, Package: "lodash",
			Range: advisory.Range{Introduced: "0", Fixed: "4.17.21"},
		}},
	}}, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 1, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	settings, err := srv.db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	settings.DependencyPolicy = mode
	if err := srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}

	f.app = &database.App{
		Slug: "recon", Name: "Reconciliation", OwnerID: "owner",
		Framework: "node", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := srv.db.CreateApp(f.app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	f.deploy = &database.Deployment{AppID: f.app.ID, Branch: "main",
		Status: database.DeployStatusSuccess}
	if err := srv.db.CreateDeployment(f.deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	f.deploy.Status = database.DeployStatusSuccess
	f.deploy.FinishedAt = &finished
	if err := srv.db.UpdateDeployment(f.deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if err := srv.db.SaveDeploymentDependencies(f.deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15",
			Path: "(this app) > webpack > lodash"},
		{Ecosystem: advisory.EcosystemNPM, Package: "express", Version: "4.18.2", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	if err := srv.db.SetDeploymentVerdict(f.deploy.ID, deps.Verdict{
		Allowed: true, Mode: mode, Scanned: true, Packages: 2,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}
	return f
}

func (f *depFixture) send(t *testing.T, method, who, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, _ := json.Marshal(body)
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, f.srv, who))
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// A viewer can do nothing with a finding, and an exact package-and-version
// manifest of a running internal tool is a target map. The same reasoning
// already keeps failure events off a viewer's Logs tab.
func TestDependencyTabRequiresCanEdit(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	if rec := getAs(t, f.srv, f.router, "viewer",
		"/api/v1/apps/"+f.app.ID+"/dependencies"); rec.Code == http.StatusOK {
		t.Errorf("a viewer should not read the findings, got %d", rec.Code)
	}
	if rec := getAs(t, f.srv, f.router, "owner",
		"/api/v1/apps/"+f.app.ID+"/dependencies"); rec.Code != http.StatusOK {
		t.Errorf("the owner should read them, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDependencyTabCarriesFindingsScopedToTheFeed(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out dependencyStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !out.Available || out.Mode != database.DependencyPolicyWarn {
		t.Fatalf("out = %+v", out)
	}
	// Every number is scoped to the corpus it was computed against.
	if out.FeedAsOf == nil || !out.HasData {
		t.Errorf("the feed's age should ride on the response: %+v", out)
	}
	if !out.Scanned || out.Serving == nil {
		t.Fatalf("the serving release should be present: %+v", out)
	}
	if len(out.Serving.Findings) != 1 {
		t.Fatalf("findings = %+v", out.Serving.Findings)
	}
	fnd := out.Serving.Findings[0]
	if fnd.AdvisoryID != "GHSA-crit" || fnd.FixedIn != "4.17.21" {
		t.Errorf("finding = %+v", fnd)
	}
	// warn does not enforce, and the finding still says it *would* — which is
	// the whole content of the warning, and the reason the field is named
	// WouldBlock rather than Blocks. The install-level answer is `enforcing`.
	if out.Enforcing {
		t.Error("warn must not enforce")
	}
	// What the gate blocks on travels with the mode (BB-030, second
	// 2026-11-03 bash): "block" alone said nothing of the threshold.
	if out.BlockSeverity == "" || !strings.Contains(rec.Body.String(), `"block_only_fixable"`) {
		t.Errorf("the gate's threshold is missing: %s", rec.Body.String())
	}
	if !fnd.WouldBlock {
		t.Error("the finding should say it would block if this install were blocking")
	}
	if out.Serving.Counts.Counted != 1 {
		t.Errorf("counts = %+v, want one counted", out.Serving.Counts)
	}
	// The inventory is there too, and its own summary.
	if len(out.Serving.Inventory) != 2 || out.Serving.Direct != 1 {
		t.Errorf("inventory = %+v direct=%d", out.Serving.Inventory, out.Serving.Direct)
	}
}

// Off means the surface says so rather than reporting a clean bill of health.
func TestDependencyTabWhileOff(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyOff)

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dependencyStateResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Available {
		t.Error("the feature should report itself unavailable")
	}
	if out.Reason == "" {
		t.Error("and say which of the four gates refused")
	}
	if out.Serving != nil {
		t.Error("nothing should be reported as scanned while it is off")
	}
}

// Accepting risk on the organization's behalf is not content editing.
func TestWaiversAreAdminOnly(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)
	body := map[string]any{
		"advisory_id": "GHSA-crit",
		"reason":      "no fix for our webpack major yet",
		"days":        30,
	}

	for _, who := range []string{"owner", "viewer"} {
		rec := f.send(t, http.MethodPost, who,
			"/api/v1/apps/"+f.app.ID+"/dependency-waivers", body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s granting a waiver = %d, want 404", who, rec.Code)
		}
	}

	rec := f.send(t, http.MethodPost, "admin1",
		"/api/v1/apps/"+f.app.ID+"/dependency-waivers", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin granting = %d: %s", rec.Code, rec.Body.String())
	}
	var waiver database.DependencyWaiver
	json.Unmarshal(rec.Body.Bytes(), &waiver)
	if waiver.ID == "" || waiver.ExpiresAt.IsZero() {
		t.Fatalf("waiver = %+v", waiver)
	}

	// It silences the refusal and stays on the record, visible to the owner
	// although only an administrator can grant one.
	get := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	var out dependencyStateResponse
	json.Unmarshal(get.Body.Bytes(), &out)
	if len(out.Waivers) != 1 {
		t.Errorf("the owner should see the waiver: %+v", out.Waivers)
	}
	if out.CanWaive {
		t.Error("but must not be offered the button")
	}
	if len(out.Serving.Findings) != 1 || !out.Serving.Findings[0].Waived {
		t.Errorf("the finding stays, marked: %+v", out.Serving.Findings)
	}
	if out.Serving.Findings[0].WouldBlock {
		t.Error("a waived finding does not block")
	}

	// And revoking is admin-only too, and scoped by app.
	if rec := f.send(t, http.MethodDelete, "owner",
		"/api/v1/apps/"+f.app.ID+"/dependency-waivers/"+waiver.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("owner revoking = %d, want 404", rec.Code)
	}
	if rec := f.send(t, http.MethodDelete, "admin1",
		"/api/v1/apps/"+f.app.ID+"/dependency-waivers/"+waiver.ID, nil); rec.Code != http.StatusNoContent {
		t.Errorf("admin revoking = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}

// A waiver with no reason is refused with a sentence a person can act on.
func TestWaiverNeedsAReason(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)
	rec := f.send(t, http.MethodPost, "admin1",
		"/api/v1/apps/"+f.app.ID+"/dependency-waivers",
		map[string]any{"advisory_id": "GHSA-crit", "days": 30})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reason") {
		t.Errorf("the refusal should name what is missing: %s", rec.Body.String())
	}
}

// Anyone with can_edit may re-check, and the copy is honest about what that is:
// no registry, no resolver, no re-reading of the manifest.
func TestRecheckNeedsCanEditAndReturnsTheState(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	if rec := f.send(t, http.MethodPost, "viewer",
		"/api/v1/apps/"+f.app.ID+"/dependency-recheck", nil); rec.Code == http.StatusOK {
		t.Errorf("a viewer should not re-check, got %d", rec.Code)
	}
	rec := f.send(t, http.MethodPost, "owner",
		"/api/v1/apps/"+f.app.ID+"/dependency-recheck", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out dependencyStateResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Serving == nil || len(out.Serving.Findings) != 1 {
		t.Errorf("a re-check should answer with the state: %+v", out)
	}
}

// The estate view, and the rule that a stopped app is separated rather than
// merged into the count.
func TestAdminEstateViewSeparatesStoppedApps(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)

	if rec := getAs(t, f.srv, f.router, "owner", "/api/v1/admin/dependencies"); rec.Code != http.StatusNotFound {
		t.Errorf("a non-admin gets 404, got %d", rec.Code)
	}

	rec := getAs(t, f.srv, f.router, "admin1", "/api/v1/admin/dependencies")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out adminDependencyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Apps) != 1 || !out.Apps[0].Serving {
		t.Fatalf("apps = %+v", out.Apps)
	}
	if out.Apps[0].Counts.Blocking != 1 {
		t.Errorf("counts = %+v, want one blocking under block mode", out.Apps[0].Counts)
	}
	// Per ecosystem, because they are fetched and swapped independently.
	if len(out.Feeds) != 1 || out.Feeds[0].Ecosystem != advisory.EcosystemNPM {
		t.Errorf("feeds = %+v", out.Feeds)
	}
	if out.CorpusSize != 1 {
		t.Errorf("corpus_size = %d", out.CorpusSize)
	}
	// The same findings grouped the other way, so the two axes cannot disagree.
	if len(out.Advisories) != 1 || out.Advisories[0].AdvisoryID != "GHSA-crit" {
		t.Fatalf("advisory groups = %+v", out.Advisories)
	}
	if out.Advisories[0].Serving != 1 || out.Advisories[0].Stopped != 0 {
		t.Errorf("a running app is exposure: %+v", out.Advisories[0])
	}

	if err := f.srv.db.UpdateAppStatus(f.app.ID, database.AppStatusStopped); err != nil {
		t.Fatalf("UpdateAppStatus: %v", err)
	}
	rec = getAs(t, f.srv, f.router, "admin1", "/api/v1/admin/dependencies")
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Apps) != 1 || out.Apps[0].Serving {
		t.Errorf("a stopped app is not exposure: %+v", out.Apps)
	}
}

// The search is by package name as well as by advisory id, because the news
// arrives as "there's something going round in event-stream" hours before
// anybody has an identifier.
func TestDependencySearch(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	for _, q := range []string{"GHSA-crit", "lodash"} {
		rec := getAs(t, f.srv, f.router, "admin1", "/api/v1/admin/dependencies/search?q="+q)
		if rec.Code != http.StatusOK {
			t.Fatalf("q=%s status = %d", q, rec.Code)
		}
		var out struct {
			Apps    []database.AffectedApp `json:"apps"`
			Serving int                    `json:"serving"`
			Stopped int                    `json:"stopped"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		if len(out.Apps) != 1 || out.Apps[0].Slug != "recon" {
			t.Errorf("q=%s apps = %+v", q, out.Apps)
		}
		// Counted apart, never summed.
		if out.Serving != 1 || out.Stopped != 0 {
			t.Errorf("q=%s serving=%d stopped=%d", q, out.Serving, out.Stopped)
		}
	}

	// An empty query is not an error and answers nothing rather than everything.
	rec := getAs(t, f.srv, f.router, "admin1", "/api/v1/admin/dependencies/search?q=")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"apps":[]`) {
		t.Errorf("an empty query should answer empty: %d %s", rec.Code, rec.Body.String())
	}
}

// The preview answers what a change *would* do, before saving — the whole
// content of the warn → block step.
func TestDependencyPreview(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	rec := f.send(t, http.MethodPost, "admin1", "/api/v1/admin/dependencies/preview",
		map[string]any{"dependency_block_severity": advisory.SeverityCritical})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Blocked []struct {
			Slug     string             `json:"slug"`
			Findings []advisory.Finding `json:"findings"`
		} `json:"blocked"`
		Unscanned int `json:"unscanned"`
		Total     int `json:"total"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Blocked) != 1 || out.Blocked[0].Slug != "recon" {
		t.Fatalf("blocked = %+v", out.Blocked)
	}
	if out.Total != 1 {
		t.Errorf("total = %d", out.Total)
	}

	// Moving the threshold the other way answers the reverse question.
	rec = f.send(t, http.MethodPost, "admin1", "/api/v1/admin/dependencies/preview",
		map[string]any{"dependency_block_severity": database.DependencySeverityOff,
			"dependency_block_malware": false})
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Blocked) != 0 {
		t.Errorf("with the threshold off nothing should block: %+v", out.Blocked)
	}
}

// The policy block rides GET /apps/{id}, and is absent for a viewer.
func TestPolicyBlockCarriesDependenciesForEditorsOnly(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Policy struct {
			Dependencies *appDependencyPolicy `json:"dependencies"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Policy.Dependencies == nil {
		t.Fatal("the policy block should carry the dependency section for an editor")
	}
	d := out.Policy.Dependencies
	if !d.Scanned || d.Counts.Blocking != 1 {
		t.Errorf("dependencies = %+v", d)
	}
	// The one wording, in a fifth place, read before somebody presses Redeploy.
	if !d.DeploysBlocked || !strings.Contains(d.BlockedReason, "GHSA-crit") {
		t.Errorf("the next deploy should be reported blocked and say why: %+v", d)
	}
	if d.Release != f.deploy.ID {
		t.Errorf("release = %q, want the serving one", d.Release)
	}

	// A viewer gets the app and not the manifest.
	//
	// Decoded into a fresh value on purpose: unmarshalling an absent key into a
	// reused struct leaves the previous pointer in place, so a shared `out`
	// would pass this assertion whatever the server sent.
	rec = getAs(t, f.srv, f.router, "viewer", "/api/v1/apps/"+f.app.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer status = %d", rec.Code)
	}
	var asViewer struct {
		Policy struct {
			Dependencies *appDependencyPolicy `json:"dependencies"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &asViewer); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if asViewer.Policy.Dependencies != nil {
		t.Error("a viewer must not receive the dependency section")
	}
}

// Malware earns its notice in every mode, including one whose threshold would
// not block it — there is no low-severity credential stealer.
func TestMalwareIsFlaggedEvenInWarn(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	at := time.Now().UTC()
	if err := f.srv.db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-2", []advisory.Record{{
		ID: "MAL-1", Kind: advisory.KindMalware, Severity: advisory.SeverityLow,
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemNPM, Package: "express",
			Range: advisory.Range{Versions: []string{"4.18.2"}},
		}},
	}}, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 1, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID)
	var out struct {
		Policy struct {
			Dependencies *appDependencyPolicy `json:"dependencies"`
		} `json:"policy"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Policy.Dependencies == nil || !out.Policy.Dependencies.MalwareServing {
		t.Fatalf("malware in the serving release must be flagged: %+v", out.Policy.Dependencies)
	}
	// Malware refuses in warn too (deps.Policy.Refuses, ADR §39), so the
	// notice says the next deploy will be refused — the same answer the gate
	// gives, read before somebody presses Redeploy.
	if !out.Policy.Dependencies.DeploysBlocked {
		t.Error("warn must report the next deploy blocked when malware is serving")
	}
	if out.Policy.Dependencies.Counts.Malware != 1 {
		t.Errorf("counts = %+v", out.Policy.Dependencies.Counts)
	}
}

// A release whose resolver read the manifest and gave up is not never_scanned:
// a redeploy of the same tree gives up the same way, and both the tab and the
// app's own policy block say so (docs/bugs 2026-10-28 #4, a marimo app whose
// pyyaml had no wheel for its Python).
func TestAnUnresolvedReleaseIsNotNeverScanned(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)
	if _, err := f.srv.db.Conn().Exec(
		`DELETE FROM deployment_dependencies WHERE deployment_id = ?`, f.deploy.ID); err != nil {
		t.Fatalf("clearing the inventory: %v", err)
	}
	if err := f.srv.db.SetDeploymentVerdict(f.deploy.ID, deps.Verdict{
		Allowed: true, Mode: database.DependencyPolicyWarn,
		Unread: []deps.Gap{{Kind: deps.GapUnresolved, Ecosystem: advisory.EcosystemPyPI,
			File: "requirements.txt", Reason: "pyyaml publishes no wheel for this Python"}},
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	var tab dependencyStateResponse
	json.Unmarshal(rec.Body.Bytes(), &tab)
	if tab.Scanned || tab.ScanReason != "unresolved" {
		t.Errorf("tab: scanned=%v scan_reason=%q, want unresolved", tab.Scanned, tab.ScanReason)
	}

	rec = getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID)
	var out struct {
		Policy struct {
			Dependencies *appDependencyPolicy `json:"dependencies"`
		} `json:"policy"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Policy.Dependencies == nil || out.Policy.Dependencies.ScanReason != "unresolved" {
		t.Errorf("app policy = %+v, want scan_reason unresolved", out.Policy.Dependencies)
	}
}

// An app with a release and no inventory is *unscanned*, which is a state with
// a name and never reads as zero findings.
func TestUnscannedIsItsOwnState(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)
	if _, err := f.srv.db.Conn().Exec(
		`DELETE FROM deployment_dependencies WHERE deployment_id = ?`, f.deploy.ID); err != nil {
		t.Fatalf("clearing the inventory: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	var out dependencyStateResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Scanned {
		t.Error("an app with no inventory is not scanned")
	}
	if out.ScanReason != "never_scanned" {
		t.Errorf("scan_reason = %q, want the cause a redeploy fixes", out.ScanReason)
	}

	// And the other cause, which a redeploy would not fix.
	settings, _ := f.srv.db.GetPlatformSettings()
	settings.DependencyScanOnDeploy = false
	if err := f.srv.db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
	rec = getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.ScanReason != "not_scanned_by_policy" {
		t.Errorf("scan_reason = %q, want the setting named", out.ScanReason)
	}
}

// **Recorded is not scanned, and the tab has to say both.**
//
// An R app with a renv.lock has a complete, exact package list and a corpus of
// fourteen advisories to check it against — CRAN is synced and deliberately
// absent from advisory.Ecosystems. So the inventory is shown in full and
// `scanned` stays false, under its own reason: `not_enumerable` would claim the
// packages could not be listed, when they are listed right beside the words,
// and a `scanned` derived from the row count would render "0 findings" — a
// statement about the feed wearing the appearance of a statement about the app.
func TestRInventoryIsRecordedAndNotScanned(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)

	app := &database.App{
		Slug: "chl-hypoxia", Name: "Hypoxia", OwnerID: "owner",
		Framework: "shiny", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deploy := &database.Deployment{AppID: app.ID, Branch: "main",
		Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	deploy.FinishedAt = &finished
	if err := f.srv.db.UpdateDeployment(deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if err := f.srv.db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "shiny", Version: "1.9.1", Direct: true},
		{Ecosystem: advisory.EcosystemCRAN, Package: "ggplot2", Version: "4.0.1", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	if err := f.srv.db.SetDeploymentVerdict(deploy.ID, deps.Verdict{
		Allowed: true, Mode: database.DependencyPolicyBlock, Scanned: false, Packages: 2,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+app.ID+"/dependencies")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out dependencyStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Scanned {
		t.Error("a CRAN-only release is not scanned")
	}
	if out.ScanReason != "not_covered" {
		t.Errorf("scan_reason = %q, want not_covered — the packages are listed, "+
			"so not_enumerable would be a status that is a lie", out.ScanReason)
	}
	// And the inventory is there to read, which is the whole point of
	// recording something nothing can be checked against.
	if out.Serving == nil || len(out.Serving.Inventory) != 2 {
		t.Fatalf("serving = %+v, want the two recorded packages", out.Serving)
	}
	if len(out.Serving.Ecosystems) != 1 || out.Serving.Ecosystems[0] != advisory.EcosystemCRAN {
		t.Errorf("ecosystems = %v, want [cran]", out.Serving.Ecosystems)
	}

	// The estate view agrees, and its row carries the count so an
	// administrator can tell "nothing recorded" from "recorded, not checked".
	admin := getAs(t, f.srv, f.router, "admin1", "/api/v1/admin/dependencies")
	var estate adminDependencyResponse
	if err := json.Unmarshal(admin.Body.Bytes(), &estate); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	var row *database.AppInventoryState
	for i := range estate.Apps {
		if estate.Apps[i].Slug == "chl-hypoxia" {
			row = &estate.Apps[i].AppInventoryState
		}
	}
	if row == nil {
		t.Fatalf("the R app is missing from the estate: %+v", estate.Apps)
	}
	if row.Scanned {
		t.Error("the estate must not count a CRAN-only release as scanned")
	}
	if row.Packages != 2 {
		t.Errorf("packages = %d, want the recorded count", row.Packages)
	}
}

// **A finding on a release that is not scanned**, which is the state the two
// lists exist to make expressible.
//
// `scanned` reports whether *silence* is a verdict; it has never reported
// whether anything was checked. So an R release matched against the CRAN corpus
// can carry a real advisory and still, correctly, read as not scanned — and the
// tab must hand the finding over rather than routing to an empty state on the
// strength of that flag.
func TestACRANFindingSurfacesOnAnUnscannedRelease(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)

	at := time.Now().UTC()
	if err := f.srv.db.ReplaceSnapshot(advisory.EcosystemCRAN, "snap-cran",
		[]advisory.Record{{
			ID: "RSEC-2023-3", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
			Summary: "jsonlite denial of service", URL: "https://example.com/rsec",
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemCRAN, Package: "jsonlite",
				Range: advisory.Range{Introduced: "0.9.12", Fixed: "1.8.8"},
			}},
		}}, advisory.FeedState{
			Ecosystem: advisory.EcosystemCRAN, LastSuccessAt: &at, SnapshotAt: &at,
			AdvisoryCount: 1, Origin: "sync",
		}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	app := &database.App{
		Slug: "chl-intake", Name: "Intake", OwnerID: "owner",
		Framework: "shiny", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deploy := &database.Deployment{AppID: app.ID, Branch: "main",
		Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	deploy.FinishedAt = &finished
	if err := f.srv.db.UpdateDeployment(deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	// A lock pinning a 2023 version, which is the realistic case: renv.lock
	// pins, so an app that has not been rebuilt sits where the advisory is.
	if err := f.srv.db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "shiny", Version: "1.9.1", Direct: true},
		{Ecosystem: advisory.EcosystemCRAN, Package: "jsonlite", Version: "1.8.4"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	if err := f.srv.db.SetDeploymentVerdict(deploy.ID, deps.Verdict{
		Allowed: true, Mode: database.DependencyPolicyBlock, Scanned: false, Packages: 2,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+app.ID+"/dependencies")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out dependencyStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	// Both halves, together. Either one alone is a status that is a lie.
	if out.Scanned {
		t.Error("a matched advisory does not make a CRAN-only release scanned")
	}
	if out.ScanReason != "not_covered" {
		t.Errorf("scan_reason = %q, want not_covered", out.ScanReason)
	}
	if out.Serving == nil || len(out.Serving.Findings) != 1 {
		t.Fatalf("the finding must reach the tab, got %+v", out.Serving)
	}
	got := out.Serving.Findings[0]
	if got.AdvisoryID != "RSEC-2023-3" || got.Package != "jsonlite" ||
		got.Ecosystem != advisory.EcosystemCRAN {
		t.Errorf("finding = %+v", got)
	}
	// And it is enforced like any other: nothing about a thin corpus makes the
	// records in it advisory-only.
	if !got.WouldBlock {
		t.Errorf("a critical CRAN finding with a fix blocks under this policy: %+v", got)
	}
	if out.Serving.Counts.Total != 1 || out.Serving.Counts.Blocking != 1 {
		t.Errorf("counts = %+v, want one blocking finding", out.Serving.Counts)
	}

	// The app list's glyph reads the same rows, so the two cannot disagree.
	alerts, err := f.srv.db.DependencyAlerts(deps.Policy{
		Mode: database.DependencyPolicyBlock, Licensed: true,
		BlockSeverity: advisory.SeverityCritical, FeedAsOf: &at,
	}, []string{app.ID})
	if err != nil {
		t.Fatalf("DependencyAlerts: %v", err)
	}
	if a, ok := alerts[app.ID]; !ok {
		t.Error("an unscanned app with a finding still raises the list alert")
	} else if a.Severity != advisory.SeverityCritical || !a.Blocking {
		t.Errorf("alert = %+v, want a blocking critical (red)", a)
	}

	// Under warn the same critical is still counted, so the glyph still shows
	// — red, because the colour is the severity — and it is not blocking,
	// because what happens is the mode's.
	alerts, err = f.srv.db.DependencyAlerts(deps.Policy{
		Mode: database.DependencyPolicyWarn, Licensed: true,
		BlockSeverity: advisory.SeverityCritical, FeedAsOf: &at,
	}, []string{app.ID})
	if err != nil {
		t.Fatalf("DependencyAlerts: %v", err)
	}
	if a := alerts[app.ID]; a.Severity != advisory.SeverityCritical || a.Blocking {
		t.Errorf("warn alert = %+v, want critical and not blocking", a)
	}
}

// The neighbouring case, unchanged by the above: an R app with no lockfile
// records nothing, and a redeploy would not change that — so it keeps the
// reason whose copy says what to commit.
func TestRWithNoLockfileStaysNotEnumerable(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	app := &database.App{
		Slug: "bare-shiny", Name: "Bare", OwnerID: "owner",
		Framework: "shiny", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deploy := &database.Deployment{AppID: app.ID, Branch: "main",
		Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	deploy.FinishedAt = &finished
	if err := f.srv.db.UpdateDeployment(deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+app.ID+"/dependencies")
	var out dependencyStateResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Scanned || out.ScanReason != "not_enumerable" {
		t.Errorf("scanned=%v scan_reason=%q, want not_enumerable",
			out.Scanned, out.ScanReason)
	}
}

// TOFT-BB-111, answered by ADR §39 rather than by the published skill: an R
// app with no renv.lock built today records what Toft installs into every R
// release (the library's `platform` rows), so it is `not_covered` — the rows
// are listed, a CRAN finding on one of them is shown and enforced — and not
// `not_enumerable`, which the tab renders as "recorded no packages" with the
// inventory and its findings hidden. `not_enumerable` stays for a release that
// recorded nothing (TestRWithNoLockfileStaysNotEnumerable). The skill that
// said otherwise is what was wrong.
func TestRWithNoLockfileButPlatformPackagesIsNotCovered(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	app := &database.App{
		Slug: "bare-batch-r", Name: "Bare batch", OwnerID: "owner",
		Framework: string(builder.FrameworkBatchR), Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deploy := &database.Deployment{AppID: app.ID, Branch: "main",
		Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	deploy.FinishedAt = &finished
	if err := f.srv.db.UpdateDeployment(deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if err := f.srv.db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "jsonlite", Version: "1.8.9", Source: deps.SourcePlatform},
		{Ecosystem: advisory.EcosystemCRAN, Package: "curl", Version: "6.0.1", Source: deps.SourcePlatform},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+app.ID+"/dependencies")
	var out dependencyStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %d %s", rec.Code, rec.Body.String())
	}
	if out.Scanned || out.ScanReason != "not_covered" {
		t.Errorf("scanned=%v scan_reason=%q, want not_covered", out.Scanned, out.ScanReason)
	}
	if out.Serving == nil || len(out.Serving.Inventory) != 2 {
		t.Errorf("the platform packages are listed: %+v", out.Serving)
	}
}

// A CRAN finding the deploy gate enforces is named by the block preview and
// counted by the estate. Both readers skipped every app whose ecosystems are
// all uncovered -- right about what *absence* means there, wrong about a
// finding that is recorded (docs/bugs 2026-10-22-e2e-identity-automation #1).
func TestARecordedCRANFindingIsInThePreviewAndTheEstate(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)
	at := time.Now().UTC()
	if err := f.srv.db.ReplaceSnapshot(advisory.EcosystemCRAN, "snap-cran", []advisory.Record{{
		ID: "GHSA-cran-crit", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
		Summary: "R6 issue", URL: "https://example.com/r6",
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemCRAN, Package: "R6",
			Range: advisory.Range{Introduced: "0", Fixed: "9.9.9"},
		}},
	}}, advisory.FeedState{
		Ecosystem: advisory.EcosystemCRAN, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 1, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	app := &database.App{Slug: "r-report", Name: "R report", OwnerID: "owner",
		Framework: "shiny", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatal(err)
	}
	deploy := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(deploy, nil); err != nil {
		t.Fatal(err)
	}
	finished := time.Now()
	deploy.FinishedAt = &finished
	if err := f.srv.db.UpdateDeployment(deploy); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "R6", Version: "2.6.1", Direct: true},
	}); err != nil {
		t.Fatal(err)
	}

	rec := f.send(t, http.MethodPost, "admin1", "/api/v1/admin/dependencies/preview",
		map[string]any{"dependency_block_severity": advisory.SeverityCritical})
	var preview struct {
		Blocked []struct {
			Slug string `json:"slug"`
		} `json:"blocked"`
	}
	json.Unmarshal(rec.Body.Bytes(), &preview)
	named := false
	for _, b := range preview.Blocked {
		named = named || b.Slug == "r-report"
	}
	if !named {
		t.Errorf("the preview of block does not name the R app the gate would refuse: %s", rec.Body.String())
	}

	rec = getAs(t, f.srv, f.router, "admin1", "/api/v1/admin/dependencies")
	var estate struct {
		Apps []struct {
			Slug   string           `json:"slug"`
			Counts dependencyCounts `json:"counts"`
		} `json:"apps"`
	}
	json.Unmarshal(rec.Body.Bytes(), &estate)
	for _, a := range estate.Apps {
		if a.Slug == "r-report" && a.Counts.Total != 1 {
			t.Errorf("the estate counts %+v for the R app, want its one finding", a.Counts)
		}
	}
}

// A release whose deploy recorded a verdict with nothing unread and nothing
// resolved had no dependencies — a Node app with no packages, a Go module on
// the standard library — and is no_dependencies, not never_scanned beside a
// scanned_at (docs/bugs 2026-10-28 #6).
func TestAReleaseWithNothingToScanIsNoDependencies(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)
	if _, err := f.srv.db.Conn().Exec(
		`DELETE FROM deployment_dependencies WHERE deployment_id = ?`, f.deploy.ID); err != nil {
		t.Fatalf("clearing the inventory: %v", err)
	}
	if err := f.srv.db.SetDeploymentVerdict(f.deploy.ID, deps.Verdict{
		Allowed: true, Mode: database.DependencyPolicyWarn, Unread: []deps.Gap{},
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}
	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	var tab dependencyStateResponse
	json.Unmarshal(rec.Body.Bytes(), &tab)
	if tab.Scanned || tab.ScanReason != "no_dependencies" {
		t.Errorf("tab: scanned=%v scan_reason=%q, want no_dependencies", tab.Scanned, tab.ScanReason)
	}
}

// The estate's *Needs a decision* list is built from these two flags, so each
// pair carries the policy's own answer rather than the page re-deriving it.
func TestGroupByAdvisoryMarksWhatIsOverTheLine(t *testing.T) {
	policy := deps.Policy{BlockSeverity: advisory.SeverityHigh, BlockMalware: true}
	estate := []database.AffectedApp{{
		AppID: "a1", Slug: "one", Status: database.AppStatusRunning,
		Findings: []advisory.Finding{
			{AdvisoryID: "GHSA-high", Kind: advisory.KindVuln, Severity: advisory.SeverityHigh, WouldBlock: true},
			{AdvisoryID: "GHSA-low", Kind: advisory.KindVuln, Severity: advisory.SeverityLow},
			{AdvisoryID: "GHSA-waived", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical, Waived: true},
		},
	}}
	got := map[string]advisoryGroupApp{}
	for _, g := range groupByAdvisory(policy, estate) {
		got[g.AdvisoryID] = g.Apps[0]
	}
	if a := got["GHSA-high"]; !a.Counted || !a.WouldBlock {
		t.Errorf("a high finding under a high line: counted=%v would_block=%v", a.Counted, a.WouldBlock)
	}
	if got["GHSA-low"].Counted {
		t.Error("a low finding under a high line is not over it")
	}
	if got["GHSA-waived"].Counted {
		t.Error("a waived finding is never over the line")
	}
}

// An app that never deployed, whose one deploy the policy refused because the
// resolver gave up, reads unresolved — its refused deploy's record says why
// there is no inventory — not never_scanned (drills on trytoft 2026-11-02:
// marimo under block, pyyaml with no wheel for the install's Python).
func TestAnAppRefusedAsUnresolvedIsNotNeverScanned(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)
	if _, err := f.srv.db.Conn().Exec(`DELETE FROM deployments WHERE id = ?`, f.deploy.ID); err != nil {
		t.Fatalf("removing the serving release: %v", err)
	}
	refused := &database.Deployment{AppID: f.app.ID, Branch: "main", Status: database.DeployStatusFailed}
	if err := f.srv.db.CreateDeployment(refused, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	refused.Status = database.DeployStatusFailed
	refused.FinishedAt = &finished
	if err := f.srv.db.UpdateDeployment(refused); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if err := f.srv.db.SetDeploymentVerdict(refused.ID, deps.Verdict{
		Allowed: false, Mode: database.DependencyPolicyBlock, Reason: "REFUSED: requirements.txt could not be checked",
		Unread: []deps.Gap{{Kind: deps.GapUnresolved, Ecosystem: advisory.EcosystemPyPI,
			File: "requirements.txt", Reason: "pyyaml publishes no wheel for this Python"}},
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+f.app.ID+"/dependencies")
	var tab dependencyStateResponse
	json.Unmarshal(rec.Body.Bytes(), &tab)
	if tab.Scanned || tab.ScanReason != "unresolved" {
		t.Errorf("scanned=%v scan_reason=%q, want unresolved (%s)", tab.Scanned, tab.ScanReason, rec.Body.String())
	}
	if tab.Refused == nil || tab.Refused.DeploymentID != refused.ID {
		t.Errorf("refused = %+v, want the refused deploy", tab.Refused)
	}
}

// DG-5 (security review 2026-11-14): a Python release on an install whose npm
// corpus has loaded and PyPI's never has is listed and **not scanned** —
// `no_corpus` — on the tab and on the app list's policy block alike.
func TestAReleaseWithNoLoadedCorpusIsNotScanned(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyBlock)
	app := &database.App{
		Slug: "pyapp", Name: "Py", OwnerID: "owner",
		Framework: "fastapi", Status: database.AppStatusRunning,
		Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deploy := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess}
	if err := f.srv.db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	deploy.FinishedAt = &finished
	if err := f.srv.db.UpdateDeployment(deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if err := f.srv.db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemPyPI, Package: "flask", Version: "3.0.0", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	rec := getAs(t, f.srv, f.router, "owner", "/api/v1/apps/"+app.ID+"/dependencies")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out dependencyStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Scanned || out.ScanReason != "no_corpus" {
		t.Errorf("scanned=%v scan_reason=%q, want false and no_corpus", out.Scanned, out.ScanReason)
	}
	if out.Serving == nil || len(out.Serving.Inventory) != 1 {
		t.Errorf("serving = %+v, want the recorded package", out.Serving)
	}
	if p := f.srv.dependencyPolicyFor(app); p == nil || p.Scanned || p.ScanReason != "no_corpus" {
		t.Errorf("app-list policy block = %+v, want not scanned, no_corpus", p)
	}
	// The npm app beside it is still scanned: its corpus has loaded.
	if p := f.srv.dependencyPolicyFor(f.app); p == nil || !p.Scanned {
		t.Errorf("the npm app's block = %+v, want scanned", p)
	}
}
