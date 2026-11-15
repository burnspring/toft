package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The governance route, whose whole reason to exist is the case the findings
// search cannot answer. The fixture's app carries lodash 4.17.15, which one
// advisory covers, and express 4.18.2, which nothing has ever been filed
// against — so express is the question and lodash is the control.

// packagesAs issues the governance read as somebody, with an optional Accept.
func packagesAs(t *testing.T, f *depFixture, who, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, f.srv, who))
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func decodePackages(t *testing.T, rec *httptest.ResponseRecorder) packageSearchResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out packageSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return out
}

// TestPackageSearchAnswersWhatTheFindingsSearchCannot is the gap, stated as
// behaviour: an app installs express, no advisory mentions it, and the route
// that reads as a package search says nothing.
func TestPackageSearchAnswersWhatTheFindingsSearchCannot(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	out := decodePackages(t, packagesAs(t, f, "admin1", "/api/v1/admin/packages?q=express", ""))
	if out.Mode != "search" {
		t.Errorf("mode = %q", out.Mode)
	}
	if len(out.Occurrences) != 1 || out.Occurrences[0].Slug != "recon" {
		t.Fatalf("occurrences = %+v", out.Occurrences)
	}
	if out.Occurrences[0].Version != "4.18.2" || !out.Occurrences[0].Direct {
		t.Errorf("wrong row: %+v", out.Occurrences[0])
	}

	// The old route, on the same query, still answers nothing — which is
	// correct for exposure and is why this one exists.
	rec := getAs(t, f.srv, f.router, "admin1", "/api/v1/admin/dependencies/search?q=express")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"apps":[]`) {
		t.Errorf("the findings search should still find nothing: %s", rec.Body.String())
	}
}

// TestPackageSearchCarriesCoverage — without it, "1 app uses express" on an
// install where most apps have no inventory reads as an estate-wide all-clear.
func TestPackageSearchCarriesCoverage(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	// A second app, deployed and never scanned: it can never appear in a
	// result, so the only thing that keeps it visible is the count.
	unscanned := &database.App{
		Slug: "quiet", Name: "Quiet", OwnerID: "owner", Framework: "node",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated,
	}
	if err := f.srv.db.CreateApp(unscanned); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	out := decodePackages(t, packagesAs(t, f, "admin1", "/api/v1/admin/packages?q=express", ""))
	if out.AppsTotal != 2 || out.AppsScanned != 1 {
		t.Errorf("coverage = %d of %d, want 1 of 2", out.AppsScanned, out.AppsTotal)
	}
}

// TestPackageCatalogNeedsNoNameToType — the question a search box cannot be
// used for, because you have to know the name to type it.
func TestPackageCatalogNeedsNoNameToType(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	out := decodePackages(t, packagesAs(t, f, "admin1", "/api/v1/admin/packages", ""))
	if out.Mode != "catalog" {
		t.Fatalf("mode = %q", out.Mode)
	}
	if len(out.Packages) != 2 {
		t.Fatalf("packages = %+v", out.Packages)
	}
	byName := map[string]database.PackageCatalogEntry{}
	for _, p := range out.Packages {
		byName[p.Package] = p
	}
	if e := byName["express"]; e.Apps != 1 || e.Occurrences != 1 || e.Newest != "4.18.2" {
		t.Errorf("express = %+v", e)
	}
	if _, ok := byName["lodash"]; !ok {
		t.Errorf("the catalog is the whole inventory, not the unaffected half: %+v", out.Packages)
	}
	// Coverage rides on the catalog too.
	if out.AppsTotal != 1 || out.AppsScanned != 1 {
		t.Errorf("coverage = %d of %d", out.AppsScanned, out.AppsTotal)
	}
}

// TestPackageSearchIsAdministratorOnly — an estate-wide package-and-version
// manifest is the target-map argument multiplied, so 404 for everybody else.
func TestPackageSearchIsAdministratorOnly(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	for _, who := range []string{"owner", "viewer", ""} {
		rec := packagesAs(t, f, who, "/api/v1/admin/packages?q=express", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q got %d, want 404", who, rec.Code)
		}
	}
}

// TestPackageSearchExportsCSV — the destination for this data is usually a
// spreadsheet somebody else asked for.
func TestPackageSearchExportsCSV(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	rec := packagesAs(t, f, "admin1", "/api/v1/admin/packages?q=express", "text/csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type = %q", ct)
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parsing the csv: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want a header and one row, got %d: %v", len(rows), rows)
	}
	if rows[0][0] != "app" || rows[1][0] != "recon" || rows[1][4] != "express" {
		t.Errorf("wrong csv shape: %v", rows)
	}
}

// TestPackageSearchRefusesAnEcosystemItCannotResolve — filtering to nothing and
// returning an empty result would read as "no app has this".
func TestPackageSearchRefusesAnEcosystemItCannotResolve(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	rec := packagesAs(t, f, "admin1", "/api/v1/admin/packages?q=express&ecosystem=alpine", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// The spellings a feed uses still resolve.
	out := decodePackages(t, packagesAs(t, f, "admin1", "/api/v1/admin/packages?q=express&ecosystem=npm", ""))
	if len(out.Occurrences) != 1 {
		t.Errorf("occurrences = %+v", out.Occurrences)
	}
}

// TestPackageSearchIsAbsentWithoutAPolicy — absent, not dormant: the same gate
// as the rest of the feature, and nothing appears in the interface.
func TestPackageSearchIsAbsentWithoutAPolicy(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyOff)

	out := decodePackages(t, packagesAs(t, f, "admin1", "/api/v1/admin/packages?q=express", ""))
	if out.Available {
		t.Errorf("available should be false with no policy configured")
	}
	if len(out.Occurrences) != 0 || len(out.Packages) != 0 {
		t.Errorf("an absent feature answers nothing: %+v %+v", out.Occurrences, out.Packages)
	}
}

// TestVersionPredicateNarrowsAndKeepsWhatItCannotCompare — the route's half of
// the rule the query layer holds.
func TestVersionPredicateNarrowsAndKeepsWhatItCannotCompare(t *testing.T) {
	f := newDepFixture(t, database.DependencyPolicyWarn)

	// express 4.18.2 is inside [4.0.0, 5.0.0) and outside [5.0.0, ∞).
	out := decodePackages(t, packagesAs(t, f, "admin1",
		"/api/v1/admin/packages?q=express&introduced=4.0.0&fixed=5.0.0", ""))
	if len(out.Occurrences) != 1 {
		t.Fatalf("want the in-range row, got %+v", out.Occurrences)
	}
	out = decodePackages(t, packagesAs(t, f, "admin1",
		"/api/v1/admin/packages?q=express&introduced=5.0.0", ""))
	if len(out.Occurrences) != 0 {
		t.Errorf("want nothing in range, got %+v", out.Occurrences)
	}

	// An exact version is the closed interval, so it is a version comparison
	// rather than a string one.
	out = decodePackages(t, packagesAs(t, f, "admin1",
		"/api/v1/admin/packages?q=express&version=4.18.2", ""))
	if len(out.Occurrences) != 1 {
		t.Errorf("version= should match exactly one row, got %+v", out.Occurrences)
	}
}
