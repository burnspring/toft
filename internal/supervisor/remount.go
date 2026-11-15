package supervisor

import (
	"errors"
	"log"
	"sync"

	"toft/internal/applog"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
)

// Remounting: the second half of a revocation.
//
// ADR §12a decided, on purpose, that a mapping change takes effect on the next
// deployment and never on a running workload. That decision is unchanged, and
// it was correct when the only person who could write a mapping was the only
// person who could take it away. A delegation that can be *revoked* inherits a
// gap it never had to face: between the moment an administrator revokes and the
// moment somebody happens to deploy, the mount is still under a live process,
// and for a stable internal app that interval is months. So a revocation
// restarts what it took mounts away from.
//
// It lives here rather than in internal/api for one reason: this package
// already owns "start these apps, and record what failed". A handler owning a
// goroutine that outlives its own request would be the second place in the
// codebase that knows how to do that, and the second place is where the two
// disagree.
//
// It is **not** the recovery loop and shares none of its state. There is no
// backoff, no attempt counter and no ending: one attempt per app. An app that
// does not come up has its old workload **halted** — a start can fail long
// before it replaces anything, and the old process would otherwise keep the
// access the restart existed to remove — and is written `crashed`. An isolated
// one is then handed to the recovery loop with Notice, which spends that
// loop's own five attempts, not this one's; a shared one stays `crashed`, with
// no workload, until a person restarts or deploys it.

// RestartTarget is one app a permission change has to reach: the two fields
// this package actually uses, rather than the shape of whichever permission
// changed.
//
// It was `[]database.StorageManagedApp` while storage was the only caller, and
// that was honest then. A second caller — a credential detached from an app —
// asks for exactly the same thing on a different subject, and threading a
// storage type through it would make every later caller either convert into one
// or add a third parameter type to a mechanism that needs neither.
type RestartTarget struct {
	ID   string
	Slug string
}

// There is no scope. **Every permission Remount takes away is live under a
// workload in either mode**: a credential is in the environment of both, and
// storage is mounted in both — through a CSI driver in isolated mode, and as a
// gated directory on a host root in shared mode, where the gate is a
// supplementary group read only at spawn. There used to be a ScopeIsolatedOnly
// for storage, resting on "storage in shared mode is refused at deploy time",
// which stopped being true when roots arrived; it made every shared app skip a
// storage revocation with no error, the permission reading as revoked while the
// process still held the group.

// maxRemountConcurrency bounds the fan-out. An isolated start is a pod
// schedule, an image pull and a readiness wait; a revocation touching thirty
// apps must not ask the cluster for thirty of those at once, and must not take
// thirty times the readiness timeout either.
const maxRemountConcurrency = 4

// Remount restarts every app in the list that is carrying a live mount, onto
// its **current release**.
//
// It returns immediately; the work runs on this package's wait group, so a
// platform shutdown waits for the starts in flight rather than abandoning a
// half-created workload.
//
// **A start, never a deploy.** A deploy re-clones at the branch head, so an
// administrator revoking a permission would otherwise ship whatever commits
// landed since the last release — a permission edit becoming a code release for
// somebody else's app, at a moment nobody chose. Builder.Start re-materialises
// what is already running and assembles the spec from the database at the
// moment of the call, mounts included, so the restarted workload carries the
// new mount list by construction and nothing here touches a spec.
func (s *Supervisor) Remount(apps []RestartTarget, reason string) {
	if s == nil || len(apps) == 0 {
		return
	}
	// Nothing is started after Close. The revocation itself already committed,
	// so those apps come back without the mount at boot Reconcile — which
	// assembles the spec from the rows as they are then.
	select {
	case <-s.stop:
		return
	default:
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.remount(apps, reason)
	}()
}

