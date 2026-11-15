package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/deps"
	"toft/internal/entitlements"
	"toft/internal/toolchain"
)

// Enforcement point 1: the gate immediately before the framework install step.

// gateFixture stands up a release directory with a vulnerable lockfile, a
// corpus that knows about it, and a policy in the given mode.
func gateFixture(t *testing.T, db *database.DB, mode string) (*Builder, *database.App, *database.Deployment, string) {
	t.Helper()

	at := time.Now().UTC()
	if err := db.ReplaceSnapshot(advisory.EcosystemNPM, "snap-1", []advisory.Record{{
		ID: "GHSA-p6mc-m468-83gg", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
		Summary: "Prototype pollution",
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemNPM, Package: "lodash",
			Range: advisory.Range{Introduced: "0", Fixed: "4.17.21"},
		}},
	}}, advisory.FeedState{
		Ecosystem: advisory.EcosystemNPM, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 1, Origin: "sync",
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

	b := testBuilder(t, db)
	// **A licensed install, asked for by name.** The entitlement default is the
	// demo tier now, and dependency policy needs a trial license — an
	// unlicensed resolver is `Policy.Off()`, which is exactly the production
	// behaviour a lapse should have and exactly the wrong fixture for a test
	// about what the gate does when it is on.
	b.opts.Deps = &deps.Resolver{Store: db, Ents: entitlements.AtTier(entitlements.TierPaid)}

	app := mkApp(t, db, "gated", "")
	deploy := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusBuilding}
	if err := db.CreateDeployment(deploy, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"gated","dependencies":{"lodash":"^4.17.0"}}`)
	write(t, dir, "package-lock.json", `{
	  "lockfileVersion": 3,
	  "packages": {
	    "": {"dependencies": {"webpack": "^5.0.0"}},
	    "node_modules/webpack": {"version": "5.90.0", "dependencies": {"lodash": "^4.17.0"}},
	    "node_modules/lodash": {"version": "4.17.15"}
	  }
	}`)
	return b, app, deploy, dir
}

func TestGateRefusesAndSaysWhy(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyBlock)

	var log []string
	why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) })
	if why == "" {
		t.Fatalf("a critical finding with a fix should refuse in block mode\n%s", strings.Join(log, "\n"))
	}
	for _, want := range []string{
		"REFUSED", "lodash 4.17.15", "GHSA-p6mc-m468-83gg", "Fixed in 4.17.21",
		// The chain, because the app's own package.json names lodash at a
		// range this copy does not satisfy — the actionable fact is webpack.
		"Reached via", "Admin → Dependencies",
	} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal should mention %q:\n%s", want, why)
		}
	}

	// The inventory is written even for a refused deploy — it is exactly the
	// one whose package list somebody wants to read.
	list, err := db.ListDeploymentDependencies(deploy.ID)
	if err != nil {
		t.Fatalf("ListDeploymentDependencies: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("want the resolved inventory recorded, got %+v", list)
	}

	// And the verdict is stored on the release, which is what the bootstrap
	// handler reads instead of re-judging at 3am.
	v, ok, err := db.GetDeploymentVerdict(deploy.ID)
	if err != nil || !ok {
		t.Fatalf("GetDeploymentVerdict: ok=%v err=%v", ok, err)
	}
	if v.Allowed || !v.Scanned || v.Reason == "" {
		t.Errorf("verdict = %+v", v)
	}

	// The reading is written beside the verdict, and **a refused deploy is the
	// one somebody most wants back**: two facts with two homes, and the history
	// has no hole precisely where the app changed.
	scans, err := db.ListDependencyScans(app.ID, 10)
	if err != nil {
		t.Fatalf("ListDependencyScans: %v", err)
	}
	if len(scans) != 1 {
		t.Fatalf("the deploy wrote %d readings, want 1", len(scans))
	}
	scan := scans[0]
	if scan.Trigger != database.DependencyScanTriggerDeploy {
		t.Errorf("trigger = %q, want deploy", scan.Trigger)
	}
	if scan.DeploymentID != deploy.ID {
		t.Errorf("deployment_id = %q, want the release this reading is about", scan.DeploymentID)
	}
	if !scan.Scanned || scan.Packages != 2 || scan.Critical != 1 || scan.Counted != 1 {
		t.Errorf("scan = %+v", scan)
	}
	if len(scan.Advisories) != 1 || scan.Advisories[0] != "GHSA-p6mc-m468-83gg" {
		t.Errorf("advisories = %v", scan.Advisories)
	}
}

// `warn` records and shows exactly the same set and fails no build over a
// vulnerability. It is the mode an install runs in while it finds out whether
// its own estate can pass. (A malicious package refuses here too —
// deps.Policy.Refuses, TestDecideRefusesMalwareUnderWarn.)
func TestGateWarnRecordsAndDoesNotRefuse(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyWarn)

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("warn must not refuse: %s", why)
	}
	out := strings.Join(log, "\n")
	// The blocking one is still named, because on a warn install this is the
	// whole content of the warning.
	if !strings.Contains(out, "GHSA-p6mc-m468-83gg") {
		t.Errorf("warn should still name what would block:\n%s", out)
	}
	if !strings.Contains(out, "not enforced") {
		t.Errorf("warn should say it is not enforcing:\n%s", out)
	}
	if list, _ := db.ListDeploymentDependencies(deploy.ID); len(list) != 2 {
		t.Errorf("warn records the inventory too: %+v", list)
	}
	v, ok, _ := db.GetDeploymentVerdict(deploy.ID)
	if !ok || !v.Allowed || !v.Scanned {
		t.Errorf("verdict = %+v, want allowed and scanned", v)
	}
}

// Off means absent: no resolve step, no rows, nothing in the log.
func TestGateOffDoesNothingAtAll(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyOff)

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("off must not refuse: %s", why)
	}
	if len(log) != 0 {
		t.Errorf("off should write nothing to the build log: %v", log)
	}
	if list, _ := db.ListDeploymentDependencies(deploy.ID); len(list) != 0 {
		t.Errorf("off should record no inventory: %+v", list)
	}
	if _, ok, _ := db.GetDeploymentVerdict(deploy.ID); ok {
		t.Error("off should record no verdict")
	}
}

// A builder with no resolver at all is the same as off — the capability is
// absent rather than crashing, which is how every optional one behaves.
func TestGateWithNoResolverIsAbsent(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyBlock)
	b.opts.Deps = nil

	if why := b.dependencyGate(app, deploy, dir, func(string) {}); why != "" {
		t.Fatalf("a builder with no resolver must not refuse: %s", why)
	}
}

// With the deploy scan off there is no resolve step, and the release records
// nothing — which is *unknown*, and the surfaces must never render it as clean.
func TestGateHonoursTheDeployScanSwitch(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyWarn)

	settings, _ := db.GetPlatformSettings()
	settings.DependencyScanOnDeploy = false
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("must not refuse: %s", why)
	}
	if len(log) != 0 {
		t.Errorf("no scan means no log lines: %v", log)
	}
	if list, _ := db.ListDeploymentDependencies(deploy.ID); len(list) != 0 {
		t.Errorf("no scan means no inventory: %+v", list)
	}
}

// A waived finding stops the refusal and stays on the record.
func TestGateHonoursAWaiver(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyBlock)

	if err := db.GrantDependencyWaiver(&database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: "GHSA-p6mc-m468-83gg",
		Reason: "pinned by our webpack major; reviewed", ExpiresAt: time.Now().UTC().Add(48 * time.Hour),
	}, 90, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("GrantDependencyWaiver: %v", err)
	}

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("a waived finding must not refuse: %s", why)
	}
	if !strings.Contains(strings.Join(log, "\n"), "waived") {
		t.Errorf("the log should say one was waived:\n%s", strings.Join(log, "\n"))
	}
}

// A release with no corpus to check against does not block — `block` with no
// data is not enforcing, and says so rather than failing every deploy.
func TestGateDoesNotBlockWithNoAdvisoryData(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyBlock)

	// Empty the corpus, leaving no snapshot age at all.
	if _, err := db.Conn().Exec(`DELETE FROM advisory_feed_state`); err != nil {
		t.Fatalf("clearing the feed state: %v", err)
	}
	if _, err := db.Conn().Exec(`DELETE FROM advisories`); err != nil {
		t.Fatalf("clearing advisories: %v", err)
	}

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("no corpus means nothing to check against, so nothing to refuse: %s", why)
	}
	if !strings.Contains(strings.Join(log, "\n"), "no advisory data") {
		t.Errorf("the log should say why nothing was checked:\n%s", strings.Join(log, "\n"))
	}
	// And the deploy's own warnings say it, because nobody reads a log of a
	// deploy that succeeded and every verdict here reads "no known advisories".
	if w := strings.Join(DecodeWarnings(deploy.Warnings), "\n"); !strings.Contains(w, "No advisory data has loaded") {
		t.Errorf("an empty corpus should be a deploy warning, got %q", w)
	}
}

// Rollback carries the restored release's inventory and verdict rather than
// re-resolving, and is never blocked.
func TestCarryDependencyRecordOnRollback(t *testing.T) {
	db := dbtest.New(t)
	b, app, _, _ := gateFixture(t, db, database.DependencyPolicyBlock)

	source := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusSuccess}
	if err := db.CreateDeployment(source, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if err := db.SaveDeploymentDependencies(source.ID, []deps.Dependency{
		{Ecosystem: advisory.EcosystemNPM, Package: "lodash", Version: "4.17.21", Direct: true},
	}); err != nil {
		t.Fatalf("SaveDeploymentDependencies: %v", err)
	}
	if err := db.SetDeploymentVerdict(source.ID, deps.Verdict{
		Allowed: true, Mode: "block", Scanned: true, Packages: 1,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SetDeploymentVerdict: %v", err)
	}

	rollback := &database.Deployment{AppID: app.ID, Branch: "main", Status: database.DeployStatusBuilding}
	if err := db.CreateDeployment(rollback, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	var log []string
	b.carryDependencyRecord(source.ID, rollback, func(s string) { log = append(log, s) })

	list, _ := db.ListDeploymentDependencies(rollback.ID)
	if len(list) != 1 || list[0].Package != "lodash" {
		t.Errorf("the rollback should carry the inventory: %+v", list)
	}
	v, ok, _ := db.GetDeploymentVerdict(rollback.ID)
	if !ok || !v.Allowed || !v.Scanned {
		t.Errorf("the verdict should be carried as the release's: %+v", v)
	}
	if !strings.Contains(strings.Join(log, "\n"), "recorded inventory") {
		t.Errorf("the log should say what happened:\n%s", strings.Join(log, "\n"))
	}
}

// **R is recorded and not scanned, and those are two facts rather than one.**
//
// `deps.Resolve` deliberately keeps `cran` out of `Ecosystems` — the corpus is
// 14 advisories in its entire history, so claiming coverage would put a
// permanent "0 findings" on every R app. What it must not do is throw the
// package list away on the way past: the gate used to test the ecosystem count
// before it saved the inventory, so a Shiny app with a 61-package renv.lock
// recorded nothing, logged "No dependency manifest to read", and rendered on
// its own tab as an app whose packages *cannot be listed* — for a lockfile the
// builder had just parsed.
//
// Nothing refuses here because nothing in the CRAN corpus names shiny or
// ggplot2, which is the ordinary case rather than a rule — the companion test
// below is the one where an advisory does.
func TestGateRecordsRPackagesItCannotScan(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, _ := gateFixture(t, db, database.DependencyPolicyBlock)

	// A pure-R release: renv.lock and nothing else. Block mode on purpose, so
	// that "not scanned" is shown to be a property of the corpus rather than
	// of the mode.
	dir := t.TempDir()
	write(t, dir, "renv.lock", `{"Packages":{
	  "shiny":   {"Package":"shiny",   "Version":"1.9.1"},
	  "ggplot2": {"Package":"ggplot2", "Version":"4.0.1"}
	}}`)

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("no advisory names these packages, so nothing may refuse: %s", why)
	}
	out := strings.Join(log, "\n")

	// The inventory is the point.
	list, err := db.ListDeploymentDependencies(deploy.ID)
	if err != nil {
		t.Fatalf("ListDeploymentDependencies: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("renv.lock's packages should be recorded, got %+v\n%s", list, out)
	}
	for _, d := range list {
		if d.Ecosystem != advisory.EcosystemCRAN {
			t.Errorf("inventory row = %+v, want cran", d)
		}
	}

	// And the release is *not* scanned, which is what keeps the tab from
	// reporting a clean bill of health nothing supports.
	v, ok, err := db.GetDeploymentVerdict(deploy.ID)
	if err != nil || !ok {
		t.Fatalf("GetDeploymentVerdict: ok=%v err=%v", ok, err)
	}
	if v.Scanned {
		t.Errorf("a CRAN-only release is not scanned: %+v", v)
	}
	if !v.Allowed {
		t.Errorf("and with nothing found, it is not refused: %+v", v)
	}
	if v.Packages != 2 {
		t.Errorf("the verdict carries the count it recorded, got %d", v.Packages)
	}

	// The build log says both halves, and never the empty-release wording.
	if strings.Contains(out, "No dependency manifest to read") {
		t.Errorf("a parsed renv.lock is not a missing manifest:\n%s", out)
	}
	for _, want := range []string{"Recorded 2 package(s)", "not scanned"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log should say %q:\n%s", want, out)
		}
	}
	// **The empty state on an unscanned release is not "no known advisories".**
	// That sentence is the clean bill of health this whole design refuses to
	// write, and the log is the first of the five places it would appear.
	if strings.Contains(out, "No known advisories among them") {
		t.Errorf("an unscanned release may not report a clean bill of health:\n%s", out)
	}
	if !strings.Contains(out, "not a clean bill of health") {
		t.Errorf("the log should say what nothing-found means here:\n%s", out)
	}
}

// **A CRAN advisory that exists is enforced like any other, on a release that
// is still not scanned.** The two facts sit together and the surfaces have to
// carry both: `Scanned` reports whether *silence* is a verdict, and nothing
// about a thin corpus makes the fourteen records in it less true.
//
// This is the realistic shape rather than a contrived one. R deploys from
// `renv.lock`, which **pins** — so a lock written in 2023 carries jsonlite
// 1.8.4 into 2026 and RSEC-2023-3 still covers it, where an app rebuilt against
// today's CRAN would have moved past every advisory but one.
func TestGateEnforcesACRANAdvisoryOnAnUnscannedRelease(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, _ := gateFixture(t, db, database.DependencyPolicyBlock)

	at := time.Now().UTC()
	if err := db.ReplaceSnapshot(advisory.EcosystemCRAN, "snap-cran-1", []advisory.Record{{
		ID: "RSEC-2023-3", Kind: advisory.KindVuln, Severity: advisory.SeverityCritical,
		Summary: "Denial of service parsing deeply nested JSON",
		Affected: []advisory.Affected{{
			Ecosystem: advisory.EcosystemCRAN, Package: "jsonlite",
			Range: advisory.Range{Introduced: "0.9.12", Fixed: "1.8.8"},
		}},
	}}, advisory.FeedState{
		Ecosystem: advisory.EcosystemCRAN, LastSuccessAt: &at, SnapshotAt: &at,
		AdvisoryCount: 1, Origin: "sync",
	}); err != nil {
		t.Fatalf("ReplaceSnapshot: %v", err)
	}

	dir := t.TempDir()
	write(t, dir, "renv.lock", `{"Packages":{
	  "shiny":    {"Package":"shiny",    "Version":"1.9.1"},
	  "jsonlite": {"Package":"jsonlite", "Version":"1.8.4"}
	}}`)

	var log []string
	why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) })
	out := strings.Join(log, "\n")
	if why == "" {
		t.Fatalf("a critical CRAN advisory with a fix must refuse under block:\n%s", out)
	}
	if !strings.Contains(why, "RSEC-2023-3") {
		t.Errorf("the refusal names the advisory, got %q", why)
	}

	v, ok, err := db.GetDeploymentVerdict(deploy.ID)
	if err != nil || !ok {
		t.Fatalf("GetDeploymentVerdict: ok=%v err=%v", ok, err)
	}
	if v.Allowed {
		t.Errorf("the refusal is recorded on the release: %+v", v)
	}
	// The half that must survive the finding: a matched advisory does not make
	// the release scanned. Reading it the other way is how "R was checked and
	// is clear" gets said on the next release that finds nothing.
	if v.Scanned {
		t.Errorf("a CRAN-only release is still not scanned: %+v", v)
	}

	findings, err := db.FindingsFor(deploy.ID, app.ID, deps.Policy{
		Mode: database.DependencyPolicyBlock, Licensed: true,
		BlockSeverity: advisory.SeverityCritical,
	})
	if err != nil {
		t.Fatalf("FindingsFor: %v", err)
	}
	if len(findings) != 1 || findings[0].Package != "jsonlite" ||
		findings[0].Ecosystem != advisory.EcosystemCRAN {
		t.Fatalf("the finding is derived and stored, got %+v", findings)
	}
	if findings[0].FixedIn != "1.8.8" {
		t.Errorf("the way out travels with it, got %q", findings[0].FixedIn)
	}
}

// The other emptiness, kept apart from the one above: nothing resolved at all.
// Here "No dependency manifest to read" is the true sentence, and there is no
// inventory to record.
func TestGateRecordsNothingForAReleaseWithNoManifest(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, _ := gateFixture(t, db, database.DependencyPolicyWarn)

	dir := t.TempDir()
	write(t, dir, "index.html", "<h1>hello</h1>")

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("nothing to judge cannot refuse: %s", why)
	}
	out := strings.Join(log, "\n")
	if !strings.Contains(out, "No dependency manifest to read") {
		t.Errorf("the log should say so plainly:\n%s", out)
	}
	list, err := db.ListDeploymentDependencies(deploy.ID)
	if err != nil {
		t.Fatalf("ListDeploymentDependencies: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("nothing resolved, so nothing recorded, got %+v", list)
	}
	v, ok, _ := db.GetDeploymentVerdict(deploy.ID)
	if !ok || v.Scanned || v.Packages != 0 {
		t.Errorf("verdict = %+v ok=%v", v, ok)
	}
}

// Every gap kind has a deploy warning, so a kind added without one is noticed.
func TestEveryGapKindHasASentence(t *testing.T) {
	for _, k := range []string{deps.GapForeignLock, deps.GapUnlocked, deps.GapVendored, deps.GapUnresolved, deps.GapDirectOnly} {
		if got := gapSentences([]deps.Gap{{Kind: k, Ecosystem: "npm", File: "x"}}); len(got) != 1 || got[0] == "" {
			t.Errorf("%s has no sentence", k)
		}
	}
}

// A resolver that read the file and knows why it gave up says so; "could not
// be read" is only for a file that could not be (docs/bugs 2026-10-28 #4: a
// marimo requirements.txt was summarised as unreadable).
func TestAnUnresolvedGapWithAReasonIsNotCalledUnreadable(t *testing.T) {
	got := gapSentences([]deps.Gap{{Kind: deps.GapUnresolved, Ecosystem: "pypi", File: "requirements.txt",
		Reason: "pyyaml publishes no wheel for this Python"}})
	want := "requirements.txt's pypi dependencies were not checked: pyyaml publishes no wheel for this Python."
	if len(got) != 1 || got[0] != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
	got = gapSentences([]deps.Gap{{Kind: deps.GapUnresolved, Ecosystem: "npm", File: "package-lock.json"}})
	if len(got) != 1 || !strings.Contains(got[0], "could not be read") {
		t.Errorf("a real read failure = %q, want it still called unreadable", got)
	}
}

// A lockfile `npm ci` installs from and this platform does not read is not a
// way past `block`: the release is refused, not let through as not scanned.
// Under `warn` it goes ahead with a warning that names the file.
func TestGateRefusesAnUnreadableLockfileUnderBlock(t *testing.T) {
	for _, mode := range []string{database.DependencyPolicyBlock, database.DependencyPolicyWarn} {
		t.Run(mode, func(t *testing.T) {
			db := dbtest.New(t)
			b, app, deploy, dir := gateFixture(t, db, mode)
			write(t, dir, "package-lock.json",
				`{"lockfileVersion": 1, "dependencies": {"lodash": {"version": "4.17.15"}}}`)

			var log []string
			why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) })
			v, ok, err := db.GetDeploymentVerdict(deploy.ID)
			if err != nil || !ok {
				t.Fatalf("GetDeploymentVerdict: ok=%v err=%v", ok, err)
			}
			if mode == database.DependencyPolicyBlock {
				if !strings.HasPrefix(why, "REFUSED") || !strings.Contains(why, "package-lock.json") {
					t.Fatalf("block should refuse naming the lockfile, got %q\n%s", why, strings.Join(log, "\n"))
				}
				if v.Allowed || v.Reason == "" {
					t.Errorf("verdict = %+v, want refused", v)
				}
				return
			}
			if why != "" {
				t.Fatalf("warn must not refuse: %s", why)
			}
			if !v.Allowed || v.Scanned {
				t.Errorf("verdict = %+v, want allowed and not scanned", v)
			}
			if !strings.Contains(strings.Join(log, "\n"), "package-lock.json") {
				t.Errorf("warn should name the file it could not read:\n%s", strings.Join(log, "\n"))
			}
		})
	}
}

// LB-1 (security review 2026-11-15): one manifest value Postgres refuses — a
// NUL spelled \u0000 in a renv.lock — lost the whole inventory, and the gate
// then passed the release as allowed and not scanned under `block`. It is the
// app's own manifest being unreadable: refused under block, warned under warn,
// and the clean rows beside it are still recorded.
func TestGateRefusesAManifestValueItCannotRecordUnderBlock(t *testing.T) {
	for _, mode := range []string{database.DependencyPolicyBlock, database.DependencyPolicyWarn} {
		t.Run(mode, func(t *testing.T) {
			db := dbtest.New(t)
			b, app, deploy, dir := gateFixture(t, db, mode)
			write(t, dir, "package-lock.json", `{"lockfileVersion": 3, "packages": {
			  "": {"dependencies": {"lodash": "^4.17.0"}},
			  "node_modules/lodash": {"version": "4.17.21"}}}`)
			write(t, dir, "renv.lock", `{"Packages":{
			  "shiny": {"Package":"shiny", "Version":"1.9.1\u0000"}
			}}`)

			var log []string
			why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) })
			out := strings.Join(log, "\n")
			v, ok, err := db.GetDeploymentVerdict(deploy.ID)
			if err != nil || !ok {
				t.Fatalf("GetDeploymentVerdict: ok=%v err=%v\n%s", ok, err, out)
			}
			if mode == database.DependencyPolicyBlock {
				if !strings.HasPrefix(why, "REFUSED") || !strings.Contains(why, "renv.lock") {
					t.Fatalf("block should refuse naming renv.lock, got %q\n%s", why, out)
				}
				if v.Allowed {
					t.Errorf("verdict = %+v, want refused", v)
				}
				return
			}
			if why != "" {
				t.Fatalf("warn must not refuse: %s", why)
			}
			if !strings.Contains(out, "renv.lock") {
				t.Errorf("warn should name the file:\n%s", out)
			}
			rows, err := db.ListDeploymentDependencies(deploy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Package != "lodash" {
				t.Errorf("the clean rows should still be recorded, got %+v\n%s", rows, out)
			}
			if strings.Contains(out, "could not be recorded") {
				t.Errorf("the inventory insert failed:\n%s", out)
			}
		})
	}
}

// D3 follow-up: the scan resolves Python for the interpreter the build will
// install with — the release's `[runtime] python` — not the host default uv
// would discover. A non-Python framework, or a pin nothing satisfies, gets the
// install's default — never none, which would be uv's own discovery (DG-2).
func TestTheScanResolvesForTheReleasesPython(t *testing.T) {
	available := toolchain.Available(toolchain.Python)
	if len(available) == 0 {
		t.Skip("no Python on this host")
	}
	want := available[len(available)-1] // the lowest: not the default when there are two
	b := &Builder{}
	app := &database.App{}

	dir := t.TempDir()
	write(t, dir, "requirements.txt", "fastapi\n")
	write(t, dir, "main.py", "from fastapi import FastAPI\napp = FastAPI()\n")
	write(t, dir, "toft.toml", "[runtime]\npython = \""+want.Series()+"\"\n")
	if lang := LanguageOf(DetectFramework(dir).Framework); lang != toolchain.Python {
		t.Fatalf("fixture detected as %q, want a Python app", lang)
	}
	if got := b.scanPython(app, dir); got != want.Path {
		t.Errorf("scanPython = %q, want the pinned %s at %q", got, want.Version, want.Path)
	}

	// Never "" while the install has a Python: uv left to itself would run
	// an interpreter the release chose (DG-2).
	def := defaultScanPython()
	if def == "" {
		t.Fatal("no default Python, though Available listed one")
	}
	write(t, dir, "toft.toml", "[runtime]\npython = \"2.7\"\n")
	if got := b.scanPython(app, dir); got != def {
		t.Errorf("an unsatisfiable pin: scanPython = %q, want the default %q", got, def)
	}

	node := t.TempDir()
	write(t, node, "package.json", `{"name":"n","scripts":{"start":"node index.js"}}`)
	write(t, node, "index.js", "")
	write(t, node, "requirements.txt", "requests\n")
	if got := b.scanPython(app, node); got != def {
		t.Errorf("a Node app: scanPython = %q, want the default %q", got, def)
	}
}

// A dependency lock is lifted only by a release whose judgement ran and found
// nothing the lock acts on. Under `warn` with the lock at `critical`, the
// fixture's critical lodash deploys — and must still not lift the lock, or the
// next pass would only put it back.
func TestADeployLiftsADependencyLockOnlyWhenItsReleaseClearsIt(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyWarn)
	settings, _ := db.GetPlatformSettings()
	settings.DependencyLockOn = advisory.SeverityCritical
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
	if _, err := db.LockAppForDependency(app.ID, app.Slug, "Locked for GHSA-p6mc-m468-83gg.", nil, time.Now()); err != nil {
		t.Fatalf("LockAppForDependency: %v", err)
	}
	noLog := func(string) {}

	if why := b.liftDependencyLock(app, deploy, noLog); why == "" {
		t.Fatal("a deploy that was never judged lifted the lock")
	}

	if why := b.dependencyGate(app, deploy, dir, noLog); why != "" {
		t.Fatalf("warn refused: %s", why)
	}
	if why := b.liftDependencyLock(app, deploy, noLog); !strings.Contains(why, "still carries") {
		t.Fatalf("a release still carrying the critical finding: %q", why)
	}
	if fresh, _ := db.GetApp(app.ID); !fresh.LockedByPolicy() {
		t.Fatal("the lock moved")
	}

	// The fixed release.
	write(t, dir, "package-lock.json", `{"lockfileVersion": 3, "packages": {
	  "": {"dependencies": {"lodash": "^4.17.0"}},
	  "node_modules/lodash": {"version": "4.17.21"}}}`)
	if why := b.dependencyGate(app, deploy, dir, noLog); why != "" {
		t.Fatalf("the fixed release refused: %s", why)
	}
	if why := b.liftDependencyLock(app, deploy, noLog); why != "" {
		t.Fatalf("the fixed release did not lift the lock: %s", why)
	}
	if fresh, _ := db.GetApp(app.ID); fresh.Locked() {
		t.Error("still locked after a clean release")
	}
}

// Every gap kind's warning carries a code, so a deploy's warning_details has
// an entry for "no lockfile" and the rest, not a sentence alone (BB-017).
func TestEveryGapWarningIsCoded(t *testing.T) {
	want := map[string]string{
		deps.GapForeignLock: WarningLockfileNotRead, deps.GapUnlocked: WarningNoLockfile,
		deps.GapVendored: WarningVendoredUnchecked, deps.GapUnresolved: WarningDepsUnresolved,
		deps.GapDirectOnly: WarningDepsDirectOnly,
	}
	for k, code := range want {
		got := gapWarnings([]deps.Gap{{Kind: k, Ecosystem: "pypi", File: "x"}})
		if len(got) != 1 || got[0].Code != code || got[0].Message == "" {
			t.Errorf("%s = %+v, want code %s", k, got, code)
		}
	}
	var b Builder
	d := &database.Deployment{ID: "d"}
	b.addDeployWarningDetails(d, gapWarnings([]deps.Gap{{Kind: deps.GapUnlocked, Ecosystem: "pypi"}}))
	if det := DecodeWarningDetails(d.Warnings); len(det) != 1 || det[0].Code != WarningNoLockfile {
		t.Errorf("details = %+v", det)
	}
}

// A Yarn or pnpm lockfile is passed over, and the build log says so (BB-027).
func TestAPassedOverNodeLockIsLogged(t *testing.T) {
	dir := t.TempDir()
	if got := passedOverLockLines(dir); len(got) != 0 {
		t.Fatalf("no lock = %v", got)
	}
	os.WriteFile(filepath.Join(dir, "yarn.lock"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte("x"), 0o644)
	got := passedOverLockLines(dir)
	if len(got) != 2 || !strings.Contains(got[0], "pnpm-lock.yaml is passed over") ||
		!strings.Contains(got[1], "yarn.lock is passed over") || !strings.Contains(got[1], "from package.json") {
		t.Errorf("lines = %v", got)
	}
}

// A release whose manifest could not be read does not lift a dependency lock:
// it may still carry the package the lock was placed for (code review
// 2026-11-11, E-2).
func TestAnUnreadReleaseDoesNotLiftADependencyLock(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, dir := gateFixture(t, db, database.DependencyPolicyWarn)
	settings, _ := db.GetPlatformSettings()
	settings.DependencyLockOn = advisory.SeverityCritical
	if err := db.SetPlatformSettings(&settings, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetPlatformSettings: %v", err)
	}
	if _, err := db.LockAppForDependency(app.ID, app.Slug, "Locked for GHSA-p6mc-m468-83gg.", nil, time.Now()); err != nil {
		t.Fatalf("LockAppForDependency: %v", err)
	}
	noLog := func(string) {}
	// A lockfile npm wrote but this platform cannot read: under warn the deploy
	// goes on with a warning — and the lock stays.
	write(t, dir, "package-lock.json", `{"lockfileVersion": 3, "packages": {"": {}, "node_modules/x": `)
	if why := b.dependencyGate(app, deploy, dir, noLog); why != "" {
		t.Fatalf("warn refused: %s", why)
	}
	if why := b.liftDependencyLock(app, deploy, noLog); why == "" {
		t.Fatal("a release nobody could read lifted the lock")
	}
	if fresh, _ := db.GetApp(app.ID); !fresh.LockedByPolicy() {
		t.Fatal("the lock moved")
	}
}

// npm installs from npm-shrinkwrap.json when one is present and ignores
// package-lock.json, so every reader of the lock asks the same question (code
// review 2026-11-11, E-3).
func TestTheShrinkwrapIsTheLockWhenPresent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"demo","dependencies":{"lodash":"^4.17.0"}}`)
	write(t, dir, "package-lock.json", `{"lockfileVersion": 3, "packages": {"": {}, "node_modules/lodash": {"version": "4.17.21"}}}`)
	b := &Builder{}
	if _, args := b.npmInstallCommand(dir); args[0] != "ci" {
		t.Errorf("with a package-lock: %v", args)
	}
	write(t, dir, "npm-shrinkwrap.json", `{"lockfileVersion": 3, "packages": {"": {}, "node_modules/lodash": {"version": "4.17.15"}}}`)
	if _, args := b.npmInstallCommand(dir); args[0] != "ci" {
		t.Errorf("with a shrinkwrap: %v", args)
	}
	if lines := passedOverLockLines(dir); len(lines) != 0 {
		t.Errorf("a shrinkwrap is not passed over: %v", lines)
	}
	write(t, dir, "yarn.lock", "# yarn")
	if lines := passedOverLockLines(dir); len(lines) != 1 || !strings.Contains(lines[0], "from npm-shrinkwrap.json") {
		t.Errorf("the build log names the wrong lock: %v", lines)
	}
}

