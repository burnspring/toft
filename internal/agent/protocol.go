// Package agent is the shared-mode agent: one process per install that places,
// supervises and reports on the workloads of shared mode (ADR §129).
//
// It runs as the platform's own binary (`toft agent`), started by the platform
// and outliving it, so a platform restart — every save on the development box —
// restarts no app. It never runs anything but `<its binary> runner`: the runner
// is the one launcher, and the agent is where it is placed, what restarts it,
// where its output goes and who vouches for it.
//
// The platform talks to it over HTTP/1.1 and JSON on a Unix socket only root can
// open. Nothing that crosses the socket is a secret: a workload's environment
// is fetched by its runner at bootstrap, and the description below carries
// where to run, as whom, and nothing it could not print.
package agent

import (
	"path/filepath"
	"time"
)

// Command is the platform binary's subcommand that is the agent.
const Command = "agent"

// ProtocolVersion is what the platform and the agent compare. Compatibility is
// this number and never the build: a platform rebuilt with the same protocol
// keeps the agent it finds, which is what lets the development box rebuild on
// every save without restarting an app.
//
// 2: Spec.CPULimitMillicores. A field an older agent does not know is dropped
// on decode, and the workload would start uncapped while every screen said
// enforced — so a new field the agent must honour is a new protocol, and an
// older agent is replaced in place rather than trusted to ignore it.
const ProtocolVersion = "2"

// ProtocolHeader carries ProtocolVersion on every request and response.
const ProtocolHeader = "X-Toft-Agent-Protocol"

// DefaultDir is where an install's agent keeps its socket, its state and the
// identity files it issues. Under /run, so a reboot empties it — the pids in
// the state mean nothing after one — and never under APPS_DIR, which may be a
// network filesystem. AGENT_DIR names another, which two installs on one
// machine need.
const DefaultDir = "/run/toft/agent"

// SocketPath is the agent's socket inside its directory.
func SocketPath(dir string) string { return filepath.Join(dir, "agent.sock") }

// LogPath is the agent's own log, which the platform reads back when an agent
// it started never answered.
func LogPath(dir string) string { return filepath.Join(dir, "agent.log") }

// Workload kinds. A service is restarted whenever it ends; a run never is.
const (
	KindService = "service"
	KindRun     = "run"
)

// Restart policies.
const (
	RestartAlways = "always"
	RestartNever  = "never"
)

