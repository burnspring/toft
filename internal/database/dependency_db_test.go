package database_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deps"
)

func mkDepApp(t *testing.T, db *database.DB, slug string) *database.App {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug, Name: "Owner " + slug}
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

func mkDeploy(t *testing.T, db *database.DB, appID, status string, finished time.Time) *database.Deployment {
	t.Helper()
	d := &database.Deployment{AppID: appID, Branch: "main", Status: status}
	if err := db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	d.Status = status
	d.FinishedAt = &finished
	if err := db.UpdateDeployment(d); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	return d
}

// lodashCorpus is one advisory covering npm lodash below 4.17.21.
func lodashCorpus() []advisory.Record {
	return []advisory.Record{{
		ID: "GHSA-p6mc-m468-83gg", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
		Summary: "Prototype pollution", URL: "https://example.com/a",
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemNPM, Package: "lodash",
			Range: advisory.Range{Introduced: "0", Fixed: "4.17.21"},
		}},
	}}
}

func syncCorpus(t *testing.T, db *database.DB, ecosystem string, records []advisory.Record, at time.Time) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.ReplaceSnapshot(ecosystem, "snap-"+ecosystem+at.Format("150405"), records,
		advisory.FeedState{
			Ecosystem: ecosystem, Endpoint: "https://osv.example.com",
			LastSyncAt: &now, LastSuccessAt: &now, SnapshotAt: &at,
			AdvisoryCount: len(records), Origin: "sync",
		}); err != nil {
		t.Fatalf("ReplaceSnapshot(%s): %v", ecosystem, err)
	}
}

func TestReplaceSnapshotSwapsOneEcosystemAtATime(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC().Add(-time.Hour)

	npm := lodashCorpus()
	pypi := []advisory.Record{{
		ID: "PYSEC-1", Kind: advisory.KindVuln, Severity: advisory.SeverityHigh,
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemPyPI, Package: "flask",
			Range: advisory.Range{Introduced: "0", Fixed: "2.0"},
		}},
	}}
	syncCorpus(t, db, advisory.EcosystemNPM, npm, at)
	syncCorpus(t, db, advisory.EcosystemPyPI, pypi, at)

	total, _, err := db.CountAdvisories()
	if err != nil {
		t.Fatalf("CountAdvisories: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}

	// Re-syncing npm with an empty corpus must not take PyPI's with it. This
	// is the whole reason the swap deletes by ecosystem and the orphan cleanup
	// works by "has no ranges left" rather than by snapshot id.
	syncCorpus(t, db, advisory.EcosystemNPM, nil, at)
	total, _, _ = db.CountAdvisories()
	if total != 1 {
		t.Fatalf("total after emptying npm = %d, want PyPI's 1", total)
	}
}

// An advisory covering two ecosystems must survive one of them being replaced.
func TestReplaceSnapshotKeepsTheOtherHalfOfAnAdvisory(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()

	both := advisory.Record{
		ID: "GHSA-both", Kind: advisory.KindVuln, Severity: advisory.SeverityHigh,
		Affected: []advisory.Affected{
			{Ecosystem: advisory.EcosystemNPM, Package: "thing",
				Range: advisory.Range{Introduced: "0", Fixed: "2.0.0"}},
			{Ecosystem: advisory.EcosystemPyPI, Package: "thing",
				Range: advisory.Range{Introduced: "0", Fixed: "2.0"}},
		},
	}
	syncCorpus(t, db, advisory.EcosystemNPM, []advisory.Record{both}, at)
	syncCorpus(t, db, advisory.EcosystemPyPI, []advisory.Record{both}, at)

	// Emptying npm leaves the PyPI half, so the advisory is not an orphan.
	syncCorpus(t, db, advisory.EcosystemNPM, nil, at)
	total, _, _ := db.CountAdvisories()
	if total != 1 {
		t.Fatalf("total = %d, want the advisory kept for its PyPI half", total)
	}

	// Emptying PyPI too leaves nothing, and now it is an orphan.
	syncCorpus(t, db, advisory.EcosystemPyPI, nil, at)
	total, _, _ = db.CountAdvisories()
	if total != 0 {
		t.Fatalf("total = %d, want 0 once no ecosystem names it", total)
	}
}

// A 304 moves the "we checked" clock and deliberately not the snapshot's own
// age: an archive served unmodified for a month is a month old.
func TestUnchangedFetchDoesNotRefreshTheAge(t *testing.T) {
	db := dbtest.New(t)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), old)

	now := time.Now().UTC()
	if err := db.ReplaceSnapshot(advisory.EcosystemNPM, "", nil, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, Endpoint: "https://osv.example.com",
		LastSyncAt: &now, LastSuccessAt: &now, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot(unchanged): %v", err)
	}

	age, err := db.OldestFeedAge()
	if err != nil || age == nil {
		t.Fatalf("OldestFeedAge: %v %v", age, err)
	}
	if time.Since(*age) < 29*24*time.Hour {
		t.Errorf("the snapshot age moved on an unchanged fetch: %v", *age)
	}
	// The corpus is untouched too.
	if total, _, _ := db.CountAdvisories(); total != 1 {
		t.Errorf("total = %d, want the corpus left alone", total)
	}
}

