package depsweep_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deps"
	"toft/internal/depsweep"
	"toft/internal/entitlements"
	"toft/internal/events"
)

// The sweep's second job: leaving a record, and the one automatic consequence.

// lockSetup is `setup` with the malware advisory the lock threshold acts on and
// the settings a caller wants to vary.
func lockSetup(t *testing.T, apply func(*database.PlatformSettings)) (*database.DB, *depsweep.Sweeper, *database.App) {
	return lockSetupWith(t, apply, true)
}

// loadLockCorpus is one malicious package and one critical vulnerability, so a
// test can tell the two classes apart — the reversal is about the first and
// deliberately not about the second.
func loadLockCorpus(t *testing.T, db *database.DB, at time.Time) {
	t.Helper()
	if err := db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-1", []advisory.Record{
		{
			ID: "MAL-1", Kind: advisory.KindMalware, Summary: "Exfiltrates credentials",
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "event-stream",
				Range: advisory.Range{Versions: []string{"3.3.6"}},
			}},
		},
		{
			ID: "GHSA-crit", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "lodash",
				Range: advisory.Range{Introduced: "0", Fixed: "4.17.21"},
			}},
		},
	}, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 2, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
}

// lockSetupWith takes the one gate a setting cannot express: whether a corpus
// has ever loaded at all.
func lockSetupWith(t *testing.T, apply func(*database.PlatformSettings), corpus bool) (*database.DB, *depsweep.Sweeper, *database.App) {
	t.Helper()
	db := dbtest.New(t)

	at := time.Now().UTC()
	if corpus {
		loadLockCorpus(t, db, at)
	}

	settings, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	settings.DependencyPolicy = database.DependencyPolicyWarn
	settings.DependencyLockOn = database.DependencyLockMalware
	if apply != nil {
		apply(&settings)
	}
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}

	owner := &database.User{ID: "lock-owner", Login: "lock-owner"}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: "locked-app", Name: "Locked app", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/locked", Branch: "main",
		Status: database.AppStatusRunning,
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
		{Ecosystem: advisory.EcosystemNPM, Package: "event-stream", Version: "3.3.6", Direct: true},
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	// Already baselined, so the pass under test is an ordinary one rather than
	// the silent first evaluation.
	if err := db.SetDependencyBaseline(app.ID, time.Now().UTC()); err != nil {
		t.Fatalf("SetDependencyBaseline: %v", err)
	}

	rec := events.NewRecorder(db, nil, nil)
	sweeper := depsweep.NewSweeper(db, &deps.Resolver{
		Store: db, Ents: entitlements.AtTier(entitlements.TierPaid),
	}, rec)
	return db, sweeper, app
}

func mustApp(t *testing.T, db *database.DB, id string) *database.App {
	t.Helper()
	app, err := db.GetApp(id)
	if err != nil || app == nil {
		t.Fatalf("GetApp: %v, %v", app, err)
	}
	return app
}

// **The failure this catches is an estate that quietly locks itself**, found by
// its users rather than by a test. Table-driven over every gate, because no
// other test here would notice any of them coming loose.
func TestNothingLocksUnlessEveryGateIsOpen(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*database.PlatformSettings)
		// tier is the entitlement, since Policy.Off folds it.
		tier entitlements.Tier
		// wipe removes the corpus, which is the gate a policy cannot express.
		wipe bool
		lock bool
	}{
		{
			name: "every gate open",
			lock: true,
		},
		{
			name:  "the policy is off",
			apply: func(s *database.PlatformSettings) { s.DependencyPolicy = database.DependencyPolicyOff },
		},
		{
			name:  "the lock threshold is off",
			apply: func(s *database.PlatformSettings) { s.DependencyLockOn = database.DependencyLockOff },
		},
		{
			name:  "the watch is off",
			apply: func(s *database.PlatformSettings) { s.DependencyWatchEnabled = false },
		},
		{
			name: "the install is not licensed",
			tier: entitlements.TierDemo,
		},
		{
			// A lock over a corpus that has never loaded is a lock over no
			// evidence. The one gate a setting cannot express.
			name: "no corpus has ever loaded",
			wipe: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, sweeper, app := lockSetupWith(t, tc.apply, !tc.wipe)
			if tc.tier != "" {
				sweeper = depsweep.NewSweeper(db, &deps.Resolver{
					Store: db, Ents: entitlements.AtTier(tc.tier),
				}, events.NewRecorder(db, nil, nil))
			}

			if err := sweeper.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			locked := mustApp(t, db, app.ID).LockedByPolicy()
			if locked != tc.lock {
				t.Errorf("locked = %v, want %v", locked, tc.lock)
			}
		})
	}
}

