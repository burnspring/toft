package database_test

import (
	"testing"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deps"
)

func scanApp(t *testing.T, db *database.DB, slug string) *database.App {
	t.Helper()
	owner := &database.User{ID: "scan-owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: owner.ID,
		RepoURL: "https://github.com/example/" + slug, Branch: "main",
		Status: database.AppStatusRunning,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

// **The failure this catches is silent.** An ORDER BY the wrong way round keeps
// the *oldest* N readings forever and looks entirely normal on every screen: a
// full table, plausible dates, and every answer three months stale.
func TestScanPruneKeepsTheNewest(t *testing.T) {
	db := dbtest.New(t)
	app := scanApp(t, db, "prune")

	base := time.Now().UTC().Add(-24 * time.Hour)
	for i := 0; i < 10; i++ {
		scan := &database.DependencyScan{
			AppID:      app.ID,
			Trigger:    database.DependencyScanTriggerScheduled,
			OccurredAt: base.Add(time.Duration(i) * time.Hour),
			Counted:    i,
		}
		if err := db.RecordDependencyScan(scan, 3); err != nil {
			t.Fatalf("RecordDependencyScan %d: %v", i, err)
		}
	}

	scans, err := db.ListDependencyScans(app.ID, 100)
	if err != nil {
		t.Fatalf("ListDependencyScans: %v", err)
	}
	if len(scans) != 3 {
		t.Fatalf("kept %d readings, want 3", len(scans))
	}
	// Newest first, and the newest three are the ones that survived.
	for i, want := range []int{9, 8, 7} {
		if scans[i].Counted != want {
			t.Errorf("reading %d has counted=%d, want %d — the prune kept the wrong end",
				i, scans[i].Counted, want)
		}
	}

	latest, err := db.LatestDependencyScan(app.ID)
	if err != nil || latest == nil {
		t.Fatalf("LatestDependencyScan: %v, %v", latest, err)
	}
	if latest.Counted != 9 {
		t.Errorf("latest reading has counted=%d, want 9", latest.Counted)
	}
}

// Never checked and checked-and-found-nothing are different answers, and the
// tab renders them differently, so the store has to keep them apart.
func TestLatestScanIsNilBeforeAnythingHasLooked(t *testing.T) {
	db := dbtest.New(t)
	app := scanApp(t, db, "unlooked")

	latest, err := db.LatestDependencyScan(app.ID)
	if err != nil {
		t.Fatalf("LatestDependencyScan: %v", err)
	}
	if latest != nil {
		t.Errorf("an app nothing has looked at returned a reading: %+v", latest)
	}
}

// The summary is the one place a finding set becomes numbers, so a history is
// readable across its own rows.
func TestSummarizeScanCountsAndCapsTheAdvisoryIDs(t *testing.T) {
	policy := deps.Policy{
		Mode: "warn", Licensed: true, BlockMalware: true,
		BlockSeverity: advisory.SeverityHigh,
	}
	findings := []advisory.Finding{
		{AdvisoryID: "MAL-1", Kind: advisory.KindMalware},
		{AdvisoryID: "GHSA-crit", Severity: advisory.SeverityCritical},
		{AdvisoryID: "GHSA-mod", Severity: advisory.SeverityModerate},
		{AdvisoryID: "GHSA-waived", Severity: advisory.SeverityCritical, Waived: true},
		{AdvisoryID: "GHSA-none", Severity: advisory.SeverityUnknown},
	}
	inventory := []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "a", Version: "1"},
		{Ecosystem: advisory.EcosystemNPM, Package: "b", Version: "2"},
	}

	scan := database.SummarizeScan(policy, findings, inventory)
	if scan.Findings != 5 || scan.Packages != 2 {
		t.Errorf("findings=%d packages=%d, want 5 and 2", scan.Findings, scan.Packages)
	}
	if scan.Malware != 1 || scan.Critical != 2 || scan.Moderate != 1 || scan.Unknown != 1 {
		t.Errorf("by severity = %+v", scan)
	}
	// Waived is excluded from counted and recorded on its own — the visible
	// reward for having dealt with one.
	if scan.Waived != 1 {
		t.Errorf("waived = %d, want 1", scan.Waived)
	}
	if scan.Counted != 2 {
		t.Errorf("counted = %d, want 2 (malware and the unwaived critical)", scan.Counted)
	}
	// Only the notable ones, and never the waived one: the ids exist so that
	// "which advisory arrived on 3 August" stays answerable.
	if len(scan.Advisories) != 2 {
		t.Fatalf("advisories = %v, want the two counted ones", scan.Advisories)
	}
	for _, id := range scan.Advisories {
		if id == "GHSA-waived" || id == "GHSA-mod" {
			t.Errorf("%s should not be recorded as notable", id)
		}
	}
}