// The oldest corpus, not the newest: a fresh npm sync does not make a
// six-month-old PyPI one current.
func TestOldestFeedAgeTakesTheWorstEcosystem(t *testing.T) {
	db := dbtest.New(t)
	recent := time.Now().UTC().Add(-time.Hour)
	ancient := time.Now().UTC().Add(-200 * 24 * time.Hour)
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), recent)
	syncCorpus(t, db, advisory.EcosystemPyPI, nil, ancient)

	age, err := db.OldestFeedAge()
	if err != nil || age == nil {
		t.Fatalf("OldestFeedAge: %v %v", age, err)
	}
	if time.Since(*age) < 199*24*time.Hour {
		t.Errorf("OldestFeedAge = %v, want the ancient PyPI corpus", *age)
	}
}

// An uncovered ecosystem never decides the install's staleness.
//
// CRAN's archive is republished when a volunteer files an R advisory — twice a
// year, against npm's several times a day — so left in this figure it pins
// Policy.Stale to true on every install forever and puts a permanent warn on
// the checks page. It contributes findings it holds and never an absence of
// them, so it cannot make a verdict older than it is.
func TestOldestFeedAgeIgnoresUncoveredEcosystems(t *testing.T) {
	db := dbtest.New(t)
	recent := time.Now().UTC().Add(-time.Hour)
	ancient := time.Now().UTC().Add(-200 * 24 * time.Hour)
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), recent)
	syncCorpus(t, db, advisory.EcosystemCRAN, nil, ancient)

	age, err := db.OldestFeedAge()
	if err != nil || age == nil {
		t.Fatalf("OldestFeedAge: %v %v", age, err)
	}
	if time.Since(*age) > 2*time.Hour {
		t.Errorf("OldestFeedAge = %v, want npm's — an ecosystem whose silence is "+
			"not a verdict must not decide how stale this install's verdicts are", *age)
	}

	// And the age itself is not hidden: the row still carries it, with the
	// coverage fact beside it so a reader can tell the two apart.
	states, err := db.FeedStates()
	if err != nil {
		t.Fatalf("FeedStates: %v", err)
	}
	var saw bool
	for _, st := range states {
		if st.Ecosystem != advisory.EcosystemCRAN {
			continue
		}
		saw = true
		if st.SnapshotAt == nil || time.Since(*st.SnapshotAt) < 199*24*time.Hour {
			t.Errorf("the uncovered feed lost its own age: %v", st.SnapshotAt)
		}
		if st.Covered {
			t.Error("cran reported itself covered")
		}
	}
	if !saw {
		t.Fatal("the uncovered feed's row is gone from FeedStates")
	}
}

// An install holding nothing but an uncovered corpus has no data, so `block`
// does not block. Coverage is what HasData is about.
func TestUncoveredCorpusAloneIsNotData(t *testing.T) {
	db := dbtest.New(t)
	syncCorpus(t, db, advisory.EcosystemCRAN, nil, time.Now().UTC())

	age, err := db.OldestFeedAge()
	if err != nil {
		t.Fatalf("OldestFeedAge: %v", err)
	}
	if age != nil {
		t.Errorf("OldestFeedAge = %v, want nil — nothing whose silence is a verdict has loaded", *age)
	}
}

