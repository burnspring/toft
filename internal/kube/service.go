package kube

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"toft/internal/runner"
	"toft/internal/runtime"
)

func memoryQuantity(mb int) resource.Quantity {
	return resource.MustParse(fmt.Sprintf("%dMi", mb))
}

// cpuQuantity renders millicores in the only spelling Kubernetes has for a
// fraction of a core. 1000m is one core, and `m` is the unit the API server
// stores whatever an operator wrote — so this is a translation, not a choice.
func cpuQuantity(millicores int) resource.Quantity {
	return resource.MustParse(fmt.Sprintf("%dm", millicores))
}

// Deploy runs an app as its own pod and returns the cluster address the
// gateway reaches it by.
//
// The endpoint is a Service DNS name, not an IP: a pod that is rescheduled
// gets a new IP, and a caller that had persisted one would silently proxy into
// nothing. This is the same endpoint abstraction the shared backend returns,
// which is why the proxy needs no knowledge of which backend answered.
func (b *Backend) Deploy(spec runtime.WorkloadSpec) (runtime.Endpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()

	name := objectName(spec.ProcessName())

	podSpec, err := b.podSpec(ctx, spec, "serve", "")
	if err != nil {
		return runtime.Endpoint{}, err
	}

	labels := b.labelsFor(spec, "")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: b.cfg.Namespace,
			Labels:      labels,
			Annotations: b.annotationsFor(spec),
		},
		Spec: *podSpec,
	}

	// Deploy replaces whatever is running under this name, so a pod already
	// there is retired rather than left alone. Swallowing AlreadyExists was
	// right while Deploy was only ever called with a new release name and
	// silently wrong the moment it became the restart path too, where "left
	// alone" means "did nothing" — which is how a restart of an isolated app
	// reported success and changed nothing.
	//
	// The wait is not optional. Creating into a terminating pod's name hits the
	// same AlreadyExists, and waitReady would then watch the *dying* pod reach
	// Running and report a healthy workload with seconds to live.
	if err := b.deletePodAndWait(ctx, name); err != nil {
		return runtime.Endpoint{}, err
	}

	// The policies go in before the pod, never after. A pod created first
	// runs — on the first start of a release name — with no egress rule at
	// all for as long as the next two API calls take, which is long enough to
	// read the metadata endpoint once.
	//
	// The network policy is what makes the isolation an app opted into real:
	// without it, pods in a shared namespace reach each other freely and
	// "isolated" would mean only "its own resource envelope".
	if err := b.ensureEgressPolicy(ctx); err != nil {
		return runtime.Endpoint{}, err
	}
	if err := b.ensureNetworkPolicy(ctx, name, labels); err != nil {
		return runtime.Endpoint{}, err
	}
	if _, err := b.client.CoreV1().Pods(b.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return runtime.Endpoint{}, fmt.Errorf("kube: creating pod %s: %w", name, err)
	}

	if err := b.ensureService(ctx, name, labels); err != nil {
		return runtime.Endpoint{}, err
	}

	if err := b.waitReady(ctx, name, imagePullGrace); err != nil {
		return runtime.Endpoint{}, err
	}

	return runtime.Endpoint{
		Host: fmt.Sprintf("%s.%s.svc.cluster.local", name, b.cfg.Namespace),
		Port: containerPort,
	}, nil
}

func (b *Backend) ensureService(ctx context.Context, name string, labels map[string]string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.cfg.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{labelWorkload: labels[labelWorkload]},
			Ports: []corev1.ServicePort{{
				Name:       b.cfg.ServicePortName,
				Port:       containerPort,
				TargetPort: intstr.FromString(b.cfg.ServicePortName),
			}},
			// ClusterIP, deliberately. Toft stays in the data path in
			// both modes: traffic flows browser → gateway → workload, so there
			// is exactly one authorization implementation and no new security
			// surface. A LoadBalancer or NodePort here would put an app on the
			// network without passing the gateway at all.
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	_, err := b.client.CoreV1().Services(b.cfg.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating service %s: %w", name, err)
	}
	return nil
}

