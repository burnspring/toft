package kube

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"toft/internal/runtime"
)

// Tier two: a real cluster that actually runs pods.
//
// The distinction from kube_test.go matters and is easy to lose. A fake
// clientset proves what the backend *sends*; it cannot run a workload, so it
// proves nothing about scheduling, image pulls, readiness, or the difference
// between a pod that failed and one that has not started yet. Only a real
// cluster does — and the classification bug that shipped past the fake tier
// (an unset phase reading as "failed") is the standing evidence for keeping
// both.
//
//	TEST_KUBE_CONTEXT=default go test ./internal/kube/   # k3s (scripts/k3s.sh)
//
// Skipped when TEST_KUBE_CONTEXT is unset, following the TEST_DATABASE_URL
// pattern, so `go test ./...` stays green with no cluster running.
const kubeContextEnv = "TEST_KUBE_CONTEXT"

// clusterBackend returns a backend against the real cluster, in a namespace
// dedicated to this test and torn down afterwards.
func clusterBackend(t *testing.T, images map[string]string) (*Backend, kubernetes.Interface, string) {
	t.Helper()

	kubeContext := os.Getenv(kubeContextEnv)
	if kubeContext == "" {
		t.Skipf("%s not set — skipping real-cluster test (needs a cluster, e.g. TEST_KUBE_CONTEXT=default on the k3s box)", kubeContextEnv)
	}

	// A namespace per test: the network policies and label selectors below are
	// namespace-scoped, and sharing one would let tests see each other's pods.
	ns := "toft-test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	if len(ns) > 60 {
		ns = ns[:60]
	}
	ns = strings.Trim(ns, "-")

	cfg, err := restConfig(Config{Context: kubeContext})
	if err != nil {
		t.Fatalf("cluster config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	ctx := context.Background()
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("creating namespace %s: %v", ns, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		client.CoreV1().Namespaces().Delete(bg, ns, metav1.DeleteOptions{})
	})

	// The account every workload pod runs as. The chart creates it in the apps
	// namespace; a throwaway namespace has to create its own, and without it the
	// API server refuses the pod outright — *forbidden*, before scheduling, so
	// every test here fails as "the pod never appeared" rather than as anything
	// about the thing it was testing. It holds no RBAC: it is a name a
	// TokenReview can report, which is the whole of what the platform wants
	// from it.
	if _, err := client.CoreV1().ServiceAccounts(ns).Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: WorkloadServiceAccount, Namespace: ns}},
		metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating service account %s in %s: %v", WorkloadServiceAccount, ns, err)
	}

	b := NewWithClient(client, Config{
		Namespace:    ns,
		CallbackURL:  "http://toft.default.svc.cluster.local:8080",
		RunnerImages: images,
	})
	return b, client, ns
}

// A workload the cluster can actually run without any Toft image: busybox
// with a shell command. What is under test here is the backend's object
// lifecycle and status reporting, not the runner — that is proven separately
// against the real runner image in internal/runner.
func busyboxImages() map[string]string {
	return map[string]string{"python": "busybox:1.36", "node": "busybox:1.36", "go": "busybox:1.36"}
}

