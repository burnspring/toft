package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	runtimeobj "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"toft/internal/runner"
	"toft/internal/runtime"
)

// Tier one: what the backend *sends* to the API, in milliseconds, against a
// fake clientset. This proves nothing about whether a workload runs — that is
// tier two, in kube_cluster_test.go against a real cluster — but it is where
// the object-shaping rules that are easy to get quietly wrong are pinned.

func testBackend(t *testing.T) (*Backend, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset()
	b := NewWithClient(client, Config{
		Namespace:    "toft-apps",
		CallbackURL:  "http://toft.toft.svc.cluster.local:8080",
		RunnerImages: map[string]string{"python": "reg/runner-python:1", "node": "reg/runner-node:1", "go": "reg/runner-go:1"},
	})
	return b, client
}

func testSpec() runtime.WorkloadSpec {
	return runtime.WorkloadSpec{
		AppID: "app-1", Slug: "demo", Name: "demo-abc123",
		Release:   runtime.ArtifactRelease("releases/app-1/dep-1.tar.gz"),
		Framework: "flask", BasePath: "/apps/demo", MemoryLimitMB: 512,
		Env: map[string]string{"APP_SECRET": "hunter2"},
	}
}

// The property that matters most in a pod spec: it is readable by anyone with
// get-pod in the namespace, so nothing sized or secret may travel in it. Only
// identity and pointers go in the environment; the app's decrypted environment
// arrives in the bootstrap *response*.
func TestSecretsNeverReachThePodSpec(t *testing.T) {
	b, client := testBackend(t)

	spec := testSpec()
	go b.Deploy(spec) // Deploy blocks waiting for readiness; we only need the create.
	waitForPod(t, client, "demo-abc123")

	pod, err := client.CoreV1().Pods("toft-apps").Get(context.Background(), "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, env := range pod.Spec.Containers[0].Env {
		if strings.Contains(env.Value, "hunter2") {
			t.Errorf("the app's secret %s=%q is in the pod spec", env.Name, env.Value)
		}
		if env.Name == "APP_SECRET" {
			t.Error("the app's environment was injected into the pod spec rather than fetched at bootstrap")
		}
	}

	// What *should* be there: the pointers a runner needs to call home.
	got := map[string]string{}
	for _, env := range pod.Spec.Containers[0].Env {
		got[env.Name] = env.Value
	}
	if got["TOFT_CALLBACK_URL"] == "" {
		t.Error("no callback URL — the runner cannot bootstrap")
	}
	// And what must *not* be there any more: a credential of any kind. The
	// workload's identity is a projected token kubelet writes into a volume, so
	// the spec carries no secret and nothing in it expires.
	if _, ok := got["TOFT_BOOTSTRAP_TOKEN"]; ok {
		t.Error("a bootstrap token is back in the pod spec — a credential in an immutable spec is what this design removed")
	}
	if got["TOFT_RUNNER_MODE"] != "serve" {
		t.Errorf("mode = %q, want serve", got["TOFT_RUNNER_MODE"])
	}
}

// Every workload gets the scratch emptyDir at the fixed contract path — not
// part of the opt-in storage machinery, no env in the pod spec (the runner
// sets TMPDIR/TOFT_SCRATCH_DIR itself from the same constant). The
// emptyDir dying with the pod is what makes restart wipe scratch in this
// backend without any code.
func TestEveryWorkloadGetsAScratchEmptyDir(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	waitForPod(t, client, "demo-abc123")

	pod, err := client.CoreV1().Pods("toft-apps").Get(context.Background(), "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "scratch" {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.EmptyDir == nil {
		t.Fatal("no scratch emptyDir on the pod")
	}
	if vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.String() != "1Gi" {
		t.Errorf("scratch size limit = %v, want 1Gi", vol.EmptyDir.SizeLimit)
	}

	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "scratch" {
			found = true
			if m.MountPath != runner.ScratchMountPath {
				t.Errorf("scratch mounted at %q, want %q", m.MountPath, runner.ScratchMountPath)
			}
			if m.ReadOnly {
				t.Error("scratch is read-only")
			}
		}
	}
	if !found {
		t.Error("scratch volume is not mounted into the workload container")
	}
}

// A workload must be unreachable except through Toft's gateway. In
// Kubernetes that means a ClusterIP Service — a LoadBalancer or NodePort would
// put an app on the network with no authorization in front of it at all.
func TestServiceIsClusterIPOnly(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	waitForService(t, client, "demo-abc123")

	svc, err := client.CoreV1().Services("toft-apps").Get(context.Background(), "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service type = %q — an app must not be reachable without passing the gateway", svc.Spec.Type)
	}
}

// The network policy is what makes the isolation an app opted into real.
// Without it, pods in a shared namespace reach each other freely and
// "isolated" would mean only "its own resource envelope".
func TestEachWorkloadGetsAnIngressPolicy(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	waitForService(t, client, "demo-abc123")

	policies, err := client.NetworkingV1().NetworkPolicies("toft-apps").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 1 {
		t.Fatalf("policies = %d, want 1", len(policies.Items))
	}
	p := policies.Items[0]
	if p.Spec.PodSelector.MatchLabels[labelWorkload] != "demo-abc123" {
		t.Errorf("policy selects %v, not this workload", p.Spec.PodSelector.MatchLabels)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].From) == 0 {
		t.Fatal("the policy admits everything or nothing; it should admit only the platform")
	}
	// Egress is deliberately unrestricted: apps legitimately reach internal
	// databases and third-party APIs, and guessing at those would break more
	// than it protected.
	for _, pt := range p.Spec.PolicyTypes {
		if pt == "Egress" {
			t.Error("egress restricted; that is a deliberate non-goal here")
		}
	}
}

