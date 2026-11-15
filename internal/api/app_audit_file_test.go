package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/compliance"
	"toft/internal/database"
)

type auditFileFixture struct {
	srv    *Server
	router http.Handler
	app    *database.App
}

func newAuditFileFixture(t *testing.T) auditFileFixture {
	t.Helper()
	srv := newTestServer(t)
	enablePersonalKeys(t, srv, true)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "ed", database.SystemRolePublisher)
	seedUser(t, srv.db, "vee", database.SystemRoleViewer)
	app := &database.App{Slug: "claims", Name: "Claims", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"ed": database.ObjectRoleEditor, "vee": database.ObjectRoleViewer} {
		if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser, user, role, "owner",
			database.RequestMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	return auditFileFixture{srv, router, app}
}

func outcomes(t *testing.T, rec *httptest.ResponseRecorder) (map[string]compliance.Outcome, map[string]json.RawMessage) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("audit file = %d %s", rec.Code, rec.Body.String())
	}
	var f struct {
		Sections []struct {
			ID      string             `json:"id"`
			Outcome compliance.Outcome `json:"outcome"`
			Reason  string             `json:"reason"`
			Value   json.RawMessage    `json:"value"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	out, vals := map[string]compliance.Outcome{}, map[string]json.RawMessage{}
	for _, s := range f.Sections {
		out[s.ID], vals[s.ID] = s.Outcome, s.Value
	}
	return out, vals
}

// The pack never gives a caller what the mirrored route would refuse them,
// for any kind of caller, and a refused section's value is never read. The
// failure would be invisible: the page renders and the export digests.
func TestAppFileNeverExceedsTheCallersGates(t *testing.T) {
	e := newAuditFileFixture(t)
	path := "/api/v1/apps/" + e.app.ID + "/audit-file"

	if rec := getAs(t, e.srv, e.router, "vee", path); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer = %d, want 403", rec.Code)
	}
	for who, want := range map[string]map[string]compliance.Outcome{
		"ed":    {"viewers": compliance.OutcomeRefused, "activity": compliance.OutcomeRefused, "access": compliance.OutcomeOK, "releases": compliance.OutcomeOK},
		"owner": {"viewers": compliance.OutcomeOK, "activity": compliance.OutcomeRefused},
		"root":  {"viewers": compliance.OutcomeOK, "activity": compliance.OutcomeOK},
	} {
		got, _ := outcomes(t, getAs(t, e.srv, e.router, who, path))
		if len(got) != len(compliance.AppFileSections) {
			t.Errorf("%s got %d sections, want every one", who, len(got))
		}
		for key, w := range want {
			if got[key] != w {
				t.Errorf("%s: %s = %s, want %s", who, key, got[key], w)
			}
		}
	}

	// A personal key reads what its owner reads, and loses a section the
	// moment that section's route leaves the key list.
	secret := mintScopedKey(t, e.srv, "owner", "quarterly", database.UserKeyScopeFull, nil, false)
	byKey := func() map[string]compliance.Outcome {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		got, _ := outcomes(t, rec)
		return got
	}
	if got := byKey(); got["viewers"] != compliance.OutcomeOK {
		t.Errorf("the owner's key: viewers = %s, want ok", got["viewers"])
	}
	delete(apiKeyAllowedRoutes, "GET /api/v1/apps/{appID}/usage/viewers")
	defer func() { apiKeyAllowedRoutes["GET /api/v1/apps/{appID}/usage/viewers"] = true }()
	if got := byKey(); got["viewers"] != compliance.OutcomeRefused {
		t.Errorf("with its route off the key list, viewers = %s, want refused", got["viewers"])
	}
}

// Every section has a collector and every collector a section; every gated
// section mirrors a route this server serves.
func TestEveryAppFileSectionHasACollectorAndARealRoute(t *testing.T) {
	routes, err := RouteInventory()
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, r := range routes {
		served[r.Method+" "+r.Pattern] = true
	}
	keys := map[string]bool{}
	for _, sec := range compliance.AppFileSections {
		keys[sec.Key] = true
		if _, ok := appFileReads[sec.Key]; !ok {
			t.Errorf("section %s has no collector", sec.Key)
		}
		if sec.Mirrors != "" && !served[sec.Mirrors] {
			t.Errorf("section %s mirrors %s, which is no route", sec.Key, sec.Mirrors)
		}
	}
	for key := range appFileReads {
		if !keys[key] {
			t.Errorf("collector %s has no section", key)
		}
	}
}

// Every reach source and every table reach excludes is in the pack or named
// as excluded.
func TestAppFileCoversEveryReachSource(t *testing.T) {
	sections := map[string]bool{}
	for _, sec := range compliance.AppFileSections {
		sections[sec.Key] = true
	}
	check := func(key string) {
		v, ok := AppFileCoverage[key]
		if !ok {
			t.Errorf("%s is in neither the pack nor its exclusions", key)
		} else if !strings.HasPrefix(v, "excluded: ") && !sections[v] {
			t.Errorf("%s maps to %q, which is no section", key, v)
		}
	}
	for _, src := range database.ReachSources {
		check(src.Key)
	}
	for table := range database.NotReach {
		check(table)
	}
}

// A NULL granter is never blank, and the group-owner label is used only for
// the row it describes; a person-granted row names the person.
func TestAppFileExplainsEveryNullGrantedBy(t *testing.T) {
	e := newAuditFileFixture(t)
	if _, err := e.srv.db.Conn().Exec(`UPDATE object_grants SET granted_by = NULL WHERE object_id = ? AND principal_id = 'vee'`, e.app.ID); err != nil {
		t.Fatal(err)
	}
	_, vals := outcomes(t, getAs(t, e.srv, e.router, "root", "/api/v1/apps/"+e.app.ID+"/audit-file"))
	var access struct {
		Grants []map[string]any `json:"grants"`
	}
	if err := json.Unmarshal(vals["access"], &access); err != nil {
		t.Fatal(err)
	}
	by := map[string]any{}
	for _, g := range access.Grants {
		by[g["principal_id"].(string)] = g["granted_by"]
	}
	if by["vee"] != grantedByUnknown {
		t.Errorf("a NULL granter reads %v, want the unknown label", by["vee"])
	}
	if m, ok := by["ed"].(map[string]any); !ok || m["id"] != "owner" {
		t.Errorf("a person-granted row reads %v, want the owner", by["ed"])
	}
}

// Who started a build is named on that build, and a build with no record says
// not recorded.
func TestAppFileDeployerWhenRecorded(t *testing.T) {
	e := newAuditFileFixture(t)
	named := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusSuccess}
	if err := e.srv.db.CreateDeployment(named, &database.DeployRecord{ActorType: "user", ActorID: "ed", Via: "git"}); err != nil {
		t.Fatal(err)
	}
	old := &database.Deployment{AppID: e.app.ID, Status: database.DeployStatusSuccess}
	if err := e.srv.db.CreateDeployment(old, nil); err != nil {
		t.Fatal(err)
	}
	_, vals := outcomes(t, getAs(t, e.srv, e.router, "ed", "/api/v1/apps/"+e.app.ID+"/audit-file"))
	var rows []map[string]any
	if err := json.Unmarshal(vals["releases"], &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r["id"] {
		case named.ID:
			if m, ok := r["started_by"].(map[string]any); !ok || m["id"] != "ed" {
				t.Errorf("the attributed build says %v", r["started_by"])
			}
		case old.ID:
			if r["started_by"] != notRecorded {
				t.Errorf("the unattributed build says %v", r["started_by"])
			}
		}
	}
}

// No credential value, on the response bytes.
func TestAppFileCarriesNoCredentialValue(t *testing.T) {
	e := newAuditFileFixture(t)
	if err := e.srv.db.SetEnvVar(e.app.ID, "DB_PASSWORD", "hunter2-value", true); err != nil {
		t.Skipf("seeding an env var: %v", err)
	}
	rec := getAs(t, e.srv, e.router, "root", "/api/v1/apps/"+e.app.ID+"/audit-file/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2-value") {
		t.Error("a credential value is in the export")
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "claims-audit-") {
		t.Errorf("filename = %q", rec.Header().Get("Content-Disposition"))
	}
}

// A deploy key is refused both routes: it is owner on every app, and the
// pack per app would rebuild the estate read every key is refused.
func TestAppFileRoutesAreDeniedToDeployKeys(t *testing.T) {
	e := newAuditFileFixture(t)
	secret := mintDeployKey(t, e.srv, e.router, "ci")
	for _, p := range []string{"/audit-file", "/audit-file/export"} {
		if rec := asDeployKey(t, e.router, http.MethodGet, "/api/v1/apps/"+e.app.ID+p, secret, ""); rec.Code < 400 {
			t.Errorf("a deploy key on %s = %d, want a refusal", p, rec.Code)
		}
	}
	if !deployKeyReadRoutes["GET /api/v1/apps/{appID}/deployments/{deployID}/sbom"] {
		t.Error("the SBOM, which the pipeline reads, is off the deploy-key read list")
	}
}

// Every app-keyed table is removed with the app by a cascade, or named as left
// behind — and nothing is both.
func TestDeletionListMatchesTheSchema(t *testing.T) {
	e := newAuditFileFixture(t)
	keys, err := e.srv.db.AppForeignKeys()
	if err != nil {
		t.Fatal(err)
	}
	cascades := map[string]bool{}
	for _, k := range keys {
		if k.DeleteRule == "CASCADE" {
			cascades[k.Table] = true
		}
	}
	keyed, err := e.srv.db.AppKeyedTables()
	if err != nil {
		t.Fatal(err)
	}
	for table, column := range keyed {
		_, left := DeletionLeftBehind[table]
		switch {
		case cascades[table] && left:
			t.Errorf("%s cascades and is also listed as left behind", table)
		case !cascades[table] && !left && column == "app_id":
			t.Errorf("%s holds an app_id with no cascade and is not listed as left behind", table)
		}
	}
	for table := range DeletionLeftBehind {
		if _, ok := keyed[table]; !ok {
			t.Errorf("%s is listed as left behind and holds no app's rows", table)
		}
	}
}

// The third reader reads three actions on one app and nothing else.
func TestAppAttributionsIsBounded(t *testing.T) {
	e := newAuditFileFixture(t)
	for _, a := range []database.AuditEntry{
		{ActorType: "user", ActorID: "ed", Action: "app.stop", TargetType: "app", TargetID: e.app.ID},
		{ActorType: "user", ActorID: "ed", Action: database.ActionAppDeploy, TargetType: "app", TargetID: "other-app",
			After: `{"deployment_id": "d-other"}`},
	} {
		if err := e.srv.db.WriteAuditRow(a); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.srv.db.AppAttributions(e.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.Action != "app.create" && a.Action != database.ActionAppDeploy && a.Action != "app.release_promoted" {
			t.Errorf("read %s", a.Action)
		}
		if a.DeploymentID == "d-other" {
			t.Error("another app's row was read")
		}
	}
}

// VN-9 (2026-11-15): the activity section is one page, and when the page is
// full it says it was cut and how far back it reaches, rather than reporting
// complete_from over a list that is missing its older rows.
func TestAppFileSaysWhenTheActivityWasCut(t *testing.T) {
	e := newAuditFileFixture(t)
	activity := func() map[string]any {
		_, vals := outcomes(t, getAs(t, e.srv, e.router, "root", "/api/v1/apps/"+e.app.ID+"/audit-file"))
		var v map[string]any
		if err := json.Unmarshal(vals["activity"], &v); err != nil {
			t.Fatalf("activity section: %v (%s)", err, vals["activity"])
		}
		return v
	}

	if v := activity(); v["truncated"] != nil {
		t.Errorf("a short history says it was cut: %v", v["truncated"])
	}

	for i := 0; i < database.MaxActivityLimit+1; i++ {
		if err := e.srv.db.InsertAppEvent(&database.AppEvent{AppID: e.app.ID, Kind: "app_crashed",
			Severity: database.EventSeverityError, Source: database.EventSourceJobs,
			Summary: "crashed", Fingerprint: "f"}); err != nil {
			t.Fatal(err)
		}
	}
	v := activity()
	entries, _ := v["entries"].([]any)
	if len(entries) != database.MaxActivityLimit {
		t.Fatalf("entries = %d, want %d", len(entries), database.MaxActivityLimit)
	}
	if v["truncated"] != true {
		t.Errorf("truncated = %v, want true", v["truncated"])
	}
	last, _ := entries[len(entries)-1].(map[string]any)
	if v["oldest_shown"] == nil || v["oldest_shown"] != last["occurred_at"] {
		t.Errorf("oldest_shown = %v, want the last entry's time %v", v["oldest_shown"], last["occurred_at"])
	}
}
