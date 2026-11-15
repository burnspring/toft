// Package kube is the Kubernetes isolated backend: an app runs as its own pod,
// with its own network namespace, identity, and resource envelope.
//
// All Kubernetes API usage is confined here. Nothing outside this package
// imports client-go, and nothing inside it is visible through the interfaces
// it implements — a caller hands over a WorkloadSpec and receives an Endpoint
// or a run handle, exactly as it does from the shared backend.
//
// The guardrail on scope: Toft is an API client that creates workloads,
// services, and jobs. It provisions no cluster, ships no ingress or service
// mesh, and ensures only that *its own* objects exist and match. It never
// manages drift within them — the cluster reconciles itself, and reimplementing
// that is how an app host turns into a worse GitOps tool.
package kube

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/telemetry"
)

// Config is what an operator supplies. Absent, the backend is not constructed
// at all — there is no half-configured state where promotion is offered and
// then fails at deploy time.
type Config struct {
	// Namespace holds every isolated workload. One namespace rather than one
	// per app: a namespace per app would multiply RBAC and quota objects for
	// no isolation gain, since the network policy is per-pod either way.
	Namespace string
	// PlatformNamespace is where the platform's own pods run, and the only
	// namespace a workload's NetworkPolicy admits traffic from. In-cluster,
	// New reads it from the service-account mount; it is empty only when the
	// platform runs outside the cluster, and then the policy falls back to a
	// toft-labelled pod in any namespace.
	PlatformNamespace string
	// KubeConfig and Context point at a cluster from outside. Empty means the
	// in-cluster service account, which is the production shape — Toft
	// runs inside the cluster it orchestrates, so callback reachability and
	// service discovery are trivial.
	KubeConfig string
	Context    string

	// CallbackURL is how a workload reaches the platform. A cluster-internal
	// address, not BASE_URL: that is a browser-facing name which may not
	// resolve from a pod.
	CallbackURL string
	// RunnerImages maps a language family to its image.
	RunnerImages map[string]string

	// ServicePortName is the port name Services expose. Named rather than
	// numeric so a workload's own port can change without the Service.
	ServicePortName string

	// --- Durable storage ---
	//
	// Absent configuration means the capability is not offered. Nothing here
	// creates a filesystem or a bucket: an operator's infrastructure module
	// makes those, and Toft binds to them.

	// StorageClass is the class EFS-backed file stores are provisioned from —
	// one an operator created against a shared filesystem, with
	// provisioningMode: efs-ap and reclaimPolicy: Retain. Empty means that
	// backing is not offered; object stores need no class and are unaffected.
	//
	// It is the install's *default* class rather than its only one: a resource
	// may name another the operator created, which is what lets one install
	// offer both backings at once.
	StorageClass string
	// S3FilesStorageClass is the same thing for an S3 Files-backed file store:
	// a class the operator created against an Amazon S3 Files file system,
	// through the same EFS CSI driver. A second setting rather than a list,
	// because the platform has to know which backing a class provides and a
	// class name cannot tell it — the operator says which is which, once, and
	// a resource that names neither gets its backing's default.
	//
	// Empty means S3 Files is not offered, and the create form does not show
	// it: absent, not dormant.
	S3FilesStorageClass string
	// ExtraStorageClasses is KUBE_STORAGE_CLASSES, parsed: classes beyond the
	// two defaults a resource may name.
	ExtraStorageClasses []runtime.DeclaredClass
	// StoreCSIDriver mounts `mountpoint` object stores. Defaulted rather than
	// required: the Mountpoint driver has one name, and making every install
	// restate it would be configuration for its own sake.
	//
	// It names the S3 one of two. A `blob` store mounts through
	// BlobCSIDriver instead, which is not the same object with a different
	// value in it — the volume attributes and the mount options differ as much
	// as the name does, which is why the backing selects the whole set rather
	// than this string selecting a driver.
	StoreCSIDriver string
	// StoreRegion is passed to the mount when the bucket is not in the driver's
	// own default region. S3 only; Azure derives the account's region itself.
	StoreRegion string
	// StoreAccount is the Azure storage account `blob` stores live in. Empty
	// means that backing is not offered — absent, not dormant, and the create
	// form does not show it.
	StoreAccount string

	// BlockedEgressCIDRs are addresses an app workload may not reach, enforced
	// as a NetworkPolicy egress rule.
	//
	// This is the isolated half of BLOCK_AWS_METADATA. The shared half is an
	// iptables rule keyed on the app accounts' group (docker/entrypoint.sh);
	// neither mechanism works in the other's mode, and the invariant they both
	// serve is the same one — a deployed app must not be able to read the
	// instance or task IAM credentials and thereby help itself to the storage
	// its mounts were supposed to mediate.
	//
	// Empty leaves egress unrestricted, which is the default: apps legitimately
	// reach internal databases and third-party APIs, and a policy that guessed
	// at those would break more than it protected.
	BlockedEgressCIDRs []string
}

