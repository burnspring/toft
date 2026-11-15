// Package runner defines the contract between Toft and a workload
// running somewhere it does not control, and implements the runner side of it.
//
// The contract is *versioned* because runner and platform deploy
// independently: a long-lived isolated service may outlive several platform
// upgrades, and an operator running a mismatched pair must be told rather than
// left to discover it through some unrelated symptom.
//
// Everything a workload sends home travels over this one channel — bootstrap,
// logs, consumption, progress, result. Using a cloud log service or a cloud
// metrics service instead would mean reading them back for the UI (latency,
// extra permissions, and a third and fourth path beside process tailing), and
// would mean a different mechanism per cloud. Native log drivers still run in
// parallel as the operator's debug view.
package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// ContractVersion is the runner side of the versioned contract. It travels in
// a header on every callback; the platform compares it against its own and
// says so on mismatch rather than failing obscurely later.
const ContractVersion = "2"

// VersionHeader carries ContractVersion on every callback request.
const VersionHeader = "X-Toft-Runner-Version"

// BootstrapFailed opens the line a runner prints when its bootstrap fails, so
// the platform can tell that the workload died before any app code ran. The
// readiness check matches on it (builder.watchWorkload) to say the failure is
// Toft's, not the app's (TOFT-BB-003). Change both together.
const BootstrapFailed = "toft runner: could not get the app's start-up details from the platform"

// Environment variables the platform sets on a runner. These are the runner's
// own inputs — deliberately *not* the app's. App code sees the app's
// environment and the narrowed run token, and nothing here.
const (
	EnvCallbackURL = "TOFT_CALLBACK_URL"
	EnvMode        = "TOFT_RUNNER_MODE" // "build" | "serve" | "job"
	EnvWorkDir     = "TOFT_WORK_DIR"
	// The host backend's three inputs (ADR §106). A run on the platform's own
	// machine has no projected identity, no pod-local scratch and no need to
	// download a release it can read from disk, so each of those is named
	// rather than assumed. All three are unset before app code runs, like the
	// rest of this block.
	EnvIdentityFile = "TOFT_IDENTITY_FILE"
	EnvReleaseDir   = "TOFT_RELEASE_DIR"
	EnvScratchPath  = "TOFT_RUNNER_SCRATCH"
	// The address a host service listens on, which is the placer's fact and
	// not the bootstrap's (ADR §131): a pod's network
	// namespace makes 0.0.0.0:8080 right for every pod, and a process sharing
	// the machine's must bind the port the shared backend allocated, on
	// loopback. Set, they override the bootstrap's answer; unset in a pod.
	// Unset before app code runs, like the rest of this block — the app reads
	// PORT and HOST.
	EnvPort = "TOFT_PORT"
	EnvBind = "TOFT_BIND"
)

// IdentityMountPath is where the workload's identity is projected, and
// IdentityTokenPath is the file inside it the runner reads to prove who it is.
//
// A *file*, not an environment variable, and that is the whole of contract
// version 2. The credential is issued by the infrastructure — a projected
// ServiceAccount token, bound to this pod, scoped to an audience the API server
// itself refuses — and rotated in place before it expires. Nothing in the pod
// spec has a deadline any more, which is what makes an in-place container
// restart work: the runner re-execs, re-reads this file, and presents a
// credential that is current.
//
// It is therefore re-presentable by the pod it was issued to, for as long as
// that pod lives, which is the point rather than a regression. What it is not
// is transferable: another pod's token names another pod.
const (
	IdentityMountPath = "/var/run/toft/identity"
	IdentityTokenPath = IdentityMountPath + "/token"
)