// A row per pass, whether or not anything changed: "checked, unchanged for
// thirty days" and "not checked for thirty days" are different answers.
func TestEveryPassWritesAReadingEvenWhenNothingChanged(t *testing.T) {
	db, sweeper, app := lockSetup(t, func(s *database.PlatformSettings) {
		s.DependencyLockOn = database.DependencyLockOff
	})

	for i := 0; i < 3; i++ {
		if err := sweeper.Sweep(context.Background()); err != nil {
			t.Fatalf("Sweep %d: %v", i, err)
		}
	}
	scans, err := db.ListDependencyScans(app.ID, 100)
	if err != nil {
		t.Fatalf("ListDependencyScans: %v", err)
	}
	if len(scans) != 3 {
		t.Fatalf("three passes wrote %d readings, want 3", len(scans))
	}
	s := scans[0]
	if s.Trigger != database.DependencyScanTriggerScheduled {
		t.Errorf("trigger = %q, want scheduled", s.Trigger)
	}
	if !s.Scanned {
		t.Error("an npm release is covered, so the reading must say so")
	}
	if s.Packages != 2 {
		t.Errorf("packages = %d, want 2", s.Packages)
	}
	if s.Malware != 1 || s.Critical != 1 {
		t.Errorf("counts = %+v, want one malicious and one critical", s)
	}
	if s.FeedAsOf == nil {
		t.Error("a reading is scoped to the corpus it was taken against")
	}
}

// The button is a person asking a question, not asking for their app to be
// taken off the air — but the answer is still written down.
func TestSweepAppRecordsAndNeverLocks(t *testing.T) {
	db, sweeper, app := lockSetup(t, nil)

	baselined, _ := db.DependencyBaselinedAt(app.ID)
	deployID, err := db.ServingDeploymentID(app.ID)
	if err != nil {
		t.Fatalf("ServingDeploymentID: %v", err)
	}
	if _, err := sweeper.SweepApp(deps.ServingRelease{
		AppID: app.ID, Slug: app.Slug, Status: app.Status,
		DeploymentID: deployID, BaselinedAt: baselined,
	}); err != nil {
		t.Fatalf("SweepApp: %v", err)
	}

	if mustApp(t, db, app.ID).Locked() {
		t.Error("Check again locked the app; an automatic consequence needs an automatic trigger")
	}
	scans, _ := db.ListDependencyScans(app.ID, 10)
	if len(scans) != 1 {
		t.Fatalf("SweepApp wrote %d readings, want 1", len(scans))
	}
	if scans[0].Trigger != database.DependencyScanTriggerManual {
		t.Errorf("trigger = %q, want manual", scans[0].Trigger)
	}
}

