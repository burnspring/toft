package builder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"toft/internal/applog"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
	"toft/internal/telemetry"
)

// endpointAdopter is implemented by backends that keep their address
// bookkeeping in memory and therefore forget it across a platform restart.
// Reconcile tells such a backend about the workloads it finds already running,
// so it does not later hand a second workload the address one of them holds.
// A backend whose addresses live in the cluster has nothing to adopt.
type endpointAdopter interface {
	Adopt(name string, ep runtime.Endpoint)
}

// errNoRelease means the release directory is gone (ephemeral storage without
// a volume, or a wiped APPS_DIR) — only a fresh deploy can bring the app back.
var errNoRelease = errors.New("release directory missing — redeploy the app")

// Reconcile restarts every app the database believes is running but the
// process manager has no process for. That is the normal state after a host
// reboot, a container replacement, or a shared-mode agent that lost its state
// (which lives under /run, so a reboot empties it): the release directories
// and the `running` rows survive, the processes don't — and
// nothing else ever starts them again (the gateway proxies straight to the
// dead port).
//
// Call it once at startup, after the runtime's daemon is up. It is safe to run
// concurrently with the build queue: it goes through the same rt.Deploy path a
// build does (so account isolation and release handover apply), reuses the
// app's allocated port, and touches only apps whose workload is not running —
// a node that finds the process already alive (another node on the same host
// beat it there, or the agent outlived a platform-only restart, as it is meant
// to) leaves it
// alone. Missing is one way to be not running; an entry the process manager
// still lists but is not running is the other, and both restart.
// Reconciled closes once the boot Reconcile has finished, whatever it found —
// it is a statement that every app has been looked at, not that every app is
// up. The health watcher holds its sweeps on it (see health.WithHold).
func (b *Builder) Reconciled() <-chan struct{} { return b.reconciled }