// Environment variables the runner exports *into* app code. The run token is
// here; the bootstrap token deliberately is not.
const (
	EnvRunToken = "TOFT_JOB_TOKEN"
	EnvRunID    = "TOFT_RUN_ID"
	EnvJobName  = "TOFT_JOB_NAME"
	EnvAppSlug  = "TOFT_APP_SLUG"
	EnvTrigger  = "TOFT_TRIGGER"
	// EnvTriggerRole answers *who* started this run, where EnvTrigger answers
	// *how*. Derived at bootstrap from the same row the trigger comes from —
	// job_runs.actor_id resolved to a granted role, `system` for a schedule,
	// `app` for the app acting as itself — and never stored, because a stored
	// role would be a second answer that ages.
	EnvTriggerRole = "TOFT_TRIGGER_ROLE"
	EnvParams      = "TOFT_PARAMS"
	// EnvScheduledFor is the instant a run is for — its schedule's tick, or one
	// a person named — as RFC 3339 in the schedule's own zone. Absent when
	// nobody gave one, never defaulted to now (ADR §104).
	EnvScheduledFor = "TOFT_SCHEDULED_FOR"
	EnvAPIURL       = "TOFT_API_URL"
	// EnvAppToken is the app's outbound credential, and it is spelled the same
	// here as the shared backend spells it (`runtime.EnvAppToken`) so the SDKs
	// read one variable in both execution modes. Set for a service workload,
	// absent for a job.
	EnvAppToken = "TOFT_APP_TOKEN"
	// EnvAppID is the app's own id, spelled the same here as the shared backend
	// spells it (`runtime.EnvAppID`) for the reason the token and the role key
	// are — so an SDK reads one variable wherever the app runs. Set for a
	// service workload, absent for a job, on the same condition as the token:
	// the platform decides which by sending an app token or not, and the runner
	// keeps no rule of its own about it.
	//
	// No route accepts an app id — the app-facing surface is entirely
	// self-referential — so this is here so an app can put its own id in its
	// own logs, and the SDK never sends it. It is also the one identity a
	// rename cannot move, which is why an app would want it rather than the
	// slug.
	EnvAppID = "TOFT_APP_ID"
	// EnvRoleKey is the key this workload verifies X-Toft-Role with, and
	// it is spelled the same here as the shared backend spells it
	// (`runtime.EnvRoleKey`) so an app verifies identically in both execution
	// modes. Set for a service workload, absent for a job — a job serves no
	// requests, so there is nothing for it to verify.
	EnvRoleKey = "TOFT_ROLE_KEY"
	// EnvOutputURI is where a job writes its one output. A job writes it with
	// an ordinary HTTP client and never learns whether the other end is an
	// object store, which is what lets the same job code run anywhere. A job's
	// only input is its params; large input is the app's storage.
	EnvOutputURI = "TOFT_OUTPUT_URI"
	// EnvRerunOf is the run this one deliberately repeats, absent otherwise.
	EnvRerunOf = "TOFT_RERUN_OF"
	// EnvDeadline is when this run will be stopped, RFC 3339 — what a job
	// reads to decide whether there is time for one more batch.
	EnvDeadline = "TOFT_DEADLINE"
	// EnvStorage describes the durable storage mounted into this workload: a
	// JSON array of {name, kind, path, access}.
	//
	// It arrives in the runner's *own* environment, set by the backend beside
	// the mounts themselves, and is passed through to app code unchanged. That
	// is deliberate rather than incidental: computing it a second time at
	// bootstrap would let the description and the filesystem disagree if a
	// mapping changed in the window between the workload being placed and it
	// calling home. Mounted paths are declared, not discovered — code looks a
	// resource up by name so an administrator can move it.
	EnvStorage = "TOFT_STORAGE"
	// EnvScratchDir is the app's ephemeral scratch directory; TMPDIR points at
	// the same place. Unlike EnvStorage it needs no pod-spec plumbing: the
	// mount path is fixed (ScratchMountPath), so the runner sets both vars
	// itself. Same name as runtime.EnvScratchDir — restated here because this
	// package is deliberately standalone.
	EnvScratchDir = "TOFT_SCRATCH_DIR"
	// EnvConsumption is how the runner's figures are measured. Unset, the
	// runner reads its own control group, which in a pod is the workload's.
	// ConsumptionByHost says the platform measures this run itself: a host run
	// with no cgroup of its own shares the platform's, and reading that
	// reported the whole platform's memory as the run's.
	EnvConsumption = "TOFT_CONSUMPTION"
)

