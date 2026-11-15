package builder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/deps"
	"toft/internal/events"
	"toft/internal/runtime"
	"toft/internal/telemetry"
	"toft/internal/toolchain"
)

// Enforcement point 1 of three: the deploy's scan phase, between the source
// landing and anything being built from it.
//
// It is the **first** point at which anything is knowable. Unlike a repository
// URL, nothing can be said about an app's dependencies until its code has been
// cloned — so unlike the source policy there is no check at POST /apps, the
// failure mode of this control is always a deploy that fails rather than a
// form that refuses, and the copy has to carry the whole explanation.
//
// The scan runs before `npm ci` and before `uv pip install`, which is the
// point: after them the package is on the disk, and if it has a lifecycle
// script and BUILD_ALLOW_SCRIPTS is set, it has already run. It used to run in
// the build step, one statement before the install; it is now a step of its
// own, several steps earlier, which changes when it refuses but not what it
// can refuse — everything between the two positions detects and configures,
// and none of it installs.

// dependencyGate resolves this build's dependencies, records them, judges them
// and returns a refusal to fail the build with, or "" to go ahead.
//
// It **fails open on its own errors** — and closed only on the app's own
// manifest being unreadable under `block` (see deps.UnreadRefusal) — which is the
// opposite of the source policy three files away, and the difference is worth
// stating because the two sit close together. A source policy the platform
// cannot read is a security control whose failure costs a clone from wherever
// the URL points, and a settings read that fails means the database is down so
// nothing else works either. This one's inputs include a third party's corpus
// and a resolver that shells out to npm: treating either failing as a refusal
// would convert somebody else's bad afternoon into "nobody here can deploy".
// What it does instead is say so, in the build log, and record that the
// release was **not scanned** — which is a state with a name and never reads
// as a clean bill of health.
func (b *Builder) dependencyGate(app *database.App, deploy *database.Deployment,
	releaseDir string, appendLog func(string)) string {

	policy, err := b.opts.Deps.Policy()
	if err != nil {
		appendLog(fmt.Sprintf("    Warning: this install's dependency policy could not be read, "+
			"so dependencies were not checked: %v", err))
		return ""
	}
	if policy.Off() || !policy.ScanOnDeploy {
		// Absent, not dormant: no resolve step, no rows, nothing in the log.
		// An install that has turned the deploy scan off gets no line either —
		// narrating a decision somebody made on every build is noise, and the
		// app's own tab is where that state is explained.
		return ""
	}

	appendLog(b.feedHeadline(policy))
	if !policy.HasData() {
		// A deploy warning as well as the log line: every verdict taken now
		// reads "no known advisories", and the log is not where anybody looks
		// for that. Not a refusal — an install that has never synced must not
		// become one where nobody can deploy (see Policy.HasData).
		b.addDeployWarningDetails(deploy, []Warning{{Code: WarningAdvisoryDataMissing,
			Message: "No advisory data has loaded on this install yet, " +
				"so this release's dependencies were listed but not checked against any."}})
	}

	python, earlier := b.scanPythons(app, releaseDir)
	res, err := deps.ResolveWith(context.Background(), releaseDir, deps.Options{
		Python: python, EarlierPythons: earlier,
	}, func(format string, args ...any) {
		appendLog(fmt.Sprintf(format, args...))
	})
	if err != nil {
		// A manifest the build is about to install from could not be read.
		// That is the app's file, not this gate's failure, so under `block`
		// it refuses: a lockfile `npm ci` installs from and this platform does
		// not read was otherwise a way past the policy. Under `warn` it is a
		// warning naming the file, and whatever the other resolvers read is
		// still recorded and judged below.
		appendLog(fmt.Sprintf("    Some dependencies could not be resolved, so they were not checked: %v", err))
		telemetry.RecordDependencyScan(context.Background(), "mixed", telemetry.OutcomeFailed)
		var unread *deps.UnreadError
		if !errors.As(err, &unread) {
			unread = &deps.UnreadError{Problems: []deps.UnreadProblem{{File: "a dependency manifest", Err: err}}}
		}
		if msg := deps.UnreadRefusal(policy, unread, !IsUploadedSource(app.RepoURL)); msg != "" {
			if len(res.Dependencies) > 0 {
				if err := b.db.SaveDeploymentDependencies(deploy.ID, res.Dependencies); err != nil {
					appendLog(fmt.Sprintf("    Warning: the dependency inventory could not be recorded: %v", err))
				}
			}
			b.recordVerdict(deploy, deps.Verdict{
				Allowed: false, Reason: msg, Mode: policy.Mode, Scanned: false,
				CheckedAt: time.Now().UTC(), FeedAsOf: policy.FeedAsOf,
				Packages: len(res.Dependencies), Ecosystems: res.Ecosystems,
				Unread: deps.UnreadOf(res.Gaps), HashedRequirements: res.HashedRequirements,
			})
			b.opts.Events.Record(events.Event{
				AppID:        app.ID,
				Kind:         events.KindDependencyBlocked,
				Severity:     database.EventSeverityError,
				Source:       database.EventSourceBuilder,
				Summary:      "A deploy was refused by this install's dependency policy",
				Detail:       msg,
				DeploymentID: deploy.ID,
				Fingerprint:  "unread:" + strings.Join(unread.Files(), ","),
			})
			telemetry.RecordDependencyRefusal(context.Background(), "unknown", policy.Mode)
			return msg
		}
	}
	for _, note := range res.Notes {
		appendLog("    " + note)
	}
	// What the resolution did not read reaches the deploy's own warnings too,
	// for the person reading the deploy; the verdict below carries it as
	// kinds, which is what anything downstream reads.
	b.addDeployWarningDetails(deploy, gapWarnings(res.Gaps))
	// **An empty resolve and an uncovered one are different facts, and the
	// order below is what keeps them apart.** Nothing resolved at all is a
	// release with no manifest this build reads. Packages resolved into no
	// *covered* ecosystem is R: `renv.lock` was parsed, every package is named
	// at a version, and what is missing is a corpus worth checking them
	// against — see advisory.Covered. Testing the ecosystem count first would
	// discard the second one's inventory on the way past, and put "this app's
	// packages cannot be listed" on an app whose lockfile had just been read.
	if len(res.Dependencies) == 0 {
		if err == nil {
			appendLog("    No dependency manifest to read.")
		}
		b.recordVerdict(deploy, deps.Verdict{
			Allowed: true, Mode: policy.Mode, Scanned: false,
			CheckedAt: time.Now().UTC(), FeedAsOf: policy.FeedAsOf,
			Unread: deps.UnreadOf(res.Gaps),
		})
		// A release with no manifest clears a lock; one whose manifest could
		// not be read does not — it may still carry the package (E-2).
		b.lockClears.Store(deploy.ID, err == nil && !deps.LeftUnresolved(res.Gaps))
		return ""
	}

	// The inventory is written whatever the verdict, and before it. A refused
	// deploy is exactly the one whose package list somebody wants to read —
	// and so is a release nothing can be checked against, which is the only
	// record R has.
	recorded := true
	if err := b.db.SaveDeploymentDependencies(deploy.ID, res.Dependencies); err != nil {
		appendLog(fmt.Sprintf("    Warning: the dependency inventory could not be recorded: %v", err))
		recorded = false
		res.Gaps = append(res.Gaps, deps.Gap{Kind: deps.GapUnresolved,
			Reason: "the inventory could not be recorded: " + oneLineErr(err)})
		// Findings are read off the stored inventory, so judging an
		// unrecorded one would pass the release on an empty table. Under an
		// enforcing policy that is a refusal; under `warn` it goes ahead as
		// not scanned and says why (LB-1).
		if policy.Enforcing() {
			return b.refuseUnrecorded(app, deploy, policy, len(res.Dependencies), res.Ecosystems,
				deps.UnreadOf(res.Gaps), res.HashedRequirements, err)
		}
	}

	// **Covered and matched are two questions, and this is where they part.**
	// An R-only release resolves into no *covered* ecosystem, so its silence is
	// never a verdict and `scanned` stays false. Its packages are still matched,
	// against the CRAN corpus this install syncs, because an advisory that
	// exists is evidence whatever the density of its neighbours — suppressing a
	// real one on a meta-fact about the feed would be a second policy nobody
	// configured. Recognition may only subtract: this can move a release from
	// "nothing known" to "one advisory" and never to "clean". See
	// advisory.Synced, and note there is no branch below this one — a CRAN
	// finding blocks under the administrator's threshold like any other.
	// Findings are read off the stored inventory, so a release whose inventory
	// was not stored has not been scanned, whatever its ecosystems: judging the
	// empty table would pass it as clean (G-2).
	//
	// **A covered ecosystem with no corpus was checked against nothing**, so
	// "scanned" asks which of the release's ecosystems has one loaded — not
	// whether any ecosystem anywhere on the install does (DG-5). Each one
	// without is a deploy warning by name; the install-wide warning above
	// already says it when nothing at all has loaded.
	withoutCorpus := deps.WithoutCorpus(policy, res.Ecosystems)
	scanned := len(res.Ecosystems) > len(withoutCorpus) && recorded
	if policy.HasData() && len(withoutCorpus) > 0 {
		var ws []Warning
		for _, eco := range withoutCorpus {
			appendLog(fmt.Sprintf("    No %s advisory data has loaded on this install, so this "+
				"release's %s packages were listed but not checked against any.", eco, eco))
			ws = append(ws, Warning{Code: WarningAdvisoryCorpusMissing,
				Message: "No " + eco + " advisory data has loaded on this install, so this release's " +
					eco + " dependencies were listed but not checked against any."})
		}
		b.addDeployWarningDetails(deploy, ws)
	}
	if scanned {
		appendLog(fmt.Sprintf("    Resolved %d packages (%s)",
			len(res.Dependencies), strings.Join(res.Ecosystems, ", ")))
		// Labelled by the covered ecosystems alone, and absent for the other
		// branch: a scan that did not happen is not an outcome, and the
		// cardinality budget is code (§31).
		telemetry.RecordDependencyScan(context.Background(),
			ecosystemLabel(res.Ecosystems), telemetry.OutcomeSucceeded)
	} else {
		appendLog(fmt.Sprintf("    Recorded %d package(s). None is in an ecosystem whose "+
			"silence this install trusts, so this release is not scanned — an advisory that "+
			"does name one of them is still reported.", len(res.Dependencies)))
	}

	// An R release is judged again once it is installed, and that judgement
	// writes the deploy's row of history; this one writes it only if it
	// refuses, which is the reading nobody would otherwise get.
	deferHistory := LanguageOf(DetectFramework(releaseDir).Framework) == toolchain.R
	return b.judge(app, deploy, policy, res.Dependencies, scanned, res.Ecosystems,
		deps.UnreadOf(res.Gaps), res.HashedRequirements, deferHistory, appendLog)
}

