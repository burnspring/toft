package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"toft/internal/crypto"
	"toft/internal/runner"
	"toft/internal/toolchain"
)

type Config struct {
	Host    string
	Port    int
	BaseURL string

	// Deployed apps are served from their own listener so that they land on a
	// different browser origin from the platform. Same host, same certificate,
	// different port — a hostile app can then neither read platform API
	// responses (same-origin policy) nor forge writes (see the Origin check in
	// internal/api).
	AppsPort    int
	AppsBaseURL string
	// AppRequestDeadline is the longest a request into an app can be expected
	// to live, end to end — including any load balancer in front of Toft, whose
	// idle timeout is usually what cuts a slow request off (300 s on an AWS ALB
	// by default here). The gateway tells the app with X-Toft-Request-Deadline,
	// so the SDK can refuse to wait on a job past it instead of the browser
	// getting a bare 504. APP_REQUEST_DEADLINE_SECONDS; advisory, not enforced.
	AppRequestDeadline time.Duration

	// --- Sign-in providers ---
	//
	// Providers are normally administered at runtime (`auth_providers`, the
	// Admin → Authentication page). The two sets below exist for the case a
	// runtime surface cannot solve: an install with no users yet, where nobody
	// can reach the admin page to configure the way in. An environment-provided
	// provider is offered on the login page like any other, and is shown
	// read-only there — the operator owns it, and a form must not imply
	// otherwise.

	GitHubClientID     string
	GitHubClientSecret string

	// The generic OIDC bootstrap. One provider, described entirely by its
	// issuer, which is the shape a typical enterprise install actually has:
	// a single corporate IdP, set once by whoever deploys the platform.
	OIDCProviderID     string
	OIDCDisplayName    string
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCScopes         string
	OIDCAllowedDomains string
	OIDCAllowedEmails  string
	OIDCDefaultRole    string
	// OIDCPermittedLoginGroups restricts sign-in to members of the named
	// directory groups. Empty means every group is permitted.
	//
	// The operator-owned half of a setting an administrator sets per provider
	// on a managed row. Without it an environment-configured provider — which
	// the admin UI shows read-only, on purpose — could not use the feature at
	// all, and `OIDC_ALLOWED_DOMAINS` would be the only gate an operator has.
	//
	// A non-empty value refuses a sign-in that carries no group claim. See
	// authprovider.GroupsPermitted; `OIDC_GROUPS_SEPARATOR` is often needed
	// with it.
	OIDCPermittedLoginGroups string
	OIDCGroupsClaim          string
	OIDCGroupsSeparator      string

	JWTSecret     string
	EncryptionKey string

	// EncryptionKeyOld is the retired keys, kept only for reading, from a
	// comma-separated ENCRYPTION_KEY_OLD. Rotation is: put the new key in
	// ENCRYPTION_KEY, move the previous one here, restart. Every stored record
	// still reads and every new write uses the new key, so the swap is a
	// restart rather than a flag day. The list empties when the rotate command
	// reports the sweep complete — until then, dropping a key here is dropping
	// the ability to read what it wrote.
	EncryptionKeyOld []string

	// EncryptionKeyURI selects the managed backend, and **presence decides**:
	// set it and the key that encrypts this install's secrets is generated
	// locally and wrapped by a KEK that never leaves the KMS; leave it unset and
	// the local keyring above is the whole of it. No mode flag, matching
	// KubernetesConfigured() and the rest — so there is no way to declare a mode
	// that disagrees with the configuration.
	//
	// The local backend is a peer rather than a fallback: it is what dev.sh, the
	// test suite, the one-box compose install and every air-gapped site run.
	EncryptionKeyURI string

	// BootstrapAdminPassword is the password the built-in administrator is
	// created with on an install that has none yet, and it is read exactly once:
	// a boot that finds the account already has a password ignores it.
	//
	// Empty means generate one and print it to the log at first boot, which is
	// the right default — it is what keeps an install published at a hostname
	// from shipping a credential the internet already knows. Set it when the
	// deployment has somewhere safe to keep a password: the Helm chart generates
	// one into the release Secret, where `kubectl get secret` reads it back.
	//
	// It is never used to *reset* a password. A value left in the environment
	// that reset the administrator on every restart would be a lockout for
	// whoever changed it and a backdoor for whoever still knew it.
	BootstrapAdminPassword string

	// DatabaseURL is a Postgres connection URL. The schema is owned and
	// migrated by the binary itself (internal/database/migrations).
	DatabaseURL string

	AppsDir string
	LogsDir string
	// JobRunDir (JOB_RUN_DIR) holds a host job run's directory — its identity,
	// scratch and its own copy of the release — in place of
	// APPS_DIR/<app>/runs. For an APPS_DIR on a network filesystem, where
	// unpacking and handing over a release a file at a time is minutes of
	// queue per run: point it at local disk with room for the largest
	// releases of the runs in flight at once. Empty keeps runs under APPS_DIR.
	JobRunDir string
	// AgentDir (AGENT_DIR) is the shared-mode agent's directory: its socket,
	// its state and the identity files it issues (internal/agent). One per
	// install, so two installs on one machine name two. Under /run by
	// default, because a reboot must empty it.
	AgentDir string
	// AgentCgroupParent (AGENT_CGROUP_PARENT) is the cgroup v2 directory the
	// agent creates each workload's cgroup under, for a memory limit the
	// kernel holds. Empty is the agent's own cgroup, which works as root in a
	// container or as a systemd service with Delegate=yes.
	AgentCgroupParent string
	// AgentBridgeCIDR (AGENT_BRIDGE_CIDR) overrides the range the agent's
	// namespace tier picks for its bridge. Empty lets it pick the first clear
	// candidate once and keep it (ADR §135).
	AgentBridgeCIDR string
	PortRangeStart  int
	PortRangeEnd    int

	Runtime string

	// GalleryCatalogURL is where the gallery catalog is fetched from. See
	// DefaultGalleryCatalogURL — this is configuration, never a setting.
	GalleryCatalogURL string

	// DocsURL is the documentation site this install links to, without a
	// version — docs.SiteFor appends the running minor. Empty means DOCS_URL=off:
	// no documentation links anywhere. See DefaultDocsURL; configuration, never
	// a setting.
	DocsURL string

	MaxConcurrentBuilds int
	// RRepos is where R packages are installed from: a URL naming a binary
	// package repository, defaulted to Posit Public Package Manager's current
	// snapshot and overridable to a dated one or to an internal mirror.
	//
	// It belongs to the install rather than to the image because both of those
	// are the operator's statements about their own estate: in the image the
	// value would version with the platform, and neither pinning a date nor
	// moving to a mirror could happen without a rebuild. See DefaultRRepos for
	// why the default is undated and what an install gives up by leaving it so.
	//
	// **This is the one typed URL the platform deliberately does not validate**,
	// and the catalog rule it appears to break is a rule about a different
	// thing. *A URL somebody typed is server-side input, refused at save and at
	// dial* governs a URL an administrator pastes into a settings form, which
	// the platform then dials. This is neither: the legitimate value is
	// routinely the RFC1918 address safefetch exists to refuse, and the dial is
	// not ours — it happens inside Rscript, which is handed a repository option
	// and no socket. It is the RUNNER_IMAGE_* class of configuration, an
	// operator's statement about their own infrastructure.
	RRepos string
	// AutoDeployPollSeconds is how often the auto-deploy poller checks tracked
	// branches for new commits. 0 disables polling entirely.
	AutoDeployPollSeconds int
	// CacheDir holds shared npm/uv caches so rebuilds do not re-download.
	CacheDir string

	// ArtifactStore selects where built releases are published so they outlive
	// the machine that built them: "" (none — releases stay local directories,
	// which is a VM install's normal state), "local", "s3", or "azblob".
	//
	// Absent configuration means the capability is simply not offered. No
	// object-store code path activates and nothing changes about how a release
	// is started, which is what keeps an on-prem install free of dormant cloud
	// machinery.
	//
	// **It is a backend name and never a cloud.** There is no CLOUD variable
	// here or anywhere else: an install may publish releases to Blob and sign
	// store URLs against a MinIO the finance team already runs, and a single
	// switch would make that shape a special case (ADR §82).
	ArtifactStore  string
	ArtifactDir    string // local store root
	ArtifactBucket string
	ArtifactPrefix string
	// ArtifactEndpoint points the S3 client at an S3-compatible server
	// (MinIO, Ceph) instead of AWS, and the Blob client at Azurite or a
	// sovereign cloud. Empty means the public cloud in both cases.
	ArtifactEndpoint string
	ArtifactRegion   string
	// ArtifactS3PathStyle is required by most self-hosted S3-compatible
	// servers, which cannot do virtual-host addressing.
	ArtifactS3PathStyle bool
	// ArtifactAccount is the Azure storage account holding ArtifactBucket,
	// which on `azblob` is a container. Required by that backend and ignored by
	// every other: the account is what the service URL is derived from, and an
	// account name and a URL that disagree is a configuration that can be
	// half-applied.
	ArtifactAccount string
	// ArtifactConnectionString is Azurite's shape and only Azurite's — the
	// emulator ships one well-known account key and there is no identity chain
	// to reach it with. A real install leaves it empty and authenticates from
	// the ambient chain, which is what keeps a storage credential out of `.env`.
	ArtifactConnectionString string

	// --- Isolated mode ---
	//
	// Isolated mode is not a flag an operator turns on; it is *derived* from
	// whether a place to put isolated workloads has been configured. Absent
	// configuration means the capability is not offered, no cluster code path
	// activates, and nothing about it appears in the UI — which is what keeps
	// an on-prem or VM install free of dormant cloud machinery.

	// KubeNamespace is the namespace isolated workloads are placed into.
	// Setting it is what enables the Kubernetes backend. Toft runs inside
	// the cluster it orchestrates in production, so the in-cluster service
	// account is used; KubeConfig points at a cluster from outside, which is
	// how the kind-based development loop works.
	KubeNamespace string
	KubeConfig    string
	KubeContext   string

	// --- Durable app storage ---
	//
	// Admin-provisioned volumes and object stores, mounted into isolated
	// workloads. Absent configuration means the capability is not offered:
	// no storage admin surface appears and no binding objects are ever created.

	// KubeStorageClass is the class *volumes* are provisioned from — one an
	// operator created against a shared filesystem (EFS), with
	// `provisioningMode: efs-ap` and `reclaimPolicy: Retain`. Under a Delete
	// reclaim policy, removing a claim would destroy the access point and
	// everything in it, so Retain is a requirement rather than a preference.
	//
	// Empty means EFS-backed file stores are not offered. Object stores need no
	// class and are unaffected.
	//
	// It is the default class rather than the only one: a resource may name
	// another the operator created, which is how one install offers both
	// backings at once.
	KubeStorageClass string
	// KubeS3FilesStorageClass is the same thing for an Amazon S3 Files file
	// system — a general-purpose bucket mounted with NFS semantics, through the
	// same EFS CSI driver, with `reclaimPolicy: Retain` for the same reason.
	//
	// A second setting rather than a list of classes, because the platform has
	// to know which backing a class provides and a class name cannot tell it.
	// Empty means S3 Files is not offered and the create form does not show it.
	KubeS3FilesStorageClass string
	// KubeStorageClasses declares further classes volumes may be created on —
	// a cluster's further drives — as `name[=backing]`. See
	// runtime.ParseStorageClasses.
	KubeStorageClasses string
	// KubeStoreCSIDriver mounts object *stores* (Mountpoint for Amazon S3).
	// Defaulted, because the driver has one name and making every install
	// restate it would be configuration for its own sake.
	KubeStoreCSIDriver string
	// KubeStoreRegion is passed to the mount when the bucket is not in the
	// driver's own default region.
	KubeStoreRegion string

	// StoreUploads switches on `toft store put` — presigned upload and download
	// against a store, for a person holding a grant rather than an app holding
	// a mount.
	//
	// **Opt-in, and deliberately not derived.** Until this is set the platform
	// holds no permission on any store's bucket at all, which is a sentence
	// worth keeping true by default: turning it on means the service account
	// needs PutObject/GetObject/ListBucket on each store's prefix, and
	// DeleteObject where a `full` grant exists. An operator has to write that
	// policy, so an operator has to say so here — inferring it from AWS_REGION
	// being set would quietly change what the platform's credential can do.
	//
	// Absent, the /api/v1/store routes 404 and nothing about uploads appears in
	// the UI, the same as every other optional capability.
	StoreUploads bool
	// StoreBackend names which object store the signer speaks to: "s3" (the
	// default) or "azblob".
	//
	// **A second variable rather than a default from ARTIFACT_STORE**, because
	// the two address different accounts on purpose: the artifact store is the
	// platform's own bucket and a store is somebody else's. An install that
	// publishes releases to Blob and signs against a MinIO the finance team
	// already runs is a configuration, not a mistake.
	//
	// Derivation was the alternative and it is refused for migration 00018's
	// reason — the backing is stated, never derived — sharpened by what a wrong
	// guess costs here: a wrong signer produces a signature that verifies
	// nowhere, with an error naming neither the account nor the scheme.
	StoreBackend string
	// StoreEndpoint and StoreS3PathStyle point the signer at an S3-compatible
	// server. Note that a store still *mounts* only from real S3 — Mountpoint
	// takes no endpoint option here yet — so setting these makes a store that
	// can be uploaded to and not mounted. They exist so that the mount is the
	// only thing left to do, and so the upload path is exercisable on a laptop.
	//
	// StoreEndpoint doubles as the Blob service URL on `azblob`, for Azurite
	// and for a sovereign cloud; StoreS3PathStyle is ignored there.
	StoreEndpoint    string
	StoreS3PathStyle bool
	// StoreRegion is the signer's region, defaulted from the mount's so a
	// single-region install configures it once. S3 only.
	StoreRegion string
	// StoreAccount is the Azure storage account the stores live in, and
	// StoreConnectionString is Azurite's shape and only Azurite's — see
	// ArtifactAccount and ArtifactConnectionString, which are the same two
	// facts about the platform's own bucket.
	StoreAccount          string
	StoreConnectionString string

	// ECSCluster is the cluster isolated workloads are placed into. Setting it
	// plus the network settings is what enables the ECS backend. Elastic work
	// runs on Fargate capacity: the EC2 task is the fixed always-on footprint
	// carrying Toft and every shared app, and putting elastic workloads
	// there too would drag capacity headroom, ASG scaling, and placement into
	// Toft.
	ECSCluster          string
	ECSSubnets          []string
	ECSSecurityGroups   []string
	ECSTaskRoleARN      string
	ECSExecutionRoleARN string
	// ECSAssignPublicIP is needed when tasks run in public subnets without a
	// NAT gateway; private subnets with VPC endpoints are the intended shape.
	ECSAssignPublicIP bool

	// RunnerImages maps a language family to the image that runs it. One
	// published image per family, versioned in step with platform releases and
	// mirrored into the operator's registry — never per-app images.
	RunnerImages map[string]string
	// RunnerVersion is the platform's half of the runner contract. A runner
	// reporting a different version is refused or flagged rather than silently
	// tolerated, so a partial upgrade fails loudly.
	RunnerVersion string
	// CallbackURL is where a runner reaches the platform. Inside a cluster
	// this is a service address, not BASE_URL, which is a browser-facing name
	// that may not resolve from a pod.
	CallbackURL string
	// CallbackListenAddr is the fourth listener (CALLBACK_ADDR), unset by
	// default and then absent, like METRICS_ADDR. It serves the workload
	// callback routes and nothing else, and believes no forwarded header, so
	// that an isolated install can point CALLBACK_URL at it and confine the
	// apps namespace to this port: an app that cannot reach :8080 cannot
	// forge X-Forwarded-For on a browser route from inside a range trusted for
	// the ingress controller (ADR §31 D4, §34). The same routes stay on :8080,
	// so shared mode and an install that leaves this unset are unchanged.
	CallbackListenAddr string
	// AllowBuildScripts re-enables npm dependency lifecycle scripts.
	AllowBuildScripts bool
	// AppMemoryLimitMB caps each deployed app's memory (0 = uncapped).
	AppMemoryLimitMB int
	// AppCPURequestMillicores is what each workload asks the scheduler to
	// reserve — isolated mode only, because it is the scheduler's word and a
	// shared workload has no scheduler. 0 sets nothing.
	//
	// Millicores rather than a Kubernetes quantity string for the reason
	// AppMemoryLimitMB is megabytes: the number an operator types is
	// unambiguous, and the one package that knows what "100m" means is the one
	// that talks to the cluster.
	AppCPURequestMillicores int
	// AppCPULimitMillicores is the ceiling: an amount of CPU time, never a set
	// of cores — 1000 is 100 ms of CPU per 100 ms, on any of the machine's
	// cores. Honoured by a pod and by the shared backend at the cgroup tier
	// (cpu.max), and it defaults to one core (DefaultAppCPULimitMillicores):
	// without one, an app in a busy loop takes every core on a shared box and
	// its neighbours' latency becomes its behaviour. Over it a workload is
	// throttled rather than restarted, which app_cpu_throttled says. 0 in the
	// environment uncaps.
	AppCPULimitMillicores int
	// JobMemoryLimitMB and JobCPULimitMillicores are a job run's own defaults,
	// separate from the app's: an app's limits no longer reach its jobs, and a
	// job's toft.toml declares none. An administrator overrides them per job.
	JobMemoryLimitMB      int
	JobCPULimitMillicores int

	// Deployed apps run as a separate OS account when configured. Without it
	// they share the platform's identity and can read its database and secrets.
	AppRunAsUser  string
	AppUserPerApp bool
	AppUserPrefix string
	// AppUserAutoCreate lets Toft create missing app accounts at deploy
	// time (APP_USER_AUTOCREATE=1). Off by default: on a shared host account
	// creation is the operator's call. In a container it is the only workable
	// model, and the image enables it.
	AppUserAutoCreate bool
	// AppUserGroup is the shared primary group for auto-created app accounts;
	// egress firewall rules key on it (see docker/entrypoint.sh).
	//
	// It is the *primary* group and it stays that way. Shared-mode storage adds
	// a group per storage resource, always supplementary, precisely so this one
	// keeps matching `iptables -m owner --gid-owner`.
	AppUserGroup string
	// SharedStorageRoots are the host directories an operator mounted and
	// declared, as `name=/path` or `name=/path:ro`, comma-separated. A
	// shared-mode storage resource lives under one of them, in a directory
	// Toft creates and gates; the *mount* is the operator's and Toft
	// makes none.
	//
	// Empty means shared-mode storage is not offered at all — the admin surface
	// does not offer it and a deploy with mappings is refused with the reason.
	// Absent, not dormant.
	SharedStorageRoots string
	// SharedStorageGIDRange is `lo-hi`, the numbers the system groups gating
	// those directories are allocated from. Empty is 62000-62999. The numbers
	// live in the database because the host's /etc/group dies with its
	// container and the numbers recorded on the file system do not.
	SharedStorageGIDRange string
	// AppAccountIDRange is `lo-hi`, the numbers auto-created per-app accounts
	// take as their uid and their per-app group's gid. Empty is 100000-1999999.
	// Recorded in the database for the same reason as the gate range (ADR §122),
	// and it must not overlap it: both are gids.
	AppAccountIDRange string

	// BlockAWSMetadata stops deployed apps reaching the endpoints that hand out
	// instance and task IAM credentials. One switch, two mechanisms, because
	// neither works in the other's mode:
	//
	//	shared    an iptables OUTPUT rule keyed on the app accounts' group,
	//	          installed by docker/entrypoint.sh. Needs CAP_NET_ADMIN.
	//	isolated  a NetworkPolicy egress rule on each workload, installed by
	//	          the cluster backend. Needs a CNI that enforces egress.
	//
	// The invariant is identical either way — a deployed app must not be able
	// to read the credentials of the node it happens to run on, and thereby
	// help itself to the storage its mounts were supposed to mediate.
	BlockAWSMetadata bool

	// LogFormat is `text` or `json`, and LogLevel is one of debug/info/warn/error.
	//
	// Deliberately not inferred from anything else. An install's log format is
	// what its collector parses, and deriving it from a development heuristic —
	// the way the Vite write-origin was derived from a loopback BASE_URL — is
	// how a real install silently gets the wrong one.
	LogFormat string
	LogLevel  slog.Level

	DevPassword string // If set, all routes require HTTP Basic Auth (user: admin)

	// DevInsecureCookies drops the Secure flag from cookies
	// (DEV_INSECURE_COOKIES=1). Only for plain-http deployments on hosts
	// other than localhost — browsers refuse Secure cookies there.
	DevInsecureCookies bool

	// TrustedProxyCIDRs lists the networks whose X-Forwarded-For /
	// X-Real-IP / X-Forwarded-Proto / X-Forwarded-Host headers are believed
	// (TRUSTED_PROXY_CIDRS, comma-separated CIDRs or bare IPs). Empty means no
	// proxy is trusted and the socket peer address is used everywhere.
	//
	// Defaults to loopback (DefaultTrustedProxyCIDRs) rather than to empty.
	// A connection arriving from 127.0.0.1 came from a process on this
	// machine: either the reverse proxy in front of us, or something that
	// already has host access and can do considerably worse than forge a
	// header. A connection from anywhere else is a remote client whose
	// headers must never be believed. That is exactly the line this setting
	// draws, and the binary can draw it without being told — so the common
	// install (Caddy or nginx on the same box) needs no configuration, and
	// this stays a knob only for the topologies nothing could infer: a proxy
	// on another host, a load balancer, a Kubernetes ingress controller
	// dialing from a pod address.
	//
	// Deliberately not widened to RFC1918. On a flat corporate network that
	// would trust every employee laptop to declare its own address and scheme.
	TrustedProxyCIDRs []*net.IPNet

	// ExtraOrigins lists additional browser origins trusted for write requests
	// (EXTRA_ORIGINS, comma-separated scheme://host[:port]). The origin derived
	// from BASE_URL is always trusted; this covers reaching the same install by
	// a second URL — an SSH tunnel on http://localhost:8080, or the Vite dev
	// server — which the Origin check would otherwise reject with a bare 403
	// that looks like a broken button.
	//
	// It widens a security boundary, so it is an explicit operator list rather
	// than anything inferred: every entry is a page allowed to act as whoever
	// is signed in. Empty means nothing extra is trusted.
	ExtraOrigins []string

	// Telemetry is OTLP export and the scrape endpoint. Deployment
	// configuration rather than an audited setting — see the type's doc comment
	// and ADR §31 D3 for why this one lands on the opposite side of the line
	// from email. Unset means the capability is absent.
	Telemetry Telemetry
}