func TestSummarizeScanCapsTheAdvisoryList(t *testing.T) {
	policy := deps.Policy{Mode: "warn", Licensed: true, BlockMalware: true}
	var findings []advisory.Finding
	for i := 0; i < database.MaxScanAdvisoryIDs+20; i++ {
		findings = append(findings, advisory.Finding{
			AdvisoryID: "MAL-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Kind:       advisory.KindMalware,
		})
	}
	scan := database.SummarizeScan(policy, findings, nil)
	if len(scan.Advisories) != database.MaxScanAdvisoryIDs {
		t.Errorf("advisories = %d, want the cap of %d",
			len(scan.Advisories), database.MaxScanAdvisoryIDs)
	}
	if scan.Counted != len(findings) {
		t.Errorf("counted = %d, want %d — the cap bounds the ids, never the count",
			scan.Counted, len(findings))
	}
}

// The guarded transition is what makes the consequence safe: exactly one node
// locks, and an administrator's own lock is never overwritten.
func TestLockForDependencyNeverOverwritesAnAdministratorsLock(t *testing.T) {
	db := dbtest.New(t)
	app := scanApp(t, db, "already-locked")

	now := time.Now().UTC()
	if err := db.SetAppLock(app.ID, database.BuiltinAdminID, "Decommissioned — use Finance Hub.",
		now, app.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("SetAppLock: %v", err)
	}

	locked, err := db.LockAppForDependency(app.ID, app.Slug, "Locked for MAL-1.",
		map[string]any{"advisory_id": "MAL-1"}, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("LockAppForDependency: %v", err)
	}
	if locked {
		t.Fatal("the policy re-locked an app an administrator had already locked")
	}

	fresh, err := db.GetApp(app.ID)
	if err != nil || fresh == nil {
		t.Fatalf("GetApp: %v", err)
	}
	if fresh.LockedMessage != "Decommissioned — use Finance Hub." {
		t.Errorf("the administrator's message was rewritten: %q", fresh.LockedMessage)
	}
	if fresh.LockedBy != database.BuiltinAdminID {
		t.Errorf("the attribution moved to %q", fresh.LockedBy)
	}
	if fresh.LockSource() != database.AppLockSourceAdmin {
		t.Errorf("locked_source = %q, want admin", fresh.LockSource())
	}
}

// Two nodes sweeping at once means one lock, one audit row, and one
// notification — the second call changes nothing and writes nothing.
func TestLockForDependencyIsOneTransitionAndOneAuditRow(t *testing.T) {
	db := dbtest.New(t)
	app := scanApp(t, db, "locks-once")

	before, err := db.CountAuditEntries()
	if err != nil {
		t.Fatalf("CountAuditEntries: %v", err)
	}

	now := time.Now().UTC()
	detail := map[string]any{"advisory_id": "MAL-1", "package": "event-stream", "threshold": "malware"}
	first, err := db.LockAppForDependency(app.ID, app.Slug, "Locked for MAL-1.", detail, now)
	if err != nil || !first {
		t.Fatalf("first lock: %v, %v", first, err)
	}
	second, err := db.LockAppForDependency(app.ID, app.Slug, "Locked again.", detail, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second lock: %v", err)
	}
	if second {
		t.Error("the second pass locked an already-locked app")
	}

	after, err := db.CountAuditEntries()
	if err != nil {
		t.Fatalf("CountAuditEntries: %v", err)
	}
	if after-before != 1 {
		t.Errorf("wrote %d audit rows for two passes, want 1", after-before)
	}

	fresh, _ := db.GetApp(app.ID)
	if !fresh.LockedByPolicy() {
		t.Errorf("locked_source = %q, want dependency", fresh.LockSource())
	}
	if fresh.LockedBy != "" {
		t.Errorf("locked_by = %q; a platform lock has no user id and must not invent one", fresh.LockedBy)
	}
	if fresh.LockedMessage != "Locked for MAL-1." {
		t.Errorf("locked_message = %q", fresh.LockedMessage)
	}
	// The refused visitor gets a real sentence rather than the install-wide
	// default about contacting an administrator who does not yet know either.
	if got := database.ResolveLockMessage(fresh, database.PlatformSettings{}); got != "Locked for MAL-1." {
		t.Errorf("ResolveLockMessage = %q", got)
	}

	n, err := db.CountAppsLockedByDependency()
	if err != nil || n != 1 {
		t.Errorf("CountAppsLockedByDependency = %d, %v; want 1", n, err)
	}
}

