package builder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"toft/internal/appint"
	"toft/internal/applog"
	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/deploysteps"
	"toft/internal/deps"
	"toft/internal/events"
	"toft/internal/gitconn"
	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/telemetry"
	"toft/internal/textutil"
	"toft/internal/toolchain"
	"toft/sdk"
)

const (
	// readinessTimeout is how long a freshly started release has to accept a
	// connection before the deploy is considered failed.
	readinessTimeout = 90 * time.Second
	commandTimeout   = 10 * time.Minute
	// buildOutputGrace is how long a build step's Wait waits, after the step
	// has exited, for whatever it started to let go of its output.
	buildOutputGrace = 2 * time.Second
)

// PendingDeployWindow is how recent a pending deployment must be to block a new
// one. Pending rows older than this were orphaned by a restart — the build queue
// is in memory — and must not wedge the app.
//
// Exported, and here rather than in internal/api, because it is a property of
// *this* queue and there are now two things outside the API that enqueue into
// it. Two copies of a coalescing window is two answers to "why did my deploy
// not happen".
const PendingDeployWindow = 30 * time.Minute

type Options struct {
	AppsDir string
	// JobRunDir is JOB_RUN_DIR: where a host job run's directory lives, under
	// <app id>, when it is not APPS_DIR. Empty keeps runs under APPS_DIR.
	JobRunDir string
	// LogsDir is LOGS_DIR, where the shared backend writes each app's output
	// under LOGS_DIR/<app-id>. An app's directory there goes with the app.
	LogsDir string
	BaseURL string
	// CacheDir holds the shared npm, uv, and Go build/module caches. Sharing
	// them across apps is what keeps a warm rebuild from re-downloading the world.
	CacheDir string
	// AllowBuildScripts permits npm lifecycle scripts (postinstall etc.) during
	// install. They are arbitrary code from the dependency tree, so the default
	// is off; apps that genuinely need them (prisma, sharp) opt in.
	AllowBuildScripts bool
	// Dials resolves the operating envelope — the memory ceiling, the two CPU
	// figures, the R repository and how many builds may start — at each use
	// rather than at construction. It sits here rather than as a set of numbers
	// because a number copied onto Options at process start can only change on a
	// restart, which is the thing the dial exists to end.
	Dials *config.Resolver
	// Reports announces a scheduled re-render to the people its owner named.
	//
	// An interface rather than the concrete deliverer, and nil-checked at the
	// call site, because this package is built and tested on installs that have
	// no mail at all — and because a builder that could not be constructed
	// without a mail service would make every build test carry one.
	Reports ReportDeliverer
	// MaxConcurrent is the size the worker pool is *built* to, and it stays a
	// construction-time number: resizing a goroutine pool at runtime means
	// stopping workers, which means abandoning a build or waiting on one, in a
	// package whose whole job is not to abandon builds.
	//
	// How many builds may **start** is the dial, read from the envelope before
	// each dequeue and bounded above by this. Lowering it stops new builds
	// starting and interrupts none; raising it takes effect at the next
	// dequeue. The pool is built to at least buildPoolFloor so the dial has room
	// on an install that never touched MAX_CONCURRENT_BUILDS — a setting that is
	// disabled on every default install reads as broken.
	MaxConcurrent int
	// Events records a failed build where the app's people can see it. Only
	// *automatic* deploys are recorded: a manual deploy has someone watching
	// the streamed log, and mailing them about what is on their screen is the
	// kind of notification people build filters for.
	//
	// May be nil; every call through it is nil-safe.
	Events *events.Recorder
	// Deps resolves the dependency policy — enforcement point 1 of three
	// (ADR §39). Nil means the capability is absent: no resolve step, no
	// inventory, no gate, which is what an install with the feature off gets
	// and is also what every test that does not care about it gets.
	Deps *deps.Resolver
	// Roles derives the per-app, per-start key a shared workload verifies
	// X-Toft-Role with. The platform's JWT service is the only
	// implementation; it is an interface here because the builder has no other
	// reason to hold one, and because nil has to be a working configuration.
	//
	// Nil means no key reaches the workload, so every role assertion it
	// receives reads as `unknown` and refuses. That is what a test builder
	// gets, and it fails in the safe direction.
	Roles auth.RoleMinter
	// BuildAccount names the OS account an app's build commands run as, which
	// is the account its processes run as (ADR §29). Nil, or an answer of nil,
	// is "the platform's own" — an install that separates nothing, where the
	// app would hold the platform's privileges at run time anyway.
	BuildAccount func(slug string) (*runtime.AppAccount, error)
	// DiskSoftFreePercent and DiskHardFreePercent override the release
	// disk's two watermarks for a test. Zero is DiskSoftFree/DiskHardFree.
	DiskSoftFreePercent int
	DiskHardFreePercent int
}

type Builder struct {
	// identities is the account each in-flight build or repair runs as, keyed
	// by the id its commands are run under (identity.go).
	identities sync.Map

	// rPlatform is, per release directory being deployed, the R packages the
	// library held before the app's own were restored: Toft's, whatever the
	// app said (rInstalledGate). Written by the R install, taken by the gate.
	rPlatform sync.Map

	// lockClears is, per deployment, whether the last judgement its release
	// passed found nothing at or above dependency_lock_on. Absent is "not
	// judged", which never lifts a lock (liftDependencyLock).
	lockClears sync.Map

	// starting holds one *sync.Mutex per app id: two starts of one app run one
	// after the other, never interleaved. See startLock.
	starting sync.Map

	// storagePlanner judges and writes toft.toml's [[storage]]; nil refuses
	// the table. See SetStoragePlanner.
	storagePlanner StoragePlanner

	// flipping holds one *sync.Mutex per app id: two deploys of one app flip
	// one after the other. See flipLock.
	flipping sync.Map

	// reconciled closes when the boot Reconcile has been through every app, so
	// the health watcher does not judge an app the reconciler has not reached.
	reconciled     chan struct{}
	reconciledOnce sync.Once

	db *database.DB
	// rts holds one execution backend per mode. The builder talks to whichever
	// one the deployment's mode selects, and only through the workload
	// interface: it hands over a spec and receives an endpoint, and never
	// learns whether the workload is a process, a task, or a pod.
	//
	// Selecting per deployment rather than per install is what makes execution
	// mode real for services. Before it, promoting an app changed a row and
	// unlocked background jobs while the app itself kept running as a process
	// beside every other shared app.
	rts       *runtime.Runtimes
	encryptor *crypto.Encryptor
	// gitCred is the one reader of a clone credential, shared with the
	// auto-deploy poller so that the two cannot drift about which credential
	// an app authenticates with or what to say when it will not open.
	gitCred *gitconn.Resolver
	// artifacts makes a release durable beyond the machine that built it. Nil
	// means the install offers no artifact store — a VM install — and releases
	// stay local directories, which is what they have always been.
	artifacts artifact.Store
	// narrator is where a *repair* says what it did, for the app's owner. Nil
	// is a real configuration — a builder standing on its own in a test — and
	// every call through it is a no-op then.
	//
	// A repair only, never a build: a deploy already streams its whole log to
	// a surface the owner reads, and putting the same words in two places is
	// the duplication this design otherwise argues against.
	narrator *applog.Sink
	opts     Options

	// pool is how many workers exist, and starts is the token each one takes
	// before it begins. The channel's capacity is the pool; the dial decides how
	// much of that capacity may be in use. See acquireStart.
	pool   int
	starts chan struct{}

	queue chan *BuildJob
	mu    sync.RWMutex
	// Two fabrics over one mutex, because they are written from the same
	// places: a deploy's output, and where it has got to. See SubscribeProgress
	// for why they are not one stream.
	listeners map[string][]chan string        // deployment ID -> log listeners
	watchers  map[string][]chan ProgressEvent // deployment ID -> progress watchers

	// live is each running build's log so far, so a reader who arrives
	// mid-build is handed every line rather than the ones after it connected.
	// Guarded by mu, with listeners, so a subscribe and a line cannot interleave.
	live map[string][]string
	// liveAt is when each entry of live was written, index for index — the
	// line times of a build (ADR §155). Kept here rather than beside buildLog
	// in execute so a mid-build reader is handed them with the backlog, under
	// the one lock.
	liveAt map[string][]time.Time

	// cacheMu guards the shared npm/uv/Go cache against being emptied while
	// something is building out of it.
	//
	// It is a read/write lock the wrong way round from the intuition: a *build*
	// takes the read side, because any number of builds may use the cache at
	// once, and the *reclaim* takes the write side, because it is the only
	// thing that may not overlap with them. Builds therefore stay exactly as
	// concurrent as they were — RLock does not exclude RLock — and a reclaim
	// waits for the last one out.
	//
	// The rule the whole design rests on, stated mechanically: **a path that
	// calls installDependencies holds cacheMu.RLock**. There are two, execute
	// and Repair, and a third is the foreseeable regression —
	// TestEveryBuildPathHoldsTheCacheLock is what catches it.
	//
	// The two meet: execute's rt.Deploy reaches Repair through the shared
	// backend, on the same goroutine, which the source walk cannot see through
	// runtime.Repairer. The lock is not re-entrant, so the spec says the
	// caller holds it (WorkloadSpec.BuildHoldsCache) and Repair does not take
	// it again (PB-3).
	//
	// A build started while a reclaim is waiting queues behind it, because Go's
	// RWMutex stops admitting readers once a writer is blocked. That is what
	// lets the reclaim wait rather than refuse: a busy install cannot starve it.
	//
	// It lives on the Builder rather than in a package of its own because the
	// Builder is the only type that knows the four cache directories exist, and
	// the only one with a synchronisation point that means anything.
	cacheMu sync.RWMutex

	// cacheStateMu guards cacheState, and it is deliberately *not* cacheMu.
	//
	// A reclaim holds cacheMu's write side for as long as the tools take, and
	// the whole point of the state is that a surface can ask what is happening
	// while that is going on. Guarding both with one lock would mean the Checks
	// page blocked on the operation it is trying to report.
	cacheStateMu sync.Mutex
	cacheState   CacheState
}

type BuildJob struct {
	App        *database.App
	Deployment *database.Deployment
	SkipClone  bool // true when source is already staged (e.g. ZIP upload)
	// FromBundle says this release's code arrived as an uploaded archive, as
	// opposed to being cloned or carried over from the current release.
	//
	// It is the *source of this release*, which is not the same question as
	// `IsUploadedSource(app.RepoURL)` — that one is about the app, and since a
	// laptop may push a bundle at a git-backed app the two differ. Nor is it
	// SkipClone, which is also true of a redeploy that reuses the release
	// already on disk. Two things read it, and both would otherwise tell a lie
	// about a working tree: the commit stamped on the deployment row, and the
	// commit the auto-deploy poller is told has shipped.
	FromBundle bool
	// Removed is what the unpacker took out of the uploaded archive before any
	// of this was queued (ADR §60), and it is here rather than on the upload's
	// own log for a mechanical reason: `uploadPhase.buildLog` is persisted only
	// by `fail()`, and on the success path the worker owns the log from its
	// first line — `deploy.BuildLog = buildLog.String()` overwrites whatever
	// was there. Riding on the job is what makes the report survive a *working*
	// deploy, which is the only deploy anybody reads it on.
	Removed []RemovedEntry
	// SourceDeployID names an earlier successful deployment whose release
	// directory is started exactly as built — no clone, no build. Set for
	// rollbacks; the Deployment row above records the rollback itself.
	SourceDeployID string
	// Reseat makes this job a move: a rebuild of the code already running,
	// at whatever base path the app should live at now (BasePathFor). The
	// source is the live release's tree for every app — nothing is cloned, so
	// a git app ships exactly the commit it was serving rather than whatever
	// was pushed to its branch since, and the source policy, which governs
	// fetches, has nothing to judge. The Deployment row carries the live
	// commit; the poller is never advanced.
	//
	// It rebuilds rather than restarting because Next.js needs a build and the
	// job does not branch on framework: one behaviour to explain, at the cost
	// of a build for the frameworks that read their path at start.
	Reseat bool
	// reseatFrom is the deployment whose tree a Reseat copies; "" is the live
	// release. Set only by a rollback whose target was built for another base
	// path (see execute), which is rebuilt rather than started as built.
	reseatFrom string
	// settleAs is the status a move leaves the app in once its release has
	// flipped: "stopped" or "sleeping" for an app that was, "" for every other
	// job. An administrator moving an address did not ask for anything to run,
	// so a stopped app is rebuilt and stays stopped — the build still starts
	// the release to prove it answers, and stops it after the flip.
	settleAs string
	// enqueuedAt is stamped by Enqueue and read once, by the worker that takes
	// the job, to record how long it waited. Unexported because it is nobody
	// else's business: a caller that set it would be reporting a queue wait
	// that did not happen.
	enqueuedAt time.Time
}

// stampsCommit reports whether the release this job produced is a checkout
// whose HEAD honestly describes it.
//
// Only a bundle is not. It is a *working tree*: it may be dirty, on a different
// branch, or rebuilt from a stash, and the `.git` directory it happens to carry
// names a commit that is not what is in these files. A row claiming to be
// a91f3c2 when it is a91f3c2 plus four uncommitted files is a lie in the one
// place people go to answer "what is running".
//
// Nor is a Reseat's tree: it is a copy of a release, which may carry no .git
// at all, and its commit is the live deployment's, stamped when it is queued.
func (j *BuildJob) stampsCommit() bool { return !j.FromBundle && !j.Reseat }

// advancesPoller reports whether this deploy should tell the auto-deploy poller
// that its commit is already live — the write that stops the next poll
// rebuilding what was just deployed.
//
// Four things have to hold, and each excludes a different way of being wrong:
//
//   - Not a bundle. Marking a commit as shipped when what shipped was a
//     laptop's working tree would make auto-deploy skip the real one.
//   - The app has a remote at all; an uploaded app has no poller.
//   - There is a commit. Nothing to record otherwise.
//   - Not a rollback. Rewinding last_seen_sha to the old commit would make the
//     poller see the tip as new and rebuild it, undoing the promote.
//   - Not a move, for the same reason: it rebuilt the commit already running,
//     which may be behind the tip the poller has already seen.
func (j *BuildJob) advancesPoller() bool {
	return !j.FromBundle &&
		!IsUploadedSource(j.App.RepoURL) &&
		j.Deployment.CommitSHA != "" &&
		j.SourceDeployID == "" &&
		!j.Reseat
}

// buildPoolFloor is the smallest pool the dial has room to move inside.
//
// Most installs never touch MAX_CONCURRENT_BUILDS, so without a floor the pool
// would be 1 on almost every install and the setting could only ever say 1 —
// a control that is disabled everywhere reads as broken. Four is enough headroom
// to be worth having and small enough that nobody's box notices four idle
// goroutines.
const buildPoolFloor = 4

// PoolSize is how many workers a builder started with MAX_CONCURRENT_BUILDS=n
// runs, which is the ceiling the build-concurrency dial cannot exceed.
//
// Exported so the settings surface can name it: a field that cannot pass a
// number and does not say what that number is, is a field nobody can reason
// about. It is a function of the configuration rather than a method on the
// Builder, because the API asks it on an install whose builder it does not hold.
func PoolSize(maxConcurrent int) int {
	if maxConcurrent < buildPoolFloor {
		return buildPoolFloor
	}
	return maxConcurrent
}