// Spec describes one workload. It is the whole of what the platform tells the
// agent, and it holds no secret: the runner fetches the app's environment at
// bootstrap, with an identity the agent issues here (TOFT_IDENTITY_FILE).
//
// There is deliberately no argv. The agent runs its own binary's `runner`
// subcommand and nothing else, so a description that reached the socket some
// other way could at most start the runner as some account — never a program.
type Spec struct {
	Name  string `json:"name"`
	AppID string `json:"app_id"`
	Kind  string `json:"kind"`

	// UID, GID and Groups are the account the runner runs as, resolved by the
	// platform. Groups is the whole supplementary list — the per-app group and
	// every storage gate — because Go's spawn sets exactly what it is given.
	// A UID of -1 runs the runner as the agent's own user.
	UID    int   `json:"uid"`
	GID    int   `json:"gid"`
	Groups []int `json:"groups,omitempty"`
	// IdentityGID is the group allowed to read the workload's identity file:
	// the per-app group, which no other app account holds. Zero means none —
	// the file is then owned by UID instead.
	IdentityGID int `json:"identity_gid,omitempty"`

	// Dir is the runner's working directory: the release, for a service.
	Dir string `json:"dir"`
	// Env is the runner's environment, in full: the agent adds the identity
	// file and nothing else.
	Env map[string]string `json:"env"`

	// MemoryLimitMB is the cap the agent holds the workload to. Zero is none.
	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`
	// CPULimitMillicores is the CPU time the workload may take, written as
	// its cgroup's cpu.max: 1000 is 100 ms per 100 ms, on any of the
	// machine's cores. Zero is none, and below the cgroup tier nothing holds
	// it.
	CPULimitMillicores int `json:"cpu_limit_millicores,omitempty"`
	// Restart is RestartAlways or RestartNever.
	Restart string `json:"restart"`

	// LogPath is the file the workload's output is appended to. Its directory
	// is the platform's to make and to gate.
	LogPath string `json:"log_path"`

	// Mask and Keep are the namespace tier's view of the machine: every
	// directory in Mask is covered with an empty tmpfs, and every one in Keep
	// that a mask would hide is put back. The platform names APPS_DIR,
	// LOGS_DIR and CACHE_DIR to mask and the app's own tree to keep; the agent
	// adds its own directory and the workload's identity. Ignored below the
	// namespace tier.
	Mask []string `json:"mask,omitempty"`
	Keep []string `json:"keep,omitempty"`

	// Labels are the platform's own facts about the workload — the deployment
	// it is a release of, the run it is — echoed back on every listing and
	// event, so a reader never has to parse a name.
	Labels map[string]string `json:"labels,omitempty"`
}

// The states a workload is reported in: the seam's four words
// (runtime.Status*), plus exited for a run that has ended.
const (
	StateOnline    = "online"
	StateLaunching = "launching"
	StateStopped   = "stopped"
	StateErrored   = "errored"
	StateExited    = "exited"
)

// Status is what the agent knows about one workload.
type Status struct {
	Name   string            `json:"name"`
	AppID  string            `json:"app_id"`
	Kind   string            `json:"kind"`
	Labels map[string]string `json:"labels,omitempty"`

	State string `json:"state"`
	// PID is the runner's, while one is running; zero otherwise.
	PID int `json:"pid,omitempty"`
	// StartedAt is when the current process started, CreatedAt when the
	// workload was first described under this name — kept across restarts.
	StartedAt time.Time `json:"started_at,omitzero"`
	CreatedAt time.Time `json:"created_at"`
	Restarts  int       `json:"restarts"`

	// How the most recent process ended. ExitCode is nil when it has not, or
	// when it ended while the agent was not its parent: a non-parent cannot
	// read an exit status, and nil is the seam's word for "not known".
	ExitCode   *int   `json:"exit_code,omitempty"`
	ExitSignal string `json:"exit_signal,omitempty"`
	// OOMKilled says the most recent end was the agent holding the memory
	// limit, which is the agent's certain answer rather than an inference.
	OOMKilled bool `json:"oom_killed,omitempty"`
	// ExitPeakMemoryMB is the most the most recent *ended* process held, kept
	// beside OOMKilled because it describes the same ending. PeakMemoryMB below
	// moves to the restarted process the moment it starts, so without this the
	// figure that explains a memory kill is gone before anyone reads it.
	ExitPeakMemoryMB int `json:"exit_peak_memory_mb,omitempty"`
	// CrashLooping is the agent's verdict: it is backing off, and the workload
	// has not stayed up for the reset interval since it last restarted.
	CrashLooping bool `json:"crash_looping,omitempty"`

	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`
	// PeakMemoryMB is the most the most recent process was seen to hold: its
	// cgroup's own high-water mark, or the watchdog's samples.
	PeakMemoryMB int    `json:"peak_memory_mb,omitempty"`
	Detail       string `json:"detail,omitempty"`
	// Address is the workload's own, at the namespace tier, where it listens
	// on WorkloadPort: its endpoint, kept for the workload's life.
	Address string `json:"address,omitempty"`
	// Cgroup is the running process's own cgroup, when it has one: its runner
	// reports its own consumption from it, as in a pod, and an outside sampler
	// must not bill it again (ADR §132).
	Cgroup string `json:"cgroup,omitempty"`
	// CPUSeconds and MemoryBytes are read from that cgroup, and absent without
	// one — the platform measures the process tree instead.
	CPUSeconds  *float64 `json:"cpu_seconds,omitempty"`
	MemoryBytes *int64   `json:"memory_bytes,omitempty"`
}

// The containment tiers (ADR §128), lowest first. Detected once, at start.
const (
	TierProcess   = "process"
	TierCgroup    = "cgroup"
	TierNamespace = "namespace"
)

// How a namespaced workload resolves names (dns_linux.go): through the host's
// own file, a file naming systemd-resolved's upstreams, the agent's forwarder
// on the gateway, or — the forwarder unable to bind — not reliably.
const (
	DNSHost         = "host"
	DNSUpstreamFile = "upstream_file"
	DNSForwarder    = "forwarder"
	DNSUnavailable  = "unavailable"
)