func (b *Builder) Reconcile() {
	// Nil on a Builder made without New — a test's struct literal — which has
	// nothing waiting on it; closing nil would panic the reconcile itself.
	defer b.reconciledOnce.Do(func() {
		if b.reconciled != nil {
			close(b.reconciled)
		}
	})
	// One sweep, one span, one duration. Reconcile runs on the way up and its
	// cost is the gap between "the process is listening" and "the estate is
	// serving again" — which is the number an operator wants after a restart
	// and which nothing until now recorded.
	ctx, span := telemetry.Start(context.Background(), "reconcile")
	start := time.Now()

	apps, err := b.db.ListAllApps()
	if err != nil {
		log.Printf("reconcile: listing apps: %v", err)
		span.End(err)
		telemetry.RecordSweep(ctx, telemetry.SweepReconcile, telemetry.OutcomeFailed, time.Since(start))
		return
	}
	defer func() {
		span.EndOK(telemetry.OutcomeSucceeded)
		telemetry.RecordSweep(ctx, telemetry.SweepReconcile, telemetry.OutcomeSucceeded, time.Since(start))
	}()

	// Starts run reconcileParallelism at a time. One at a time, the last app
	// waited for every fetch, unpack and hand-over ahead of it — on a disk the
	// platform did not keep, that is minutes per app after every task
	// replacement. Each app is started once: the loop hands each to one worker.
	var (
		mu               sync.Mutex
		restored, failed int
		wg               sync.WaitGroup
	)
	slots := make(chan struct{}, reconcileParallelism)
	count := func(ok bool) {
		mu.Lock()
		defer mu.Unlock()
		if ok {
			restored++
		} else {
			failed++
		}
	}
	defer func() {
		wg.Wait()
		if restored+failed > 0 {
			log.Printf("reconcile: restored %d app(s), %d failed", restored, failed)
		}
	}()
	for i := range apps {
		app := &apps[i]
		if app.CurrentRelease == "" {
			continue
		}
		// A sleeping app must stay asleep — restarting it here would undo the
		// reaper on every platform restart. But its process is still registered
		// with the backend and still holds an address, and a backend that keeps
		// that bookkeeping in memory has just forgotten it. Adopt without
		// starting anything, or a later deploy hands the sleeping app's port to
		// something else.
		if app.Status == database.AppStatusSleeping {
			rt, err := b.rts.For(app.RunningModeOrShared())
			if err != nil {
				log.Printf("reconcile: %s: %v", app.Slug, err)
				continue
			}
			if _, err := rt.Metrics(app.CurrentRelease); err == nil {
				if adopter, ok := rt.(endpointAdopter); ok {
					adopter.Adopt(app.CurrentRelease, app.Endpoint())
				}
			}
			continue
		}

		// `crashed` gets the same treatment as `running`: its interstitial
		// promises the app is restarting, and after a daemon death nothing
		// else will ever make that true. `stopped` and `failed` are deliberate
		// states — an owner or a build set them — and stay untouched.
		if app.Status != database.AppStatusRunning && app.Status != database.AppStatusCrashed {
			continue
		}
		// A lock withholds starting, and a boot restart is a start. Left as it
		// is: the status stays whatever the lock's stop left it.
		if app.Locked() {
			continue
		}

		// The backend that started this release owns it, and that is recorded
		// on the app rather than inferred: asking the *desired* mode would send
		// the reconciler to the wrong backend for every app an administrator
		// promoted since its last deploy, and it would then restart a running
		// app as a duplicate somewhere else.
		rt, err := b.rts.For(app.RunningModeOrShared())
		if err != nil {
			log.Printf("reconcile: %s: %v", app.Slug, err)
			count(false)
			continue
		}

		// Present is not running. The question this asks is whether the
		// workload is *up*, and a backend answers Metrics perfectly well for
		// one that is not: an `errored` entry the agent is backing off, or
		// one it holds `stopped`. Splitting on `err == nil` read
		// both as "the process manager owns it from here" and skipped them at
		// every boot — which is why an app could sit errored across restarts
		// with nothing ever starting it.
		if m, err := rt.Metrics(app.CurrentRelease); err == nil && runtime.WorkloadIsUp(m.Status) {
			// Hand the backend back the address this survivor is using, so a
			// later deploy cannot allocate it to something else. A backend
			// whose addresses live in the cluster has nothing to adopt.
			if adopter, ok := rt.(endpointAdopter); ok {
				adopter.Adopt(app.CurrentRelease, app.Endpoint())
			}
			// The process is running; the process manager owns it from here.
			// But heal the status if the DB disagrees with reality — a
			// `crashed` row outlives the agent's own restart because nothing
			// else ever writes the recovery back.
			if app.Status == database.AppStatusCrashed && m.Status == runtime.StatusOnline {
				b.db.UpdateAppStatus(app.ID, database.AppStatusRunning)
				log.Printf("reconcile: %s is online again — status healed to running", app.Slug)
			}
			continue
		}

		wg.Add(1)
		slots <- struct{}{}
		go func(app *database.App, rt runtime.ServiceRuntime) {
			defer wg.Done()
			defer func() { <-slots }()
			// The status this pass saw is the one it may change: a stop made
			// while the start was in flight stands (F-5).
			seen := app.Status
			if err := b.startOn(app, rt); err != nil {
				if errors.Is(err, ErrStartSuperseded) {
					// A deploy got there first; its flip owns the status.
					return
				}
				log.Printf("reconcile: %s: %v", app.Slug, err)
				// The app is provably not serving; leave a status the dashboard
				// and gateway interstitial tell the truth about.
				b.db.UpdateAppStatusFrom(app.ID, seen, database.AppStatusCrashed)
				count(false)
				return
			}
			// A Stop or a lock made while the start was in flight wrote its own
			// word; the workload this start placed is stopped again rather than
			// left live under it (DB-U1).
			if moved, err := b.db.UpdateAppStatusFrom(app.ID, seen, database.AppStatusRunning); err == nil && !moved {
				b.StopIfNoLongerWanted(app.ID)
			}
			count(true)
		}(app, rt)
	}
}

// reconcileParallelism is how many boot starts run at once: enough to overlap
// one app's download with another's unpack, few enough that a boot does not
// saturate the store, the disk or the CPU the platform itself needs.
const reconcileParallelism = 4

