package database_test

import (
	"testing"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deps"
)

// The governance reader of the inventory: "what do we have", asked of apps that
// are not vulnerable and of packages no advisory has ever mentioned.

// saveDeps records an app's serving release and what it installed.
func saveDeps(t *testing.T, db *database.DB, slug string, list []deps.Dependency) *database.App {
	t.Helper()
	app := mkDepApp(t, db, slug)
	d := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))
	if err := db.SaveDeploymentDependencies(d.ID, list); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	return app
}

func npmDep(pkg, version string) deps.Dependency {
	return deps.Dependency{Ecosystem: advisory.EcosystemNPM, Package: pkg, Version: version, Direct: true}
}

// TestPackageSearchFindsAppsNoAdvisoryMentions is the whole point of the second
// reader: the findings route inner-joins the corpus, so an app carrying a
// package nothing has ever been filed against is invisible to it.
func TestPackageSearchFindsAppsNoAdvisoryMentions(t *testing.T) {
	db := dbtest.New(t)
	saveDeps(t, db, "reports", []deps.Dependency{npmDep("left-pad", "1.3.0")})

	got, err := db.SearchPackageOccurrences(database.PackageFilter{Query: "left-pad"})
	if err != nil {
		t.Fatalf("SearchPackageOccurrences: %v", err)
	}
	if len(got.Occurrences) != 1 {
		t.Fatalf("want 1 occurrence of an unadvised package, got %d", len(got.Occurrences))
	}
	if got.Occurrences[0].Slug != "reports" || got.Occurrences[0].Version != "1.3.0" {
		t.Errorf("wrong row: %+v", got.Occurrences[0])
	}

	// And the findings route still says nothing about it, which is the gap.
	affected, err := db.AppsAffectedBy("left-pad", deps.Policy{})
	if err != nil {
		t.Fatalf("AppsAffectedBy: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("findings search should not answer a governance question, got %d apps", len(affected))
	}
}

// TestVersionPredicatePerEcosystem checks the predicate against each ecosystem's
// own ordering, and that a version none of them can parse lands in its own
// bucket rather than being dropped.
func TestVersionPredicatePerEcosystem(t *testing.T) {
	cases := []struct {
		name      string
		ecosystem string
		pkg       string
		in        string // matches the range
		out       string // outside the range
		odd       string // no ordering can place it
		rng       advisory.Range
	}{
		{
			name: "npm semver", ecosystem: advisory.EcosystemNPM, pkg: "lodash",
			in: "4.17.20", out: "5.1.0", odd: "git+https://github.com/lodash/lodash#a1b2c3",
			rng: advisory.Range{Introduced: "4.0.0", Fixed: "5.0.0"},
		},
		{
			name: "pypi pep440", ecosystem: advisory.EcosystemPyPI, pkg: "requests",
			in: "2.31.0", out: "2.32.1", odd: "file:///opt/wheels/requests",
			rng: advisory.Range{Introduced: "2.0.0", Fixed: "2.32.0"},
		},
		{
			name: "go semver", ecosystem: advisory.EcosystemGo, pkg: "golang.org/x/net",
			in: "v0.17.0", out: "v0.25.0", odd: "devel",
			rng: advisory.Range{Introduced: "v0.1.0", Fixed: "v0.23.0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := dbtest.New(t)
			saveDeps(t, db, "in-range", []deps.Dependency{
				{Ecosystem: tc.ecosystem, Package: tc.pkg, Version: tc.in, Direct: true}})
			saveDeps(t, db, "out-of-range", []deps.Dependency{
				{Ecosystem: tc.ecosystem, Package: tc.pkg, Version: tc.out, Direct: true}})
			saveDeps(t, db, "pinned-to-a-fork", []deps.Dependency{
				{Ecosystem: tc.ecosystem, Package: tc.pkg, Version: tc.odd, Direct: true}})

			got, err := db.SearchPackageOccurrences(database.PackageFilter{
				Query: tc.pkg, Ecosystem: tc.ecosystem, Version: tc.rng})
			if err != nil {
				t.Fatalf("SearchPackageOccurrences: %v", err)
			}

			if len(got.Occurrences) != 1 || got.Occurrences[0].Slug != "in-range" {
				t.Fatalf("want only in-range, got %+v", got.Occurrences)
			}
			// Never dropped: an answer that silently omits the app pinned to a
			// fork reads as complete and is not.
			if len(got.Incomparable) != 1 || got.Incomparable[0].Slug != "pinned-to-a-fork" {
				t.Fatalf("want the fork in its own bucket, got %+v", got.Incomparable)
			}
			// And never counted as a match.
			for _, o := range got.Occurrences {
				if o.Version == tc.odd {
					t.Errorf("an unorderable version was counted as a match: %+v", o)
				}
			}
		})
	}
}

// TestNoVersionPredicateIsTheWholeNameMatch — the common case, *do we have this
// at all*, where even the unorderable pin is an ordinary answer.
func TestNoVersionPredicateIsTheWholeNameMatch(t *testing.T) {
	db := dbtest.New(t)
	saveDeps(t, db, "one", []deps.Dependency{npmDep("lodash", "4.17.20")})
	saveDeps(t, db, "two", []deps.Dependency{npmDep("lodash", "git+https://github.com/x/y#abc")})

	got, err := db.SearchPackageOccurrences(database.PackageFilter{Query: "lodash"})
	if err != nil {
		t.Fatalf("SearchPackageOccurrences: %v", err)
	}
	if len(got.Occurrences) != 2 {
		t.Fatalf("want both apps, got %d", len(got.Occurrences))
	}
	if len(got.Incomparable) != 0 {
		t.Errorf("nothing was compared, so nothing is incomparable: %+v", got.Incomparable)
	}
}

// TestSamePackageAtTwoVersionsIsTwoOccurrences — the reason a row is an
// occurrence rather than an app.
func TestSamePackageAtTwoVersionsIsTwoOccurrences(t *testing.T) {
	db := dbtest.New(t)
	saveDeps(t, db, "monolith", []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.20",
			Direct: true, Path: "monolith > lodash"},
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "3.10.1",
			Path: "monolith > webpack > lodash"},
	})

	got, err := db.SearchPackageOccurrences(database.PackageFilter{Query: "lodash"})
	if err != nil {
		t.Fatalf("SearchPackageOccurrences: %v", err)
	}
	if len(got.Occurrences) != 2 {
		t.Fatalf("want two occurrences for one app, got %d", len(got.Occurrences))
	}
	// The path is what settles "we don't depend on that".
	var transitive bool
	for _, o := range got.Occurrences {
		if o.Version == "3.10.1" {
			transitive = o.Path == "monolith > webpack > lodash" && !o.Direct
		}
	}
	if !transitive {
		t.Errorf("the transitive row lost its path or its direct flag: %+v", got.Occurrences)
	}

	cat, err := db.PackageCatalog(database.PackageFilter{Query: "lodash"})
	if err != nil {
		t.Fatalf("PackageCatalog: %v", err)
	}
	if len(cat) != 1 {
		t.Fatalf("want one catalog row, got %d", len(cat))
	}
	// One app, two occurrences: both numbers, because they answer different
	// questions and the difference is the finding.
	if cat[0].Apps != 1 || cat[0].Occurrences != 2 {
		t.Errorf("want 1 app and 2 occurrences, got %d and %d", cat[0].Apps, cat[0].Occurrences)
	}
	if cat[0].Newest != "4.17.20" || cat[0].Oldest != "3.10.1" {
		t.Errorf("wrong spread: newest %q oldest %q", cat[0].Newest, cat[0].Oldest)
	}
}