// ensureNetworkPolicy admits traffic to a workload from the platform only, and
// — where the operator asked for it — keeps the workload away from the
// addresses that hand out IAM credentials.
//
// The ingress half is the network-level separation between isolated apps that
// the accepted app↔app risk is reduced by. Without it, pods in a shared
// namespace reach each other freely and "isolated" would mean only "its own
// resource envelope".
//
// Egress is otherwise deliberately unrestricted: apps legitimately reach
// internal databases and third-party APIs, and a policy that guessed at those
// would break more than it protected.
func (b *Backend) ensureNetworkPolicy(ctx context.Context, name string, labels map[string]string) error {
	// The platform's namespace, by the label the API server sets on every
	// namespace. An empty selector would admit any pod that wears the toft
	// label — and anyone who can create a pod anywhere in the cluster can put
	// that label on one and reach every isolated app around the gateway.
	from := &metav1.LabelSelector{}
	if b.cfg.PlatformNamespace != "" {
		from.MatchLabels = map[string]string{"kubernetes.io/metadata.name": b.cfg.PlatformNamespace}
	}
	spec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{labelWorkload: labels[labelWorkload]},
		},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				// Only Toft's own pods. Every other workload in the
				// namespace — including every other isolated app — is
				// denied, which is the property that makes promotion
				// worth something.
				NamespaceSelector: from,
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/name": managedByValue},
				},
			}},
		}},
	}

	// Naming an egress rule at all makes egress default-deny, so the rule has
	// to re-allow everything and carve the blocked addresses out with `except`.
	// Expressing it the other way round — a deny rule — is not something
	// NetworkPolicy can say.
	//
	// The same rule also lives in the namespace-wide [egressPolicyName], which
	// is what covers job pods; policies are additive and the rules identical,
	// so carrying it here as well costs nothing and keeps a workload covered
	// if that one object is ever deleted by hand.
	if len(b.cfg.BlockedEgressCIDRs) > 0 {
		spec.PolicyTypes = append(spec.PolicyTypes, networkingv1.PolicyTypeEgress)
		spec.Egress = blockedEgressRules(b.cfg.BlockedEgressCIDRs)
	}

	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.cfg.Namespace, Labels: labels},
		Spec:       spec,
	}
	return b.applyNetworkPolicy(ctx, policy)
}

// egressPolicyName is the one namespace-wide policy that carries the egress
// block for every Toft pod in the apps namespace.
const egressPolicyName = "toft-egress"

// ensureEgressPolicy keeps the namespace-wide egress block in step with the
// configuration: present when BlockedEgressCIDRs is set, absent when it is not.
//
// The per-workload policy selects one release name, and a job pod's workload
// label is its run id — so before this object existed no egress rule selected
// a job pod at all, and a job could read the metadata endpoint that every
// service pod was kept off. Selecting on the managed-by label covers every pod
// Toft creates in the namespace, jobs and services alike, whatever its name.
//
// Deleting it when the list is empty matters as much as creating it: an object
// left behind would keep blocking after the operator switched the block off,
// and nothing else would ever remove it.
func (b *Backend) ensureEgressPolicy(ctx context.Context) error {
	policies := b.client.NetworkingV1().NetworkPolicies(b.cfg.Namespace)
	if len(b.cfg.BlockedEgressCIDRs) == 0 {
		err := policies.Delete(ctx, egressPolicyName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("kube: removing network policy %s: %w", egressPolicyName, err)
		}
		return nil
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: egressPolicyName, Namespace: b.cfg.Namespace,
			Labels: map[string]string{labelManagedBy: managedByValue},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{labelManagedBy: managedByValue},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      blockedEgressRules(b.cfg.BlockedEgressCIDRs),
		},
	}
	return b.applyNetworkPolicy(ctx, policy)
}

// blockedEgressRules re-allows the world and carves the blocked addresses out,
// one peer per address family.
//
// Naming an egress rule at all makes egress default-deny, so a rule with only
// an IPv4 peer silently denies every IPv6 destination: on a dual-stack cluster
// that is all IPv6 egress, and on an IPv6-only one it is everything, DNS and
// the bootstrap callback included, so no workload could start. Each family's
// `except` holds only its own addresses, because the API refuses an except
// that is not inside its CIDR.
func blockedEgressRules(blocked []string) []networkingv1.NetworkPolicyEgressRule {
	var v4, v6 []string
	for _, cidr := range blocked {
		if strings.Contains(cidr, ":") {
			v6 = append(v6, cidr)
		} else {
			v4 = append(v4, cidr)
		}
	}
	return []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{
			// Cluster DNS survives this: traffic to a Service is translated
			// to a pod address before policy is evaluated, and pod addresses
			// are inside the world blocks.
			{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: v4}},
			{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: v6}},
		},
	}}
}

