package depsweep

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
	"toft/internal/deps"
	"toft/internal/events"
	"toft/internal/runtime"
	"toft/sdk"
)

// Enforcement point 3: the same rule, applied to code that is already running.
//
// **A serving app is never stopped, slept, rolled back, or degraded by this —
// with one named exception the administrator has to switch on.** A third-party
// feed publishing a bad advisory at 3am must not be able to take down an estate
// of internal tools people are using at the moment the sweep runs. That
// reasoning is intact for vulnerabilities and holds unchanged: the platform
// records, shows and tells; a person decides; and the automatic consequence is
// that the app's *next deploy* is judged under the new information.
//
// Malware inverts it, which is why `dependency_lock_on` exists and why its
// default is `malware` and nothing above it. A package confirmed to be exfiltrating credentials is not a
// risk to the app: it is running inside the install, on the platform's own
// machine, with the app's environment, and every hour it keeps serving is the
// harm rather than the mitigation. The app most likely to be running one is the
// one nobody has redeployed since it was published — exactly the app the deploy
// gate never touches. See deps.Policy.LocksOn.
//
// **The sweep also says that it ran.** Every pass writes one dependency_scans
// row per app, whether or not anything changed, because a finding is derived
// against a corpus that gets swapped wholesale and yesterday's reading cannot
// be recovered tomorrow.

// defaultSweepInterval is how often serving releases are re-judged on an
// install that has never touched the setting. Six hours, which is what this was
// before it became `dependency_scan_interval_hours` — so no install's behaviour
// changed when it did.
const defaultSweepInterval = 6 * time.Hour

// SweepStore is the slice of the database the sweep needs.
type SweepStore interface {
	ListServingReleases() ([]deps.ServingRelease, error)
	FindingsFor(deploymentID, appID string, policy deps.Policy) ([]advisory.Finding, error)
	ListDeploymentDependencies(deploymentID string) ([]deps.Dependency, error)
	// AppendDeploymentDependencies adds rows the inventory does not hold and
	// reports how many — a shared R app's runtime installs.
	AppendDeploymentDependencies(deploymentID string, list []deps.Dependency) (int, error)
	OpenEventFingerprints(appID, kind string) (map[string]bool, error)
	ResolveEventsExcept(appID, kind string, keep []string) (int64, error)
	SetDependencyBaseline(appID string, at time.Time) error
	// RecordDependencyScan writes the reading and prunes the app's history to
	// the newest `kept` rows, in one transaction.
	RecordDependencyScan(scan *database.DependencyScan, kept int) error
	// LockAppForDependency is the guarded transition. It reports whether this
	// call is the one that locked the app, which is how the sweep knows not to
	// tell anybody twice.
	LockAppForDependency(appID, slug, message string, detail map[string]any, now time.Time) (bool, error)
	GetApp(id string) (*database.App, error)
	UpdateAppStatus(id, status string) error
}

// Sweeper re-evaluates serving releases against a corpus that moves.
//
// It costs no rebuild and touches no registry: the stored inventory is joined
// against refreshed advisories, which is the whole reason that table exists.
type Sweeper struct {
	store    SweepStore
	resolver *deps.Resolver
	events   *events.Recorder
	// rts is how a lock stops the workload. Nil is a coherent install — the
	// lock is still stored and the proxy still refuses — but a malicious
	// package would keep running, so main.go always supplies one and only a
	// test leaves it out.
	rts *runtime.Runtimes
	// interval is the fallback until the first pass reads the policy's own.
	interval time.Duration
	// treeDir is where a shared workload's release tree is on this machine,
	// or "" where it is not. Nil reads nothing off disk (WithReleaseTrees).
	treeDir func(releaseRef string) string

	mu      sync.Mutex
	stop    chan struct{}
	stopped bool
	running sync.Mutex
}

// NewSweeper builds one. It performs no I/O and starts no goroutine.
func NewSweeper(store SweepStore, resolver *deps.Resolver, rec *events.Recorder) *Sweeper {
	return &Sweeper{
		store: store, resolver: resolver, events: rec,
		interval: defaultSweepInterval, stop: make(chan struct{}),
	}
}

