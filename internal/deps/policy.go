// Package deps decides which third-party packages this install will run, and
// resolves what a build is about to install so the decision has something to
// act on.
//
// The decision itself is pure: Policy holds the administrator's settings and
// nothing else, and Policy.Blocks answers one finding at a time with no
// database and no clock. That is the ResolveSourcePolicy / ResolveRetention /
// ResolveAttachment pattern — one place that says what the policy is, so the
// builder, the bootstrap handler and the sweep cannot disagree about it.
//
// # What this constrains, and what it does not
//
// It reads the **names and versions** of an app's third-party packages and
// compares them to **published advisories**. It is not an app scanner: it does
// not read the app's code, find its SQL injection, or notice its hardcoded
// password. It does not stop a zero-day, by definition; it does not stop a
// malicious package nobody has reported yet, which is most of the window that
// matters; it does not stop a build script that curls a tarball; and it never
// stops an app that is already running. Every sentence of copy this package
// produces has to survive that, because a control that is oversold produces
// confidence rather than safety.
package deps

import (
	"fmt"
	"strings"
	"time"

	"toft/internal/advisory"
)

// Policy is the install's decision, resolved once and passed down.
//
// It carries the entitlement and the feed's state alongside the settings
// because the four gates are answered together — licensed, enabled, has data,
// not waived — and a refusal has to be able to name **which one**. "Not
// available" makes an operator open a ticket to discover which of four
// different things to do.
type Policy struct {
	// Mode is off, warn or block.
	Mode string `json:"mode"`

	ScanOnDeploy bool `json:"scan_on_deploy"`
	WatchEnabled bool `json:"watch_enabled"`

	BlockMalware bool `json:"block_malware"`
	// BlockSeverity is a threshold level, or DependencySeverityOff.
	BlockSeverity    string `json:"block_severity"`
	BlockOnlyFixable bool   `json:"block_only_fixable"`

	WaiverMaxDays int `json:"waiver_max_days"`

	// ScanInterval is how often the watch re-judges every serving release, and
	// HistoryKept is how many readings each app keeps. Both are the watch's own
	// dials rather than the feed's: a sweep is a local join, a feed refresh is
	// an outbound fetch of tens of megabytes, and one number for both is one
	// number that is wrong for one of them.
	ScanInterval time.Duration `json:"-"`
	HistoryKept  int           `json:"history_kept"`
	// LockOn is the threshold at which the platform takes a serving app off the
	// air by itself: off, malware, critical or high. `malware` by default, and
	// the only value the copy recommends. See LocksOn.
	LockOn string `json:"lock_on"`

	// FeedMaxAge is when the corpus starts being called stale. Staleness
	// warns; it never blocks.
	FeedMaxAge time.Duration `json:"-"`
	// FeedAsOf is the age of the *oldest* corpus the install keeps, or nil
	// while nothing has ever loaded. The oldest rather than the newest,
	// because a fresh npm corpus does not make a six-month-old PyPI one
	// current and a number that said otherwise would be the wrong kind of
	// reassuring.
	FeedAsOf *time.Time `json:"feed_as_of,omitempty"`
	// LoadedEcosystems is the covered ecosystems whose corpus has loaded at
	// least once. FeedAsOf is the oldest of them, so one loaded corpus made
	// it — and HasData — true for every other ecosystem, and a Python release
	// on an install that had only ever imported npm read as scanned with no
	// findings (security review 2026-11-14, DG-5). Scanned asks this per
	// ecosystem: see ScannedPackages.
	LoadedEcosystems map[string]bool `json:"-"`

	// Licensed is the entitlement, resolved by the caller — this package has
	// no opinion about licensing and no way to ask. LicenseReason carries the
	// words when it is false.
	Licensed      bool   `json:"licensed"`
	LicenseReason string `json:"license_reason,omitempty"`
}

// Off reports that the feature is not part of this install at all.
func (p Policy) Off() bool { return p.Mode == "" || p.Mode == "off" || !p.Licensed }

