package runtime

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// This file is the seam. Two host assumptions used to leak past the runtime
// interface: *where the code lives* (callers built a filesystem path) and
// *where traffic goes* (callers built a loopback address from a port they had
// allocated themselves). Both are now behind types a backend owns:
//
//	ReleaseRef — an opaque handle to a built release, resolved only by a backend.
//	Endpoint   — where a started workload can be reached, *returned* by the
//	             backend that started it and never guessed by a caller.
//
// A backend that runs workloads somewhere other than this machine — a task, a
// pod — can therefore be added without touching the proxy, the cron trigger,
// the readiness wait, or the reconciler.

// --- Where the code lives ---

// ReleaseKind names the resolvable forms of a release reference. The set is
// closed on purpose: a backend switches on it exhaustively, so a new form is a
// compile-time prompt to teach every backend about it.
type ReleaseKind string

const (
	// ReleaseLocal is a directory on the machine running the backend. This is
	// what the shared backend produces and consumes today.
	ReleaseLocal ReleaseKind = "local"
	// ReleaseArtifact is a packed release tree in the artifact store, fetched
	// and unpacked by the backend before the workload starts.
	ReleaseArtifact ReleaseKind = "artifact"
)

// ReleaseRef is an opaque handle to a built release. Callers pass one around
// and persist it; only a backend turns it into something on a disk. Treating a
// release as a *path* is what made a release mean "a directory on this
// machine" — the assumption that makes releases non-durable under containers
// and makes a second execution mode inexpressible.
type ReleaseRef struct {
	Kind  ReleaseKind `json:"kind"`
	Value string      `json:"value"`
}

// LocalRelease references a directory on this machine.
func LocalRelease(dir string) ReleaseRef {
	return ReleaseRef{Kind: ReleaseLocal, Value: dir}
}

// ArtifactRelease references a packed release tree by its artifact-store key.
func ArtifactRelease(key string) ReleaseRef {
	return ReleaseRef{Kind: ReleaseArtifact, Value: key}
}

func (r ReleaseRef) IsZero() bool { return r.Kind == "" && r.Value == "" }

func (r ReleaseRef) String() string {
	if r.IsZero() {
		return ""
	}
	return string(r.Kind) + ":" + r.Value
}

// ParseReleaseRef reads back the String form. An empty string is the zero ref,
// and a bare value with no prefix is read as a local path so that references
// persisted before the artifact store existed still resolve.
func ParseReleaseRef(s string) ReleaseRef {
	if s == "" {
		return ReleaseRef{}
	}
	for _, kind := range []ReleaseKind{ReleaseLocal, ReleaseArtifact} {
		prefix := string(kind) + ":"
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			return ReleaseRef{Kind: kind, Value: s[len(prefix):]}
		}
	}
	return LocalRelease(s)
}

// ReleaseResolver turns a release reference into a directory the backend can
// exec out of. The shared backend holds one; it is the single place that knows
// a release can also be something other than a local directory.
type ReleaseResolver interface {
	Resolve(ref ReleaseRef) (dir string, err error)
}

// PrivateReleaseResolver is a resolver that can also place a release where the
// caller says, for a workload that must not share the app's release tree.
//
// A host job is that workload. Resolving an artifact reference replaces the
// release directory with a verified copy from the store — the directory the
// app's own service is executing out of — and a job started beside a live app
// did exactly that: it deleted the serving tree under the running process,
// lost the race with it half way, and left a release the next restart could
// not start. A run's copy lives in the run's own directory and goes with it.
//
// dest must not exist yet. A reference that is already a directory (a local
// release) is returned as it is: there is nothing to fetch, and nothing is
// replaced.
type PrivateReleaseResolver interface {
	ResolvePrivate(ref ReleaseRef, dest string) (dir string, err error)
}