func Load() (*Config, error) {
	// Every boolean and integer this install carries goes through one reader.
	// The list of boolean names below is the specification —
	// TestEveryBooleanSwitchIsReadStrictly fails the build if a tenth switch
	// appears beside them read some other way, which is how the nine came to be
	// read as "set to anything at all" while an argued strict reader sat in the
	// same package.
	flags := &envFlags{}

	port := flags.integer("PORT", 8080, 1, 65535)
	portStart := flags.integer("PORT_RANGE_START", 3100, 1, 65535)
	portEnd := flags.integer("PORT_RANGE_END", 3999, 1, 65535)
	maxBuilds := flags.integer("MAX_CONCURRENT_BUILDS", 0, 0, 1<<20)
	if maxBuilds < 1 {
		maxBuilds = DefaultConcurrentBuilds(goruntime.GOMAXPROCS(0))
	}

	// Never 0: the split is collapsed by setting APPS_PORT equal to PORT, and
	// by nothing else.
	appsPort := flags.integer("APPS_PORT", 8081, 1, 65535)
	requestDeadline := flags.integer("APP_REQUEST_DEADLINE_SECONDS", 300, 0, 1<<31-1)
	if requestDeadline <= 0 {
		requestDeadline = 300
	}
	autoDeployPoll := flags.integer("AUTO_DEPLOY_POLL_INTERVAL", 60, 0, 1<<31-1)
	// The three quantities an operator states about a machine. Each has two
	// spellings — the large unit and the small one — and an install may use
	// either; what it may not do is use both, because that is an old value
	// beside a new one with no way to tell which was meant.
	appMemLimit, memErr := parseQuantityPair(
		"APP_MEMORY_LIMIT_GB", "gigabytes", mebibytesPerGibibyte,
		"APP_MEMORY_LIMIT_MB", "megabytes", DefaultAppMemoryLimitMB)
	appCPURequest, cpuReqErr := parseQuantityPair(
		"APP_CPU_REQUEST_CORES", "cores", millicoresPerCore,
		"APP_CPU_REQUEST_MILLICORES", "millicores", DefaultAppCPURequestMillicores)
	appCPULimit, cpuLimErr := parseQuantityPair(
		"APP_CPU_LIMIT_CORES", "cores", millicoresPerCore,
		"APP_CPU_LIMIT_MILLICORES", "millicores", DefaultAppCPULimitMillicores)
	jobMemLimit, jobMemErr := parseQuantityPair(
		"JOB_MEMORY_LIMIT_GB", "gigabytes", mebibytesPerGibibyte,
		"JOB_MEMORY_LIMIT_MB", "megabytes", DefaultJobMemoryLimitMB)
	jobCPULimit, jobCPUErr := parseQuantityPair(
		"JOB_CPU_LIMIT_CORES", "cores", millicoresPerCore,
		"JOB_CPU_LIMIT_MILLICORES", "millicores", DefaultJobCPULimitMillicores)

	cfg := &Config{
		Host:                     getEnv("HOST", "0.0.0.0"),
		Port:                     port,
		BaseURL:                  getEnv("BASE_URL", "http://localhost:8080"),
		AppsPort:                 appsPort,
		AppRequestDeadline:       time.Duration(requestDeadline) * time.Second,
		AppsBaseURL:              getEnv("APPS_BASE_URL", ""),
		CacheDir:                 getEnv("CACHE_DIR", "./data/cache"),
		ArtifactStore:            strings.ToLower(getEnv("ARTIFACT_STORE", "")),
		ArtifactAccount:          getEnv("ARTIFACT_ACCOUNT", ""),
		ArtifactConnectionString: getEnv("ARTIFACT_CONNECTION_STRING", ""),
		ArtifactDir:              getEnv("ARTIFACT_DIR", "./data/artifacts"),
		ArtifactBucket:           getEnv("ARTIFACT_BUCKET", ""),
		ArtifactPrefix:           getEnv("ARTIFACT_PREFIX", ""),
		ArtifactEndpoint:         getEnv("ARTIFACT_ENDPOINT", ""),
		ArtifactRegion:           getEnv("ARTIFACT_REGION", getEnv("AWS_REGION", "")),
		ArtifactS3PathStyle:      flags.read("ARTIFACT_S3_PATH_STYLE"),
		KubeNamespace:            getEnv("KUBE_NAMESPACE", ""),
		KubeConfig:               getEnv("KUBECONFIG", ""),
		KubeContext:              getEnv("KUBE_CONTEXT", ""),
		KubeStorageClass:         getEnv("KUBE_STORAGE_CLASS", ""),
		KubeS3FilesStorageClass:  getEnv("KUBE_S3FILES_STORAGE_CLASS", ""),
		KubeStorageClasses:       getEnv("KUBE_STORAGE_CLASSES", ""),
		KubeStoreCSIDriver:       getEnv("KUBE_STORE_CSI_DRIVER", ""),
		KubeStoreRegion:          getEnv("KUBE_STORE_REGION", getEnv("AWS_REGION", "")),
		StoreUploads:             flags.read("STORE_UPLOADS"),
		StoreBackend:             strings.ToLower(getEnv("STORE_BACKEND", "s3")),
		StoreAccount:             getEnv("STORE_ACCOUNT", ""),
		StoreConnectionString:    getEnv("STORE_CONNECTION_STRING", ""),
		StoreEndpoint:            getEnv("STORE_ENDPOINT", ""),
		StoreS3PathStyle:         flags.read("STORE_S3_PATH_STYLE"),
		StoreRegion:              getEnv("STORE_REGION", getEnv("KUBE_STORE_REGION", getEnv("AWS_REGION", ""))),
		ECSCluster:               getEnv("ECS_CLUSTER", ""),
		ECSSubnets:               splitList(getEnv("ECS_SUBNETS", "")),
		ECSSecurityGroups:        splitList(getEnv("ECS_SECURITY_GROUPS", "")),
		ECSTaskRoleARN:           getEnv("ECS_TASK_ROLE_ARN", ""),
		ECSExecutionRoleARN:      getEnv("ECS_EXECUTION_ROLE_ARN", ""),
		ECSAssignPublicIP:        flags.read("ECS_ASSIGN_PUBLIC_IP"),
		RunnerVersion:            getEnv("RUNNER_VERSION", DefaultRunnerVersion),
		GalleryCatalogURL:        getEnv("GALLERY_CATALOG_URL", DefaultGalleryCatalogURL),
		CallbackURL:              getEnv("CALLBACK_URL", ""),
		CallbackListenAddr:       strings.TrimSpace(getEnv("CALLBACK_ADDR", "")),
		AllowBuildScripts:        flags.read("BUILD_ALLOW_SCRIPTS"),
		AppMemoryLimitMB:         appMemLimit,
		AppCPURequestMillicores:  appCPURequest,
		AppCPULimitMillicores:    appCPULimit,
		JobMemoryLimitMB:         jobMemLimit,
		JobCPULimitMillicores:    jobCPULimit,
		AppRunAsUser:             getEnv("APP_RUN_AS", ""),
		AppUserPerApp:            flags.readDefault("APP_USER_PER_APP", perAppAccountsByDefault()),
		AppUserPrefix:            getEnv("APP_USER_PREFIX", "toft-app-"),
		AppUserAutoCreate:        flags.readDefault("APP_USER_AUTOCREATE", perAppAccountsByDefault()),
		AppUserGroup:             getEnv("APP_USER_GROUP", "toft-apps"),
		SharedStorageRoots:       getEnv("SHARED_STORAGE_ROOTS", ""),
		SharedStorageGIDRange:    getEnv("SHARED_STORAGE_GID_RANGE", ""),
		AppAccountIDRange:        getEnv("APP_ACCOUNT_ID_RANGE", ""),
		BlockAWSMetadata:         flags.read("BLOCK_AWS_METADATA"),
		GitHubClientID:           trimmedEnv("GITHUB_CLIENT_ID"),
		GitHubClientSecret:       trimmedEnv("GITHUB_CLIENT_SECRET"),
		OIDCProviderID:           getEnv("OIDC_PROVIDER_ID", "sso"),
		OIDCDisplayName:          getEnv("OIDC_DISPLAY_NAME", "Single sign-on"),
		OIDCIssuer:               getEnv("OIDC_ISSUER", ""),
		OIDCClientID:             trimmedEnv("OIDC_CLIENT_ID"),
		OIDCClientSecret:         trimmedEnv("OIDC_CLIENT_SECRET"),
		OIDCScopes:               getEnv("OIDC_SCOPES", ""),
		OIDCAllowedDomains:       getEnv("OIDC_ALLOWED_DOMAINS", ""),
		OIDCAllowedEmails:        getEnv("OIDC_ALLOWED_EMAILS", ""),
		OIDCPermittedLoginGroups: getEnv("OIDC_PERMITTED_LOGIN_GROUPS", ""),
		OIDCGroupsClaim:          getEnv("OIDC_GROUPS_CLAIM", ""),
		OIDCGroupsSeparator:      getEnv("OIDC_GROUPS_SEPARATOR", ""),
		OIDCDefaultRole:          getEnv("OIDC_DEFAULT_ROLE", ""),
		JWTSecret:                getEnv("JWT_SECRET", ""),
		EncryptionKey:            getEnv("ENCRYPTION_KEY", ""),
		EncryptionKeyOld:         splitList(getEnv("ENCRYPTION_KEY_OLD", "")),
		EncryptionKeyURI:         strings.TrimSpace(getEnv("ENCRYPTION_KEY_URI", "")),
		BootstrapAdminPassword:   getEnv("BOOTSTRAP_ADMIN_PASSWORD", ""),
		DatabaseURL:              getEnv("DATABASE_URL", ""),
		AppsDir:                  getEnv("APPS_DIR", "./apps"),
		JobRunDir:                getEnv("JOB_RUN_DIR", ""),
		AgentDir:                 getEnv("AGENT_DIR", "/run/toft/agent"),
		AgentCgroupParent:        getEnv("AGENT_CGROUP_PARENT", ""),
		AgentBridgeCIDR:          getEnv("AGENT_BRIDGE_CIDR", ""),
		LogsDir:                  getEnv("LOGS_DIR", "./logs"),
		PortRangeStart:           portStart,
		PortRangeEnd:             portEnd,
		Runtime:                  getEnv("RUNTIME", DefaultRuntime),
		MaxConcurrentBuilds:      maxBuilds,
		RRepos:                   getEnv("TOFT_R_REPOS", DefaultRRepos),
		AutoDeployPollSeconds:    autoDeployPoll,
		DevPassword:              getEnv("DEV_PASSWORD", ""),
		DevInsecureCookies:       flags.read("DEV_INSECURE_COOKIES"),
	}

	// Said before anything else is checked: an install whose switches mean
	// nothing is one whose behaviour nobody can predict, and every refusal is
	// reported at once rather than one restart at a time.
	if len(flags.errs) > 0 {
		return nil, errors.Join(flags.errs...)
	}

	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}
	// HS256 with a short secret is brute-forceable offline from one captured
	// cookie, and a forged system_admin claim is total compromise.
	if len(cfg.JWTSecret) < 32 {
		return nil, fmt.Errorf("JWT_SECRET must be at least 32 bytes (got %d) — generate one with `openssl rand -hex 32`", len(cfg.JWTSecret))
	}
	// There is no unencrypted mode. With a URI the data key takes over as the
	// encrypt key, so ENCRYPTION_KEY becomes optional — and it stays meaningful,
	// because an install that stored anything under it before the move needs it
	// (or ENCRYPTION_KEY_OLD) to keep reading those records.
	if cfg.EncryptionKey == "" && cfg.EncryptionKeyURI == "" {
		return nil, fmt.Errorf("ENCRYPTION_KEY is required (or set ENCRYPTION_KEY_URI to use a KMS)")
	}

	// Refused here rather than at the first decrypt an hour later. A typo in a
	// scheme is the most likely way to get this wrong, and the error names the
	// schemes that exist.
	if cfg.EncryptionKeyURI != "" {
		known := false
		for _, scheme := range crypto.KnownSchemes {
			if strings.HasPrefix(cfg.EncryptionKeyURI, scheme) {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("ENCRYPTION_KEY_URI must begin with %s",
				strings.Join(crypto.KnownSchemes, " or "))
		}
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required (e.g. postgres://toft:toft@localhost:5432/toft?sslmode=disable — run `docker compose up -d` for a local one)")
	}

	// Present-but-empty is honoured verbatim, which is why this reads the
	// variable itself rather than going through getEnv: `TRUSTED_PROXY_CIDRS=`
	// is how an operator says "trust nothing, not even loopback", and getEnv
	// would hand back the default and quietly ignore them.
	proxyCIDRs, ok := os.LookupEnv("TRUSTED_PROXY_CIDRS")
	if !ok {
		proxyCIDRs = DefaultTrustedProxyCIDRs
	}
	trustedProxies, err := parseCIDRList(proxyCIDRs)
	if err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err)
	}
	cfg.TrustedProxyCIDRs = trustedProxies

	// Refused rather than defaulted: an operator who typed `LOG_FORMAT=jsonl`
	// wants structured output, and quietly giving them text is how a collector
	// silently receives nothing it can parse for a week.
	switch cfg.LogFormat = strings.ToLower(strings.TrimSpace(getEnv("LOG_FORMAT", "text"))); cfg.LogFormat {
	case "text", "json":
	default:
		return nil, fmt.Errorf("LOG_FORMAT: %q is not `text` or `json`", cfg.LogFormat)
	}

	docsURL, err := ParseDocsURL(os.Getenv("DOCS_URL"))
	if err != nil {
		return nil, err
	}
	cfg.DocsURL = docsURL
	switch lvl := strings.ToLower(strings.TrimSpace(getEnv("LOG_LEVEL", "info"))); lvl {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "info":
		cfg.LogLevel = slog.LevelInfo
	case "warn", "warning":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("LOG_LEVEL: %q is not debug, info, warn or error", lvl)
	}

	extraOrigins, err := parseOriginList(os.Getenv("EXTRA_ORIGINS"))
	if err != nil {
		return nil, fmt.Errorf("EXTRA_ORIGINS: %w", err)
	}
	cfg.ExtraOrigins = extraOrigins

	if cfg.AppsBaseURL == "" {
		cfg.AppsBaseURL = deriveAppsBaseURL(cfg.BaseURL, cfg.AppsPort)
	}
	cfg.AppsBaseURL = strings.TrimSuffix(cfg.AppsBaseURL, "/")

	// These three leave the platform's working directory and are resolved by
	// something else: the cache dir is exported into build environments
	// (GOCACHE, GOMODCACHE, npm_config_cache, UV_CACHE_DIR) where builds run
	// with the release dir as their working directory, and the apps and logs
	// dirs are sent to the shared-mode agent in every workload's description
	// (its `dir`, its log path), which that separate, long-lived process
	// resolves against whatever directory it was first started from. A relative value is rejected by Go outright, and silently
	// lands per-release caches or a second apps tree for everything else, so
	// resolve them here — once, where the value enters the process.
	for _, p := range []*string{&cfg.CacheDir, &cfg.AppsDir, &cfg.LogsDir} {
		if abs, err := filepath.Abs(*p); err == nil {
			*p = abs
		}
	}
	for _, p := range []*string{&cfg.JobRunDir, &cfg.AgentDir} {
		if *p != "" {
			if abs, err := filepath.Abs(*p); err == nil {
				*p = abs
			}
		}
	}
	// Two names that no longer mean anything, refused by name rather than
	// ignored: an operator who set one meant something by it, and an install
	// that starts anyway is doing something else (envFlag's rule).
	switch cfg.Runtime {
	case "agent", "mock":
	case "pm2":
		return nil, fmt.Errorf("RUNTIME=pm2 is no longer supported; use RUNTIME=agent (the default): " +
			"the shared-mode agent replaced PM2")
	default:
		return nil, fmt.Errorf("RUNTIME=%q is not a runtime: use agent or mock", cfg.Runtime)
	}
	if _, set := os.LookupEnv("JOB_CGROUP_PARENT"); set {
		return nil, fmt.Errorf("JOB_CGROUP_PARENT is no longer read; set AGENT_CGROUP_PARENT, " +
			"which the shared-mode agent places every workload's cgroup under")
	}

	switch cfg.ArtifactStore {
	case "", "local":
	case "s3":
		if cfg.ArtifactBucket == "" {
			return nil, fmt.Errorf("ARTIFACT_STORE=s3 requires ARTIFACT_BUCKET")
		}
	case "azblob":
		if cfg.ArtifactBucket == "" {
			return nil, fmt.Errorf("ARTIFACT_STORE=azblob requires ARTIFACT_BUCKET (the container name)")
		}
		// The account is what the service URL is derived from, so a missing one
		// is not a default to fill in — it is the address. Azurite is the one
		// exception and it says so by carrying a connection string instead.
		if cfg.ArtifactAccount == "" && cfg.ArtifactConnectionString == "" {
			return nil, fmt.Errorf("ARTIFACT_STORE=azblob requires ARTIFACT_ACCOUNT " +
				"(the storage account holding the container), or ARTIFACT_CONNECTION_STRING for Azurite")
		}
	default:
		return nil, fmt.Errorf("ARTIFACT_STORE must be empty, \"local\", \"s3\", or \"azblob\" (got %q)", cfg.ArtifactStore)
	}

	// Refused whether or not uploads are switched on: an operator who spells the
	// backend wrong and has not yet set STORE_UPLOADS should find out now rather
	// than on the day they do.
	switch cfg.StoreBackend {
	case "s3":
	case "azblob":
		if cfg.StoreUploads && cfg.StoreAccount == "" && cfg.StoreConnectionString == "" {
			return nil, fmt.Errorf("STORE_BACKEND=azblob requires STORE_ACCOUNT " +
				"(the storage account the stores live in), or STORE_CONNECTION_STRING for Azurite")
		}
	default:
		return nil, fmt.Errorf("STORE_BACKEND must be \"s3\" or \"azblob\" (got %q)", cfg.StoreBackend)
	}

	cfg.RunnerImages = runnerImagesFromEnv()

	// Refused here rather than half-started later, for the reason every other
	// partial configuration in this function is: an export that silently goes
	// nowhere looks exactly like an install nobody has instrumented.
	telemetryCfg, err := loadTelemetry()
	if err != nil {
		return nil, err
	}
	cfg.Telemetry = telemetryCfg
	if err := cfg.validateCallbackListener(); err != nil {
		return nil, err
	}

	// Refused rather than ignored, and refused on every install rather than
	// only on one with a cluster: a typo that silently means "no request" is
	// the exact condition this dial exists to end, and an operator who moves
	// an install into isolated mode later should not discover it then.
	for _, err := range []error{memErr, cpuReqErr, cpuLimErr, jobMemErr, jobCPUErr} {
		if err != nil {
			return nil, err
		}
	}
	if cfg.AppCPULimitMillicores > 0 && cfg.AppCPULimitMillicores < cfg.AppCPURequestMillicores {
		return nil, fmt.Errorf("APP_CPU_LIMIT_MILLICORES (%d) is below APP_CPU_REQUEST_MILLICORES (%d): "+
			"a workload cannot be capped below what it reserves, and the cluster would refuse every pod. "+
			"Raise the limit, or set APP_CPU_LIMIT_MILLICORES explicitly; it defaults to %d",
			cfg.AppCPULimitMillicores, cfg.AppCPURequestMillicores, DefaultAppCPULimitMillicores)
	}
	// A job pod pairs its own limit with the same request (code review
	// 2026-11-11, H-5).
	if cfg.JobCPULimitMillicores > 0 && cfg.JobCPULimitMillicores < cfg.AppCPURequestMillicores {
		return nil, fmt.Errorf("JOB_CPU_LIMIT_MILLICORES (%d) is below APP_CPU_REQUEST_MILLICORES (%d): "+
			"a job cannot be capped below what it reserves, and the cluster would refuse every run",
			cfg.JobCPULimitMillicores, cfg.AppCPURequestMillicores)
	}

	if err := cfg.validateIsolated(); err != nil {
		return nil, err
	}

	// The operator's own statement about their own image, checked at the first
	// boot rather than at the deploy three weeks later that would have needed
	// it (ADR §64 D11). Every path in TOFT_PYTHON_PATH /
	// TOFT_NODE_PATH / TOFT_GO_PATH is confirmed by exec, and every
	// RUNNER_*_VERSION has to be a version.
	//
	// This is also what forces the probe, so the execs happen during boot and
	// the set is settled for the life of the process — which is what makes
	// restart-to-notice the honest description rather than a TTL nobody can
	// predict.
	if err := toolchain.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// DefaultGalleryCatalogURL is where the gallery catalog lives: a document in a
