package depsweep_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deps"
	"toft/internal/depsweep"
	"toft/internal/entitlements"
	"toft/internal/events"
	"toft/internal/runtime"
)

// runtimeSetup is a shared Shiny app whose deploy recorded only Toft's own
// packages, whose library on disk also holds one the app installed while it
// ran, and a CRAN corpus that names that one as malware.
func runtimeSetup(t *testing.T, mode string) (*database.DB, *depsweep.Sweeper, *database.App, string) {
	t.Helper()
	db := dbtest.New(t)

	at := time.Now().UTC()
	// The covered feeds as well: an install with CRAN alone has no data the
	// policy will act on (Policy.HasData is the oldest covered corpus).
	loadLockCorpus(t, db, at)
	if err := db.ReplaceSnapshot(advisory.EcosystemCRAN, "snap-r", []advisory.Record{{
		ID: "MAL-R-1", Kind: advisory.KindMalware, Summary: "Exfiltrates credentials",
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemCRAN, Package: "evilpkg",
			Range: advisory.Range{Versions: []string{"0.1.0"}},
		}},
	}}, advisory.FeedState{
		Ecosystem: advisory.EcosystemCRAN, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 1, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	settings, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	settings.DependencyPolicy = database.DependencyPolicyWarn
	settings.DependencyLockOn = database.DependencyLockMalware
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}

	tree := t.TempDir()
	lib := filepath.Join(tree, "renv", "library", "R-4.5")
	for name, version := range map[string]string{"renv": "1.0.7", "evilpkg": "0.1.0"} {
		if err := os.MkdirAll(filepath.Join(lib, name), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "Package: " + name + "\nVersion: " + version + "\n"
		if err := os.WriteFile(filepath.Join(lib, name, "DESCRIPTION"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	owner := &database.User{ID: "r-owner", Login: "r-owner"}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: "shiny-app", Name: "Shiny app", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/shiny", Branch: "main",
		Status: database.AppStatusRunning, Framework: "shiny", RunningMode: mode,
		ReleaseRef: runtime.LocalRelease(tree).String(),
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	deploy := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess}
	if err := db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	finished := time.Now()
	deploy.FinishedAt = &finished
	if err := db.UpdateDeployment(deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "renv", Version: "1.0.7", Source: deps.SourcePlatform},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	if err := db.SetDependencyBaseline(app.ID, time.Now().UTC()); err != nil {
		t.Fatalf("SetDependencyBaseline: %v", err)
	}

	sweeper := depsweep.NewSweeper(db, &deps.Resolver{
		Store: db, Ents: entitlements.AtTier(entitlements.TierPaid),
	}, events.NewRecorder(db, nil, nil)).WithReleaseTrees(func(ref string) string {
		return runtime.ParseReleaseRef(ref).Value
	})
	return db, sweeper, app, deploy.ID
}

// **The silent one.** If the read returned nothing, every other dependency
// test would still pass and an R app's runtime malware would serve forever.
func TestASharedRAppsRuntimeInstallIsRecordedAndLocks(t *testing.T) {
	db, sweeper, app, deployID := runtimeSetup(t, "shared")

	rels, err := db.ListServingReleases()
	if err != nil || len(rels) != 1 {
		t.Fatalf("ListServingReleases: %v %v", rels, err)
	}

	// Check again reads the library and records what it found, and never locks.
	if _, err := sweeper.SweepApp(rels[0]); err != nil {
		t.Fatalf("SweepApp: %v", err)
	}
	if mustApp(t, db, app.ID).Locked() {
		t.Fatal("a button is a person: Check again must never lock")
	}
	inv, _ := db.ListDeploymentDependencies(deployID)
	var found bool
	for _, d := range inv {
		if d.Package == "evilpkg" {
			found = true
			if d.Source != deps.SourceRuntime {
				t.Errorf("evilpkg recorded as %q, want runtime", d.Source)
			}
		}
		if d.Package == "renv" && d.Source != deps.SourcePlatform {
			t.Errorf("renv was rewritten to %q", d.Source)
		}
	}
	if !found {
		t.Fatalf("the runtime install was not recorded: %+v", inv)
	}

	if err := sweeper.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := mustApp(t, db, app.ID); !got.LockedByPolicy() {
		t.Errorf("a scheduled pass must lock on runtime malware: %+v", got)
	}
}

// An isolated pod's library is not on this machine; whatever the platform's
// copy of the tree holds is not what the pod loaded.
func TestAnIsolatedRAppsTreeIsNotRead(t *testing.T) {
	db, sweeper, _, deployID := runtimeSetup(t, "isolated")
	if err := sweeper.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	inv, _ := db.ListDeploymentDependencies(deployID)
	if len(inv) != 1 {
		t.Errorf("an isolated app's inventory grew: %+v", inv)
	}
}