func New(db *database.DB, rts *runtime.Runtimes, enc *crypto.Encryptor, gitCred *gitconn.Resolver, artifacts artifact.Store, opts Options) *Builder {
	if opts.MaxConcurrent < 1 {
		opts.MaxConcurrent = 1
	}
	// The pool the workers live in, which is the ceiling the dial cannot exceed.
	pool := PoolSize(opts.MaxConcurrent)

	b := &Builder{
		reconciled: make(chan struct{}),
		db:         db,
		rts:        rts,
		encryptor:  enc,
		gitCred:    gitCred,
		artifacts:  artifacts,
		opts:       opts,
		queue:      make(chan *BuildJob, 100),
		listeners:  make(map[string][]chan string),
		live:       make(map[string][]string),
		watchers:   make(map[string][]chan ProgressEvent),
	}

	b.pool = pool
	b.starts = make(chan struct{}, pool)
	for i := 0; i < pool; i++ {
		go b.worker()
	}

	// The build queue's depth, read on the metric export interval and never on
	// a request path. It is the channel's own length: the queue *is* the
	// channel, so there is no second count to drift out of step with it.
	telemetry.ObserveBuildQueue(func() int64 { return int64(len(b.queue)) })

	// The shared cache's size, measured on its own slow ticker and read from
	// what that left behind — never walked on the export interval. Started only
	// when a cache is configured, so a builder in a test walks nothing.
	// The release disk's soft watermark.
	if opts.AppsDir != "" {
		b.startDiskWatcher()
	}

	if opts.CacheDir != "" {
		b.startCacheMeasurer()
		telemetry.ObserveBuildCacheBytes(func() (int64, bool) {
			// Absent until the first walk finishes rather than zero: a fresh
			// install has not got an empty cache, it has an unmeasured one.
			st := b.CacheStatus()
			return st.Bytes, st.Measured()
		})
	}

	return b
}

// WithNarrator wires the owner-facing half of a repair's log. Set after
// construction rather than in it, like the backend's own narrator, so a caller
// that has no sink is not obliged to say so.
func (b *Builder) WithNarrator(n *applog.Sink) *Builder {
	b.narrator = n
	return b
}

func (b *Builder) Enqueue(job *BuildJob) {
	job.enqueuedAt = time.Now()
	b.queue <- job
}

// SubscribeLogs follows one build's log. backlog is every line the build has
// printed so far, and ch carries each line after it — taken under one lock, so
// no line is in both and none falls between them.
//
// A build's log is written to its row once, at the end, and until this answered
// a backlog a page that connected at minute four of a build showed what was
// printed after minute four and said so in a footnote (ADR §87). backlog is nil
// for a build that is not running in this process: its row is the whole log.
func (b *Builder) SubscribeLogs(deploymentID string) (backlog []string, ch chan string) {
	backlog, _, ch = b.SubscribeLogsTimed(deploymentID)
	return backlog, ch
}

// SubscribeLogsTimed is SubscribeLogs with the time each backlog entry was
// written, taken under the same lock. An entry may hold several lines; each is
// stamped with its entry's time.
func (b *Builder) SubscribeLogsTimed(deploymentID string) (backlog []string, at []time.Time, ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if lines, ok := b.live[deploymentID]; ok {
		backlog = append([]string(nil), lines...)
		at = append([]time.Time(nil), b.liveAt[deploymentID]...)
	}
	ch = make(chan string, 256)
	b.listeners[deploymentID] = append(b.listeners[deploymentID], ch)
	return backlog, at, ch
}

// LiveLogTimes is a running build's line times so far, encoded as the row
// stores them (EncodeLineTimes), and whether the build is running here.
func (b *Builder) LiveLogTimes(deploymentID string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	lines, ok := b.live[deploymentID]
	if !ok {
		return "", false
	}
	return EncodeLineTimes(lines, b.liveAt[deploymentID]), true
}

// LiveLog is a running build's log so far, and whether the build is running in
// this process. The read routes serve it in place of the row, which holds
// nothing until the build ends.
func (b *Builder) LiveLog(deploymentID string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	lines, ok := b.live[deploymentID]
	if !ok {
		return "", false
	}
	if len(lines) == 0 {
		return "", true
	}
	return strings.Join(lines, "\n") + "\n", true
}

func (b *Builder) UnsubscribeLogs(deploymentID string, ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	listeners := b.listeners[deploymentID]
	for i, l := range listeners {
		if l == ch {
			b.listeners[deploymentID] = append(listeners[:i], listeners[i+1:]...)
			break
		}
	}
	close(ch)
}

// broadcast records a line in the build's live log and sends it to every
// listener. "DONE" is framing for the socket, not a line, and the stored log
// never holds it either.
func (b *Builder) broadcast(deploymentID, line string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if lines, ok := b.live[deploymentID]; ok && line != "DONE" {
		b.live[deploymentID] = append(lines, line)
		if b.liveAt == nil {
			b.liveAt = make(map[string][]time.Time)
		}
		b.liveAt[deploymentID] = append(b.liveAt[deploymentID], time.Now())
	}
	for _, ch := range b.listeners[deploymentID] {
		select {
		case ch <- line:
		default:
		}
	}
}

func (b *Builder) worker() {
	for job := range b.queue {
		// The dial, read *before* the build rather than at construction: what an
		// administrator types is the number of builds that may start, and it
		// takes effect at the next dequeue. Lowering it below the number already
		// in flight interrupts nothing — those builds hold tokens they took
		// when the number was higher, and they keep them until they finish.
		b.acquireStart()
		b.runJob(job)
	}
}

// runJob is one build, with its start token handed back however it ends. The
// recover is the backstop of "one app cannot take the platform down": a panic
// inside execute has already failed its deploy (execute's own recover), and
// one that escapes before the deploy could be failed is logged here rather
// than ending the process and every other app's build with it.
func (b *Builder) runJob(job *BuildJob) {
	defer func() { <-b.starts }()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("builder: build of deployment %s panicked: %v\n%s", job.Deployment.ID, r, debug.Stack())
		}
	}()
	b.execute(job)
}

// acquireStart blocks until this build is allowed to begin.
//
// The semaphore is the pool's size and never changes; what changes is how many
// of its tokens the dial permits, so the wait is "are fewer than N in flight"
// rather than "is a token free". Polled rather than signalled because the number
// changes on a settings write that this package never hears about, and a
// half-second of latency at the head of a build that takes minutes is not a
// cost worth a notification channel for.
func (b *Builder) acquireStart() {
	for {
		allowed := b.envelope().MaxConcurrentBuilds
		if allowed < 1 {
			allowed = 1
		}
		if allowed > b.pool {
			allowed = b.pool
		}
		if len(b.starts) < allowed {
			select {
			case b.starts <- struct{}{}:
				return
			default:
			}
		}
		time.Sleep(buildStartPoll)
	}
}

// reportDeliveryTimeout bounds one scheduled render's whole mail-out (PB-8).
const reportDeliveryTimeout = 5 * time.Minute

// buildStartPoll is how often a waiting worker re-reads the dial.
const buildStartPoll = 500 * time.Millisecond