// The policy admits the platform's namespace and nothing else. An empty
// namespace selector would let anyone who can create a pod anywhere in the
// cluster label it app.kubernetes.io/name=toft and reach the app around the
// gateway.
func TestTheIngressPolicyAdmitsOnlyThePlatformsNamespace(t *testing.T) {
	client := fake.NewSimpleClientset()
	b := NewWithClient(client, Config{Namespace: "toft-apps", PlatformNamespace: "toft",
		CallbackURL: "http://toft.toft.svc.cluster.local:8080"})
	labels := map[string]string{labelWorkload: "demo-abc123"}
	if err := b.ensureNetworkPolicy(context.Background(), "demo-abc123", labels); err != nil {
		t.Fatal(err)
	}
	p, err := client.NetworkingV1().NetworkPolicies("toft-apps").Get(context.Background(), "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	from := p.Spec.Ingress[0].From[0]
	if from.NamespaceSelector == nil || from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "toft" {
		t.Errorf("namespace selector = %+v, want the platform's namespace only", from.NamespaceSelector)
	}
	if from.PodSelector == nil || from.PodSelector.MatchLabels["app.kubernetes.io/name"] != managedByValue {
		t.Errorf("pod selector = %+v", from.PodSelector)
	}
}

// A policy is brought up to date on every start, not written once: switching
// BLOCK_AWS_METADATA on reaches an app the next time it starts, without a new
// release name.
func TestTheIngressPolicyIsReappliedWhenTheConfigurationMoves(t *testing.T) {
	client := fake.NewSimpleClientset()
	cfg := Config{Namespace: "toft-apps", PlatformNamespace: "toft",
		CallbackURL: "http://toft.toft.svc.cluster.local:8080"}
	labels := map[string]string{labelWorkload: "demo-abc123"}
	ctx := context.Background()
	if err := NewWithClient(client, cfg).ensureNetworkPolicy(ctx, "demo-abc123", labels); err != nil {
		t.Fatal(err)
	}

	cfg.BlockedEgressCIDRs = MetadataCIDRs
	if err := NewWithClient(client, cfg).ensureNetworkPolicy(ctx, "demo-abc123", labels); err != nil {
		t.Fatalf("second start: %v", err)
	}
	p, err := client.NetworkingV1().NetworkPolicies("toft-apps").Get(ctx, "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.Egress) != 1 || len(p.Spec.Egress[0].To) != 2 || p.Spec.Egress[0].To[0].IPBlock == nil ||
		len(p.Spec.Egress[0].To[0].IPBlock.Except)+len(p.Spec.Egress[0].To[1].IPBlock.Except) != len(MetadataCIDRs) {
		t.Fatalf("the metadata block did not reach the existing policy: %+v", p.Spec)
	}

	// Unchanged configuration writes nothing.
	client.ClearActions()
	if err := NewWithClient(client, cfg).ensureNetworkPolicy(ctx, "demo-abc123", labels); err != nil {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "update" {
			t.Error("an unchanged policy was rewritten")
		}
	}
}

// The endpoint must be a Service DNS name, never a pod IP: a rescheduled pod
// gets a new IP, and a caller that persisted one would proxy into nothing.
func TestEndpointIsAStableServiceName(t *testing.T) {
	b, client := testBackend(t)

	type result struct {
		ep  runtime.Endpoint
		err error
	}
	done := make(chan result, 1)
	go func() {
		ep, err := b.Deploy(testSpec())
		done <- result{ep, err}
	}()
	waitForPod(t, client, "demo-abc123")
	markPodRunning(t, client, "demo-abc123")

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("deploy: %v", r.err)
		}
		ep := r.ep
		want := "demo-abc123.toft-apps.svc.cluster.local"
		if ep.Host != want {
			t.Errorf("host = %q, want %q", ep.Host, want)
		}
		if ep.Port != containerPort {
			t.Errorf("port = %d", ep.Port)
		}
		// And the same helpers the shared backend's endpoint uses must render
		// it — the gateway has one code path, not two.
		if ep.URL() != "http://"+want+":8080" {
			t.Errorf("URL = %q", ep.URL())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("deploy never returned")
	}
}

// A pod is never handed the default service-account token: app code has no
// business talking to the Kubernetes API, and that token is the most valuable
// thing in an otherwise well-isolated pod.
func TestPodsDoNotMountAServiceAccountToken(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	pod := waitForPod(t, client, "demo-abc123")

	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("the pod mounts a Kubernetes API token")
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("the container may run as root")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("privilege escalation is permitted")
	}
}

// The workload spec meets Pod Security `restricted`, which the apps namespace
// enforces: a field missing here is a pod the API server refuses, not a
// weaker pod. Seccomp is the one the spec once lacked — unset, the container
// runs unconfined on most runtimes.
// Service links would hand every isolated pod the name and ClusterIP of every
// other release in the namespace (security review 2026-11-14, K8-5). Services
// and jobs share podSpec, so both are asked.
func TestServiceLinksAreOff(t *testing.T) {
	b, _ := testBackend(t)
	for _, mode := range []struct{ mode, runID string }{{"serve", ""}, {"job", "run-7"}} {
		spec, err := b.podSpec(context.Background(), testSpec(), mode.mode, mode.runID)
		if err != nil {
			t.Fatal(err)
		}
		if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
			t.Errorf("%s pod: EnableServiceLinks = %v, want false", mode.mode, spec.EnableServiceLinks)
		}
	}
}

func TestWorkloadPodsMeetPodSecurityRestricted(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	pod := waitForPod(t, client, "demo-abc123")

	psc := pod.Spec.SecurityContext
	if psc == nil || psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccomp = %+v, want RuntimeDefault", psc)
	}
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.RunAsUser == nil || *psc.RunAsUser == 0 {
		t.Errorf("pod may run as root: %+v", psc)
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Error("the pod shares a host namespace")
	}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("container %s has no security context", c.Name)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("container %s seccomp = %+v, want RuntimeDefault", c.Name, sc.SeccompProfile)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("container %s may escalate", c.Name)
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.Errorf("container %s is privileged", c.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
			len(sc.Capabilities.Add) != 0 {
			t.Errorf("container %s capabilities = %+v, want drop ALL and add nothing", c.Name, sc.Capabilities)
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				t.Errorf("container %s binds host port %d", c.Name, p.HostPort)
			}
		}
	}
	// restricted admits only these volume types.
	for _, v := range pod.Spec.Volumes {
		vs := v.VolumeSource
		if vs.EmptyDir == nil && vs.Projected == nil && vs.PersistentVolumeClaim == nil &&
			vs.ConfigMap == nil && vs.Secret == nil && vs.CSI == nil && vs.DownwardAPI == nil && vs.Ephemeral == nil {
			t.Errorf("volume %s is of a type Pod Security restricted refuses: %+v", v.Name, vs)
		}
	}
}

// A serve pod is restarted in place, and it can be because nothing in its spec
// expires any more. While the spec carried a bootstrap token with fifteen
// minutes on it, kubelet's restart re-execed the runner against a dead
// credential forever; the identity is now projected and rotated, so a restarted
// container reads a current one. This is the fastest recovery available — ten
// seconds, no schedule, no image pull — and the platform's supervisor is the
// backstop for what no in-pod restart can reach.
func TestServePodsAreRestartedInPlace(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	pod := waitForPod(t, client, "demo-abc123")

	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("restart policy = %q, want Always", pod.Spec.RestartPolicy)
	}
}