// ConsumptionByHost is EnvConsumption's one value.
const ConsumptionByHost = "host"

// ScratchMountPath is where the scratch emptyDir appears inside an isolated
// workload. A constant shared by the kube backend (which mounts it) and the
// runner (which points TMPDIR and EnvScratchDir at it), so the path an app is
// told and the path that is actually mounted cannot disagree. Ephemerality
// comes from the pod lifecycle: a restart is a pod recreation, and the
// emptyDir dies with the pod.
const ScratchMountPath = "/workspace/scratch"

// FailureStage names where a workload died. Reported distinctly because
// collapsing them into a bare exit code moves debugging into the cloud
// console, which is what this design exists to prevent — an operator must see
// *which* stage failed in Toft's own UI.
type FailureStage string

const (
	StageImagePull FailureStage = "image_pull"
	// StageRuntimeVersion is this image not being what the platform was told it
	// is. Its own stage because it is neither an image problem the cluster can
	// see nor a fault in the app: it is a configuration disagreement between
	// RUNNER_*_VERSION and the image that variable names, and an operator
	// reading "build failed" would go and look at the wrong thing.
	StageRuntimeVersion      FailureStage = "runtime_version"
	StageArtifactFetch       FailureStage = "artifact_fetch"
	StageBuild               FailureStage = "build"
	StageAppExec             FailureStage = "app_exec"
	StageCallbackUnreachable FailureStage = "callback_unreachable"
)

// BootstrapRequest is the runner's first call home. It presents the bootstrap
// token and nothing else: everything sized travels in the *response*, because
// ECS caps RunTask overrides at about 8KB and because secrets in an override
// would be readable from DescribeTasks output.
type BootstrapRequest struct {
	Version string `json:"version"`
	// WorkloadName lets the platform confirm the caller is the workload it
	// thinks it is before handing over an environment.
	WorkloadName string `json:"workload_name"`
}

// ServiceTokenTTL is how long a **service** workload's run token lives.
//
// A run token's life is its run's timeout, so a token from a finished run is
// inert. A service has no run to be bounded by, and this is the answer instead:
// short, and exchanged rather than extended. The runner presents the identity
// the cluster issued and kubelet rotates, and gets a fresh one — so a leaked
// service token is still worth less than an hour, which a long TTL would have
// given away to remove the same cliff.
const ServiceTokenTTL = time.Hour

// TokenRequest asks for a fresh run token, presenting the workload identity
// rather than the credential that is about to expire.
type TokenRequest struct {
	Version string `json:"version"`
	// WorkloadName is confirmed against the identity for the same reason
	// bootstrap confirms it: a token minted for one workload must not be
	// obtainable by another.
	WorkloadName string `json:"workload_name"`
}

// TokenResponse carries the exchanged credential and nothing else.
//
// Deliberately not a second BootstrapResponse. Re-bootstrapping to refresh a
// credential would decrypt the app's environment again, reopen the workload's
// narration budget, and mint a fresh app token — superseding the one the
// process already running in this pod holds in its environment. A refresh must
// change exactly one thing.
type TokenResponse struct {
	Version  string `json:"version"`
	RunToken string `json:"run_token"`
}

