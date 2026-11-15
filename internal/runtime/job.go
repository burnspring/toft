package runtime

import (
	"context"
	"io"
	"time"
)

// One-shot work is a *separate* interface from ServiceRuntime, over the same
// WorkloadSpec.
//
// The temptation is to force one interface over both, and it is the wrong
// trade: a service has an endpoint and is restarted when it dies, a job has an
// exit code and is not, so a merged interface grows "not applicable here"
// branches in every backend. Sharing the *spec* is what actually pays — it is
// what stops the release-resolution and bootstrap machinery being written
// twice, and what lets two isolated backends differ only in their API calls.

// JobState is where a run has got to. The set is closed, and the distinctions
// in it are the ones a developer needs to fix the right problem.
type JobState string

const (
	// JobPending is accepted but not yet placed on capacity.
	JobPending JobState = "pending"
	JobRunning JobState = "running"
	// JobSucceeded is a clean exit with status 0.
	JobSucceeded JobState = "succeeded"
	// JobFailed is a non-zero exit: the app's own code failed.
	JobFailed JobState = "failed"
	// JobTimedOut and JobResourceExceeded are deliberately *not* JobFailed. A
	// job killed for exceeding its memory limit and a job that threw an
	// exception need different fixes, and collapsing them into one status
	// sends the developer to debug the wrong thing.
	JobTimedOut          JobState = "timed_out"
	JobResourceExceeded  JobState = "resource_exceeded"
	JobCancelled         JobState = "cancelled"
	JobInfrastructureErr JobState = "infrastructure_error"
)

// Terminal reports whether a run has finished and will not change again.
func (s JobState) Terminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobTimedOut, JobResourceExceeded, JobCancelled, JobInfrastructureErr:
		return true
	}
	return false
}

// FailureStage names where a workload died, and exists because collapsing
// these into a bare exit code moves debugging into the cloud console — which
// is what this whole design exists to prevent. An operator must be able to see
// *which* stage failed in Toft's own UI.
type FailureStage string

const (
	FailureNone FailureStage = ""
	// FailureImagePull is the runner image itself being unavailable: a
	// registry, mirror, or credential problem, and never the app's fault.
	FailureImagePull FailureStage = "image_pull"
	// FailureArtifactFetch is the release not arriving — an artifact store
	// problem, distinct from the release being genuinely absent.
	FailureArtifactFetch FailureStage = "artifact_fetch"
	// FailureBuild is the app's own build failing, and must carry the actual
	// build error rather than a generic backend failure.
	FailureBuild FailureStage = "build"
	// FailureExec is the app's own code failing to start or run.
	FailureExec FailureStage = "app_exec"
	// FailureCallbackUnreachable is the runner being unable to reach the
	// platform. It must fail loudly rather than run on silently, so the
	// reconciler's sweep catches the orphan instead of a workload consuming
	// capacity that nothing knows about.
	FailureCallbackUnreachable FailureStage = "callback_unreachable"
)

// JobStatus is one run's state, as the backend sees it.
type JobStatus struct {
	State JobState
	// ExitCode is meaningful only once State is terminal.
	ExitCode int
	// Stage names where a failed run died. FailureNone on success.
	Stage FailureStage
	// Message carries the underlying error, including the app's own build or
	// runtime error where there is one.
	Message   string
	StartedAt time.Time
	EndedAt   time.Time
	// PeakMemoryMB is the most memory the backend saw the run hold, 0 when it
	// measures none. The host backend's own figure, because a host run with no
	// cgroup of its own has nothing the runner could read that is the run's.
	PeakMemoryMB int
}

// JobRuntime is the one-shot lifecycle. A backend implements it alongside
// ServiceRuntime when its topology supports background work; a VM install has
// no implementation at all, which is the shape "not offered" should take.
type JobRuntime interface {
	// Start places a one-shot workload and returns the backend's handle for
	// it. The spec is the same one a service consumes, so a job runs the same
	// release, with the same environment and identity, as the app it belongs
	// to — which is the whole premise: same repo, same environment, same
	// identity, no second packaging story.
	Start(ctx context.Context, spec WorkloadSpec) (handle string, err error)
	Status(ctx context.Context, handle string) (JobStatus, error)
	Cancel(ctx context.Context, handle string) error
	Logs(ctx context.Context, handle string) (io.ReadCloser, error)
}