func TestRecordSyncFailureLeavesTheCorpusAlone(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), at)

	if err := db.RecordSyncFailure(advisory.EcosystemNPM, "https://osv.example.com",
		"connection refused"); err != nil {
		t.Fatalf("RecordSyncFailure: %v", err)
	}
	if total, _, _ := db.CountAdvisories(); total != 1 {
		t.Errorf("a failed sync must leave the last good corpus serving, got %d", total)
	}
	states, err := db.FeedStates()
	if err != nil {
		t.Fatalf("FeedStates: %v", err)
	}
	if len(states) != 1 || states[0].LastError == "" {
		t.Errorf("the failure should be recorded: %+v", states)
	}
	if states[0].LastSuccessAt == nil {
		t.Error("the last success must survive a failure — it is what the age is measured from")
	}
}

func blockPolicy(feedAt time.Time) deps.Policy {
	return deps.Policy{
		Mode: "block", Licensed: true, ScanOnDeploy: true, WatchEnabled: true,
		BlockMalware: true, BlockSeverity: advisory.SeverityCritical, BlockOnlyFixable: true,
		WaiverMaxDays: 90, FeedMaxAge: 72 * time.Hour, FeedAsOf: &feedAt,
	}
}

func TestFindingsForMatchesVersionsAndAppliesWaivers(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), at)

	app := mkDepApp(t, db, "reports")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())

	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15",
			Path: "(this app) > webpack > lodash"},
		// Patched, so the same advisory must not match it.
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.21", Direct: true},
		{Ecosystem: advisory.EcosystemNPM, Package: "express", Version: "4.18.2", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	policy := blockPolicy(at)
	findings, err := db.FindingsFor(deploy.ID, app.ID, policy)
	if err != nil {
		t.Fatalf("FindingsFor: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly the vulnerable lodash, got %+v", findings)
	}
	f := findings[0]
	if f.Version != "4.17.15" || f.FixedIn != "4.17.21" || !f.WouldBlock {
		t.Errorf("finding = %+v", f)
	}
	if f.Path != "(this app) > webpack > lodash" {
		t.Errorf("the chain should survive to the finding: %q", f.Path)
	}

	// A waiver silences the refusal and never the record.
	w := &database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-p6mc-m468-83gg",
		Reason:    "no fix available for our webpack major yet",
		ExpiresAt: time.Now().UTC().Add(14 * 24 * time.Hour),
	}
	if err := db.GrantDependencyWaiver(w, 90, "owner-reports", database.RequestMeta{}); err != nil {
		t.Fatalf("GrantDependencyWaiver: %v", err)
	}

	findings, err = db.FindingsFor(deploy.ID, app.ID, policy)
	if err != nil {
		t.Fatalf("FindingsFor after waiver: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("the finding must stay on the record: %+v", findings)
	}
	if !findings[0].Waived || findings[0].WouldBlock {
		t.Errorf("a waived finding is recorded and does not block: %+v", findings[0])
	}
	if findings[0].WaiverEnd == nil || findings[0].WaivedBy == "" {
		t.Errorf("the waiver should say by whom and until when: %+v", findings[0])
	}
}

// A withdrawn advisory is kept in the corpus and excluded from every read, so
// "the advisory was retracted" and "the sync lost it" stay tellable apart.
func TestFindingsForIgnoresWithdrawnAdvisories(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	withdrawn := at.Add(-time.Hour)
	records := lodashCorpus()
	records[0].WithdrawnAt = &withdrawn
	syncCorpus(t, db, advisory.EcosystemNPM, records, at)

	app := mkDepApp(t, db, "withdrawn")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	findings, err := db.FindingsFor(deploy.ID, app.ID, blockPolicy(at))
	if err != nil {
		t.Fatalf("FindingsFor: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("a withdrawn advisory must not produce a finding: %+v", findings)
	}
}

// DG-10 (security review 2026-11-14): a version the matcher cannot order is
// no finding, and it is counted rather than silently nothing.
func TestFindingsForCountsIncomparableRanges(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), at)

	app := mkDepApp(t, db, "incomparable")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "1.2.3.4"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	findings, incomparable, err := db.FindingsForCounted(deploy.ID, app.ID, blockPolicy(at))
	if err != nil {
		t.Fatalf("FindingsForCounted: %v", err)
	}
	if len(findings) != 0 || incomparable != 1 {
		t.Errorf("findings=%+v incomparable=%d, want none and 1", findings, incomparable)
	}
}