// applyNetworkPolicy creates a policy, or brings an existing one of that name
// up to date with the spec it was given.
func (b *Backend) applyNetworkPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) error {
	name := policy.Name
	spec := policy.Spec
	_, err := b.client.NetworkingV1().NetworkPolicies(b.cfg.Namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kube: creating network policy %s: %w", name, err)
	}
	// Already there, from an earlier start of this workload — rendered under
	// whatever the platform was configured with then. Brought up to date on
	// every start, because a policy written once is how BLOCK_AWS_METADATA
	// switched on later never reached the apps already deployed.
	existing, err := b.client.NetworkingV1().NetworkPolicies(b.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("kube: reading network policy %s: %w", name, err)
	}
	if apiequality.Semantic.DeepEqual(existing.Spec, spec) {
		return nil
	}
	existing.Spec = spec
	if _, err := b.client.NetworkingV1().NetworkPolicies(b.cfg.Namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kube: updating network policy %s: %w", name, err)
	}
	return nil
}

// imagePullGrace is how long a deploy keeps waiting through a pull the kubelet
// is still retrying.
//
// The kubelet backs off 10s, 20s, 40s, so this covers three attempts. The
// asymmetry is what sets it: giving up on the first sighting costs a failed
// deploy that would have worked, while waiting on an image that will never
// arrive costs ninety seconds before an operator is told — and told at
// `image_pull`, with the registry's own words, rather than by the generic
// timeout at the end of waitTimeout.
const imagePullGrace = 90 * time.Second

// waitReady blocks until the pod is running, or reports *why* it is not.
//
// A pending pod carrying a stage is *not* a failure: classify returns
// image_pull that way while the kubelet is still retrying a pull. This is the
// caller that owns the budget for it — the grace starts at the first sighting,
// not at the call, so a pull that begins late is not charged for the scheduling
// that preceded it.
//
// The grace is a parameter rather than the constant read inline so that the
// give-up path is testable in milliseconds; Deploy passes [imagePullGrace] and
// is the only caller.
func (b *Backend) waitReady(ctx context.Context, name string, pullGrace time.Duration) error {
	ticker := time.NewTicker(waitTick)
	defer ticker.Stop()

	var pullingSince time.Time
	var pullMessage string
	// The scheduler's own account of a pod nothing can place, kept from the
	// last tick that had one. **`classify` cannot see it**: it reads container
	// statuses, and an unschedulable pod has none at all — it never gets as far
	// as a container — so every tick falls through to the default and the only
	// thing left to say at the end was that five minutes had passed. The
	// message is on the pod object already fetched, so this costs no call.
	var blocked string

	for {
		pod, err := b.getPod(ctx, name)
		if err == nil {
			if why := unscheduledReason(pod); why != "" {
				blocked = why
			}
			state, stage, _, msg := classify(pod)
			switch {
			case state == runtime.JobRunning || state == runtime.JobSucceeded:
				return nil
			case state.Terminal():
				// Naming the stage is the whole point: an operator must be
				// able to tell an unpullable image from an app that crashed.
				return fmt.Errorf("kube: workload %s failed at %s: %s", name, stage, msg)
			case stage == runtime.FailureImagePull:
				if pullingSince.IsZero() {
					pullingSince = time.Now()
				}
				if msg != "" {
					pullMessage = msg
				}
				if waited := time.Since(pullingSince); waited > pullGrace {
					return fmt.Errorf("kube: workload %s failed at %s after %s of retries: %s",
						name, stage, waited.Round(time.Second), pullMessage)
				}
			case stage != runtime.FailureNone:
				return fmt.Errorf("kube: workload %s failed at %s: %s", name, stage, msg)
			default:
				// The pull recovered, or never happened. A later one starts its
				// own grace rather than inheriting an earlier one's.
				pullingSince, pullMessage = time.Time{}, ""
			}
		} else if !apierrors.IsNotFound(err) && !budgetSpent(ctx, waitTick) {
			return fmt.Errorf("kube: reading pod %s: %w", name, err)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			if !pullingSince.IsZero() {
				// The budget ran out inside a pull. Say that rather than "did
				// not become ready", which sends an operator to the app.
				return fmt.Errorf("kube: workload %s did not become ready within %s: still at %s — %s",
					name, waitTimeout, runtime.FailureImagePull, pullMessage)
			}
			if blocked != "" {
				// **Nothing could place it, and the scheduler said why.** The
				// same shape as the pull above and for the same reason: the
				// bare timeout sends an operator to look at their app, and the
				// app never ran. This sentence — *0/1 nodes are available:
				// persistentvolumeclaim "lpv-x" is being deleted* — is the
				// whole answer, and it existed nowhere a person could reach
				// without kubectl, including in the event a failed start
				// records, which relays exactly this error.
				return fmt.Errorf("kube: workload %s did not become ready within %s: it could not "+
					"be scheduled — %s", name, waitTimeout, blocked)
			}
			return fmt.Errorf("kube: workload %s did not become ready within %s", name, waitTimeout)
		}
	}
}