// repository the Toft team controls.
//
// It is a constant rather than a setting because where the catalog lives is a
// fact about how this binary was built, not a decision an administrator
// revisits — and because a catalog URL an attacker could edit through the API
// would be a way to choose what every deploy button on the install offers.
// GALLERY_CATALOG_URL overrides it for one case only: an operator pointing an
// air-gapped or bandwidth-restricted install at an internal mirror. That is a
// lever for whoever stood the install up, which is why it is in the environment
// and not in the admin panel.
const DefaultGalleryCatalogURL = "https://raw.githubusercontent.com/" + GitHubOwner + "/toft-gallery/main/catalog.json"

// DefaultDocsURL is the public documentation site: the user, admin and CLI &
// SDK guides, published one folder per minor from GitHubOwner/toft-docs at the
// root of the product's docs subdomain. An install links to it and never
// fetches it. Binaries built before the move link to the old Pages address,
// which GitHub forwards here and the site's 404 page strips back to the page.
//
// A constant overridable only by DOCS_URL, on the catalog URL's ground: where
// the manuals live is part of how the install was stood up, and a URL the API
// could write would choose where every reader of Documentation is sent. DOCS_URL
// is for a mirror on a network with no way out, or `off`.
const DefaultDocsURL = "https://docs." + ProductDomain

