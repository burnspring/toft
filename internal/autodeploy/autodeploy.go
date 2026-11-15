// Package autodeploy deploys new commits automatically for apps that opted in.
//
// It polls rather than listens: on each pass it asks every auto-deploy app's
// remote (git ls-remote, no clone) for the tracked branch's tip and compares
// it to the last commit already claimed for a build. Polling needs no inbound
// reachability from the git host — the right trade for an internal platform —
// at the cost of up to one interval of latency.
//
// Multi-node safety comes from the claim, not the ticker: every node may
// observe the same new commit, but advancing apps.last_seen_sha is a guarded
// UPDATE (WHERE last_seen_sha = old), so exactly one node wins and enqueues
// the build. A failed build does not retry on the next pass — the claim has
// already advanced — it takes another push or a manual redeploy, which is what
// keeps a broken commit from building every minute forever.
package autodeploy

import (
	"context"
	"errors"
	"log"
	"time"

	"toft/internal/builder"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/gitconn"
	"toft/internal/telemetry"
)

const (
	// lsRemoteTimeout bounds one remote query; a hung git host must not stall
	// the whole pass.
	lsRemoteTimeout = 30 * time.Second
	// treeFetchTimeout bounds the blob-less fetch that reads a folder app's
	// tree; it moves more than ls-remote does, and fails open to a build.
	treeFetchTimeout = 60 * time.Second
	// pendingWindow is how recent a pending deployment must be to count as
	// "already queued". Older pending rows are orphans of a restart (the build
	// queue is in-memory) and must not suppress deploys forever.
	pendingWindow = 30 * time.Minute
)

type Poller struct {
	db  *database.DB
	bld *builder.Builder
	// gitCred is the one reader of a clone credential, shared with the
	// builder. The poller used to decrypt apps.git_token here, with a sentence
	// of its own that the build never said; the resolver is where that
	// disagreement was resolved rather than documented.
	gitCred *gitconn.Resolver
	// events is where a failed poll goes. This is the loudest source of events
	// on the platform — a revoked token is rediscovered once per interval,
	// forever — which is why the recorder collapses mail on a fingerprint and
	// why the classification below is the fingerprint rather than the message.
	//
	// May be nil; every call through it is nil-safe.
	events *events.Recorder
	// interval is the number this process was started with, and the floor the
	// dial defers to. See the envelope below.
	interval time.Duration
	// dials is where the administrator's poll interval is read from, at each
	// tick rather than at construction. Nil is a poller with no settings
	// document behind it — a test — and the environment's interval stands.
	dials *config.Resolver
}

func New(db *database.DB, bld *builder.Builder, gitCred *gitconn.Resolver, rec *events.Recorder,
	interval time.Duration, dials *config.Resolver) *Poller {
	return &Poller{db: db, bld: bld, gitCred: gitCred, events: rec, interval: interval, dials: dials}
}

// Start launches the poll loop. It runs for the life of the process.
//
// A timer rather than a ticker, reset from the envelope after every tick, so an
// administrator who shortens the interval is waited on for at most one of the
// old ones. **Whether the poller exists at all is not a dial**: an install with
// AUTO_DEPLOY_POLL_INTERVAL=0 never constructs one, and a stored 0 means "the
// operator's number" like every other dial — see database.SettingAutoDeployPollSeconds
// for why the two zeroes deliberately differ.
func (p *Poller) Start() {
	go func() {
		t := time.NewTimer(p.every())
		defer t.Stop()
		for range t.C {
			p.tick()
			t.Reset(p.every())
		}
	}()
}