// ErrUnresolvableRelease is returned for a reference a resolver cannot handle —
// an artifact reference on an install with no artifact store, for instance.
var ErrUnresolvableRelease = errors.New("release reference cannot be resolved")

// LocalResolver handles local references only. It is the resolver a VM install
// uses, where every release is already a directory and there is no object
// store to fetch from.
type LocalResolver struct{}

func (LocalResolver) Resolve(ref ReleaseRef) (string, error) {
	switch ref.Kind {
	case ReleaseLocal:
		if ref.Value == "" {
			return "", fmt.Errorf("%w: local reference has no path", ErrUnresolvableRelease)
		}
		return ref.Value, nil
	case "":
		return "", fmt.Errorf("%w: no release reference given", ErrUnresolvableRelease)
	default:
		return "", fmt.Errorf("%w: %s references need an artifact store", ErrUnresolvableRelease, ref.Kind)
	}
}

// --- Where traffic goes ---

// Endpoint is where a started workload accepts requests. It is *returned* by
// the backend that started the workload and persisted by the caller; nothing
// outside a backend may construct one from a port it believes an app has.
type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (e Endpoint) IsZero() bool { return e.Host == "" && e.Port == 0 }

// Addr is the "host:port" form, suitable for net.Dial.
func (e Endpoint) Addr() string { return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

// URL is the http origin of the workload, with no trailing slash.
func (e Endpoint) URL() string { return "http://" + e.Addr() }

// --- Bind addresses ---

const (
	// BindLoopback is the only address a *shared* workload may bind. It runs
	// inside Toft's own network namespace, so a port on all interfaces
	// would be reachable without passing the authorizing gateway.
	BindLoopback = "127.0.0.1"
	// BindAll is correct for an *isolated* workload, where the network
	// namespace is the boundary and nothing but the gateway can route to it.
	// The invariant — an app is unreachable without passing the gateway — is
	// unchanged; only the mechanism enforcing it is mode-specific.
	BindAll = "0.0.0.0"
)

// --- The spec ---

// WorkloadSpec is everything a backend needs to run one workload, and nothing
// about where it runs. The service and job lifecycles stay separate interfaces
// — a service has an endpoint and is restarted, a job has an exit code and is
// not — but both consume this one spec, which is what stops the release and
// bootstrap machinery being written twice.
type WorkloadSpec struct {
	AppID string
	Slug  string
	// Name identifies this workload to the backend. Releases use distinct
	// names so a new release can run alongside the old one until the gateway
	// is flipped over to it.
	Name string
	// Release is where the built code lives. Opaque here; resolved by the
	// backend.
	Release ReleaseRef
	// BindAddress is the interface the app is told to listen on. Empty means
	// the backend's default for its mode — loopback for shared.
	BindAddress string
	// Env is the app's own environment, secrets included. It reaches the
	// process through the backend's exec environment and is never written to
	// a file inside the release tree.
	Env map[string]string
	// Framework selects the start invocation: "nextjs", "node", "go", "flask",
	// "fastapi", "streamlit", "python".
	Framework  string
	Entrypoint string // e.g. "app.py", "main.py" (Python); main package path (Go)
	StartCmd   string // explicit start command (Node apps: package.json scripts.start)
	BasePath   string // URL prefix the app is served under, e.g. "/apps/my-app"
	// MemoryLimitMB caps RSS; the backend restarts the workload if exceeded.
	MemoryLimitMB int
	// CPURequestMillicores is what the workload reserves from its scheduler,
	// and CPULimitMillicores is the CPU time it may not exceed — an amount,
	// never a set of cores. 0 means the backend sets nothing.
	//
	// **The limit is honoured by both backends**: a pod as its CPU limit, the
	// shared-mode agent as its cgroup's cpu.max at the cgroup and namespace
	// tiers. Below them nothing holds it, and runtime.Enforcement says so
	// rather than letting a screen call it enforced. The request is the
	// scheduler's word, isolated only: a shared workload has no scheduler to
	// reserve from, and is ignored there rather than refused.
	CPURequestMillicores int
	CPULimitMillicores   int
	// AppToken is this deployment's outbound credential (tofta_…): how the app's
	// own process calls /api/v1/app/* and is recognised as this app.
	//
	// Deliberately not part of Env, even though it reaches the process the same
	// way. Env is the *app author's* environment — what they typed on the
	// Settings tab — and anything the platform injects beside it belongs in a
	// field of its own, so that listing an app's configuration never shows a
	// credential the platform minted and no person handles.
	//
	// Empty is normal: an install where nothing has attached an integration
	// still deploys, and the app simply has no token to present.
	AppToken string
	// RoleKey is what this workload verifies X-Toft-Role with — the
	// platform's per-app, per-start derivation, injected beside AppToken and
	// for the same reasons: it is the platform's, not the app author's, and it
	// belongs in a field of its own so that listing an app's configuration
	// never shows a key no person handles.
	//
	// Shared mode only, exactly like AppToken: an isolated workload is handed
	// its key when its pod bootstraps, which is the one moment the platform can
	// give a pod a secret at all.
	//
	// Empty means the workload cannot verify a role assertion and will read
	// every one of them as `unknown`, which refuses. That is the right
	// direction to fail in and needs no special case here.
	RoleKey string
	// Mounts are the durable storage resources this workload may reach.
	//
	// Isolated mode only. A backend that cannot provide them must *reject* a
	// non-empty value rather than ignore it: silently dropping a mount is how a
	// capability leaks into one mode, and an app that believes it wrote to a
	// durable volume and did not is the worst available outcome.
	Mounts []Mount
	// Timeout is a job run's effective limit, as a backstop the backend may
	// enforce itself: the kube Job sets activeDeadlineSeconds a margin past it,
	// so a run stays bounded while the platform that enforces the stored
	// deadline is down. Zero for services, and for a backend that bounds
	// nothing itself.
	Timeout time.Duration
	// BuildHoldsCache says the caller already holds the build cache's read
	// lock — a deploy (Builder.execute) starting its release. The repairer
	// then does not take it again: Go's RWMutex is not re-entrant, and a
	// reclaim waiting between the two RLocks on one goroutine deadlocked every
	// build until restart (security review 2026-11-14, PB-3). False for every
	// other start — boot, wake, restart — which hold nothing.
	BuildHoldsCache bool
}

// Mount is one durable resource on a workload's filesystem.
//
// It is a path and nothing else as far as app code is concerned — no SDK, no
// endpoint, no credential — which is what makes the same app code work
// unchanged in Node, Python, and Go. The fields here are the *backend's*
// inputs; what the app sees is the path.
// ErrJobNotFound is what a JobBackend's Status wraps when it has no record of
// the handle at all — the Job's TTL expired, or it never existed. Every other
// error is trouble reaching the backend, and means nothing about the run: the
// boot reconcile used to fail every in-flight run on any error, and a brief
// API-server or agent outage at restart recorded running jobs as lost while
// they ran on (code review 2026-11-11, C-3).
var ErrJobNotFound = errors.New("the backend has no record of this run")

type Mount struct {
	// Name is the resource's slug, as it appears in TOFT_STORAGE, so code
	// can locate a resource by name rather than hardcoding a path an
	// administrator may later change.
	Name string
	// Claim is the backend object the mount binds through.
	Claim string
	// Path is where it appears inside the workload. Absolute.
	Path string
	// Kind is MountVolume or MountStore. The semantics differ and the mount
	// enforces the difference — a store refuses a rename rather than emulating
	// one — so the backend must know which it is building.
	Kind string
	// Access is the level from the ladder below.
	Access string
	// Folder is which directory of the resource this mount is rooted at, and
	// empty is the resource itself. It appears in TOFT_STORAGE because two
	// mounts of one resource share a name, and the name is how app code locates
	// a resource — so the folder is what tells them apart. It is *not* part of
	// the path: where a resource appears inside a workload stays the
	// administrator's choice, and an app whose paths moved when somebody
	// re-scoped a mapping is the coupling TOFT_STORAGE exists to prevent.
	Folder string
	// Root names the operator's host mount this resource lives under, and is
	// empty on every install with a cluster. It rides on the mount rather than
	// being looked up again at start time, so the description a workload is
	// given and the directory it is pointed at come from one read of one row.
	Root string
	// Gate is the system group a host root's directory is gated with, and its
	// number, as the database recorded them. Zero on every install with a
	// cluster. It rides on the mount for the reason Root does: the group a
	// start recreates and the directory it re-gates come from one read.
	Gate Gate
	// Partitioned says each folder of the resource is its own gate: the
	// resource's own directory is traversable and unwritable, and the folder is
	// what is gated. See SharedStorage.Attach.
	Partitioned bool
}

// Gate is one host group: the name a start asks the host for, and the gid it
// must have there. The number is the database's (storage_gates), never the
// host's, because the host's account database dies with its container and the
// numbers on the file system do not.
type Gate struct {
	Group string
	GID   int
}

// NeedsRoot reports whether a shared workload can be given this mount only
// under a directory an operator mounted. A volume is a directory and needs one;
// a store is reached over signed URLs and is never attached on a shared host,
// so it needs nothing the host has. An unset kind is read as a volume, the
// stricter answer.
func (m Mount) NeedsRoot() bool { return m.Kind != MountStore }

// unattachedStores is what a shared workload is told about the stores among
// its mounts: the same entry, with no path, because nothing was attached. The
// SDK reads an empty path as "reach this over the platform's URLs".
func unattachedStores(mounts []Mount) []Mount {
	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		if !m.NeedsRoot() {
			m.Path = ""
		}
		out = append(out, m)
	}
	return out
}