// execute runs one deployment. The app currently serving traffic is left
// untouched until a new release has been built, started on its own port, and
// answered a request — so a failed build costs nothing but the queue slot.
func (b *Builder) execute(job *BuildJob) {
	// The cache's read side, held for the whole build. Taken here rather than
	// around installDependencies because a build reads the cache from more
	// places than that one call — the dependency gate resolves through npm and
	// uv, and a notebook render installs into the release — and the span that
	// is safe to reclaim underneath is "no build at all", not "no install".
	b.cacheMu.RLock()
	defer b.cacheMu.RUnlock()

	app := job.App
	deploy := job.Deployment
	// A build can outlive its app: deleted while queued or building, the build
	// went on to write a release and start a workload under a directory the
	// delete had already cleared (webhook-inspector on trytoft, 2026-10-23).
	// Whatever path it leaves by, a build whose app is gone takes its files with
	// it.
	defer b.dropIfDeleted(app.ID)

	// The build's span tree and its phase timings. Deferred rather than closed
	// at the bottom: fail() is followed by `return` at roughly thirty sites, and
	// a close at the end would be stepped over by every one of them.
	trace := startBuildTrace(app, deploy, time.Since(job.enqueuedAt), job.SourceDeployID != "")
	outcome := telemetry.OutcomeFailed
	defer func() { trace.finish(outcome) }()

	// The same phases, recorded durably for the people watching and streamed to
	// anything watching live. Continues the row rather than starting fresh: an
	// uploaded deploy arrives here with two steps already behind it. Deferred
	// for the same reason trace.finish is — see StepLog.Close.
	steps := NewStepLog(b.db, b, deploy)
	defer steps.Close()

	// The live copy opens before the first line and closes after the last —
	// after the row is written, so a reader always finds the log in one of the
	// two places.
	b.mu.Lock()
	if b.live == nil {
		b.live = make(map[string][]string)
	}
	if b.liveAt == nil {
		b.liveAt = make(map[string][]time.Time)
	}
	b.live[deploy.ID] = []string{}
	b.liveAt[deploy.ID] = []time.Time{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.live, deploy.ID)
		delete(b.liveAt, deploy.ID)
		b.mu.Unlock()
	}()
	// saveLineTimes stores the times beside the text the row has just been
	// given. Best effort: a log without times is the log every build before
	// this had.
	saveLineTimes := func() {
		if enc, ok := b.LiveLogTimes(deploy.ID); ok {
			if err := b.db.SetDeploymentLogTimes(deploy.ID, enc); err != nil {
				log.Printf("Warning: recording line times of deployment %s: %v", deploy.ID, err)
			}
		}
	}

	buildLog := &logFunnel{b: b, id: deploy.ID}
	appendLog := buildLog.add

	// What the upload no longer contains, before anything is said about what it
	// does. The platform changed what somebody shipped, and the first lines of
	// the log they will read is where that belongs — ahead of the fetch step,
	// which is where the archive becomes the release.
	for _, line := range RemovedBlock(job.Removed) {
		appendLog(line)
	}

	// State that must be unwound if the deploy fails partway. rt is resolved
	// below; until then there is nothing started to unwind.
	var newProcName string
	var rt runtime.ServiceRuntime
	// The app's status before the build began — read again where it becomes
	// building — so a failure at any step puts a sleeping, stopped or crashed
	// release back as it was.
	statusBefore := app.Status
	frameworkBefore := app.Framework

	// step names the phase the build is in, and is the event fingerprint when
	// it fails. A variable moved along the happy path rather than an argument
	// at ~30 fail() sites: it cannot be forgotten at a new one, and the value
	// it carries is exactly what makes two failures "the same problem" — a
	// repo that will not clone and a test suite that will not pass are
	// different problems even though both are "the deploy failed".
	step := StepSetup
	steps.Enter(step)

	// advance moves the build to its next step in all four places it is
	// recorded: the local variable that fingerprints a failure, the span tree,
	// the deployment row the UI reads, and the progress stream a CLI watches
	// (the last two both through StepLog). One call site, so a new step cannot
	// be added to some of the four.
	advance := func(next BuildStep) BuildStep {
		steps.Enter(next)
		return trace.next(next)
	}

	// failWith ends the deploy, saying why in both halves: the paragraph the
	// build log and a person get, and the key a program branches on.
	//
	// `fail` derives that key from the step, which is the whole answer at every
	// step that has one way to fail. The handful that do not — `detect`, which
	// refuses three different things, and `fetch`, where a policy refusal and a
	// broken clone land together — call failWith and name it. A reason variable
	// moved along the happy path like `step` would be wrong here: the sites
	// that need to sharpen it are not all the sites that fail, so a value set
	// and then not used would leak into the next failure.
	// ownRelease is the release directory this build created, once it has —
	// never a rollback's, which is an earlier build's and still a version.
	var ownRelease string
	// pending and publishedKey are this build's own artifact, once it has
	// published one — never a rollback's, whose key names the source
	// deployment's artifact. A deploy that fails before the flip removes it
	// (PB-7).
	var pending *pendingPublish
	var publishedKey string
	// flipped is set once SetAppRelease has made the new release the serving
	// one. The panic backstop below unwinds through failWithCode, which used
	// to delete the new workload and its tree at whatever point the panic
	// happened — after the flip, that was the release it had just made live
	// (code review 2026-11-11, F-10).
	flipped := false
	// finishDeployment is the row's terminal write. Its error used to go
	// unread, so a log Postgres refused took the status and finished_at down
	// with it; a second try without the log lands those (PB-2).
	finishDeployment := func() {
		err := b.db.UpdateDeployment(deploy)
		if err == nil {
			return
		}
		log.Printf("Warning: recording the outcome of deployment %s: %v", deploy.ID, err)
		withLog := deploy.BuildLog
		deploy.BuildLog = "the build log could not be stored; the platform log has the reason\n"
		if err := b.db.UpdateDeployment(deploy); err != nil {
			log.Printf("Warning: recording the outcome of deployment %s without its log: %v", deploy.ID, err)
		}
		deploy.BuildLog = withLog
	}
	// failWithCode is failWith with the refusal's own code beside the reason
	// (storage_unknown under storage_refused), on the row and the frame alike.
	var failWithCode func(reason deploysteps.Reason, code, msg string)
	failWith := func(reason deploysteps.Reason, msg string) { failWithCode(reason, "", msg) }
	failWithCode = func(reason deploysteps.Reason, code, msg string) {
		msg = textutil.Clean(msg)
		appendLog(fmt.Sprintf("ERROR: %s", msg))
		if flipped {
			appendLog("    (the release had already been made live; it is left serving)")
		}
		if newProcName != "" && rt != nil && !flipped {
			// Remove the half-started release; the previous one keeps serving.
			// Delete is also what gives the backend's reservation back — the
			// builder no longer holds a port of its own to release.
			//
			// Never the serving workload, whatever this build thought it was
			// starting: a failed deploy does not take the app down (PB-1).
			if b.isServing(app.ID, newProcName) {
				appendLog("    (the release it was starting is the serving one; it is left running)")
			} else {
				_ = rt.Delete(newProcName)
			}
		}
		if ownRelease != "" && !flipped {
			// A failed build is not a version and can never be made current,
			// so its directory is disk and nothing else. Left behind, it took a
			// slot in the release sweep from a build that could be rolled back
			// to. The row and its log stay: they are the explanation.
			_ = os.RemoveAll(ownRelease)
		}
		if !flipped && (pending != nil || publishedKey != "") {
			b.discardPublished(pending, publishedKey)
		}

		// The reason before the status: a reader that sees `failed` — a CLI
		// polling the row, a reconnecting watcher — must find why beside it.
		// The other order left a window in which the row said failed with no
		// reason. The frame below is published after both for the same cause.
		if err := b.db.SetDeploymentFailure(deploy.ID, string(reason), code, msg); err != nil {
			log.Printf("Warning: recording the failure of deployment %s: %v", deploy.ID, err)
		}
		now := time.Now().UTC()
		deploy.Status = database.DeployStatusFailed
		deploy.BuildLog = buildLog.text()
		deploy.FinishedAt = &now
		finishDeployment()
		saveLineTimes()

		// If a previous release is still up, the app is still running. Note the
		// asymmetry this creates and why the event matters: an app with a live
		// release stays "running" through a failed deploy, so the app list shows
		// nothing wrong and only deployments.status records it.
		// From the row as it is now, not the snapshot the job was queued
		// with: a deploy admitted while another was building carried a
		// CurrentRelease of "" and wrote `failed` over the release the other
		// had just flipped, or `running` over a stop made since. And only
		// while the row still says `building` — if anything else has written
		// the status in between (a flip, a stop, the reaper), that word stands
		// (code review 2026-11-11, F-2).
		current := app.CurrentRelease
		if fresh, err := b.db.GetApp(app.ID); err == nil && fresh != nil {
			current = fresh.CurrentRelease
		}
		status := database.AppStatusFailed
		if current != "" {
			status = restoredStatus(statusBefore, frameworkBefore)
		}
		if !flipped {
			if _, err := b.db.UpdateAppStatusFrom(app.ID, database.AppStatusBuilding, status); err != nil {
				log.Printf("Warning: restoring %s to %s after a failed deploy: %v", app.Slug, status, err)
			}
		}

		// The terminal frame, carrying the step it died at and why — the whole
		// message, not its first line. A watcher stops here; without it a CLI
		// would sit on a stream whose last word was "building" until it timed
		// out.
		failure := NewFailureEvent(deploy.ID, step, reason, msg)
		failure.Code = code
		failure.Warnings = DecodeWarnings(deploy.Warnings)
		failure.WarningDetails = DecodeWarningDetails(deploy.Warnings)
		b.PublishEvent(failure)
		// And the socket's terminator, which only the success path sent: a
		// console watching a failed build held its handler, its hijacked
		// connection and its listener for the life of the process (code
		// review 2026-11-11, F-9).
		appendLog("DONE")

		b.recordDeployFailure(app, deploy, step, msg, buildLog.text())
	}

	// fail is failWith with the step's own default reason, which is what ~30
	// call sites want and what a new one gets without having to know this
	// exists.
	fail := func(msg string) {
		failWith(deploysteps.ReasonOf(string(step)), msg)
	}

	// A panic anywhere below — a parser handed content nobody anticipated —
	// fails this deploy as an internal error rather than the platform process.
	// Registered once failWith exists and before every later defer, so it runs
	// first and the row, the stream and the release unwind the way any other
	// failure unwinds them.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("builder: deployment %s panicked at %s: %v\n%s", deploy.ID, step, r, debug.Stack())
			failWith(deploysteps.ReasonInternal, fmt.Sprintf("internal error during %s; the platform log has the detail", step))
		}
	}()

	b.rebuildRollbackAcrossPaths(job, appendLog)

	// Step 0: decide where this release will run, and prove it can, before
	// anything is cloned or built.
	//
	// Mode is settled here rather than at the start because everything after it
	// depends on the answer: which backend starts the workload, whether the
	// app's storage can be attached at all, and what the deployment row will
	// say this release ran as forever after. Failing now costs a queue slot;
	// failing at the flip would cost a build.
	// fail() records the row, so neither early return writes one of its own.
	// Nothing has started yet either, which is exactly the property this
	// ordering buys.
	mode, mounts, err := b.resolveTarget(app, job, appendLog)
	if err != nil {
		fail(err.Error())
		return
	}
	deploy.ExecutionMode = string(mode)
	trace.placed(string(mode))

	rt, err = b.rts.For(mode)
	if err != nil {
		fail(err.Error())
		return
	}

	deploy.Status = database.DeployStatusBuilding
	if err := b.db.UpdateDeployment(deploy); err != nil {
		log.Printf("Warning: marking deployment %s building: %v", deploy.ID, err)
	}
	// What the app was before this build said `building`: a failed deploy
	// puts it back, since the release it leaves serving is the one that was
	// in this state. Read from the row, not the snapshot — a job that waited
	// in the queue, or was admitted beside a build, holds a status a flip, a
	// stop or the reaper has since made untrue (F-2). A row that already
	// says `building` is another deploy's; its own before-state is kept.
	//
	// Recorded on the row too (MarkAppBuilding), because this variable dies
	// with the process and the boot sweep that settles an interrupted build
	// must put a stopped or sleeping app back as it was (DB-5).
	statusBefore = app.Status
	if before, err := b.db.MarkAppBuilding(app.ID); err == nil && before != "" {
		statusBefore = before
	} else if err != nil {
		log.Printf("Warning: marking %s building: %v", app.Slug, err)
		b.db.UpdateAppStatus(app.ID, database.AppStatusBuilding)
	}
	// And the serving release's framework, which is what a restored status
	// is computed from: `app.Framework` is overwritten at detect with the
	// *new* tree's (F-6).
	frameworkBefore = app.Framework

	// Whatever populates the release directory below, its parents must be
	// traversable by the app account or the spawn fails with EACCES.
	if err := EnsureAppDirs(b.opts.AppsDir, app.ID); err != nil {
		failWith(deploysteps.ReasonInternal, fmt.Sprintf("creating app directory: %v", err))
		return
	}

	// Refused before anything is written, never discovered half way through a
	// dependency install: the disk past its hard watermark, or short of what
	// this app's last release needed.
	if history, err := b.db.ListDeployments(app.ID); err == nil {
		need := buildEstimate(history)
		if job.SourceDeployID != "" {
			need = 0 // a rollback's fetch is admitted by restoreRelease
		}
		if err := b.admitTree(need); err != nil {
			failWith(deploysteps.ReasonDiskFull, err.Error())
			return
		}
	}

	releaseDir := ReleaseDir(b.opts.AppsDir, app.ID, deploy.ID)
	// What the dependency gates leave for later steps of this deploy, gone with
	// it however it ends.
	defer b.lockClears.Delete(deploy.ID)
	defer b.rPlatform.Delete(filepath.Clean(releaseDir))
	// clonedSHA is the commit of a clone whose folder became the release, read
	// before the move because the folder carries no .git of its own.
	var clonedSHA string
	// clonedTree is that folder's git tree id at clonedSHA, recorded beside it
	// when the build succeeds so the poller can tell a push that left the
	// folder alone (ADR §112).
	var clonedTree string
	if job.SourceDeployID != "" {
		// Rollback: start an earlier release directory exactly as it was
		// built. Nothing is fetched and nothing is rebuilt; the new
		// deployment row only records the rollback.
		releaseDir = ReleaseDir(b.opts.AppsDir, app.ID, job.SourceDeployID)
		if !fileExists(releaseDir) {
			// The directory is gone, but the artifact may not be — which is
			// the whole point of publishing releases. Without this, every
			// rollback target would be unreachable on a topology where the
			// apps directory does not survive a task replacement.
			if err := b.restoreAdmitted(app.ID, job.SourceDeployID, releaseDir, appendLog); err != nil {
				// Still StepSetup, whose default reason is about placement.
				// A rollback target that has been pruned is the code not being
				// there, which is what a developer has to act on.
				if errors.Is(err, ErrDiskFull) {
					failWith(deploysteps.ReasonDiskFull, err.Error())
				} else if missingReleaseErr(err) {
					failWith(deploysteps.ReasonSourceUnavailable,
						"that release no longer exists (pruned) — run a fresh deploy instead")
				} else {
					failWith(deploysteps.ReasonSourceUnavailable,
						fmt.Sprintf("fetching that release: %v", err))
				}
				return
			}
		}
		appendLog(fmt.Sprintf("==> Rolling back to release %s", job.SourceDeployID))
	} else if err := os.MkdirAll(releaseDir, 0700); err != nil {
		failWith(deploysteps.ReasonInternal, fmt.Sprintf("creating release directory: %v", err))
		return
	} else {
		ownRelease = releaseDir
	}

	// Step 1: Populate the release directory.
	//
	// usedStaging records that this build's source came from the upload staging
	// tree, so the success path knows whether it is entitled to clear it. A
	// rollback that runs while a fresh upload is waiting must not throw the
	// upload away.
	usedStaging := false
	step = advance(StepFetch)
	if job.SourceDeployID != "" {
		// Already populated — it is the old release.
	} else if job.Reseat {
		// A move: the code already running, or the rollback target's, copied
		// rather than fetched (see BuildJob.Reseat).
		from := job.reseatFrom
		if from == "" {
			from = LiveReleaseID(b.opts.AppsDir, app)
		}
		appendLog("==> Moving the running code to a new address — rebuilding the current release's source")
		var err error
		if from != "" {
			err = b.WithRelease(app.ID, from, func(dir string) error { return copyTree(dir, releaseDir) })
		} else {
			err = copyTree(CurrentLink(b.opts.AppsDir, app.ID), releaseDir)
		}
		if errors.Is(err, ErrReleaseGone) {
			fail("The release being moved no longer exists. Run a fresh deploy instead.")
			return
		}
		if err != nil {
			failWith(deploysteps.ReasonInternal, fmt.Sprintf("copying the release being moved: %v", err))
			return
		}
		// A rollback rebuilt: the copy is made, so the row stops naming a
		// source and is the owner of the tree it is about to build.
		if job.reseatFrom != "" {
			deploy.SourceDeployID = ""
			if err := b.db.ClearDeploymentSource(deploy.ID); err != nil {
				log.Printf("Warning: clearing the source of deployment %s: %v", deploy.ID, err)
			}
		}
	} else if job.SkipClone {
		staging := UploadStagingDir(b.opts.AppsDir, app.ID)
		switch {
		case fileExists(staging):
			appendLog("==> Source uploaded — staging release")
			if err := stageUploadedSource(staging, releaseDir); err != nil {
				failWith(deploysteps.ReasonInternal, fmt.Sprintf("staging uploaded source: %v", err))
				return
			}
			usedStaging = true
		case app.CurrentRelease != "":
			// A redeploy of an uploaded app: there is no remote to clone, so
			// the previous release's source is the source of truth.
			appendLog("==> Reusing source from the current release")
			// From the store when the disk no longer has it: an app that has
			// not started since the platform's disk was emptied has no
			// `current`, and its release is still in the store.
			var err error
			if liveID := LiveReleaseID(b.opts.AppsDir, app); liveID != "" {
				err = b.WithRelease(app.ID, liveID, func(dir string) error { return copyTree(dir, releaseDir) })
			} else {
				err = copyTree(CurrentLink(b.opts.AppsDir, app.ID), releaseDir)
			}
			if errors.Is(err, ErrReleaseGone) {
				fail("The current release's source no longer exists. Upload the app archive again to redeploy.")
				return
			}
			if err != nil {
				failWith(deploysteps.ReasonInternal,
					fmt.Sprintf("copying source from the current release: %v", err))
				return
			}
		default:
			fail("No source available. Upload the app archive again to redeploy.")
			return
		}
	} else {
		// Blocked, not unavailable: this install will not fetch from that
		// address, which is a decision the developer cannot fix by pushing.
		if err := ValidateRepoURL(app.RepoURL); err != nil {
			failWith(deploysteps.ReasonSourceBlocked, err.Error())
			return
		}
		// The source policy, immediately before the clone.
		//
		// Not redundant with the create route that already checked it. The
		// policy can change between an app's creation and any later build, a
		// build can sit in the queue for minutes, and this is the last place
		// where the decision is still cheap. It is also the point that protects
		// paths added later by someone who has not read the design: the gallery
		// alone added three ways an app is created, and none of them goes
		// through POST /apps.
		//
		// Neither of the two branches above reaches this: a rollback republishes
		// a release already on disk, vetted under the policy in force when it
		// was built, and an upload has no remote at all.
		if why := b.sourceRefusal(app); why != "" {
			failWith(deploysteps.ReasonSourceBlocked, why)
			return
		}
		if err := ValidateBranch(app.Branch); err != nil {
			failWith(deploysteps.ReasonSourceBlocked, err.Error())
			return
		}
		if err := ValidateSubdir(app.RepoSubdir); err != nil {
			failWith(deploysteps.ReasonSourceBlocked, err.Error())
			return
		}
		// One resolver, and this is one of its two call sites. The build used
		// to swallow a credential it could not read into "decrypting git
		// credential: cipher: message authentication failed", which is a
		// sentence for the person who wrote the platform and not for the person
		// whose deploy just stopped. The poller already knew better; now both
		// inherit the same words from the same place.
		cred, err := b.gitCred.CloneCredential(context.Background(), app)
		if err != nil {
			var unreadable *gitconn.Unreadable
			if errors.As(err, &unreadable) {
				fail(unreadable.Summary)
			} else {
				fail(fmt.Sprintf("reading the git credential: %v", err))
			}
			return
		}
		// An app built from a folder of the repository is cloned beside its
		// release and only the folder moves in, so everything after this —
		// detection, the manifest, the build, the published artifact — reads
		// the folder as the app's root and never learns the difference. The
		// commit is read from the clone first, because the folder has no .git.
		cloneDir := releaseDir
		if app.RepoSubdir != "" {
			cloneDir = releaseDir + ".clone"
			defer os.RemoveAll(cloneDir)
			appendLog(fmt.Sprintf("==> Cloning repository (building the folder %s)...", app.RepoSubdir))
		} else {
			appendLog("==> Cloning repository...")
		}
		if err := b.gitClone(deploy.ID, app.RepoURL, app.Branch, cloneDir, cred.Token, appendLog); err != nil {
			// runCommand carries git's own stderr in the error, so the same
			// classification the poller uses works here — and the owner is told
			// "your token expired" rather than "exit status 128". The credential
			// travels whole rather than as its token, because the sentence
			// depends on where it came from.
			class := ClassifyGitError(err.Error())
			if class.NeedsAttention() {
				fail(class.Explain(app.RepoURL, app.Branch, cred) + "\n" + err.Error())
			} else {
				fail(fmt.Sprintf("git clone failed: %v", err))
			}
			return
		}
		if app.RepoSubdir != "" {
			if sha, tree, err := cloneSubdirTree(context.Background(), cloneDir, app.RepoSubdir); err == nil {
				clonedSHA, clonedTree = sha, tree
			}
			if err := PlaceSubdir(cloneDir, app.RepoSubdir, releaseDir); err != nil {
				fail(fmt.Sprintf("%v at %s on %s. Check the app's folder setting, or the branch it tracks.", err, app.RepoURL, app.Branch))
				return
			}
			os.RemoveAll(cloneDir)
		}
	}

	// No link out of the release survives the fetch (dropEscapingLinks): what
	// runs before the handover runs as the platform. An upload carries no
	// links; a clone can. A rollback restores a tree that went through this.
	if job.SourceDeployID == "" {
		dropped, err := dropEscapingLinks(releaseDir)
		if err != nil {
			fail(fmt.Sprintf("checking the release for links out of it: %v", err))
			return
		}
		if len(dropped) > 0 {
			appendLog(fmt.Sprintf("    Removed %d link(s) that point outside the repository or nowhere: %s",
				len(dropped), strings.Join(firstN(dropped, 10), ", ")))
		}
	}

	// The CLI strips `.git/` from a bundle before it sends anything, so this
	// gate is the belt to that suspenders — the CLI is not the only thing that
	// can POST an archive, and one built by hand can contain whatever its
	// author put in it.
	if job.stampsCommit() {
		if clonedSHA != "" {
			deploy.CommitSHA = clonedSHA
		} else if sha, err := b.getCommitSHA(releaseDir); err == nil {
			deploy.CommitSHA = sha
		}
	}
	// The commit on the row now, not at the final write: the poller reads
	// in-flight rows to tell that this build already covers a tip, and a
	// building row with no commit reads as "not cloned yet" until it ends
	// (security review 2026-11-14, DG-13). And what last_seen_sha said at the
	// clone, so the finishing write never rewinds a claim made after it.
	seenAtClone, seenKnown := "", false
	if deploy.CommitSHA != "" {
		if err := b.db.SetDeploymentCommit(deploy.ID, deploy.CommitSHA); err != nil {
			log.Printf("Warning: recording %s's cloned commit: %v", app.Slug, err)
		}
	}
	if job.advancesPoller() {
		if cur, err := b.db.GetApp(app.ID); err == nil && cur != nil {
			seenAtClone, seenKnown = cur.LastSeenSHA, true
		}
	}

	// A Posit Connect bundle declares its packages in `manifest.json`, which
	// nothing downstream reads — so it is translated into the `renv.lock` all
	// three readers already ask for (rmanifest.go).
	//
	// **Here, and not in the R branch of installDependencies**, because the
	// scan below is the next thing to run and it resolves R from the same file:
	// converting later would install one set of packages and record another,
	// and the inventory would say `not_enumerable` for a release whose contents
	// the platform knew exactly. Nothing about it is framework-specific — the
	// file is the fact, which is the judgement `deps.Resolve` already makes
	// about `renv.lock` itself.
	//
	// Never fatal. A bundle that could not be translated deploys the way it did
	// before this existed: installing nothing, and saying so twice.
	if job.SourceDeployID == "" {
		if report, err := adoptConnectManifest(releaseDir, b.envelope().RRepos); err != nil {
			appendLog(fmt.Sprintf("    Warning: %v", err))
		} else if report != nil {
			report.report(appendLog)
		}
	}

	// Step 2: Judge what this release wants to install, before anything is
	// built for it.
	//
	// The gate used to run inside the build step, immediately before `npm ci`.
	// It moved here when a deploy gained its phases, and the move is safe
	// because the gate reads lockfiles and manifests off the release directory
	// — it has never needed the framework, the base path or the environment,
	// all of which the steps between then and now produce. The environment it
	// does not see is also the one the build does not get: an app's variable
	// that would configure an installer is kept out of buildEnvironment
	// (toolchain.IsInstallerConfig), so the two resolve alike. What it buys is that
	// a refusal is a *scan* failure rather than a build failure, in its own
	// phase, and that no configuration work is done for code that will not be
	// allowed to install.
	//
	// A rollback carries the restored release's inventory and verdict rather
	// than re-resolving or re-judging, and is never blocked — the same
	// asymmetry the version cap has, and for the same reason.
	step = advance(StepScan)
	if job.SourceDeployID != "" {
		b.carryDependencyRecord(job.SourceDeployID, deploy, appendLog)
	} else if why := b.dependencyGate(app, deploy, releaseDir, appendLog); why != "" {
		fail(why)
		return
	}

	// Step 3: Detect framework
	step = advance(StepDetect)
	appendLog("==> Detecting framework...")
	result := DetectFramework(releaseDir)
	if result.Framework == FrameworkUnsupported {
		fail(UnsupportedMessage(releaseDir))
		return
	}
	// A release that claimed one of the notebook frameworks and cannot be
	// served as one is refused here, before the framework is written to the app
	// row and before anything is built for it. Detection keeps the framework
	// and drops the entrypoint in that case — its only other vocabulary is
	// "unsupported", which is the wrong word for a repository holding two
	// notebooks — so the reason is asked for again here, where a deploy can
	// carry it to the person who wrote the file.
	if _, err := ResolveNotebook(releaseDir); err != nil {
		var ne NotebookError
		if errors.As(err, &ne) && ne.Field != "" {
			failWithCode(deploysteps.ReasonManifestInvalid, "invalid_notebook_"+ne.Field, err.Error())
			return
		}
		fail(err.Error())
		return
	}
	appendLog(fmt.Sprintf("    Detected: %s", result.Framework))
	trace.detected(string(result.Framework))

	// Used from here on by this build, and written to the app row at the
	// flip (SetAppRelease) and not before: the gateway routes the serving
	// release on the row's framework (prefix stripping, trailing slash,
	// serves-during-build), so writing the new detection at detect sent the
	// still-serving release /apps/{slug}/… unstripped for the length of the
	// build — and for good when the build failed, since nothing restored it
	// (code review 2026-11-11, F-6). A workload of this release reads its
	// detection off the deployment row, written next.
	app.Framework = string(result.Framework)
	app.Entrypoint = result.Entrypoint
	app.StartCmd = result.StartCmd
	// And on the deployment, which is what a workload of this release reads
	// when it starts (ADR §121's rule one table over): the app row describes
	// the newest build, not the serving one, from this line until the flip —
	// and for good after a failed build. A rollback writes the source too, so
	// a release built before the columns existed is described from now on;
	// the directory is the source's, so the answer is the same.
	b.db.SetDeploymentDetection(deploy.ID, app.Framework, app.Entrypoint, app.StartCmd)
	if job.SourceDeployID != "" {
		b.db.SetDeploymentDetection(job.SourceDeployID, app.Framework, app.Entrypoint, app.StartCmd)
	}

	// Where the app should live, read now rather than off the App this build
	// was queued with: a rename or a hostname switched while the build waited
	// is then built for, not left to the next move. Recorded on the row, which
	// is what serving reads once this release flips (ServingBasePath).
	// A rollback still a rollback here was built for this same path (the
	// check above), so the answer is the one its tree already has.
	basePath := BasePathFor(b.freshApp(app))
	appendLog(fmt.Sprintf("==> Base path: %s", describeBasePath(basePath)))
	if err := b.db.SetDeploymentBasePath(deploy.ID, basePath); err != nil {
		log.Printf("Warning: recording the base path of deployment %s: %v", deploy.ID, err)
	}

	// Declared jobs, from the repo's own toft.toml. A malformed manifest
	// fails the deploy rather than being ignored: shipping code whose jobs the
	// platform decided not to understand means the developer finds out at 3am
	// when the report does not arrive.
	// Judged now, recorded at the flip (applyManifestJobs): see checkManifest.
	declared, err := b.checkManifest(app, deploy, releaseDir, appendLog)
	if err != nil {
		reason, code := deploysteps.ReasonManifestInvalid, ""
		var refused *storageRefusal
		var me ManifestError
		switch {
		case errors.As(err, &refused):
			reason = deploysteps.ReasonStorageRefused
			if len(refused.problems) > 0 {
				code = refused.problems[0].Code
			}
		case errors.As(err, &me):
			code = me.Code
		}
		failWithCode(reason, code, err.Error())
		return
	}

	// The new release starts from the plan, not the table (ADR § Storage
	// somebody else can attach): the table stays the serving release's until
	// the flip, so a crash restart or a job run during this build still mounts
	// what is serving. Bindings and gates for what the plan adds are made now,
	// before the start that needs them; the refusal resolveTarget makes before
	// the clone is asked again of the planned mounts.
	// Whenever the file declares storage, not only when the plan writes: a
	// Keep-only plan took its mounts from the table as it was at clone time
	// and skipped the flip's re-diff, so a deploy of the same app that flipped
	// in between left this release's mounts and the table disagreeing (code
	// review 2026-11-11, B-8). The reconcile is a no-op when nothing differs.
	if plan := declared.plan; plan != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := b.storagePlanner.PrepareStoragePlan(ctx, app, plan)
		cancel()
		if err != nil {
			failWith(deploysteps.ReasonStorageRefused, "toft.toml [[storage]]: "+err.Error())
			return
		}
		if mode != runtime.ModeIsolated {
			if refused := b.rts.UnplaceableSharedMounts(plan.Mounts); len(refused) > 0 {
				failWith(deploysteps.ReasonStorageRefused, storageRequiresIsolation(refused).Error())
				return
			}
		}
		mounts = plan.Mounts
	}

	// Which language version this release is built against — refused here, in
	// the deploy phase, rather than at start. The build produces the venv and
	// node_modules on this host in *both* execution modes, so a version
	// decision made after it is a decision about an artifact that is already
	// committed to an interpreter (ADR §64).
	//
	// Before applyEnvDecls and before the build environment is assembled,
	// because a refusal should cost the clone and nothing more.
	resolvedRT, err := b.resolveRuntime(result.Framework, releaseDir, mode, appendLog)
	if err != nil {
		failWith(deploysteps.ReasonRuntimeVersion, err.Error())
		return
	}
	resolvedRT.report(appendLog)
	resolvedRT.record(b, deploy.ID, appendLog)
	if w, ok := nodeVersionUnread(releaseDir, resolvedRT); ok {
		appendLog("==> Warning: " + w.Message)
		b.addDeployWarningDetails(deploy, []Warning{w})
	}
	if w, ok := goToolchainFetched(resolvedRT); ok {
		appendLog("==> Warning: " + w.Message)
		b.addDeployWarningDetails(deploy, []Warning{w})
	}
	// The check said this of a go.work `use` directory the tree lacks; the
	// deploy said nothing and the build then failed to load it (BB-007).
	if result.Framework == FrameworkGo || result.Framework == FrameworkBatchGo {
		if ws := goWorkDirsMissing(releaseDir); len(ws) > 0 {
			for _, w := range ws {
				appendLog("==> Warning: " + w.Message)
			}
			b.addDeployWarningDetails(deploy, ws)
		}
	}

	// The variables toft.toml [env] declares, seeded before the env read below
	// so the very first build already exports them, and at configure rather than
	// the flip because the release's process starts before the flip.
	if err := b.applyEnvDecls(app, releaseDir, appendLog); err != nil {
		failWith(deploysteps.ReasonInternal, fmt.Sprintf("seeding toft.toml [env]: %v", err))
		return
	}

	// Step 4: Assemble the build environment. App secrets are passed through
	// the process environment — never written to a file inside the app tree,
	// where the app itself (or its users) could read them back.
	step = advance(StepConfigure)
	envVars, err := b.getDecryptedEnvVars(app.ID)
	if err != nil {
		fail(fmt.Sprintf("decrypting environment variables: %v", err))
		return
	}
	buildEnv := b.buildEnvironment(envVars, basePath)
	if dropped := toolchain.InstallerConfigNames(envVars); len(dropped) > 0 {
		appendLog(fmt.Sprintf("    Not passed to the build: %s — an installer's configuration is the install's, not an app's (the app's process still receives it)",
			strings.Join(dropped, ", ")))
	}
	// Node and Go take their interpreter by PATH, because npm and go have no
	// equivalent of uv's --python and the tool beside the interpreter is the one
	// that has to be used. Python takes it by argument, below.
	resolvedRT.apply(buildEnv)

	// Step 5: Framework-specific build. Skipped on rollback: the release
	// directory already holds that build's artifacts.
	step = advance(StepBuild)
	if job.SourceDeployID == "" {
		// The release becomes its app account's here, and not before: every
		// step above read a tree only root could write, and every step below
		// runs the repository's code — as that account (identity.go).
		if _, done, err := b.beginBuildAs(app.Slug, releaseDir); err != nil {
			fail(err.Error())
			return
		} else {
			defer done()
		}
		// One document, both voices: on a deploy the stage marks and the
		// commands' output go to the same build log, which is what the owner
		// already reads on the Deployments tab.
		if err := b.installDependencies(deploy.ID, result, releaseDir, basePath,
			envVars, buildEnv, resolvedRT, b.buildTimeout(app), sameBoth(appendLog)); err != nil {
			fail(err.Error())
			return
		}
		// Executing the notebook, which is a step of the *deploy* and not of
		// installDependencies — deliberately, because that function's other
		// caller is Repair.
		//
		// A repair rebuilds a venv whose interpreter moved underneath it. The
		// rendered document is a file: it dangles from nothing, it is already
		// in this release, and re-executing every cell to produce it again
		// would be a re-render outside a deploy — which is the one thing the
		// automation rule refuses, because a start replaces the tree and the
		// new output would vanish at the next wake. So a repair repairs the
		// venv and the app keeps serving the document it was deployed with.
		if err := b.renderIfNotebook(deploy.ID, result, releaseDir,
			envVars, buildEnv, sameBoth(appendLog)); err != nil {
			fail(err.Error())
			return
		}
		// R's packages judged a second time, as installed, before anything is
		// published: the library is the only record of what Toft put there
		// and of what renv pulled in that the lockfile did not name.
		if LanguageOf(result.Framework) == toolchain.R {
			if why := b.rInstalledGate(app, deploy, releaseDir, appendLog); why != "" {
				failWith(deploysteps.ReasonDependencyBlocked, why)
				return
			}
		}
	}
	// Step 6: Publish the built tree, so the release outlives the machine that
	// built it. On rollback nothing is published — the source deployment's
	// artifact already exists and is what this release *is*.
	//
	// It finishes before traffic moves, so that a store outage costs a failed
	// deploy with the previous release still serving, rather than a running app
	// whose code will vanish with the next task replacement. In shared mode the
	// upload runs *while* the release starts (ADR §123): the start reads the
	// tree on disk, never the store. An isolated pod fetches its release from
	// the store as it starts, so there the upload has to be done first.
	step = advance(StepPublish)
	// An app deleted while this built is noticed before its release is
	// published: noticed only after, the upload landed after the delete's
	// Expire had run and the object stayed in the bucket for good, the app id
	// being its only handle (code review 2026-11-11, F-8).
	if fresh, err := b.db.GetApp(app.ID); err == nil && fresh == nil {
		log.Printf("build %s: %s was deleted while it built; not published", deploy.ID, app.Slug)
		appendLog("DONE")
		return
	}
	releaseRef := runtime.LocalRelease(releaseDir)
	artifactKey := ""
	if job.SourceDeployID == "" {
		if mode == runtime.ModeShared {
			p, err := b.publishBehind(app.ID, deploy.ID, releaseDir, appendLog)
			if err != nil {
				fail(fmt.Sprintf("publishing the release: %v", err))
				return
			}
			pending = p
		}
		if pending != nil {
			releaseRef = runtime.ArtifactRelease(pending.key)
		} else {
			ref, desc, err := b.publishRelease(app.ID, deploy.ID, releaseDir, appendLog)
			if err != nil {
				fail(fmt.Sprintf("publishing the release: %v", err))
				return
			}
			releaseRef, artifactKey = ref, desc.Key
			publishedKey = desc.Key
			b.recordReleaseSizes(app.ID, deploy.ID, desc)
		}
	} else if runtime.ParseReleaseRef(app.ReleaseRef).Kind == runtime.ReleaseArtifact {
		// The install serves releases from the store, so point the reference at
		// the source deployment's artifact. The local case needs no rewrite:
		// releaseRef already names the source's release directory. Adopting the
		// app's stored ref here instead would restart the *outgoing* release
		// under a new name — the promote would silently not happen, and every
		// protection check downstream would guard the wrong directory.
		releaseRef = runtime.ArtifactRelease(artifact.ReleaseKey(app.ID, job.SourceDeployID))
	}

	// The release a promote serves — and therefore the process name, the
	// current symlink, and what pruning must keep — belongs to the source
	// deployment, not to the transient row tracking the promote.
	releaseDeployID := deploy.ID
	if job.SourceDeployID != "" {
		releaseDeployID = job.SourceDeployID
	}

	// Step 7: Start the new release alongside the old one. The backend picks
	// the address and reports it back; the builder neither allocates a port
	// nor knows what the app is bound to.
	// Named after the deployment that owns the release being started, so after
	// a promote the app's current_release points at the source row and the
	// history shows that build as current, in its own place in the timeline.
	//
	// A rollback takes the app's flip lock here, before its start, and holds
	// it to the end: two rollbacks to one release share this name, so the
	// second, run after the first had flipped, restarted the serving workload
	// in place and deleted it if anything after failed (PB-1). Under the lock
	// the row says whether the release is already serving, and then there is
	// nothing to do. A fresh deploy's name is its own and cannot be serving.
	rollbackHoldsFlip := false
	if job.SourceDeployID != "" {
		unlock := b.flipLock(app.ID)
		defer unlock()
		rollbackHoldsFlip = true
		if b.isServing(app.ID, ReleaseProcessName(app.Slug, releaseDeployID)) {
			appendLog(fmt.Sprintf("==> Release %s is already the serving one; nothing to do", releaseDeployID))
			if _, err := b.db.DeleteDeployment(app.ID, deploy.ID); err != nil {
				log.Printf("Warning: removing transient promote row %s: %v", deploy.ID, err)
			}
			restored := restoredStatus(statusBefore, frameworkBefore)
			if _, err := b.db.UpdateAppStatusFrom(app.ID, database.AppStatusBuilding, restored); err != nil {
				log.Printf("Warning: restoring %s to %s after a no-op rollback: %v", app.Slug, restored, err)
			}
			appendLog("DONE")
			success := NewProgressEvent(deploy.ID, database.DeployStatusSuccess, step, "")
			success.Warnings = DecodeWarnings(deploy.Warnings)
			success.WarningDetails = DecodeWarningDetails(deploy.Warnings)
			b.PublishEvent(success)
			outcome = telemetry.OutcomeSucceeded
			return
		}
	}
	newProcName = ReleaseProcessName(app.Slug, releaseDeployID)

	// A batch app (ADR §107) serves nothing: there is no process to start and
	// nothing to wait on, so the flip is the whole of going live. The next run
	// of each job executes this release, and a run already queued executes the
	// one it was queued against.
	batch := result.Framework.IsBatch()

	// The one lock a deploy is admitted under, and the one it can lift: here,
	// before the release becomes current — a batch app's included, whose jobs
	// would run it — and not at the gate, because a deploy can still fail
	// between the two, and an app unlocked on its flagged release would be
	// served at the next wake.
	if job.SourceDeployID == "" {
		if fresh, err := b.db.GetApp(app.ID); err == nil && fresh.LockedByPolicy() {
			if why := b.liftDependencyLock(fresh, deploy, appendLog); why != "" {
				failWith(deploysteps.ReasonDependencyBlocked, why)
				return
			}
		}
	}

	var endpoint runtime.Endpoint
	if batch {
		appendLog("==> This is a batch app: it serves nothing, so nothing is started. " +
			"Its jobs run this release from now on.")
	} else {
		step = advance(StepStart)
		appendLog(fmt.Sprintf("==> Starting release %s (%s mode)...", newProcName, mode))
		// This workload's own outbound credential. Minted here, injected into the
		// process it was minted for, and never shown to a person.
		//
		// **Shared mode only.** An isolated workload is minted one when its pod
		// bootstraps, because that is the only moment the platform can hand a pod a
		// secret at all — minting a second one here would create a row nothing ever
		// receives, and the pod's own mint would supersede it seconds later.
		//
		// Failure to mint is deliberately not fatal: an app whose token could not be
		// written still serves traffic, it simply cannot call /api/v1/app/*. Refusing
		// to deploy over a capability the app may not even use would be the wrong
		// trade.
		var appToken string
		if mode == runtime.ModeShared && !bootstrapsIdentity(rt) {
			appToken, _ = b.mintAppToken(app, releaseDeployID, newProcName, appendLog)
		}

		// One read of the envelope for the whole description: a start carries the
		// configuration as it is now, and "now" has to be one instant rather than
		// three.
		env := b.envelope()
		spec := runtime.WorkloadSpec{
			AppID:         app.ID,
			Slug:          app.Slug,
			AppToken:      appToken,
			RoleKey:       b.roleKeyFor(app.ID, newProcName, mode),
			Name:          newProcName,
			Release:       releaseRef,
			Env:           envVars,
			Framework:     string(result.Framework),
			Entrypoint:    result.Entrypoint,
			StartCmd:      result.StartCmd,
			BasePath:      basePath,
			MemoryLimitMB: env.ResourcesForApp(app.MemoryLimitMB, app.CPULimitMillicores).MemoryMB.Value,
			Mounts:        mounts,

			CPURequestMillicores: env.AppCPURequestMillicores,
			CPULimitMillicores:   env.ResourcesForApp(app.MemoryLimitMB, app.CPULimitMillicores).CPUMillicores.Value,

			// Loopback is right for a process sharing Toft's network
			// namespace and wrong for a workload whose namespace *is* the
			// boundary. The invariant — unreachable without passing the gateway —
			// is the same either way; only what enforces it differs.
			BindAddress: bindAddressFor(mode),

			// execute holds cacheMu.RLock for the whole build, and the shared
			// backend's start calls Repair on this goroutine (PB-3).
			BuildHoldsCache: true,
		}
		// The lock is a precondition for starting a workload, not only for
		// admitting the request: this build may have been queued, or running,
		// when an administrator or the dependency sweep locked the app.
		fresh, gerr := b.db.GetApp(app.ID)
		if gerr == nil && fresh == nil {
			// Deleted while this build ran. There is no deployment row left to
			// fail and nobody to start a workload for; dropIfDeleted takes the
			// files.
			log.Printf("build %s: %s was deleted while it built; not started", deploy.ID, app.Slug)
			if pending != nil {
				// Let the upload land, so the deferred drop finds the object
				// and expires it rather than racing it (F-8).
				_, _ = pending.wait(appendLog)
			}
			appendLog("DONE")
			return
		}
		if gerr == nil && fresh.Locked() {
			failWith(deploysteps.ReasonSetupFailed, "this app was locked while the deploy was in progress, so it was not started")
			return
		}
		deployStart := time.Now()
		started, err := rt.Deploy(spec)
		trace.deployed(string(mode), job.SourceDeployID != "", time.Since(deployStart), err)
		if err != nil {
			fail(fmt.Sprintf("failed to start app: %v", err))
			return
		}
		endpoint = started

		// The superseded tokens are revoked at the flip, below — not here. A
		// release that started and then never became ready left the serving
		// process holding a revoked token: the old release kept answering while
		// every SDK call it made was invalid_app_token, until a restart (2026-11-03
		// evening drills).
		appendLog(fmt.Sprintf("    Listening on %s", endpoint.Addr()))

		// Step 8: Wait until it actually answers before sending traffic to it.
		step = advance(StepReady)
		appendLog("==> Waiting for the app to become ready...")
		if err := waitReady(endpoint, readinessTimeout, watchWorkload(rt, newProcName)); err != nil {
			msg := fmt.Sprintf("app did not become ready: %v", err)
			var exited *workloadExitedError
			if errors.As(err, &exited) && b.db != nil {
				if err := b.db.SetDeploymentFailureOutput(deploy.ID, encodeFailedStart(exited.output)); err != nil {
					log.Printf("Warning: recording the failed start's output of deployment %s: %v", deploy.ID, err)
				}
			}
			if mode == runtime.ModeIsolated && !errors.Is(err, errWorkloadExited) {
				// The workload may be perfectly healthy and simply unreachable from
				// here, which is a different problem with a different fix — and one
				// the gateway is about to have too, since it dials the same address.
				msg += "\n    The workload started, so this is either the app failing to listen or " +
					"Toft being unable to reach " + endpoint.Host + ". Toft must run " +
					"inside the cluster it places workloads into; from outside, a ClusterIP is not routable."
			}
			if exited != nil && exited.memory {
				failWith(deploysteps.ReasonOutOfMemory, msg)
				return
			}
			fail(msg)
			return
		}
		appendLog("    Ready.")
	}

	// The upload started before the release did has to have landed before
	// traffic moves: the release is not durable until it has.
	if pending != nil {
		desc, err := pending.wait(appendLog)
		if err != nil {
			step = StepPublish
			fail(fmt.Sprintf("publishing the release: %v", err))
			return
		}
		artifactKey = desc.Key
		b.recordReleaseSizes(app.ID, deploy.ID, desc)
	}

	// Step 9: Flip. From here the gateway resolves the new endpoint on its next
	// request; the old release is only stopped afterwards.
	step = advance(StepFlip)
	// **The release being replaced is the one serving now, read now.** `app`
	// was read when this deploy began, and a second deploy of the same app can
	// flip in between: two deploys queued seconds apart both named the first
	// release as "previous", the second's workload was never deleted, and on
	// trytoft its log stayed open on EFS until the app could not be deleted
	// (2026-10-28, E1b). Flips of one app are serialised and each reads the
	// serving release under the lock, so every replaced release is retired
	// exactly once.
	if rollbackHoldsFlip {
		b.refreshServing(app)
	} else {
		unlockFlip := b.servingBeforeFlip(app)
		defer unlockFlip()
	}
	oldProcName := app.CurrentRelease
	// Captured before it is overwritten: the previous release belongs to
	// whichever backend started it, which is not necessarily this one — a
	// promotion is exactly the deploy where the two differ.
	oldMode := app.RunningModeOrShared()

	// The lock, read again under the flip lock: checked once before the
	// start, a lock placed during readiness or the upload was undone by the
	// flip — handleLockApp stopped the old release, and the flip then recorded
	// the new workload as current and wrote running (code review 2026-11-11,
	// F-7). A batch app, never checked before its start, is covered here too.
	// Before the storage reconcile, which commits: a refusal after it left the
	// new toft.toml's storage under the old release (PB-4).
	if fresh, err := b.db.GetApp(app.ID); err == nil && fresh != nil && fresh.Locked() {
		failWith(deploysteps.ReasonSetupFailed, "this app was locked while the deploy was in progress. The new release has been retired; the app stays stopped under the lock")
		return
	}

	// And the mounts the new workload was *started* with — read from the table
	// or the plan minutes ago, before the clone — against the table as it is
	// now, under the flip lock. A console delegation revoked during the build
	// deleted its rows while the workload kept the mount, and nothing above
	// looked again when the plan had nothing to write (code review 2026-11-11,
	// B-1). Same retirement as a revoked plan: the old release keeps serving.
	//
	// Asked first *before* the reconcile, of every started mount the plan does
	// not itself add or change — those have no row until the reconcile writes
	// it — so a console detach during the build refuses while the table is
	// still the serving release's (PB-4). Asked again after it, of everything,
	// as the floor.
	detachedDuringBuild := func(gone []string) {
		failWithCode(deploysteps.ReasonStorageRevokedDuringDeploy, StorageRevokedDuringDeploy,
			fmt.Sprintf("storage %s was detached from this app while it was building. The new release was healthy and has been retired; the previous one is still serving",
				strings.Join(gone, ", ")))
	}
	plan := declared.plan
	if plan != nil {
		if recorded, err := b.db.AppMounts(app.ID); err != nil {
			failWith(deploysteps.ReasonInternal, fmt.Sprintf("reading the app's storage at the flip: %v", err))
			return
		} else if gone := mountsWithoutRowsUnlessPlanned(mounts, recorded, plan); len(gone) > 0 {
			detachedDuringBuild(gone)
			return
		}
	}

	// The release's storage, written before the release is recorded and under
	// the flip lock: a delegation revoked since the plan refuses here, the new
	// workload is retired the way a failed readiness retires it, and the old
	// release keeps serving with the table it had — the reconcile commits
	// nothing when it refuses. Not a warning, as jobs are — a workload holding
	// a mount no row records is a mapping no revocation can collect.
	//
	// Once it has written, the table is this deploy's toft.toml's, and nothing
	// reverts it: the exits below it are a database error, and say so (PB-4).
	storageWritten := false
	if plan != nil {
		if err := b.storagePlanner.ReconcileManifestStorage(app, plan, DeployerOf(deploy)); err != nil {
			reason, code := deploysteps.ReasonStorageRevokedDuringDeploy, StorageRevokedDuringDeploy
			msg := fmt.Sprintf("%v. The new release was healthy and has been retired; the previous one is still serving", err)
			if !errors.Is(err, database.ErrStorageRevokedDuringDeploy) {
				reason, code = deploysteps.ReasonStorageRefused, ""
				msg = fmt.Sprintf("recording toft.toml storage: %v. The new release has been retired; the previous one is still serving", err)
			}
			failWithCode(reason, code, msg)
			return
		}
		storageWritten = plan.Writes()
		appendLog("==> Storage recorded as toft.toml declares it.")
	}
	// afterStorage is what a failure past the reconcile adds: the previous
	// release serves, but the storage table already says what the new one's
	// toft.toml declares.
	afterStorage := func(msg string) string {
		if !storageWritten {
			return msg
		}
		return msg + ". The previous release is still serving, but the app's storage now reflects this deploy's toft.toml; a redeploy brings the two back in step"
	}
	if recorded, err := b.db.AppMounts(app.ID); err != nil {
		failWith(deploysteps.ReasonInternal, afterStorage(fmt.Sprintf("reading the app's storage at the flip: %v", err)))
		return
	} else if gone := mountsWithoutRows(mounts, recorded); len(gone) > 0 {
		detachedDuringBuild(gone)
		return
	}

	app.Port = database.Port(endpoint.Port)
	app.EndpointHost = endpoint.Host
	app.CurrentRelease = newProcName
	app.ReleaseRef = releaseRef.String()
	app.RunningMode = string(mode)
	app.ReleaseBasePath = &basePath
	liveStatus := database.LiveStatus(string(result.Framework))
	app.Status = liveStatus
	if err := b.db.SetAppRelease(app); err != nil {
		fail(afterStorage(fmt.Sprintf("recording new release: %v", err)))
		return
	}
	flipped = true
	// The new release is the one serving, so the tokens of the ones that are
	// not can go. Not at mint time — that would cut the outgoing process off
	// mid-request during a rolling replace — and not when the new workload
	// started either: readiness, the publish or the storage plan can still
	// fail after that, and then the release left serving is the old one, whose
	// token this would have revoked (2026-11-03 evening drills). A leaked token
	// still dies at the next deploy with nobody doing anything.
	//
	// Keyed on the workload, not on a token this function minted, because in
	// isolated mode it minted none — the pod did, at bootstrap. One rule for
	// every mode and for a batch app: keep the incoming workload's, revoke
	// everything else's.
	if err := b.db.RevokeAppTokensExceptWorkload(app.ID, newProcName); err != nil {
		log.Printf("Warning: revoking superseded app tokens for %s: %v", app.Slug, err)
	}
	// The flip reconciles: if the app's address moved again while this build
	// was past choosing its path, another move is queued — read under the flip
	// lock, so no change can fall between this check and the release it
	// compares against. No coordination with the handler that made the change.
	if MoveNeeded(b.freshApp(app)) {
		b.queueMoveAfterFlip(app.ID)
	}
	// Its own statement, because UpdateApp no longer carries status — see the
	// comment there. Kept *here* rather than left to the one at the end of the
	// flip so the moment the row starts saying `running` does not move: between
	// the two the new release is already the one being served, and a visitor
	// arriving in that window would get the building interstitial instead of
	// the app.
	//
	// Unless the row says `stopped`: the build wrote `building` when it began,
	// so a `stopped` here was written during this build — a person's Stop, or
	// a lock — and the new workload is stopped below rather than left serving
	// under it (AP-2). Not "only from building": an overlapping deploy's flip
	// may already have written the live status, and a batch release must
	// still move it to `ready`.
	stoppedDuringDeploy := false
	if moved, err := b.db.UpdateAppStatusUnless(app.ID, database.AppStatusStopped, liveStatus); err != nil {
		log.Printf("Warning: marking %s %s: %v", app.Slug, liveStatus, err)
	} else if !moved {
		stoppedDuringDeploy = true
	}
	// The release's jobs, now that the release is the one serving. Not a
	// failure of the deploy if it cannot be written: the new code is already
	// live, and refusing now would report a serving release as failed. The
	// previous definitions stay, and the deploy says so.
	//
	// A scheduled re-render is a deployment nobody made, so its flip keeps
	// what a person paused (keepPauses); a person's deploy re-applies
	// toft.toml, which says the job runs — including over a pause made while
	// that deploy was building.
	keepPauses := deploy.TriggeredBy == database.DeployTriggerRender
	if err := b.applyManifestJobs(app, declared.Jobs, keepPauses, appendLog); err != nil {
		appendLog("==> Warning: " + err.Error() + " — the jobs listed are still the previous release's")
		b.addDeployWarningDetails(deploy, []Warning{{Code: WarningJobsNotRecorded, Table: "jobs", Message: err.Error()}})
		log.Printf("Warning: %s: %v", app.Slug, err)
	}
	// And its `[restart]` and `[render]`, on the same terms.
	if err := b.applyManifestSchedules(app, deploy, declared, keepPauses, appendLog); err != nil {
		appendLog("==> Warning: " + err.Error() + " — the schedules are still the previous release's")
		b.addDeployWarningDetails(deploy, []Warning{{Code: WarningSchedulesNotRecorded, Message: err.Error()}})
		log.Printf("Warning: %s: %v", app.Slug, err)
	}
	// Same rule as every other start: what is serving is carrying today's
	// variables. After the flip rather than after readiness, because between
	// the two the *old* release is still the one being served — a deploy that
	// could not record its release would otherwise report an environment as
	// live on a workload no visitor reaches.
	if err := b.db.MarkEnvApplied(app.ID); err != nil {
		log.Printf("Warning: recording applied environment for %s: %v", app.Slug, err)
	}
	if artifactKey != "" {
		deploy.ArtifactKey = artifactKey
	}

	if err := linkCurrent(b.opts.AppsDir, app.ID, releaseDeployID); err != nil {
		log.Printf("Warning: updating current symlink for %s: %v", app.Slug, err)
	}

	// Step 10: Retire the previous release. Delete is what returns its address
	// to the backend's pool, so there is nothing further to release here.
	if oldProcName != "" && oldProcName != newProcName {
		appendLog(fmt.Sprintf("==> Stopping previous release %s...", oldProcName))
		if oldRT, err := b.rts.For(oldMode); err != nil {
			log.Printf("Warning: no %s backend to stop previous release %s: %v", oldMode, oldProcName, err)
		} else if err := oldRT.Delete(oldProcName); err != nil {
			log.Printf("Warning: stopping previous release %s: %v", oldProcName, err)
		}
	}
	// Step 11: version retention. The one sweep in the system that runs on a
	// deploy rather than a timer.
	b.sweepReleases(app, releaseDeployID)

	// The second live-status write, seconds after the first and after the
	// previous release was retired — on the same terms: a Stop or a lock made
	// between the two stands (DB-4). Written before the deployment is marked
	// done, so what is said here is in the log the row keeps.
	if !stoppedDuringDeploy {
		if moved, err := b.db.UpdateAppStatusUnless(app.ID, database.AppStatusStopped, liveStatus); err != nil {
			log.Printf("Warning: marking %s %s: %v", app.Slug, liveStatus, err)
		} else if !moved {
			stoppedDuringDeploy = true
		}
	}
	switch {
	case stoppedDuringDeploy:
		// The release stays recorded — it is what a Start brings back — and
		// the deploy still succeeded; only its workload is not left running.
		if err := rt.Stop(newProcName); err != nil && runtime.AbsenceReason(err) == "" {
			log.Printf("Warning: stopping %s, stopped during its deploy: %v", app.Slug, err)
		}
		appendLog("==> The app was stopped while this deploy was in progress; the new release is recorded and left stopped")
	case job.settleAs != "":
		if err := rt.Stop(newProcName); err != nil && runtime.AbsenceReason(err) == "" {
			log.Printf("Warning: returning moved %s to %s: %v", app.Slug, job.settleAs, err)
		} else {
			b.db.UpdateAppStatus(app.ID, job.settleAs)
			appendLog(fmt.Sprintf("==> The app was %s before the move, and is again", job.settleAs))
		}
	}

	if job.SourceDeployID != "" {
		// The row existed to track the promote while it ran and to keep the
		// log if it failed. Success is recorded by the source row being
		// current again; keeping a second success row would put the promote at
		// the top of the history and hand "current" to a row that owns no
		// storage. The durable who-promoted-what record is the audit row the
		// API wrote when it accepted the request.
		if _, err := b.db.DeleteDeployment(app.ID, deploy.ID); err != nil {
			log.Printf("Warning: removing transient promote row %s: %v", deploy.ID, err)
		}
	} else {
		now := time.Now().UTC()
		deploy.Status = database.DeployStatusSuccess
		deploy.BuildLog = buildLog.text()
		deploy.FinishedAt = &now
		finishDeployment()
		saveLineTimes()
	}

	// The upload has done its job: this release is live, so `current` now holds
	// the source a later redeploy copies from and the staging tree is the same
	// bytes twice. Cleared here and nowhere else — see stageUploadedSource.
	if usedStaging {
		if err := os.RemoveAll(UploadStagingDir(b.opts.AppsDir, app.ID)); err != nil {
			log.Printf("Warning: clearing upload staging for %s: %v", app.Slug, err)
		}
	}

	// Tell the auto-deploy poller this commit is already live, so a manual
	// deploy of the tip doesn't get rebuilt on the next poll — and, for a
	// folder app, what the folder was at it.
	if job.advancesPoller() {
		tree := ""
		if deploy.CommitSHA == clonedSHA {
			tree = clonedTree
		}
		var err error
		if seenKnown {
			err = b.db.AdvanceLastSeen(app.ID, deploy.CommitSHA, tree, seenAtClone)
		} else {
			err = b.db.SetLastSeen(app.ID, deploy.CommitSHA, tree)
		}
		if err != nil {
			log.Printf("Warning: recording built commit for %s: %v", app.Slug, err)
		}
	}

	// The catalog version this app is *running*, advanced here and nowhere
	// else. Writing it when the deploy was enqueued would leave a failed
	// gallery update claiming a version whose code never started, while the
	// previous release went on serving — and the whole reason this figure
	// exists is to answer "what is actually running" for somebody deciding
	// whether to take an update.
	//
	// A rollback carries the source deployment's value, so this line rolls the
	// number back with the code.
	if deploy.GalleryVersion != "" {
		if err := b.db.SetAppGalleryVersion(app.ID, deploy.GalleryVersion); err != nil {
			log.Printf("Warning: recording the gallery version for %s: %v", app.Slug, err)
		}
	}

	// The deploy worked, so whatever was wrong with deploying is over.
	b.clearDeployProblems(app.ID)

	// A scheduled re-render is announced to the people its owner named. The
	// discriminator is the trigger the scheduler already stamps on the row —
	// `recordDeployFailure` reads it the same way for the failure case, so this
	// is the symmetric half of an existing branch rather than new plumbing.
	//
	// **Only a render.** A manual redeploy mails nobody: a developer pushing a
	// fix at 11pm should not mail the finance team.
	//
	// **And it cannot fail this deploy.** The release is already serving by the
	// time this runs; a send that could fail it would make a mail relay a
	// dependency of publishing. `Deliver` returns nothing and logs its own
	// failures for exactly that reason.
	//
	// **And it does not hold the worker.** Sequential sends, each bounded only
	// by the mail timeout, ran here under the flip lock, the cache read lock
	// and a start token: a stalled relay held all three for 30 s a recipient
	// (PB-8). Handed to its own goroutine, with one deadline for the run.
	if deploy.TriggeredBy == database.DeployTriggerRender && b.opts.Reports != nil {
		a := *app
		url := PublicURL(b.opts.BaseURL, b.freshApp(app))
		reports, deployID := b.opts.Reports, deploy.ID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), reportDeliveryTimeout)
			defer cancel()
			reports.Deliver(ctx, &a, deployID, url)
		}()
	}

	appendLog(fmt.Sprintf("==> Deploy complete! App available at %s", PublicURL(b.opts.BaseURL, b.freshApp(app))))
	appendLog("DONE")
	// The terminal frame for a watcher. It names the last step rather than a
	// step of its own: "success at flip" is the truth, and inventing a tenth
	// step to mean "finished" would be a step no build ever spends time in.
	// It carries what the deploy noticed and carried on past. A successful
	// deploy is exactly when a warning is easiest to miss and most worth
	// having: nothing failed, so nothing sends anybody to the build log.
	success := NewProgressEvent(deploy.ID, database.DeployStatusSuccess, step, "")
	success.Warnings = DecodeWarnings(deploy.Warnings)
	success.WarningDetails = DecodeWarningDetails(deploy.Warnings)
	b.PublishEvent(success)
	// The one place this is set. Every other exit from execute() is a failure,
	// which is why the deferred finish() defaults to it rather than deriving it
	// from a variable thirty call sites could forget to update.
	outcome = telemetry.OutcomeSucceeded
}