// BootstrapResponse is everything a workload needs to run, fetched at startup
// rather than injected. This is the decided secrets path: not a secrets-manager
// sync, not plaintext overrides.
type BootstrapResponse struct {
	Version string `json:"version"`

	AppID    string `json:"app_id"`
	AppSlug  string `json:"app_slug"`
	BasePath string `json:"base_path"`

	// Framework selects the start invocation, exactly as it does for the
	// shared backend — one detector, one set of per-framework commands.
	Framework  string `json:"framework"`
	Entrypoint string `json:"entrypoint,omitempty"`
	StartCmd   string `json:"start_cmd,omitempty"`

	// RuntimeLanguage and RuntimeVersion are what the platform believes this
	// image carries, from RUNNER_PYTHON_VERSION / RUNNER_NODE_VERSION /
	// RUNNER_R_VERSION — and the runner's job is to check that belief against
	// its own filesystem before it starts anything (ADR §64 D17).
	//
	// **RuntimeVersion is a comma-separated list, spelled exactly as the
	// variable it comes from** (D19), and the runner requires *every* series in
	// it to be present. One version is that list with one entry, which is what
	// lets an install that declared a single version keep meaning it.
	//
	// The whole declaration rather than the series this app resolved to,
	// because the two answer different questions and the app's own is already
	// answered better elsewhere: DiscardUnusable reads the series stamped into
	// the release's library and refuses with both numbers when this image
	// cannot load it. That check is per app, arrives with the tree in hand, and
	// works on a restart and a wake as well as a start. This one is about the
	// *image*, and an image missing a declared series is a misconfiguration
	// every app that resolves to it will hit — so it is worth saying once, at
	// bootstrap, rather than once per unlucky app.
	//
	// **Decide, then confirm.** The deploy decision cannot wait for a pod: the
	// build produces the venv and node_modules on the platform host, so by the
	// time this response is being written the artifact is already committed to
	// an interpreter. The declaration is therefore what the deploy is judged
	// against, and this is what catches a declaration that was wrong.
	//
	// The residual is stated rather than closed: between a wrong RUNNER_*_VERSION
	// and the first pod start, the platform will accept a pin it cannot honour.
	// That is a misconfiguration, it surfaces loudly here on the next deploy, and
	// the alternative — pulling and inspecting the image before accepting a
	// deploy — buys little for a great deal of cluster coupling.
	//
	// Empty for a static workload, which has no interpreter, and for Go, whose
	// toolchain stays on the build host: `go build` produces a binary and the
	// workload execs that.
	RuntimeLanguage string `json:"runtime_language,omitempty"`
	RuntimeVersion  string `json:"runtime_version,omitempty"`

	// SDKBuild is [sdk.Build] for this app's language as the *platform* carries
	// it, so the runner can say whether the SDK it is about to install is the
	// same one the platform serves.
	//
	// **The two copies are independent by design and nothing compared them.**
	// The SDK is embedded in both binaries — this one and the platform's — and
	// the two upgrade separately, which is the property the family image exists
	// to have. An install that moved one and not the other installs one SDK
	// into an app's venv here and serves a different one from `GET
	// /api/v1/sdk/{lang}`, which is what `toft sdk vendor` hands a developer to
	// build against. Both declare the same version, because a version is a
	// declaration and both trees declared it, so nothing an app or its author
	// could read told them apart: it was found by reading `inspect.getsource`
	// inside a running process.
	//
	// Decide, then confirm — the same shape as RuntimeVersion above, and a
	// warning rather than a refusal, because the two differ in what a
	// disagreement costs. An image that cannot run the interpreter the deploy
	// resolved to cannot run the app at all; an SDK a version behind still
	// works, and an app that never imports it is unaffected. The deploy says
	// so and carries on.
	//
	// Empty from an install older than the field, which the runner reads as
	// "this install does not say" and never as agreement.
	SDKBuild string `json:"sdk_build,omitempty"`

	// RRepos is the package repository an R repair installs from — the same
	// TOFT_R_REPOS the platform-side build used.
	//
	// Carried rather than baked into the image, for the reason the value is an
	// install's rather than an image's: an operator moves it to pick up a
	// package's security update, or to point at their own mirror, and a value
	// compiled into the family image could not follow. It reaches the pod here
	// because a repair happens in the pod and has to install from the same
	// place the deploy did, or the rebuilt library is not the one that was
	// tested.
	//
	// Empty for every framework that is not R.
	RRepos string `json:"r_repos,omitempty"`

	// ArtifactURL is a presigned URL for the release tarball. Presigned so the
	// workload needs no object-store credential of its own: app code that
	// leaked one could read every other app's releases.
	ArtifactURL string `json:"artifact_url"`
	// ArtifactSHA256 lets the runner verify what it fetched before executing
	// any of it.
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`

	// Env is the app's decrypted environment. The response is the only place
	// it exists in the clear outside the platform.
	Env map[string]string `json:"env"`

	// RunToken is the narrowed credential the runner exports to app code:
	// progress, logs, consumption, result. Never the bootstrap token.
	RunToken string `json:"run_token"`

	// AppToken is the app's outbound credential (`tofta_…`) — what `toft.notify`,
	// `toft.jobs`, `toft.viewer` and the app-facing schedule trigger authenticate
	// with. Empty for a **job** workload, deliberately: ADR §27 D1 rests on a
	// job holding none, which is why a report generated by a job has nothing
	// able to mail it and why scheduled tasks are HTTP triggers.
	//
	// It travels here rather than in the pod spec because a pod spec is
	// readable by anyone with get-pod in the namespace, and this credential
	// lives until the next deploy. Nothing long-lived goes in the spec — which
	// is now the whole rule, since the spec carries no credential at all: the
	// workload's identity is a projected token kubelet issues and rotates.
	// This response is already the one place an app's secrets exist in the
	// clear outside the platform.
	AppToken string `json:"app_token,omitempty"`

	// RoleKey is what this workload verifies X-Toft-Role with: the
	// platform's per-app, per-start derivation. It travels here rather than in
	// the pod spec for the same reason AppToken does — a spec is readable by
	// anyone with get-pod in the namespace — and it is derived rather than
	// stored, so a redeploy invalidates every assertion minted for the previous
	// start and nothing anywhere can read this value back.
	//
	// Service workloads only. Empty means the app cannot verify a role and will
	// read every one as `unknown`, which refuses.
	RoleKey string `json:"role_key,omitempty"`

	// Job fields, empty for a service workload.
	RunID   string `json:"run_id,omitempty"`
	JobName string `json:"job_name,omitempty"`
	Command string `json:"command,omitempty"`
	// Args is the argv of the release's single built binary, run instead of a
	// shell command line. Empty when Command is set.
	Args []string `json:"args,omitempty"`
	// Trigger is who started the run: app, viewer, cli, schedule or ui.
	Trigger string `json:"trigger,omitempty"`
	// RerunOf is the run this one deliberately repeats.
	RerunOf string `json:"rerun_of,omitempty"`
	// Deadline is when the run will be stopped, RFC 3339.
	Deadline string `json:"deadline,omitempty"`
	// TriggerRole is the granted role of whoever started this run — the *who*
	// beside Trigger's *how*. Resolved at bootstrap from the run's actor rather
	// than carried on the row: a role recorded when the run was queued would be
	// a second answer that ages, and job_runs already records the provenance
	// this is derived from.
	TriggerRole string          `json:"trigger_role,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	// ScheduledFor is EnvScheduledFor's value, already formatted in the
	// schedule's zone; empty when the run carries none.
	ScheduledFor string `json:"scheduled_for,omitempty"`

	// OutputURL is what a job needs beyond its environment: somewhere to put
	// what it produces.
	//
	// It is a *presigned URL*, not an object-store path and not a credential.
	// A credential that can write this run's output can, in every object store
	// worth using, also read every other app's data — so app code never gets
	// one. A URL is narrow to one object, one verb, and one short window, and a
	// leaked one is worth exactly that much.
	//
	// OutputURL accepts a single PUT. A job producing several files asks the
	// platform for more URLs through its run token rather than being handed a
	// prefix it could write anywhere under.
	OutputURL string `json:"output_url,omitempty"`

	// Port and BindAddress are what a service workload must listen on. An
	// isolated workload binds all interfaces because its network namespace is
	// the boundary; the invariant that it is unreachable except through the
	// gateway is unchanged, only its mechanism is.
	Port        int    `json:"port,omitempty"`
	BindAddress string `json:"bind_address,omitempty"`

	// TimeoutSeconds is the run's deadline, and also the life of RunToken.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// DependencyRefusal is the dependency policy's answer for **this release**,
	// empty when it may run (ADR §39 D5).
	//
	// The pod is told the answer and never handed the machinery to compute it:
	// it has no egress to the advisory feed, would need the feed credential,
	// and a second implementation of the matcher could disagree with the
	// first. Same rule as "an isolated workload carries a trace id, never an
	// exporter".
	//
	// **It is the release's verdict, not today's.** A pod that is evicted,
	// OOM-killed or rolled by a node drain bootstraps again with no new code
	// and nothing anybody did; a handler that evaluated the current feed would
	// refuse it, and an app serving happily since March would simply not come
	// back at 3am because a third party published an advisory. What new
	// information changes is the next *deploy*.
	DependencyRefusal string `json:"dependency_refusal,omitempty"`
}