// refuseUnrecorded is the refusal for a release whose inventory could not be
// stored, under an enforcing policy: nothing judged it, and the estate watch
// would have no rows for it either (LB-1).
func (b *Builder) refuseUnrecorded(app *database.App, deploy *database.Deployment, policy deps.Policy,
	packages int, ecosystems []string, unread []deps.Gap, hashed bool, cause error) string {

	again := "push again"
	if IsUploadedSource(app.RepoURL) {
		again = "upload the app again"
	}
	msg := "REFUSED: this install does not deploy packages it could not record, and this " +
		"release's dependency inventory could not be stored: " + oneLineErr(cause) +
		"\n\nThe way forward: check that every package name and version in the release's " +
		"manifests is plain text — then " + again + ". Administrators set the policy on Admin → Dependencies."
	b.recordVerdict(deploy, deps.Verdict{
		Allowed: false, Reason: msg, Mode: policy.Mode, Scanned: false,
		CheckedAt: time.Now().UTC(), FeedAsOf: policy.FeedAsOf,
		Packages: packages, Ecosystems: ecosystems,
		Unread: unread, HashedRequirements: hashed,
	})
	b.lockClears.Store(deploy.ID, false)
	b.opts.Events.Record(events.Event{
		AppID:        app.ID,
		Kind:         events.KindDependencyBlocked,
		Severity:     database.EventSeverityError,
		Source:       database.EventSourceBuilder,
		Summary:      "A deploy was refused by this install's dependency policy",
		Detail:       msg,
		DeploymentID: deploy.ID,
		Fingerprint:  "unrecorded",
	})
	telemetry.RecordDependencyRefusal(context.Background(), "unknown", policy.Mode)
	return msg
}