// Wake starts a sleeping app's workload again and marks it running. It is the
// inverse of what the idle reaper did, and it never builds: the release is
// already on disk, which is what makes a wake a second rather than a deploy.
//
// It used to have two paths — a backend-level restart when the process manager
// still knew the workload, and a full restore when it did not — and the first
// of them was a quiet bug: the process manager of the day replayed the
// environment it saved at process creation, so an app woken after a variable
// changed came back with the old value. There is one path now because there
// is one way to start a workload,
// and it reads the environment from the database on the way.
//
// A failed wake leaves the app `crashed`, whose interstitial already tells a
// visitor the truth. Leaving it `sleeping` would be worse: every subsequent
// request would try the wake again and hang for its own timeout.
//
// **How long it took is said to the owner** (ADR §123): the first visitor to a
// slept app waits for exactly this, and the metric beside it exists only on an
// install with telemetry on. One line in the Logs tab is what lets an owner
// weigh "sleeping saves money" against "my first viewer waits".
func (b *Builder) Wake(app *database.App) error {
	began := time.Now()
	if err := b.Start(app); err != nil {
		if errors.Is(err, ErrStartSuperseded) {
			// A deploy flipped while the wake was asked for; its workload is
			// the one serving, and its flip wrote the status.
			return nil
		}
		b.db.UpdateAppStatusFrom(app.ID, database.AppStatusSleeping, database.AppStatusCrashed)
		return fmt.Errorf("waking %s: %w", app.Slug, err)
	}
	took := time.Since(began).Round(100 * time.Millisecond)
	b.narrator.For(app.ID, app.CurrentRelease, "").Sayf(applog.StreamStdout,
		"Woke from idle sleep in %s.", took)

	log.Printf("idle: woke %s on %s in %s", app.Slug, app.Endpoint().Addr(), took)
	// From sleeping: a Stop made while the wake was in flight stands (F-5),
	// and the workload the wake placed is stopped again under it (DB-U1).
	moved, err := b.db.UpdateAppStatusFrom(app.ID, database.AppStatusSleeping, database.AppStatusRunning)
	if err == nil && !moved {
		b.StopIfNoLongerWanted(app.ID)
	}
	return err
}

// Start makes the app's current release be what is running, and is the only
// way a workload is started outside a deploy — which builds first and then
// arrives at the same Deploy call.
//
// **The platform supplies the whole description, every time.** That is not an
// implementation detail of this function, it is the reason it exists: a
// backend handed only a process name cannot rebuild an isolated pod spec at
// all, because every part of it — release reference, framework, entrypoint,
// mounts — lives in this database and not in a name, and a shared backend
// asked to bounce a name replays the
// environment it saved when the process was created. Assembling the spec here
// — release, framework, entrypoint, mounts, identity, and `Env` read from the
// database at the moment of the call — is what makes a start carry the
// configuration as it is now, in both modes, with no rebuild.
//
// Five callers and the list is the specification: a deploy, boot Reconcile,
// the proxy's wake, the Start/Restart button, and internal/supervisor's
// recovery loop. A sixth that starts a workload without coming through here is
// a bug.
func (b *Builder) Start(app *database.App) error {
	rt, err := b.rts.For(app.RunningModeOrShared())
	if err != nil {
		return err
	}
	return b.startOn(app, rt)
}