// MetadataCIDRs are the endpoints that hand out cloud credentials to anything on
// the box that asks: the EC2 instance metadata service, the ECS task metadata
// endpoint, and — at the same first address — Azure's IMDS. Blocking them is
// what stops a deployed app inheriting the identity its node runs as.
//
// The list is already right on Azure, which is why only the symbol's name
// changed and the environment variable did not: `BLOCK_AWS_METADATA` is in every
// existing .env, every values.yaml and every deploy document, and renaming it
// would be a flag day paid for a word (ADR §84). 169.254.170.2 is ECS-only and
// costs nothing elsewhere.
//
// **168.63.129.16 is deliberately absent.** Azure's wireserver carries DNS and
// the platform health probe and hands out no credential; blocking it would break
// name resolution for every app on the node. A CIDR list that closed a door
// nobody uses and broke DNS would be the most expensive kind of correct.
//
// fd00:ec2::254 is the EC2 metadata service's IPv6 address, reachable from a
// Nitro instance with an IPv6 address; it lands in the policy's IPv6 peer.
var MetadataCIDRs = []string{"169.254.169.254/32", "169.254.170.2/32", "fd00:ec2::254/128"}

// The CSI drivers that mount an object store, one per backing.
//
// DefaultStoreCSIDriver is Mountpoint for Amazon S3 and is what an unset
// KUBE_STORE_CSI_DRIVER means. BlobCSIDriver is Azure's, and it is a constant
// rather than a setting because a second knob would let an operator point the
// `blob` backing at a driver whose mount options mean something else.
const (
	DefaultStoreCSIDriver = "s3.csi.aws.com"
	BlobCSIDriver         = "blob.csi.azure.com"
)

// Backend implements runtime.ServiceRuntime and runtime.JobRuntime against a
// Kubernetes cluster.
type Backend struct {
	client kubernetes.Interface
	cfg    Config
}

// WorkloadServiceAccount is the account every workload pod runs as, created by
// the chart in the apps namespace and holding no RBAC. It is a name, not a
// permission: it is what a TokenReview's username has to be for the platform to
// believe a caller is one of its own workloads.
const WorkloadServiceAccount = "toft-workload"

// IdentityAudience scopes the projected token. The API server rejects a token
// bearing it, and the platform accepts nothing else — which is what keeps a
// credential app code can read from being a credential against the cluster.
const IdentityAudience = "toft-bootstrap"

// identityTokenTTLSeconds is how long each projection lives before kubelet
// rotates it. Ten minutes: short enough that a copied token is stale by the
// time it is useful anywhere, and rotation is the infrastructure's job rather
// than something a pod spec has to survive.
const identityTokenTTLSeconds = 600

// containerPort is the port every isolated workload listens on inside its own
// network namespace. It is fixed because there is nothing to collide with: the
// pod has the namespace to itself, which is exactly the property that makes
// port allocation meaningless for an isolated backend.
const containerPort = 8080

// runnerUID is the account the runner image runs as. It has to be known here,
// not only in the Dockerfile: Kubernetes verifies non-root by UID and cannot
// read a username out of an image, so a mismatch between the two shows up as
// a container that will not start.
const runnerUID = 10001

// terminationGraceSeconds is how long a pod has between SIGTERM and SIGKILL.
//
// Kubernetes' own default is 30 and this is deliberately the same number — the
// point is not to change it but to own it, because it decides two things this
// package is answerable for: how long an in-flight request survives a stop, and
// the floor on the gap a replacement opens. A default inherited from the
// platform underneath is a number nobody here would notice moving.
const terminationGraceSeconds = 30

// scratchSizeLimit caps the scratch emptyDir. Deliberately a constant, not
// yet a config knob: scratch is working space for uploads-in-flight, and an
// app that needs more has outgrown ephemerality, not the limit.
var scratchSizeLimit = resource.MustParse("1Gi")