// The workload's identity: a projected ServiceAccount token, bound to the pod,
// scoped to an audience the API server itself refuses, and rotated in place.
// Nothing in the spec expires, which is what makes the in-place restart above
// able to succeed.
func TestServePodsCarryAProjectedIdentity(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	pod := waitForPod(t, client, "demo-abc123")

	if pod.Spec.ServiceAccountName != WorkloadServiceAccount {
		t.Errorf("service account = %q, want %q", pod.Spec.ServiceAccountName, WorkloadServiceAccount)
	}
	// The hardening this replaces has to survive it: automount would grant the
	// token whose audience is the API server, and that stays off.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("the pod mounts the default Kubernetes API token")
	}

	var proj *corev1.ServiceAccountTokenProjection
	for _, v := range pod.Spec.Volumes {
		if v.Name != "identity" || v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ServiceAccountToken != nil {
				proj = src.ServiceAccountToken
			}
		}
	}
	if proj == nil {
		t.Fatal("no projected identity: this pod cannot bootstrap")
	}
	if proj.Audience != IdentityAudience {
		t.Errorf("audience = %q, want %q — an unscoped token works against the API server", proj.Audience, IdentityAudience)
	}
	if proj.ExpirationSeconds == nil || *proj.ExpirationSeconds > 3600 {
		t.Errorf("expiration = %v, want a short rotation", proj.ExpirationSeconds)
	}

	var mount *corev1.VolumeMount
	for i, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "identity" {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("the identity volume is not mounted")
	}
	if mount.MountPath != runner.IdentityMountPath {
		t.Errorf("mounted at %q, want the contract path %q", mount.MountPath, runner.IdentityMountPath)
	}
	if !mount.ReadOnly {
		t.Error("the identity is mounted writable")
	}
}

// A reserved-namespace slug — every first-party guide — is not a legal label
// value, so the pod's label is a sanitised copy and the pod carries the name
// as given beside it. Both readers that answer with a name read the copy the
// caller keys by: the metrics map, which app_handlers looks up by ProcessName,
// and the identity a bootstrapping pod is matched against.
func TestAWorkloadNameALabelCannotHoldStillRoundTrips(t *testing.T) {
	b, client := testBackend(t)
	spec := testSpec()
	spec.Slug, spec.Name, spec.BasePath = "_user-guide", "_user-guide-abc123", "/apps/_user-guide"
	go b.Deploy(spec)
	pod := waitForPod(t, client, "user-guide-abc123")

	if got := pod.Labels[labelWorkload]; got != "user-guide-abc123" {
		t.Errorf("label = %q, want the sanitised name a selector can hold", got)
	}
	if got := workloadNameOf(pod); got != "_user-guide-abc123" {
		t.Errorf("workloadNameOf = %q, want the name the platform gave", got)
	}
	workloads, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := workloads["_user-guide-abc123"]; !ok {
		t.Errorf("List keyed by %v; a caller looking up %q finds nothing", keysOf(workloads), "_user-guide-abc123")
	}
}

func keysOf(m map[string]*runtime.AppMetrics) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A framework with no configured image must be refused, not guessed at: a
// default would produce a pod that pulls, starts, and fails at exec for a
// reason nobody traces back to configuration.
func TestMissingRunnerImageIsRefused(t *testing.T) {
	client := fake.NewSimpleClientset()
	b := NewWithClient(client, Config{
		Namespace: "toft-apps", CallbackURL: "http://platform",
		RunnerImages: map[string]string{"node": "reg/runner-node:1"},
	})

	spec := testSpec() // flask -> python family, which is not configured
	_, err := b.Deploy(spec)
	if err == nil {
		t.Fatal("a workload with no runner image was accepted")
	}
	if !strings.Contains(err.Error(), "python") {
		t.Errorf("the error should name the missing family: %v", err)
	}
}

func TestLanguageFamilyMapping(t *testing.T) {
	cases := map[string]string{
		"flask": "python", "fastapi": "python", "streamlit": "python", "python": "python",
		"go": "go", "node": "node", "nextjs": "node", "": "node",
		// A family of its own, never the node default: an install that
		// configured only RUNNER_IMAGE_GO must not be told it needs a Node
		// image in order to serve an HTML file containing no JavaScript.
		"static": "static",
	}
	for framework, want := range cases {
		if got := languageFamily(framework); got != want {
			t.Errorf("%q -> %q, want %q", framework, got, want)
		}
	}
}

// Object names are DNS labels. A release process name is close but not
// guaranteed, and an invalid name is rejected by the API server at create
// time — after the caller has already been told the deploy started.
func TestObjectNamesAreValidDNSLabels(t *testing.T) {
	cases := map[string]string{
		"demo-abc123":            "demo-abc123",
		"My_App.v2":              "my-app-v2",
		"":                       "workload",
		"---":                    "workload",
		strings.Repeat("a", 100): strings.Repeat("a", 63),
	}
	for in, want := range cases {
		got := objectName(in)
		if got != want {
			t.Errorf("objectName(%q) = %q, want %q", in, got, want)
		}
		if len(got) > 63 {
			t.Errorf("objectName(%q) is %d chars", in, len(got))
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Errorf("objectName(%q) = %q has a leading or trailing dash", in, got)
		}
	}
}

// --- Jobs ---

// A Job must not retry itself: retries are the queue's business, and a
// kubelet-driven retry would run outside the platform's accounting, so a
// failed run would silently cost twice what its record says.
func TestJobsDoNotRetryThemselves(t *testing.T) {
	b, client := testBackend(t)

	spec := testSpec()
	spec.Name = "run-7"
	handle, err := b.Jobs().Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	job, err := client.BatchV1().Jobs("toft-apps").Get(context.Background(), handle, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoff limit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Error("no TTL: finished Jobs would accumulate in the namespace forever")
	}
}

// Starting the same run twice must not execute the work twice — a dispatcher
// retrying after an ambiguous failure is a normal event.
func TestStartingARunTwiceIsIdempotent(t *testing.T) {
	b, client := testBackend(t)
	spec := testSpec()
	spec.Name = "run-7"

	first, err := b.Jobs().Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Jobs().Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("a repeated start should be accepted: %v", err)
	}
	if first != second {
		t.Errorf("handles differ: %q vs %q", first, second)
	}

	jobs, _ := client.BatchV1().Jobs("toft-apps").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Errorf("created %d jobs; the work would run twice", len(jobs.Items))
	}
}