// ParseDocsURL reads DOCS_URL. Unset is DefaultDocsURL; `off`, in any case, is
// the empty string, meaning no documentation links; anything else must be an
// absolute http(s) URL with a host and no credentials — http because an
// internal mirror is the case the variable exists for. A trailing slash is
// dropped so the version can be appended.
func ParseDocsURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultDocsURL, nil
	}
	if strings.EqualFold(raw, "off") {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("DOCS_URL must be an http(s) URL of the documentation site, with no credentials, query or fragment — or off (got %q)", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// GitHubOwner is the organization every published repository lives under: the
// gallery catalog, the three manuals, the client and the installers. Written
// once per module, because the owner is a placeholder that will be renamed, and
// a rename that has to find every URL is one that misses one. The client is a
// separate module and keeps its own copy (cli/install_version.go), the publish
// scripts read scripts/lib/owner.sh, and owner_test.go fails when any of them
// disagrees with this one.
const GitHubOwner = "burnspring"

// ProductDomain is the product's own site, gettoft.com; the manuals are its
// docs subdomain.
const ProductDomain = "gettoft.com"

// DefaultRunnerVersion is the callback contract version this platform speaks.
// Runner and platform deploy independently and a long-lived isolated service
// may outlive several platform upgrades, so the version is explicit and a
// mismatch is a stated outcome rather than undefined behaviour.
//
// **It is the runner's own constant, not a second copy of it.** It was a second
// copy — a literal "1" here against `runner.ContractVersion` of "2" — and the
// two had drifted, which did not produce a subtle bug: *every* isolated deploy
// failed at bootstrap with `runner speaks 2, platform speaks 1 — a partial
// upgrade; redeploy the mismatched half`, on an install where both halves came
// out of the same build. A version check comparing a package constant against a
// hand-copied duplicate of it can only ever report on the copy.
//
// The environment still overrides, because that is what makes a genuine
// mismatch survivable while an operator upgrades the halves in some order. What
// is gone is the ability for the *default* to be wrong.
const DefaultRunnerVersion = runner.ContractVersion

// DefaultTrustedProxyCIDRs is the fallback for TRUSTED_PROXY_CIDRS: the two
// loopback networks and nothing else. See the field comment for why loopback
// is a safe default and why the list stops there.
const DefaultTrustedProxyCIDRs = "127.0.0.1/32,::1/128"

// DefaultRRepos is the fallback for TOFT_R_REPOS: Posit Public Package
// Manager's current snapshot, serving binaries built per distribution and per R
// minor, so an install is an unpack rather than a compile.
//
// **Undated on purpose, and the reproducibility argument for a date was the
// weaker one.** What a date buys is that an app with no lockfile builds the same
// way twice. What it costs is everything published after it: a package released
// since the date is not an older version in a dated snapshot, it is *absent*,
// and the deploy fails naming a package that plainly exists. It costs this
// install's own dependency gate too — the gate refuses a release on a CRAN
// advisory that names a fixed version, and a snapshot predating the fix cannot
// serve it, so the refusal has no remedy the person reading the build log owns.
// And the date never bought the binaries it looked like it bought: renv restores
// the version the lockfile *pins*, which any snapshot serves as a binary only
// for a lockfile resolved near it — the finding that put a compiler in this
// image (ADR §64).
//
// What a date would have bought for a *lockfile* — binaries for its old pins —
// the build now gets per pin, from a snapshot dated while each was current
// (builder/rsnapshot.go, ADR §123), without dating anything else.
//
// The trade is stated rather than hidden: an app with no lockfile can pick up a
// package's new major version on a rebuild it did not ask for. **An install that
// wants a date sets one** — that is what this variable and the Admin -> Settings
// dial beside it are for, and pinning is the right answer for an estate that has
// decided when it moves. It is not the right default for one that has not.
//
// An install with no route to this host and no mirror of its own cannot deploy
// R at all: there is no fallback to source, because the runner image carries no
// compiler.
//
// **trixie**, because that is the distribution both this platform's image and
// docker/runner/Dockerfile's family-r are built on, and a binary R package is
// compiled against a distribution's system libraries. The build runs on the
// platform host in both execution modes, so a library restored here has to be
// one the pod can load; naming another distribution's binaries would produce a
// release that unpacks and then fails at dyn.load, one package at a time.
//
// **P3M publishes trixie binaries for x86_64 only.** On an arm64 install there
// is nothing here to unpack: renv falls back to source, and the runner image
// has no compiler, so R deploys fail. An arm64 operator has to point
// TOFT_R_REPOS at a mirror that carries arm64 builds. That is a real
// narrowing of where R works and it is stated rather than discovered — the same
// bargain D4 makes for an install with no route to P3M at all.
const DefaultRRepos = "https://p3m.dev/cran/__linux__/trixie/latest"

// runnerImageEnv maps a framework family to the env var naming its image.
// One image per *language family*, not per framework and never per app.
var runnerImageEnv = map[string]string{
	"python": "RUNNER_IMAGE_PYTHON",
	"node":   "RUNNER_IMAGE_NODE",
	"go":     "RUNNER_IMAGE_GO",
	"r":      "RUNNER_IMAGE_R",
	// A static site has no language runtime, so it has no language family to
	// borrow. Its image is an empty base holding the runner binary — about
	// 10 MB, more than an order of magnitude smaller than the smallest of the
	// three, which matters most on exactly the install this exists for.
	"static": "RUNNER_IMAGE_STATIC",
}

func runnerImagesFromEnv() map[string]string {
	out := map[string]string{}
	for family, env := range runnerImageEnv {
		if v := getEnv(env, ""); v != "" {
			out[family] = v
		}
	}
	return out
}

// DefaultAppCPURequestMillicores is what an isolated workload reserves when
// nobody has said otherwise: a tenth of a core.
//
// Non-zero on purpose, and small on purpose. With no request at all the
// scheduler treats every app as costing nothing and will pack a node until
// they starve each other — the condition this exists to end — so shipping the
// dial switched off would close the gap only for operators who read release
// notes. A tenth of a core is small enough that an upgrade changes nothing
// until an install is running more than ten apps per core, and at that density
// the node was already oversubscribed and the scheduler was already the wrong
// thing to be lying to.
//
// Setting it to 0 is still available and still means "reserve nothing".
const DefaultAppCPURequestMillicores = 100

// DefaultAppCPULimitMillicores is one core: the most CPU time an app's
// workload takes when nobody has said otherwise. An amount of time, spread
// across any of the machine's cores, never a pinning.
const DefaultAppCPULimitMillicores = 1000

// DefaultJobMemoryLimitMB and DefaultJobCPULimitMillicores are a job run's
// limits when an administrator has set none for it: a job is batch work, and
// its timeout bounds it, so it gets more than an app's service does.
const (
	DefaultJobMemoryLimitMB      = 1024
	DefaultJobCPULimitMillicores = 2000
)

// DefaultRuntime is what an install that names no runtime gets.
//
// `mock` was the default and it is a test double: no delivery artifact and no
// developer runs it, and the failure it produces is quiet — an install that
// accepts a deploy and starts nothing. A default nothing outside a test uses is
// a default in the wrong place, and the four artifacts that had to say `pm2` were
// four restatements of it. Tests set RUNTIME=mock explicitly, which is where a
// test double belongs.
//
// `agent` since the shared-mode agent replaced PM2 (ADR §129):
// it needs nothing on the machine but this binary.
const DefaultRuntime = "agent"

// perAppAccountsByDefault is the posture an install that said nothing gets.
//
// **On when the platform is root and running shared workloads on this machine**,
// which is exactly the condition the posture requires: per-app accounts need
// root because the agent starts each runner as its account and the release tree
// is chowned, so a non-root platform cannot have them and fails the deploy
// closed if told to. The isolated backends have no OS accounts
// at all.
//
// So the only installs this changes are the ones running as root *and* saying
// nothing — which until now got AccountsPlatform, every app able to read the
// platform database and every other app's secrets, with a startup warning as the
// only notice. That install is not choosing that posture; it is failing to say
// anything, and the platform was answering with the least safe of three.
// internal/runtime/isolation.go already calls per-app "the shipped posture" in a
// comment, and this makes the code say what the comment says.
//
// Read here rather than at the point of use so the whole answer is settled once,
// during Load, like every other configuration decision.
func perAppAccountsByDefault() bool {
	return os.Geteuid() == 0 && SharedHostRuntime(getEnv("RUNTIME", DefaultRuntime))
}

// SharedHostRuntime reports whether a RUNTIME runs shared workloads as real
// processes on this machine — the shared-mode agent's, and not the mock's.
// What the boot reconcile, the health watcher and the per-app accounts key on.
func SharedHostRuntime(runtime string) bool { return runtime == "agent" }

// DefaultAppMemoryLimitMB is the ceiling an install that says nothing gets:
// half a gibibyte, the figure .env.example has carried since the beginning.
// Named rather than inlined now that two spellings resolve to it.
const DefaultAppMemoryLimitMB = 512

// The two conversions, stated once. A gigabyte here is a gibibyte, and that is
// deliberate: the field says GB and means what both backends count in — the
// agent takes integer MiB and Kubernetes a binary quantity — so an
// operator who types 0.5 gets 512, the number that has been in .env.example
// since the beginning. An operator who got 500 instead would have been given a
// worse answer by a more correct one.
const (
	mebibytesPerGibibyte = 1024
	millicoresPerCore    = 1000
)

// parseQuantityPair reads one quantity that has two spellings — the large unit
// an operator usually thinks in, and the small one the platform stores — and
// hands back the value in the small one.
//
// **Neither spelling is the old one.** Megabytes is the right unit for a 256 MB
// cap and gigabytes for a 16 GB one, and asking an operator to convert is
// asking them to make an arithmetic mistake in a file nobody reviews twice. So
// both are read, and which one an install writes is its own business.
//
// Two rules, and each excludes a different way of being wrong:
//
//   - **Both set is refused**, naming the two and the values seen, exactly as
//     KUBE_NAMESPACE and ECS_CLUSTER both set are refused rather than resolved
//     by precedence. There is no direction to guess in: an operator with both
//     has two numbers and the platform cannot know which they meant.
//   - **Unreadable is refused**, never approximated. That is this package's
//     rule everywhere else and the reason the strict boolean reader exists.
//
// The large spelling takes a decimal, because the alternative — a Kubernetes
// quantity string, `512Mi` / `1.5Gi` — is a parser with an error path on a field
// where a 7% difference is invisible. A bare decimal in a stated unit has no
// ambiguity to resolve. It is rounded to the nearest small unit, which is
// lossless at every value anybody types and honest at the ones they do not.
func parseQuantityPair(bigKey, bigUnit string, perBigUnit int, smallKey, smallUnit string, def int) (int, error) {
	bigRaw := strings.TrimSpace(os.Getenv(bigKey))
	smallRaw := strings.TrimSpace(os.Getenv(smallKey))

	if bigRaw != "" && smallRaw != "" {
		return 0, fmt.Errorf("%s and %s are both set (%s=%s, %s=%s): "+
			"they are the same number in two units, and the platform will not guess which one you meant. "+
			"Set one of them",
			bigKey, smallKey, bigKey, bigRaw, smallKey, smallRaw)
	}

	if bigRaw != "" {
		f, err := strconv.ParseFloat(bigRaw, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number of %s (decimals are allowed, e.g. 0.5), got %q", bigKey, bigUnit, bigRaw)
		}
		if f < 0 {
			return 0, fmt.Errorf("%s cannot be negative, got %s", bigKey, bigRaw)
		}
		return int(math.Round(f * float64(perBigUnit))), nil
	}

	if smallRaw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(smallRaw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number of %s, got %q — or write it as %s, which takes decimals",
			smallKey, smallUnit, smallRaw, bigKey)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s cannot be negative, got %d", smallKey, n)
	}
	return n, nil
}

// validateIsolated refuses a half-configured cluster backend. Partial
// configuration is worse than none: it produces an install that offers
// promotion and then fails at deploy time, which is exactly the "confusing
// error instead of a decision" the design exists to avoid.
func (c *Config) validateIsolated() error {
	if c.KubernetesConfigured() && c.ECSConfigured() {
		return fmt.Errorf("configure one isolated backend, not both: " +
			"KUBE_NAMESPACE and ECS_CLUSTER are both set")
	}

	if c.ECSCluster != "" {
		if len(c.ECSSubnets) == 0 {
			return fmt.Errorf("ECS_CLUSTER requires ECS_SUBNETS (tasks must reach Toft's callback API and any internal databases)")
		}
		if c.ECSExecutionRoleARN == "" {
			return fmt.Errorf("ECS_CLUSTER requires ECS_EXECUTION_ROLE_ARN (the role that pulls the runner image)")
		}
	}

	if !c.IsolatedConfigured() {
		return nil
	}

	// An isolated workload fetches its release rather than sharing a disk with
	// the platform, so there has to be somewhere to fetch it from.
	if c.ArtifactStore == "" {
		return fmt.Errorf("isolated mode requires an artifact store (ARTIFACT_STORE): " +
			"an isolated workload has no access to APPS_DIR and must fetch its release")
	}
	if len(c.RunnerImages) == 0 {
		return fmt.Errorf("isolated mode requires at least one runner image " +
			"(RUNNER_IMAGE_PYTHON / RUNNER_IMAGE_NODE / RUNNER_IMAGE_GO / RUNNER_IMAGE_R / RUNNER_IMAGE_STATIC)")
	}
	if c.CallbackAddr() == "" {
		return fmt.Errorf("isolated mode requires CALLBACK_URL: a runner reaches the platform " +
			"by a cluster-internal address, and BASE_URL is a browser-facing name that may not resolve from a workload")
	}
	return nil
}

// KubernetesConfigured reports whether the Kubernetes backend is on offer.
func (c *Config) KubernetesConfigured() bool { return c.KubeNamespace != "" }

// ECSConfigured reports whether the ECS backend is on offer.
func (c *Config) ECSConfigured() bool { return c.ECSCluster != "" }

// IsolatedConfigured reports whether *any* isolated backend is on offer, which
// is also the answer to whether background jobs exist on this install.
func (c *Config) IsolatedConfigured() bool {
	return c.KubernetesConfigured() || c.ECSConfigured()
}

// TwoHostMode reports whether deployed apps answer on a different HOSTNAME than
// the platform, rather than on a different port of the same one.
//
// The distinction decides how a viewer's session reaches the gateway. On one
// host with two ports a browser scopes the cookie to the host and ignores the
// port, so the session arrives for free while app JavaScript still cannot read
// /api/v1 as the viewer. On two hostnames the cookie is host-only and does not
// arrive at all, so the gateway has to establish a session of its own through
// the app-grant handshake (internal/auth/appgrant.go).
//
// Two hostnames is what a load balancer forces: an ALB, an nginx-ingress and an
// F5 all route by host and path, and none of them can route by listener port.
func (c *Config) TwoHostMode() bool {
	platform, apps := hostOfURL(c.BaseURL), hostOfURL(c.AppsBaseURL)
	if platform == "" || apps == "" {
		return false
	}
	return !strings.EqualFold(platform, apps)
}

// ReservedHosts is the install's own hosts — the platform's and the apps
// host — which no app's hostname may be, nor be a parent or a child of.
// ExtraOriginHosts is the host of each EXTRA_ORIGINS entry, which no app's
// hostname may be. Both feed database.ParseAppHostname.
func (c *Config) ReservedHosts() []string {
	return []string{hostOfURL(c.BaseURL), hostOfURL(c.AppsBaseURL)}
}

func (c *Config) ExtraOriginHosts() []string {
	var out []string
	for _, o := range c.ExtraOrigins {
		if h := hostOfURL(o); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// AppHostOrigin is the canonical origin of an app on a hostname of its own:
// the apps host's scheme and port, the app's name. One function, because the
// handshake, the origin checks, the redirect off /apps/{slug} and every public
// URL have to agree on it. No per-app scheme or port: the names arrive through
// the same upstream as the apps host — on a production install a port-less
// https address, on a dev box the apps listener's own port — and a second
// setting would be one whose only use is a mismatch.
func (c *Config) AppHostOrigin(hostname string) string {
	return AppHostOriginFor(c.AppsBaseURL, hostname)
}

// AppHostOriginFor is AppHostOrigin for a caller holding the apps base URL
// rather than the configuration.
func AppHostOriginFor(appsBaseURL, hostname string) string {
	scheme, port := "http", ""
	if u, err := url.Parse(appsBaseURL); err == nil {
		if u.Scheme != "" {
			scheme = u.Scheme
		}
		port = u.Port()
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(hostname, port)
	}
	return scheme + "://" + hostname
}

// hostOfURL is the hostname without its port. The port is deliberately dropped:
// two ports on one host are the SAME host, which is the whole distinction
// TwoHostMode exists to draw.
func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// validateCallbackListener refuses a CALLBACK_ADDR that cannot be bound or
// that names a port another listener already holds. Shared with the platform's
// port, the listener would be the platform listener — the very thing it exists
// to keep apps away from — and the second bind would fail at start anyway, as
// a fatal log line rather than a reason.
func (c *Config) validateCallbackListener() error {
	if c.CallbackListenAddr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(c.CallbackListenAddr)
	if err != nil || port == "" {
		return fmt.Errorf("CALLBACK_ADDR=%q is not host:port (e.g. :8082)", c.CallbackListenAddr)
	}
	taken := map[string]string{strconv.Itoa(c.Port): "PORT"}
	if c.SeparateAppsListener() {
		taken[strconv.Itoa(c.AppsPort)] = "APPS_PORT"
	}
	if c.Telemetry.MetricsAddr != "" {
		if _, mp, err := net.SplitHostPort(c.Telemetry.MetricsAddr); err == nil {
			taken[mp] = "METRICS_ADDR"
		}
	}
	if name, ok := taken[port]; ok {
		return fmt.Errorf("CALLBACK_ADDR=%s shares its port with %s: the callback listener exists "+
			"to be a port apart from the others", c.CallbackListenAddr, name)
	}
	return nil
}

// CallbackAddr is where a runner reaches the platform. With no CALLBACK_URL,
// a shared-only install is one machine, so the runner reaches the platform's
// own listener over loopback rather than BASE_URL. BASE_URL goes through
// whatever fronts the install, and a load balancer answers 503 until it
// registers a restarted platform; boot Reconcile starts every app inside that
// window, so each image or instance refresh failed the apps that came up first
// (TOFT-BB-003, task-mailer on trytoft 2026-11-06).
func (c *Config) CallbackAddr() string {
	if c.CallbackURL != "" {
		return strings.TrimSuffix(c.CallbackURL, "/")
	}
	if !c.IsolatedConfigured() {
		host := c.Host
		if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
			host = "127.0.0.1"
		}
		return "http://" + net.JoinHostPort(host, strconv.Itoa(c.Port))
	}
	return ""
}

// splitList parses a comma-separated setting, trimming blanks.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// deriveAppsBaseURL keeps the platform's scheme and hostname and swaps in the
// apps port, which is the whole point of the split: same site, distinct origin.
func deriveAppsBaseURL(baseURL string, appsPort int) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Sprintf("http://localhost:%d", appsPort)
	}
	return fmt.Sprintf("%s://%s:%d", parsed.Scheme, parsed.Hostname(), appsPort)
}

