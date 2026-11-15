package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

// GoBinaryName is the output name of a Go app's compiled binary inside its
// release directory. The builder writes it (`go build -o`) and the runtime
// executes it; the toft- prefix keeps it clear of repo files.
const GoBinaryName = "toft-app"

// NotebookRenderDir is the directory inside a notebook release that holds the
// rendered document, and the document root of the workload that serves it.
//
// Named here as well as in internal/builder — which is where it is written, and
// which this package must not import — for the reason staticserve.ManifestFile
// is named twice: the launcher has to know the path and cannot learn it from
// the thing that produced it. `internal/builder`'s notebook_test.go walks both
// launchers and fails the build if either stops agreeing.
const NotebookRenderDir = "toft-render"

// ErrNotSupported means a backend cannot answer this question at all, as
// distinct from failing to. Whole-machine metrics are the case that matters:
// host CPU and disk describe the box a shared workload sits on, and mean
// nothing to an isolated one that has a box to itself. A caller must render
// "not available" rather than a zeroed struct, which would read as an idle
// machine.
var ErrNotSupported = errors.New("not supported by this backend")

// ErrAppLocked and ErrStartSuperseded are the two refusals a start can answer
// that are not failures: a lock withholds starting, and a deploy that flipped
// first owns the status. They are defined here, at the seam both the builder
// (which returns them) and the supervisor (which must not read them as a crash)
// already import; internal/builder re-exports them under its own names.
var (
	ErrAppLocked       = errors.New("this app is locked by an administrator and will not be started")
	ErrStartSuperseded = errors.New("the app was deployed since this start was asked for; its new release is the one running")
)