// BuildStep names a phase of a deploy. It is the fingerprint of a deploy
// failure: two builds that died installing dependencies are the same problem
// and are mailed about once, while one that then fails to start is new.
//
// The vocabulary moved to `internal/deploysteps` when a deploy gained its three
// phases, because by then three packages needed it and one of them — the API
// handler that receives an uploaded bundle — records steps the builder never
// sees. These are aliases so that the builder still reads in its own words.
type BuildStep = deploysteps.Step

const (
	// The two an upload performs before the builder is handed anything. They
	// are recorded by the API handler, not here; they are named here so the
	// whole vocabulary can be read in one place.
	StepReceive = deploysteps.Receive
	StepUnpack  = deploysteps.Unpack

	StepSetup     = deploysteps.Setup
	StepFetch     = deploysteps.Fetch
	StepScan      = deploysteps.Scan
	StepDetect    = deploysteps.Detect
	StepConfigure = deploysteps.Configure
	StepBuild     = deploysteps.Build
	StepPublish   = deploysteps.Publish
	StepStart     = deploysteps.Start
	StepReady     = deploysteps.Ready
	StepFlip      = deploysteps.Flip
)

// recordDeployFailure records every failed deploy, and tells the people who
// own the app only about one nobody was watching.
//
// A manual deploy is somebody sitting in front of the streamed build log; they
// already know, and telling them again by email is how a notification channel
// earns a filter rule — so theirs is recorded **silently**. It is still
// recorded: `toft events` and the Events tab are where an agent or a colleague
// asks "what went wrong with this app", and a build that failed an hour ago
// answering "nothing has gone wrong" is the record lying by omission. The
// unattended ones are the opposite case: a push landed at 2am, or a nightly
// report was due, the build failed, and before this nothing said so anywhere
// an owner would look.
//
// The summary names which, because "a pushed commit did not deploy" is a
// sentence about a commit — and a scheduled re-render has none of its own to
// point at.
// deployProblemKinds are the open problems a successful deploy proves over:
// it built, so the deploy failure is; it reached the remote, so the credential
// works, which is the one thing the poller cannot distinguish from a network
// blip on its own; and it passed the dependency gate, so the policy's refusal
// is. A refusal left open after a good deploy never sent its recovery, and the
// next refusal with the same fingerprint collapsed into it and told nobody.
var deployProblemKinds = []events.Kind{
	events.KindDeployFailed,
	events.KindGitAuthFailed,
	events.KindDependencyBlocked,
}