// SeparateAppsListener reports whether apps get their own port. When they do
// not, apps share the platform origin and the cross-origin protections that
// depend on the split do not apply.
func (c *Config) SeparateAppsListener() bool {
	return c.AppsPort > 0 && c.AppsPort != c.Port
}

func (c *Config) AppsAddr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.AppsPort)
}

func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// parseOriginList parses a comma-separated list of browser origins into the
// scheme://host[:port] form an Origin header actually carries.
//
// A path, query or fragment is rejected rather than trimmed away. Such a value
// was copied out of an address bar instead of written as an origin, and quietly
// accepting it would store a setting that looks applied, compares against what
// the browser sends, and never matches — the failure would surface as an
// unexplained 403 far from the typo that caused it.
func parseOriginList(s string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("%q is not an origin (want scheme://host[:port])", raw)
		}
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%q has a path, query or fragment; an origin is only scheme://host[:port]", raw)
		}
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out, nil
}

// parseCIDRList parses a comma-separated list of CIDRs; a bare IP means just
// that address (/32 or /128).
func parseCIDRList(s string) ([]*net.IPNet, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP %q", part)
			}
			if ip.To4() != nil {
				part += "/32"
			} else {
				part += "/128"
			}
		}
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", part)
		}
		out = append(out, ipnet)
	}
	return out, nil
}