// LogBatch is a batch of shipped output. Batched with a per-run cap and
// explicit truncation: a job in a tight print loop must not be able to turn
// log ingest into a denial of service against the platform it reports to.
type LogBatch struct {
	Version string     `json:"version"`
	Lines   []LogLine  `json:"lines"`
	Sent    time.Time  `json:"sent"`
	Dropped int        `json:"dropped,omitempty"`
	Stage   *StageMark `json:"stage,omitempty"`
}

type LogLine struct {
	Seq int       `json:"seq"`
	At  time.Time `json:"at"`
	// Source is LogSourceApp or LogSourcePlatform — who spoke.
	//
	// The shipper has known it since record() was written and used to drop it
	// at this boundary, because a service's whole batch was discarded at ingest
	// and a run's lines are all the app's by definition. Now that the platform's
	// half of a service's batch is kept, the label has to survive the wire: the
	// tee's marker is on the *pod's* copy and nothing above the seam can read a
	// line the pod no longer holds.
	//
	// omitempty, and an absent value reads as the app's — which is what an
	// older runner's every line actually is, since it labelled nothing. That is
	// the same degradation ParseLogLine already makes, and the reason
	// ContractVersion does not move for this.
	Source string `json:"source,omitempty"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// StageMark lets the runner say which stage it has reached, so a workload that
// dies without reporting is still attributable to a stage.
type StageMark struct {
	Stage FailureStage `json:"stage"`
}

// ConsumptionReport is the isolated collector's half of the metric model. It
// carries exactly the same figures the shared collector samples, so the
// storage and reporting layers never learn which mode produced a number.
//
// Reported on the same batched interval as logs rather than at exit: an
// out-of-memory kill is the case that matters most, and reporting at exit
// would lose the record precisely when it is most wanted.
type ConsumptionReport struct {
	Version string `json:"version"`
	// CPUSeconds is cumulative since the workload started, not since the last
	// report — a missed interval then loses resolution but not total.
	CPUSeconds  float64 `json:"cpu_seconds"`
	MemoryBytes int64   `json:"memory_bytes"`
	PeakBytes   int64   `json:"peak_bytes"`
	// Periods, ThrottledPeriods and ThrottledSeconds are cpu.stat's
	// throttling counters, cumulative like CPUSeconds. Absent — never zero —
	// where the workload's cgroup keeps none.
	Periods          *int64    `json:"periods,omitempty"`
	ThrottledPeriods *int64    `json:"throttled_periods,omitempty"`
	ThrottledSeconds *float64  `json:"throttled_seconds,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
}