// waitTick is how often waitReady reads the pod.
const waitTick = 2 * time.Second

// budgetSpent reports whether ctx's deadline is within margin. A read that
// fails then is the timeout arriving, not a failure of its own: client-go's
// rate limiter refuses a call it cannot finish before the deadline ("would
// exceed context deadline") *before* the deadline passes, and returning that
// replaced the scheduler's sentence — the whole answer — in the event a failed
// start records.
func budgetSpent(ctx context.Context, margin time.Duration) bool {
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && time.Until(dl) < margin
}

func (b *Backend) Stop(name string) error {
	// Kubernetes has no "stopped pod" — deleting the pod while leaving the
	// Service and policy in place is the closest thing, and it is what makes
	// a later Restart land on the address callers have persisted.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := b.client.CoreV1().Pods(b.cfg.Namespace).Delete(ctx, objectName(name), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		// No pod holds the name. Said as an absence rather than swallowed as a
		// success, so the shared backend can say the same thing about the same
		// state — both backends answer a missing workload identically, and a
		// caller reads the word rather than the backend.
		return runtime.Absent(runtime.LogAbsentNoWorkload)
	}
	if err != nil {
		return fmt.Errorf("kube: stopping %s: %w", name, err)
	}
	return nil
}

// deletePodAndWait retires the pod holding a name and blocks until the API
// server agrees it is gone, so the create that follows lands on a free name
// rather than on a terminating one.
//
// The Service and the NetworkPolicy are untouched: they are what make the
// workload's address survive a replacement, which is the whole reason a caller
// may persist an endpoint. Delete is still the only thing that tears them down.
// forceDeleteAfter is how long deletePodAndWait waits on an ordinary delete
// before re-issuing it with no grace. A variable so a test can shorten it.
var forceDeleteAfter = terminationGraceSeconds*time.Second + terminatingMargin

func (b *Backend) deletePodAndWait(ctx context.Context, name string) error {
	pods := b.client.CoreV1().Pods(b.cfg.Namespace)

	if err := pods.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("kube: replacing %s: %w", name, err)
	}

	// A pod on a node that stopped answering stays Terminating until the
	// kubelet comes back, which on a single box or a hung-but-present node is
	// never — and because the pod's name is the release name, the restart that
	// is the one manual remedy would wait out the whole deploy timeout and
	// fail. Past the grace and the margin, the delete is re-issued with no
	// grace, which removes the object without waiting for the kubelet.
	forceAt := time.Now().Add(forceDeleteAfter)
	forced := false

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := b.getPod(ctx, name)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("kube: waiting for %s to terminate: %w", name, err)
		}
		if !forced && time.Now().After(forceAt) {
			forced = true
			err := pods.Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("kube: forcing %s out after its grace: %w", name, err)
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("kube: %s did not terminate within %s", name, waitTimeout)
		}
	}
}

func (b *Backend) Delete(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	obj := objectName(name)
	var firstErr error
	record := func(err error) {
		if err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = err
		}
	}

	record(b.client.CoreV1().Pods(b.cfg.Namespace).Delete(ctx, obj, metav1.DeleteOptions{}))
	record(b.client.CoreV1().Services(b.cfg.Namespace).Delete(ctx, obj, metav1.DeleteOptions{}))
	record(b.client.NetworkingV1().NetworkPolicies(b.cfg.Namespace).Delete(ctx, obj, metav1.DeleteOptions{}))
	// And the Job, when the name is a run id. Orphans reports a job pod under
	// its workload name, which is the run id, and none of the three objects
	// above exist for it — the pod is `job-<run>-<suffix>` and belongs to the
	// Job `job-<run>` — so without this the collector reported a job orphan
	// removed every sweep while its pod kept running and kept its claims
	// pinned. Foreground, so the pod goes before the Job does. A release name
	// never has a Job of this name; for it this is one NotFound.
	foreground := metav1.DeletePropagationForeground
	record(b.client.BatchV1().Jobs(b.cfg.Namespace).Delete(ctx, objectName("job-"+name),
		metav1.DeleteOptions{PropagationPolicy: &foreground}))

	if firstErr != nil {
		return fmt.Errorf("kube: deleting %s: %w", name, firstErr)
	}
	return nil
}

