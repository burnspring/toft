package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"toft/internal/compliance"
	"toft/internal/database"
	"toft/internal/entitlements"
)

// **Every evidence item has a collector, and the list is the specification.**
//
// The first of the silent failures. An item added to the registry and forgotten
// here does not break anything: the report renders, the totals add up, and one
// row quietly tells an assessor that a piece of evidence could not be
// collected on this install. Nobody inside the company reads that row, because
// nobody inside the company reads the document — that is what it is for.
//
// It is a plain unit test with no database because the mapping it checks is
// between two compiled-in things.
func TestEveryEvidenceItemHasACollector(t *testing.T) {
	reg, err := compliance.Load()
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}
	for _, item := range reg.Evidence {
		if _, ok := collectors[item.ID]; !ok {
			t.Errorf("evidence %q has no collector in compliance_handlers.go. It would "+
				"report an error to an assessor on every run of this report.", item.ID)
		}
	}
	// And the other direction: a collector for an item nobody defines is dead
	// code that reads as live, which is what the keyroutes stale test exists to
	// prevent one file over.
	for id := range collectors {
		if reg.EvidenceItem(id) == nil {
			t.Errorf("a collector is registered for %q, which the evidence registry does "+
				"not define", id)
		}
	}
}

// **Only an administrator, and only through the platform's own page.**
//
// The three compliance routes answer 404 to everyone who is not system_admin,
// and are denied to every API key class by name — so what is left is a browser
// holding a session cookie. This asserts the first half directly; the key half
// is asserted by the walk in keyroutes_test.go, which fails the build on an
// unclassified route rather than needing a case here.
//
// 404 and not 403, on the house rule: a refusal that confirms the surface
// exists tells an unauthorized caller something about the install.
func TestComplianceRoutesAreAdminOnly(t *testing.T) {
	e := newTierEnv(t, entitlements.TierPaid)

	routes := []string{
		"/api/v1/admin/compliance",
		"/api/v1/admin/compliance/controls",
		"/api/v1/admin/compliance/report",
	}
	// publisher is the non-admin this fixture seeds; a viewer would answer the
	// same way, and the gate reads IsAdmin rather than enumerating roles.
	for _, who := range []string{"publisher"} {
		for _, route := range routes {
			rec := e.request(t, who, http.MethodGet, route, "")
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s GET %s = %d, want 404 — what an install is capable of is "+
					"infrastructure detail, and a 403 would confirm the surface exists",
					who, route, rec.Code)
			}
		}
	}
	for _, route := range routes {
		if rec := e.request(t, "admin-user", http.MethodGet, route, ""); rec.Code != http.StatusOK {
			t.Errorf("admin GET %s = %d, want 200 (%s)", route, rec.Code, rec.Body.String())
		}
	}
}

// The three routes are denied to every key class, stated here directly rather
// than left implicit in a large map — the same shape as
// TestCredentialRoutesAreDenied, and for a related reason: this is the whole
// install described in one response, and a token in a pipeline's environment
// has no business holding it.
func TestComplianceRoutesAreDeniedToEveryKey(t *testing.T) {
	for _, route := range []string{
		"GET /api/v1/admin/compliance",
		"GET /api/v1/admin/compliance/controls",
		"GET /api/v1/admin/compliance/report",
	} {
		if apiKeyAllowedRoutes[route] {
			t.Errorf("%s is reachable by API key", route)
		}
		if !apiKeyDeniedRoutes[route] {
			t.Errorf("%s is not explicitly denied — it would be refused by default, but "+
				"the refusal has to be a decision somebody took", route)
		}
	}
}