// judge derives a release's findings from the inventory already recorded for
// it, records the verdict, and returns the refusal's message or "".
//
// One judgement for both of a deploy's gates — the lockfiles at Step 2 and,
// for R, the installed library after the build — so the two can never refuse
// in different words or record different things. deferHistory leaves the
// history row to a later judgement of the same deploy unless this one refuses.
func (b *Builder) judge(app *database.App, deploy *database.Deployment, policy deps.Policy,
	inventory []deps.Dependency, scanned bool, ecosystems []string, unread []deps.Gap,
	hashed, deferHistory bool, appendLog func(string)) string {

	findings, incomparable, err := b.db.FindingsForCounted(deploy.ID, app.ID, policy)
	if err != nil {
		appendLog(fmt.Sprintf("    Warning: advisories could not be read, so dependencies were "+
			"not checked: %v", err))
		b.recordVerdict(deploy, deps.Verdict{
			Allowed: true, Mode: policy.Mode, Scanned: false,
			CheckedAt: time.Now().UTC(), FeedAsOf: policy.FeedAsOf,
			Packages: len(inventory), Ecosystems: ecosystems,
			Unread: unread, HashedRequirements: hashed,
		})
		b.lockClears.Store(deploy.ID, false)
		return ""
	}

	decision := deps.Decide(policy, findings, !IsUploadedSource(app.RepoURL),
		scanned, len(inventory), ecosystems)
	decision.Verdict.Unread = unread
	decision.Verdict.HashedRequirements = hashed
	decision.Verdict.Incomparable = incomparable
	if incomparable > 0 {
		// Never a finding — a refusal on a string the platform cannot read is
		// one nobody can act on — and never silently nothing either (DG-10).
		appendLog(fmt.Sprintf("    %d advisory %s could not be compared with the installed version, "+
			"so %s not checked.", incomparable, plural(incomparable, "range", "ranges"),
			plural(incomparable, "it was", "they were")))
	}
	b.recordVerdict(deploy, decision.Verdict)
	b.lockClears.Store(deploy.ID, !holdsLock(policy, findings) && !deps.LeftUnresolved(unread))
	// The observation, beside the decision and never derived from it. A deploy
	// writes one so that the history has no hole precisely where the app
	// changed — including a refused deploy, which is the reading somebody is
	// most likely to want back.
	if !deferHistory || decision.Refusal != nil {
		b.recordDependencyScan(app, deploy, policy, findings, inventory, scanned)
	}

	if decision.Refusal != nil {
		// Recorded whether the deploy was automatic or not, unlike a build
		// failure. This is the KindSourceBlocked argument: nothing is broken,
		// an administrator's decision is what stopped it, and an owner
		// watching commits land and nothing happen is exactly what the events
		// feed exists to prevent. The fingerprint is the advisory set, so a
		// developer retrying the same commit five times is one problem.
		b.opts.Events.Record(events.Event{
			AppID:        app.ID,
			Kind:         events.KindDependencyBlocked,
			Severity:     database.EventSeverityError,
			Source:       database.EventSourceBuilder,
			Summary:      "A deploy was refused by this install's dependency policy",
			Detail:       decision.Refusal.Message(),
			DeploymentID: deploy.ID,
			Fingerprint:  refusalFingerprint(decision.Refusal.Blocking),
		})
		telemetry.RecordDependencyRefusal(context.Background(),
			worstSeverity(decision.Refusal.Blocking), policy.Mode)
		// One wording, five places. The build log gets it first because it is
		// the surface a developer is actually looking at when this happens.
		return decision.Refusal.Message()
	}

	b.logFindings(policy, decision, scanned, appendLog)
	return ""
}