// DG-5 (security review 2026-11-14): with the npm corpus loaded and PyPI's
// never, a Python release is listed and **not scanned** — a deploy warning
// names the ecosystem, and the log never says "No known advisories".
func TestAReleaseInAnEcosystemWithNoCorpusIsNotScanned(t *testing.T) {
	db := dbtest.New(t)
	b, app, deploy, _ := gateFixture(t, db, database.DependencyPolicyBlock)
	dir := t.TempDir()
	write(t, dir, "requirements.txt", hashedRequirements) // read as the lock: no uv

	var log []string
	if why := b.dependencyGate(app, deploy, dir, func(s string) { log = append(log, s) }); why != "" {
		t.Fatalf("refused: %s", why)
	}
	out := strings.Join(log, "\n")
	if strings.Contains(out, "No known advisories") {
		t.Errorf("a release checked against nothing read as clean:\n%s", out)
	}
	v, ok, _ := db.GetDeploymentVerdict(deploy.ID)
	if !ok || v.Scanned {
		t.Errorf("verdict = %+v, want not scanned", v)
	}
	if w := strings.Join(DecodeWarnings(deploy.Warnings), "\n"); !strings.Contains(w, "No pypi advisory data") {
		t.Errorf("deploy warnings = %q, want one naming pypi", w)
	}
	if list, _ := db.ListDeploymentDependencies(deploy.ID); len(list) != 2 {
		t.Errorf("the inventory is still recorded: %+v", list)
	}
}