// HasData reports whether any corpus has ever loaded. While it is false,
// `block` does not block: an install that has never synced must not either
// fail every deploy or quietly pass every deploy, and it says which.
func (p Policy) HasData() bool { return p.FeedAsOf != nil }

// Enforcing reports whether a finding can actually stop a deploy right now.
//
// All four conditions, in one place. The one that surprises people is
// HasData: a `block` install whose feed has never synced is not enforcing, and
// every surface says so rather than showing a green tick over a control that
// has nothing to check against.
func (p Policy) Enforcing() bool {
	return p.Licensed && p.Mode == "block" && p.HasData()
}

// Stale reports whether the corpus is older than the administrator's warning
// threshold.
//
// **A stale feed warns; it never blocks.** This is deliberately the opposite
// of the source policy's fail-closed, and the difference is where the data
// lives. The source policy is a list this install holds: failing to read it
// means the database is down, and nothing else about a deploy works either.
// The feed is a document on somebody else's infrastructure, and an outage
// there — or a lapsed certificate on a customer's mirror — must not become
// "nobody at this company can deploy anything".
func (p Policy) Stale() bool {
	if p.FeedAsOf == nil || p.FeedMaxAge <= 0 {
		return false
	}
	return time.Since(*p.FeedAsOf) > p.FeedMaxAge
}

// Blocks reports whether one finding is a reason to refuse a deploy, ignoring
// waivers and ignoring the mode.
//
// Waivers are the caller's business because a waiver is per app and this
// object is per install; the mode is the caller's business because `warn`
// records exactly the same set and simply does not act on it. Keeping both out
// is what lets the tab say "this would block" on a `warn` install, which is
// the whole content of the warn → block preview.
func (p Policy) Blocks(f advisory.Finding) bool {
	if f.Malware() {
		// **Malware is not a severity.** There is no low-severity credential
		// stealer, so this is a boolean of its own and the threshold below
		// never gets a say — including for a malware advisory somebody scored
		// `moderate`, which happens.
		return p.BlockMalware
	}
	if p.BlockSeverity == "" || p.BlockSeverity == "off" {
		return false
	}
	if !advisory.AtOrAbove(f.Severity, p.BlockSeverity) {
		return false
	}
	if p.BlockOnlyFixable && strings.TrimSpace(f.FixedIn) == "" {
		// An unfixable transitive finding gives the developer nothing to do,
		// and a control whose only available response is a waiver trains
		// everyone to waive. Still recorded, still shown, still counted.
		return false
	}
	return true
}

// Refuses reports whether one finding refuses a deploy **right now**: Blocks,
// unwaived, under a policy that acts on it.
//
// **Malware refuses in `warn` too.** `warn` exists so that an administrator
// can see what a severity threshold would break before it breaks anything; a
// confirmed malicious package has no such question to preview — there is no
// build anybody wants to go through with one in it — so the mode does not get
// a say, exactly as the threshold does not. Everything else needs `block`.
// The gate, the app page's notice and the list glyph all ask this, so the
// three cannot disagree about what the next deploy will do.
func (p Policy) Refuses(f advisory.Finding) bool {
	if f.Waived || !p.Blocks(f) {
		return false
	}
	if f.Malware() {
		return !p.Off() && p.HasData()
	}
	return p.Enforcing()
}

// Counts reports that it cares: whether a finding is one this install has said
// it wants to see on a tab count or a list glyph.
//
// Deliberately *not* a second configurable threshold. An install with two
// numbers to explain has two numbers to get wrong, so what is counted is
// derived from what blocks — malware, plus anything at or above the severity
// threshold — while what is *recorded* stays everything.
func (p Policy) Counts(f advisory.Finding) bool {
	if f.Waived {
		// The visible reward for having dealt with one.
		return false
	}
	if f.Malware() {
		return true
	}
	return advisory.AtOrAbove(f.Severity, p.BlockSeverity)
}