// startLock takes the app's start mutex and returns its release.
// flipLock serialises the flips of one app's deploys: each reads the serving
// release and retires it under the lock, so no release is retired twice and
// none is skipped.
func (b *Builder) flipLock(appID string) func() {
	m, _ := b.flipping.LoadOrStore(appID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// servingBeforeFlip takes app's flip lock and refreshes the snapshot's serving
// release and mode from the row, so the flip retires what is serving now and
// not what was serving when the deploy began. The caller releases the lock
// once the previous release is retired.
func (b *Builder) servingBeforeFlip(app *database.App) func() {
	unlock := b.flipLock(app.ID)
	b.refreshServing(app)
	return unlock
}

// refreshServing is servingBeforeFlip's read without its lock, for a caller
// that already holds the flip lock — a rollback takes it before its start
// (PB-1).
func (b *Builder) refreshServing(app *database.App) {
	if fresh, err := b.db.GetApp(app.ID); err == nil && fresh != nil {
		app.CurrentRelease = fresh.CurrentRelease
		app.RunningMode = fresh.RunningMode
	}
}

// isServing reports whether name is the app's serving workload as the row says
// now. A failed deploy never deletes that workload, and a rollback to the
// release already serving does nothing (PB-1): two rollbacks to one release
// share one workload name, so without the question the second restarted the
// serving workload in place, and deleted it if anything later failed. A read
// error answers false — the caller's ordinary path.
func (b *Builder) isServing(appID, name string) bool {
	if name == "" {
		return false
	}
	fresh, err := b.db.GetApp(appID)
	return err == nil && fresh != nil && fresh.CurrentRelease == name
}

func (b *Builder) startLock(appID string) func() {
	m, _ := b.starting.LoadOrStore(appID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Halt removes the app's current workload, in whichever backend runs it, under
// the start lock so it cannot land in the middle of somebody else's start.
//
// It is for a restart that was owed and did not happen (supervisor.Remount): a
// start can fail long before it replaces anything — restoring the release,
// reading mounts, decrypting variables, an unreachable agent — and in both
// modes the old workload is replaced only deep inside the start. Left alone, it
// goes on running with the mount or credential the restart existed to take
// away, and the health watcher heals the row back to `running` within a
// minute. An absent workload is a successful halt.
func (b *Builder) Halt(app *database.App) error {
	rt, err := b.rts.For(app.RunningModeOrShared())
	if err != nil {
		return err
	}
	unlock := b.startLock(app.ID)
	defer unlock()
	if err := rt.Delete(app.CurrentRelease); err != nil && runtime.AbsenceReason(err) == "" {
		return err
	}
	// No process is left to hold any of this workload's tokens.
	if err := b.db.SupersedeWorkloadTokens(app.ID, app.CurrentRelease, ""); err != nil {
		log.Printf("halt: withdrawing %s's tokens: %v", app.Slug, err)
	}
	return nil
}

// StopIfNoLongerWanted is what a start owes when its guarded status write did
// not move: it re-reads the row and, if the app is now `stopped` or locked,
// stops the workload the start just placed. Best effort; failures are logged.
//
// A guarded write alone is not enough. A Stop that lands before the start has
// placed its workload finds nothing to stop and is answered as a success (an
// absent workload is a stopped one), the start then places the workload
// anyway, and the guarded write declines — leaving a live process under a
// `stopped` row that nothing will ever look at again. Every start path that
// guards its status write (remount, restart, wake, boot reconcile, the
// supervisor's recovery) calls this when the write declines.
//
// It takes the start lock, so a person's Start pressed after their Stop is
// waited for and then seen as `running` rather than stopped underneath them.
// **The caller must not hold the start lock.**
func (b *Builder) StopIfNoLongerWanted(appID string) {
	unlock := b.startLock(appID)
	defer unlock()
	fresh, err := b.db.GetApp(appID)
	if err != nil || fresh == nil {
		if err != nil {
			log.Printf("start: re-reading app %s after a declined status write: %v", appID, err)
		}
		return
	}
	if fresh.Status != database.AppStatusStopped && !fresh.Locked() {
		return
	}
	rt, err := b.rts.For(fresh.RunningModeOrShared())
	if err != nil {
		log.Printf("start: %s was stopped during its start, and cannot be stopped again: %v", fresh.Slug, err)
		return
	}
	if err := rt.Stop(fresh.ProcessName()); err != nil && runtime.AbsenceReason(err) == "" {
		log.Printf("start: %s was stopped during its start, and stopping it again failed: %v", fresh.Slug, err)
		return
	}
	log.Printf("start: %s was stopped or locked while it started; its workload is stopped again", fresh.Slug)
}

// servingDetection is what the serving release's build detected: from its own
// deployment row when the build recorded it there, from the app row for a
// deployment made before migration 00073 (or when the row cannot be read).
func (b *Builder) servingDetection(app *database.App) (framework, entrypoint, startCmd string) {
	if deploys, err := b.db.ListDeploymentReleases(app.ID); err == nil {
		if d := DeploymentForWorkload(deploys, app.Slug, app.CurrentRelease); d != nil && d.Framework != "" {
			return d.Framework, d.Entrypoint, d.StartCmd
		}
	}
	return app.Framework, app.Entrypoint, app.StartCmd
}

// startOn is Start with the backend already resolved, for the one caller that
// has it in hand for another reason.
func (b *Builder) startOn(app *database.App, rt runtime.ServiceRuntime) (err error) {
	// Detection re-reads the release on every boot, wake and restart: content
	// that panics a parser fails this one start, not the process and every
	// other app's reconcile with it.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("start: %s panicked: %v\n%s", app.Slug, r, debug.Stack())
			err = fmt.Errorf("internal error starting %s", app.Slug)
		}
	}()
	// One start per app at a time. Each start mints a token for the workload
	// and, once its process answers, supersedes every other token of that
	// workload — right for one start, and fatal for two that overlap: each
	// revokes the other's, and whichever process ends up serving holds a
	// revoked TOFT_APP_TOKEN while the app reads `running`. Two credential
	// detaches a second apart (supervisor.Remount starts in a goroutine), or a
	// detach beside a wake, were enough. Serialised, the later start simply
	// replaces the earlier one, and the token left live is the serving one's.
	unlock := b.startLock(app.ID)
	defer unlock()

	// The row as it is under the lock, not the caller's snapshot. Boot
	// Reconcile lists every app once and reaches some of them minutes later;
	// a deploy that flipped in between had its old workload started again
	// under the old name beside the new one, and SetAppEndpoint then pointed
	// the row at the stray (code review 2026-11-11, F-5). A start asked for a
	// release that is no longer the app's is nothing to start.
	if fresh, err := b.db.GetApp(app.ID); err == nil && fresh != nil {
		if fresh.Locked() {
			return ErrAppLocked
		}
		if fresh.CurrentRelease != app.CurrentRelease {
			return ErrStartSuperseded
		}
		app = fresh
	} else if app.Locked() {
		return ErrAppLocked
	}
	// A batch app has no service (ADR §107). Every caller above already leaves
	// a `ready` app alone; this is the floor under all of them, so a sixth
	// that forgets refuses rather than crash-looping a program that exits.
	if IsBatchFramework(app.Framework) {
		return ErrNoService
	}
	mode := app.RunningModeOrShared()

	// One read for the whole description, as in the deploy path.
	env := b.envelope()
	res := env.ResourcesForApp(app.MemoryLimitMB, app.CPULimitMillicores)
	spec := runtime.WorkloadSpec{
		AppID:         app.ID,
		Slug:          app.Slug,
		Name:          app.CurrentRelease,
		BasePath:      ServingBasePath(app),
		MemoryLimitMB: res.MemoryMB.Value,
		BindAddress:   bindAddressFor(mode),

		CPURequestMillicores: env.AppCPURequestMillicores,
		CPULimitMillicores:   res.CPUMillicores.Value,

		// Derived, not recovered — which is the difference between this and the
		// app token beneath it. A token is a hashed row and a restored workload
		// has to be minted a new one; a key is a function of the app id and the
		// workload name, so a woken or restarted workload derives the *same* key
		// its deploy did and every assertion already in flight stays valid.
		RoleKey: b.roleKeyFor(app.ID, app.CurrentRelease, mode),
	}

	// A restored workload gets its own token, because the one its deploy minted
	// is unrecoverable — hashed on the way in. Without this, an app woken from
	// idle sleep or restored after a crash came back with **no platform
	// identity**: `toft.notify`, `toft.jobs` and the rest failed until somebody
	// deployed again, and nothing said why.
	//
	// Shared mode only. An isolated workload is handed one when its pod
	// bootstraps, which is the one moment the platform can give it a secret at
	// all, and minting a second here would supersede that one before the pod
	// had used it.
	//
	// The outgoing process's token is superseded only once the replacement
	// answers, as the deploy path does: a shared restart reuses the workload's
	// name, so superseding at mint time cut the old process off for the whole
	// of a slow start — a minute of `invalid_app_token` on every SDK call a
	// Streamlit page made while it was still the one serving.
	var tokenID string
	if mode == runtime.ModeShared && !bootstrapsIdentity(rt) {
		var token string
		token, tokenID = b.mintAppToken(app, "", app.CurrentRelease, func(string) {})
		spec.AppToken = token
	}

	if mode == runtime.ModeIsolated {
		// An isolated workload has no access to APPS_DIR, so there is no
		// directory here to re-detect from — it fetches its published release
		// and is told what to run. That is exactly why detection results are
		// persisted, and why they are read from the serving release's own
		// deployment: the app row already describes a build that is still
		// running, or one that failed.
		if app.ReleaseRef == "" {
			return errNoRelease
		}
		spec.Release = runtime.ParseReleaseRef(app.ReleaseRef)
		spec.Framework, spec.Entrypoint, spec.StartCmd = b.servingDetection(app)
	} else {
		releaseDir := CurrentLink(b.opts.AppsDir, app.ID)
		if !fileExists(releaseDir) {
			if err := b.restoreCurrent(app); err != nil {
				return err
			}
		}
		// The recorded detection, as the isolated branch uses: the tree is
		// the app account's after the handover, and re-detecting from it let
		// a manifest-less Python app that had written one .html beside app.py
		// come back from sleep as a static site serving its own source (code
		// review 2026-11-11, E-6). Detection is re-run only for a release
		// with no record — one built before the columns existed.
		spec.Release = runtime.LocalRelease(releaseDir)
		spec.Framework, spec.Entrypoint, spec.StartCmd = b.servingDetection(app)
		if spec.Framework == "" {
			detection := DetectFramework(releaseDir)
			spec.Framework = string(detection.Framework)
			spec.Entrypoint = detection.Entrypoint
			spec.StartCmd = detection.StartCmd
		}
	}

	// Both modes. Reading these in the isolated branch alone left a shared app
	// woken, restarted or restored after a reboot with no TOFT_STORAGE: its
	// links and its group survived the restart, its description of them did
	// not, so `toft.storage()` failed until somebody deployed again.
	mounts, err := b.db.AppMounts(app.ID)
	if err != nil {
		return fmt.Errorf("reading storage mappings: %w", err)
	}
	spec.Mounts = mounts

	envVars, err := b.getDecryptedEnvVars(app.ID)
	if err != nil {
		return err
	}
	spec.Env = envVars

	endpoint, err := rt.Deploy(spec)
	if err != nil {
		// The token minted for a start that never happened is withdrawn;
		// whatever was serving before keeps its own (review #6).
		if tokenID != "" {
			if err := b.db.RevokeAppToken(tokenID); err != nil {
				log.Printf("start: withdrawing the failed start's token for %s: %v", app.Slug, err)
			}
		}
		b.sayStartFailed(app, err)
		return err
	}
	if err := waitReady(endpoint, readinessTimeout, watchWorkload(rt, spec.Name)); err != nil {
		_ = rt.Delete(app.CurrentRelease)
		// The workload is deleted, old instance and new: none of its tokens
		// has a process left to hold it (review #6).
		if tokenID != "" {
			if err := b.db.SupersedeWorkloadTokens(app.ID, app.CurrentRelease, ""); err != nil {
				log.Printf("start: withdrawing %s's tokens after a failed start: %v", app.Slug, err)
			}
		}
		b.sayStartFailed(app, err)
		return err
	}
	if tokenID != "" {
		if err := b.db.SupersedeWorkloadTokens(app.ID, app.CurrentRelease, tokenID); err != nil {
			log.Printf("restore: superseding app tokens for %s: %v", app.Slug, err)
		}
	}

	// The restarted workload's address is whatever the backend just reported —
	// not necessarily the one the row remembers, since the backend allocates
	// afresh when it has no memory of this release. Persist it before the row
	// is marked running, or the gateway proxies to the dead address.
	if endpoint != app.Endpoint() {
		if err := b.db.SetAppEndpoint(app.ID, endpoint.Host, endpoint.Port); err != nil {
			_ = rt.Delete(app.CurrentRelease)
			return fmt.Errorf("recording restored endpoint: %w", err)
		}
		app.EndpointHost = endpoint.Host
		app.Port = database.Port(endpoint.Port)
	}

	// The running process is now carrying today's variables, so anything the
	// app's Overview was reporting as un-applied is applied. After readiness,
	// deliberately: a start that never came up is not carrying anything.
	if err := b.db.MarkEnvApplied(app.ID); err != nil {
		log.Printf("start: recording applied environment for %s: %v", app.Slug, err)
	}

	log.Printf("reconcile: restored %s (release %s, endpoint %s)", app.Slug, app.CurrentRelease, endpoint.Addr())
	return nil
}

// sayStartFailed is what an owner is left with when a start does not come up.
//
// **Every caller of this path had somewhere for the error to go except the one
// that matters.** A restart holds the request open for `wakeHold` and then
// answers 202, so a failure arriving four minutes later reached nobody: the row
// went to `crashed`, an audit row said a person pressed Restart, and the
// reason — which `waitReady` had in its hand, naming the stage and the
// cluster's own words — was discarded. The owner then had `crashed` with no
// explanation, which is a smaller version of the `running` with no explanation
// that the health watcher's grace was added to fix.
//
// Both halves, because they are read in different places and by different
// people: the event is what `toft events` and the app's page show, and the
// narration is what the Logs tab holds — the only log there is when the
// container never started.
//
// **Once per transition, not once per attempt.** An app already `crashed` is
// the supervisor retrying on a backoff, and five identical rows over half an
// hour would bury the one that says what happened. The supervisor's own ending
// (`app_not_restarting`) is the last word on that sequence.
func (b *Builder) sayStartFailed(app *database.App, cause error) {
	b.narrator.For(app.ID, app.CurrentRelease, "").Sayf(applog.StreamStderr,
		"The workload did not come up: %v", cause)
	if app.Status == database.AppStatusCrashed {
		return
	}
	b.opts.Events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppCrashed,
		Severity: database.EventSeverityError,
		Source:   database.EventSourceHealth,
		Summary:  "The app could not be started",
		// The error carries the stage and, in isolated mode, the cluster's own
		// sentence — *persistentvolumeclaim "lpv-x" not found*, an image that
		// will not pull, a node with no room. That sentence exists nowhere else
		// a person can reach without kubectl.
		Detail:      cause.Error(),
		Fingerprint: app.CurrentRelease,
	})
}

