package depsweep_test

import (
	"context"
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

// The sweep's whole job is transitions: one open row per (app, advisory),
// opened when a serving release first matches and closed when it stops. Never
// one per sweep — it runs every six hours forever.

func setup(t *testing.T, mode string) (*database.DB, *depsweep.Sweeper, *database.App) {
	t.Helper()
	db := dbtest.New(t)

	at := time.Now().UTC()
	if err := db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-1", []advisory.Record{
		{
			ID: "GHSA-crit", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
			Summary: "Prototype pollution",
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "lodash",
				Range: advisory.Range{Introduced: "0", Fixed: "4.17.21"},
			}},
		},
		{
			// Below a critical threshold, so it is recorded and shown and never
			// opens an event: a moderate transitive CVE is on nearly every Node
			// app, and one refreshed row per app per such finding is the noise
			// that buries the malware row.
			ID: "GHSA-low", Kind: advisory.KindVuln, Severity: advisory.SeverityLow,
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "express",
				Range: advisory.Range{Introduced: "0", Fixed: "5.0.0"},
			}},
		},
	}, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 2, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	settings, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	settings.DependencyPolicy = mode
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}

	owner := &database.User{ID: "owner", Login: "owner"}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: "swept", Name: "swept", OwnerID: owner.ID,
		RepoURL: "https://github.com/example/swept", Branch: "main",
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
	deploy.Status = database.DeployStatusSuccess
	deploy.FinishedAt = &finished
	if err := db.UpdateDeployment(deploy); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15"},
		{Ecosystem: advisory.EcosystemNPM, Package: "express", Version: "4.18.2", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	rec := events.NewRecorder(db, nil, nil)
	// **A licensed install, asked for by name.** The entitlement default is now
	// the demo tier, and dependency policy needs a trial license — so an
	// unlicensed resolver reports `Policy.Off()` and the gate stops gating,
	// which is right in production and makes every test here assert nothing.
	return db, depsweep.NewSweeper(db, &deps.Resolver{
		Store: db, Ents: entitlements.AtTier(entitlements.TierPaid),
	}, rec), app
}

func openRows(t *testing.T, db *database.DB, appID string) map[string]bool {
	t.Helper()
	open, err := db.OpenEventFingerprints(appID, string(events.KindDependencyAdvisory))
	if err != nil {
		t.Fatalf("OpenEventFingerprints: %v", err)
	}
	return open
}

func TestSweepOpensOneRowPerAdvisoryAndOnlyOnce(t *testing.T) {
	db, sweeper, app := setup(t, database.DependencyPolicyWarn)

	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	open := openRows(t, db, app.ID)
	if len(open) != 1 || !open["GHSA-crit"] {
		t.Fatalf("open = %v, want just the critical one", open)
	}

	// Six hours later, and every six hours after that, forever. The whole
	// point of the fingerprint is that this adds nothing.
	before, err := db.ListAppEvents(app.ID, database.AppEventFilter{})
	if err != nil {
		t.Fatalf("ListAppEvents: %v", err)
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep again: %v", err)
	}
	after, _ := db.ListAppEvents(app.ID, database.AppEventFilter{})
	if len(after) != len(before) {
		t.Errorf("a second sweep wrote %d more rows; it should write none", len(after)-len(before))
	}
}

// The first evaluation records in full and tells nobody. Without it, the
// feature's opening act on an install with forty apps is a burst of mail about
// findings that are years old.
func TestFirstSweepIsSilentAndTheSecondIsNot(t *testing.T) {
	db, sweeper, app := setup(t, database.DependencyPolicyWarn)

	if at, _ := db.DependencyBaselinedAt(app.ID); at != nil {
		t.Fatal("a fresh app has no baseline")
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	list, err := db.ListAppEvents(app.ID, database.AppEventFilter{
		Kind: string(events.KindDependencyAdvisory)})
	if err != nil {
		t.Fatalf("ListAppEvents: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want one row recorded on the baseline pass, got %d", len(list))
	}
	// Recorded in full and visible immediately — the row is there. Silent
	// means nobody was told, which is NotifiedAt staying unset.
	if list[0].NotifiedAt != nil {
		t.Errorf("the baseline pass must claim no notification: %+v", list[0])
	}
	if at, _ := db.DependencyBaselinedAt(app.ID); at == nil {
		t.Error("the baseline should be recorded after the first pass")
	}

	// A genuinely new advisory, after the baseline, is the thing anybody
	// wanted to be told about.
	at := time.Now().UTC()
	if err := db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-2", []advisory.Record{{
		ID: "MAL-new", Kind: advisory.KindMalware,
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
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep after the new advisory: %v", err)
	}

	list, _ = db.ListAppEvents(app.ID, database.AppEventFilter{
		Kind: string(events.KindDependencyAdvisory)})
	var found bool
	for _, e := range list {
		if e.Fingerprint == "MAL-new" {
			found = true
			if e.NotifiedAt == nil {
				t.Error("a post-baseline advisory should claim the notification")
			}
			// A warning, never an error: nothing is broken and the app is
			// still serving.
			if e.Severity != "warning" {
				t.Errorf("severity = %q, want warning", e.Severity)
			}
		}
	}
	if !found {
		t.Errorf("the new advisory should have opened a row: %+v", list)
	}
}