// List is GET /workloads.
type List struct {
	Protocol string `json:"protocol"`
	// Tier is how this agent holds its workloads — TierProcess or TierCgroup,
	// fixed for its life — and TierReason why the next tier up was refused.
	Tier       string `json:"tier"`
	TierReason string `json:"tier_reason,omitempty"`
	// MemoryHeldBy is "cgroup" where the kernel holds a run's memory limit
	// and "watchdog" where the agent samples it.
	MemoryHeldBy string `json:"memory_held_by"`
	// CPUHeldBy is "cgroup" where the kernel holds a workload's CPU limit —
	// the cgroup tier with the cpu controller delegated — and "none" where
	// nothing does.
	CPUHeldBy string `json:"cpu_held_by"`
	// Bridge is the range and Gateway the platform's address on it, at the
	// namespace tier: a workload's TOFT_CALLBACK_URL names the gateway, since
	// its own loopback is its own.
	Bridge  string `json:"bridge,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// DNS is how a namespaced workload resolves names — DNSHost,
	// DNSUpstreamFile, DNSForwarder or DNSUnavailable — at the namespace tier
	// only.
	DNS       string   `json:"dns,omitempty"`
	Workloads []Status `json:"workloads"`
}

// VerifyRequest asks whether a token is a live workload's identity.
type VerifyRequest struct {
	Token string `json:"token"`
}

// VerifyResponse is who the token belongs to, as the agent recorded it when it
// started the process — never anything the caller said.
type VerifyResponse struct {
	AppID    string `json:"app_id"`
	Workload string `json:"workload"`
	Kind     string `json:"kind"`
}

// Event types on a follow stream.
const (
	EventLine  = "line"
	EventStart = "start"
	EventExit  = "exit"
	// EventDropped says a follower too slow to keep up lost Dropped events
	// before this one.
	EventDropped = "dropped"
)

// Event is one thing that happened to a workload, as a follow stream reports
// it: a line of its output, a start, an exit.
type Event struct {
	Type     string            `json:"type"`
	Workload string            `json:"workload"`
	AppID    string            `json:"app_id"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Line is a line exactly as the log file holds it: marked by the runner
	// (runner.LogMarker), or by the agent for one the runner did not mark.
	Line string `json:"line,omitempty"`
	// ExitCode and ExitSignal, on an exit; ExitCode nil when not known.
	ExitCode   *int   `json:"exit_code,omitempty"`
	ExitSignal string `json:"exit_signal,omitempty"`
	Dropped    int    `json:"dropped,omitempty"`
}

// errorBody is every non-2xx answer.
type errorBody struct {
	Error string `json:"error"`
	// Protocol is the agent's own, so a mismatch names both sides.
	Protocol string `json:"protocol,omitempty"`
}

// Stats is the agent's own figures, for a reader watching it over time: what a
// leak in the agent itself would show first is descriptors, memory or
// goroutines climbing while the workloads it holds stay level.
type Stats struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Protocol  string    `json:"protocol"`
	// FDs is the agent's open descriptors; OpenLogs how many workload logs it
	// holds open — one per workload that can still write, none for a run that
	// has ended.
	FDs      int `json:"fds"`
	OpenLogs int `json:"open_logs"`
	// FDLimit is the agent's soft RLIMIT_NOFILE, the ceiling FDs climbs
	// towards; EndedLogsOpen the runs that have exited and still hold their
	// log, which the agent never does on purpose — so any is a leak.
	FDLimit       int `json:"fd_limit,omitempty"`
	EndedLogsOpen int `json:"ended_logs_open"`
	RSSKB         int `json:"rss_kb"`
	Goroutines    int `json:"goroutines"`
	// Workloads counts what the agent holds by "kind/state" — "service/online",
	// "run/exited" — and Total all of them.
	Workloads map[string]int `json:"workloads"`
	Total     int            `json:"total"`
	// LogBytes is the agent's own log's size, before its next rotation.
	LogBytes int64 `json:"log_bytes"`
}

// WorkloadPort is the port a workload listens on inside its namespace. The
// address is the workload's own, so the port is the same for every one.
const WorkloadPort = 8000