// every is the interval as it is now: the administrator's where they set one,
// and the operator's otherwise. Never zero — a timer reset to zero would spin.
func (p *Poller) every() time.Duration {
	if p.dials != nil {
		if n := p.dials.Envelope().AutoDeployPollSeconds; n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if p.interval > 0 {
		return p.interval
	}
	return time.Minute
}

func (p *Poller) tick() {
	ctx, span := telemetry.Start(context.Background(), "autodeploy.poll")
	start := time.Now()

	apps, err := p.db.ListAutoDeployApps()
	if err != nil {
		log.Printf("autodeploy: listing apps: %v", err)
		span.End(err)
		telemetry.RecordAutodeployPoll(ctx, telemetry.OutcomeFailed, time.Since(start))
		return
	}
	defer func() {
		span.EndOK(telemetry.OutcomeSucceeded)
		telemetry.RecordAutodeployPoll(ctx, telemetry.OutcomeSucceeded, time.Since(start))
	}()
	// Sequential on purpose: N apps × one bounded ls-remote is cheap, and it
	// keeps the platform from hammering a git host with parallel queries.
	for i := range apps {
		p.check(&apps[i])
	}
}

// deploysOnPush reports whether a push should deploy an app in this status.
//
// Exactly one status is excluded, and the distinction matters more than it
// looks: a *stopped* app was stopped on purpose and deploying would silently
// restart it, while a *sleeping* one was stopped by the platform to reclaim
// memory and must still pick up new commits — it is asleep, not switched off.
// It rejoins auto-deploy when someone starts it again.
func deploysOnPush(status string) bool {
	return status != database.AppStatusStopped
}

// The reasons a poll pass does not deploy an app. A closed set, because
// `toft.skip_reason` is a metric attribute — and the source-policy one is
// the reason this counter exists at all: an administrator who has just narrowed
// the allowlist (ADR §29) needs to see how much stopped deploying, and one
// event per transition is deliberately not a number.
const (
	skipStopped = "app_stopped"
	// skipLocked is its own reason rather than folded into app_stopped, for the
	// same argument that gives every other skip its own label: an administrator
	// looking at why deploys dried up needs "somebody locked these" and "these
	// were stopped" to be different numbers. A locked app is usually also
	// stopped, so without this the lock would be invisible in the metric.
	skipLocked       = "app_locked"
	skipSourcePolicy = "source_policy"
	skipGitAuth      = "git_credential"
	skipLsRemote     = "ls_remote_failed"
)

func (p *Poller) check(app *database.App) {
	// The lock first, and before the status test: a locked app is usually
	// stopped too, and attributing the skip to the stop would hide the decision
	// that actually caused it. A git push must never revive a locked app —
	// without this the one path that deploys on the platform's own initiative
	// would be the one path ignoring the lock.
	if app.Locked() {
		telemetry.RecordAutodeploySkip(context.Background(), skipLocked)
		return
	}
	if !deploysOnPush(app.Status) {
		telemetry.RecordAutodeploySkip(context.Background(), skipStopped)
		return
	}

	// The source policy, before the platform reaches the remote at all.
	//
	// Deliberately ahead of ls-remote and not merely ahead of the build: an
	// install that has just blocked a host should stop talking to it, and a
	// query to a repository this install will not deploy from is a request
	// nobody authorized.
	//
	// auto_deploy is left **on**. Re-allowing the source resumes deployment with
	// no further action, which is what makes the switch reversible; turning the
	// flag off would leave every affected owner to remember to turn it back on,
	// and the ones who did not would silently never deploy again.
	if why := p.bld.SourceRefusal(app); why != "" {
		p.recordSourceBlocked(app, why)
		telemetry.RecordAutodeploySkip(context.Background(), skipSourcePolicy)
		return
	}
	// The source is allowed, so whatever was blocking it is over. Clear is a
	// no-op unless somebody was actually told.
	p.events.Clear(app.ID, events.KindSourceBlocked)

	// A credential that will not decrypt — a rotated ENCRYPTION_KEY, in
	// practice — is indistinguishable from a revoked one to the app's owner:
	// pushes stop deploying and nothing says why. The resolver phrases that,
	// so the build log and this event now say the same thing about the same
	// app on the same afternoon.
	cred, err := p.gitCred.CloneCredential(context.Background(), app)
	if err != nil {
		var unreadable *gitconn.Unreadable
		summary := err.Error()
		detail := err.Error()
		if errors.As(err, &unreadable) {
			summary = unreadable.Summary
			detail = unreadable.Err.Error()
		}
		log.Printf("autodeploy: %s: reading the git credential: %v", app.Slug, err)
		p.recordGitFailure(app, builder.GitFailureAuth, summary, detail)
		telemetry.RecordAutodeploySkip(context.Background(), skipGitAuth)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), lsRemoteTimeout)
	defer cancel()
	sha, err := lsRemote(ctx, app.RepoURL, app.Branch, cred.Token)
	if err != nil {
		log.Printf("autodeploy: %s: %v", app.Slug, err)
		class := builder.ClassifyGitError(err.Error())
		p.recordGitFailure(app, class, class.Explain(app.RepoURL, app.Branch, cred), err.Error())
		telemetry.RecordAutodeploySkip(context.Background(), skipLsRemote)
		return
	}
	// The remote answered, so whatever was wrong with reaching it is over. This
	// is the only place that can know that, and it runs every interval — so a
	// token someone fixed produces a recovery notice within a minute.
	p.events.Clear(app.ID, events.KindGitAuthFailed)
	p.events.Clear(app.ID, events.KindGitFetchFailed)

	if sha == app.LastSeenSHA {
		return
	}

	// A queued build, or a running one that has not cloned yet, will clone
	// the tip, so it already covers this commit; don't claim, so the next pass
	// re-checks in case that build cloned before the push landed. A running
	// build that cloned *this* commit covers it outright: claim it as seen
	// and build nothing, or a push followed by Deploy builds the commit twice
	// (security review 2026-11-14, DG-13).
	uncloned, building, err := p.db.InFlightDeployments(app.ID, time.Now().Add(-pendingWindow))
	if err != nil || uncloned {
		if err != nil {
			log.Printf("autodeploy: %s: checking in-flight deployments: %v", app.Slug, err)
		}
		return
	}
	for _, c := range building {
		if c == sha {
			won, err := p.db.ClaimAutoDeploySHA(app.ID, app.LastSeenSHA, sha)
			if err == nil && won {
				log.Printf("autodeploy: %s: %s is already being deployed", app.Slug, short(sha))
			}
			return
		}
	}

	if p.folderUnchanged(app, sha, cred.Token) {
		won, err := p.db.ClaimAutoDeploySHA(app.ID, app.LastSeenSHA, sha)
		if err == nil && won {
			log.Printf("autodeploy: %s: %s leaves %s unchanged", app.Slug, short(sha), app.RepoSubdir)
		}
		return
	}

	// The version cap, checked *before* the claim and deliberately not after.
	// Claiming advances last_seen_sha, and a commit marked seen never deploys
	// again without another push — so a blocked commit claimed here would be
	// lost for good, and freeing space afterwards would not bring it back.
	// Leaving it unclaimed means the next pass reconsiders it, and it deploys
	// the moment the app is under the cap again.
	if why := p.capBlocks(app); why != "" {
		p.recordBlocked(app, sha, why)
		return
	}

	won, err := p.db.ClaimAutoDeploySHA(app.ID, app.LastSeenSHA, sha)
	if err != nil {
		log.Printf("autodeploy: %s: claiming %s: %v", app.Slug, short(sha), err)
		return
	}
	if !won {
		return // another node (or a just-finished build) got there first
	}

	deploy := &database.Deployment{
		AppID:       app.ID,
		Branch:      app.Branch,
		Status:      database.DeployStatusPending,
		TriggeredBy: database.DeployTriggerAuto,
	}
	// The system's: the push came from someone Toft never authenticated. The
	// commit is known here, unlike at every other door — it is the one this
	// poll just claimed — so the record carries it; the row itself is left for
	// the build to stamp with what it cloned.
	if err := p.db.CreateDeployment(deploy, &database.DeployRecord{ActorType: "system", Via: "git", CommitSHA: sha}); err != nil {
		log.Printf("autodeploy: %s: creating deployment: %v", app.Slug, err)
		return
	}

	log.Printf("autodeploy: %s: new commit %s on %s — deploying", app.Slug, short(sha), app.Branch)
	p.bld.Enqueue(&builder.BuildJob{App: app, Deployment: deploy})
}