// IsLocalDevelopment reports whether this install serves itself on loopback —
// which is to say, whether it is somebody's laptop rather than a deployment.
//
// Used to decide whether the Vite dev server's origin (:5173) is an acceptable
// request origin. It reads BASE_URL rather than a dedicated flag because that is
// the fact it actually depends on: a production install has a real base URL, so
// the allowance cannot follow the binary into one.
func (c *Config) IsLocalDevelopment() bool {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return false
	}
	return IsLoopbackHost(u.Hostname())
}

// IsLoopbackHost reports whether a bind host is a loopback interface.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// trimmedEnv reads a credential whose surrounding whitespace is never meant.
// A client id or secret reaches the process through a Kubernetes Secret, a
// compose `.env` or a shell heredoc, and all three carry a trailing newline
// unless somebody remembered not to. What the identity provider then says is
// `incorrect_client_credentials` — true, unhelpful, and only visible at the end
// of a browser sign-in. Only the four OAuth credentials are read this way; a
// value where whitespace could be meaningful keeps getEnv.
func trimmedEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// The spellings a boolean switch may be written in, stated once. Both readers
// in this package share them; what separates the two is only what happens to a
// value that is in neither list.
var (
	flagOn  = []string{"1", "true", "yes", "on"}
	flagOff = []string{"", "0", "false", "no", "off"}
)