// An out-of-memory kill is not a crash, and an unpullable image is not the
// app's fault. Collapsing them sends a developer to debug the wrong thing.
func TestPodClassificationPreservesTheDistinctions(t *testing.T) {
	cases := []struct {
		name      string
		pod       *corev1.Pod
		wantState runtime.JobState
		wantStage runtime.FailureStage
	}{
		{
			name: "oom kill",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "OOMKilled", ExitCode: 137,
					}},
				}},
			}},
			wantState: runtime.JobResourceExceeded,
			wantStage: runtime.FailureExec,
		},
		{
			// **Still pulling, not failed.** The kubelet retries ErrImagePull
			// at 10s, 20s, 40s, and ImagePullBackOff *is* that retry throttled
			// — so the first sighting is the beginning of a pull. Reading it as
			// terminal aborted deploys about two seconds in, and the next
			// deploy then succeeded unchanged. The stage rides along so the
			// caller can say what it is waiting for; the budget is the
			// caller's, not this function's.
			name: "a pull the kubelet is still retrying",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "ImagePullBackOff", Message: "no such image",
					}},
				}},
			}},
			wantState: runtime.JobPending,
			wantStage: runtime.FailureImagePull,
		},
		{
			name: "a pull that has just failed for the first time",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "ErrImagePull", Message: "connection refused",
					}},
				}},
			}},
			wantState: runtime.JobPending,
			wantStage: runtime.FailureImagePull,
		},
		{
			// The one pull failure no retry can help: the string is not a
			// reference, so waiting through it buys nothing.
			name: "an image name that is not a reference",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "InvalidImageName", Message: "invalid reference format",
					}},
				}},
			}},
			wantState: runtime.JobInfrastructureErr,
			wantStage: runtime.FailureImagePull,
		},
		{
			// The kubelet retries both, and a retry that lands starts the
			// container: pending with the stage, never terminal (K8-4).
			name: "a container the kubelet could not create yet",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "CreateContainerError", Message: "subPath could not be created",
					}},
				}},
			}},
			wantState: runtime.JobPending,
			wantStage: runtime.FailureExec,
		},
		{
			name: "app exited non-zero",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
				}},
			}},
			wantState: runtime.JobFailed,
			wantStage: runtime.FailureExec,
		},
		{
			name:      "succeeded",
			pod:       &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			wantState: runtime.JobSucceeded,
			wantStage: runtime.FailureNone,
		},
		{
			name:      "still scheduling",
			pod:       &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			wantState: runtime.JobPending,
			wantStage: runtime.FailureNone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, stage, _, _ := classify(tc.pod)
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if stage != tc.wantStage {
				t.Errorf("stage = %q, want %q", stage, tc.wantStage)
			}
		})
	}
}

// A deploy waits through a pull the kubelet is still retrying.
//
// This is the bug those classifications exist to prevent: waitReady polls every
// two seconds and used to return on any stage at all, so a deploy gave up about
// two seconds into a pull the kubelet retries at 10s, 20s and 40s — well inside
// the five-minute budget it already had. The signature it left was a deploy
// that failed at image_pull and a next deploy that succeeded unchanged.
func TestADeployWaitsThroughAPullThatIsStillBeingRetried(t *testing.T) {
	b, client := testBackend(t)

	var mu sync.Mutex
	gets := 0
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		gets++
		if gets < 3 {
			return true, waitingPod("ImagePullBackOff", "pull QPS exceeded"), nil
		}
		return true, &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.waitReady(ctx, "demo-abc123", time.Minute); err != nil {
		t.Fatalf("waitReady gave up on a pull that then succeeded: %v", err)
	}
}

// And gives up naming the stage, the elapsed time and the registry's own words.
func TestAPullThatNeverLandsFailsAtImagePullAndSaysWhy(t *testing.T) {
	b, client := testBackend(t)
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		return true, waitingPod("ErrImagePull", "manifest unknown"), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := b.waitReady(ctx, "demo-abc123", time.Millisecond)
	if err == nil {
		t.Fatal("a pull that never lands has to end the deploy")
	}
	for _, want := range []string{"image_pull", "manifest unknown"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A pod nothing can place says why it could not be placed.
//
// `classify` reads container statuses and an unschedulable pod has none — it
// never gets as far as a container — so every tick fell through to the default
// and the only thing left to say after five minutes was that five minutes had
// passed. The scheduler's own sentence was on the pod object being polled the
// whole time, and it is the entire answer: a deploy that ends in "did not
// become ready" sends an operator to look at an app that never ran, and this
// error is what a failed start's event relays.
func TestAPodThatCannotBeScheduledSaysWhy(t *testing.T) {
	b, client := testBackend(t)
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		return true, &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			// No container statuses at all, which is the shape that made this
			// invisible.
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: `0/1 nodes are available: persistentvolumeclaim "lpv-drop-read" is being deleted`,
			}},
		}}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := b.waitReady(ctx, "demo-abc123", time.Hour)
	if err == nil {
		t.Fatal("a pod that never scheduled has to end the wait")
	}
	for _, want := range []string{"could not be scheduled", "lpv-drop-read", "is being deleted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

// The last read of a wait that is running out can fail on the deadline itself
// — client-go's rate limiter refuses a call it cannot finish in time — and that
// refusal is not the answer: the scheduler's sentence from the read before is.
func TestAReadRefusedAtTheDeadlineKeepsTheSchedulersReason(t *testing.T) {
	b, client := testBackend(t)
	var reads int
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		reads++
		if reads > 1 {
			return true, nil, errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
		}
		return true, &corev1.Pod{Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
				Message: "0/1 nodes are available: 1 Insufficient memory",
			}},
		}}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), waitTick+500*time.Millisecond)
	defer cancel()
	err := b.waitReady(ctx, "demo-abc123", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "Insufficient memory") {
		t.Fatalf("the wait lost the scheduler's reason to its own deadline: %v", err)
	}
	if reads < 2 {
		t.Fatalf("the wait read the pod %d time(s); the case needs a read at the deadline", reads)
	}
}