// LocksOn reports whether one finding is a reason to take a *running* app off
// the air, ignoring waivers and ignoring whether the watch is on.
//
// **This is the one place §39's refusal is reversed, and it stays that
// narrow.** *A serving app is never stopped, slept, rolled back or degraded by
// this* rests on a specific failure: a third party publishes a bad or
// over-broad advisory at 3am and an estate of internal tools people are using
// goes down over somebody else's data quality. That reasoning is intact for
// vulnerabilities and holds unchanged, which is why `critical` and `high` are
// offered and default to off and the copy beside them carries the argument
// against them rather than a softened version.
//
// **Malware inverts it.** A package confirmed to be exfiltrating credentials
// is not a risk to the app; it is running inside the install, on the platform's
// own machine, with the app's environment, and every hour it keeps serving is
// the harm rather than the mitigation. The app most likely to be running one is
// the app nobody has redeployed since it was published — which is exactly the
// app the deploy gate never touches.
//
// Pure, beside Blocks and for Blocks's reason: no clock, no database, so a
// surface can say "this would lock" without anything happening.
func (p Policy) LocksOn(f advisory.Finding) bool {
	if p.LockOn == "" || p.LockOn == "off" {
		return false
	}
	if f.Malware() {
		// Malware is not a severity, so every non-off value locks on it — a
		// threshold of `high` that let a malicious package keep serving would
		// be a stricter setting behaving more permissively.
		return true
	}
	if p.LockOn == "malware" {
		return false
	}
	return advisory.AtOrAbove(f.Severity, p.LockOn)
}

// Locking reports whether an automatic lock can actually happen right now.
//
// The same four-gate shape as Enforcing, and for the same reason: an install
// whose corpus has never loaded is not enforcing anything, and a lock placed
// over an empty corpus is a lock placed over no evidence.
func (p Policy) Locking() bool {
	return !p.Off() && p.HasData() && p.LockOn != "" && p.LockOn != "off"
}

// Verdict is what a release was allowed to deploy under, decided once and
// stored on the deployment row.
//
// **A restart is not a deploy.** An isolated pod is evicted, rescheduled,
// OOM-killed or rolled by a node drain and bootstraps again with no new code
// and nothing anybody did. A bootstrap handler that evaluated the *current*
// feed would refuse that pod, and an app serving happily since March would
// simply not come back, at 3am, because a third party published an advisory.
// So the verdict travels with the release, and what new information changes is
// what happens to the **next** deploy.
type Verdict struct {
	// Allowed is false only when a gate actually refused. A release that was
	// never scanned is allowed and says so through Scanned.
	Allowed bool `json:"allowed"`
	// Reason is the refusal, in the one wording, when Allowed is false.
	Reason string `json:"reason,omitempty"`
	// Mode is what was in force when the decision was taken, so a tab can say
	// "recorded under warn" rather than implying today's mode applied.
	Mode string `json:"mode"`
	// Scanned reports that an inventory was resolved and recorded. False means
	// *unknown*, which must never render as "no findings".
	Scanned   bool      `json:"scanned"`
	CheckedAt time.Time `json:"checked_at"`
	// FeedAsOf is the corpus age the decision was taken against. Every number
	// this feature shows is scoped to a feed and its age.
	FeedAsOf *time.Time `json:"feed_as_of,omitempty"`
	// Packages is how many were resolved, for the build log and the tab.
	Packages int `json:"packages,omitempty"`
	// Incomparable is how many advisory ranges could not be compared with the
	// installed version (advisory.Incomparable): never findings, and counted
	// so that "no findings" is not read off ranges nothing could check.
	Incomparable int `json:"incomparable,omitempty"`
	// Ecosystems names what was resolved, so an app with a committed
	// node_modules can be told which half went unread.
	Ecosystems []string `json:"ecosystems,omitempty"`
	// Unread is what the resolution did not read, as Gaps. **Always written**
	// on a release that was resolved, empty when nothing was missed, so an
	// absent key means "built before this install recorded it" — which an SBOM
	// must say, rather than claim the release was complete.
	Unread []Gap `json:"unread"`
	// HashedRequirements is a PyPI inventory read from a requirements.txt that
	// pins and hashes every package — Resolution.HashedRequirements, kept so
	// an SBOM can name the file it was complete from.
	HashedRequirements bool `json:"hashed_requirements,omitempty"`
}