// roleKeyFor is the key one shared workload verifies X-Toft-Role with.
//
// Shared mode only, and the asymmetry is the same one the app token has: an
// isolated workload is handed its key when its pod bootstraps, because that is
// the only moment the platform can give a pod a secret at all. Putting one in a
// pod spec would put it where anyone with get-pod in the namespace can read it.
//
// Empty when the install has no minter — a builder standing on its own in a
// test. The workload then reads every role assertion as `unknown`, which
// refuses.
func (b *Builder) roleKeyFor(appID, workload string, mode runtime.Mode) string {
	if b.opts.Roles == nil || mode != runtime.ModeShared {
		return ""
	}
	return b.opts.Roles.RoleKey(appID, workload)
}

// restoreCurrent brings a shared app's live release back from the artifact
// store when its directory is gone, and points `current` at it.
//
// An APPS_DIR that does not outlive the platform — a container's own layer, an
// emptyDir — is a legitimate place to keep releases once an artifact store
// holds the durable copy, and is much faster than a network filesystem for a
// tree of thousands of files. Without this, every platform restart left every
// shared app "release directory missing — redeploy the app" while the release
// it names sat in the store. With no store, or a row that names a local
// release, there is nothing to restore from and the old answer stands.
func (b *Builder) restoreCurrent(app *database.App) error {
	ref := runtime.ParseReleaseRef(app.ReleaseRef)
	deployID := ReleaseDeployID(app.ReleaseRef)
	if b.artifacts == nil || ref.Kind != runtime.ReleaseArtifact || deployID == "" {
		return errNoRelease
	}
	if err := EnsureAppDirs(b.opts.AppsDir, app.ID); err != nil {
		return err
	}
	dir := ReleaseDir(b.opts.AppsDir, app.ID, deployID)
	if err := b.restoreRelease(app.ID, deployID, dir,
		func(line string) { log.Printf("restore: %s: %s", app.Slug, strings.TrimSpace(line)) }); err != nil {
		if missingReleaseErr(err) {
			return errNoRelease
		}
		return fmt.Errorf("restoring %s from the artifact store: %w", deployID, err)
	}
	return linkCurrent(b.opts.AppsDir, app.ID, deployID)
}