// TestCatalogNeverOrdersWhatItCannotParse keeps an unreadable version out of the
// newest/oldest pair while still counting it.
func TestCatalogNeverOrdersWhatItCannotParse(t *testing.T) {
	db := dbtest.New(t)
	saveDeps(t, db, "a", []deps.Dependency{npmDep("express", "4.18.2")})
	saveDeps(t, db, "b", []deps.Dependency{npmDep("express", "4.17.1")})
	saveDeps(t, db, "c", []deps.Dependency{npmDep("express", "github:expressjs/express#deadbeef")})

	cat, err := db.PackageCatalog(database.PackageFilter{Query: "express"})
	if err != nil {
		t.Fatalf("PackageCatalog: %v", err)
	}
	if len(cat) != 1 {
		t.Fatalf("want one row, got %d", len(cat))
	}
	e := cat[0]
	if e.Apps != 3 || e.Occurrences != 3 {
		t.Errorf("want 3 apps and 3 occurrences, got %d and %d", e.Apps, e.Occurrences)
	}
	if e.Newest != "4.18.2" || e.Oldest != "4.17.1" {
		t.Errorf("an unorderable version reached the spread: newest %q oldest %q", e.Newest, e.Oldest)
	}
	if e.Incomparable != 1 {
		t.Errorf("want the fork counted, got %d", e.Incomparable)
	}
	if len(e.Versions) != 3 {
		t.Errorf("every version is still listed: %v", e.Versions)
	}
}