// An image name that is not a reference ends the deploy on first sight: no
// retry can help, so a grace would only delay the answer.
func TestAnInvalidImageNameEndsTheDeployImmediately(t *testing.T) {
	b, client := testBackend(t)
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		return true, waitingPod("InvalidImageName", "invalid reference format"), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	if err := b.waitReady(ctx, "demo-abc123", time.Hour); err == nil {
		t.Fatal("an unusable image reference is terminal")
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Errorf("waited %s before refusing an image name no retry can fix", waited)
	}
}

func waitingPod(reason, message string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: reason, Message: message,
			}},
		}},
	}}
}

// Whole-machine metrics are not answerable for a workload with a scheduler
// between it and any hardware, and saying so is correct rather than a defect.
func TestSystemMetricsAreNotSupported(t *testing.T) {
	b, _ := testBackend(t)
	m, err := b.SystemMetrics()
	if err == nil {
		t.Fatal("a cluster backend should not claim to know the host")
	}
	if m != nil {
		t.Error("a zeroed struct would render as an idle host")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("err = %v", err)
	}
}

// The per-app counterpart, and the reason it is asserted at all: this backend
// takes no CPU or memory reading, and the fields it leaves nil are what makes
// the page say so. Without this test a later edit that sets 0.0 "to fill the
// field" restores the defect — every isolated app green, at 0.0%, sorted to the
// bottom of a busiest-first table — and nothing notices.
func TestMetricsReportsNoReadingRatherThanZero(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	waitForPod(t, client, "demo-abc123")
	markPodRunning(t, client, "demo-abc123")

	m, err := b.Metrics("demo-abc123")
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.Status != "online" {
		t.Errorf("status = %q, want online — the fields this backend *can* answer still have to arrive", m.Status)
	}
	if m.CPU != nil {
		t.Errorf("Metrics reported CPU %v; this backend measures none", *m.CPU)
	}
	if m.Memory != nil {
		t.Errorf("Metrics reported memory %v; this backend measures none", *m.Memory)
	}

	all, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	listed, ok := all["demo-abc123"]
	if !ok {
		t.Fatalf("List did not report the workload: %v", all)
	}
	if listed.CPU != nil || listed.Memory != nil {
		t.Errorf("List reported figures the backend never took: cpu=%v memory=%v", listed.CPU, listed.Memory)
	}
}

// Delete must remove every object the backend created, or a redeploy inherits
// a stale Service or policy.
func TestDeleteRemovesEveryOwnedObject(t *testing.T) {
	b, client := testBackend(t)
	go b.Deploy(testSpec())
	waitForService(t, client, "demo-abc123")

	if err := b.Delete("demo-abc123"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if pods, _ := client.CoreV1().Pods("toft-apps").List(ctx, metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Error("pod survived")
	}
	if svcs, _ := client.CoreV1().Services("toft-apps").List(ctx, metav1.ListOptions{}); len(svcs.Items) != 0 {
		t.Error("service survived")
	}
	if ps, _ := client.NetworkingV1().NetworkPolicies("toft-apps").List(ctx, metav1.ListOptions{}); len(ps.Items) != 0 {
		t.Error("network policy survived")
	}
}

// Deleting something that is already gone is not an error: a retried cleanup
// after a partial failure is normal.
func TestDeleteIsIdempotent(t *testing.T) {
	b, _ := testBackend(t)
	if err := b.Delete("never-existed"); err != nil {
		t.Errorf("deleting an absent workload: %v", err)
	}
}

func TestBackendsSatisfyTheContracts(t *testing.T) {
	var _ runtime.ServiceRuntime = (*Backend)(nil)
	var _ runtime.JobRuntime = (*JobBackend)(nil)
}

func TestNewRequiresNamespaceAndCallback(t *testing.T) {
	if _, err := New(Config{CallbackURL: "http://x"}); err == nil {
		t.Error("a namespace should be required")
	}
	if _, err := New(Config{Namespace: "apps"}); err == nil {
		t.Error("a callback URL should be required — a pod cannot resolve BASE_URL")
	}
}

// --- helpers ---

// sleepBriefly yields to the goroutine driving a Deploy. Deploy blocks on
// readiness, so these tests start it concurrently and poll for the objects it
// creates on the way.
func sleepBriefly() { time.Sleep(2 * time.Millisecond) }

func waitForPod(t *testing.T, client *fake.Clientset, name string) *corev1.Pod {
	t.Helper()
	for i := 0; i < 200; i++ {
		pod, err := client.CoreV1().Pods("toft-apps").Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			return pod
		}
		sleepBriefly()
	}
	t.Fatalf("pod %s was never created", name)
	return nil
}

func waitForService(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := client.CoreV1().Services("toft-apps").Get(context.Background(), name, metav1.GetOptions{}); err == nil {
			// The policy is created immediately after the Service.
			sleepBriefly()
			return
		}
		sleepBriefly()
	}
	t.Fatalf("service %s was never created", name)
}

func markPodRunning(t *testing.T, client *fake.Clientset, name string) {
	t.Helper()
	pod, err := client.CoreV1().Pods("toft-apps").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	if _, err := client.CoreV1().Pods("toft-apps").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// --- Deploy replaces what is running ---

// Deploy is the only way a workload starts, so it has to replace rather than
// step aside. It used to swallow AlreadyExists and leave the running pod alone
// — correct while Deploy was only ever called with a *new* release name, and
// silently wrong the moment it became the restart path too, where "left alone"
// means "did nothing". That is exactly how a restart of an isolated app
// reported success and changed nothing.
func TestDeployReplacesAPodAlreadyUnderTheName(t *testing.T) {
	b, client := testBackend(t)

	// Something is already running under this name, from a start whose spec
	// this backend can no longer reconstruct — which is the whole reason the
	// platform supplies a fresh one.
	old := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-abc123",
			Namespace: "toft-apps",
			Labels:    map[string]string{"generation": "old"},
		},
	}
	if _, err := client.CoreV1().Pods("toft-apps").Create(
		context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	go b.Deploy(testSpec())

	// The replacement is the pod without the old generation's label.
	var pod *corev1.Pod
	for i := 0; i < 400; i++ {
		got, err := client.CoreV1().Pods("toft-apps").Get(
			context.Background(), "demo-abc123", metav1.GetOptions{})
		if err == nil && got.Labels["generation"] != "old" {
			pod = got
			break
		}
		sleepBriefly()
	}
	if pod == nil {
		t.Fatal("the pod already under the name was never replaced")
	}
	if pod.Labels[labelWorkload] == "" {
		t.Error("the replacement is not a pod this backend built")
	}
}