// Unlocking forgets the source with the message. A stale `dependency` on an
// unlocked app would read on every surface as a lock this policy placed.
func TestUnlockClearsTheSource(t *testing.T) {
	db := dbtest.New(t)
	app := scanApp(t, db, "unlocks")

	if _, err := db.LockAppForDependency(app.ID, app.Slug, "Locked for MAL-1.",
		nil, time.Now().UTC()); err != nil {
		t.Fatalf("LockAppForDependency: %v", err)
	}
	locked, _ := db.GetApp(app.ID)
	if err := db.ClearAppLock(app.ID, database.BuiltinAdminID, app.Slug, locked,
		database.RequestMeta{}); err != nil {
		t.Fatalf("ClearAppLock: %v", err)
	}
	fresh, _ := db.GetApp(app.ID)
	if fresh.Locked() {
		t.Fatal("the app is still locked")
	}
	if fresh.LockSource() != database.AppLockSourceAdmin {
		t.Errorf("locked_source = %q after unlocking, want admin", fresh.LockSource())
	}
	if fresh.LockedByPolicy() {
		t.Error("an unlocked app reads as locked by the policy")
	}
}

// **The silent one.** A clean deploy lifts the lock the policy placed, never an
// administrator's: without the guard in the statement, an administrator who
// locked the app mid-build would find it serving again, and nothing would say
// so.
func TestLiftDependencyLockLiftsOnlyThePolicysLock(t *testing.T) {
	db := dbtest.New(t)
	now := time.Now().UTC()

	admin := scanApp(t, db, "admin-locked")
	if err := db.SetAppLock(admin.ID, database.BuiltinAdminID, "Decommissioned.",
		now, admin.Slug, database.RequestMeta{}); err != nil {
		t.Fatalf("SetAppLock: %v", err)
	}
	before, err := db.CountAuditEntries()
	if err != nil {
		t.Fatalf("CountAuditEntries: %v", err)
	}
	lifted, err := db.LiftDependencyLock(admin.ID, admin.Slug, "dep-1", now)
	if err != nil || lifted {
		t.Fatalf("LiftDependencyLock on an admin lock = %v, %v; want false", lifted, err)
	}
	if after, _ := db.CountAuditEntries(); after != before {
		t.Errorf("an unchanged row wrote %d audit entries", after-before)
	}
	if fresh, _ := db.GetApp(admin.ID); !fresh.Locked() || fresh.LockedMessage != "Decommissioned." {
		t.Errorf("the administrator's lock moved: %+v", fresh)
	}

	policy := scanApp(t, db, "policy-locked")
	if _, err := db.LockAppForDependency(policy.ID, policy.Slug, "Locked for MAL-1.", nil, now); err != nil {
		t.Fatalf("LockAppForDependency: %v", err)
	}
	lifted, err = db.LiftDependencyLock(policy.ID, policy.Slug, "dep-2", now)
	if err != nil || !lifted {
		t.Fatalf("LiftDependencyLock on the policy's lock = %v, %v; want true", lifted, err)
	}
	fresh, _ := db.GetApp(policy.ID)
	if fresh.Locked() || fresh.LockedMessage != "" || fresh.LockSource() != database.AppLockSourceAdmin {
		t.Errorf("the lock was not cleared whole: %+v", fresh)
	}
	if again, _ := db.LiftDependencyLock(policy.ID, policy.Slug, "dep-3", now); again {
		t.Error("a second lift reported lifting a lock that was already gone")
	}
}