// The remote, as the poller reaches it; tests stand in for it.
var (
	lsRemote   = builder.LsRemote
	subdirTree = builder.SubdirTree
)

// folderUnchanged reports whether the new tip leaves the folder an app is
// built from exactly as its last successful build cloned it — in a monorepo,
// a push to another app's folder (ADR §112). Then the commit is claimed as
// seen and nothing builds: the build would produce the release already
// serving. Anything it cannot establish — an app at the repository root, no
// recorded tree yet, a fetch that fails, a branch that moved again between
// ls-remote and the fetch — is "changed", and the app builds as it always did.
func (p *Poller) folderUnchanged(app *database.App, sha, token string) bool {
	if app.RepoSubdir == "" || app.LastSeenTree == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), treeFetchTimeout)
	defer cancel()
	tip, tree, err := subdirTree(ctx, app.RepoURL, app.Branch, token, app.RepoSubdir)
	if err != nil {
		log.Printf("autodeploy: %s: reading %s at %s, building: %v", app.Slug, app.RepoSubdir, short(sha), err)
		return false
	}
	return tip == sha && tree == app.LastSeenTree
}

// capBlocks reports why this app may not take a new version, or "" to go
// ahead. A resolution that fails lets the deploy through: the poller is the
// path with no one watching, and an install where pushes silently stopped
// deploying because a query was failing is worse than one that kept deploying.
func (p *Poller) capBlocks(app *database.App) string {
	ret, versions, err := p.bld.VersionPolicy(app)
	if err != nil {
		log.Printf("autodeploy: %s: resolving the version cap, deploying anyway: %v", app.Slug, err)
		return ""
	}
	return ret.BlockReason(versions)
}