// WithRuntimes wires the backend a lock stops the workload through.
//
// An automatic lock **is** an automatic stop, the same way a manual one is:
// reclaiming what an app is spending is half of why anybody locks one, and for
// the malware case it is the whole point — a locked app still running the
// package is a label rather than a control.
func (s *Sweeper) WithRuntimes(rts *runtime.Runtimes) *Sweeper {
	s.rts = rts
	return s
}

// WithReleaseTrees wires where a shared app's release tree is, which is what
// lets the sweep read an R app's library for the packages it installed while
// it ran. Without it the sweep reads only the stored inventory, as before.
func (s *Sweeper) WithReleaseTrees(treeDir func(releaseRef string) string) *Sweeper {
	s.treeDir = treeDir
	return s
}

// Start begins sweeping.
func (s *Sweeper) Start() { go s.loop() }

// Stop ends it. Safe to call twice.
func (s *Sweeper) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
}

func (s *Sweeper) loop() {
	// A delay before the first pass so a boot already doing migrations, a
	// reconcile and a queue sweep is not also joining every app's inventory
	// against the corpus.
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			if err := s.Sweep(context.Background()); err != nil {
				log.Printf("deps: sweep: %v", err)
			}
			// Re-read every pass, like the switch above it and for the same
			// reason: an administrator who shortens the interval should not
			// have to wait out the old one to see it apply.
			timer.Reset(s.nextInterval())
		}
	}
}

// nextInterval is how long until the next pass, from the policy rather than
// from a constant.
//
// Its own dial rather than the feed's: the feed interval governs an outbound
// fetch of tens of megabytes and a sweep is a local join, so an install syncing
// weekly was being swept twenty-eight times per corpus. A policy that will not
// resolve keeps the last interval rather than spinning — the pass itself will
// report the error.
func (s *Sweeper) nextInterval() time.Duration {
	policy, err := s.resolver.Policy()
	if err != nil || policy.ScanInterval <= 0 {
		return s.interval
	}
	s.interval = policy.ScanInterval
	return s.interval
}

// Sweep is the timer's pass: it re-evaluates every serving release and it is
// the only entry point that can lock.
//
// **Off means no goroutine's worth of work**, checked fresh on every pass
// rather than at construction: an administrator switching the watch off during
// a six-hour interval must not get one more pass out of a decision the sweeper
// cached.
func (s *Sweeper) Sweep(ctx context.Context) error {
	return s.sweep(ctx, database.DependencyScanTriggerScheduled)
}

// SweepNow is the estate's *Re-check now* button.
//
// The same derivation over the same apps, recorded the same way, and it **never
// locks** — the same rule SweepApp follows and for the same reason: an
// automatic consequence needs an automatic trigger, and a button is a person.
// An administrator pressing this is asking the estate a question, not asking
// for part of it to go dark while they watch.
func (s *Sweeper) SweepNow(ctx context.Context) error {
	return s.sweep(ctx, database.DependencyScanTriggerManual)
}