// The wait is not optional, and this is why: creating into a terminating pod's
// name hits the same AlreadyExists the old code swallowed, and waitReady would
// then watch the *dying* pod reach Running and report a healthy workload with
// seconds to live.
func TestDeployWaitsForTheOldPodToBeGoneBeforeCreating(t *testing.T) {
	b, client := testBackend(t)

	if _, err := client.CoreV1().Pods("toft-apps").Create(context.Background(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-abc123", Namespace: "toft-apps"}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// A pod that lingers after its delete, which a fake clientset otherwise
	// never does — terminating is the state this wait exists for.
	var mu sync.Mutex
	deleted := false
	linger := 2
	client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		mu.Lock()
		deleted = true
		mu.Unlock()
		return false, nil, nil
	})
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if deleted && linger > 0 {
			linger--
			return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "demo-abc123", Namespace: "toft-apps",
			}}, nil
		}
		return false, nil, nil
	})

	client.ClearActions()
	go b.Deploy(testSpec())

	// The wait polls twice a second, so two lingering gets is about a second.
	for i := 0; i < 1500; i++ {
		if podCreateSeen(client) {
			break
		}
		sleepBriefly()
	}

	var sawDelete bool
	for _, a := range client.Actions() {
		if a.GetResource().Resource != "pods" {
			continue
		}
		switch a.GetVerb() {
		case "delete":
			sawDelete = true
		case "create":
			if !sawDelete {
				t.Fatal("the replacement pod was created before the old one was deleted")
			}
			mu.Lock()
			left := linger
			mu.Unlock()
			if left > 0 {
				t.Errorf("the create did not wait for the old pod to be gone: %d lingering gets left", left)
			}
			return
		}
	}
	t.Fatal("no pod was created")
}

func podCreateSeen(client *fake.Clientset) bool {
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "pods" && a.GetVerb() == "create" {
			return true
		}
	}
	return false
}

// The number that decides how long an in-flight request survives a stop lives
// in the code that owns it, rather than being inherited from Kubernetes'
// default where nothing here would notice it moving.
func TestTerminationGracePeriodIsStatedNotInherited(t *testing.T) {
	b, client := testBackend(t)

	go b.Deploy(testSpec())
	pod := waitForPod(t, client, "demo-abc123")

	if pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatal("terminationGracePeriodSeconds is unset — the pod inherits a number this package does not own")
	}
	if got := *pod.Spec.TerminationGracePeriodSeconds; got != terminationGraceSeconds {
		t.Errorf("terminationGracePeriodSeconds = %d, want %d", got, terminationGraceSeconds)
	}
}

// A crash-looping pod must not read as online, and this is the one mapping in
// the backend that was wrong for a reason no reading found: **a pod whose
// container is looping has phase Running**. Observed on k3s. While serve pods
// were RestartPolicy: Never it did not arise; restoring Always brings it back,
// and a status word taken from the phase alone would tell the health watcher
// that a workload nobody can reach is healthy.
func TestACrashLoopingPodIsNotOnline(t *testing.T) {
	running := corev1.ContainerStatus{State: corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{},
	}}
	cases := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"a serving workload", podWith(corev1.PodRunning, running), "online"},
		{"looping after a crash", podWith(corev1.PodRunning, corev1.ContainerStatus{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}), "errored"},
		{"waiting on its image", podWith(corev1.PodRunning, corev1.ContainerStatus{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}), "launching"},
		{"exited while the pod still reads Running", podWith(corev1.PodRunning, corev1.ContainerStatus{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}), "errored"},
		{"still being scheduled", podWith(corev1.PodPending), "launching"},
		{"finished", podWith(corev1.PodSucceeded), "stopped"},
		{"failed outright", podWith(corev1.PodFailed), "errored"},
		{"terminating long past its grace", terminating(podWith(corev1.PodRunning, running), -5*time.Minute), "errored"},
		{"terminating inside its grace", terminating(podWith(corev1.PodRunning, running), 20*time.Second), "launching"},
		{"running on a node marked unreachable", notReady(podWith(corev1.PodRunning, running)), "errored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := podStatusWord(tc.pod); got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// List is what the health watcher builds its whole sweep from, so it has to
// carry what Metrics carries: without CrashLooping its crash-loop guard is
// dead for isolated apps, and without Detail a pod nothing can schedule is
// reported with no reason (security review 2026-11-14, K8-3).
func TestListMatchesMetricsForCrashLoopingAndUnscheduledPods(t *testing.T) {
	client := fake.NewSimpleClientset()
	b := NewWithClient(client, Config{Namespace: "toft-apps", CallbackURL: "http://x"})
	ctx := context.Background()
	owned := map[string]string{labelManagedBy: managedByValue}

	looping := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "loop-1", Namespace: "toft-apps", Labels: owned,
			Annotations: map[string]string{annotationWorkload: "loop-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			RestartCount: 4,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 40s restarting failed container"}},
		}}},
	}
	unscheduled := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "stuck-1", Namespace: "toft-apps", Labels: owned,
			Annotations: map[string]string{annotationWorkload: "stuck-1"}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: "Unschedulable", Message: "0/1 nodes are available: insufficient memory",
		}}},
	}
	for _, p := range []*corev1.Pod{looping, unscheduled} {
		if _, err := client.CoreV1().Pods("toft-apps").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	listed, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	if m := listed["loop-1"]; m == nil || !m.CrashLooping {
		t.Errorf("List does not report the crash loop: %+v", m)
	}
	if m := listed["stuck-1"]; m == nil || !strings.Contains(m.Detail, "insufficient memory") {
		t.Errorf("List does not carry the scheduler's reason: %+v", m)
	}
	for _, name := range []string{"loop-1", "stuck-1"} {
		got, err := b.Metrics(name)
		if err != nil {
			t.Fatal(err)
		}
		l := *listed[name]
		l.CreatedAt = got.CreatedAt
		if !reflect.DeepEqual(l, *got) {
			t.Errorf("%s: List = %+v, Metrics = %+v", name, l, *got)
		}
	}
}

// terminating marks a pod deleted, its grace ending `in` from now (negative:
// already past).
func terminating(pod *corev1.Pod, in time.Duration) *corev1.Pod {
	at := metav1.NewTime(time.Now().Add(in))
	pod.DeletionTimestamp = &at
	return pod
}

func notReady(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Conditions = append(pod.Status.Conditions,
		corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "NodeLost"})
	return pod
}