// clearDeployProblems closes every deployProblemKinds event for the app.
func (b *Builder) clearDeployProblems(appID string) {
	for _, kind := range deployProblemKinds {
		b.opts.Events.Clear(appID, kind)
	}
}

func (b *Builder) recordDeployFailure(app *database.App, deploy *database.Deployment, step BuildStep, msg, buildLog string) {
	var what string
	silent := false
	switch deploy.TriggeredBy {
	case database.DeployTriggerAuto:
		what = "A pushed commit did not deploy: "
	case database.DeployTriggerRender:
		what = "A scheduled re-render did not deploy: "
	case database.DeployTriggerRollback:
		what, silent = "A rollback did not finish: ", true
	default:
		what, silent = "A deploy did not finish: ", true
	}
	b.opts.Events.Record(events.Event{
		Silent:       silent,
		AppID:        app.ID,
		Kind:         events.KindDeployFailed,
		Severity:     database.EventSeverityError,
		Source:       database.EventSourceBuilder,
		Summary:      what + firstLine(msg),
		Detail:       eventLogTail(buildLog),
		DeploymentID: deploy.ID,
		Fingerprint:  string(step),
	})
}

// eventLogTailLines is how much of a failed build's log its event carries —
// the same forty lines the CLI's `log_tail` gives a failed deploy. The whole
// log is the deployment's, one `toft logs --build` away; the event is a row in
// a list and an email body, and it held the entire log.
const eventLogTailLines = 40