// Logs is the tail, reaching one generation back when there is one.
//
// A serve pod runs with RestartPolicy: Always and an isolated container
// restarts *in place* — which is what the projected-token identity bought, and
// what lets kubelet bring a crashed workload back without the platform minting
// anything. The cost is that the container's log starts again from empty each
// time, so a crash loop leaves the log that *explains* the crash one generation
// behind the one an editor is reading. That is the single case where a log view
// most needs to be right, and one extra API call buys it.
//
// Generations 1 through n-2 are gone, and a pod that is *replaced* rather than
// restarted — evicted, OOM-killed at the pod level, drained — takes its log
// with it. Nothing here recovers those; nothing stores them.
func (b *Backend) Logs(name string, lines int) ([]runtime.LogLine, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	current, err := b.tailContainer(ctx, name, lines, false)
	if err != nil {
		return nil, err
	}

	// `lines` is a budget across both generations, spent newest first: the
	// running container is filled to the requested depth and only the
	// remainder goes to history, so a chatty running container never has its
	// own tail cut short to make room.
	budget := lines - len(current)
	if budget <= 0 {
		return current, nil
	}

	// Ask only when the restart count says there is something to fetch. A
	// container that has never restarted is the common case, and the API
	// server answers Previous against one with a 400 — so without this guard
	// the error path below would be exercised by every healthy app rather than
	// by the rare one.
	restarts, err := b.restartCount(ctx, name)
	if err != nil || restarts == 0 {
		return current, nil
	}

	previous, err := b.tailContainer(ctx, name, budget, true)
	if err != nil || len(previous) == 0 {
		// A missing previous generation is not an error. Propagating it would
		// turn "this app has not crashed" into a failed log read.
		return current, nil
	}

	// Two logs concatenated silently are worse than one log: the reader cannot
	// see where the container died. The separator is the platform's own line,
	// produced here rather than by the runner — the runner of the dead
	// generation is gone and cannot narrate its own death.
	out := make([]runtime.LogLine, 0, len(previous)+1+len(current))
	out = append(out, previous...)
	out = append(out, runtime.LogLine{
		Source: runtime.LogSourcePlatform,
		Stream: runtime.LogStreamStdout,
		Line: fmt.Sprintf("── the container restarted here (restart %d); everything above is from the previous run ──",
			restarts),
	})
	out = append(out, current...)
	return out, nil
}

// tailContainer reads one generation of a container's log.
func (b *Backend) tailContainer(ctx context.Context, name string, lines int, previous bool) ([]runtime.LogLine, error) {
	tail := int64(lines)
	req := b.client.CoreV1().Pods(b.cfg.Namespace).GetLogs(objectName(name), &corev1.PodLogOptions{
		TailLines: &tail,
		Previous:  previous,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		if reason := absentLogReason(err); reason != "" {
			return nil, runtime.Absent(reason)
		}
		return nil, fmt.Errorf("kube: reading logs for %s: %w", name, err)
	}
	defer stream.Close()

	var out []runtime.LogLine
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for sc.Scan() {
		if isKubeletLogNotice(sc.Text()) {
			continue
		}
		out = append(out, parseRunnerLine(sc.Text()))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("kube: reading logs for %s: %w", name, err)
	}
	return out, nil
}

// isKubeletLogNotice is the kubelet speaking inside a log stream rather than
// the container: when a generation's log is already gone the API server answers
// 200 with "unable to retrieve container logs for containerd://…" as the body,
// and read as a line it was quoted to an app's author as the last thing their
// app printed (the eks drills, 2026-10-22).
func isKubeletLogNotice(line string) bool {
	return strings.HasPrefix(line, "unable to retrieve container logs for ")
}

// restartCount is how many times this workload's container has been restarted
// in place, which is the same figure Metrics reports and the health watcher
// reads on its sweep.
func (b *Backend) restartCount(ctx context.Context, name string) (int, error) {
	pod, err := b.getPod(ctx, objectName(name))
	if err != nil {
		return 0, err
	}
	for _, cs := range pod.Status.ContainerStatuses {
		return int(cs.RestartCount), nil
	}
	return 0, nil
}

// maxLogLineBytes bounds one line, because a pod log is the app's to write and
// bufio's default 64KiB is small enough that a single long line would end the
// tail early with a scanner error rather than a short line.
const maxLogLineBytes = 1024 * 1024

func (b *Backend) LogStream(name string) (runtime.LogReader, error) {
	// The last *nothing*, then whatever arrives. Without it the API server
	// follows from the beginning of the container log, and the client
	// concatenates that onto the tail it has already rendered — so an isolated
	// app's whole log appeared twice, once as the tail and once as the follow
	// catching up to it. The tail is the history; this is only the part that
	// has not happened yet.
	var fromNow int64
	req := b.client.CoreV1().Pods(b.cfg.Namespace).GetLogs(objectName(name), &corev1.PodLogOptions{
		Follow:    true,
		TailLines: &fromNow,
	})
	stream, err := req.Stream(context.Background())
	if err != nil {
		if reason := absentLogReason(err); reason != "" {
			return nil, runtime.Absent(reason)
		}
		return nil, err
	}
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	return &podLogReader{rc: stream, sc: sc}, nil
}

// podLogReader labels the follow stream the same way the tail is labeled, so a
// line does not change meaning depending on whether the reader arrived before
// or after it was printed.
type podLogReader struct {
	rc io.ReadCloser
	sc *bufio.Scanner
}

func (r *podLogReader) Next() (runtime.LogLine, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return runtime.LogLine{}, err
		}
		return runtime.LogLine{}, io.EOF
	}
	return parseRunnerLine(r.sc.Text()), nil
}