// Per fingerprint, not per kind: an app matching two advisories and fixing one
// has one problem left, and closing both would report what it wants to hear.
func TestSweepClosesOnlyWhatNoLongerMatches(t *testing.T) {
	db, sweeper, app := setup(t, database.DependencyPolicyWarn)
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Add a second blocking advisory so there are two open rows.
	at := time.Now().UTC()
	if err := db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-2", []advisory.Record{
		{
			ID: "GHSA-crit", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "lodash",
				Range: advisory.Range{Introduced: "0", Fixed: "4.17.21"},
			}},
		},
		{
			ID: "GHSA-second", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "express",
				Range: advisory.Range{Introduced: "0", Fixed: "5.0.0"},
			}},
		},
	}, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 2, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if open := openRows(t, db, app.ID); len(open) != 2 {
		t.Fatalf("open = %v, want two", open)
	}

	// One of them is withdrawn; the other still stands.
	withdrawn := at.Add(-time.Minute)
	if err := db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-3", []advisory.Record{
		{
			ID: "GHSA-crit", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "lodash",
				Range: advisory.Range{Introduced: "0", Fixed: "4.17.21"},
			}},
		},
		{
			ID: "GHSA-second", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
			WithdrawnAt: &withdrawn,
			Affected: []advisory.Affected{{
				Ecosystem: advisory.EcosystemNPM, Package: "express",
				Range: advisory.Range{Introduced: "0", Fixed: "5.0.0"},
			}},
		},
	}, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 2, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	open := openRows(t, db, app.ID)
	if len(open) != 1 || !open["GHSA-crit"] {
		t.Errorf("open = %v, want only the one that still matches", open)
	}
}

// A waived finding opens no row: the whole point of a waiver is that somebody
// already decided about it.
func TestSweepIgnoresWaivedFindings(t *testing.T) {
	db, sweeper, app := setup(t, database.DependencyPolicyWarn)

	if err := db.GrantDependencyWaiver(&database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-crit", Reason: "accepted, reviewed 14 August",
		ExpiresAt: time.Now().UTC().Add(48 * time.Hour),
	}, 90, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("GrantDependencyWaiver: %v", err)
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if open := openRows(t, db, app.ID); len(open) != 0 {
		t.Errorf("open = %v, want none while it is waived", open)
	}

	// And when it lapses, the row appears with nobody having done anything —
	// which is intended, and is why the UI counts down to the date.
	if _, err := db.Conn().Exec(
		`UPDATE dependency_waivers SET expires_at = ? WHERE app_id = ?`,
		time.Now().UTC().Add(-time.Hour), app.ID); err != nil {
		t.Fatalf("expiring the waiver: %v", err)
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if open := openRows(t, db, app.ID); !open["GHSA-crit"] {
		t.Errorf("open = %v, want the finding back once the waiver lapsed", open)
	}
}

// Off means no work at all, checked fresh on every pass rather than cached at
// construction.
func TestSweepDoesNothingWhileOff(t *testing.T) {
	db, sweeper, app := setup(t, database.DependencyPolicyOff)
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if open := openRows(t, db, app.ID); len(open) != 0 {
		t.Errorf("open = %v, want none while the feature is off", open)
	}
	if at, _ := db.DependencyBaselinedAt(app.ID); at != nil {
		t.Error("off should not even baseline")
	}
}

// The watch switch is separate from the mode: an install can want the gate at
// deploy time and no background re-reading of a third-party feed.
func TestSweepHonoursTheWatchSwitch(t *testing.T) {
	db, sweeper, app := setup(t, database.DependencyPolicyWarn)

	settings, _ := db.GetPlatformSettings()
	settings.DependencyWatchEnabled = false
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
	if err := sweeper.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if open := openRows(t, db, app.ID); len(open) != 0 {
		t.Errorf("open = %v, want none with the watch off", open)
	}

	// On-demand re-check still works, which is the point: the question stays
	// answerable, just only when somebody asks.
	releases, err := db.ListServingReleases()
	if err != nil || len(releases) != 1 {
		t.Fatalf("ListServingReleases: %v %+v", err, releases)
	}
	findings, err := sweeper.SweepApp(releases[0])
	if err != nil {
		t.Fatalf("SweepApp: %v", err)
	}
	if len(findings) != 2 {
		t.Errorf("want both findings from an on-demand check, got %+v", findings)
	}
}