// parseFlag reads a boolean out of one environment value. The third answer —
// "that is not a boolean" — is the whole reason it returns an error.
func parseFlag(raw string) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	for _, on := range flagOn {
		if v == on {
			return true, nil
		}
	}
	for _, off := range flagOff {
		if v == off {
			return false, nil
		}
	}
	return false, fmt.Errorf("%q is not a boolean: write one of %s for on, or one of %s (or leave it unset) for off",
		raw, strings.Join(flagOn, ", "), strings.Join(flagOff[1:], ", "))
}

// envFlag reads one of the boolean switches Toft owns the name of.
//
// It is strict, and that is the decision. This package already had a lenient
// reader — telemetry.go's isTrue, where an unrecognised value is off — and its
// comment says why: OTEL_RECORD_ENDUSER_ID=no should not enable anything. That
// reasoning holds for a switch whose *on* position is the risky one, and these
// switches have no common safe direction. DEV_INSECURE_COOKIES and
// BUILD_ALLOW_SCRIPTS fail open when on; BLOCK_AWS_METADATA fails open when
// off, so BLOCK_AWS_METADATA=yse read leniently would leave every deployed app
// able to read the instance's IAM credentials on an install whose operator
// wrote the opposite. A reader that is safe for some of these and unsafe for
// one of them is not one reader.
//
// So there is no direction to guess in and the platform does not guess:
// *decide, then confirm*, the way ARTIFACT_STORE and LOG_FORMAT already refuse
// a spelling they do not recognise rather than falling back to one.
//
// Unset and empty are off, which is the compatibility promise: every install
// that leaves a switch out of its environment keeps the behaviour it has, and
// so does every install that writes 1. The only installs that change are the
// ones that wrote a word meaning off and got on — which is the defect.
func envFlag(key string) (bool, error) {
	on, err := parseFlag(os.Getenv(key))
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return on, nil
}

