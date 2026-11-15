package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/deps"
)

func sbomDeploy() *database.Deployment {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return &database.Deployment{ID: "d1a2b3c4-0000-0000-0000-000000000000", CommitSHA: "abc", Branch: "main",
		FinishedAt: &at, RuntimeLanguage: "node", RuntimeVersion: "22.11.0"}
}

func claimFor(claims []ecosystemClaim, eco string) string {
	for _, c := range claims {
		if c.Ecosystem == eco {
			return c.Aggregate
		}
	}
	return "absent"
}

// Nothing reads complete that the record does not support. One case per row
// of the PRD's table: the failure this guards is an SBOM a procurement team
// files as complete for a release whose resolution never ran.
func TestSBOMCompositionNeverClaimsMoreThanTheRecord(t *testing.T) {
	npm := deps.Dependency{Ecosystem: "npm", Package: "left-pad", Version: "1.3.0", Direct: true}
	cran := deps.Dependency{Ecosystem: "cran", Package: "dplyr", Version: "1.1.4", Direct: true}
	gaps := func(g ...deps.Gap) deps.Verdict { return deps.Verdict{Unread: append([]deps.Gap{}, g...)} }

	for _, tc := range []struct {
		name    string
		rec     sbomRecord
		overall string
		per     map[string]string
	}{
		{"static", sbomRecord{Static: true}, "complete", nil},
		{"no verdict", sbomRecord{Inventory: []deps.Dependency{npm}}, "unknown", nil},
		{"before gaps were recorded", sbomRecord{HasVerdict: true, Inventory: []deps.Dependency{npm}}, "unknown", map[string]string{"npm": "unknown"}},
		{"resolved, no gap", sbomRecord{HasVerdict: true, Verdict: gaps(), Inventory: []deps.Dependency{npm}}, "complete", map[string]string{"npm": "complete"}},
		{"R with renv.lock", sbomRecord{HasVerdict: true, Verdict: gaps(), Inventory: []deps.Dependency{cran}}, "complete", map[string]string{"cran": "complete"}},
		{"R without renv.lock", sbomRecord{HasVerdict: true, Verdict: gaps(), NotEnumerable: true}, "unknown", map[string]string{"cran": "unknown"}},
		{"unlocked", sbomRecord{HasVerdict: true, Verdict: gaps(deps.Gap{Kind: deps.GapUnlocked, Ecosystem: "npm"}), Inventory: []deps.Dependency{npm}}, "unknown", map[string]string{"npm": "unknown"}},
		{"foreign lock", sbomRecord{HasVerdict: true, Verdict: gaps(deps.Gap{Kind: deps.GapForeignLock, Ecosystem: "npm", File: "pnpm-lock.yaml"}), Inventory: []deps.Dependency{npm}}, "unknown", map[string]string{"npm": "unknown"}},
		{"vendored", sbomRecord{HasVerdict: true, Verdict: gaps(deps.Gap{Kind: deps.GapVendored, File: "node_modules"}), Inventory: []deps.Dependency{npm}}, "incomplete", map[string]string{"npm": "incomplete"}},
		{"unresolved beside a good tree", sbomRecord{HasVerdict: true, Verdict: gaps(deps.Gap{Kind: deps.GapUnresolved, Ecosystem: "go"}), Inventory: []deps.Dependency{npm}}, "unknown", map[string]string{"npm": "complete", "go": "unknown"}},
		{"direct only", sbomRecord{HasVerdict: true, Verdict: gaps(deps.Gap{Kind: deps.GapDirectOnly, Ecosystem: "pypi", File: "requirements.txt"}), Inventory: []deps.Dependency{{Ecosystem: "pypi", Package: "flask", Version: "3.0.0"}}}, "incomplete", map[string]string{"pypi": "incomplete"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, overall, _ := sbomComposition(tc.rec)
			if overall != tc.overall {
				t.Errorf("overall = %s, want %s", overall, tc.overall)
			}
			for eco, want := range tc.per {
				if got := claimFor(claims, eco); got != want {
					t.Errorf("%s = %s, want %s", eco, got, want)
				}
			}
		})
	}
}