func (r *podLogReader) Close() error { return r.rc.Close() }

// parseRunnerLine reads one line of a pod's log back into its three labels.
//
// The parse belongs here because the marker is this backend's wire format — and
// the shared backend's too, now that the agent's file carries the runner's
// marked lines and AgentRuntime.Logs reads them with the same parser. An
// unmarked line is
// the app's own output on stdout — which is exactly what an older runner's
// output is, every line of it, and why the contract version does not move for
// this.
func parseRunnerLine(text string) runtime.LogLine {
	at, source, stream, line := runner.ParseLogLine(text)
	l := runtime.LogLine{Source: source, Stream: stream, Line: line}
	if !at.IsZero() {
		l.At = &at
	}
	return l
}

// absentLogReason reads an API server error as "there is no log here yet",
// returning "" for anything that is a genuine failure to reach the cluster.
//
// Two shapes mean absence and neither is a server fault. A NotFound is a pod
// that does not exist — the app is stopped, or was never deployed under this
// release's name. A BadRequest is the API server refusing to open a log for a
// container that is not running yet: its message names the waiting state, which
// is the same situation podStatusWord calls `launching`, and answering 500 to
// an app that is pulling its image would be the platform reporting its own
// error for the app's ordinary progress.
func absentLogReason(err error) string {
	switch {
	case apierrors.IsNotFound(err):
		return runtime.LogAbsentNoWorkload
	case apierrors.IsBadRequest(err):
		return runtime.LogAbsentLaunching
	}
	return ""
}

// Metrics reports what the platform can see from the API. CPU and memory are
// deliberately absent: they come from the workload's own control group over
// the callback channel, which is one mechanism behaving identically across
// both isolated backends rather than a different metrics service per cloud.
func (b *Backend) Metrics(name string) (*runtime.AppMetrics, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pod, err := b.getPod(ctx, objectName(name))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("workload %s not found", name)
		}
		return nil, err
	}

	return podMetrics(pod), nil
}

// podMetrics is the one reading of a pod both Metrics and List return.
//
// One function because two drifted: List once built its own copy and left out
// CrashLooping and Detail, and the health watcher builds its whole sweep from
// List — so for isolated apps its crash-loop guard could never match, the
// status flapped between crashed and online across the kubelet's backoff, and
// a pod nothing could schedule was reported without the scheduler's reason.
// The agent backend shares one helper between the two for the same reason.
//
// CPU and Memory are left nil, which is the answer and not an omission: this
// backend takes no reading, and a zero would report every isolated app as
// idle. Filling them means measuring them — see AppMetrics.
func podMetrics(pod *corev1.Pod) *runtime.AppMetrics {
	m := &runtime.AppMetrics{Status: podStatusWord(pod), MemoryLimitMB: memoryLimitMB(pod)}
	for _, cs := range pod.Status.ContainerStatuses {
		m.Restarts = int(cs.RestartCount)
		if cs.State.Running != nil {
			m.Uptime = int64(time.Since(cs.State.Running.StartedAt.Time).Seconds())
		}
		if oomKilled(&cs) {
			m.OOMKilled = true
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			code := int(t.ExitCode)
			m.ExitCode = &code
		}
		// The kubelet's own verdict, not ours. `CrashLoopBackOff` is a
		// container it is holding back before restarting again, which is the
		// fact `internal/health` cannot reach by counting here — see
		// AppMetrics.CrashLooping.
		if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
			m.CrashLooping = true
		}
		if w := cs.State.Waiting; w != nil && m.Detail == "" && w.Message != "" {
			m.Detail = w.Reason + ": " + w.Message
		}
	}
	if m.Detail == "" {
		m.Detail = unscheduledReason(pod)
	}
	return m
}