// A pod on a node that stopped answering stays Terminating until the kubelet
// returns, and its name is the release name — so a restart waited out the
// deploy timeout and failed. Past the grace, the delete is re-issued with no
// grace (security review 2026-11-14, K8-8).
func TestDeletePodAndWaitForcesAPodThatOutlivesItsGrace(t *testing.T) {
	prev := forceDeleteAfter
	forceDeleteAfter = 0
	t.Cleanup(func() { forceDeleteAfter = prev })

	b, client := testBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.CoreV1().Pods("toft-apps").Create(ctx,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-abc123", Namespace: "toft-apps"}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Every delete with a grace is accepted and changes nothing, as on a node
	// whose kubelet is gone; only a zero-grace delete removes the object.
	var mu sync.Mutex
	var graces []string
	client.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtimeobj.Object, error) {
		opts := a.(k8stesting.DeleteActionImpl).DeleteOptions
		mu.Lock()
		defer mu.Unlock()
		if opts.GracePeriodSeconds != nil && *opts.GracePeriodSeconds == 0 {
			graces = append(graces, "0")
			return false, nil, nil
		}
		graces = append(graces, "default")
		return true, nil, nil
	})

	if err := b.deletePodAndWait(ctx, "demo-abc123"); err != nil {
		t.Fatalf("deletePodAndWait: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(graces, ",") != "default,0" {
		t.Errorf("deletes = %v, want an ordinary delete then one with grace 0", graces)
	}
}

func podWith(phase corev1.PodPhase, statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{Phase: phase, ContainerStatuses: statuses}}
}

// The scheduler's dials reach the container, and the two of them are not
// symmetrical. A memory limit kills and restarts, which is visible; a CPU limit
// throttles, which is not — so the request is what a workload always carries and
// the limit only appears when an operator asked for one.
func TestCPUReservationReachesTheContainer(t *testing.T) {
	b, client := testBackend(t)

	spec := testSpec()
	spec.CPURequestMillicores = 250
	spec.CPULimitMillicores = 1000
	go b.Deploy(spec)
	waitForPod(t, client, "demo-abc123")

	pod, err := client.CoreV1().Pods("toft-apps").Get(context.Background(), "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res := pod.Spec.Containers[0].Resources
	if got := res.Requests.Cpu().MilliValue(); got != 250 {
		t.Errorf("CPU request = %dm, want 250m", got)
	}
	if got := res.Limits.Cpu().MilliValue(); got != 1000 {
		t.Errorf("CPU limit = %dm, want 1000m", got)
	}
	// Memory is unchanged by any of this: still request and limit from one
	// number, because being killed for exceeding it is the point.
	if got := res.Limits.Memory().Value(); got != 512*1024*1024 {
		t.Errorf("memory limit = %d, want 512Mi", got)
	}
	if got := res.Requests.Memory().Value(); got != 512*1024*1024 {
		t.Errorf("memory request = %d, want 512Mi", got)
	}
}

// An unset dial sets nothing at all. A zero-valued quantity is not the same as
// an absent one to anybody reading the pod back — `kubectl describe` prints it
// as though somebody chose it, and a scheduler treats a 0m request as a claim
// rather than as silence.
func TestUnsetCPUDialsAppearNowhereInThePodSpec(t *testing.T) {
	b, client := testBackend(t)

	spec := testSpec()
	spec.CPURequestMillicores = 0
	spec.CPULimitMillicores = 0
	go b.Deploy(spec)
	waitForPod(t, client, "demo-abc123")

	pod, err := client.CoreV1().Pods("toft-apps").Get(context.Background(), "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res := pod.Spec.Containers[0].Resources
	if _, ok := res.Requests[corev1.ResourceCPU]; ok {
		t.Error("a CPU request appeared with none configured")
	}
	if _, ok := res.Limits[corev1.ResourceCPU]; ok {
		t.Error("a CPU limit appeared with none configured")
	}
}

// A workload with no dial of any kind carries no resource block at all, rather
// than two empty maps. This is the shape every install had before the CPU
// fields existed, and a pod spec that gained empty scaffolding would be a
// gratuitous difference in every diff an operator reads.
func TestAWorkloadWithNoLimitsCarriesNoResourceBlock(t *testing.T) {
	b, client := testBackend(t)

	spec := testSpec()
	spec.MemoryLimitMB = 0
	spec.CPURequestMillicores = 0
	spec.CPULimitMillicores = 0
	go b.Deploy(spec)
	waitForPod(t, client, "demo-abc123")

	pod, err := client.CoreV1().Pods("toft-apps").Get(context.Background(), "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res := pod.Spec.Containers[0].Resources
	if res.Requests != nil || res.Limits != nil {
		t.Errorf("empty resource maps reached the pod spec: %+v", res)
	}
}

// A pod carries the app's slug for a cluster's log agent to tag its lines
// with, and the selectors still read the workload label alone.
func TestPodsCarryTheAppSlugAndSelectorsDoNotMove(t *testing.T) {
	b, _ := testBackend(t)
	labels := b.labelsFor(testSpec(), "")
	if labels[labelAppSlug] != "demo" || labels[labelAppID] != "app-1" {
		t.Errorf("labels = %v, want the app's slug and id", labels)
	}
	ctx := context.Background()
	if err := b.ensureService(ctx, "demo-abc123", labels); err != nil {
		t.Fatal(err)
	}
	svc, err := b.client.CoreV1().Services(b.cfg.Namespace).Get(ctx, "demo-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Selector) != 1 || svc.Spec.Selector[labelWorkload] == "" {
		t.Errorf("the Service selector = %v, want the workload label alone", svc.Spec.Selector)
	}
}

// The egress block has to cover job pods too. The per-workload policy selects
// a release name and a job pod's workload label is its run id, so without the
// namespace-wide policy a job could read the metadata endpoint every service
// pod is kept off (security review 2026-11-14, K8-1).
func TestTheEgressBlockCoversJobPods(t *testing.T) {
	b, client := testBackend(t)
	b.cfg.BlockedEgressCIDRs = MetadataCIDRs
	ctx := context.Background()

	spec := testSpec()
	spec.Name = "run-7"
	handle, err := b.Jobs().Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("toft-apps").Get(ctx, handle, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := client.NetworkingV1().NetworkPolicies("toft-apps").Get(ctx, egressPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no namespace-wide egress policy: %v", err)
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("policy types = %v, want Egress only", p.Spec.PolicyTypes)
	}
	sel, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(labels.Set(job.Spec.Template.Labels)) {
		t.Errorf("selector %v does not match the job pod's labels %v", sel, job.Spec.Template.Labels)
	}
	if len(p.Spec.Egress) != 1 || len(p.Spec.Egress[0].To) != 2 {
		t.Fatalf("egress = %+v, want one rule with an IPv4 and an IPv6 peer", p.Spec.Egress)
	}
	v4, v6 := p.Spec.Egress[0].To[0].IPBlock, p.Spec.Egress[0].To[1].IPBlock
	if v4 == nil || v4.CIDR != "0.0.0.0/0" || !contains(v4.Except, "169.254.169.254/32") || !contains(v4.Except, "169.254.170.2/32") {
		t.Errorf("IPv4 peer = %+v", v4)
	}
	if v6 == nil || v6.CIDR != "::/0" || !contains(v6.Except, "fd00:ec2::254/128") {
		t.Errorf("IPv6 peer = %+v", v6)
	}
	for _, c := range v4.Except {
		if strings.Contains(c, ":") {
			t.Errorf("an IPv6 address in the IPv4 peer's except: %v", v4.Except)
		}
	}

	// Switching the block off reaches the cluster: the policy is removed, not
	// left behind to keep blocking.
	b.cfg.BlockedEgressCIDRs = nil
	spec.Name = "run-8"
	if _, err := b.Jobs().Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NetworkingV1().NetworkPolicies("toft-apps").Get(ctx, egressPolicyName, metav1.GetOptions{}); err == nil {
		t.Error("the egress policy survived the block being switched off")
	}
}

// A pod created before its policies runs, on the first start of a release
// name, with no egress rule at all. Both policies go in first, for services
// and for jobs.
func TestPoliciesAreCreatedBeforeThePod(t *testing.T) {
	b, client := testBackend(t)
	b.cfg.BlockedEgressCIDRs = MetadataCIDRs

	var mu sync.Mutex
	var order []string
	client.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtimeobj.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, a.GetResource().Resource)
		return false, nil, nil
	})

	go b.Deploy(testSpec())
	waitForService(t, client, "demo-abc123")
	spec := testSpec()
	spec.Name = "run-7"
	if _, err := b.Jobs().Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"networkpolicies", "networkpolicies", "pods", "services", "networkpolicies", "jobs"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("create order = %v, want %v", order, want)
	}
}