func TestRollbackCarriesItsInventory(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "rollback")
	old := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))
	if err := db.SaveDeploymentDependencies(old.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.21", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	// A rollback writes its own successful row and rebuilds nothing.
	back := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())
	if err := db.CopyDeploymentDependencies(old.ID, back.ID); err != nil {
		t.Fatalf("CopyDeploymentDependencies: %v", err)
	}

	list, err := db.ListDeploymentDependencies(back.ID)
	if err != nil {
		t.Fatalf("ListDeploymentDependencies: %v", err)
	}
	if len(list) != 1 || list[0].Package != "lodash" {
		t.Errorf("the rollback should carry the inventory it restored: %+v", list)
	}
	// And it is the serving release.
	serving, err := db.ServingDeploymentID(app.ID)
	if err != nil || serving != back.ID {
		t.Errorf("ServingDeploymentID = %q (err %v), want %q", serving, err, back.ID)
	}
}

// After a blocked deploy the app is still on the old release while the new one
// was turned away, and the tab has to keep the two apart or the developer
// reads today's refusal off yesterday's inventory.
func TestLastRefusedDeploymentIsSeparateFromWhatIsServing(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "refused")

	serving := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))
	refused := mkDeploy(t, db, app.ID, database.DeployStatusFailed, time.Now())
	if err := db.SetDeploymentVerdict(refused.ID, deps.Verdict{
		Allowed: false, Reason: "REFUSED: …", Mode: "block", Scanned: true,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}

	gotServing, _ := db.ServingDeploymentID(app.ID)
	if gotServing != serving.ID {
		t.Errorf("ServingDeploymentID = %q, want the successful one %q", gotServing, serving.ID)
	}

	got, err := db.LastRefusedDeployment(app.ID)
	if err != nil {
		t.Fatalf("LastRefusedDeployment: %v", err)
	}
	if got == nil || got.ID != refused.ID {
		t.Fatalf("LastRefusedDeployment = %+v, want %q", got, refused.ID)
	}

	// A fixed, successful deploy after the refusal ends the headline: the
	// refusal is older than what is serving (G-7).
	mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(time.Minute))
	if got, _ := db.LastRefusedDeployment(app.ID); got != nil {
		t.Errorf("a refusal older than the serving release was still shown: %+v", got)
	}

	// A build that failed for some other reason is not this feature's to show.
	other := mkDepApp(t, db, "other")
	mkDeploy(t, db, other.ID, database.DeployStatusFailed, time.Now())
	if got, _ := db.LastRefusedDeployment(other.ID); got != nil {
		t.Errorf("an unrelated failure must not appear as a refusal: %+v", got)
	}
}

