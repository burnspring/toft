package runtime

import "fmt"

// Execution mode is the density knob, and it is deliberately the *only*
// vocabulary a developer, the API, or an app row ever sees.
//
// An app is shared or isolated. Whether isolated means a Fargate task or a pod
// is the operator's infrastructure choice: storing the backend instead of the
// mode would mean an install that changes cluster technology has to rewrite
// every app row, and would put a cloud vendor's name in a developer-facing
// API for no benefit.
type Mode string

const (
	// ModeShared runs an app as a process inside Toft's own footprint.
	// The default everywhere, and available in every topology: the common case
	// is many small, low-traffic internal tools, and they should be dense.
	//
	// Its isolation properties are exactly today's — which is to say, app↔app
	// isolation is an accepted risk. Choosing density is not choosing
	// isolation, and this constant is not a weaker tier of the next one.
	ModeShared Mode = "shared"

	// ModeIsolated runs an app as its own workload — a task, a pod. It gives
	// the app its own network namespace, identity, and resource envelope, at
	// the cost of per-workload overhead and a cold start measured in tens of
	// seconds.
	//
	// It is a trade, not an upgrade.
	//
	// It is *not* the tier that unlocks background work. Every job run is an
	// isolated workload whatever mode its app is in — that is the whole point of
	// moving twelve minutes and six gigabytes out of shared process space — so a
	// shared-mode app starts jobs on exactly the same terms. Only placement of
	// the app's own service differs, and placement is not app-visible.
	ModeIsolated Mode = "isolated"
)

func (m Mode) Valid() bool { return m == ModeShared || m == ModeIsolated }

// ParseMode reads a stored or submitted mode. Empty means shared, so rows
// written before modes existed need no backfill and no migration guesswork.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeShared:
		return ModeShared, nil
	case ModeIsolated:
		return ModeIsolated, nil
	default:
		return "", fmt.Errorf("unknown execution mode %q (want %q or %q)", s, ModeShared, ModeIsolated)
	}
}

// Topology is what the install actually is, and it decides which modes are on
// offer. It is derived from configuration rather than stored: absent
// configuration means a capability is simply not offered, so an on-prem
// install has no dormant cloud code paths.
type Topology string

const (
	// TopologyVM is a single machine running the binary. Shared only, and
	// permanently so — there is no cluster to place a workload into. This is
	// the one topology with a hard ceiling rather than an upgrade path, and
	// saying so plainly is better than letting an operator discover it.
	TopologyVM Topology = "vm"
	// TopologyECS is an ECS install: an EC2 task carries Toft and every
	// shared app, and elastic work runs as Fargate tasks in the same cluster.
	// Isolated mode is an in-place upgrade, not a migration.
	TopologyECS Topology = "ecs"
	// TopologyKubernetes runs Toft as a workload in the cluster it
	// orchestrates.
	TopologyKubernetes Topology = "kubernetes"
)

// Capabilities describes what an install offers, and is consulted wherever a
// mode is chosen or a job is enqueued so that "this install cannot do that" is
// one answer given in one place.
//
// It is the *conjunction* of two independent facts, and keeping them separate
// is what makes the answers useful:
//
//	Possible — is there somewhere to put an isolated workload? An operator
//	           decides this by configuring a cluster. It cannot be turned on
//	           from inside the product.
//	Enabled  — should it be offered? An administrator decides this at runtime,
//	           reversibly, and the decision is audited.
//
// Both must hold. An admin cannot enable something the install cannot do, and
// an operator who configures a cluster does not thereby force the capability
// on — which matters because enabling it requires a one-time rebuild of every
// app, and that must be a planned window rather than a side effect of a
// deployment change.
type Capabilities struct {
	Topology Topology
	// IsolatedPossible is whether the infrastructure exists: a cluster is
	// configured, an artifact store is configured, runner images are named.
	IsolatedPossible bool
	// IsolatedEnabled is whether an administrator has turned it on.
	IsolatedEnabled bool
	// JobsEnabled gates background jobs, on by default. Jobs do not need
	// isolated mode: a run is placed on the cluster where isolated execution
	// is available and on this machine otherwise (ADR §106).
	JobsEnabled bool
	// HostJobsPossible is whether a run can be placed on the platform's own
	// machine (ADR §106) — the backend an install without a cluster has.
	HostJobsPossible bool
}

// IsolatedAvailable reports whether isolated mode can actually be selected.
func (c Capabilities) IsolatedAvailable() bool {
	return c.IsolatedPossible && c.IsolatedEnabled
}