// envFlags reads switches while holding onto every refusal, so an operator with
// two unreadable values is told about both rather than one per restart.
type envFlags struct{ errs []error }

func (f *envFlags) read(key string) bool {
	on, err := envFlag(key)
	if err != nil {
		f.errs = append(f.errs, err)
	}
	return on
}

// integer reads a whole-number setting: unset or empty is fallback, and
// anything that is not an integer in [lo, hi] refuses the start, naming the
// variable. The same rule as a boolean's — unreadable is refused, never
// approximated — because the approximation was 0, and APPS_PORT=0 is the
// two-origin split silently collapsed (KUBE-3).
func (f *envFlags) integer(key string, fallback, lo, hi int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		f.errs = append(f.errs, fmt.Errorf("%s: %q is not a whole number", key, raw))
		return fallback
	}
	if n < lo || n > hi {
		f.errs = append(f.errs, fmt.Errorf("%s: %d is outside %d–%d", key, n, lo, hi))
		return fallback
	}
	return n
}

// readDefault is read for a switch whose *absence* is not the same as its off
// position, because the platform can work the answer out.
//
// Three answers where the strict reader has two:
//
//	absent          the derived default — nobody said anything
//	present, empty  off, because a delivery artifact spells off as empty
//	present, a word the strict reader's answer, refused if it is not a boolean
//
// The middle line is what makes this safe for the Helm chart, whose configmap
// always emits the key and emits `""` for off (ADR §34). Empty has to stay off,
// or a customer who wrote `false` in their values would get the derived default
// instead of the posture they asked for.
//
// Nothing else uses it. The other switches default off either way, so absent and
// empty are the same answer for them and telling the two apart would buy nothing.
func (f *envFlags) readDefault(key string, derived bool) bool {
	raw, present := os.LookupEnv(key)
	if !present {
		return derived
	}
	on, err := parseFlag(raw)
	if err != nil {
		f.errs = append(f.errs, fmt.Errorf("%s: %w", key, err))
	}
	return on
}

// IsolatedTopology names the configured isolated backend, for logging.
func (c *Config) IsolatedTopology() string {
	switch {
	case c.KubernetesConfigured():
		return "kubernetes"
	case c.ECSConfigured():
		return "ecs"
	default:
		return "none"
	}
}

// DefaultConcurrentBuilds is MAX_CONCURRENT_BUILDS when the environment leaves
// it unset: half the CPUs this process may use (GOMAXPROCS, which honours a
// container's CPU limit where NumCPU reads the host), at least one and at most four.
//
// It used to be 1, so a second person's deploy waited out the first one's whole
// build — on an install where the build is mostly a package manager waiting on
// the network or the disk, which is exactly the work that overlaps. Half the
// CPUs rather than all of them because an R or C++ compile already fans out
// across every CPU through MAKEFLAGS; four is the pool floor, so the default
// never asks for a ceiling the pool was not built to.
func DefaultConcurrentBuilds(cpus int) int {
	n := cpus / 2
	if n < 1 {
		return 1
	}
	if n > 4 {
		return 4
	}
	return n
}