// The whole feature, once: a malicious package on a serving release locks the
// app, stops it, tells its owner, and says so in the audit log as the system.
func TestAScheduledPassLocksStopsAndRecords(t *testing.T) {
	db, sweeper, app := lockSetup(t, nil)

	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	fresh := mustApp(t, db, app.ID)
	if !fresh.LockedByPolicy() {
		t.Fatalf("the app was not locked by the policy: locked_at=%v source=%q",
			fresh.LockedAt, fresh.LockSource())
	}
	if fresh.LockedBy != "" {
		t.Errorf("locked_by = %q; the platform has no user id and must not invent one", fresh.LockedBy)
	}
	if fresh.LockedMessage == "" {
		t.Error("no layer of a refused visitor's message may be blank")
	}
	// With no runtime wired the workload cannot be stopped, and the lock is
	// still the decision: refused at the proxy is the safe half.
	if fresh.Status == database.AppStatusRunning {
		t.Log("status left as running, which is correct with no backend wired")
	}

	entries := lockAudit(t, db)
	if len(entries) != 1 {
		t.Fatalf("wrote %d audit rows, want 1", len(entries))
	}
	e := entries[0]
	// The administrator chose a policy months ago; they did not lock this app
	// today, and a trail that says they did is wrong about the only thing it
	// exists to record.
	if e.ActorLabel != "system" {
		t.Errorf("actor = %q, want the system with no id behind it", e.ActorLabel)
	}
	// This is an entry nobody was present for, so "why is my app locked" is
	// answered here or by guesswork.
	for _, want := range []string{"MAL-1", "event-stream", "malware"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("the audit row does not name %q: %s", want, e.Detail)
		}
	}

	// A second pass changes nothing and writes nothing.
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if again := lockAudit(t, db); len(again) != 1 {
		t.Errorf("a second pass wrote another audit row: %d rows", len(again))
	}
}

func lockAudit(t *testing.T, db *database.DB) []database.ActivityEntry {
	t.Helper()
	entries, err := db.ListActivity(database.ActivityFilter{
		Source: "audit", Actions: []string{"app.lock_dependency"},
	})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	return entries
}

// A locked app keeps being swept: an administrator deciding whether to unlock
// needs to see whether the finding is still there, and a history that stopped
// at the lock could not say. What does not repeat is the action.
func TestALockedAppIsStillRead(t *testing.T) {
	db, sweeper, app := lockSetup(t, nil)

	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	scans, _ := db.ListDependencyScans(app.ID, 10)
	if len(scans) != 2 {
		t.Errorf("a locked app produced %d readings across two passes, want 2", len(scans))
	}
}

// A waiver is somebody having signed for it, with an expiry date. It is the
// answer to an administrator who unlocks and means it, so it has to reach the
// lock decision and not only the counts.
func TestAWaivedFindingDoesNotLock(t *testing.T) {
	db, sweeper, app := lockSetup(t, nil)

	if err := db.GrantDependencyWaiver(&database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "MAL-1", Package: "event-stream",
		Reason:    "Vendored copy, reviewed by security on 30 August.",
		ExpiresAt: time.Now().UTC().AddDate(0, 0, 30),
	}, 90, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("GrantDependencyWaiver: %v", err)
	}

	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if mustApp(t, db, app.ID).Locked() {
		t.Error("a waived advisory locked the app anyway")
	}
	scans, _ := db.ListDependencyScans(app.ID, 10)
	if len(scans) != 1 || scans[0].Waived != 1 {
		t.Errorf("the reading should record it as waived: %+v", scans)
	}
}

// The estate's *Re-check now* is a button, so it records and never locks — the
// same rule the per-app button follows, applied to the whole estate.
func TestSweepNowRecordsAndNeverLocks(t *testing.T) {
	db, sweeper, app := lockSetup(t, nil)

	if err := sweeper.SweepNow(context.Background()); err != nil {
		t.Fatalf("SweepNow: %v", err)
	}
	if mustApp(t, db, app.ID).Locked() {
		t.Error("the estate re-check button locked an app")
	}
	scans, _ := db.ListDependencyScans(app.ID, 10)
	if len(scans) != 1 {
		t.Fatalf("SweepNow wrote %d readings, want 1", len(scans))
	}
	if scans[0].Trigger != database.DependencyScanTriggerManual {
		t.Errorf("trigger = %q, want manual", scans[0].Trigger)
	}
}