// A job pod of a deleted app is reported under its run id, and Delete of that
// run id has to remove its Job — or the collector reports it removed every
// sweep while the pod keeps running and keeps its claims pinned (security
// review 2026-11-14, K8-7).
func TestOrphanedJobPodIsRemovedByDeleteOfItsRunID(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	spec := testSpec()
	spec.AppID = "dead-app"
	spec.Name = "run-7"
	handle, err := b.Jobs().Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("toft-apps").Get(ctx, handle, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The pod the Job controller would make, which the fake does not.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: handle + "-x7k2p", Namespace: "toft-apps",
			Labels:            job.Spec.Template.Labels,
			Annotations:       job.Spec.Template.Annotations,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := client.CoreV1().Pods("toft-apps").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	orphans, err := b.Orphans(map[string]string{}, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].Workload != "run-7" {
		t.Fatalf("orphans = %+v, want the job pod under its run id", orphans)
	}
	if err := b.Delete(orphans[0].Workload); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BatchV1().Jobs("toft-apps").Get(ctx, handle, metav1.GetOptions{}); err == nil {
		t.Error("the orphan's Job survived Delete of its run id")
	}
}

// A live app's service pod that is not its current release and is older than
// the grace is collected; the current one, a young one, a job pod, and any pod
// of an app whose current release the caller could not name are not (security
// review 2026-11-14, K8-9).
func TestOrphansReturnsOnlyASupersededReleaseOfALiveApp(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	mk := func(name string, age time.Duration, appID, runID string) {
		l := map[string]string{labelManagedBy: managedByValue, labelAppID: appID, labelWorkload: name}
		if runID != "" {
			l[labelRunID] = runID
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "toft-apps", Labels: l,
			Annotations:       map[string]string{annotationWorkload: name},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		}}
		if _, err := client.CoreV1().Pods("toft-apps").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	mk("demo-current1", time.Hour, "app-1", "")
	mk("demo-retired1", time.Hour, "app-1", "")
	mk("demo-youngst1", time.Minute, "app-1", "")
	mk("run-9", time.Hour, "app-1", "run-9")
	mk("other-retired", time.Hour, "app-2", "")

	orphans, err := b.Orphans(map[string]string{"app-1": "demo-current1", "app-2": ""}, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].Workload != "demo-retired1" || !orphans[0].Superseded {
		t.Fatalf("orphans = %+v, want only demo-retired1, superseded", orphans)
	}
}

// A Job carries its own bound, a margin past the run's timeout, so a run stays
// bounded while the platform that enforces the stored deadline is down (RT-2).
func TestAJobCarriesActiveDeadlineSeconds(t *testing.T) {
	b, client := testBackend(t)
	spec := testSpec()
	spec.Name = "run-8"
	spec.Timeout = 10 * time.Minute
	handle, err := b.Jobs().Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("toft-apps").Get(context.Background(), handle, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := int64((10*time.Minute + jobDeadlineMargin).Seconds())
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != want {
		t.Errorf("activeDeadlineSeconds = %v, want %d", job.Spec.ActiveDeadlineSeconds, want)
	}
}

// A failed Job with no pod left is not the app's failure with exit 0, and a
// pod list that fails is an error, not a verdict (K8-6).
func TestJobStatusOfAFailedJobWithNoPod(t *testing.T) {
	b, client := testBackend(t)
	ctx := context.Background()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job-run-9", Namespace: "toft-apps"},
		Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
			Message: "Job has reached the specified backoff limit",
		}}},
	}
	if _, err := client.BatchV1().Jobs("toft-apps").Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := b.Jobs().Status(ctx, "job-run-9")
	if err != nil {
		t.Fatal(err)
	}
	if st.State == runtime.JobFailed || st.State != runtime.JobInfrastructureErr {
		t.Errorf("state = %q, want %q", st.State, runtime.JobInfrastructureErr)
	}
	if !strings.Contains(st.Message, "backoff limit") {
		t.Errorf("message = %q, want the Job's own Failed condition", st.Message)
	}

	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtimeobj.Object, error) {
		return true, nil, errors.New("the API server is restarting")
	})
	if _, err := b.Jobs().Status(ctx, "job-run-9"); err == nil {
		t.Error("a failed pod list was swallowed")
	}
}