// The control document reads no install state.
//
// D2's property, asserted where it can actually be checked: the handler is
// served and its body is identical whatever the install is configured to do,
// because what Toft does about a requirement is a fact about the build.
// A later change that reaches for a setting inside this handler breaks it.
func TestTheControlDocumentIsAPropertyOfTheBuild(t *testing.T) {
	first := newTierEnv(t, entitlements.TierPaid)
	second := newTierEnv(t, entitlements.TierDemo)

	a := first.request(t, "admin-user", http.MethodGet, "/api/v1/admin/compliance/controls", "")
	b := second.request(t, "admin-user", http.MethodGet, "/api/v1/admin/compliance/controls", "")
	if a.Code != http.StatusOK || b.Code != http.StatusOK {
		t.Fatalf("controls: %d and %d, want 200 and 200", a.Code, b.Code)
	}
	if a.Body.String() != b.Body.String() {
		t.Error("the control document differs between two installs. It is a fact about " +
			"the build — what Toft does about a requirement is the same sentence " +
			"on every install of a version — and something in the handler has started " +
			"reading configuration")
	}
}

// **The report survives being serialized, item by item.**
//
// The third silent failure, and the one that actually happened while this was
// being written. `writeJSON` sets the status, writes the header and then
// discards `Encode`'s error — so a value the encoder cannot handle produces
// **HTTP 200 with an empty body**. Nothing logs, nothing 500s, and the page
// renders as though the install returned no evidence at all.
//
// The offender was `version.Revision`, which is a func rather than a string:
// passed by name it marshals as `func() string` and takes the whole document
// down with it. Any collector returning a struct with a channel, a func or a
// NaN does the same, and the failure is per-report rather than per-item, so one
// bad value silences all thirty-seven.
//
// It is asserted per item as well as over the whole report, because "the report
// is empty" is a much harder failure to attribute than "estate_reach does not
// marshal".
func TestEveryCollectorResultCanBeSerialized(t *testing.T) {
	e := newTierEnv(t, entitlements.TierPaid)
	reg, err := compliance.Load()
	if err != nil {
		t.Fatalf("loading the registry: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/compliance/report", nil)
	c := e.srv.newComplianceCollector(req)

	for _, item := range reg.Evidence {
		res := c.Collect(context.Background(), item)
		if _, err := json.Marshal(res); err != nil {
			t.Errorf("evidence %q does not serialize: %v.\nwriteJSON discards this error, "+
				"so in production it is a 200 with an empty body and no log line.",
				item.ID, err)
		}
	}

	rep := compliance.Assemble(context.Background(), reg, c)
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("the assembled report does not serialize: %v", err)
	}
	if len(body) < 1024 {
		t.Errorf("the report serialized to %d bytes, which is too small to be a reading "+
			"of %d evidence items", len(body), len(reg.Evidence))
	}
}

// The identity-provider evidence agrees with the login page it claims to
// mirror. Both read the merged list — environment-configured providers and
// rows — and count what Ready() offers, which is what GET /auth/providers
// renders. Reading the rows alone reported zero ways in on an install whose
// only SSO was the operator's environment, and counted a row that was enabled
// with no secret, which the login page hides.
func TestComplianceProviderEvidenceAgreesWithTheLoginPage(t *testing.T) {
	srv := newTestServer(t) // GitHub from the environment, local password as a row

	// A row that is enabled and half-configured: no secret, so not ready, so
	// not on the login page.
	half := &database.AuthProvider{
		ID: "okta", Kind: database.ProviderKindOIDC, DisplayName: "Okta",
		Enabled: true, ClientID: "client-id", Issuer: "https://okta.example.invalid",
		DefaultRole: database.SystemRoleViewer,
	}
	if err := srv.db.CreateAuthProvider(half, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin-1",
	}); err != nil {
		t.Fatal(err)
	}

	rr := getThroughRouter(t, srv, "/api/v1/auth/providers")
	var opts loginOptionsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &opts); err != nil {
		t.Fatal(err)
	}
	offered := len(opts.Providers)
	if offered == 0 {
		t.Fatal("the fixture offers no way in; the test needs at least the environment's GitHub")
	}

	c := &complianceCollector{s: srv, r: httptest.NewRequest(http.MethodGet, "/", nil)}
	pub := c.authProvidersPublic().Value.(map[string]any)
	if got := pub["ways_in_offered"]; got != offered {
		t.Errorf("ways_in_offered = %v, want %d — what the login page renders", got, offered)
	}

	all := c.authProviders().Value.(map[string]any)
	ids := map[string]bool{}
	for _, p := range all["providers"].([]map[string]any) {
		ids[p["id"].(string)] = true
	}
	if !ids["github"] {
		t.Error("the environment-configured provider is missing from the evidence")
	}
	if !ids["okta"] {
		t.Error("the half-configured row is missing from the inventory; it exists, it is just not ready")
	}
}

