package builder

import (
	"fmt"

	"toft/internal/database"
	"toft/internal/runtime"
)

// Where a release runs, and what it can reach — settled before a deploy does
// any work.
//
// Execution mode used to be a property of the *app*, read at whatever moment
// something happened to look. Making it a property of the *deployment* is what
// lets an administrator change the desired mode freely, in either direction,
// without disturbing anything running: the change takes effect on the next
// deployment and never on a live workload.

// resolveTarget decides the mode this release will run under and collects the
// storage it may reach, refusing any combination that cannot work.
//
// The refusals happen here, before the clone, for the reason the whole deploy
// pipeline is ordered the way it is: a failed deploy must cost nothing but a
// queue slot, and the running release must be untouched by it.
func (b *Builder) resolveTarget(app *database.App, job *BuildJob, appendLog func(string)) (runtime.Mode, []runtime.Mount, error) {
	mode := app.DesiredMode()

	// A rollback restarts an earlier release's directory exactly as it was
	// built, so it inherits the mode that release ran under. Anything else
	// would be a silent rebuild-free mode change: a natively built tree started
	// inside a runner image, or the reverse.
	if job.SourceDeployID != "" {
		source, err := b.db.GetDeployment(job.SourceDeployID)
		if err != nil {
			return "", nil, fmt.Errorf("reading the deployment being rolled back to: %w", err)
		}
		if source == nil {
			return "", nil, fmt.Errorf("that deployment no longer exists")
		}
		inherited, err := runtime.ParseMode(source.ExecutionMode)
		if err != nil {
			return "", nil, fmt.Errorf("that deployment records an unreadable execution mode %q", source.ExecutionMode)
		}
		if inherited != mode {
			// Inheriting is consistent — the release ran that way before — but
			// it must not quietly resurrect a mode the app has since left. The
			// admin gets to see that the rollback is also a mode change, and
			// the validation below still applies to the inherited mode, so an
			// isolated release cannot come back on an install that no longer
			// offers isolation.
			appendLog(fmt.Sprintf(
				"==> This release was built to run %s; the app is now set to %s. "+
					"Rolling back restores the release as it was built, so it starts %s.",
				inherited, mode, inherited))
		}
		mode = inherited
	}

	mounts, err := b.db.AppMounts(app.ID)
	if err != nil {
		return "", nil, fmt.Errorf("reading this app's storage mappings: %w", err)
	}

	// Storage with nowhere to put it is a deploy-time failure, not a degraded
	// mode. Background jobs already refuse shared mode on the same principle: a
	// capability that needs a boundary the install does not have is refused with
	// the reason, never half-provided.
	//
	// **What is refused is no longer the mode.** Shared mode gained durable
	// storage under a root the operator mounted, so the condition is whether
	// this install declares one — and the backend answers that question rather
	// than this package guessing at it, because the two refusals must not be
	// able to disagree.
	//
	// The backend refuses this too. Refusing here as well is not redundancy for
	// its own sake — this is the message a developer reads, and it is worth
	// more than a start failure three steps later, after a clone and a build.
	if mode != runtime.ModeIsolated {
		if refused := b.rts.UnplaceableSharedMounts(mounts); len(refused) > 0 {
			return "", nil, storageRequiresIsolation(refused)
		}
	}

	if len(mounts) > 0 {
		for _, m := range mounts {
			appendLog(fmt.Sprintf("==> Storage: %s (%s, %s) at %s", m.Name, m.Kind, m.Access, m.Path))
		}
	}
	return mode, mounts, nil
}

// bindAddressFor is the interface a workload in this mode is told to listen on.
//
// A shared workload runs inside Toft's own network namespace, where a port
// on all interfaces is reachable without passing the authorizing gateway. An
// isolated one has a namespace to itself, and the gateway is the only route in.
func bindAddressFor(mode runtime.Mode) string {
	if mode == runtime.ModeIsolated {
		return runtime.BindAll
	}
	return runtime.BindLoopback
}

// storageRequiresIsolation is the refusal an app with mappings gets when its
// next deployment would be a shared process.
func storageRequiresIsolation(mounts []runtime.Mount) error {
	names := ""
	for i, m := range mounts {
		if i > 0 {
			names += ", "
		}
		names += m.Name
	}
	return fmt.Errorf("this app has storage mapped (%s) but is set to shared mode, and this "+
		"install has nowhere to attach it: shared-mode storage lives under a root an operator "+
		"mounted on the host, and none is declared. An operator can declare one "+
		"(SHARED_STORAGE_ROOTS), or an administrator can promote the app to isolated mode or "+
		"remove its storage mappings; nothing running is affected either way", names)
}