// UnreadOf is gaps as the verdict stores them: never nil, so the key is
// written.
func UnreadOf(gaps []Gap) []Gap {
	if gaps == nil {
		return []Gap{}
	}
	return gaps
}

// LeftUnresolved reports gaps that mean part of the release was never judged:
// a resolver that gave up, or one that read the direct dependencies alone.
// **Not judged is not clear**: a dependency lock is lifted only by a release
// whose whole set was read, so a deploy that could not be resolved never
// lifts the lock placed for a package it may still carry (code review
// 2026-11-11, E-2).
func LeftUnresolved(gaps []Gap) bool {
	for _, g := range gaps {
		if g.Kind == GapUnresolved || g.Kind == GapDirectOnly {
			return true
		}
	}
	return false
}

// AllowedVerdict is the verdict for a release nothing refused.
func AllowedVerdict(mode string, scanned bool, packages int, ecosystems []string, feedAsOf *time.Time) Verdict {
	return Verdict{
		Allowed: true, Mode: mode, Scanned: scanned, CheckedAt: time.Now().UTC(),
		FeedAsOf: feedAsOf, Packages: packages, Ecosystems: ecosystems,
	}
}

// Refusal is a deploy this install will not run, and why.
//
// It is one object rendered in five places — the build log, the API's error
// field, the app's Dependencies tab, the deploy's failure event, and the
// notice above the app's tabs — because five wordings of one rule is how a
// platform ends up sounding like it has five rules. That is
// `source_policy.go`'s comment, and it binds here for the same reason.
type Refusal struct {
	// Blocking is the findings that caused it, already sorted.
	Blocking []advisory.Finding
	// Threshold is the severity word in force, for the headline.
	Threshold string
	// FromGit says the app has a remote to push to. **An uploaded app has
	// nothing to push**, and "update the dependency and push again" is a lie
	// to its owner.
	FromGit bool
	// WaiversAvailable is false when dependency_waiver_max_days is 0. Offering
	// a waiver then sends the developer to an administrator who has no button.
	WaiversAvailable bool
}

func (r *Refusal) Error() string { return r.Message() }

// Message is the sentence, the findings, and the way out.
//
// The way out has to be **the reader's** way out, so three clauses vary. Each
// is a way a single fixed wording would be a lie to somebody, and a rule that
// names an action the reader cannot take is how a refusal gets escalated to
// support.
func (r *Refusal) Message() string {
	var b strings.Builder
	b.WriteString("REFUSED: ")
	b.WriteString(r.Headline())

	for _, f := range r.Blocking {
		b.WriteString("\n\n  ")
		b.WriteString(f.Package + " " + f.Version + "  →  " + f.AdvisoryID)
		if f.Severity != advisory.SeverityUnknown {
			b.WriteString("  (" + f.Severity + ")")
		} else if f.Malware() {
			b.WriteString("  (malicious package)")
		}
		if fix := strings.TrimSpace(f.FixedIn); fix != "" {
			b.WriteString("\n     Fixed in " + fix + ".")
		} else {
			b.WriteString("\n     No fixed version has been published.")
		}
		if p := strings.TrimSpace(f.Path); p != "" && !f.Direct {
			// Not decoration: "update lodash" is not actionable when the app's
			// own manifest has never named lodash, and the chain is the only
			// thing that says which of *their* dependencies to bump.
			b.WriteString(" Reached via: " + p)
		}
		if f.Source == SourcePlatform {
			b.WriteString("\n     Installed by Toft into every R release, not by this app.")
		}
	}

	for _, line := range r.clearAll() {
		b.WriteString("\n\n  " + line)
	}

	b.WriteString("\n\n")
	b.WriteString(r.WayOut())
	return b.String()
}