// runtimeDefaultSeccomp is the container runtime's own default profile.
func runtimeDefaultSeccomp() *corev1.SeccompProfile {
	return &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
}

// serviceAccountNamespaceFile is where the kubelet writes the namespace of the
// pod a process runs in, beside its service-account token.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func New(cfg Config) (*Backend, error) {
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("kube: a namespace is required")
	}
	if cfg.CallbackURL == "" {
		return nil, fmt.Errorf("kube: a callback URL is required — a pod cannot resolve a browser-facing BASE_URL")
	}
	cfg = withDefaults(cfg)

	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building client: %w", err)
	}
	if cfg.PlatformNamespace == "" && cfg.KubeConfig == "" && cfg.Context == "" {
		// In-cluster, the platform's own namespace is a fact the kubelet
		// wrote down; nothing for an operator to restate.
		if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
			cfg.PlatformNamespace = strings.TrimSpace(string(b))
		}
	}
	if cfg.PlatformNamespace == "" {
		log.Printf("kube: the platform's own namespace is unknown (it is not running in the cluster), " +
			"so each workload admits a pod labelled app.kubernetes.io/name=toft from any namespace")
	}
	return &Backend{client: client, cfg: cfg}, nil
}

// NewWithClient is the seam the fast tests use: a fake clientset asserts what
// the backend *sends* to the API in milliseconds, which is a different tier of
// test from running an actual workload.
func NewWithClient(client kubernetes.Interface, cfg Config) *Backend {
	return &Backend{client: client, cfg: withDefaults(cfg)}
}

// withDefaults fills the settings that have one correct answer, so both
// constructors agree and a test client behaves like a real one.
func withDefaults(cfg Config) Config {
	if cfg.ServicePortName == "" {
		cfg.ServicePortName = "http"
	}
	if cfg.StoreCSIDriver == "" {
		cfg.StoreCSIDriver = DefaultStoreCSIDriver
	}
	return cfg
}