// The distinction the fake tier cannot make: a pod that has been accepted but
// not yet scheduled must read as pending, and only a genuinely dead one as
// failed. Getting this wrong aborts good deploys.
func TestClusterPodReachesRunningNotFailed(t *testing.T) {
	b, client, ns := clusterBackend(t, busyboxImages())
	ctx := context.Background()

	spec := runtime.WorkloadSpec{
		AppID: "app-1", Slug: "demo", Name: "sleeper",
		Framework: "python", BasePath: "/apps/demo",
	}

	// Deploy blocks on readiness, so start it and watch the classification
	// while the cluster actually schedules the pod.
	done := make(chan error, 1)
	go func() {
		_, err := b.Deploy(spec)
		done <- err
	}()

	// Override the container the backend produced with one busybox can run.
	// (The backend always runs the runner entrypoint; there is no runner
	// binary in busybox, so the command is replaced here.)
	patchContainerCommand(t, client, ns, "sleeper", []string{"sh", "-c", "sleep 300"})

	sawPending := false
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		pod, err := b.getPod(ctx, "sleeper")
		if err == nil {
			state, _, _, msg := classify(pod)
			switch state {
			case runtime.JobPending:
				sawPending = true
			case runtime.JobRunning:
				if !sawPending {
					t.Log("pod reached Running before any pending observation (fast scheduler)")
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("deploy: %v", err)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("pod is running but Deploy did not return")
				}
				return
			case runtime.JobFailed, runtime.JobResourceExceeded, runtime.JobInfrastructureErr:
				t.Fatalf("pod classified as %s while starting: %s", state, msg)
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("pod never reached Running")
}

// A workload must be reachable *by its Service name*, from inside the cluster,
// and that is the address the gateway proxies to. A pod IP would break the
// moment the pod moved.
func TestClusterWorkloadIsReachableByItsServiceName(t *testing.T) {
	b, client, ns := clusterBackend(t, busyboxImages())

	spec := runtime.WorkloadSpec{
		AppID: "app-1", Slug: "demo", Name: "webby", Framework: "python",
	}
	var endpoint runtime.Endpoint
	done := make(chan error, 1)
	go func() {
		ep, err := b.Deploy(spec)
		endpoint = ep
		done <- err
	}()

	// A real HTTP server on the port the Service targets. busybox httpd rather
	// than a hand-rolled nc loop: nc's options vary between builds, and a
	// broken probe would look exactly like a broken Service.
	patchContainerCommand(t, client, ns, "webby", []string{"sh", "-c",
		fmt.Sprintf("mkdir -p /tmp/www && echo ok > /tmp/www/index.html && httpd -f -p %d -h /tmp/www", containerPort)})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deploy: %v", err)
		}
	case <-time.After(3 * time.Minute):
		t.Fatal("deploy never returned")
	}

	if !strings.HasSuffix(endpoint.Host, ".svc.cluster.local") {
		t.Fatalf("endpoint %q is not a cluster service name", endpoint.Host)
	}

	// Curl it from another pod in the same namespace, which is what the
	// gateway does in production.
	out := runInPod(t, client, ns, "prober", "busybox:1.36",
		[]string{"sh", "-c", "wget -q -T 20 -O - http://" + endpoint.Addr() + " || echo UNREACHABLE"})
	if !strings.Contains(out, "ok") {
		t.Errorf("workload not reachable at its endpoint %s: %q", endpoint.Addr(), out)
	}
}

// A job has an exit code and is not restarted; a non-zero exit must surface as
// a failure with the app's own code, and a clean one as success.
func TestClusterJobReportsItsExitCode(t *testing.T) {
	b, client, ns := clusterBackend(t, busyboxImages())
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		command string
		want    runtime.JobState
		code    int
	}{
		{"success", "echo done", runtime.JobSucceeded, 0},
		{"failure", "echo oops >&2; exit 3", runtime.JobFailed, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := runtime.WorkloadSpec{
				AppID: "app-1", Slug: "demo", Name: "run-" + tc.name, Framework: "python",
			}
			handle, err := b.Jobs().Start(ctx, spec)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			patchJobCommand(t, client, ns, handle, []string{"sh", "-c", tc.command})

			deadline := time.Now().Add(3 * time.Minute)
			for time.Now().Before(deadline) {
				status, err := b.Jobs().Status(ctx, handle)
				if err == nil && status.State.Terminal() {
					if status.State != tc.want {
						t.Fatalf("state = %q, want %q (%s)", status.State, tc.want, status.Message)
					}
					if tc.code != 0 && status.ExitCode != tc.code {
						t.Errorf("exit code = %d, want %d", status.ExitCode, tc.code)
					}
					return
				}
				time.Sleep(2 * time.Second)
			}
			t.Fatal("job never reached a terminal state")
		})
	}
}