// feedHeadline is the line that scopes every number below it. **Every figure
// this feature shows is scoped to the corpus it was checked against**, so the
// corpus and its age lead rather than being a footnote.
// scanPython is the interpreter the build will install this release's Python
// dependencies with, so the scan resolves for the same one (ADR §39: the judged
// set is the installed set). The gate runs before the deploy resolves its
// runtime, so it asks the same question resolveRuntime answers there, with the
// log discarded — that step reports it.
//
// Never "" while this install has a Python: anything that is not a Python
// framework, or a pin nothing satisfies, gets the install's default
// interpreter. Left to itself uv would discover one — from the release's
// uv.toml or a committed .venv — and execute it, as the platform, before the
// handover (security review 2026-11-14, DG-2). A pin nothing satisfies is
// still refused by the runtime step, in its own words. "" means the install
// has no Python at all, and the scan does not run uv.
func (b *Builder) scanPython(app *database.App, releaseDir string) string {
	python, _ := b.scanPythons(app, releaseDir)
	return python
}

// scanPythons is scanPython and, beside it, the Python lines this install
// offers below the release's — the ones a missing-wheel refusal may suggest
// declaring (deps.Options.EarlierPythons), newest first.
func (b *Builder) scanPythons(app *database.App, releaseDir string) (string, []string) {
	framework := DetectFramework(releaseDir).Framework
	mode := app.DesiredMode()
	if LanguageOf(framework) != toolchain.Python {
		return defaultScanPython(), nil
	}
	rt, err := b.resolveRuntime(framework, releaseDir, mode, func(string) {})
	if err != nil || rt == nil || rt.BuildWith.Path == "" {
		return defaultScanPython(), nil
	}
	return rt.BuildWith.Path, earlierSeries(rt.Chosen.Series(), b.effectiveSeries(toolchain.Python, mode))
}