func restConfig(cfg Config) (*rest.Config, error) {
	if cfg.KubeConfig == "" && cfg.Context == "" {
		// In-cluster: the production shape.
		if c, err := rest.InClusterConfig(); err == nil {
			return c, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.KubeConfig != "" {
		rules.ExplicitPath = cfg.KubeConfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: no usable cluster configuration: %w", err)
	}
	return c, nil
}

// --- Naming ---

// Kubernetes object names are DNS labels: lowercase alphanumerics and dashes,
// 63 characters. A release process name is close but not guaranteed, so it is
// normalised once here rather than at each call site.
func objectName(workloadName string) string {
	name := strings.ToLower(workloadName)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.':
			b.WriteByte('-')
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "workload"
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// Labels let the backend find its own objects again after a restart, and let
// an operator see at a glance what Toft owns in their namespace.
//
// A label value is a DNS-label-shaped thing, so every label here is written
// through objectName and is *lossy*: `_user-guide-7a08302c` becomes
// `user-guide-7a08302c`. That is fine for a selector and wrong for anything
// that has to answer with the name the platform handed in — the identity a
// bootstrapping pod is matched against, the key a List() reading is filed
// under, the workload an orphan is reported as. Those read the annotation,
// which carries the name as given; a label is only ever a selector.
//
// The first casualty was every first-party guide on an isolated install: the
// runner presented `_user-guide-…` from its environment, the platform answered
// with the sanitised label, the replay guard called that a different workload,
// and the pod exited on a 401 nothing logged.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelWorkload  = "toft.dev/workload"
	labelAppID     = "toft.dev/app-id"
	// labelAppSlug is for a cluster's log agent to tag an app's lines with a
	// name a person reads; no selector uses it.
	labelAppSlug   = "toft.dev/app-slug"
	labelRunID     = "toft.dev/run-id"
	labelStorage   = "toft.dev/storage"
	managedByValue = "toft"

	// annotationWorkload carries the workload name exactly as the platform
	// gave it. An annotation value has no shape rule, so it survives what the
	// label cannot.
	annotationWorkload = "toft.dev/workload-name"
)

// annotationsFor is the pod's own copy of the facts a label cannot hold.
func (b *Backend) annotationsFor(spec runtime.WorkloadSpec) map[string]string {
	return map[string]string{annotationWorkload: spec.ProcessName()}
}

// workloadNameOf answers with the name the platform handed the backend, which
// is what every caller keys by. The label is the fallback for a pod created
// before the annotation existed, and is only wrong for a name objectName had
// to change.
func workloadNameOf(pod *corev1.Pod) string {
	if name := pod.Annotations[annotationWorkload]; name != "" {
		return name
	}
	return pod.Labels[labelWorkload]
}

func (b *Backend) labelsFor(spec runtime.WorkloadSpec, runID string) map[string]string {
	l := map[string]string{
		labelManagedBy: managedByValue,
		labelWorkload:  objectName(spec.ProcessName()),
		labelAppID:     objectName(spec.AppID),
	}
	if spec.Slug != "" {
		l[labelAppSlug] = objectName(spec.Slug)
	}
	if runID != "" {
		l[labelRunID] = objectName(runID)
	}
	return l
}

func (b *Backend) ownedSelector() string {
	return labelManagedBy + "=" + managedByValue
}

// --- Pod construction ---

// image picks the runner image for a framework's language family. An
// unconfigured family is an error rather than a default: guessing would
// produce a pod that pulls, starts, and then fails at exec for a reason nobody
// traces back to configuration.
func (b *Backend) image(framework string) (string, error) {
	family := languageFamily(framework)
	img, ok := b.cfg.RunnerImages[family]
	if !ok || img == "" {
		return "", fmt.Errorf("kube: no runner image configured for the %s family (framework %q)", family, framework)
	}
	return img, nil
}

// LanguageFamilyForTest exposes the family map to the one test outside this
// package that has to read it: internal/builder's table over every site that
// switches on `app.framework`, which is the specification of that list and
// would be worth less with a hole in it.
//
// Exported rather than duplicated, because a copy of this switch is exactly the
// thing the test exists to prevent.
func LanguageFamilyForTest(framework string) string { return languageFamily(framework) }

// languageFamily maps a framework to the image family that runs it. One image
// per family, never per framework and never per app.
func languageFamily(framework string) string {
	switch framework {
	case "flask", "fastapi", "streamlit", "python", "marimo", "notebook", "batch-python":
		// `notebook` is python and not static, and the tiny static image is the
		// tempting wrong answer. Three reasons, in increasing severity. The
		// release carries a venv, and Unrunnable's repair needs an interpreter
		// present to rebuild one — a venv-carrying release in an image with no
		// Python is the exact failure ADR §44 exists to catch. Jobs and
		// services share podSpec, so the static family would silently make a
		// notebook app unable to run a job, and a notebook is the app most
		// likely to want one. And a static image is not a capability: it is a
		// size optimisation for a release that genuinely has no runtime, which
		// this one does not.
		//
		// Stated rather than designed around: a notebook's serving pod pulls
		// the Python image to run a file server, and its artifact carries a
		// venv nothing at run time opens.
		return "python"
	case "go", "batch-go":
		return "go"
	case "shiny", "plumber", "rmarkdown", "quarto", "batch-r":
		// R's four frameworks. `languageFamily` stays a pure function of the
		// framework string, which is the whole reason the .qmd engine question
		// is settled in *detection* rather than here: this is called from
		// podSpec, which holds a WorkloadSpec and no release directory, for
		// both the service pod and the job pod. Making the family a per-app
		// question would mean threading a release into pod construction for one
		// file format.
		return "r"
	case "static":
		// A family of its own rather than the node default, and rather than the
		// borrowed Go image the phrase "Go is small" would talk somebody into.
		// The Go *binary* is small; the Go family *image* is the largest one
		// published, because it both builds and runs and so carries the whole
		// toolchain. A static workload installs nothing, repairs nothing,
		// fetches its release in pure Go and identifies its user numerically —
		// so its image is an empty base with the runner binary in it, and an
		// install that configured only RUNNER_IMAGE_GO is not told it needs a
		// Node image to serve an HTML file containing no JavaScript.
		return "static"
	default: // node, nextjs, and the empty default
		return "node"
	}
}

// podSpec renders the container for a workload.
//
// Only identity and pointers travel in the environment: the callback URL, the
// mode, and a one-shot bootstrap token. Everything sized — the app's decrypted
// environment, the artifact URL, the job's command and parameters — arrives in
// the bootstrap *response*. That keeps secrets out of the pod spec, which is
// readable by anyone with get-pod in the namespace.
//
// ctx carries the platform's trace context, and TRACEPARENT is the *only* thing
// this subsystem takes from it (ADR §31 D9). No OTLP endpoint, no headers, no
// SDK in the runner image: giving a workload an exporter means shipping the
// operator's collector credential into it, which D8 refuses in the same breath
// as it refuses os.Environ(). A W3C trace id is neither a secret nor a
// credential, and it is what lets an operator who *has* instrumented their own
// job code get one connected trace instead of two unrelated ones.
func (b *Backend) podSpec(ctx context.Context, spec runtime.WorkloadSpec, mode, runID string) (*corev1.PodSpec, error) {
	img, err := b.image(spec.Framework)
	if err != nil {
		return nil, err
	}

	// Memory is request *and* limit from one number, because over its memory
	// limit a container is killed and restarted — a loud, visible outcome that
	// an operator can see and an app's people are told about. CPU is not that:
	// over a CPU limit a container is *throttled*, silently and with nothing
	// written down, so the two dials are separate and only one of them is on
	// by default. See config.DefaultAppCPURequestMillicores.
	resources := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{},
		Requests: corev1.ResourceList{},
	}
	if spec.MemoryLimitMB > 0 {
		resources.Limits[corev1.ResourceMemory] = memoryQuantity(spec.MemoryLimitMB)
		resources.Requests[corev1.ResourceMemory] = memoryQuantity(spec.MemoryLimitMB)
	}
	if spec.CPURequestMillicores > 0 {
		resources.Requests[corev1.ResourceCPU] = cpuQuantity(spec.CPURequestMillicores)
	}
	if spec.CPULimitMillicores > 0 {
		resources.Limits[corev1.ResourceCPU] = cpuQuantity(spec.CPULimitMillicores)
	}
	// An empty map and an absent one are the same thing to the API server, but
	// not to a test reading the spec back, and not to `kubectl describe`, which
	// prints an empty Limits block as if somebody meant it.
	if len(resources.Limits) == 0 {
		resources.Limits = nil
	}
	if len(resources.Requests) == 0 {
		resources.Requests = nil
	}

	// Durable storage. The volumes and the mount points are built from one
	// slice, and TOFT_STORAGE is rendered from that same slice — so what
	// an app is told it has and what is actually mounted cannot disagree.
	volumes, mountPoints := storageVolumes(spec.Mounts)

	// The ephemeral scratch mount — every workload, not part of the opt-in
	// storage machinery above. An emptyDir lives exactly as long as its pod,
	// and Restart on this backend is a pod recreation, so wipe-on-restart
	// needs no code here: it is the pod lifecycle. The pod SecurityContext's
	// FSGroup makes it writable by the runner UID.
	volumes = append(volumes, corev1.Volume{
		Name: "scratch",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &scratchSizeLimit},
		},
	})
	mountPoints = append(mountPoints, corev1.VolumeMount{
		Name: "scratch", MountPath: runner.ScratchMountPath,
	})

	// The workload's identity, issued by kubelet rather than by the platform.
	// A projected ServiceAccount token, bound to this pod, scoped to an
	// audience the API server itself refuses, and rotated before it expires —
	// so nothing in this spec has a deadline, which is what makes a container
	// restart work at all.
	volumes = append(volumes, corev1.Volume{
		Name: "identity",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Audience:          IdentityAudience,
						ExpirationSeconds: ptr(int64(identityTokenTTLSeconds)),
						Path:              "token",
					},
				}},
			},
		},
	})
	mountPoints = append(mountPoints, corev1.VolumeMount{
		Name: "identity", MountPath: runner.IdentityMountPath, ReadOnly: true,
	})

	env := []corev1.EnvVar{
		{Name: "TOFT_CALLBACK_URL", Value: b.cfg.CallbackURL},
		{Name: "TOFT_RUNNER_MODE", Value: mode},
		{Name: "TOFT_WORKLOAD_NAME", Value: spec.ProcessName()},
	}
	if tp := telemetry.TraceParent(ctx); tp != "" {
		env = append(env, corev1.EnvVar{Name: "TRACEPARENT", Value: tp})
	}
	if len(spec.Mounts) > 0 {
		// Not a secret — names, paths, and levels — so it belongs in the pod
		// spec beside the mounts rather than in the bootstrap response, where
		// it would be computed a second time from a mapping that may have moved
		// in between.
		env = append(env, corev1.EnvVar{
			Name: runner.EnvStorage, Value: runtime.DescribeMounts(spec.Mounts),
		})
	}

	container := corev1.Container{
		Name:  "workload",
		Image: img,
		// Stated rather than inherited, for the reason
		// TerminationGracePeriodSeconds is below. With no policy Kubernetes
		// picks one from the *tag*: `IfNotPresent` for a pinned tag and
		// `Always` for anything tagged `latest`. An install whose runner images
		// are side-loaded (`k3s ctr images import`) has no registry serving
		// them at all, so inheriting `Always` for one operator's tag choice
		// means every deploy fails at image_pull against a registry that has
		// never held the image. The platform pins its own images by version and
		// never publishes `latest`, so present-is-current holds.
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             env,
		VolumeMounts:    mountPoints,
		Resources:       resources,
		SecurityContext: &corev1.SecurityContext{
			// An isolated workload already has its own namespace and envelope;
			// running privileged inside it would hand a compromised app the
			// container's full capability set for free.
			AllowPrivilegeEscalation: ptr(false),
			RunAsNonRoot:             ptr(true),
			// The UID must be stated numerically. RunAsNonRoot alone makes the
			// kubelet *verify* the user is non-root, and it cannot do that
			// from a USER directive naming an account — it refuses the
			// container with CreateContainerConfigError rather than guessing.
			// Setting it here means the guarantee holds even for an image
			// whose USER is a name.
			RunAsUser:    ptr(int64(runnerUID)),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			// The runtime's default syscall filter, stated. Unset, containerd
			// and CRI-O run the container unconfined unless the cluster turned
			// on SeccompDefault — arbitrary app code with the whole syscall
			// table against the node's kernel. It is also the one field Pod
			// Security `restricted` requires that this spec lacked, which is
			// why the apps namespace can enforce that level.
			SeccompProfile: runtimeDefaultSeccomp(),
		},
	}

	if mode == "serve" {
		container.Ports = []corev1.ContainerPort{{
			Name:          b.cfg.ServicePortName,
			ContainerPort: containerPort,
		}}
	}

	return &corev1.PodSpec{
		Containers: []corev1.Container{container},
		// A workload's own filesystem is still ephemeral: nothing an app writes
		// outside a mapped path survives a reschedule, and an app persisting to
		// local disk is relying on an accident. Durable state goes on a mapped
		// volume or store, which is what Volumes below are.
		Volumes: volumes,
		// A serve pod is restarted in place; a job is not, and the difference is
		// what each failure means.
		//
		// A job has an exit code. Retries are the queue's business, not the
		// kubelet's — otherwise a failing job would retry invisibly, outside the
		// run's own accounting.
		//
		// A serve pod is Always *because nothing in its spec expires any more*.
		// While the spec carried a bootstrap token with fifteen minutes on it,
		// an in-place restart re-execed the runner against a dead credential and
		// could never succeed; asking for one manufactured CrashLoopBackOff and
		// a 401, and — worse — hid the outage, because a looping pod's phase
		// stays Running and this backend maps phase to status. The identity is
		// now a projected token kubelet rotates, so a restarted container reads
		// a current one, and the ten-second in-place restart is the fastest
		// recovery available. podStatusWord is what keeps the loop visible;
		// internal/supervisor remains the backstop for everything no in-pod
		// restart can reach — an evicted pod, a lost node, a deleted pod.
		RestartPolicy: restartPolicyFor(mode),
		// Stated rather than inherited. This is how long an in-flight request
		// has to finish once a stop or a replacement has begun, and leaving it
		// to Kubernetes' 30-second default puts the number that decides that in
		// somebody else's code — where nothing here would notice it changing.
		// It is also the floor on how long a restart's gap lasts, which is the
		// interruption a replacement deliberately accepts.
		TerminationGracePeriodSeconds: ptr(int64(terminationGraceSeconds)),
		// The account exists to be a *name*, and holds no RBAC whatsoever. If it
		// ever acquires a permission, the audience scoping on the projected
		// token above stops being the whole of the argument.
		ServiceAccountName: WorkloadServiceAccount,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr(true),
			RunAsUser:    ptr(int64(runnerUID)),
			// The release tree is unpacked at runtime, so the group must be
			// able to write the workspace the image created.
			FSGroup:        ptr(int64(runnerUID)),
			SeccompProfile: runtimeDefaultSeccomp(),
		},
		// Nothing mounts the *default* service-account token, and that is not a
		// technicality now that the pod names an account. Automount would grant
		// the token whose audience is the API server; the projected volume
		// above grants one whose audience is IdentityAudience and nothing else,
		// which the API server refuses for its own. So a workload pod still
		// holds no credential that works against the cluster — which is the
		// thing this line was always protecting.
		AutomountServiceAccountToken: ptr(false),
		// No `<SVC>_SERVICE_HOST` variables. Every isolated release has a
		// Service in this one namespace, so the default hands each container
		// the name and ClusterIP of every other app's release — readable by app
		// code through the runner's /proc/<pid>/environ — and at a few thousand
		// Services the list alone breaks exec. Nothing reads them.
		EnableServiceLinks: ptr(false),
	}, nil
}