// eventLogTail is the end of a build log as an event's detail: the last
// eventLogTailLines lines, each as a terminal would have left it.
func eventLogTail(log string) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > eventLogTailLines {
		lines = lines[len(lines)-eventLogTailLines:]
	}
	for i, l := range lines {
		lines[i] = terminalLine(l)
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// terminalLine is one line of a tool's output as a terminal would have shown
// it: what follows the last carriage return, with trailing blanks dropped.
//
// A build tool that renders progress or diagnostics to a fixed width pads every
// line with spaces out to it, whether or not anybody is looking — the event of
// a failed deploy arrived with every line padded to 200 columns, in a pipe as
// much as on a screen. A progress bar redraws with `\r`, and the frames before
// the last are a terminal's business, not the log's.
func terminalLine(line string) string {
	line = strings.TrimRight(line, " \t\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	return strings.TrimRight(line, " \t")
}

// firstLine keeps an event *summary* to one line — the messages fail() is handed
// are sometimes several, and the rest of them belong in the detail.
//
// The progress frame deliberately no longer uses this. An event summary is a
// list row and has to fit one; a terminal frame is the only thing some clients
// will ever read about a failure, and clipping a dependency refusal to its
// headline threw away the packages, the advisories and the fixed versions.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// ReleaseProcessName names the process manager entry for one release. It stays
// stable for the life of the release even if the app is renamed.
func ReleaseProcessName(slug, deploymentID string) string {
	short := strings.ReplaceAll(deploymentID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return slug + "-" + short
}

// ReleaseNameIsDeployment reports whether a release process name was made for
// deploymentID, under whatever slug the app had when it was built. A name
// rebuilt from today's slug misses after a rename that needed no move (an app
// on its own hostname), which marked no deployment current and left the
// serving one deletable by that rule (code review 2026-11-11, the P1-7 case).
func ReleaseNameIsDeployment(name, deploymentID string) bool {
	if name == "" || deploymentID == "" {
		return false
	}
	short := ReleaseProcessName("", deploymentID) // "-" + the id's short form
	return strings.HasSuffix(name, short) && len(name) > len(short)
}

// bootstrapsIdentity reports whether a backend's workloads call home at
// bootstrap, as a pod does and as a runner placed by the shared-mode agent
// does: their app token is minted there, by the platform, for the process that
// asked — and one minted here as well would be a row nothing receives, whose
// supersede after readiness revokes the token the workload actually holds.
func bootstrapsIdentity(rt runtime.ServiceRuntime) bool {
	_, ok := rt.(runtime.IdentityVerifier)
	return ok
}

// DeploymentForWorkload is the deployment a workload name was made from, out
// of an app's deployments, or nil when none matches: a workload named before
// the rule, or a row since pruned. A comparison rather than a parse, because
// ReleaseProcessName truncates the id.
func DeploymentForWorkload(deploys []database.DeploymentRelease, slug, workloadName string) *database.DeploymentRelease {
	if workloadName == "" {
		return nil
	}
	for i := range deploys {
		if ReleaseProcessName(slug, deploys[i].ID) == workloadName {
			return &deploys[i]
		}
	}
	return nil
}

// waitReady polls the endpoint the backend reported until something accepts a
// connection and answers an HTTP request. Any status code counts — a 404 still
// proves the app is up. It takes an endpoint rather than a port because the
// workload need not be a process on this machine.
//
// **It also asks the backend whether the workload is still alive**, through
// watch, and gives up the moment it is not. Dialling alone cannot tell an app
// still importing its dependencies from one that died on its first line, so a
// traceback on startup used to cost the full timeout before anybody saw it. nil
// watch dials only.
func waitReady(endpoint runtime.Endpoint, timeout time.Duration, watch func() error) error {
	if endpoint.Port == 0 {
		return fmt.Errorf("backend reported no endpoint")
	}

	deadline := time.Now().Add(timeout)
	addr := endpoint.Addr()
	// The first response of any status is the answer, a redirect included:
	// following one sent the probe to https://127.0.0.1:<port>/ (a force-HTTPS
	// middleware, which sees no X-Forwarded-Proto here) or to an external
	// IdP, every poll failed, and an app that was up never became ready — and
	// the platform fetched whatever URL an app's Location named (code review
	// 2026-11-11, F-3).
	client := &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	var lastErr error
	nextWatch := time.Now().Add(readinessWatchEvery)
	for time.Now().Before(deadline) {
		// Asked between dials rather than on every one: each answer is a
		// round trip to something else — the agent's socket, the API server —
		// where a dial is only a connect to loopback.
		if watch != nil && !time.Now().Before(nextWatch) {
			if err := watch(); err != nil {
				return err
			}
			nextWatch = time.Now().Add(readinessWatchEvery)
		}
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		conn.Close()

		resp, err := client.Get(endpoint.URL() + "/")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return fmt.Errorf("after %s: %w", timeout, lastErr)
}

// readinessWatchEvery is how often waitReady asks the backend about the
// workload, and readinessRestartsDead how many restarts during the wait mean it
// is not coming up. Two, not one: a process that fails once on a dependency
// that was not up yet and then starts is an app that works. Both backends
// restart the first crash at once and the second after ten seconds (the
// agent uses the kubelet's numbers), so the second restart arrives well inside
// the readiness wait, and the backend's own crash-loop verdict says the same
// thing as soon as it is backing off.
var readinessWatchEvery = 2 * time.Second

const readinessRestartsDead = 2

// errWorkloadExited is what watchWorkload reports; the log tail is appended to
// it by the caller that still has a workload to read the log of.
var errWorkloadExited = errors.New("the app exited before it was ready")

// watchWorkload is waitReady's question to the backend about one workload:
// errored, stopped, crash-looping by the backend's own verdict, or restarted
// readinessRestartsDead times since the wait began. The restart count is
// measured from the first answer rather than from zero, because a backend may
// carry a count across a replacement under the same name.
//
// A backend that cannot answer is not a dead workload: Metrics errors and an
// absent entry keep the wait dialling, exactly as before this existed.
//
// The error carries the last outputTailLines lines of the workload's own log,
// read here because the failure path deletes the workload, and its log with it
// in isolated mode.
func watchWorkload(rt runtime.ServiceRuntime, name string) func() error {
	baseline := -1
	return func() error {
		m, err := rt.Metrics(name)
		if err != nil || m == nil {
			return nil
		}
		if baseline < 0 {
			baseline = m.Restarts
		}
		var why string
		switch {
		case m.CrashLooping:
			why = "it is crash-looping"
		case m.Status == runtime.StatusErrored:
			why = "the backend reports it errored"
		case m.Status == runtime.StatusStopped:
			why = "it stopped"
		case m.Restarts-baseline >= readinessRestartsDead:
			why = fmt.Sprintf("it restarted %d times", m.Restarts-baseline)
		default:
			return nil
		}
		// A death that was memory's is said as memory, whichever of the cases
		// above caught it: the kernel's kill leaves the app nothing to print,
		// so the log tail below reads as the cause when it is not (2026-11-15
		// EPA redeploy: nwca-planttool's tail was package-attach notices).
		memory := runtime.DiedOfMemory(m)
		if memory {
			why = "it ran out of memory (" + runtime.MemoryPhrase(m, 0, false) + ")"
		} else if m.Detail != "" {
			why += " (" + m.Detail + ")"
		}
		tail, lines := workloadLogTail(rt, name)
		if memory {
			why += "\n    Nothing in the app's own output will say so: it was sent a signal, not an error. " +
				"It needs a larger memory limit, which an administrator sets in the app's Runtime settings."
		}
		if tail != "" {
			why += "\n    Last lines of its log:\n" + tail
		}
		return &workloadExitedError{why: why, output: lines, memory: memory,
			platform: !memory && diedInBootstrap(lines)}
	}
}

// diedInBootstrap reports whether the workload's last words are its runner's
// failed bootstrap: the process never got as far as running app code, so the
// failure is Toft's own call to its API and not the app (TOFT-BB-003).
func diedInBootstrap(lines []runtime.LogLine) bool {
	if len(lines) == 0 {
		return false
	}
	last := lines[len(lines)-1]
	return last.Source == runtime.LogSourcePlatform && strings.Contains(last.Line, runner.BootstrapFailed)
}

// workloadExitedError is errWorkloadExited with the lines it quotes, each
// with its own time and stream, so the failure can keep them as they were
// rather than as prose (BB-038, fourth 2026-11-03 black-box list).
type workloadExitedError struct {
	why    string
	output []runtime.LogLine
	// platform is a runner that died in its bootstrap, before any app code
	// ran, and changes the headline: "the app exited" blamed the app for
	// Toft failing to reach itself.
	platform bool
	// memory is a workload the kernel or the agent killed for its memory
	// limit, which is deploysteps.ReasonOutOfMemory rather than not_ready.
	memory bool
}

// errPlatformUnreachable is the headline of a workload whose runner could not
// bootstrap. Still errWorkloadExited underneath, for every caller that asks.
const errPlatformUnreachable = "Toft could not reach its own API to start the app, so no app code ran — " +
	"this is a platform problem, not a fault in the app"

func (e *workloadExitedError) Error() string {
	if e.platform {
		return errPlatformUnreachable + ": " + e.why
	}
	return errWorkloadExited.Error() + ": " + e.why
}
func (e *workloadExitedError) Unwrap() error { return errWorkloadExited }

// FailedStartLine is one line of a failed start's output as the deployment
// keeps it (deployments.failure_output).
type FailedStartLine struct {
	At     *time.Time `json:"at,omitempty"`
	Stream string     `json:"stream,omitempty"`
	Line   string     `json:"line"`
}

// encodeFailedStart is the column's JSON, or "" for none.
func encodeFailedStart(lines []runtime.LogLine) string {
	if len(lines) == 0 {
		return ""
	}
	out := make([]FailedStartLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, FailedStartLine{At: l.At, Stream: l.Stream, Line: l.Line})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ansiEscape is a terminal colour or cursor code: a person reads the failure
// message in a browser, an email and a JSON result, none of them a terminal.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b[^\[]`)

// readinessLogLineMax bounds one line of the tail: the failure message is a
// paragraph a person reads and travels in an email, and a minified stack frame
// is one line of tens of kilobytes.
const readinessLogLineMax = 500

// workloadLogTail is the workload's own output, newest last, as the failure
// message carries it — the app's lines, and the runner's own, but not the
// platform's narration (the `==> ` lines), which is already in the build log
// above.
//
// **The runner's lines are the platform's voice and are not narration.** A
// runner that fails before the app starts — a callback it cannot reach, an
// identity it cannot read — says why on its stderr, marked platform, and exits
// in milliseconds. Kept to the app's lines, that crash loop carried no reason
// anywhere an owner could read it.
func workloadLogTail(rt runtime.ServiceRuntime, name string) (string, []runtime.LogLine) {
	lines, err := rt.Logs(name, outputTailLines*2)
	if err != nil {
		return "", nil
	}
	var out []string
	var kept []runtime.LogLine
	for _, l := range lines {
		if l.Marker != "" {
			continue
		}
		if l.Source != runtime.LogSourceApp && (l.Source != runtime.LogSourcePlatform || strings.HasPrefix(l.Line, "==> ")) {
			continue
		}
		text := ansiEscape.ReplaceAllString(l.Line, "")
		if len(text) > readinessLogLineMax {
			text = text[:readinessLogLineMax] + "…"
		}
		out = append(out, "    "+text)
		kept = append(kept, l)
	}
	if len(out) > outputTailLines {
		out = out[len(out)-outputTailLines:]
		kept = kept[len(kept)-outputTailLines:]
	}
	return strings.Join(out, "\n"), kept
}

// buildEnvironment merges app secrets with the variables Toft controls.
// Toft's own values win so an app cannot, say, redefine its own base path.
//
// Three layers, and the order between them is the whole of the policy:
//
//  1. the app's own variables, which are the only ones it gets to choose;
//  2. the operator's toolchain and proxy environment
//     (`toolchain.Passthrough`), which is how a build behind a corporate proxy
//     or an internal module mirror reaches anything at all. It beats the app,
//     because where this install fetches from is not an app's decision
//     (ADR §64 D15);
//  3. the platform's own values — the base path, and cache directories the
//     platform owns and must be able to account for — which beat both.
//
// GOOS and GOARCH are removed rather than ranked. They are not on the
// operator's allow-list either, so this only reaches an app that set them, and
// a Go app that did is refused outright rather than silently corrected (D16).
func (b *Builder) buildEnvironment(envVars map[string]string, basePath string) map[string]string {
	env := make(map[string]string, len(envVars)+16)
	// The app's layer, minus the names that configure an installer: what the
	// build installs has to be what the scan judged, and the scan never sees
	// an app's variables (toolchain.IsInstallerConfig; the caller logs them).
	for k, v := range envVars {
		if toolchain.IsInstallerConfig(k) {
			continue
		}
		env[k] = v
	}
	for k, v := range toolchain.Passthrough() {
		env[k] = v
	}
	toolchain.StripCrossCompile(env)

	env["BASE_PATH"] = basePath
	env["NEXT_PUBLIC_BASE_PATH"] = basePath
	// The platform's caches, not the operator's. A Go toolchain fetched by
	// GOTOOLCHAIN=auto lands in GOMODCACHE, which is exactly where it belongs:
	// shared across apps, surviving restarts, owned by the platform and counted
	// by it — a module download into a platform cache rather than a mutation of
	// the host (D6).
	//
	// Named in cacheEnv rather than here, because the reclaim has to point the
	// same four tools at the same four directories. Two lists would be one
	// list and a bug: a build filling a directory the reclaim does not empty.
	for k, v := range b.cacheEnv() {
		env[k] = v
	}
	// No uv.toml — the app's `[tool.uv]` tables, its uv.toml, the operator's
	// ~/.config/uv or /etc/uv — shapes what the build installs, because none
	// shapes what the scan judged: the scan passes --no-config, and the two
	// halves change together or the judged set and the installed set diverge
	// (security review 2026-11-14, DG-1). Set last, so nothing above wins.
	env["UV_NO_CONFIG"] = "1"
	// The same for Go: the go command reads GOFLAGS, GOWORK, GOPROXY and the
	// rest from $HOME/.config/go/env when they are not in its environment,
	// and a build's HOME is the account's own, kept between deploys — so one
	// build could plant a module set the next build installs and the scan
	// never judged. The operator's Go settings arrive by Passthrough, never
	// by a file (security review 2026-11-15, LB-10).
	env["GOENV"] = "off"
	return env
}

// ReportDeliverer announces a completed render. One verb, and it returns
// nothing: the deploy has already succeeded when it is called, so there is no
// outcome the builder could act on and nothing it should do differently.
type ReportDeliverer interface {
	Deliver(ctx context.Context, app *database.App, deploymentID, appURL string)
}

// npmInstallCommand prefers a lockfile-exact install, and disables dependency
// lifecycle scripts unless the operator has opted in.
func (b *Builder) npmInstallCommand(dir string) (string, []string) {
	args := []string{"install"}
	if fileExists(filepath.Join(dir, deps.NPMLockfile(dir))) {
		args = []string{"ci"}
	}
	if !b.opts.AllowBuildScripts {
		args = append(args, "--ignore-scripts")
	}
	return "npm", args
}

func (b *Builder) gitClone(deployID, repoURL, branch, destDir, token string, appendLog func(string)) error {
	os.RemoveAll(destDir)
	os.MkdirAll(filepath.Dir(destDir), 0711)

	// "--" stops git parsing later arguments as options, belt-and-braces with
	// ValidateRepoURL. The credential travels via GitAuthEnv, never argv.
	return b.runCommand(deployID, "", "git", []string{
		"clone", "--depth=1", "--single-branch", "--branch", branch, "--", repoURL, destDir,
	}, gitEnv(token), appendLog)
}

func (b *Builder) getCommitSHA(srcDir string) (string, error) {
	// Only report a SHA for a checkout that is itself a repository. Without
	// this, git discovers upward from the release dir and an uploaded app —
	// no .git of its own — records the SHA of whatever repo APPS_DIR happens
	// to live inside (on a dev machine, the platform's own).
	//
	// A directory, never a file: the platform's clones are always a full
	// `.git` directory, and a `.git` *file* (`gitdir: …`) can only have
	// arrived in an uploaded archive, naming a git directory the uploader
	// wrote — whose HEAD would then be stamped as the commit (security review
	// 2026-11-14, CS-7).
	if info, err := os.Lstat(filepath.Join(srcDir, ".git")); err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a git checkout")
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = srcDir
	cmd.Env = runtime.BaseEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (b *Builder) runCommand(deployID, dir, name string, args []string, env map[string]string, appendLog func(string)) error {
	return b.runCommandFor(commandTimeout, deployID, dir, name, args, env, appendLog)
}

// buildTimeout is the clock one build's commands run under: the app's own
// override, then the install's setting, then the compiled-in default.
//
// **Resolved in one place**, which is the property worth stating — a second
// resolution order would mean an app whose override held for its dependency
// install and not for its render, which is precisely the build that the
// override was raised to let finish.
//
// A nil app is a repair rather than a deploy. It has no row to read, and it is
// restoring what dangles rather than doing the work an override was raised for.
func (b *Builder) buildTimeout(app *database.App) time.Duration {
	if app != nil && app.BuildTimeoutSeconds != nil && *app.BuildTimeoutSeconds > 0 {
		return time.Duration(*app.BuildTimeoutSeconds) * time.Second
	}
	if b.db != nil {
		if s, err := b.db.GetPlatformSettings(); err == nil && s.BuildTimeoutSecs > 0 {
			return s.BuildTimeout()
		}
	}
	return commandTimeout
}

// envelope is the operating envelope a workload is started with: the
// administrator's dials where they have set one, and the operator's numbers
// otherwise.
//
// **Read at every start rather than held on Options**, which is what makes the
// settings mean anything: Options is assembled once at process start, so a
// value taken from it could only change on a restart of the platform. Reading
// here costs one settings query per start and is what lets an administrator
// raise a number and have the next start of each app pick it up.
func (b *Builder) envelope() config.Envelope {
	return b.opts.Dials.Envelope()
}

// runCommandFor is runCommand with the clock named by the caller.
//
// One build command has a duration that is a property of the *app's work*
// rather than of package resolution — executing a notebook's cells — and the
// ten minutes above was chosen for `npm ci`. Everything else keeps it; a
// notebook render passes `notebook_render_timeout_seconds`. The distinction is
// a parameter rather than a second function body so that a future per-app
// build timeout has one place to arrive.
func (b *Builder) runCommandFor(timeout time.Duration, deployID, dir, name string, args []string, env map[string]string, appendLog func(string)) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// As the account building this release, when one is (identity.go).
	name, args, env, creds, err := b.identityFor(dir).command(name, args, env)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Build steps run repo-controlled code (npm scripts, setup.py). They get
	// the allowlisted base environment only — never the platform's secrets.
	cmd.Env = append(runtime.BaseEnv(), envSlice(env)...)

	// Read to EOF before Wait, a step that left a daemon holding its output
	// hung the build worker until the timeout — and past it, since killing the
	// step alone left the daemon. Its own process group dies on the timeout
	// and after the step exits; through io.Pipe with WaitDelay, Wait returns at
	// the step's exit plus at most buildOutputGrace, and closing the writer
	// ends the scan.
	ownGroup(cmd)
	asAccount(cmd, creds)
	stdout, outW := io.Pipe()
	cmd.Stdout, cmd.Stderr = outW, outW
	cmd.WaitDelay = buildOutputGrace

	if err := cmd.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		endGroup(cmd)
		outW.Close()
		waited <- err
	}()

	// The command's own words are kept as well as streamed. Without this the
	// error a caller sees is a bare "exit status 128" and git's explanation
	// lives only in the build-log blob — which is exactly the information
	// ClassifyGitError needs, and exactly what an owner needs told.
	var recent tailBuffer
	// The first line a tool marked as its error, kept when the tail has
	// scrolled past it: `npm ci` on an out-of-sync lockfile says why in one
	// line and then prints forty of usage, so the tail alone was the usage
	// (BB-028, second 2026-11-03 bash).
	var firstError string
	var firstErrorAt int

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Stored as a terminal would have left it: padding to a renderer's
		// width and the progress frames before a carriage return are not the
		// command's words, and they were the bulk of what a failed deploy's
		// log and event held.
		line := terminalLine(scanner.Text())
		appendLog("    " + line)
		recent.add(line)
		if firstError == "" && isToolErrorLine(line) {
			firstError, firstErrorAt = line, recent.n
		}
	}
	// A line past the scanner's limit stops the scan; the copy into the pipe
	// must still drain or Wait never returns.
	_, _ = io.Copy(io.Discard, stdout)

	err = <-waited
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // the step exited 0; something it started held the output
	}
	// A command the context killed reports "signal: killed" and nothing else,
	// which is the least useful sentence in the product: it names neither the
	// clock that ran out nor how long it was. Said here, once, so every caller
	// gets it — and errors.Is still works on whatever is underneath.
	if err != nil && ctx.Err() != nil {
		err = &stepTimedOut{err: err, after: timeout}
	}
	if err != nil && recent.len() > 0 {
		// Wrapped, not replaced: callers that only print it get something
		// readable, and errors.Is still works on the exit error underneath.
		if firstError != "" && recent.n-firstErrorAt >= outputTailLines {
			return fmt.Errorf("%w\n%s\n...\n%s", err, firstError, recent.string())
		}
		return fmt.Errorf("%w\n%s", err, recent.string())
	}
	return err
}

// isToolErrorLine is a line a build tool marks as its error: npm's
// "npm error"/"npm ERR!", the convention every caller reads first. npm
// opens with "npm error code EUSAGE" and a bare "npm error", neither of
// which says why — the sentence is the line after them — so those two are
// not the error line (BB-028, seen on trytoft 2026-11-03).
func isToolErrorLine(line string) bool {
	for _, p := range []string{"npm error", "npm ERR!"} {
		rest, ok := strings.CutPrefix(line, p)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		return rest != "" && !strings.HasPrefix(rest, "code ") && !strings.HasPrefix(rest, "errno ")
	}
	return false
}

// npmLockOutOfSync is npm ci's refusal of a lockfile that disagrees with
// package.json — the one npm failure whose fix is always the same.
const npmLockOutOfSync = "can only install packages when your package.json and package-lock.json"

// stepTimedOut is a command its clock stopped. Its own type, so a caller that
// offers "raise the timeout" offers it only when the timeout is what happened.
type stepTimedOut struct {
	err   error
	after time.Duration
}

func (e *stepTimedOut) Error() string { return fmt.Sprintf("%v (stopped after %s)", e.err, e.after) }
func (e *stepTimedOut) Unwrap() error { return e.err }

// timedOut reports whether err is a command its clock stopped.
func timedOut(err error) bool {
	var t *stepTimedOut
	return errors.As(err, &t)
}

// maxBuildLogBytes bounds what one build's log holds in memory. The platform
// is one process — builder, API and app gateway — and the log was held three
// times while a build ran (the text, the live lines, their times) with no
// bound on the number of lines, so one build that printed without stopping
// took every app down with it (security review 2026-11-15, LB-8).
const maxBuildLogBytes = 16 << 20

// buildLogTailLines is how many of the lines after the cap are kept, each at
// most buildLogTailLineBytes, so the ERROR line that ends a failed build is
// always in the stored log.
const (
	buildLogTailLines     = 300
	buildLogTailLineBytes = 4 << 10
)

// buildLogCapNotice is the line a capped log carries where storing stopped.
const buildLogCapNotice = "… the build log reached 16 MB; output from here on is not stored, " +
	"the last lines follow at the end"

// logFunnel is every line a build logs, on its way to the stored text, the
// live copy and its listeners.
//
// Each line is cleaned before it is stored or streamed, so the live stream,
// the line times and the row all hold the same text: tool output carrying a
// NUL byte or invalid UTF-8 failed the row's final write, which left it
// `building` with no log (PB-2). Here and not in terminalLine, because git
// output, the removed-files block and readiness errors arrive here too.
//
// Past maxBuildLogBytes the text, the live lines and their times all stop
// together — so they stay aligned for LiveLogTimes — and later lines are only
// counted, the last buildLogTailLines of them kept for text().
type logFunnel struct {
	b       *Builder
	id      string
	head    strings.Builder
	capped  bool
	dropped int
	tail    []string
	tailN   int
}

func (f *logFunnel) add(line string) {
	line = textutil.Clean(line)
	if line == "DONE" {
		// The socket's framing: always sent, never a line of the live copy.
		f.b.broadcast(f.id, line)
	}
	if !f.capped && f.head.Len()+len(line)+1 > maxBuildLogBytes {
		f.capped = true
		f.head.WriteString(buildLogCapNotice + "\n")
		f.b.broadcast(f.id, buildLogCapNotice)
	}
	if f.capped {
		f.dropped++
		if f.tail == nil {
			f.tail = make([]string, buildLogTailLines)
		}
		f.tail[f.tailN%buildLogTailLines] = textutil.Clip(line, buildLogTailLineBytes)
		f.tailN++
		return
	}
	f.head.WriteString(line + "\n")
	if line != "DONE" {
		f.b.broadcast(f.id, line)
	}
}

// text is the log as stored: everything up to the cap, then how many lines
// were dropped, then the last of them.
func (f *logFunnel) text() string {
	if !f.capped {
		return f.head.String()
	}
	kept := f.tailN
	if kept > buildLogTailLines {
		kept = buildLogTailLines
	}
	var out strings.Builder
	out.WriteString(f.head.String())
	if gone := f.dropped - kept; gone > 0 {
		fmt.Fprintf(&out, "… %d lines dropped …\n", gone)
	}
	for i := f.tailN - kept; i < f.tailN; i++ {
		out.WriteString(f.tail[i%buildLogTailLines] + "\n")
	}
	return out.String()
}

// outputTailLines is how many lines of a failed command's output are carried
// back in its error. Enough for a stack trace or a compiler's last complaint,
// short enough to put in an email body.
const outputTailLines = 20

// tailBuffer keeps the last outputTailLines lines with a fixed footprint. A
// build log can be megabytes and none of it before the end explains a failure.
type tailBuffer struct {
	lines [outputTailLines]string
	n     int
}

func (t *tailBuffer) add(line string) {
	t.lines[t.n%outputTailLines] = line
	t.n++
}

func (t *tailBuffer) len() int {
	if t.n < outputTailLines {
		return t.n
	}
	return outputTailLines
}

func (t *tailBuffer) string() string {
	count := t.len()
	out := make([]string, 0, count)
	start := t.n - count
	for i := start; i < t.n; i++ {
		out = append(out, t.lines[i%outputTailLines])
	}
	return strings.Join(out, "\n")
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func (b *Builder) getDecryptedEnvVars(appID string) (map[string]string, error) {
	vars, err := b.db.GetEnvVars(appID)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, v := range vars {
		decrypted, err := b.encryptor.Decrypt(v.Value)
		if err != nil {
			return nil, fmt.Errorf("decrypting %s: %w", v.Key, err)
		}
		result[v.Key] = decrypted
	}

	// The third source of a workload's environment, beside the app author's
	// variables above and the platform's contract variables added later: a
	// credential an administrator attached.
	//
	// **No precedence question is being answered here**, and that is by
	// construction rather than by luck: a key an attached credential supplies
	// and the app also declares is refused at the *attach*, where a person is
	// looking at both. Either order would be defensible and both would be
	// silent — the app winning means an administrator's credential quietly does
	// nothing, the credential winning means an owner's variable quietly changes
	// value.
	//
	// A failure here fails the deploy rather than starting the app without the
	// credential. An app that comes up missing the warehouse password fails at
	// its first query, in somebody else's logs, and looks like a database
	// problem.
	brokered, err := b.db.AppCredentialValues(appID, b.encryptor.Decrypt)
	if err != nil {
		return nil, fmt.Errorf("reading this app's credentials: %w", err)
	}
	for k, v := range brokered {
		result[k] = v
	}
	return result, nil
}

// mintAppToken issues an outbound credential for one workload start and
// records it.
//
// Returns the secret to inject and the row id to keep when superseded tokens
// are revoked. Both are empty when minting failed, which is a warning in the
// build log and not a failed deploy: the app still runs, it just holds no
// platform identity, and the SDK will say so by name rather than mysteriously.
//
// `workload` is the process this token is for. It is what lets a later start of
// the *same* workload supersede this one without touching a different release
// running beside it — see database.SupersedeWorkloadTokens.
func (b *Builder) mintAppToken(app *database.App, deployID, workload string, appendLog func(string)) (secret, tokenID string) {
	token, hash, prefix, err := appint.MintAppToken()
	if err != nil {
		appendLog(fmt.Sprintf("==> Warning: could not mint this app's platform token: %v", err))
		return "", ""
	}
	tokenID = uuid.NewString()
	if err := b.db.CreateAppToken(&database.AppToken{
		ID:           tokenID,
		AppID:        app.ID,
		DeploymentID: deployID,
		Workload:     workload,
		TokenHash:    hash,
		Prefix:       prefix,
	}); err != nil {
		appendLog(fmt.Sprintf("==> Warning: could not record this app's platform token: %v", err))
		return "", ""
	}
	return token, tokenID
}

// installNodeSDK copies the Node SDK into an app's node_modules, unless the app
// installed its own.
//
// The app's copy wins, which is the same floor-not-ceiling rule the Python side
// gets from ordering — but it has to be a presence check here, because `npm ci`
// runs before this and would wipe anything installed ahead of it.
//
// Never fatal, for the reason the Python warning gives: an app that does not
// import the SDK should not fail to deploy over a convenience.
// The two registers rather than one, for the reason buildLog gives: this
// function's failure is the platform's own SDK plumbing, and on the repair path
// the step register is read by the app's owner.
func installNodeSDK(releaseDir string, lg buildLog) {
	installed, err := sdk.EnsureNode(releaseDir)
	switch {
	case err != nil:
		lg.output(fmt.Sprintf("    (installing the Toft SDK failed: %v)", err))
		lg.step("    Warning: the Toft SDK could not be installed.")
		lg.step("    'import toft from \"" + sdk.NodePackage + "\"' will not work unless the app vendors it.")
	case installed:
		lg.step("==> Installed the Toft SDK (" + sdk.NodePackage + ") " + sdk.Describe("node"))
		lg.step(sdk.NodeDeprecatedLine)
	default:
		lg.step(sdk.NodeKeptLine(releaseDir))
	}
}

// recordReleaseSizes records what a release costs, in both places it can cost
// anything. Only ever for a deployment that produced a release of its own: a
// promote serves the source deployment's directory and artifact, so crediting it
// with their size would count the same bytes twice and then offer to reclaim
// them twice.
func (b *Builder) recordReleaseSizes(appID, deployID string, desc artifact.Descriptor) {
	var packed *int64
	if desc.Key != "" {
		packed = &desc.Size
	}
	if err := b.db.SetDeploymentSizes(deployID, measureReleaseDir(b.opts.AppsDir, appID, deployID), packed); err != nil {
		log.Printf("Warning: recording release size for %s: %v", deployID, err)
	}
}

// restoredStatus is what an app with a previous release reads after a failed
// deploy: the state that release was in before the build, when it was not
// serving — asleep, stopped or crashed — and the live status otherwise. Writing
// `running` over a sleeping release told the gateway to dial a workload that
// was not there: the app answered 503 "unavailable" while toft status said
// running, and only a restart brought it back (2026-11-03 evening drills).
func restoredStatus(before, framework string) string {
	switch before {
	case database.AppStatusSleeping, database.AppStatusStopped, database.AppStatusCrashed:
		return before
	}
	return database.LiveStatus(framework)
}

// mountsWithoutRowsUnlessPlanned is mountsWithoutRows asked before the flip's
// storage reconcile: a started mount the plan adds or changes has no row until
// the reconcile writes it, so it is not a gap (PB-4). Matched on name, folder,
// path and access — the plan's entry, as the workload was started with it.
func mountsWithoutRowsUnlessPlanned(started, recorded []runtime.Mount, plan *StoragePlan) []string {
	if plan == nil {
		return mountsWithoutRows(started, recorded)
	}
	planned := map[string]bool{}
	for _, list := range [][]PlannedStorage{plan.Add, plan.Change} {
		for _, p := range list {
			planned[p.Name+"\x00"+p.Folder+"\x00"+p.Path+"\x00"+p.Access] = true
		}
	}
	var rest []runtime.Mount
	for _, m := range started {
		if !planned[m.Name+"\x00"+m.Folder+"\x00"+m.Path+"\x00"+m.Access] {
			rest = append(rest, m)
		}
	}
	return mountsWithoutRows(rest, recorded)
}

// mountsWithoutRows names the mounts a workload was started with that the
// table no longer records — by name and folder, at the same path and access.
// A mapping attached since the start is not a difference: the workload does
// not hold it, and its next start will.
func mountsWithoutRows(started, recorded []runtime.Mount) []string {
	have := map[string]bool{}
	for _, m := range recorded {
		have[m.Name+"\x00"+m.Folder+"\x00"+m.Path+"\x00"+m.Access] = true
	}
	var gone []string
	for _, m := range started {
		if !have[m.Name+"\x00"+m.Folder+"\x00"+m.Path+"\x00"+m.Access] {
			name := m.Name
			if m.Folder != "" {
				name += "/" + m.Folder
			}
			gone = append(gone, name)
		}
	}
	return gone
}