// defaultScanPython is this build host's default Python — the one the scan
// can execute — or "" when it has none. The build host's, not the isolated
// runner image's declared set: those versions carry no path here.
func defaultScanPython() string {
	var first string
	for _, v := range toolchain.Available(toolchain.Python) {
		if v.Path == "" {
			continue
		}
		if v.Default {
			return v.Path
		}
		if first == "" {
			first = v.Path
		}
	}
	return first
}

func (b *Builder) effectiveSeries(lang string, mode runtime.Mode) []string {
	set, _ := b.effectiveSet(lang, mode, func(string) {})
	var out []string
	for _, v := range set {
		out = append(out, v.Series())
	}
	return out
}

// earlierSeries is each distinct series below current, newest first.
func earlierSeries(current string, offered []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range offered {
		if s == "" || seen[s] || current == "" || toolchain.Compare(s, current) >= 0 {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return toolchain.Compare(out[i], out[j]) > 0 })
	return out
}

func (b *Builder) feedHeadline(policy deps.Policy) string {
	if policy.FeedAsOf == nil {
		return "==> Checking dependencies — no advisory data has been loaded yet, so nothing " +
			"can be checked against it"
	}
	line := fmt.Sprintf("==> Checking dependencies against advisory data (synced %s)",
		policy.FeedAsOf.Format("2006-01-02 15:04 UTC"))
	if policy.Stale() {
		// Loud, and it never blocks. An outage at a third party must not become
		// an outage here, but silence about it would be the other failure.
		line += "\n    Warning: that data is older than this install's staleness threshold."
	}
	return line
}

// logFindings says what was found when nothing was refused.
//
// The empty state is the product in `warn` mode rather than a shrug, and the
// non-empty one has to be honest about which half of the mode it is in: a
// finding that *would* block on a `block` install is a different sentence from
// one that is merely recorded.
//
// `scanned` changes only the empty state, and it has to: "no known advisories"
// on an unscanned release would be the clean bill of health this whole design
// refuses to write. Nothing found is a fact either way; whether it is *good
// news* is what the two wordings differ about.
func (b *Builder) logFindings(policy deps.Policy, d deps.Decision, scanned bool,
	appendLog func(string)) {

	if len(d.Findings) == 0 {
		if !scanned {
			appendLog("    None of them appears in the advisory data this install holds — " +
				"which for these packages is not a clean bill of health.")
			return
		}
		appendLog("    No known advisories among them.")
		return
	}

	var counted, waived int
	for _, f := range d.Findings {
		if f.Waived {
			waived++
			continue
		}
		if policy.Counts(f) {
			counted++
		}
	}
	appendLog(fmt.Sprintf("    %d known %s, %d at or above this install's threshold%s.",
		len(d.Findings), plural(len(d.Findings), "advisory", "advisories"), counted,
		waivedSuffix(waived)))

	// The blocking ones by name, because on a `warn` install this is the whole
	// content of the warning — an administrator reading a build log has to be
	// able to see what `block` would have done.
	for _, f := range d.Blocking() {
		appendLog(fmt.Sprintf("      %s %s  →  %s (%s)%s", f.Package, f.Version, f.AdvisoryID,
			severityWord(f), fixedSuffix(f)))
	}
	if len(d.Blocking()) > 0 && !policy.Enforcing() {
		appendLog("    Recorded, not enforced: this install's dependency policy is not blocking.")
	}
	appendLog("    Full detail is on the app's Dependencies tab.")
}

func severityWord(f advisory.Finding) string {
	if f.Malware() {
		return "malicious package"
	}
	if f.Severity == advisory.SeverityUnknown {
		return "unscored"
	}
	return f.Severity
}

func fixedSuffix(f advisory.Finding) string {
	if fix := strings.TrimSpace(f.FixedIn); fix != "" {
		return " — fixed in " + fix
	}
	return " — no fix published"
}

func waivedSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d waived", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// recordVerdict stores the decision on the release. A failed write is logged
// and dropped: a build must never fail because the platform could not describe
// what it decided.
func (b *Builder) recordVerdict(deploy *database.Deployment, v deps.Verdict) {
	if err := b.db.SetDeploymentVerdict(deploy.ID, v); err != nil {
		log.Printf("deps: recording the verdict for %s: %v", deploy.ID, err)
	}
}