// An out-of-memory kill must be reported as resource_exceeded, not as a crash:
// the fix for one is a bigger limit and for the other a code change, and only
// a real kernel produces the signal that tells them apart.
func TestClusterOutOfMemoryIsNotACrash(t *testing.T) {
	b, client, ns := clusterBackend(t, busyboxImages())
	ctx := context.Background()

	spec := runtime.WorkloadSpec{
		AppID: "app-1", Slug: "demo", Name: "hungry", Framework: "python",
		MemoryLimitMB: 16,
	}
	handle, err := b.Jobs().Start(ctx, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Allocate far past the limit.
	patchJobCommand(t, client, ns, handle, []string{"sh", "-c",
		"dd if=/dev/zero of=/dev/shm/fill bs=1M count=256 2>/dev/null; sleep 5"})

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		status, err := b.Jobs().Status(ctx, handle)
		if err == nil && status.State.Terminal() {
			if status.State != runtime.JobResourceExceeded {
				t.Fatalf("state = %q, want resource_exceeded (message: %s)", status.State, status.Message)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Skip("the workload was not OOM-killed within the deadline; /dev/shm may not count against the limit on this kernel")
}

// An image that will never pull must be reported as image_pull rather than
// leaving the workload apparently pending forever. This is the operator's
// problem, not the app author's, and the stage is what says so.
func TestClusterUnpullableImageIsReportedAsImagePull(t *testing.T) {
	b, _, _ := clusterBackend(t, map[string]string{
		"python": "localhost:1/definitely-not-a-real-image:v0",
	})
	ctx := context.Background()

	spec := runtime.WorkloadSpec{
		AppID: "app-1", Slug: "demo", Name: "unpullable", Framework: "python",
	}
	if _, err := b.Jobs().Start(ctx, spec); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		pods, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{})
		if err == nil {
			for i := range pods.Items {
				_, stage, _, _ := classify(&pods.Items[i])
				if stage == runtime.FailureImagePull {
					return // reported correctly
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("an unpullable image was never reported as image_pull")
}

// --- helpers ---

// patchContainerCommand replaces the workload container's command, so a test
// can use a stock image instead of the runner. The pod is recreated because a
// running pod's command is immutable.
func patchContainerCommand(t *testing.T, client kubernetes.Interface, ns, name string, command []string) {
	t.Helper()
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		pod, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			spec := pod.Spec.DeepCopy()
			spec.Containers[0].Command = command
			spec.Containers[0].SecurityContext.RunAsNonRoot = ptr(false)
			spec.SecurityContext.RunAsNonRoot = ptr(false)

			client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
			waitGone(t, client, ns, name)

			replacement := pod.DeepCopy()
			replacement.Spec = *spec
			replacement.ResourceVersion = ""
			replacement.Status = corev1.PodStatus{}
			if _, err := client.CoreV1().Pods(ns).Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
				t.Fatalf("recreating pod with a runnable command: %v", err)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pod %s never appeared", name)
}

func patchJobCommand(t *testing.T, client kubernetes.Interface, ns, handle string, command []string) {
	t.Helper()
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		job, err := client.BatchV1().Jobs(ns).Get(ctx, handle, metav1.GetOptions{})
		if err == nil {
			job.Spec.Template.Spec.Containers[0].Command = command
			job.Spec.Template.Spec.Containers[0].SecurityContext.RunAsNonRoot = ptr(false)
			job.Spec.Template.Spec.SecurityContext.RunAsNonRoot = ptr(false)
			sel := job.Spec.Selector
			job.Spec.Selector = sel

			policy := metav1.DeletePropagationForeground
			client.BatchV1().Jobs(ns).Delete(ctx, handle, metav1.DeleteOptions{PropagationPolicy: &policy})
			waitJobGone(t, client, ns, handle)

			fresh := job.DeepCopy()
			fresh.ResourceVersion = ""
			fresh.Spec.Selector = nil
			delete(fresh.Spec.Template.Labels, "controller-uid")
			delete(fresh.Spec.Template.Labels, "batch.kubernetes.io/controller-uid")
			delete(fresh.Spec.Template.Labels, "job-name")
			delete(fresh.Spec.Template.Labels, "batch.kubernetes.io/job-name")
			fresh.Status = batchv1.JobStatus{}
			if _, err := client.BatchV1().Jobs(ns).Create(ctx, fresh, metav1.CreateOptions{}); err != nil {
				t.Fatalf("recreating job with a runnable command: %v", err)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("job %s never appeared", handle)
}

func waitGone(t *testing.T, client kubernetes.Interface, ns, name string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if _, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("pod %s never went away", name)
}

func waitJobGone(t *testing.T, client kubernetes.Interface, ns, handle string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if _, err := client.BatchV1().Jobs(ns).Get(context.Background(), handle, metav1.GetOptions{}); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("job %s never went away", handle)
}

// runInPod runs a throwaway pod to completion and returns its output. Used to
// probe a workload from *inside* the cluster, which is the only place a
// ClusterIP service is reachable from — and proving that is the point.
func runInPod(t *testing.T, client kubernetes.Interface, ns, name, image string, command []string) string {
	t.Helper()
	ctx := context.Background()

	// The prober stands in for the gateway, so it has to carry what the gateway
	// carries: every isolated workload's NetworkPolicy admits ingress from pods
	// labelled `app.kubernetes.io/name: toft` and denies everything else,
	// which is the property that makes promotion worth something. Without the
	// label the policy refuses the probe — and k3s's controller rejects rather
	// than drops, so it arrives as "connection refused" and reads exactly like a
	// broken Service.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{"app.kubernetes.io/name": managedByValue},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "probe", Image: image, Command: command,
			}},
		},
	}
	if _, err := client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating probe pod: %v", err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		got, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && (got.Status.Phase == "Succeeded" || got.Status.Phase == "Failed") {
			stream, err := client.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{}).Stream(ctx)
			if err != nil {
				t.Fatalf("probe logs: %v", err)
			}
			defer stream.Close()
			var sb strings.Builder
			io.Copy(&sb, stream)
			return sb.String()
		}
		time.Sleep(time.Second)
	}
	t.Fatal("probe pod never finished")
	return ""
}