func restartPolicyFor(mode string) corev1.RestartPolicy {
	if mode == "job" {
		return corev1.RestartPolicyNever
	}
	return corev1.RestartPolicyAlways
}

func ptr[T any](v T) *T { return &v }

// --- Status mapping ---

// classify turns a pod's state into the platform's own vocabulary.
//
// The distinctions matter more than the mapping: OOMKilled is
// resource_exceeded rather than a generic failure, and an image that will
// never pull is image_pull rather than a mysterious pending. Collapsing them
// sends a developer to debug the wrong thing.
func classify(pod *corev1.Pod) (runtime.JobState, runtime.FailureStage, int, string) {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return runtime.JobSucceeded, runtime.FailureNone, 0, ""
	case corev1.PodPending:
		// A pull that will never succeed looks like Pending forever unless the
		// waiting reason is read.
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "ErrImagePull", "ImagePullBackOff":
					// **Pending, with a stage: still pulling, and here is
					// why.** `ErrImagePull` is by definition the *first* failed
					// attempt and `ImagePullBackOff` its throttled repeat — the
					// kubelet retries at 10s, 20s, 40s and on. Reading the
					// first sighting as terminal aborted deploys about two
					// seconds into a pull that would have landed well inside
					// the caller's budget, and the next deploy then succeeded
					// unchanged, which is the signature this leaves.
					//
					// The stage travels with the pending state so a caller can
					// say what it is waiting for and give up naming it. Each
					// caller owns its own budget, because they differ: a job
					// records the stage and stays pending, a deploy waits a
					// stated grace.
					return runtime.JobPending, runtime.FailureImagePull, 0, w.Message
				case "InvalidImageName":
					// The one pull failure that no retry can help: the string
					// is not a reference. Terminal on first sight.
					return runtime.JobInfrastructureErr, runtime.FailureImagePull, 0, w.Message
				case "CreateContainerConfigError", "CreateContainerError":
					// Pending, with a stage, like a pull: the kubelet retries
					// both, and a retry that lands starts the container. Read
					// as terminal, a job run was recorded failed while its Job
					// stayed in the cluster — free to start late with the
					// app's environment, or never to finish and pin its
					// claims (K8-4). A deploy still fails fast on the stage
					// (waitReady), and a job stays pending, stage visible,
					// until its stored deadline cancels it.
					return runtime.JobPending, runtime.FailureExec, 0, w.Message
				}
			}
		}
		return runtime.JobPending, runtime.FailureNone, 0, ""
	case corev1.PodRunning:
		return runtime.JobRunning, runtime.FailureNone, 0, ""
	case corev1.PodFailed:
		// Fall through to the per-container inspection below.
	default:
		// An empty or Unknown phase is *not* a failure. A pod read back
		// immediately after creation has no phase yet, and treating that as
		// failed would abort a perfectly good deploy the instant it started —
		// the caller must keep waiting, not give up.
		return runtime.JobPending, runtime.FailureNone, 0, ""
	}

	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil {
			continue
		}
		if t.Reason == "OOMKilled" {
			return runtime.JobResourceExceeded, runtime.FailureExec, int(t.ExitCode),
				"the workload exceeded its memory limit and was killed"
		}
		return runtime.JobFailed, runtime.FailureExec, int(t.ExitCode), t.Message
	}
	return runtime.JobFailed, runtime.FailureExec, 1, pod.Status.Message
}

// waitTimeout bounds how long a caller waits for a workload to be addressable.
// A cold start on a cluster is tens of seconds — an image pull plus an
// artifact fetch — so this is deliberately generous.
const waitTimeout = 5 * time.Minute

func (b *Backend) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	return b.client.CoreV1().Pods(b.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
}