func (s *Supervisor) remount(apps []RestartTarget, reason string) {
	sem := make(chan struct{}, maxRemountConcurrency)
	var wg sync.WaitGroup
	// Deferred rather than called at the end, so a shutdown mid-fan-out still
	// waits for the starts already in flight: abandoning one leaves a workload
	// half-created, which is the state nothing on the way back up expects.
	defer wg.Wait()

	for _, target := range apps {
		select {
		case <-s.stop:
			// The platform is going down. What is left will come back without
			// the mount at boot Reconcile, which reads the mappings as they are
			// now — the rows committed before any of this started.
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(target RestartTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			s.remountOne(target, reason)
		}(target)
	}
}

// remountOne is one app, re-read from the database rather than trusted from the
// list: the revocation committed some time ago by now, and a person may have
// stopped, locked or deployed the app in between.
func (s *Supervisor) remountOne(target RestartTarget, reason string) {
	app, err := s.store.GetApp(target.ID)
	if err != nil {
		log.Printf("remount: reading app %s: %v", target.Slug, err)
		return
	}
	if app == nil {
		return
	}
	if !remountEligible(app) {
		log.Printf("remount: %s is %s in %s mode — nothing to restart for this change",
			app.Slug, app.Status, app.RunningModeOrShared())
		return
	}
	// A deploy in flight checks the mappings itself at its flip, against the
	// table as it is then, and retires its new workload if a mount it was
	// started with has no row any more (builder: mountsWithoutRows). Racing
	// it with a restart is how two workloads end up under one name.
	pending, err := s.store.HasPendingDeployment(app.ID, s.now().Add(-pendingWindow))
	if err != nil {
		log.Printf("remount: %s: checking pending deployments: %v", app.Slug, err)
		return
	}
	if pending {
		log.Printf("remount: %s has a deploy in flight, which will pick the mounts up itself", app.Slug)
		return
	}

	log.Printf("remount: restarting %s onto release %s (%s)", app.Slug, app.CurrentRelease, reason)
	// The one permission change that restarts a workload, said on the workload
	// it restarted. Without this an owner sees their app bounce for no reason
	// they can find — the administrator's action is in the audit log, which
	// they cannot read.
	//
	// `reason` is the administrator's own words about the grant, composed by
	// the platform and already shown in the confirmation before the click.
	s.narrator.For(app.ID, app.ProcessName(), "").Sayf(applog.StreamStdout,
		"Restarting: %s.", reason)
	err = s.starter.Start(app)
	switch {
	case err == nil:
		// The row said `running` when this began and a successful start leaves
		// it there, so there is nothing to write — except from `crashed`, which
		// the watcher may have written while the restart waited on readiness. A
		// Stop or a lock made in that window wrote its own word, and the
		// workload this start placed is stopped again rather than left live
		// under it.
		if moved, werr := s.store.UpdateAppStatusFrom(app.ID, database.AppStatusCrashed, database.AppStatusRunning); werr != nil {
			log.Printf("remount: marking %s running: %v", app.Slug, werr)
		} else if !moved {
			s.starter.StopIfNoLongerWanted(app.ID)
		}
		return
	case errors.Is(err, runtime.ErrStartSuperseded), errors.Is(err, runtime.ErrAppLocked):
		// Nothing failed. A deploy flipped first, and its new workload was
		// started with the mappings as they are now; or a lock landed, whose
		// own stop took the workload down. Either way the status is someone
		// else's to write.
		log.Printf("remount: %s: %v — nothing to restart", app.Slug, err)
		return
	}

	// The revocation stands. This is the accepted cost of "revoked means
	// unmounted", and it is in the confirmation copy before the click rather
	// than discovered after it: an administrator's permission change can leave
	// somebody's app crashed.
	//
	// The old workload is halted first. The start may have failed before it
	// replaced anything — restoring the release, reading mounts, decrypting
	// variables, an unreachable agent — and the old process would then go on
	// holding the group, mount or credential being revoked, and be healed back
	// to `running` by the health watcher within a minute.
	log.Printf("remount: restarting %s failed: %v", app.Slug, err)
	if herr := s.starter.Halt(app); herr != nil {
		log.Printf("remount: halting %s after its failed restart: %v", app.Slug, herr)
	}
	// From running, which is what remountEligible saw: a Stop made while the
	// start was in flight stands, and is not turned into a crash that boot
	// Reconcile would then restart.
	moved, werr := s.store.UpdateAppStatusFrom(app.ID, database.AppStatusRunning, database.AppStatusCrashed)
	if werr != nil {
		log.Printf("remount: marking %s crashed: %v", app.Slug, werr)
		return
	}
	if !moved {
		log.Printf("remount: %s was stopped or locked while it restarted; leaving the row as it is", app.Slug)
		return
	}
	if app.RunningModeOrShared() != runtime.ModeIsolated {
		// Halted, and nothing will restart it: the interstitial must not
		// promise a refresh.
		s.markHalted(app.ID, app.CurrentRelease)
	}
	s.narrator.For(app.ID, app.ProcessName(), "").Say(applog.StreamStderr,
		"The app did not come back up after that change, and is marked crashed.")
	s.events.Record(events.Event{
		AppID:    app.ID,
		Kind:     events.KindAppCrashed,
		Severity: database.EventSeverityError,
		Source:   database.EventSourceHealth,
		Summary:  "This app did not come back after a storage change",
		Detail: "A platform administrator revoked a storage delegation this app was mounting, " +
			"so the app was restarted onto its current release without that mount and did not " +
			"come up. The app's logs will say why. Nothing was rebuilt and no code changed.",
		Fingerprint: app.CurrentRelease + ":remount",
	})
	// The recovery loop takes an isolated app from here; Notice is otherwise
	// called only by the watcher's own running→crashed transition, which a row
	// already written `crashed` never makes.
	if app.RunningModeOrShared() == runtime.ModeIsolated {
		app.Status = database.AppStatusCrashed
		s.Notice(app)
	}
}

// remountEligible is the D7 table, and each exclusion has its own reason:
//
//   - **`running`** is the whole list. That is the only state in which a
//     permission is under a live process.
//   - **`stopped`.** Not running. Nothing is mounted, and starting it would
//     argue with a person's decision.
//   - **Locked or retired.** Not running, and **must not be started**:
//     unlocking never starts a workload, and a revocation that did would be an
//     eighth lock enforcement point wired backwards.
//   - **`crashed`.** Not running. The recovery loop reads the new mounts when
//     it next attempts, because `Start` is how it attempts.
//   - **`sleeping`.** Shared mode only.
//   - **No current release.** There is nothing to start.
//   - **Isolated with no published release reference.** An isolated workload
//     fetches its release from the artifact store, so without a reference there
//     is nothing to start. A shared one starts from a directory on this host,
//     and on an install with no artifact store no app carries a reference at
//     all — requiring one there made a revocation a silent no-op on exactly
//     those installs.
//
// `building` is the one exclusion this cannot see from the row, and it is
// checked against the deployments table by the caller above.
func remountEligible(app *database.App) bool {
	if app.Locked() {
		return false
	}
	if app.Status != database.AppStatusRunning {
		return false
	}
	if app.CurrentRelease == "" {
		return false
	}
	if app.RunningModeOrShared() == runtime.ModeIsolated && app.ReleaseRef == "" {
		return false
	}
	return true
}
