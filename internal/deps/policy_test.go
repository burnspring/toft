package deps

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"toft/internal/advisory"
)

func base() Policy {
	return Policy{
		Mode: "block", Licensed: true, ScanOnDeploy: true, WatchEnabled: true,
		BlockMalware: true, BlockSeverity: advisory.SeverityCritical, BlockOnlyFixable: true,
		WaiverMaxDays: 90, FeedMaxAge: 72 * time.Hour, FeedAsOf: ptr(time.Now()),
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestBlocksSeparatesMalwareFromSeverity(t *testing.T) {
	p := base()

	// The whole reason malware is its own control: a credential stealer scored
	// `moderate` is below a `critical` threshold, and there is no such thing
	// as a low-severity credential stealer.
	mal := advisory.Finding{Kind: advisory.KindMalware, Severity: advisory.SeverityModerate}
	if !p.Blocks(mal) {
		t.Error("malware must block regardless of the severity threshold")
	}
	// And it has no fix, ever — so only-fixable must not exempt it either.
	if !p.Blocks(advisory.Finding{Kind: advisory.KindMalware}) {
		t.Error("malware with no fixed version must still block")
	}

	p.BlockMalware = false
	if p.Blocks(mal) {
		t.Error("with the malware switch off, a moderate malware finding must not block")
	}
}

func TestBlocksSeverityThreshold(t *testing.T) {
	p := base()
	critical := advisory.Finding{Severity: advisory.SeverityCritical, FixedIn: "2.0.0"}
	high := advisory.Finding{Severity: advisory.SeverityHigh, FixedIn: "2.0.0"}

	if !p.Blocks(critical) {
		t.Error("critical should block at a critical threshold")
	}
	if p.Blocks(high) {
		t.Error("high should not block at a critical threshold")
	}

	p.BlockSeverity = advisory.SeverityHigh
	if !p.Blocks(high) {
		t.Error("high should block once the threshold moves")
	}

	// An advisory nobody scored can never clear a bar somebody set.
	if p.Blocks(advisory.Finding{Severity: advisory.SeverityUnknown, FixedIn: "2.0.0"}) {
		t.Error("an unscored advisory must not block")
	}

	// "off" is a settable threshold that turns severity blocking off without
	// turning the feature off — and must not be confused with an advisory's
	// own empty severity.
	p.BlockSeverity = "off"
	if p.Blocks(critical) {
		t.Error("with the threshold off, nothing should block on severity")
	}
	if !p.Blocks(advisory.Finding{Kind: advisory.KindMalware}) {
		t.Error("the threshold being off must not switch malware blocking off too")
	}
}

func TestBlocksOnlyFixable(t *testing.T) {
	p := base()
	unfixable := advisory.Finding{Severity: advisory.SeverityCritical}
	if p.Blocks(unfixable) {
		t.Error("an unfixable finding must not block while only-fixable is on")
	}
	p.BlockOnlyFixable = false
	if !p.Blocks(unfixable) {
		t.Error("with only-fixable off, it should block")
	}
}

// All four gates in one place, and the one that surprises people is HasData:
// a `block` install whose feed has never loaded is not enforcing anything, and
// must say so rather than either failing or passing every deploy silently.
func TestEnforcingNeedsAllFourGates(t *testing.T) {
	p := base()
	if !p.Enforcing() {
		t.Fatal("the baseline policy should be enforcing")
	}

	off := p
	off.Mode = "warn"
	if off.Enforcing() {
		t.Error("warn must not enforce")
	}

	unlicensed := p
	unlicensed.Licensed = false
	if unlicensed.Enforcing() {
		t.Error("an unlicensed install must not enforce")
	}
	if !unlicensed.Off() {
		t.Error("an unlicensed install reads as off")
	}

	noData := p
	noData.FeedAsOf = nil
	if noData.Enforcing() {
		t.Error("block with no corpus must not enforce")
	}
	if noData.HasData() {
		t.Error("HasData should be false with no corpus")
	}
}

// A stale feed warns and never blocks — the opposite of the source policy's
// fail-closed, because somebody else's outage must not become "nobody at this
// company can deploy anything".
func TestStaleWarnsAndStillEnforces(t *testing.T) {
	p := base()
	old := time.Now().Add(-100 * time.Hour)
	p.FeedAsOf = &old
	if !p.Stale() {
		t.Error("a 100-hour-old corpus is stale against a 72-hour threshold")
	}
	if !p.Enforcing() {
		t.Error("staleness must not stop enforcement — it warns")
	}

	p.FeedMaxAge = 0
	if p.Stale() {
		t.Error("with no threshold configured nothing is stale")
	}
}

// What is counted on a tab is derived from what blocks rather than being a
// second configurable threshold: an install with two numbers to explain has
// two numbers to get wrong.
func TestCountsFollowsTheThresholdAndDropsWaived(t *testing.T) {
	p := base()
	if !p.Counts(advisory.Finding{Severity: advisory.SeverityCritical}) {
		t.Error("a critical finding should count")
	}
	// Counted even though only-fixable stops it blocking: what is *recorded*
	// and shown is everything at or above the threshold.
	if !p.Counts(advisory.Finding{Severity: advisory.SeverityCritical, FixedIn: ""}) {
		t.Error("an unfixable critical finding should still count")
	}
	if p.Counts(advisory.Finding{Severity: advisory.SeverityLow}) {
		t.Error("a low finding should not count at a critical threshold")
	}
	if !p.Counts(advisory.Finding{Kind: advisory.KindMalware, Severity: advisory.SeverityLow}) {
		t.Error("malware always counts")
	}
	// The visible reward for having dealt with one.
	if p.Counts(advisory.Finding{Severity: advisory.SeverityCritical, Waived: true}) {
		t.Error("a waived finding must not count")
	}
}

func TestRefusalNamesThePackageAdvisoryAndWayOut(t *testing.T) {
	r := &Refusal{
		Threshold:        advisory.SeverityCritical,
		FromGit:          true,
		WaiversAvailable: true,
		Blocking: []advisory.Finding{{
			AdvisoryID: "GHSA-p6mc-m468-83gg", Kind: advisory.KindVuln,
			Severity: advisory.SeverityCritical, Package: "lodash", Version: "4.17.15",
			FixedIn: "4.17.21", Path: "(this app) > webpack > lodash",
		}},
	}
	msg := r.Message()
	for _, want := range []string{
		"REFUSED",
		"known critical advisories",
		"lodash 4.17.15",
		"GHSA-p6mc-m468-83gg",
		"Fixed in 4.17.21",
		// The chain is the only thing that says which of *their* dependencies
		// to bump when their own manifest has never named lodash.
		"Reached via: (this app) > webpack > lodash",
		"push again",
		"time-limited waiver",
		"Admin → Dependencies",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should mention %q:\n%s", want, msg)
		}
	}
}

// Three clauses vary, and each is a way one fixed wording would be a lie to
// somebody.
func TestRefusalWayOutVariesByReader(t *testing.T) {
	fixable := advisory.Finding{AdvisoryID: "GHSA-1", Severity: advisory.SeverityCritical,
		Package: "x", Version: "1.0.0", FixedIn: "1.0.1"}
	unfixable := advisory.Finding{AdvisoryID: "GHSA-2", Severity: advisory.SeverityCritical,
		Package: "y", Version: "1.0.0"}

	t.Run("an uploaded app has nothing to push", func(t *testing.T) {
		r := &Refusal{Blocking: []advisory.Finding{fixable}, FromGit: false, WaiversAvailable: true}
		out := r.WayOut()
		if strings.Contains(out, "push again") {
			t.Errorf("an uploaded app cannot push: %s", out)
		}
		if !strings.Contains(out, "upload the app again") {
			t.Errorf("it should be told to upload: %s", out)
		}
	})

	t.Run("waivers off means no waiver is offered", func(t *testing.T) {
		r := &Refusal{Blocking: []advisory.Finding{fixable}, FromGit: true, WaiversAvailable: false}
		out := r.WayOut()
		if strings.Contains(out, "ask an administrator for a time-limited waiver") {
			t.Errorf("offering a waiver sends them to an admin with no button: %s", out)
		}
		if !strings.Contains(out, "does not grant waivers") {
			t.Errorf("it should say so plainly: %s", out)
		}
	})

	t.Run("no published fix leads with the waiver", func(t *testing.T) {
		r := &Refusal{Blocking: []advisory.Finding{unfixable}, FromGit: true, WaiversAvailable: true}
		out := r.WayOut()
		if !strings.Contains(out, "No fix has been published") {
			t.Errorf("it should say there is nothing to update to: %s", out)
		}
		if strings.Contains(out, "Two ways forward") {
			t.Errorf("there is only one way forward here: %s", out)
		}
		if !strings.Contains(out, "GHSA-2") {
			t.Errorf("the waiver request should name the advisory: %s", out)
		}
	})
}

func TestRefusalHeadlineNamesTheClass(t *testing.T) {
	mal := advisory.Finding{AdvisoryID: "MAL-1", Kind: advisory.KindMalware, Package: "evil"}
	vuln := advisory.Finding{AdvisoryID: "GHSA-1", Severity: advisory.SeverityCritical, Package: "x"}

	only := (&Refusal{Blocking: []advisory.Finding{mal}, Threshold: advisory.SeverityCritical}).Headline()
	if !strings.Contains(only, "malicious") || strings.Contains(only, "critical") {
		t.Errorf("a malware-only refusal should not talk about a severity threshold: %s", only)
	}

	both := (&Refusal{Blocking: []advisory.Finding{mal, vuln}, Threshold: advisory.SeverityCritical}).Headline()
	if !strings.Contains(both, "malicious") || !strings.Contains(both, "critical") {
		t.Errorf("a mixed refusal should name both: %s", both)
	}
}

// LocksOn is the one place §39's "a serving app is never stopped" is reversed,
// and the tests are about how narrow that reversal is.
func TestLocksOnIsMalwareFirstAndOffByDefault(t *testing.T) {
	p := base()
	p.LockOn = ""

	mal := advisory.Finding{Kind: advisory.KindMalware, Severity: advisory.SeverityModerate}
	crit := advisory.Finding{Severity: advisory.SeverityCritical}

	// Unset and "off" are the same answer, and it is the answer that takes
	// nothing off the air.
	for _, v := range []string{"", "off"} {
		p.LockOn = v
		if p.LocksOn(mal) || p.LocksOn(crit) {
			t.Errorf("LockOn=%q locked something; the default must lock nothing", v)
		}
	}

	// `malware` is the recommended value, and it is *only* malware: a critical
	// vulnerability at this setting is recorded, shown, counted and left
	// serving, which is the half of §39 that is unchanged.
	p.LockOn = "malware"
	if !p.LocksOn(mal) {
		t.Error("LockOn=malware must lock on a malicious package")
	}
	if p.LocksOn(crit) {
		t.Error("LockOn=malware must not lock on a critical vulnerability")
	}

	// A stricter threshold can never be more permissive about malware — a
	// `high` install that let a credential stealer keep serving would be the
	// setting behaving backwards.
	for _, v := range []string{advisory.SeverityCritical, advisory.SeverityHigh} {
		p.LockOn = v
		if !p.LocksOn(mal) {
			t.Errorf("LockOn=%q must still lock on malware", v)
		}
	}

	p.LockOn = advisory.SeverityHigh
	if !p.LocksOn(advisory.Finding{Severity: advisory.SeverityHigh}) {
		t.Error("LockOn=high must lock on a high advisory")
	}
	if p.LocksOn(advisory.Finding{Severity: advisory.SeverityModerate}) {
		t.Error("LockOn=high must not lock on a moderate advisory")
	}
	// Nobody scored it, so it meets no threshold — the same rule Blocks keeps.
	if p.LocksOn(advisory.Finding{Severity: advisory.SeverityUnknown}) {
		t.Error("an unscored advisory must meet no lock threshold")
	}
}

// The lock threshold is deliberately not the block threshold: one decides what
// refuses a deploy with a person waiting, the other what takes a running app
// off the air with nobody present.
func TestLockThresholdIsIndependentOfTheBlockThreshold(t *testing.T) {
	p := base()
	p.BlockSeverity = advisory.SeverityLow
	p.BlockMalware = true
	p.LockOn = "off"

	f := advisory.Finding{Severity: advisory.SeverityCritical, FixedIn: "2.0.0"}
	if !p.Blocks(f) {
		t.Fatal("fixture: a critical finding should block at a low threshold")
	}
	if p.LocksOn(f) {
		t.Error("an install that set block_severity did not thereby ask for its estate to go dark")
	}
}

// Locking folds the same four gates Enforcing does, and the one that matters is
// HasData: a lock over an empty corpus is a lock over no evidence.
func TestLockingNeedsALicenseAModeAndACorpus(t *testing.T) {
	p := base()
	p.LockOn = "malware"
	if !p.Locking() {
		t.Fatal("fixture: a licensed, enabled install with a corpus should be able to lock")
	}

	off := p
	off.Mode = "off"
	if off.Locking() {
		t.Error("a policy that is off locks nothing")
	}
	unlicensed := p
	unlicensed.Licensed = false
	if unlicensed.Locking() {
		t.Error("an unlicensed install locks nothing")
	}
	noCorpus := p
	noCorpus.FeedAsOf = nil
	if noCorpus.Locking() {
		t.Error("a lock over a corpus that has never loaded is a lock over no evidence")
	}
	// A `warn` install still locks. The mode governs the *deploy* gate, and
	// this control is not that gate — an administrator who set a lock
	// threshold set it on purpose.
	warn := p
	warn.Mode = "warn"
	if !warn.Locking() {
		t.Error("warn is about the deploy gate; it must not silently disable the lock")
	}
}

func TestLocksOnIsPure(t *testing.T) {
	p := base()
	p.LockOn = "malware"
	f := advisory.Finding{Kind: advisory.KindMalware}
	before := p
	_ = p.LocksOn(f)
	if !reflect.DeepEqual(p, before) {
		t.Error("LocksOn changed the policy it was asked about")
	}
}

// Refuses is the one answer to "does the next deploy fail": malware in `warn`
// as well as `block`, everything else only under `block` — and nothing at all
// with the policy off, no corpus, or a waiver.
func TestRefusesMalwareInWarnAndTheRestOnlyInBlock(t *testing.T) {
	mal := advisory.Finding{Kind: advisory.KindMalware}
	crit := advisory.Finding{Severity: advisory.SeverityCritical, FixedIn: "2.0.0"}

	p := base()
	if !p.Refuses(mal) || !p.Refuses(crit) {
		t.Error("block must refuse both malware and a fixable critical")
	}

	p.Mode = "warn"
	if !p.Refuses(mal) {
		t.Error("warn must still refuse a malicious package: there is no build anybody wants to preview with one in it")
	}
	if p.Refuses(crit) {
		t.Error("warn must not refuse a vulnerability: previewing that is what warn is for")
	}

	for name, q := range map[string]Policy{
		"off":        func() Policy { q := base(); q.Mode = "off"; return q }(),
		"unlicensed": func() Policy { q := base(); q.Mode = "warn"; q.Licensed = false; return q }(),
		"no corpus":  func() Policy { q := base(); q.Mode = "warn"; q.FeedAsOf = nil; return q }(),
		"switch off": func() Policy { q := base(); q.Mode = "warn"; q.BlockMalware = false; return q }(),
	} {
		if q.Refuses(mal) {
			t.Errorf("%s: malware was refused", name)
		}
	}

	waived := mal
	waived.Waived = true
	if base().Refuses(waived) {
		t.Error("a waiver silences the refusal")
	}
}

// The gate asks Refuses, so a `warn` install's deploy with a malicious package
// in it fails, naming only that package, while a critical beside it is recorded.
func TestDecideRefusesMalwareUnderWarn(t *testing.T) {
	p := base()
	p.Mode = "warn"
	mal := advisory.Finding{Kind: advisory.KindMalware, Package: "evil", AdvisoryID: "MAL-1", WouldBlock: true}
	crit := advisory.Finding{Severity: advisory.SeverityCritical, Package: "lodash", AdvisoryID: "GHSA-1",
		FixedIn: "2.0.0", WouldBlock: true}

	d := Decide(p, []advisory.Finding{mal, crit}, true, true, 2, []string{"npm"})
	if d.Refusal == nil || d.Verdict.Allowed {
		t.Fatalf("warn with malware must refuse: %+v", d.Verdict)
	}
	if len(d.Refusal.Blocking) != 1 || d.Refusal.Blocking[0].Package != "evil" {
		t.Errorf("the refusal must name only the malware: %+v", d.Refusal.Blocking)
	}

	d = Decide(p, []advisory.Finding{crit}, true, true, 1, []string{"npm"})
	if d.Refusal != nil || !d.Verdict.Allowed {
		t.Errorf("warn with only a critical must not refuse: %+v", d.Verdict)
	}
}

// A package Toft installed into every R release is not the app's to update,
// and its refusal must not tell the owner to update it.
func TestRefusalForAPlatformPackageNamesWhoseFixItIs(t *testing.T) {
	curl := advisory.Finding{AdvisoryID: "RSEC-1", Severity: advisory.SeverityCritical,
		Ecosystem: "cran", Package: "curl", Version: "5.2.3", FixedIn: "5.2.4", Source: SourcePlatform}
	own := advisory.Finding{AdvisoryID: "RSEC-2", Severity: advisory.SeverityCritical,
		Ecosystem: "cran", Package: "dplyr", Version: "1.0.0", FixedIn: "1.0.1", Source: SourceInstalled}

	r := &Refusal{Blocking: []advisory.Finding{curl}, FromGit: true, WaiversAvailable: true}
	msg := r.Message()
	if !strings.Contains(msg, "Installed by Toft into every R release") {
		t.Errorf("the row should say Toft installed it: %s", msg)
	}
	if strings.Contains(msg, "update the dependency") || !strings.Contains(msg, "No change to this app removes RSEC-1") {
		t.Errorf("the way out should be a waiver or a Toft release: %s", msg)
	}

	mixed := &Refusal{Blocking: []advisory.Finding{curl, own}, FromGit: true, WaiversAvailable: true}
	if !strings.Contains(mixed.WayOut(), "update the dependency") {
		t.Errorf("an app's own package is still the app's to update: %s", mixed.WayOut())
	}
}

// D11: several fixable advisories on one package name the one version that
// clears them all, not only each one's own fix.
func TestARefusalNamesTheVersionThatClearsEveryAdvisory(t *testing.T) {
	r := &Refusal{Threshold: "high", FromGit: true, Blocking: []advisory.Finding{
		{AdvisoryID: "A", Ecosystem: advisory.EcosystemNPM, Package: "next", Version: "15.5.4", FixedIn: "15.5.24", Severity: "high"},
		{AdvisoryID: "B", Ecosystem: advisory.EcosystemNPM, Package: "next", Version: "15.5.4", FixedIn: "15.5.28", Severity: "high"},
		{AdvisoryID: "C", Ecosystem: advisory.EcosystemNPM, Package: "next", Version: "15.5.4", FixedIn: "15.5.7", Severity: "high"},
	}}
	if msg := r.Message(); !strings.Contains(msg, "on next 15.5.4, move to 15.5.28 or later") {
		t.Fatalf("message = %s", msg)
	}
}