func TestAppsAffectedBySearchesByPackageAndAdvisory(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), at)

	hit := mkDepApp(t, db, "affected")
	hitDeploy := mkDeploy(t, db, hit.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(hitDeploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	clean := mkDepApp(t, db, "clean")
	cleanDeploy := mkDeploy(t, db, clean.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(cleanDeploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.21"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	// The identifier — what a security team reads in the news.
	byID, err := db.AppsAffectedBy("GHSA-p6mc-m468-83gg", blockPolicy(at))
	if err != nil {
		t.Fatalf("AppsAffectedBy: %v", err)
	}
	if len(byID) != 1 || byID[0].Slug != "affected" {
		t.Fatalf("by advisory = %+v", byID)
	}

	// The package name — what actually gets typed, hours earlier.
	byName, err := db.AppsAffectedBy("lodash", blockPolicy(at))
	if err != nil {
		t.Fatalf("AppsAffectedBy: %v", err)
	}
	if len(byName) != 1 || byName[0].Slug != "affected" {
		t.Errorf("by package = %+v, want only the app on a vulnerable version", byName)
	}

	if none, _ := db.AppsAffectedBy("nothing-like-this", blockPolicy(at)); len(none) != 0 {
		t.Errorf("want no matches, got %+v", none)
	}
}

// Only the release each app is actually serving. An old deployment's inventory
// answers a historical question, not "what is exposed right now".
func TestAppsAffectedByReadsOnlyTheServingRelease(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), at)

	app := mkDepApp(t, db, "upgraded")
	old := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now().Add(-time.Hour))
	if err := db.SaveDeploymentDependencies(old.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.15"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	current := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(current.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.21"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	got, err := db.AppsAffectedBy("lodash", blockPolicy(at))
	if err != nil {
		t.Fatalf("AppsAffectedBy: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the app upgraded; only the serving release counts: %+v", got)
	}
}

func TestWaiverRules(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "waivers")
	meta := database.RequestMeta{}

	// A reason is required: the next administrator, six months on, needs the
	// sentence somebody wrote at the time.
	err := db.GrantDependencyWaiver(&database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-1",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, 90, "owner-waivers", meta)
	if err == nil {
		t.Error("a waiver with no reason must be refused")
	}

	// There is no permanent waiver, and the cap is the install's.
	err = db.GrantDependencyWaiver(&database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-1", Reason: "accepted",
		ExpiresAt: time.Now().AddDate(0, 0, 200),
	}, 90, "owner-waivers", meta)
	if err == nil {
		t.Error("an expiry past the install's cap must be refused")
	}

	// Zero days means waivers are off entirely, and storing one would be a lie
	// on every surface that shows it.
	err = db.GrantDependencyWaiver(&database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-1", Reason: "accepted",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, 0, "owner-waivers", meta)
	if err == nil {
		t.Error("with waivers off, granting one must be refused")
	}

	good := &database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-1", Reason: "no fix yet; reviewed 14 August",
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	}
	if err := db.GrantDependencyWaiver(good, 90, "owner-waivers", meta); err != nil {
		t.Fatalf("GrantDependencyWaiver: %v", err)
	}

	live, err := db.ListDependencyWaivers(app.ID, false)
	if err != nil {
		t.Fatalf("ListDependencyWaivers: %v", err)
	}
	if len(live) != 1 || live[0].Reason != good.Reason {
		t.Fatalf("live = %+v", live)
	}
	// A GUID is not who accepted the risk.
	if live[0].CreatedByName != "Owner waivers" {
		t.Errorf("CreatedByName = %q", live[0].CreatedByName)
	}

	// Revoked rather than deleted, so the estate view can explain why a
	// finding that was quiet last week is loud today.
	ok, err := db.RevokeDependencyWaiver(app.ID, good.ID, "owner-waivers", meta)
	if err != nil || !ok {
		t.Fatalf("RevokeDependencyWaiver: ok=%v err=%v", ok, err)
	}
	if live, _ = db.ListDependencyWaivers(app.ID, false); len(live) != 0 {
		t.Errorf("a revoked waiver is not live: %+v", live)
	}
	if all, _ := db.ListDependencyWaivers(app.ID, true); len(all) != 1 {
		t.Errorf("it is still on the record: %+v", all)
	}

	// The route takes an id from a URL, so the query is scoped by app.
	other := mkDepApp(t, db, "not-yours")
	if ok, _ := db.RevokeDependencyWaiver(other.ID, good.ID, "x", meta); ok {
		t.Error("revoking another app's waiver must not work")
	}
}

// A narrow waiver beats a broad one, so an administrator can accept an
// advisory on one package of an app without accepting it on every package.
func TestPerPackageWaiverIsPreferred(t *testing.T) {
	db := dbtest.New(t)
	at := time.Now().UTC()
	syncCorpus(t, db, advisory.EcosystemNPM, []advisory.Record{{
		ID: "GHSA-two-pkgs", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
		Affected: []advisory.Affected{
			{Ecosystem: advisory.EcosystemNPM, Package: "alpha",
				Range: advisory.Range{Introduced: "0", Fixed: "2.0.0"}},
			{Ecosystem: advisory.EcosystemNPM, Package: "beta",
				Range: advisory.Range{Introduced: "0", Fixed: "2.0.0"}},
		},
	}}, at)

	app := mkDepApp(t, db, "narrow")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "alpha", Version: "1.0.0", Direct: true},
		{Ecosystem: advisory.EcosystemNPM, Package: "beta", Version: "1.0.0", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}

	if err := db.GrantDependencyWaiver(&database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-two-pkgs", Package: "alpha",
		Reason: "alpha only", ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}, 90, "owner-narrow", database.RequestMeta{}); err != nil {
		t.Fatalf("GrantDependencyWaiver: %v", err)
	}

	findings, err := db.FindingsFor(deploy.ID, app.ID, blockPolicy(at))
	if err != nil {
		t.Fatalf("FindingsFor: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("both packages stay on the record: %+v", findings)
	}
	for _, f := range findings {
		switch f.Package {
		case "alpha":
			if !f.Waived || f.WouldBlock {
				t.Errorf("alpha should be waived: %+v", f)
			}
		case "beta":
			if f.Waived || !f.WouldBlock {
				t.Errorf("beta should still block: %+v", f)
			}
		}
	}
}