// ProgressReport is a job telling the platform how far along it is.
type ProgressReport struct {
	Version string `json:"version"`
	Percent int    `json:"percent"`
	// Message is an optional short line beside the percentage — "loading
	// rows" — shown wherever the run's progress is.
	Message string `json:"message,omitempty"`
}

// OutputURLPath mints a fresh output PUT URL for the calling run, at the
// moment it writes. The URL handed over at bootstrap (EnvOutputURI) was signed
// for the run's timeout capped at six hours, and on S3 behind a role with a
// credential that may expire sooner, so a long run's write would find it dead;
// every SDK's write_output asks here first and falls back to EnvOutputURI.
// Authenticated with the run token; the key is the run's own, fixed
// server-side, and a finished run is refused.
const OutputURLPath = "/api/v1/runner/output-url"

// OutputURLResponse is OutputURLPath's answer. Headers are the ones the PUT
// must carry (Azure Blob's x-ms-blob-type); the SDKs send that header always.
type OutputURLResponse struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expires_at"`
}

// OutputBlobTypeHeader is the header every output PUT carries: Azure Blob's
// Put Blob refuses a request without it, and S3 ignores it.
const OutputBlobTypeHeader = "x-ms-blob-type"

// ResultReport is a job's structured result plus its outcome.
type ResultReport struct {
	Version  string          `json:"version"`
	Result   json.RawMessage `json:"result,omitempty"`
	ExitCode int             `json:"exit_code"`
	// Stage and Reason are set only on failure, and Stage is what tells an
	// operator whose problem it is: their registry, the artifact store, the
	// app's build, the app's code, or the platform's reachability.
	Stage  FailureStage `json:"stage,omitempty"`
	Reason string       `json:"reason,omitempty"`
	// RecordOnly stores Result and leaves the run in flight. It is what the
	// SDKs' set_result sends, because the job is still running when it records
	// its result; only the runner's own report, at the end, finishes a run.
	// Without it a set_result call finished the run as succeeded on the spot,
	// so the job's later sends were refused and its later failure was lost
	// under first-terminal-status-wins.
	RecordOnly bool `json:"record_only,omitempty"`
}