// recordBlocked leaves a failed deployment row for a push that could not be
// built, once per commit.
//
// A push that did not deploy is a real event and belongs in the history: the
// alternative is an owner watching commits land and nothing happen, with the
// explanation living only in a server log they cannot read. But the poller
// revisits an unclaimed commit every pass, so the row is written only when the
// newest deployment is not already this commit's refusal — otherwise a blocked
// app would accumulate one row a minute, and the history that was meant to
// explain the problem would bury it.
func (p *Poller) recordBlocked(app *database.App, sha, why string) {
	deployments, err := p.db.ListDeployments(app.ID)
	if err != nil {
		log.Printf("autodeploy: %s: reading deployments before recording a blocked push: %v", app.Slug, err)
		return
	}
	if len(deployments) > 0 {
		if d := deployments[0]; d.Status == database.DeployStatusFailed &&
			d.TriggeredBy == database.DeployTriggerAuto && d.CommitSHA == sha {
			return // already recorded; say nothing until something changes
		}
	}

	log.Printf("autodeploy: %s: %s not deployed — %s", app.Slug, short(sha), why)

	now := time.Now().UTC()
	deploy := &database.Deployment{
		AppID:       app.ID,
		CommitSHA:   sha,
		Branch:      app.Branch,
		Status:      database.DeployStatusFailed,
		TriggeredBy: database.DeployTriggerAuto,
		BuildLog:    "==> Not deployed.\n" + why + "\n",
		FinishedAt:  &now,
	}
	// No app.deploy: this records a push that was not built.
	if err := p.db.CreateDeployment(deploy, nil); err != nil {
		log.Printf("autodeploy: %s: recording the blocked push: %v", app.Slug, err)
		return
	}
	// CreateDeployment writes the row as pending; this is the same two-step
	// every failed build takes, and it is what puts the reason in the log.
	if err := p.db.UpdateDeployment(deploy); err != nil {
		log.Printf("autodeploy: %s: recording why the push was blocked: %v", app.Slug, err)
	}
}