// Unscanned is a state with a name, and it never reads as zero findings —
// most of an estate is in it on the first day.
func TestListAppInventoryStatesDistinguishesUnscanned(t *testing.T) {
	db := dbtest.New(t)

	scanned := mkDepApp(t, db, "scanned")
	d := mkDeploy(t, db, scanned.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(d.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.21"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	// Deployed before the feature was switched on: a release, but no inventory.
	never := mkDepApp(t, db, "never")
	mkDeploy(t, db, never.ID, database.DeployStatusSuccess, time.Now())
	// Never deployed at all.
	fresh := mkDepApp(t, db, "fresh")
	// Recorded in a covered ecosystem whose corpus has never loaded: checked
	// against nothing, so not scanned (security review 2026-11-14, DG-5).
	nocorpus := mkDepApp(t, db, "nocorpus")
	d2 := mkDeploy(t, db, nocorpus.ID, database.DeployStatusSuccess, time.Now())
	if err := db.SaveDeploymentDependencies(d2.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemPyPI, Package: "flask", Version: "3.0.0"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	syncCorpus(t, db, advisory.EcosystemNPM, nil, time.Now().UTC())

	states, err := db.ListAppInventoryStates()
	if err != nil {
		t.Fatalf("ListAppInventoryStates: %v", err)
	}
	byID := map[string]database.AppInventoryState{}
	for _, s := range states {
		byID[s.AppID] = s
	}
	if s := byID[scanned.ID]; !s.Scanned || s.Packages != 1 {
		t.Errorf("scanned = %+v", s)
	}
	if s := byID[never.ID]; s.Scanned || s.Packages != 0 {
		t.Errorf("an app with a release and no inventory is unscanned: %+v", s)
	}
	if s := byID[fresh.ID]; s.Scanned || s.DeploymentID != "" {
		t.Errorf("an app with no release is unscanned: %+v", s)
	}
	if s := byID[nocorpus.ID]; s.Scanned || s.Packages != 1 {
		t.Errorf("an app whose ecosystem has no corpus is unscanned: %+v", s)
	}
	if byID[scanned.ID].OwnerName == "" {
		t.Error("the estate view names owners")
	}
}

// The first evaluation of an app is silent, per app rather than per install,
// so an app onboarded in November is treated like one that was there on day one.
func TestDependencyBaselineIsPerAppAndWrittenOnce(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "baseline")

	at, err := db.DependencyBaselinedAt(app.ID)
	if err != nil || at != nil {
		t.Fatalf("a fresh app has no baseline: %v %v", at, err)
	}

	first := time.Now().UTC().Truncate(time.Second)
	if err := db.SetDependencyBaseline(app.ID, first); err != nil {
		t.Fatalf("SetDependencyBaseline: %v", err)
	}
	// Written once: a second sweep must not move it, or every sweep would be
	// somebody's first and nothing would ever notify.
	if err := db.SetDependencyBaseline(app.ID, first.Add(time.Hour)); err != nil {
		t.Fatalf("SetDependencyBaseline again: %v", err)
	}
	at, _ = db.DependencyBaselinedAt(app.ID)
	if at == nil || !at.Equal(first) {
		t.Errorf("baseline = %v, want it pinned to %v", at, first)
	}

	// Re-enabling the watch re-baselines rather than firing a month of
	// accumulated news at once.
	if err := db.ClearDependencyBaselines(); err != nil {
		t.Fatalf("ClearDependencyBaselines: %v", err)
	}
	if at, _ = db.DependencyBaselinedAt(app.ID); at != nil {
		t.Errorf("baseline = %v, want cleared", at)
	}
}

func TestResolveDependencyPolicy(t *testing.T) {
	db := dbtest.New(t)

	// Off by default, and off means the corpus is not even consulted.
	p, err := db.ResolveDependencyPolicy(true, "")
	if err != nil {
		t.Fatalf("ResolveDependencyPolicy: %v", err)
	}
	if !p.Off() || p.Enforcing() {
		t.Fatalf("a fresh install has the feature off: %+v", p)
	}

	settings, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	settings.DependencyPolicy = database.DependencyPolicyBlock
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "req", "ip", "ua"); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}

	// Still not enforcing: no corpus has ever loaded.
	p, _ = db.ResolveDependencyPolicy(true, "")
	if p.Off() {
		t.Error("block is not off")
	}
	if p.Enforcing() {
		t.Error("block with no corpus must not enforce")
	}

	syncCorpus(t, db, advisory.EcosystemNPM, lodashCorpus(), time.Now().UTC())
	p, _ = db.ResolveDependencyPolicy(true, "")
	if !p.Enforcing() {
		t.Errorf("block with a corpus should enforce: %+v", p)
	}

	// An unlicensed install reads as off however the settings are written.
	p, _ = db.ResolveDependencyPolicy(false, "no license")
	if !p.Off() || p.Enforcing() {
		t.Errorf("unlicensed = %+v, want off", p)
	}
}