// clearAll names, for each package with several fixable findings, the one
// version that clears them all — the highest of their fixes. Each finding's
// own "Fixed in" is that advisory's, so a reader moving next 15.5.4 to the
// first fix listed (15.5.24) was refused again for an advisory fixed in
// 15.5.28 (D11). A package every finding of which is fixable only; with one
// unfixed, no version clears them all and the per-finding lines say so.
func (r *Refusal) clearAll() []string {
	type pkg struct {
		eco, name, version, max string
		n                       int
		unfixed                 bool
	}
	var order []string
	seen := map[string]*pkg{}
	for _, f := range r.Blocking {
		k := f.Ecosystem + "\x00" + f.Package + "\x00" + f.Version
		p := seen[k]
		if p == nil {
			p = &pkg{eco: f.Ecosystem, name: f.Package, version: f.Version}
			seen[k] = p
			order = append(order, k)
		}
		p.n++
		fix := strings.TrimSpace(f.FixedIn)
		if fix == "" {
			p.unfixed = true
			continue
		}
		if p.max == "" {
			p.max = fix
		} else if c, ok := advisory.Compare(f.Ecosystem, fix, p.max); ok && c > 0 {
			p.max = fix
		}
	}
	var out []string
	for _, k := range order {
		p := seen[k]
		if p.n < 2 || p.unfixed || p.max == "" {
			continue
		}
		out = append(out, fmt.Sprintf("To clear every advisory above on %s %s, move to %s or later.",
			p.name, p.version, p.max))
	}
	return out
}

// Headline is the one sentence, and it names the class rather than the count.
func (r *Refusal) Headline() string {
	var malware, vuln bool
	for _, f := range r.Blocking {
		if f.Malware() {
			malware = true
		} else {
			vuln = true
		}
	}
	level := r.Threshold
	if level == "" || level == "off" {
		level = "known"
	}
	switch {
	case malware && vuln:
		return fmt.Sprintf("this install does not deploy packages reported as malicious, "+
			"or packages with known %s advisories.", level)
	case malware:
		return "this install does not deploy packages reported as malicious."
	default:
		return fmt.Sprintf("this install does not deploy packages with known %s advisories.", level)
	}
}

// WayOut is the actionable half, and the part that varies by reader.
func (r *Refusal) WayOut() string {
	// Whether a code change is even available. When nothing that blocks has a
	// published fix, "update the dependency" is the advice that wastes an
	// afternoon — so the line says so and leads with the waiver.
	anyFixable := false
	for _, f := range r.Blocking {
		if strings.TrimSpace(f.FixedIn) != "" {
			anyFixable = true
			break
		}
	}

	update := "update the dependency and push again"
	if !r.FromGit {
		// An uploaded app has no remote. Keyed on the same distinction the app
		// page already draws.
		update = "update the dependency, rebuild, and upload the app again"
	}

	admins := "Administrators set the threshold on Admin → Dependencies."

	if r.allPlatform() {
		// Nothing in the app's code can move these, so "update the dependency"
		// would send its owner after a package they never chose.
		if !r.WaiversAvailable {
			return "No change to this app removes " + r.advisoryList() + ": Toft installs the " +
				"package itself. A Toft release that moves it is the way forward, and the " +
				"threshold is the only other thing that can move. " + admins
		}
		return "No change to this app removes " + r.advisoryList() + ": Toft installs the " +
			"package itself. Ask an administrator for a time-limited waiver on this app, or " +
			"wait for a Toft release that moves it. " + admins
	}

	switch {
	case !r.WaiversAvailable:
		// Waivers are off on this install, so there is exactly one way out and
		// it is not the reader's to take.
		return "The way forward is to " + update + ". This install does not grant waivers, so " +
			"the threshold is the only other thing that can move. " + admins
	case !anyFixable:
		return "No fix has been published for " + r.advisoryList() + ", so there is nothing to " +
			"update to yet. Ask an administrator for a time-limited waiver, or wait for a fixed " +
			"release. " + admins
	default:
		return "Two ways forward: " + update + ", or ask an administrator for a time-limited " +
			"waiver for " + r.advisoryList() + " on this app. " + admins
	}
}

// allPlatform is whether every blocking finding is a package Toft installed.
func (r *Refusal) allPlatform() bool {
	for _, f := range r.Blocking {
		if f.Source != SourcePlatform {
			return false
		}
	}
	return len(r.Blocking) > 0
}