// VersionMismatch describes a runner and platform that do not agree.
type VersionMismatch struct {
	Runner   string `json:"runner"`
	Platform string `json:"platform"`
}

func (v VersionMismatch) Error() string {
	return "runner contract version mismatch: runner speaks " + v.Runner +
		", platform speaks " + v.Platform +
		" — a partial upgrade; redeploy the mismatched half"
}

// CheckVersion compares a reported version against the platform's.
//
// Mismatch is an explicit, named outcome rather than best-effort tolerance: a
// partial upgrade must fail loudly, because the alternative is a workload that
// half-works in ways nobody traces back to a version skew.
func CheckVersion(runnerVersion, platformVersion string) error {
	if runnerVersion == "" {
		return VersionMismatch{Runner: "(unreported)", Platform: platformVersion}
	}
	if runnerVersion != platformVersion {
		return VersionMismatch{Runner: runnerVersion, Platform: platformVersion}
	}
	return nil
}

// The log marker — how a teed line says who is speaking.
//
// A pod's log is one undifferentiated stream: kubelet merges stdout and stderr
// before anything can read them back, and the API offers no way to tell them
// apart. The shipper knows both the stream and whether it was the runner or the
// app speaking, so if the tee does not write those down, the pull loses what
// the push had.
//
// Deliberately not JSON lines. An operator running `kubectl logs` on a broken
// pod is a first-class reader of this stream — and reads it during exactly the
// incidents where reading it matters most — so the marker is fixed, ASCII, and
// short enough that the line still reads as a line. A parser is served no
// better by JSON than by this.
//
// The timestamp is the runner's own, the same one the shipped record carries,
// rather than kubelet's `Timestamps: true`. Kubelet would stamp every line a
// second time, and it is the runner's stamp that matches what a job's log says.
const (
	// LogSourceApp is the workload's own output, and LogSourcePlatform is
	// Toft speaking. An app may never produce the second: see NeutralizeLine.
	LogSourceApp      = "app"
	LogSourcePlatform = "platform"

	// LogMarkerTimeLayout is millisecond-precision UTC. Nanoseconds are noise
	// at the rate a log is read, and whole seconds put too many lines on the
	// same stamp to order a fast failure by.
	LogMarkerTimeLayout = "2006-01-02T15:04:05.000Z"
)