// recordDependencyScan writes the deploy's own row of history.
//
// Beside recordVerdict rather than inside it, because they are two facts with
// two homes: the verdict is the release's decision, read by the runner at every
// bootstrap and never updated after this moment, and this is an observation
// taken at a time. Nothing derives one from the other.
//
// A failed write is logged and dropped, for recordVerdict's reason: a build
// must never fail because the platform could not write down what it saw.
func (b *Builder) recordDependencyScan(app *database.App, deploy *database.Deployment,
	policy deps.Policy, findings []advisory.Finding, inventory []deps.Dependency, scanned bool) {

	scan := database.SummarizeScan(policy, findings, inventory)
	scan.AppID = app.ID
	scan.DeploymentID = deploy.ID
	scan.Trigger = database.DependencyScanTriggerDeploy
	scan.Scanned = scanned
	if err := b.db.RecordDependencyScan(&scan, policy.HistoryKept); err != nil {
		log.Printf("deps: recording the scan for %s: %v", deploy.ID, err)
	}
}

// carryDependencyRecord copies a release's inventory and verdict onto the
// rollback that restores it.
//
// A rollback republishes a release already on disk: nothing is fetched and
// nothing is rebuilt, so there is nothing to resolve and — deliberately —
// nothing to re-judge. **Rollback is never blocked by this feature**, which is
// the same asymmetry the version cap and the source policy both have, and for
// the same reason: it is the one operation an app with a bad release depends
// on, and refusing it would leave an owner with nothing to do at the moment
// they least want to be told no.
func (b *Builder) carryDependencyRecord(sourceDeployID string, deploy *database.Deployment,
	appendLog func(string)) {

	if b.opts.Deps == nil || sourceDeployID == "" {
		return
	}
	if err := b.db.CopyDeploymentDependencies(sourceDeployID, deploy.ID); err != nil {
		log.Printf("deps: carrying the inventory to rollback %s: %v", deploy.ID, err)
		return
	}
	v, ok, err := b.db.GetDeploymentVerdict(sourceDeployID)
	if err != nil || !ok {
		return
	}
	// The verdict is carried as the *release's*, not re-taken. What it said
	// when that release was allowed to deploy is what it still says.
	b.recordVerdict(deploy, v)
	if n, err := b.db.CountDeploymentDependencies(deploy.ID); err == nil && n > 0 {
		appendLog(fmt.Sprintf("    Restoring that release's recorded inventory (%d packages)", n))
	}
}