// advisoryList names the advisories, so a waiver request is a copy and paste
// rather than a hunt through a build log.
func (r *Refusal) advisoryList() string {
	seen := map[string]bool{}
	var ids []string
	for _, f := range r.Blocking {
		if !seen[f.AdvisoryID] {
			seen[f.AdvisoryID] = true
			ids = append(ids, f.AdvisoryID)
		}
	}
	switch len(ids) {
	case 0:
		return "these advisories"
	case 1:
		return ids[0]
	case 2:
		return ids[0] + " and " + ids[1]
	default:
		return strings.Join(ids[:len(ids)-1], ", ") + " and " + ids[len(ids)-1]
	}
}

// ServingRelease is one app and the release it is running, as the sweep and
// the on-demand re-check see it.
//
// It lives here rather than beside the sweep because `database` returns it and
// `database` imports this package. (The sweep itself is in `internal/depsweep`
// for the mirror-image reason: it needs `internal/events`, which imports
// `database`, so a sweep in this package would close a cycle.)
type ServingRelease struct {
	AppID        string
	Slug         string
	Status       string
	DeploymentID string
	// Stopped apps are swept and recorded like any other. A stopped app is not
	// *exposure* — which matters to the estate view's headline count — but it
	// is still a fact about the app, and a sweep that skipped them would make
	// the tab disagree with the estate view the moment somebody stopped an app.
	Stopped bool
	// BaselinedAt is nil until this app has been evaluated once. The first
	// evaluation records everything and notifies nobody.
	BaselinedAt *time.Time
	// Framework, RunningMode and ReleaseRef are the app's, for the one reading
	// the sweep takes off disk: a shared R app's library (depsweep).
	Framework   string
	RunningMode string
	ReleaseRef  string
}

// CoveredPackages is how many of an inventory's rows are in an ecosystem whose
// silence this build treats as a verdict.
//
// **This is what "scanned" means, and it is not "an inventory exists" — nor is
// it "findings were derived".** The first two came apart when R's packages
// started being recorded; the third when CRAN's corpus started being matched.
// A release whose whole list is R has been read completely and checked against
// fourteen advisories, so it can honestly carry a finding *and* read as not
// scanned: what it cannot support is the inference from no findings to no
// problem. Deriving Scanned from the row count, or from the finding count,
// would render that as a clean bill of health.
//
// Here rather than beside its first caller because it now has four — the app's
// tab, the estate assembly, the sweep's history row and the estate query's SQL
// — and they have to agree.
//
// It is not the "scanned" test any more: a covered ecosystem whose corpus has
// never loaded was checked against nothing. ScannedPackages is that test.
func CoveredPackages(inventory []Dependency) int {
	n := 0
	for _, d := range inventory {
		if advisory.Covered(d.Ecosystem) {
			n++
		}
	}
	return n
}

// CorpusLoaded reports whether ecosystem's advisory corpus has ever loaded on
// this install.
func (p Policy) CorpusLoaded(ecosystem string) bool { return p.LoadedEcosystems[ecosystem] }

// ScannedPackages is how many packages of inventory are in a covered
// ecosystem whose corpus has loaded — **the one test of "scanned"**. Above
// zero, the release was checked against something; zero with CoveredPackages
// above it is `no_corpus`, never a clean bill of health (DG-5). The app's tab,
// the app-list block, the sweep's history row and the deploy gate all ask it,
// and the estate query spells it in SQL against advisory_feed_state.
func ScannedPackages(p Policy, inventory []Dependency) int {
	n := 0
	for _, d := range inventory {
		if advisory.Covered(d.Ecosystem) && p.CorpusLoaded(d.Ecosystem) {
			n++
		}
	}
	return n
}

// WithoutCorpus is the covered ecosystems among ecosystems whose corpus has
// never loaded, in the order given.
func WithoutCorpus(p Policy, ecosystems []string) []string {
	var out []string
	for _, e := range ecosystems {
		if advisory.Covered(e) && !p.CorpusLoaded(e) {
			out = append(out, e)
		}
	}
	return out
}