// logMarkerPattern matches a marker at the start of a line: an ASCII bracket,
// a timestamp, a source, a slash, a stream, a closing bracket and one space.
//
// Anchored and narrow on purpose. It decides two different things — how the
// platform reads its own lines back, and whether an app is trying to forge one
// — and a loose pattern makes the first miss and the second over-trigger.
var logMarkerPattern = regexp.MustCompile(
	`^\[\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z (app|platform)/(stdout|stderr)\] `)

// LogMarker renders the prefix a teed line carries.
func LogMarker(at time.Time, source, stream string) string {
	return fmt.Sprintf("[%s %s/%s] ", at.UTC().Format(LogMarkerTimeLayout), source, stream)
}

// ParseLogLine reads a teed line back, returning the marker's three facts and
// the text after it.
//
// A line with no marker is the app's own output on stdout. That is what an
// older runner's output actually is — every line of it, since it wrote nothing
// of its own to the pod — which is why the contract version does not move for
// this: the parse degrades safely, and forcing a fleet-wide redeploy of every
// isolated service to fix a display would be the wrong trade.
func ParseLogLine(text string) (at time.Time, source, stream, line string) {
	m := logMarkerPattern.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, LogSourceApp, "stdout", text
	}
	// The layout parsed the pattern that matched it, so an error here is not
	// reachable; a zero time is the honest answer if it ever were.
	stamp, _ := time.Parse(LogMarkerTimeLayout, text[1:1+len(LogMarkerTimeLayout)])
	return stamp, m[1], m[2], text[len(m[0]):]
}

// NeutralizeLine defuses a line of app output that is shaped like a marker.
//
// Without it an app prints `[…Z platform/stderr] ERROR at app_exec: …` and the
// Logs tab shows Toft blaming itself, in Toft's own voice, for the
// app's failure. The runner owns file descriptor 1 and app output reaches it
// only through the shipper's Pump, so there is exactly one place this can be
// done and this is it.
//
// The marker is broken rather than the line dropped: the app did print this,
// and a log that silently swallows output is worse than one showing a mangled
// bracket. What it must not do is survive as a *valid* marker.
func NeutralizeLine(line string) string {
	if logMarkerPattern.MatchString(line) {
		return " " + line
	}
	return line
}

// MarkedWriter wraps an io.Writer so every line written through it carries a
// log marker naming the given source.
//
// The standard logger is the reason this exists. The runner's own `log.Printf`
// diagnostics go straight to stderr and never touch the shipper, so the tee
// cannot mark them — and an unmarked line reads as the *app's* output, which
// would put the platform's diagnostics in the app's voice. That is the same
// confusion NeutralizeLine prevents in the other direction, and it is worth as
// much: "the runner could not reach the platform" attributed to the app sends
// its author looking for a bug they do not have.
//
// Found by reading a real pod's log rather than by reasoning about one: the
// tail of a healthy isolated workload is `toft-runner: …` lines, not the
// near-empty stream the design assumed.
func MarkedWriter(w io.Writer, source string) io.Writer {
	return &markedWriter{w: w, source: source}
}

type markedWriter struct {
	w      io.Writer
	source string
}

func (m *markedWriter) Write(p []byte) (int, error) {
	// The logger hands over one formatted record, newline included. Marking it
	// per line keeps a multi-line record legible rather than marking only its
	// first line.
	text := strings.TrimSuffix(string(p), "\n")
	at := time.Now().UTC()
	var b strings.Builder
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(LogMarker(at, m.source, "stderr"))
		b.WriteString(line)
	}
	b.WriteByte('\n')
	if _, err := io.WriteString(m.w, b.String()); err != nil {
		return 0, err
	}
	// Report the caller's own length: a short write of our longer string would
	// otherwise look to the logger like a partial write of its record.
	return len(p), nil
}