// Mount kinds and access levels. The levels are a ladder: each is the previous
// plus what its mount flags add, and the flags are the enforcement. Nothing above the
// mount checks them at runtime, which is exactly why an app misusing a store
// fails immediately and loudly at the first wrong call.
const (
	MountVolume = "volume"
	MountStore  = "store"

	AccessRead  = "read"
	AccessWrite = "write"
	AccessFull  = "full"
)

// Backings: what is underneath a resource, as the seam needs to see it.
//
// The kind is what an app's code experiences and the backing is what the
// operator pointed it at. A backend needs the backing for two things a kind
// cannot tell it — which class to provision from, and which prerequisites to
// name when there is none — and for nothing else.
const (
	BackingEFS        = "efs"
	BackingS3Files    = "s3files"
	BackingMountpoint = "mountpoint"
	// BackingHost is a directory under a root the operator mounted, on an
	// install with no cluster. The platform cannot see through a mount, so this
	// is what is known rather than a guess at what is underneath it.
	BackingHost = "host"
	// BackingAzureFiles is an Azure Files share, provisioned from the same
	// class variable EFS is. Whether it is SMB or NFS 4.1 is invisible from
	// here — the platform cannot see through a mount — which is why the
	// condition on the embedded-database recipe lives in the create form's copy
	// rather than in a fourth backing.
	BackingAzureFiles = "azurefiles"
	// BackingBlob is an Azure Blob container prefix mounted with blobfuse2, and
	// it is the first store that is not a Mountpoint mount. The driver has no
	// allow-delete and no allow-overwrite, so `write`, the ladder's middle step,
	// cannot be expressed as a mount here at all; that is a refusal at attach
	// time, never a promotion.
	BackingBlob = "blob"
)

// ReadOnly reports whether the mount forbids writes outright.
func (m Mount) ReadOnly() bool { return m.Access == AccessRead || m.Access == "" }

// ProcessName is the name the backend knows this workload by.
func (s WorkloadSpec) ProcessName() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Slug
}