// The credential is its own document and never appears in the settings audit
// blob — the same treatment the SMTP password and the collector headers get.
func TestFeedCredentialIsItsOwnDocument(t *testing.T) {
	db := dbtest.New(t)
	meta := database.RequestMeta{}

	if got, _ := db.GetDependencyFeedCredential(); got != "" {
		t.Errorf("a fresh install has no credential, got %q", got)
	}
	admin := database.BuiltinAdminID
	if err := db.SetDependencyFeedCredential("ciphertext-1", admin, meta); err != nil {
		t.Fatalf("SetDependencyFeedCredential: %v", err)
	}
	if got, _ := db.GetDependencyFeedCredential(); got != "ciphertext-1" {
		t.Errorf("credential = %q", got)
	}

	// Clearing deletes the row rather than storing a blank: a secret retained
	// for no purpose is a secret somebody eventually leaks.
	if err := db.SetDependencyFeedCredential("", admin, meta); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got, _ := db.GetDependencyFeedCredential(); got != "" {
		t.Errorf("credential = %q, want cleared", got)
	}

	entries, err := db.ListActivity(database.ActivityFilter{Source: "audit", Limit: 100})
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	var sawAction bool
	for _, e := range entries {
		if e.Action == "dependency_feed_credential.update" {
			sawAction = true
			if !contains(e.Detail, "credential_set") {
				t.Errorf("the audit row should say whether one is set: %s", e.Detail)
			}
		}
		if contains(e.Detail, "ciphertext-1") {
			t.Fatalf("the credential reached the audit log: %s", e.Detail)
		}
	}
	if !sawAction {
		t.Error("the change should be audited, just not its value")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// R's second write replaces R's rows and nothing else: a release with npm and
// R keeps its npm inventory after the installed library becomes the record.
// Silent if broken — the tab would simply lose the npm half.
func TestReplaceEcosystemDependenciesLeavesOtherEcosystems(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "mixed")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())

	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "express", Version: "4.18.2", Direct: true},
		{Ecosystem: advisory.EcosystemCRAN, Package: "shiny", Version: "1.8.0", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	if err := db.ReplaceEcosystemDependencies(deploy.ID, advisory.EcosystemCRAN, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "shiny", Version: "1.8.1", Direct: true, Source: deps.SourceInstalled},
		{Ecosystem: advisory.EcosystemCRAN, Package: "renv", Version: "1.0.7", Source: deps.SourcePlatform},
	}); err != nil {
		t.Fatalf("ReplaceEcosystemDependencies: %v", err)
	}

	got, err := db.ListDeploymentDependencies(deploy.ID)
	if err != nil {
		t.Fatalf("ListDeploymentDependencies: %v", err)
	}
	want := map[string]string{
		"npm/express@4.18.2": deps.SourceResolved,
		"cran/renv@1.0.7":    deps.SourcePlatform,
		"cran/shiny@1.8.1":   deps.SourceInstalled,
	}
	if len(got) != len(want) {
		t.Fatalf("want %d rows, got %+v", len(want), got)
	}
	for _, d := range got {
		k := d.Ecosystem + "/" + d.Package + "@" + d.Version
		if src, ok := want[k]; !ok || src != d.Source {
			t.Errorf("%s: source %q, want %q (present: %v)", k, d.Source, src, ok)
		}
	}
}