// Every field a settings-sourced evidence item names is a key of the settings
// document. projectSettings records a missing one as null, so a setting
// removed from the product left the report showing a retention dial as absent
// on every install — two of them, for as long as nobody read the report
// closely. This makes the next removal fail the build instead.
func TestSettingsEvidenceNamesOnlyFieldsThatExist(t *testing.T) {
	reg, err := compliance.Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(database.DefaultPlatformSettings())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for id, collect := range collectors {
		if !sameFunc(collect, (*complianceCollector).fromSettings) {
			continue
		}
		item := reg.EvidenceItem(id)
		if item == nil {
			t.Errorf("%s: a settings collector with no evidence item", id)
			continue
		}
		for _, f := range item.Source.Fields {
			if _, ok := doc[f]; !ok {
				t.Errorf("%s names %q, which the settings document does not carry", id, f)
			}
		}
	}
}

// sameFunc is whether two collectors are the same function.
func sameFunc(a, b func(*complianceCollector) compliance.Result) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// VN-8 (2026-11-15): the access review is keyed by account id. A login is not
// unique, and keyed by login one of two accounts sharing it was dropped.
func TestAccessReviewKeepsTwoAccountsSharingALogin(t *testing.T) {
	e := newTierEnv(t, entitlements.TierPaid)
	for _, u := range []struct{ id, role string }{
		{"dup-a", database.SystemRoleAdmin},
		{"dup-b", database.SystemRolePublisher},
	} {
		if err := e.db.UpsertUser(&database.User{ID: u.id, Login: "same-login"}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.UpdateUserRole(u.id, u.role); err != nil {
			t.Fatal(err)
		}
	}

	c := &complianceCollector{s: e.srv, r: httptest.NewRequest(http.MethodGet, "/", nil)}
	out := c.accessReview().Value.(map[string]any)
	roles := map[string]bool{}
	for _, v := range out {
		entry := v.(map[string]any)
		if entry["login"] == "same-login" {
			roles[entry["role"].(string)] = true
		}
	}
	if !roles[database.SystemRoleAdmin] || !roles[database.SystemRolePublisher] {
		t.Errorf("entries for the shared login carry roles %v, want both accounts", roles)
	}
}

// VN-10 (2026-11-15): the report and its export name the install, not only a
// hostname that changes with every pod.
func TestComplianceReportCarriesTheInstallID(t *testing.T) {
	e := newTierEnv(t, entitlements.TierPaid)
	want, err := e.db.InstallID()
	if err != nil || want == "" {
		t.Fatalf("InstallID = %q, %v", want, err)
	}

	rec := e.request(t, "admin-user", http.MethodGet, "/api/v1/admin/compliance/report", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("report = %d %s", rec.Code, rec.Body.String())
	}
	var rep struct {
		InstallID string `json:"install_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.InstallID != want {
		t.Errorf("report install_id = %q, want %q", rep.InstallID, want)
	}

	rec = e.request(t, "admin-user", http.MethodGet, "/api/v1/admin/compliance/report/export", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d %s", rec.Code, rec.Body.String())
	}
	var exp struct {
		Report struct {
			InstallID string `json:"install_id"`
		} `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &exp); err != nil {
		t.Fatal(err)
	}
	if exp.Report.InstallID != want {
		t.Errorf("export install_id = %q, want %q", exp.Report.InstallID, want)
	}
}