// recordSourceBlocked writes one event when an app transitions into the
// blocked state — not one per poll.
//
// The poller runs every minute forever, so "record it and let the cooldown
// handle the mail" is not enough here: the mail would collapse but the Events
// tab would fill with 1,440 identical rows a day and bury the one that
// explains the problem. The open-event check is what makes it a transition
// rather than a condition, and Clear on the success path is the other half.
//
// Nothing is written when the check itself fails. A row we are not sure is new
// is worse than a row missing for one interval — the next pass will write it.
func (p *Poller) recordSourceBlocked(app *database.App, why string) {
	open, err := p.db.HasOpenAppEvent(app.ID, string(events.KindSourceBlocked))
	if err != nil {
		log.Printf("autodeploy: %s: checking for an open source_blocked event: %v", app.Slug, err)
		return
	}
	if open {
		return
	}

	log.Printf("autodeploy: %s: not polling — %s", app.Slug, why)
	p.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindSourceBlocked,
		Severity: database.EventSeverityWarning,
		Source:   database.EventSourceAutodeploy,
		// A warning, not an error: the app is serving, nothing is broken, and an
		// administrator's decision is not a malfunction. What has stopped is the
		// automatic deploy, and the summary says exactly that rather than
		// implying the app is down.
		Summary: "Automatic deploys have stopped: " + app.RepoURL + " is not an approved source on " +
			"this install. The app keeps running and keeps serving.",
		Detail:      why,
		Fingerprint: string(events.KindSourceBlocked),
	})
}

// recordGitFailure turns a classified poll failure into an app event.
//
// The fingerprint is the classification, not the message: git's stderr carries
// the repository URL and sometimes a request id, so fingerprinting on the text
// would make every poll a brand-new problem and mail once a minute — which is
// the failure mode this whole feature exists to avoid.
//
// Severity follows GitFailure.NeedsAttention: something a person must fix is an
// error, an unreachable host is a warning, because the next poll may well
// succeed and the app is not actually broken.
func (p *Poller) recordGitFailure(app *database.App, class builder.GitFailure, summary, detail string) {
	kind := events.KindGitFetchFailed
	severity := database.EventSeverityWarning
	if class.NeedsAttention() {
		kind = events.KindGitAuthFailed
		severity = database.EventSeverityError
	}
	p.events.Record(events.Event{
		AppID:       app.ID,
		Kind:        kind,
		Severity:    severity,
		Source:      database.EventSourceAutodeploy,
		Summary:     summary,
		Detail:      detail,
		Fingerprint: string(class),
	})
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// PollNow runs one pass over the named apps immediately, instead of waiting for
// the next tick.
//
// **It is an accelerator and never the mechanism.** A verified webhook delivery
// calls this and nothing else changes: `check` does exactly what it does on a
// tick — the source policy, the credential, ls-remote, the guarded claim — so a
// delivery that never arrives, arrives twice, or fails verification costs the
// install exactly the minute it was buying, and an install with no inbound
// reachability needs no configuration and no reachability query at all.
//
// Enqueues nothing itself. Two deliveries are two passes and one build, because
// the guarded claim was already the mechanism that made several nodes seeing one
// push build it once.
//
// In its own goroutine so an unauthenticated HTTP handler never waits on a git
// host, and never on this platform's own poll loop.
func (p *Poller) PollNow(appIDs []string) {
	if p == nil || len(appIDs) == 0 {
		return
	}
	go func() {
		for _, id := range appIDs {
			app, err := p.db.GetApp(id)
			if err != nil || app == nil {
				continue
			}
			// The same gate the tick applies. A webhook must not be a way to
			// deploy an app the poller would have skipped.
			if !app.AutoDeploy {
				continue
			}
			p.check(app)
		}
	}()
}