// unscheduledReason is the scheduler's own account of a pod that is not
// running, or "" when it has none.
//
// **The most useful sentence about a workload that never started is one only
// the cluster can see.** A pod nothing can place carries it on its
// `PodScheduled` condition — a claim being deleted, a node with no room, a
// taint nothing tolerates — and without reading it the platform's account of
// twelve minutes of that was a workload still coming up. The alternative was
// `kubectl describe pod`, which an app's owner does not have.
func unscheduledReason(pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
			return c.Message
		}
	}
	return ""
}

// oomKilled reads the one thing this backend knows for certain about why a
// container went away, and the shared backend can only guess at.
//
// The *last* state, not the current one: a container over its memory limit is
// killed and immediately restarted in place, so by the time any sweep looks the
// container is running again and the termination that explains the restart is
// the one behind it. It stays there for the life of the pod, which is why a
// caller reads it against a restart count that moved rather than on its own.
// memoryLimitMB is the cap this pod was actually created with, read back off
// the spec rather than off the settings the platform holds now: a workload
// still running under a limit an administrator has since raised was killed by
// the old one, and the old one is what explains the restart. The agent reports
// the limit from the description it placed the workload with, for the same
// reason.
//
// 0 for a pod with no memory limit, which is a fact rather than an absence.
func memoryLimitMB(pod *corev1.Pod) int {
	for i := range pod.Spec.Containers {
		if q, ok := pod.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory]; ok {
			return int(q.Value() / (1024 * 1024))
		}
	}
	return 0
}

func oomKilled(cs *corev1.ContainerStatus) bool {
	if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
		return true
	}
	// The container that is down *now* — a pod between restarts, which is what
	// a sweep landing inside the backoff sees.
	return cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled"
}

// podStatusWord maps a pod onto the same vocabulary the shared backend reports,
// so the UI needs no per-backend branch.
//
// **The phase is not enough, and that was measured rather than reasoned about.**
// A pod whose container is crash-looping has phase `Running` — observed on k3s
// while reproducing the bootstrap-credential chain — so a mapping that read the
// phase alone answered `online` for a workload that was down, the health watcher
// never called the app crashed, and nothing recovered it or said why. That is
// the failure mode a serve pod's RestartPolicy: Always brings back, so the
// container's own state is read before the phase is believed.
//
// **Phase and container state are written only by the kubelet**, so a pod on a
// node that stopped reporting keeps reading Running with every container up
// while the gateway gets nothing but 502s. Two facts from outside the kubelet
// are read first for that reason. A pod being deleted is not `online`: it is
// `launching` while it is inside its grace (an ordinary restart, which the
// two-sweep guard must not read as a crash) and `errored` once it is past it
// by [terminatingMargin], which is a node not answering. And a Running pod
// whose Ready condition is False while its containers all run is `errored`:
// no Toft pod has a readiness probe, so the only writer of that False is the
// node controller marking the node unreachable.
func podStatusWord(pod *corev1.Pod) string {
	if pod.DeletionTimestamp != nil {
		// The timestamp is the deadline the grace period ends at, not when
		// the delete was asked for.
		if time.Since(pod.DeletionTimestamp.Time) > terminatingMargin {
			return "errored"
		}
		return "launching"
	}
	switch pod.Status.Phase {
	case corev1.PodRunning:
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionFalse && allContainersRunning(pod) {
				return "errored"
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Running != nil {
				continue
			}
			// Not running, whatever the pod's phase says. Waiting to be
			// restarted after a crash is `errored`; waiting for anything else —
			// a pull, a container being created — is a workload on its way up,
			// which the watcher treats as neither healthy nor a crash.
			if w := cs.State.Waiting; w != nil {
				if w.Reason == "CrashLoopBackOff" {
					return "errored"
				}
				return "launching"
			}
			if cs.State.Terminated != nil {
				return "errored"
			}
		}
		return "online"
	case corev1.PodPending:
		return "launching"
	case corev1.PodSucceeded:
		return "stopped"
	default:
		return "errored"
	}
}