// TestAppWithNoInventoryIsAbsentRatherThanEmpty — it contributes no rows, and
// the count that stops that reading as an all-clear is the coverage line the
// route carries, not anything here.
func TestAppWithNoInventoryIsAbsentRatherThanEmpty(t *testing.T) {
	db := dbtest.New(t)
	saveDeps(t, db, "scanned", []deps.Dependency{npmDep("lodash", "4.17.20")})
	syncCorpus(t, db, advisory.EcosystemNPM, nil, time.Now().UTC())
	// Deployed successfully, never scanned.
	unscanned := mkDepApp(t, db, "unscanned")
	mkDeploy(t, db, unscanned.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))

	got, err := db.SearchPackageOccurrences(database.PackageFilter{})
	if err != nil {
		t.Fatalf("SearchPackageOccurrences: %v", err)
	}
	for _, o := range got.Occurrences {
		if o.Slug == "unscanned" {
			t.Fatalf("an app with no inventory produced a row: %+v", o)
		}
	}

	states, err := db.ListAppInventoryStates()
	if err != nil {
		t.Fatalf("ListAppInventoryStates: %v", err)
	}
	var total, scanned int
	for _, s := range states {
		total++
		if s.Scanned {
			scanned++
		}
	}
	if total != 2 || scanned != 1 {
		t.Errorf("coverage should see both apps and count one scanned, got %d of %d", scanned, total)
	}
}

// TestDirectAndDevNarrowButDefaultToEverything — the tree is where event-stream
// was, so the default answers "is it in there anywhere".
func TestDirectAndDevNarrowButDefaultToEverything(t *testing.T) {
	db := dbtest.New(t)
	saveDeps(t, db, "app", []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "chalk", Version: "5.0.0", Direct: true},
		{Ecosystem: advisory.EcosystemNPM, Package: "chalk", Version: "4.1.2", Dev: true},
	})

	all, err := db.SearchPackageOccurrences(database.PackageFilter{Query: "chalk"})
	if err != nil {
		t.Fatalf("SearchPackageOccurrences: %v", err)
	}
	if len(all.Occurrences) != 2 {
		t.Fatalf("the default is the whole tree, got %d", len(all.Occurrences))
	}

	yes := true
	direct, err := db.SearchPackageOccurrences(database.PackageFilter{Query: "chalk", Direct: &yes})
	if err != nil {
		t.Fatalf("SearchPackageOccurrences: %v", err)
	}
	if len(direct.Occurrences) != 1 || direct.Occurrences[0].Version != "5.0.0" {
		t.Errorf("direct=true should narrow to one row, got %+v", direct.Occurrences)
	}
}

// TestSearchReadsTheServingReleaseOnly records what the scope costs: a version
// rolled off in April is not an answer this route gives.
func TestSearchReadsTheServingReleaseOnly(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "rolled-forward")

	old := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-72*time.Hour))
	if err := db.SaveDeploymentDependencies(old.ID, []deps.Dependency{npmDep("lodash", "4.17.20")}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	newer := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))
	if err := db.SaveDeploymentDependencies(newer.ID, []deps.Dependency{npmDep("lodash", "4.17.21")}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	got, err := db.SearchPackageOccurrences(database.PackageFilter{Query: "lodash"})
	if err != nil {
		t.Fatalf("SearchPackageOccurrences: %v", err)
	}
	if len(got.Occurrences) != 1 || got.Occurrences[0].Version != "4.17.21" {
		t.Fatalf("want only the serving release, got %+v", got.Occurrences)
	}
	if got.Occurrences[0].DeploymentID != newer.ID {
		t.Errorf("the row should name the release it came from")
	}
}