// VMCapabilities is what a single-machine install offers before anything else
// is known: shared mode and no isolated mode, permanently. Whether it runs jobs
// is the job service's half (HostJobsPossible) — runs go on this machine.
func VMCapabilities() Capabilities {
	return Capabilities{Topology: TopologyVM}
}

// Allows reports whether a mode can be selected on this install.
func (c Capabilities) Allows(m Mode) bool {
	switch m {
	case ModeShared:
		return true // every topology, always
	case ModeIsolated:
		return c.IsolatedAvailable()
	default:
		return false
	}
}

// BackgroundJobsAvailable reports whether background work can run here: jobs
// are switched on, and there is somewhere to place a run — a pod where isolated
// execution is available, this machine otherwise (ADR §106). It asks nothing
// about any particular app: this was always a question about the install.
func (c Capabilities) BackgroundJobsAvailable() bool {
	return c.JobsEnabled && (c.IsolatedAvailable() || c.HostJobsPossible)
}

// JobPlacement is where a run is placed, decided once for the install rather
// than per app: isolated wherever isolated execution is available, which keeps
// every install that ran jobs before §106 placing them exactly as it did, and
// this machine otherwise. The app's own mode is not consulted — what a run
// actually is decides how its cost is classified.
func (c Capabilities) JobPlacement() Mode {
	if c.IsolatedAvailable() {
		return ModeIsolated
	}
	return ModeShared
}

// ExplainUnavailable returns why a mode cannot be selected, phrased so the
// reader learns what would change it rather than only that it failed. An empty
// string means the mode is available.
//
// The two halves get different answers on purpose. "No cluster" is something
// only an operator can fix, and on a VM cannot be fixed at all; "not enabled"
// is a switch an administrator can throw, and telling them that is the
// difference between a dead end and a next step.
func (c Capabilities) ExplainUnavailable(m Mode) string {
	if c.Allows(m) {
		return ""
	}
	if m != ModeIsolated {
		return fmt.Sprintf("%q is not an execution mode", m)
	}

	if !c.IsolatedPossible {
		switch c.Topology {
		case TopologyVM:
			return "Isolated mode needs a Kubernetes or ECS cluster, and this install runs on a " +
				"single machine with no upgrade path."
		default:
			return "This install has no isolated backend configured. An operator must set up a cluster " +
				"(KUBE_NAMESPACE or ECS_CLUSTER), an artifact store and a runner image."
		}
	}

	return "Isolated execution is turned off; an administrator can enable it under Admin → Settings. " +
		"The first time, every app must be rebuilt to run in the runner image."
}

// ExplainJobsUnavailable is the error an app gets when it tries to enqueue
// background work and cannot. It names the requirement and the remedy rather
// than failing generically, because the requirement is a decision *somebody*
// has to make and a bare 400 does not tell them there is one.
//
// Both remaining refusals are install-level: either there is nowhere to put an
// isolated workload, or an administrator has turned jobs off. Neither is about
// the app's own execution mode, and the mode argument survives only so the
// caller does not have to know that.
//
// It used to refuse a shared-mode app on the ground that "enqueueing work hands
// the app a credential that can run compute with the app's own environment, and
// that is only safe inside a workload with its own isolation boundary". That
// argument was already overruled by the app token, which is injected into
// shared-mode processes regardless — so keeping the refusal withheld no
// credential, only the one capability that would have put it to good use. The
// blast radius runs the other way besides: what the refusal forced was the app
// running six gigabytes of reconciliation *inside* the shared process, where a
// co-tenant already shares the address space. A job is the same app's code from
// the same release with a different entrypoint, moved into a sandbox with a
// memory limit, a deadline and a durable record. Starting one is a
// de-escalation. See docs/architecture-decisions.md §26.
//
// What never changed, and is the reason none of this needs a mode check: the
// *workload* is isolated in both modes. Only its placement differs, and
// placement was never app-visible.
func (c Capabilities) ExplainJobsUnavailable(m Mode) string {
	if c.BackgroundJobsAvailable() {
		return ""
	}
	if !c.IsolatedAvailable() && !c.HostJobsPossible {
		return "Background jobs need somewhere to run, which this install does not offer: " +
			"it has neither isolated execution nor a shared runtime to run them on this machine. " +
			c.ExplainUnavailable(ModeIsolated)
	}
	return "Background jobs are turned off for this platform. An administrator can enable " +
		"them under Admin → Settings."
}