// terminatingMargin is how far past its grace a terminating pod may be before
// it reads as a node that is not answering rather than a pod on its way out —
// and how long deletePodAndWait waits past the grace before forcing.
const terminatingMargin = 30 * time.Second

func allContainersRunning(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running == nil {
			return false
		}
	}
	return true
}

// SystemMetrics is not answerable here, and saying so is the correct answer
// rather than a defect. "The machine the apps run on" is a real, shared,
// finite thing for the shared backend and meaningless for a workload with a
// node's worth of scheduler between it and any hardware.
func (b *Backend) SystemMetrics() (*runtime.SystemMetrics, error) {
	return nil, runtime.ErrNotSupported
}

// Orphans names the workloads whose app is gone. See [runtime.OrphanFinder].
//
// The match is on `toft.dev/app-id`, which every workload carries because
// the identity path already needed it, and never on the workload name: a name
// is a release, and a release tells you nothing about whether the row that
// named it still exists.
//
// `objectName` is applied to the *live* ids rather than reversed out of the
// label, because it is lossy — it lowercases and replaces anything outside
// [a-z0-9.-]. Comparing raw ids against a sanitised label would report every
// app whose id needed sanitising as an orphan, which is the one mistake here
// that deletes something that is serving. Mapping the other way cannot: two
// live ids can only ever collide into one label value, and a collision makes
// this *skip* a workload rather than collect it.
func (b *Backend) Orphans(live map[string]string, grace time.Duration) ([]runtime.Orphan, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: b.ownedSelector(),
	})
	if err != nil {
		return nil, fmt.Errorf("kube: listing workloads: %w", err)
	}

	// Keyed by the label's spelling of the app id, valued by its current
	// release ("" when the caller could not say).
	claimed := make(map[string]string, len(live))
	for id, current := range live {
		claimed[objectName(id)] = current
	}

	var out []runtime.Orphan
	for i := range pods.Items {
		pod := &pods.Items[i]
		workload := workloadNameOf(pod)
		appID := pod.Labels[labelAppID]
		// A workload that does not say which app it belongs to is left alone.
		// Absent is not "no app": it is "this backend cannot tell", and the
		// two must not collapse into a deletion.
		if workload == "" || appID == "" {
			continue
		}
		age := podAge(pod)
		if age < grace {
			continue
		}
		if current, ok := claimed[appID]; ok {
			// A live app's pod is collected only when it is certainly not
			// what serves the app: a service pod (a job pod is the
			// dispatcher's), named something other than a current release
			// the caller was sure of. A retired release whose delete failed
			// at the flip otherwise kept running the old code, with the old
			// environment and any mount revoked since, until the app was
			// deleted.
			if current == "" || workload == current || pod.Labels[labelRunID] != "" {
				continue
			}
			out = append(out, runtime.Orphan{Workload: workload, AppID: appID, Age: age, Superseded: true})
			continue
		}
		out = append(out, runtime.Orphan{Workload: workload, AppID: appID, Age: age})
	}
	return out, nil
}

// podAge is how long the pod has existed, which is deliberately not how long
// its container has been running: a crash-looping orphan restarts its
// container forever and would never grow old enough to be collected.
func podAge(pod *corev1.Pod) time.Duration {
	if pod.CreationTimestamp.IsZero() {
		return 0
	}
	return time.Since(pod.CreationTimestamp.Time)
}

func (b *Backend) List() (map[string]*runtime.AppMetrics, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := b.client.CoreV1().Pods(b.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: b.ownedSelector(),
	})
	if err != nil {
		return nil, fmt.Errorf("kube: listing workloads: %w", err)
	}

	out := map[string]*runtime.AppMetrics{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Keyed by the workload name as the caller supplied it, not by the
		// object name and not by the label — both are sanitised copies, and a
		// caller looking up `_user-guide-7a08302c` would find neither.
		key := workloadNameOf(pod)
		if key == "" {
			continue
		}
		m := podMetrics(pod)
		m.CreatedAt = pod.CreationTimestamp.Time
		out[key] = m
	}
	return out, nil
}