func (s *Sweeper) sweep(ctx context.Context, trigger string) error {
	if s == nil {
		return nil
	}
	s.running.Lock()
	defer s.running.Unlock()

	policy, err := s.resolver.Policy()
	if err != nil {
		return err
	}
	// The watch switch gates the timer alone. An administrator's Re-check now
	// (or the sweep after Sync now) went through here too, and with the watch
	// off was told 204 while no app was read, no reading written and no event
	// moved (code review 2026-11-11, G-8); the per-app button already worked
	// with the watch off, and the estate-wide one is the same question asked
	// of every app.
	if policy.Off() || (!policy.WatchEnabled && trigger == database.DependencyScanTriggerScheduled) {
		return nil
	}

	releases, err := s.store.ListServingReleases()
	if err != nil {
		return err
	}
	var failures int
	for _, rel := range releases {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := s.sweepOne(policy, rel, trigger); err != nil {
			// One app failing does not stop the rest: a sweep that abandoned
			// the estate because one row would not read is a sweep that
			// silently stops covering thirty-nine apps.
			log.Printf("deps: sweeping %s: %v", rel.Slug, err)
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d apps could not be swept", failures, len(releases))
	}
	return nil
}

// SweepApp re-evaluates one app on demand — the Check again button.
//
// **A re-check is not a re-scan of the code.** It is
// inventory × advisories × waivers: cheap, no registry, no resolver. What the
// repository *would* install today can only be re-read by a build, because a
// floating range resolves differently on a different day, and a button that
// silently did not re-read package.json would be the dishonest version of this
// control.
//
// It runs with the watch off, which is the point: the question stays
// answerable, just only when somebody asks.
func (s *Sweeper) SweepApp(rel deps.ServingRelease) ([]advisory.Finding, error) {
	if s == nil {
		return nil, nil
	}
	policy, err := s.resolver.Policy()
	if err != nil {
		return nil, err
	}
	if policy.Off() {
		return nil, nil
	}
	// **Manual, and therefore no lock on the app** — an automatic consequence
	// needs an automatic trigger, and a button is a person asking a question,
	// not asking for their app to be taken off the air. The reading is still
	// written. The pass lock *is* taken: two Check-agains at once, or one
	// racing the timer's pass, each read "no open event" and each inserted
	// one, against one open row per (app, advisory) (code review 2026-11-11,
	// G-6).
	s.running.Lock()
	defer s.running.Unlock()
	return s.sweepOne(policy, rel, database.DependencyScanTriggerManual)
}

// sweepOne derives one app's findings, moves its event rows to match, writes
// the reading down, and — only on a scheduled pass, and only where the
// administrator has switched it on — locks the app.
func (s *Sweeper) sweepOne(policy deps.Policy, rel deps.ServingRelease,
	trigger string) ([]advisory.Finding, error) {

	s.readRuntimeInstalls(rel)

	findings, err := s.store.FindingsFor(rel.DeploymentID, rel.AppID, policy)
	if err != nil {
		return nil, err
	}

	// Only what this install has said it cares about opens an event. A
	// moderate transitive CVE with no fix is on nearly every Node app; one
	// event row per app per such finding, refreshed forever, is the noise that
	// buries the malware row.
	notable := map[string]advisory.Finding{}
	for _, f := range findings {
		if f.Waived || !policy.Counts(f) {
			continue
		}
		// Keyed by advisory: an advisory covering three of an app's packages
		// is one problem to be told about, not three.
		if prev, ok := notable[f.AdvisoryID]; !ok || (!prev.Malware() && f.Malware()) {
			notable[f.AdvisoryID] = f
		}
	}

	open, err := s.store.OpenEventFingerprints(rel.AppID, string(events.KindDependencyAdvisory))
	if err != nil {
		return nil, err
	}

	keep := make([]string, 0, len(notable))
	for id, f := range notable {
		keep = append(keep, id)
		if open[id] {
			// Already known. Recording it again every six hours is the thing
			// the fingerprint exists to prevent.
			continue
		}
		s.events.Record(events.Event{
			AppID: rel.AppID,
			Kind:  events.KindDependencyAdvisory,
			// A **warning**, never an error: nothing is broken and the app is
			// still serving. The same treatment a blocked auto-deploy gets.
			Severity:    "warning",
			Source:      database.EventSourceDependency,
			Summary:     advisorySummary(f),
			Detail:      advisoryDetail(f),
			Fingerprint: id,
			// The first evaluation of this app records in full and tells
			// nobody. Per app, so one onboarded in November is treated like
			// one that was there on day one.
			Silent: rel.BaselinedAt == nil,
		})
	}

	// Anything no longer matching is closed. Per fingerprint, because an app
	// that fixed one of four advisories has three problems left.
	if _, err := s.store.ResolveEventsExcept(rel.AppID,
		string(events.KindDependencyAdvisory), keep); err != nil {
		return nil, err
	}

	if rel.BaselinedAt == nil {
		if err := s.store.SetDependencyBaseline(rel.AppID, time.Now().UTC()); err != nil {
			return nil, err
		}
	}

	// The reading, written whether or not any of the above changed anything.
	// **A history that only recorded changes could not tell "checked,
	// unchanged for thirty days" from "not checked for thirty days"**, which is
	// the one question it exists to answer.
	if err := s.recordScan(policy, rel, trigger, findings); err != nil {
		return nil, err
	}

	if trigger == database.DependencyScanTriggerScheduled {
		s.lockIfNeeded(policy, rel, findings)
	}
	return findings, nil
}

// recordScan writes one row of history.
//
// The inventory is read for two facts the findings cannot supply: how many
// packages the release actually has, and whether any of them are in an
// ecosystem this build matches against. **Coverage is not "an inventory
// exists"** — an R release records every package it installs and can honestly
// read "61 packages, not scanned" — and a history row that derived `scanned`
// from a finding count would render that as a clean bill of health.
func (s *Sweeper) recordScan(policy deps.Policy, rel deps.ServingRelease,
	trigger string, findings []advisory.Finding) error {

	inventory, err := s.store.ListDeploymentDependencies(rel.DeploymentID)
	if err != nil {
		return err
	}
	scan := database.SummarizeScan(policy, findings, inventory)
	scan.AppID = rel.AppID
	scan.DeploymentID = rel.DeploymentID
	scan.Trigger = trigger
	scan.Scanned = deps.ScannedPackages(policy, inventory) > 0
	scan.OccurredAt = time.Now().UTC()
	return s.store.RecordDependencyScan(&scan, policy.HistoryKept)
}

// lockIfNeeded is the one automatic consequence, and every gate it passes is
// one the administrator can see.
//
// The order matters and is the order a reader would ask the questions in: is
// this install locking at all, is there a corpus to lock over, is this app
// already locked, and only then is there a finding that meets the threshold.
// **A lock over an empty corpus is a lock over no evidence**, which is why
// Locking folds HasData the way Enforcing does.
func (s *Sweeper) lockIfNeeded(policy deps.Policy, rel deps.ServingRelease, findings []advisory.Finding) {
	if !policy.Locking() {
		return
	}
	// The finding the message will name. Malware wins whatever order the join
	// returned it in: it is the class the reversal exists for.
	var worst *advisory.Finding
	for i := range findings {
		f := &findings[i]
		// A waiver is somebody having signed for this, with an expiry date. It
		// is the answer to an administrator who unlocks and means it, so it has
		// to be read here as well as in Counts.
		if f.Waived || !policy.LocksOn(*f) {
			continue
		}
		if worst == nil || (!worst.Malware() && f.Malware()) {
			worst = f
		}
	}
	if worst == nil {
		return
	}

	app, err := s.store.GetApp(rel.AppID)
	if err != nil || app == nil {
		// A delete crossing the sweep is an outcome, not a failure.
		return
	}
	if app.Locked() {
		// **Never a re-lock.** The guarded transition below would decline it
		// anyway; this saves the round trip and says why out loud: an
		// administrator's message is theirs, and a policy that replaced it with
		// a generated one would be rewriting the record.
		return
	}

	now := time.Now().UTC()
	locked, err := s.store.LockAppForDependency(app.ID, app.Slug, lockMessage(*worst), map[string]any{
		"reason":      "dependency",
		"advisory_id": worst.AdvisoryID,
		"package":     worst.Package,
		"version":     worst.Version,
		"malware":     worst.Malware(),
		"severity":    worst.Severity,
		"threshold":   policy.LockOn,
	}, now)
	if err != nil {
		log.Printf("deps: locking %s: %v", app.Slug, err)
		return
	}
	if !locked {
		// Another node got there first. It changed a row, wrote the audit entry
		// and told the owner; this one does none of those again.
		return
	}

	// Stopping the workload is the second half of a lock, and it comes second
	// for the reason handleLockApp's does: in this order a failure to stop
	// leaves an app locked and still running — refused at the proxy, which is
	// the safe half — while the other order leaves one stopped and unlocked,
	// which reads to its owner as a platform fault.
	s.stopWorkload(app)

	// Telling somebody is the last thing and cannot fail the lock: no send may
	// fail or block the thing that triggered it.
	s.events.Notify("app_locked_dependency", app.ID,
		lockSummary(*worst), lockDetail(*worst, policy.LockOn), database.EventSeverityError)
}

// stopWorkload takes the app off the air. Best effort and logged, never fatal:
// the lock is the decision and it has been made.
func (s *Sweeper) stopWorkload(app *database.App) {
	// A batch app has no service to stop, and writing `stopped` would strand it:
	// nothing starts a batch app, so it would stay stopped after the unlock.
	// Its runs are the workload, and the jobs sweep cancels those for a locked
	// app.
	if strings.HasPrefix(app.Framework, "batch-") {
		return
	}
	if s.rts == nil {
		return
	}
	rt, err := s.rts.For(app.RunningModeOrShared())
	if err != nil {
		log.Printf("deps: no backend to stop %s on after locking it: %v", app.Slug, err)
		return
	}
	// An absent workload is already down, which is what locking was for.
	if serr := rt.Stop(app.ProcessName()); serr != nil && runtime.AbsenceReason(serr) == "" {
		log.Printf("deps: stopping %s after locking it: %v", app.Slug, serr)
		return
	}
	if err := s.store.UpdateAppStatus(app.ID, database.AppStatusStopped); err != nil {
		log.Printf("deps: marking %s stopped after locking it: %v", app.Slug, err)
	}
}

// lockMessage is what a refused visitor is shown — ResolveLockMessage's first
// layer, filled in so that nobody gets the install-wide default about
// contacting an administrator who does not yet know either.
//
// **No layer of the message may be blank**, and this one is written by the
// platform because the platform is who decided.
func lockMessage(f advisory.Finding) string {
	what := "a package with a " + severityWord(f) + " advisory (" + f.AdvisoryID + ")"
	if f.Malware() {
		what = "a package reported as malicious (" + f.AdvisoryID + ")"
	}
	return "This app was locked automatically because the release it is serving installs " +
		what + ". Its Dependencies tab has the finding. An administrator can unlock it, " +
		"and this install's dependency policy is what decides when that lock returns."
}

// lockSummary is the one line the owner reads. It names the package, because
// the app is already the context wherever this appears.
func lockSummary(f advisory.Finding) string {
	if f.Malware() {
		return fmt.Sprintf("Locked: %s %s is reported as a malicious package (%s)",
			f.Package, f.Version, f.AdvisoryID)
	}
	return fmt.Sprintf("Locked: %s %s has a %s advisory (%s)",
		f.Package, f.Version, severityWord(f), f.AdvisoryID)
}

// lockDetail is the actionable half, and it promises nothing it cannot do: no
// mail elsewhere, no unlock on its own, and no claim that the app's code was
// read.
func lockDetail(f advisory.Finding, threshold string) string {
	var b strings.Builder
	if f.Summary != "" {
		b.WriteString(f.Summary)
		b.WriteString("\n\n")
	}
	b.WriteString("This install locks an app when its serving release installs ")
	if threshold == database.DependencyLockMalware {
		b.WriteString("a package reported as malicious")
	} else {
		b.WriteString("a package reported as malicious, or one with a " + threshold +
			" advisory or worse")
	}
	b.WriteString(". The app has been stopped and visitors are being told it is locked.\n\n")
	if f.FixedIn != "" {
		b.WriteString("Fixed in " + f.FixedIn + ". ")
	} else {
		b.WriteString("No fixed version has been published. ")
	}
	if f.Path != "" && !f.Direct {
		b.WriteString("Reached via: " + f.Path + ". ")
	}
	b.WriteString("\n\nDeploy a release that does not install it, or ask an administrator " +
		"to unlock the app. An unlocked app whose finding still matches is locked again on " +
		"the next pass, so the way to mean it is a waiver: per app, per advisory, with a " +
		"reason and an expiry date.")
	if f.URL != "" {
		b.WriteString("\n" + f.URL)
	}
	return b.String()
}

// severityWord is the level as a person reads it. An advisory nobody scored is
// "unscored", never blank and never "unknown" dressed as a level.
func severityWord(f advisory.Finding) string {
	if f.Severity == advisory.SeverityUnknown || f.Severity == "" {
		return "unscored"
	}
	return f.Severity
}

// advisorySummary is the one line a person reads in a list and in a subject.
//
// It names the package rather than the app, because the app is already the
// context everywhere this appears, and it **does not promise mail** — the copy
// points at the tab, because either notification switch may be off and the
// record is what a workflow may rely on.
func advisorySummary(f advisory.Finding) string {
	if f.Malware() {
		return fmt.Sprintf("%s %s is reported as a malicious package (%s)",
			f.Package, f.Version, f.AdvisoryID)
	}
	severity := f.Severity
	if severity == advisory.SeverityUnknown {
		severity = "unscored"
	}
	return fmt.Sprintf("%s %s has a %s advisory (%s)", f.Package, f.Version, severity, f.AdvisoryID)
}

func advisoryDetail(f advisory.Finding) string {
	detail := f.Summary
	if detail != "" {
		detail += "\n\n"
	}
	if f.Malware() {
		detail += "This app is still serving and was not stopped. "
	}
	if f.FixedIn != "" {
		detail += "Fixed in " + f.FixedIn + ". "
	} else {
		detail += "No fixed version has been published. "
	}
	if f.Path != "" && !f.Direct {
		detail += "Reached via: " + f.Path + ". "
	}
	detail += "\n\nThe app's Dependencies tab has the full finding set and says which " +
		"release it describes."
	if f.URL != "" {
		detail += "\n" + f.URL
	}
	return detail
}

// rFrameworks are the five whose release carries an R library.
var rFrameworks = map[string]bool{
	"shiny": true, "plumber": true, "rmarkdown": true, "quarto": true, "batch-r": true,
}

// platformRPackages is what Toft installs into every R release, for an
// inventory recorded before the deploy said so itself: renv, the SDK and the
// SDK's dependencies. A release built since is classified by its deploy
// (builder.rInstalledGate) and never consults this.
var platformRPackages = append([]string{"renv", "toft"}, sdk.RSDKDependencies...)

// readRuntimeInstalls appends to a shared R app's inventory every package its
// library holds that the inventory does not: the packages it installed while
// it ran.
//
// **The only reading the sweep takes off disk, and the reason it exists.** An
// R app with no renv.lock installs nothing of its own at deploy; what it loads
// it installs at runtime with install.packages(), into the release library —
// the one library R_LIBS_SITE gives it, and a tree a shared start keeps
// (builder.ArtifactResolver). Without this, the dependency policy could never
// see a package that app chose, and malware in one would never lock it.
//
// Append, never replace: a package installed at 02:00 and removed by 03:00 was
// still loaded by the app, and the inventory is the record of what was seen.
// The verdict is untouched — it is the deploy's — and the findings follow the
// inventory as they always have. Isolated mode is a stated gap: the pod's
// library is not on this machine.
func (s *Sweeper) readRuntimeInstalls(rel deps.ServingRelease) {
	if s.treeDir == nil || !rFrameworks[rel.Framework] {
		return
	}
	if mode := rel.RunningMode; mode != "" && mode != string(runtime.ModeShared) {
		return
	}
	dir := s.treeDir(rel.ReleaseRef)
	if dir == "" {
		return
	}
	inventory, err := s.store.ListDeploymentDependencies(rel.DeploymentID)
	if err != nil {
		log.Printf("deps: reading %s's inventory before its library: %v", rel.Slug, err)
		return
	}
	have := map[string]bool{}
	classified := false
	for _, d := range inventory {
		have[d.Ecosystem+"/"+d.Package+"@"+d.Version] = true
		if src := deps.SourceOf(d); src == deps.SourceInstalled || src == deps.SourcePlatform {
			classified = true
		}
	}

	var fresh []deps.Dependency
	for _, lib := range deps.RLibraryDirs(dir) {
		found, _, err := deps.ReadRLibrary(lib)
		if err != nil {
			log.Printf("deps: reading %s's R library: %v", rel.Slug, err)
			continue
		}
		for _, d := range found {
			key := d.Ecosystem + "/" + d.Package + "@" + d.Version
			if have[key] {
				continue
			}
			have[key] = true
			d.Source = deps.SourceRuntime
			if !classified && slices.Contains(platformRPackages, d.Package) {
				d.Source = deps.SourcePlatform
			}
			fresh = append(fresh, d)
		}
	}
	if len(fresh) == 0 {
		return
	}
	n, err := s.store.AppendDeploymentDependencies(rel.DeploymentID, fresh)
	if err != nil {
		log.Printf("deps: recording %s's runtime R packages: %v", rel.Slug, err)
		return
	}
	if n > 0 {
		log.Printf("deps: %s: %d R package(s) found in its library that its deploy did not install", rel.Slug, n)
	}
}
