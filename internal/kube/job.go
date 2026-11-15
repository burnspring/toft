package kube

import (
	"context"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"toft/internal/runtime"
)

// One-shot work runs as a Kubernetes Job, not a bare pod: a Job survives node
// eviction and carries its own completion semantics, which is exactly the
// difference between "has an exit code" and "is restarted".
//
// JobBackend is a distinct type rather than more methods on Backend, and that
// is not merely a naming convenience: both lifecycles have a Logs, and they
// take different arguments because one addresses a long-lived workload and the
// other a finished run. Forcing them onto one type is the same mistake as
// forcing them into one interface — the collision is the design saying so.
// They share every internal (client, namespace, images, bootstrap minter),
// which is the part that was worth sharing.
type JobBackend struct{ *Backend }

// Jobs returns the one-shot lifecycle over the same cluster and configuration.
func (b *Backend) Jobs() *JobBackend { return &JobBackend{b} }

// jobTTL is how long a finished Job's objects linger. Long enough for the
// dispatcher's sweep to observe a completion it missed, short enough that a
// busy namespace is not full of dead Jobs.
const jobTTL int32 = 3600

// jobDeadlineMargin is how far past a run's timeout the Job's own
// activeDeadlineSeconds sits: past the platform's timer and its sweep, so the
// platform's timeout is the one recorded whenever the platform is up.
const jobDeadlineMargin = 5 * time.Minute

// Start places a one-shot workload and returns the handle used to ask about it
// later. That handle is persisted, which is what lets the dispatcher re-adopt
// running work after a platform restart instead of trusting in-memory state.
func (b *JobBackend) Start(ctx context.Context, spec runtime.WorkloadSpec) (string, error) {
	runID := spec.Name
	name := objectName("job-" + spec.ProcessName())

	podSpec, err := b.podSpec(ctx, spec, "job", runID)
	if err != nil {
		return "", err
	}

	labels := b.labelsFor(spec, runID)
	ttl := jobTTL
	// Retries are the queue's business, not the kubelet's: a Job that retried
	// itself would run again outside the platform's own accounting, so a run
	// that failed would silently cost twice what its record says.
	backoff := int32(0)

	// The cluster's own bound on the run, a margin past the platform's stored
	// deadline: the platform's timeout is the one that normally fires and is
	// recorded, and this one keeps a run bounded while the platform is down or
	// its stop did not land.
	var deadline *int64
	if spec.Timeout > 0 {
		d := int64((spec.Timeout + jobDeadlineMargin).Seconds())
		deadline = &d
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.cfg.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: b.annotationsFor(spec)},
				Spec:       *podSpec,
			},
		},
	}

	// Before the Job, so its pod never exists without the egress block. The
	// per-workload policy Deploy writes selects a release name, and a job
	// pod's workload label is its run id, so this namespace-wide one is the
	// only egress rule that covers it.
	if err := b.ensureEgressPolicy(ctx); err != nil {
		return "", err
	}

	created, err := b.client.BatchV1().Jobs(b.cfg.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Idempotent: a dispatcher retrying after an ambiguous failure
			// must not start the work twice.
			return name, nil
		}
		return "", fmt.Errorf("kube: creating job %s: %w", name, err)
	}
	return created.Name, nil
}

// Status reports a run's state, mapping Kubernetes' vocabulary onto the
// platform's. The mapping preserves the distinctions that decide what a
// developer fixes: an out-of-memory kill is not a crash, and an image that
// will not pull is not the app's fault at all.
func (b *JobBackend) Status(ctx context.Context, handle string) (runtime.JobStatus, error) {
	job, err := b.client.BatchV1().Jobs(b.cfg.Namespace).Get(ctx, handle, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The Job's TTL has expired, or it never existed. Either way the
			// platform's own record is now authoritative — reporting a guess
			// here would let a completed run be re-marked from thin air.
			return runtime.JobStatus{}, fmt.Errorf("kube: run %s: %w", handle, runtime.ErrJobNotFound)
		}
		return runtime.JobStatus{}, err
	}

	status := runtime.JobStatus{State: runtime.JobPending}
	if job.Status.StartTime != nil {
		status.StartedAt = job.Status.StartTime.Time
	}

	switch {
	case job.Status.Succeeded > 0:
		status.State = runtime.JobSucceeded
		if job.Status.CompletionTime != nil {
			status.EndedAt = job.Status.CompletionTime.Time
		}
		return status, nil
	case job.Status.Active > 0:
		status.State = runtime.JobRunning
	}

	// The Job object says "failed" without saying why; the pod does. Reading
	// it is what turns a bare failure into an attributable one.
	//
	// A list that fails is returned, not swallowed: a run is made terminal
	// once, so a failed read on the deciding poll used to fix the run as the
	// app's own failure with exit 0 for good. The caller leaves the run open
	// on an error and asks again (K8-6).
	pods, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + handle,
	})
	if err != nil {
		return runtime.JobStatus{}, fmt.Errorf("kube: listing the pods of run %s: %w", handle, err)
	}
	for i := range pods.Items {
		state, stage, code, msg := classify(&pods.Items[i])
		if state.Terminal() || state == runtime.JobRunning {
			status.State, status.Stage = state, stage
			status.ExitCode, status.Message = code, msg
			if state.Terminal() {
				status.EndedAt = time.Now().UTC()
			}
			return status, nil
		}
		if stage != runtime.FailureNone {
			status.Stage, status.Message = stage, msg
		}
	}

	if job.Status.Failed > 0 && !status.State.Terminal() {
		if len(pods.Items) == 0 {
			// Failed with no pod left to say why: a node drain, a preemption,
			// an eviction, or the Job's own deadline. Nothing the app did is
			// known, so it is not blamed on the app with an exit code it
			// never returned.
			status.State = runtime.JobInfrastructureErr
			status.Stage = runtime.FailureNone
			status.Message = jobFailedMessage(job)
			status.EndedAt = time.Now().UTC()
			return status, nil
		}
		status.State = runtime.JobFailed
		if status.Stage == runtime.FailureNone {
			status.Stage = runtime.FailureExec
		}
	}
	return status, nil
}

// jobFailedMessage is the Job's own Failed condition, in its words.
func jobFailedMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Message
			}
			if c.Reason != "" {
				return c.Reason
			}
		}
	}
	return "the job failed and its pod is gone"
}

func (b *JobBackend) Cancel(ctx context.Context, handle string) error {
	// Foreground propagation, so the pods go with the Job rather than being
	// orphaned to run on unattached — a cancelled job that keeps consuming
	// capacity is the worst of both outcomes.
	policy := metav1.DeletePropagationForeground
	err := b.client.BatchV1().Jobs(b.cfg.Namespace).Delete(ctx, handle, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: cancelling run %s: %w", handle, err)
	}
	return nil
}

// Logs returns a run's output from the cluster. This is the operator's debug
// view; the platform's own record comes over the callback channel, which
// behaves identically across both isolated backends and survives the Job's TTL
// expiring.
func (b *JobBackend) Logs(ctx context.Context, handle string) (io.ReadCloser, error) {
	pods, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + handle,
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("kube: no pod for run %s", handle)
	}
	return b.client.CoreV1().Pods(b.cfg.Namespace).
		GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).Stream(ctx)
}