// The sweep's write appends and never rewrites: a package already recorded
// keeps the source it was recorded with, and only the new one is counted.
func TestAppendDeploymentDependenciesKeepsWhatIsThere(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "runtime")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())

	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "renv", Version: "1.0.7", Source: deps.SourcePlatform},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	n, err := db.AppendDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "renv", Version: "1.0.7", Source: deps.SourceRuntime},
		{Ecosystem: advisory.EcosystemCRAN, Package: "dplyr", Version: "1.1.4", Source: deps.SourceRuntime},
	})
	if err != nil {
		t.Fatalf("AppendDeploymentDependencies: %v", err)
	}
	if n != 1 {
		t.Errorf("added %d rows, want 1", n)
	}
	got, _ := db.ListDeploymentDependencies(deploy.ID)
	for _, d := range got {
		want := deps.SourceRuntime
		if d.Package == "renv" {
			want = deps.SourcePlatform
		}
		if d.Source != want {
			t.Errorf("%s: source %q, want %q", d.Package, d.Source, want)
		}
	}
}

// A lockfile that names one package at one version at two paths — the same
// version hoisted twice, routine in an npm tree — is one inventory row, and the
// whole write lands rather than failing on Postgres's refusal of two new rows
// on one key in one statement (code review 2026-11-11, G-2).
func TestSaveDeploymentDependenciesMergesDuplicateRows(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "dupes")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())

	err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "ms", Version: "2.0.0", Dev: true, Path: "app > a > debug > ms"},
		{Ecosystem: advisory.EcosystemNPM, Package: "ms", Version: "2.0.0", Direct: true, Path: "app > ms"},
		{Ecosystem: advisory.EcosystemNPM, Package: "ms", Version: "2.1.3", Path: "app > b > ms"},
	})
	if err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	got, err := db.ListDeploymentDependencies(deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(got), got)
	}
	for _, d := range got {
		if d.Version == "2.0.0" {
			if !d.Direct || d.Dev || d.Path != "app > ms" {
				t.Errorf("merged row = %+v, want direct, not dev, the shortest path", d)
			}
		}
	}
}

// LB-1 (security review 2026-11-15): a package or version Postgres refuses —
// a NUL, an over-long value — failed the whole multi-row insert. The writer
// cleans and clips both, and two rows the clip makes equal are folded rather
// than colliding on the arbiter key in one statement.
func TestSaveDeploymentDependenciesCleansWhatPostgresRefuses(t *testing.T) {
	db := dbtest.New(t)
	app := mkDepApp(t, db, "dirty")
	deploy := mkDeploy(t, db, app.ID, database.DeployStatusSuccess, time.Now())

	long := strings.Repeat("9", 5000)
	if err := db.SaveDeploymentDependencies(deploy.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemCRAN, Package: "shiny", Version: "1.9.1\x00"},
		{Ecosystem: advisory.EcosystemCRAN, Package: "big", Version: long},
		{Ecosystem: advisory.EcosystemCRAN, Package: "big", Version: long + "1"},
		{Ecosystem: advisory.EcosystemCRAN, Package: "bad\xff", Version: "1.0"},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	rows, err := db.ListDeploymentDependencies(deploy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (the two long ones folded), got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if !utf8.ValidString(r.Package) || !utf8.ValidString(r.Version) ||
			strings.ContainsRune(r.Version, 0) || len(r.Version) > deps.MaxRecordedName {
			t.Errorf("row not cleaned: %+v", r)
		}
	}
}
