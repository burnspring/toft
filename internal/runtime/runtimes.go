package runtime

import (
	"fmt"
	"maps"
	"time"
)

// Runtimes is how a caller with an app in hand reaches the backend that owns
// its workload.
//
// Before this existed the platform held exactly one ServiceRuntime, so
// promoting an app changed a row and unlocked background jobs while the app
// itself kept running as a process beside every other shared app. Mode was
// real for jobs and cosmetic for services. This is the piece that makes it
// real for both.
//
// The lookup key is a *mode*, never a backend name — an install that changes
// cluster technology rewrites no app row, and nothing outside this file learns
// whether the isolated backend is a pod or a task.
type Runtimes struct {
	shared ServiceRuntime
	// isolated is nil on an install with no cluster. Nil is a supported state,
	// not a degraded one: a VM install has nowhere to put an isolated workload
	// and the capability is simply not offered.
	isolated ServiceRuntime
}

// NewRuntimes pairs the two backends. The shared one is required — every
// topology offers shared mode, and an install with no way to run an app at all
// is not a state worth expressing.
func NewRuntimes(shared, isolated ServiceRuntime) *Runtimes {
	return &Runtimes{shared: shared, isolated: isolated}
}

// UnplaceableSharedMounts is the mounts a shared workload of this app could not
// be given: the ones that need a root, on an install that declares none.
//
// It exists so the builder's refusal, which happens before the clone, and the
// backend's refusal, which happens at the start, cannot disagree about whether
// this install has anywhere to put a mount. A backend that cannot answer says
// none: offering a capability nobody confirmed is how a deploy succeeds and the
// bytes go nowhere. **It asks per mount**, because a store needs no root — and
// asking "is there a provisioner" instead would let a volume through the moment
// a provisioner existed for a store's sake.
func (r *Runtimes) UnplaceableSharedMounts(mounts []Mount) []Mount {
	if s, ok := r.shared.(interface{ RootsDeclared() bool }); ok && s.RootsDeclared() {
		return nil
	}
	var out []Mount
	for _, m := range mounts {
		if m.NeedsRoot() {
			out = append(out, m)
		}
	}
	return out
}

// ResolvedMounts is the paths a workload of this app in this mode is handed
// for its mounts. Isolated mounts are where the mapping says, inside the pod; a
// shared backend resolves them under the app's own directory, and only it knows
// where.
func (r *Runtimes) ResolvedMounts(mode Mode, appID string, mounts []Mount) []Mount {
	if mode == ModeIsolated {
		return mounts
	}
	if s, ok := r.shared.(interface {
		ResolvedMounts(appID string, mounts []Mount) []Mount
	}); ok {
		return s.ResolvedMounts(appID, mounts)
	}
	return mounts
}

// For returns the backend a workload in this mode runs on.
//
// The error names the missing capability rather than returning a nil that some
// caller dereferences three frames later: an app row can say `isolated` on an
// install whose cluster configuration was removed, and the deploy that
// discovers it must fail with an explanation.
func (r *Runtimes) For(mode Mode) (ServiceRuntime, error) {
	switch mode {
	case ModeIsolated:
		if r.isolated == nil {
			return nil, fmt.Errorf("this app is set to isolated mode, but this install has no " +
				"isolated backend configured — an operator must set a cluster (KUBE_NAMESPACE), " +
				"an artifact store, and at least one runner image, or the app must be returned " +
				"to shared mode")
		}
		return r.isolated, nil
	case ModeShared, "":
		return r.shared, nil
	default:
		return nil, fmt.Errorf("unknown execution mode %q", mode)
	}
}

// Shared is the backend every install has. Used where the question is
// genuinely about the machine Toft runs on — whole-host metrics, the
// sampling collector — rather than about one app.
func (r *Runtimes) Shared() ServiceRuntime { return r.shared }

// Isolated is the cluster backend, or nil.
func (r *Runtimes) Isolated() ServiceRuntime { return r.isolated }

// IsolatedConfigured reports whether an isolated backend exists to place
// workloads into. This is the operator's half of the capability; whether it is
// *offered* is an administrator's separate decision.
func (r *Runtimes) IsolatedConfigured() bool { return r != nil && r.isolated != nil }

// List merges both backends' views.
//
// Callers that show "every running workload" — the dashboard, the app list —
// must not have to know how many backends exist, and one that iterated only
// the shared runtime would render every isolated app as missing.
//
// A backend that fails is skipped rather than failing the whole listing: a
// cluster that is briefly unreachable should not blank the status of every
// shared app beside it.
// CollectOrphans removes workloads whose app has been deleted, and returns
// what it removed.
//
// Finding and deleting are one call because routing a workload back to the
// backend that owns it is this type's whole job: a caller handed a list of
// names would have to work that out again, and would get it wrong for exactly
// the workloads that matter — the ones with no app row left to read the mode
// off.
//
// `live` maps the app ids that currently exist to their current release, or
// to "" where that is not certain (see [OrphanFinder]). `grace` is how long a
// workload must have been up before its app's absence is allowed to mean
// anything: the caller's list of live apps is a snapshot, and an app created
// after that snapshot was taken would otherwise have its brand-new workload
// collected. A grace of minutes closes that window completely, and an orphan
// is never urgent.
//
// A backend that cannot name the app behind a workload is skipped rather than
// guessed at, and an error from one backend does not stop the other.
func (r *Runtimes) CollectOrphans(live map[string]string, grace time.Duration) ([]Orphan, error) {
	var removed []Orphan
	var firstErr error

	for _, rt := range []ServiceRuntime{r.shared, r.isolated} {
		if rt == nil {
			continue
		}
		finder, ok := rt.(OrphanFinder)
		if !ok {
			continue
		}
		found, err := finder.Orphans(live, grace)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, o := range found {
			if err := rt.Delete(o.Workload); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed = append(removed, o)
		}
	}
	return removed, firstErr
}

func (r *Runtimes) List() (map[string]*AppMetrics, error) {
	out := map[string]*AppMetrics{}
	var firstErr error

	for _, rt := range []ServiceRuntime{r.shared, r.isolated} {
		if rt == nil {
			continue
		}
		m, err := rt.List()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		maps.Copy(out, m)
	}
	// Something answered: report what there is. Nothing did: report why.
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// SystemMetrics describes the machine shared workloads run on.
//
// It asks the shared backend only, and that is the correct answer rather than a
// gap: "the machine the apps run on" is a real, finite, shared thing for the
// shared backend and meaningless for a workload with a node's worth of
// scheduler between it and any hardware.
func (r *Runtimes) SystemMetrics() (*SystemMetrics, error) {
	if r.shared == nil {
		return nil, ErrNotSupported
	}
	return r.shared.SystemMetrics()
}