// refusalFingerprint is what makes two refusals "the same problem".
//
// The set of blocking advisories, sorted, so a developer retrying the same
// commit five times is one event and one email — while a *different* advisory
// appearing is a new problem and says so. Too coarse (the kind alone) and the
// second finding is swallowed; too fine (the deployment id) and every retry is
// news.
func refusalFingerprint(blocking []advisory.Finding) string {
	ids := make([]string, 0, len(blocking))
	seen := map[string]bool{}
	for _, f := range blocking {
		if !seen[f.AdvisoryID] {
			seen[f.AdvisoryID] = true
			ids = append(ids, f.AdvisoryID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// ecosystemLabel is the bounded value the metric carries: one of the three this
// binary resolves, or "mixed" for an app with more than one manifest.
//
// Bounded on purpose. An unbounded label here would be the standard way a
// metrics rollout takes down the monitoring system it was meant to protect.
func ecosystemLabel(ecosystems []string) string {
	if len(ecosystems) == 1 {
		return ecosystems[0]
	}
	return "mixed"
}

// worstSeverity is the label a refusal is counted under: the most serious
// blocking finding's class. Malware is a value here rather than a second
// attribute, because it is a class and not a level and the two never combine.
func worstSeverity(blocking []advisory.Finding) string {
	worst := advisory.SeverityUnknown
	for _, f := range blocking {
		if f.Malware() {
			return "malware"
		}
		if advisory.AtOrAbove(f.Severity, worst) || worst == advisory.SeverityUnknown {
			worst = f.Severity
		}
	}
	if worst == advisory.SeverityUnknown {
		return "unknown"
	}
	return worst
}

// gapWarnings is one deploy warning per thing the resolution did not read,
// each coded by its gap kind so warning_details carries it (BB-017).
//
// **"Resolved" means one thing here: what the installer picks at build time.**
// The scanner's own reading is "listed" and "checked". Using the word for both
// made a pinned requirements.txt read as "resolved at build time" on one deploy
// and "not resolved" on the next, which an operator cannot square.
func gapWarnings(gaps []deps.Gap) []Warning {
	var out []Warning
	for _, g := range gaps {
		switch g.Kind {
		case deps.GapForeignLock:
			out = append(out, Warning{Code: WarningLockfileNotRead, Message: g.File + " is not read by this platform, so this release's npm versions were resolved from package.json and can differ from the ones it pins."})
		case deps.GapUnlocked:
			out = append(out, Warning{Code: WarningNoLockfile, Message: "This release's " + g.Ecosystem + " dependencies have no lockfile this platform reads. " +
				"The versions checked are the ones this build resolved, and a later build can resolve different ones."})
		case deps.GapVendored:
			out = append(out, Warning{Code: WarningVendoredUnchecked, Message: "A committed " + g.File + " directory is not read, so what it holds is not checked against advisory data."})
		case deps.GapUnresolved:
			// A reason is a resolver that read the file and knows why it gave
			// up; "could not be read" is kept for the failure it names.
			if g.Reason != "" {
				whose := "This release's"
				if g.File != "" {
					whose = g.File + "'s"
				}
				out = append(out, Warning{Code: WarningDepsUnresolved, Message: whose + " " + g.Ecosystem + " dependencies were not checked: " + g.Reason + "."})
				continue
			}
			if g.File != "" {
				out = append(out, Warning{Code: WarningDepsUnresolved, Message: g.File + " could not be read, so this release's " + g.Ecosystem +
					" dependencies were not checked."})
				continue
			}
			out = append(out, Warning{Code: WarningDepsUnresolved, Message: "This release's " + g.Ecosystem + " dependencies could not be listed, so they were not checked."})
		case deps.GapDirectOnly:
			out = append(out, Warning{Code: WarningDepsDirectOnly, Message: "Only the packages " + g.File + " names were checked. The packages they depend on " +
				"are still installed, but could not be listed, so they were not checked."})
		}
	}
	return out
}

// gapSentences is gapWarnings' sentences alone.
func gapSentences(gaps []deps.Gap) []string {
	var out []string
	for _, w := range gapWarnings(gaps) {
		out = append(out, w.Message)
	}
	return out
}

// rInstalledGate judges an R release by the library its build installed.
//
// renv.lock is what Step 2 judged, and it is what the build *would* install.
// The library is what it *did*: every package an R app can load is in it, and
// it is the only record of the ones no lockfile names — renv itself, the SDK
// and its dependencies, which Toft installs into every R release, and whatever
// renv pulled in that the lockfile did not pin. So after the install the
// library replaces the release's cran rows, each marked with whose it is, and
// the release is judged again before it is published. A refusal here costs
// the build's time, which is why the lockfile is still judged first.
//
// **Platform packages are judged like the app's**: the app runs that code
// with its environment, whoever installed it. The refusal says which ones are
// Toft's, so its reader knows the fix is a waiver or a Toft release.
func (b *Builder) rInstalledGate(app *database.App, deploy *database.Deployment,
	releaseDir string, appendLog func(string)) string {

	var platform map[string]bool
	if v, ok := b.rPlatform.LoadAndDelete(filepath.Clean(releaseDir)); ok {
		platform = v.(map[string]bool)
	}
	if b.opts.Deps == nil {
		return ""
	}
	policy, err := b.opts.Deps.Policy()
	if err != nil || policy.Off() || !policy.ScanOnDeploy {
		// Step 2 already said why, or said nothing on purpose.
		return ""
	}

	rows, notes := installedRRows(releaseDir, platform)
	for _, note := range notes {
		appendLog("    " + note)
	}
	var own int
	for _, d := range rows {
		if d.Source != deps.SourcePlatform {
			own++
		}
	}
	appendLog(fmt.Sprintf("==> Checking the %d R packages this release installed "+
		"(%d installed by Toft into every R release)", len(rows), len(rows)-own))

	if err := b.db.ReplaceEcosystemDependencies(deploy.ID, advisory.EcosystemCRAN, rows); err != nil {
		appendLog(fmt.Sprintf("    Warning: the installed R packages could not be recorded, "+
			"so they were not checked: %v", err))
		if policy.Enforcing() {
			return b.refuseUnrecorded(app, deploy, policy, len(rows),
				[]string{advisory.EcosystemCRAN}, nil, false, err)
		}
		return ""
	}
	inventory, err := b.db.ListDeploymentDependencies(deploy.ID)
	if err != nil {
		appendLog(fmt.Sprintf("    Warning: the inventory could not be read back, so the "+
			"installed R packages were not checked: %v", err))
		return ""
	}
	ecosystems := coveredEcosystems(inventory)
	unread := []deps.Gap{}
	hashed := false
	if v, ok, err := b.db.GetDeploymentVerdict(deploy.ID); err == nil && ok {
		// What Step 2 could not read is still unread; this judgement read only
		// the library. How it read PyPI is likewise Step 2's.
		unread = deps.UnreadOf(v.Unread)
		hashed = v.HashedRequirements
	}
	return b.judge(app, deploy, policy, inventory, len(ecosystems) > 0, ecosystems,
		unread, hashed, false, appendLog)
}

// installedRRows reads a release's R libraries into inventory rows, each
// sourced: a package renv.lock names is the app's choice, `installed`; one the
// library held before the app's own were restored is Toft's, `platform`;
// anything else the restore pulled in is the app's too.
func installedRRows(releaseDir string, platform map[string]bool) ([]deps.Dependency, []string) {
	locked := deps.RenvLockPackages(releaseDir)
	seen := map[string]bool{}
	var rows []deps.Dependency
	var notes []string
	for _, lib := range deps.RLibraryDirs(releaseDir) {
		found, n, err := deps.ReadRLibrary(lib)
		notes = append(notes, n...)
		if err != nil {
			notes = append(notes, fmt.Sprintf("Warning: %v", err))
			continue
		}
		for _, d := range found {
			key := d.Package + "@" + d.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			switch {
			case locked[d.Package]:
				d.Source, d.Direct = deps.SourceInstalled, true
			case platform[d.Package]:
				d.Source = deps.SourcePlatform
			default:
				d.Source = deps.SourceInstalled
			}
			rows = append(rows, d)
		}
	}
	return rows, notes
}

// installedNames is the set of package directories in an R library now.
func installedNames(libDir string) map[string]bool {
	names := map[string]bool{}
	found, _, _ := deps.ReadRLibrary(libDir)
	for _, d := range found {
		names[d.Package] = true
	}
	return names
}

// coveredEcosystems is the covered list an inventory implies, in a stable order.
func coveredEcosystems(inventory []deps.Dependency) []string {
	var out []string
	for _, e := range advisory.Ecosystems {
		for _, d := range inventory {
			if d.Ecosystem == e {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// holdsLock reports whether findings include one the lock would act on —
// unwaived and at or above dependency_lock_on. A release with one cannot lift
// a dependency lock, however it fared at the deploy gate: under `warn` a
// critical vulnerability deploys, and with the lock at `critical` the next
// pass would only lock it again.
func holdsLock(policy deps.Policy, findings []advisory.Finding) bool {
	for _, f := range findings {
		if !f.Waived && policy.LocksOn(f) {
			return true
		}
	}
	return false
}

// liftDependencyLock lifts the lock the dependency policy placed on app if
// this deploy's release clears it, and returns "" when it did — or the
// sentence the deploy fails with when it did not. The audit row is the record;
// the build log line is for whoever is watching.
//
// A release clears the lock when its last judgement ran and found nothing
// unwaived at or above dependency_lock_on (judge records it). Not judged — the
// policy is off, or the deploy scan is — never clears: a lock placed on
// evidence is not lifted on no evidence, and an administrator is the way out.
func (b *Builder) liftDependencyLock(app *database.App, deploy *database.Deployment,
	appendLog func(string)) string {
	v, judged := b.lockClears.LoadAndDelete(deploy.ID)
	if !judged {
		return "this app is locked by this install's dependency policy, and this deploy's " +
			"dependencies were not checked, so the lock stays. An administrator can unlock it."
	}
	if !v.(bool) {
		return "this app is locked by this install's dependency policy, and this release still " +
			"carries a package the lock acts on, so it was not started. Its Dependencies tab " +
			"names it; a release without it lifts the lock, or an administrator can grant a waiver."
	}
	lifted, err := b.db.LiftDependencyLock(app.ID, app.Slug, deploy.ID, time.Now().UTC())
	if err != nil {
		log.Printf("deps: lifting the lock on %s: %v", app.Slug, err)
		return "this app's dependency lock could not be lifted, so it was not started: " + err.Error()
	}
	if lifted {
		appendLog("==> Lifted the dependency lock: this release no longer carries the package " +
			"it was placed for.")
	}
	// Not lifted means another deploy got there first, or an administrator's
	// lock replaced it; the caller re-reads the app and decides.
	return ""
}

// oneLineErr is an error's text on one line, for a gap's reason.
func oneLineErr(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