// AppMetrics is what a backend can say about one running workload. Not every
// backend can answer every field, which is why two of them are pointers:
// ErrNotSupported says a backend cannot answer a whole call, and nil is the
// same sentence at the field level. A zero would not be a refusal, it would be
// a claim — 0.0% CPU reads as an idle machine, and reads that way hardest in
// the two places it is shown, a green cell and the bottom of a busiest-first
// sort.
type AppMetrics struct {
	// CPU is percent, and Memory is resident MB. Both are nil from a backend
	// that does not measure them: the isolated backend reports a pod's status,
	// restarts and uptime and nothing else. A caller renders absence, never a
	// figure.
	CPU    *float64 `json:"cpu,omitempty"`
	Memory *float64 `json:"memory,omitempty"`
	// Uptime is elapsed seconds since the process started.
	Uptime   int64 `json:"uptime"`
	Restarts int   `json:"restarts"`
	// ExitCode is how the workload's most recent process ended, when the
	// backend kept it: the agent's pidfd reap, the kubelet's last termination. Nil
	// when the backend does not say, which is not 0 — "it exited cleanly" is a
	// claim and absence is not one. Read beside a Restarts that moved; on its
	// own it may describe an exit from long ago.
	ExitCode *int `json:"exit_code,omitempty"`
	// ExitSignal names the signal that ended that process ("SIGSEGV"), when
	// the backend knows one; ExitCode is nil beside it, because a process a
	// signal ended returned no code. The agent reads both off the reap, and
	// a process it adopted rather than started reports neither.
	ExitSignal string `json:"exit_signal,omitempty"`
	// CreatedAt is when the backend created this workload — when the agent
	// was first given the name, which survives its restarts; the pod's creation
	// time — and zero when it does not say. Not exported to clients: it is what
	// lets the health watcher tell a workload born while it was watching (every
	// restart of which is news) from one that predates it (whose lifetime
	// count is not).
	CreatedAt time.Time `json:"-"`
	Status    string    `json:"status"`
	PID       int       `json:"pid"`
	// MemoryLimitMB is the cap the backend actually applied to this workload,
	// which is not a measurement and so is not a pointer: 0 means no cap is in
	// force, and that is a fact rather than an absence.
	//
	// Reported because Memory on its own cannot explain a restart. A workload
	// killed for crossing its cap leaves nothing behind — it is sent a signal
	// and R prints "Execution halted" with no error above it — so the only
	// place the reason exists is the comparison between these two numbers, and
	// only a backend knows the second.
	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`
	// OOMKilled says the most recent termination of this workload was the
	// kernel's out-of-memory killer, and it is a backend's certain answer
	// rather than an inference — so a backend that cannot know leaves it
	// false and MemoryLimitMB above is the only evidence there is.
	//
	// Both backends can know. A container killed for its limit is terminated
	// with reason `OOMKilled`, and the kubelet keeps that on the container's
	// *last* state across the restart it then performs. The agent holds the
	// limit itself — the kernel's cgroup `memory.max` where it has a cgroup,
	// its own watchdog where it does not — and says so when either killed;
	// the 90%-of-cap reading in `internal/health` covers a kill no backend
	// attributed. Read together with a restart count that moved, never on its
	// own — a workload that was OOM-killed an hour ago still carries this.
	OOMKilled bool `json:"oom_killed,omitempty"`
	// ExitPeakMemoryMB is the most the most recently *ended* process held, 0
	// where the backend cannot say — the shared agent can, a pod's last state
	// cannot. It is what a memory kill is reported with: Memory is the
	// restarted process's reading, and the sweep before the kill can have
	// seen the app long before it grew into its cap.
	ExitPeakMemoryMB int `json:"-"`
	// CrashLooping is the *backend's own* verdict that this workload keeps
	// starting and dying, rather than an inference from a restart count.
	//
	// It exists because counting does not work against a backend that backs
	// off. The kubelet waits 10s, 20s, 40s, and on to five minutes, so a
	// genuinely broken isolated app produces perhaps three restarts in its
	// first minute and then one per five, never reaching a per-sweep threshold:
	// the loop was visible as `errored` and *nothing counted it*, so the event
	// that says "this app keeps restarting" could not be raised at all. The
	// agent backs off on the same numbers after one immediate restart.
	//
	// Both backends have already made the judgement — `CrashLoopBackOff` is a
	// container the kubelet is deliberately waiting to restart, and the agent
	// reports the same while it is backing off and the workload has not stayed
	// up for ten minutes — so this reports that judgement rather than
	// re-deriving it from a count. False from a backend with no such concept,
	// which is not an absence: the count is the evidence there.
	CrashLooping bool `json:"crash_looping,omitempty"`
	// Detail is the backend's own account of why this workload is not running,
	// in the backend's words, and empty from one that has none.
	//
	// It exists because the most useful sentence about a workload that never
	// started is one only the backend can see. An unschedulable pod carries a
	// precise, actionable reason on its own conditions — *persistentvolumeclaim
	// "lpv-x" is being deleted*, *insufficient memory*, a taint nothing
	// tolerates — while the platform's own view is a workload that is simply
	// not up yet. Twelve minutes of that read as `running` with nothing wrong,
	// and reaching the reason took `kubectl describe pod`, which an app's owner
	// does not have and should not need.
	//
	// Narration, never a decision: nothing branches on this string. What it is
	// for is the event and the log line an owner reads.
	Detail string `json:"detail,omitempty"`
	// Wrapped says PID is the runner the workload's app runs under, not the
	// app: a reader of PID's tree takes the runner off it. Not exported.
	Wrapped bool `json:"-"`
	// SelfReported says the workload's runner reports its own consumption
	// from a control group of its own, so an outside sampler must not bill
	// it a second time. Not exported.
	SelfReported bool `json:"-"`
}

// Measured wraps a figure a backend actually took, for the two AppMetrics
// fields where nil means nobody measured it.
func Measured(v float64) *float64 { return &v }

// The vocabulary of AppMetrics.Status: a closed set of four words both
// backends answer in, each backend's own word rather than its manager's — a
// run's `exited` reaches a service reader as `stopped`, and a pod's phase is
// translated before it leaves the backend. Stated once here because the seam
// owns the contract; `internal/health`, `internal/builder` and the process
// list all read it, and three private copies of two string literals is how one
// of them would come to mean something slightly different.
const (
	// StatusOnline — a process exists and the backend is running it.
	StatusOnline = "online"
	// StatusLaunching — on its way up, and indeterminate: neither up nor down.
	// It counts as up wherever the question is "does the backend own this
	// already", because starting a second workload on top of one still being
	// brought up is the duplicate that question exists to avoid.
	StatusLaunching = "launching"
	// StatusStopped — the backend knows the name and no process is behind it.
	// Shared mode only in practice: the agent keeps a stopped workload's entry
	// and the backend its port, so that a later start lands on the same port,
	// while the isolated backend deletes the pod
	// and answers absence instead. Both are the same fact — nothing is running
	// — which is why a reader of this vocabulary must treat a `stopped` entry
	// and a missing one alike, and why the process list does.
	StatusStopped = "stopped"
	// StatusErrored — the backend tried and there is no process to show for it.
	StatusErrored = "errored"
)

// WorkloadIsUp reports whether a status word means a workload the backend is
// running. The inverse is not "crashed": `stopped` and `errored` are both down
// and mean opposite things about whether anybody wanted it that way.
func WorkloadIsUp(status string) bool {
	return status == StatusOnline || status == StatusLaunching
}

type SystemMetrics struct {
	CPUPercent float64 `json:"cpu_percent"`
	// CPUCount is the cores this machine offers the platform, so a percentage
	// and a CPU limit have something to be read against.
	CPUCount      int     `json:"cpu_count"`
	MemoryTotal   uint64  `json:"memory_total"`
	MemoryUsed    uint64  `json:"memory_used"`
	MemoryPercent float64 `json:"memory_percent"`
	DiskTotal     uint64  `json:"disk_total"`
	DiskUsed      uint64  `json:"disk_used"`
	DiskPercent   float64 `json:"disk_percent"`
}

// ServiceRuntime is the lifecycle of a long-lived workload: something with an
// endpoint, which is restarted when it dies. One-shot work — something with an
// exit code, which is not restarted — is a separate interface over the same
// WorkloadSpec, so that neither grows "not applicable here" branches.
//
// Every method identifies a workload by its process name (WorkloadSpec.Name),
// which is per-release and stable for that release's life.
type ServiceRuntime interface {
	// Deploy makes the described workload be what is running under that name,
	// at the address that name already holds, and returns the endpoint.
	// Whatever was running under the name before is replaced.
	//
	// **It is the only way a workload is started**, and there is deliberately
	// no Restart beside it. A process name is not enough information to start
	// anything correctly, and the two backends proved it in opposite
	// directions: the isolated one cannot rebuild a pod spec at all, since
	// every part of it — the release reference, the framework, the entrypoint,
	// the mounts — lives in the platform's database and not in a name, so its
	// restart deleted the pod, returned nil and started nothing
	// — while the shared one appeared to work and replayed the environment its
	// process manager held from process creation, bringing the old
	// configuration back wearing
	// the new one's name. The whole description is the platform's to supply on
	// every start, which is what makes a start carry the configuration as it
	// is *now* rather than as it was when the workload was created.
	//
	// Replacement also removes the reason the old signature took an app id:
	// scratch is wiped on every start by both backends already, the shared
	// one's by construction here and the pod's by getting a fresh emptyDir.
	//
	// The endpoint is the backend's answer, not the caller's assumption:
	// callers persist it and address the app through it forever after.
	Deploy(spec WorkloadSpec) (Endpoint, error)
	Stop(name string) error
	// Delete removes the workload and releases whatever the backend reserved
	// for it — for the shared backend, its port.
	Delete(name string) error
	// Logs is the tail, newest last, already labeled. A backend parses its own
	// format here and nowhere else — the runner's marked lines, which both
	// backends now carry (the agent's file and the pod's stream) and
	// runner.ParseLogLine reads. It is not a wire format anything above the
	// seam should have to know.
	Logs(name string, lines int) ([]LogLine, error)
	// LogStream is the same lines, live.
	LogStream(name string) (LogReader, error)
	Metrics(name string) (*AppMetrics, error)
	// SystemMetrics describes the machine the workloads run on, and may return
	// ErrNotSupported where that is not a meaningful thing to ask.
	SystemMetrics() (*SystemMetrics, error)
	List() (map[string]*AppMetrics, error)
}

// Orphan is a workload whose app is gone: the row that named it has been
// deleted, so nothing will ever ask after it again.
//
// It is not a hypothetical. A delete stops and deletes the workload on the way
// past (handleDeleteApp), and both calls are best-effort by design — a backend
// that has gone missing must not block an administrator getting rid of a row.
// The gap that leaves is real: anything that recreates a pod between the stop
// and the row's deletion outlives the app, and the health watcher will never
// visit it again because the watcher iterates *apps*. Nothing then holds that
// workload's memory reservation to account, which on a single-node cluster is
// how five demo apps and one dead fixture fill a node.
type Orphan struct {
	// Workload is the name [ServiceRuntime.Delete] takes.
	Workload string
	// AppID is the app the backend recorded when it started the workload.
	AppID string
	// Age is how long it has been up. A workload younger than the caller's
	// grace is never an orphan, however absent its app looks.
	Age time.Duration
	// Superseded is a workload of a live app that is not its current release:
	// a retired or half-started release whose delete failed. Otherwise the
	// app is gone.
	Superseded bool
}

// OrphanFinder is a backend that records which app each of its workloads
// belongs to, and can therefore be asked which of them have outlived it.
//
// Optional, and deliberately not folded into [ServiceRuntime]: `List` answers
// names and metrics, which is exactly enough to decide whether a *known* app's
// workload is up and exactly not enough to notice a workload no app claims. A
// backend that cannot name the app behind a workload must not guess — there is
// no safe way to recover an app id from a process name, and the cost of being
// wrong is deleting something that is serving.
//
// The backend does its own matching against `live` rather than handing back
// ids to be compared here, because how an app id is written into a workload's
// metadata is the backend's business: the isolated one sanitises it into a
// label value, and a comparison made anywhere else would have to know that.
//
// `live` maps every existing app id to its current release — the workload name
// that is serving it — or to "" when the caller cannot say for certain (no
// release yet, or a deploy in flight that may be about to flip). A backend
// returns a live app's workload only when its current release is named, the
// workload is not it, it is no job, and it is older than the grace; "" means
// none of that app's workloads is judged.
type OrphanFinder interface {
	Orphans(live map[string]string, grace time.Duration) ([]Orphan, error)
}

// Log line vocabularies. Closed sets, because a client renders copy and colour
// from them: a backend inventing a word renders as nothing.
const (
	// LogSourceApp is the workload's own output. Everything a shared workload
	// prints is this, because there is no runner in shared mode to say
	// anything else — and so is an isolated static app's, which is Toft's
	// own serve-static code but runs under both launchers and must be labeled
	// the same by both.
	LogSourceApp = "app"
	// LogSourcePlatform is Toft speaking — the runner's stage marks, its
	// version checks, the reason a start failed. An app may never produce one:
	// an app that could would be able to make the platform appear to blame
	// itself for the app's own failure.
	LogSourcePlatform = "platform"

	LogStreamStdout = "stdout"
	LogStreamStderr = "stderr"

	// LogMarkerWorkloadStart is the boundary between the platform's stored
	// record of this release and the workload's own live output. The only
	// marker there is; a second one joins this list rather than growing a
	// second mechanism.
	LogMarkerWorkloadStart = "workload_start"
)

// LogLine is one line of a workload's output with the three facts a blob
// cannot carry: who said it, which stream it came out of, and when.
//
// The alternative was a string, and it is what the product had: the browser
// held a regex for PM2's dump format and there was exactly one such regex, so
// the Logs tab could serve exactly one backend.
type LogLine struct {
	// At is when the line was stamped, and nil when nothing stamped it — an
	// unmarked line from an older runner, for one, which runner.ParseLogLine
	// reads as the app's stdout with no time. A zero time rendered as a date
	// would be a claim rather than an absence — the same reason AppMetrics
	// leaves CPU nil rather than reporting an idle machine.
	At *time.Time `json:"at,omitempty"`
	// Source is LogSourceApp or LogSourcePlatform.
	Source string `json:"source"`
	// Stream is LogStreamStdout or LogStreamStderr.
	Stream string `json:"stream"`
	// Line is the text, with the backend's own framing already removed.
	Line string `json:"line"`
	// Marker names a boundary the platform drew rather than a line anything
	// printed. Empty on every real line, which is all but one of them.
	//
	// **A boundary is structure, not prose.** The one marker there is — the
	// point where the platform's record stops and the workload's own output
	// starts — used to travel as a box-drawing sentence in Line with no
	// timestamp, inside a stream whose whole purpose is to be machine-readable.
	// A reader sorting by At put it at the epoch, and a reader filtering the
	// platform's narration out could not tell it from narration. The sentence
	// is still there for anyone printing lines verbatim; what changed is that
	// nothing has to parse it to know what it is.
	Marker string `json:"marker,omitempty"`
}

// AppLine is a line of the workload's own output on stdout, which is what a
// backend with nothing more to say produces.
func AppLine(text string) LogLine {
	return LogLine{Source: LogSourceApp, Stream: LogStreamStdout, Line: text}
}

// LogReader is a live log as labeled lines. Next returns io.EOF when the stream
// ends, which for a follow is when the workload or the connection does.
//
// An interface rather than an io.ReadCloser because the parse belongs to the
// backend: handing the caller bytes is handing it a format to guess at.
type LogReader interface {
	Next() (LogLine, error)
	Close() error
}

// JoinLines renders labeled lines as the flat blob the API still answers with
// and the CLI still reads.
//
// One parse, one join: producing the two independently is how they drift, and
// the drift would be invisible — the CLI and the browser would each be right
// about a different log.
func JoinLines(lines []LogLine) string {
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(l.Line)
	}
	return sb.String()
}

// WorkloadIdentity is who a workload proved itself to be, as the platform
// established it rather than as the workload asserted it.
//
// Every field comes from metadata the platform wrote on a pod the cluster has
// just confirmed the caller is — never from a claim the workload carried. That
// is the difference between verifying an identity and reading one.
type WorkloadIdentity struct {
	AppID string
	// WorkloadName is the release-scoped name the backend gave this workload,
	// so a report is attributable to one instance of one release.
	WorkloadName string
	// RunID is set for a job's pod and empty for a service's.
	RunID string
}

// IdentityVerifier is implemented by a backend whose *infrastructure* issues
// workload credentials, so the platform verifies rather than mints.
//
// Optional, and absent means absent: a backend that does not implement this has
// no workloads calling home, and the bootstrap route has nothing to answer.
type IdentityVerifier interface {
	VerifyWorkloadIdentity(ctx context.Context, token string) (WorkloadIdentity, error)
}

// Log absence — why there is nothing to show, as distinct from a backend that
// failed to answer.
//
// The two were once one error, and that was a defect rather than a
// simplification: asked for the logs of a workload that was not running, the
// shared backend of the day answered success with an empty tail (`pm2 logs` on
// an unknown name exited 0) and the isolated one an error the API turned into a
// 500. The browser rendered both as "No log output yet." because it read
// neither, so the disagreement was invisible for as long as both backends had
// existed.
//
// A workload that is not there is a **200 with a reason**. Only a backend that
// could not be reached is a 500.
const (
	// LogAbsentNeverDeployed — the app has no release, so no workload has ever
	// carried its name.
	LogAbsentNeverDeployed = "never_deployed"
	// LogAbsentStopped — a person stopped it and it stays down until a person
	// says otherwise.
	LogAbsentStopped = "stopped"
	// LogAbsentSleeping — the idle reaper stopped it and the next request wakes
	// it. Shared mode only, and never collapsed into stopped: `sleeping` is not
	// `stopped`.
	LogAbsentSleeping = "sleeping"
	// LogAbsentLocked — an administrator took the app away. Locking stops the
	// workload, so under a pull the log stops existing at the same moment.
	LogAbsentLocked = "locked"
	// LogAbsentLaunching — the workload is on its way up (an image pull, a
	// container being created) and has not printed anything yet.
	LogAbsentLaunching = "launching"
	// LogAbsentNoWorkload — the name resolves to nothing the backend holds, for
	// a reason the backend cannot narrow further.
	LogAbsentNoWorkload = "no_workload"
)

// AbsentError is a backend saying the workload is not there to be asked. It
// carries one of the LogAbsent* reasons, which is a closed set: callers render
// copy from it, so a backend inventing a word would render as nothing.
//
// It is an error rather than a value because it travels the existing error
// return of a seam method, and because every caller that does not know about it
// keeps treating absence as a failure — which is the safe direction for a
// mistake to fall.
type AbsentError struct{ Reason string }

func (e *AbsentError) Error() string { return "workload absent: " + e.Reason }

// Absent builds an AbsentError for one of the LogAbsent* reasons.
func Absent(reason string) error { return &AbsentError{Reason: reason} }

// AbsenceReason returns the reason carried by an AbsentError anywhere in err's
// chain, and "" if err is an ordinary failure. One reader, so that "is this an
// absence?" and "which absence?" are never asked separately and answered
// differently.
func AbsenceReason(err error) string {
	var absent *AbsentError
	if errors.As(err, &absent) {
		return absent.Reason
	}
	return ""
}