// A Python release read from a hashed requirements.txt is complete, and says
// which file it was complete from; one resolved from an unhashed file is not.
func TestSBOMNamesTheHashedRequirementsFile(t *testing.T) {
	flask := deps.Dependency{Ecosystem: "pypi", Package: "flask", Version: "3.1.0", Direct: true}
	reason := func(rec sbomRecord) string {
		claims, _, _ := sbomComposition(rec)
		for _, c := range claims {
			if c.Ecosystem == "pypi" {
				return c.Aggregate + ": " + c.Reason
			}
		}
		return ""
	}
	hashed := sbomRecord{HasVerdict: true, Inventory: []deps.Dependency{flask},
		Verdict: deps.Verdict{Unread: []deps.Gap{}, HashedRequirements: true}}
	if got, want := reason(hashed), "complete: requirements.txt pins and hashes every package"; got != want {
		t.Errorf("hashed = %q, want %q", got, want)
	}
	unlocked := sbomRecord{HasVerdict: true, Inventory: []deps.Dependency{flask},
		Verdict: deps.Verdict{Unread: []deps.Gap{{Kind: deps.GapUnlocked, Ecosystem: "pypi"}}}}
	if got := reason(unlocked); !strings.HasPrefix(got, "unknown: no lockfile") {
		t.Errorf("unlocked = %q", got)
	}
}

// Every inventory component says where it came from: empty is resolved, and an
// R release's platform rows are told apart from the app's own.
func TestSBOMComponentsCarryTheirSource(t *testing.T) {
	rec := sbomRecord{AppID: "app-1", Deployment: sbomDeploy(), HasVerdict: true, Verdict: deps.Verdict{Unread: []deps.Gap{}},
		Inventory: []deps.Dependency{
			{Ecosystem: "npm", Package: "left-pad", Version: "1.3.0", Direct: true},
			{Ecosystem: "cran", Package: "renv", Version: "1.0.7", Source: deps.SourcePlatform},
			{Ecosystem: "cran", Package: "dplyr", Version: "1.1.4", Source: deps.SourceInstalled, Path: "(this app) > dplyr"},
		}}
	want := map[string]string{"left-pad": "resolved", "renv": "platform", "dplyr": "installed"}
	for _, c := range renderSBOM(rec).Components {
		if c.Type != "library" {
			continue
		}
		got := ""
		for _, p := range c.Properties {
			if p.Name == "toft:source" {
				got = p.Value
			}
		}
		if got != want[c.Name] {
			t.Errorf("%s toft:source = %q, want %q (properties %+v)", c.Name, got, want[c.Name], c.Properties)
		}
	}
}

// Same release, same bytes — and an app rename changes nothing, because the
// name is not in it.
func TestSBOMIsDeterministic(t *testing.T) {
	rec := sbomRecord{AppID: "app-1", Deployment: sbomDeploy(), HasVerdict: true, Verdict: deps.Verdict{Unread: []deps.Gap{}},
		Inventory: []deps.Dependency{
			{Ecosystem: "npm", Package: "zeta", Version: "1.0.0"},
			{Ecosystem: "npm", Package: "@scope/alpha", Version: "2.0.0", Direct: true, Dev: true},
		}}
	a, _ := json.Marshal(renderSBOM(rec))
	b, _ := json.Marshal(renderSBOM(rec))
	if string(a) != string(b) {
		t.Fatal("two renders of one release differ")
	}
	var bom cdxBOM
	if err := json.Unmarshal(a, &bom); err != nil {
		t.Fatal(err)
	}
	if bom.SpecVersion != "1.6" || !strings.HasPrefix(bom.SerialNumber, "urn:uuid:") || bom.Metadata.Timestamp != "2026-10-01T12:00:00Z" {
		t.Errorf("header = %s %s %s", bom.SpecVersion, bom.SerialNumber, bom.Metadata.Timestamp)
	}
	if bom.Components[0].Name != "@scope/alpha" || bom.Components[0].Purl != "pkg:npm/%40scope/alpha@2.0.0" || bom.Components[0].Scope != "optional" {
		t.Errorf("first component = %+v", bom.Components[0])
	}
	if !strings.Contains(string(a), `"toft:sbom_format"`) || strings.Contains(string(a), "\"name\":\"Reports\"") {
		t.Error("the format property is missing, or the app's name leaked in")
	}
}

// The deployment id is scoped by the app in the URL.
func TestSBOMIsScopedToItsApp(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	other := &database.App{Slug: "other", Name: "Other", OwnerID: app.OwnerID, Status: database.AppStatusRunning}
	if err := srv.db.CreateApp(other); err != nil {
		t.Fatal(err)
	}
	d := &database.Deployment{AppID: other.ID, Status: database.DeployStatusSuccess}
	if err := srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatal(err)
	}
	if rec := getAs(t, srv, router, "root", "/api/v1/apps/"+app.ID+"/deployments/"+d.ID+"/sbom"); rec.Code != http.StatusNotFound {
		t.Errorf("another app's deployment = %d, want 404", rec.Code)
	}
	rec := getAs(t, srv, router, "root", "/api/v1/apps/"+other.ID+"/deployments/"+d.ID+"/sbom")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"bomFormat": "CycloneDX"`) {
		t.Errorf("its own app = %d %.200s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dependencies were not resolved for this release") {
		t.Error("a release with no verdict did not say so")
	}
}
