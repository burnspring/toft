package runtime

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// What the shared backend's pieces have in common, kept when PM2 went: the
// port allocator, the repair and narration seams, the privilege drop a build
// runs under, and the small readers the agent's backend shares.

// listCacheTTL bounds how stale a shared backend's process table may be.
// Dashboard views ask for it once per app per render.
const listCacheTTL = 3 * time.Second

// cpuSample is one workload's previous reading. The pid is part of it because a
// counter is only comparable with itself: a workload that restarted between two
// samples has a new pid and a cumulative CPU time that started again at zero,
// and subtracting across that boundary would report a large negative or, worse,
// a plausible small number about nothing.
//
// rate is the last percentage derived for this pid, handed back to a reader
// that arrives with the same counter reading or within minSampleGap of it;
// nil until two readings far enough apart have been seen.
type cpuSample struct {
	pid        int
	cpuSeconds float64
	at         time.Time
	rate       *float64
}

// lastRate is a copy of the last derived rate, or nil if there is none: each
// caller gets its own pointer, so no reader can alter what another reports.
func (s cpuSample) lastRate() *float64 {
	if s.rate == nil {
		return nil
	}
	return Measured(*s.rate)
}

// PortAllocator is the slice of internal/ports the shared backend needs. It is
// an interface so tests can pin a port without standing up a real allocator.
type PortAllocator interface {
	Allocate() (int, error)
	Release(port int)
}

// Repairer rebuilds a release's dependency tree in place.
//
// It exists because a release is not portable across an interpreter change. A
// venv records the absolute path of the interpreter it was built against, so
// moving the platform's Python leaves every already-built Python release with a
// `venv/bin/python` that dangles — and the release itself is unchanged and comes
// back byte-for-byte on the next start, so the stale venv returns on every
// restart, wake and node move until something rebuilds it.
//
// The isolated side has had this since the runner existed, because *every* tree
// it fetches was built on another machine. The shared side needed it for the
// narrower case that is the same case: the machine did not change, the
// interpreter on it did.
//
// A seam rather than a direct call because internal/builder imports this
// package. It is deliberately not the whole builder: what a repair needs is one
// verb over a directory this backend has already resolved, and a backend that
// could reach the build queue could start a deploy.
type Repairer interface {
	// Repair rebuilds the release at dir if it cannot run here, and does
	// nothing at all if it can. The conditional belongs to the implementation,
	// so a healthy tree costs two os.Stat calls and no log line.
	Repair(spec WorkloadSpec, dir string) error
}

// Narrator is where this backend says what it did, in the app owner's register.
//
// A seam rather than a direct call, for the reason Repairer is one but pointing
// the other way: the durable store lives in internal/database, which imports
// *this* package, so a backend that reached the store directly would be an
// import cycle. What a launcher needs is two verbs about a workload it has
// already resolved, and neither of them can fail — a workload may never fail to
// start because its narration could not be written down.
//
// Starting reopens the workload's narration budget. Say adds one line.
type Narrator interface {
	Starting(appID, workloadName string)
	Say(appID, workloadName, stream, line string)
}

// releaseLabel names a release for its owner, and never says where it is.
//
// `ReleaseRef.String()` is the operator's answer: an absolute directory under
// APPS_DIR for a local release, a store key for an artifact one. Both describe
// the *install's* layout rather than the app, and putting either in front of
// somebody with edit access on one app is exactly the leak this register
// exists to avoid. Found by reading what a running install had actually
// written, which is the only way this kind of thing is ever found.
//
// What survives the trim is the release identifier the Deployments tab already
// shows — or, for the legacy `current` symlink, no identifier at all, which is
// the honest answer rather than the word "current" dressed up as one.
func releaseLabel(ref ReleaseRef) string {
	base := path.Base(strings.TrimSuffix(strings.TrimSpace(ref.Value), "/"))
	base = strings.TrimSuffix(base, ".tar.zst")
	base = strings.TrimSuffix(base, ".tar.gz")
	switch base {
	case "", ".", "/", "current":
		return "the current release"
	}
	return "release " + base
}

// The stage in front of what the platform runs as an app's account from an
// argv rather than placing it: a build step (SpawnCommand, through
// buildIdentity.command). The drop itself is the clone's — uid, primary gid and
// the account's whole group list (AppAccount.Credentials) — as it is for a
// workload the agent places.
//
// **The whole group list, not setgid(2) plus setuid(2).** A drop that sets only
// those keeps the *caller's* supplementary groups and never gains the
// account's. The per-app group that gates `apps/<id>` is supplementary by
// design (ADR §3), so a process dropped that way is missing the one group that
// lets it traverse into the release it is working on — `venv/bin/python`
// cannot read its own `pyvenv.cfg`, `next` cannot resolve its own
// `node_modules`, and the failure names neither. PM2's `uid`/`gid` was exactly
// that drop, which is how this was learned.
//
// The stage is a shell that sets the umask and execs, so the command is the
// same process the caller waits on. The app itself gets the same umask from the
// runner (setAppUmask), because Go cannot set a child's umask and the runner is
// the child; this stage is the same rule for what runs as the account through
// SpawnCommand. **Gated storage needs a group-writable file.** Setgid on a gate
// (sharedstorage.go's gatedMode) gives a new file the gate's group, and 0007 is
// what lets the other apps on the gate edit it while every other account is
// shut out. Setting it on the platform itself would reach the platform's own
// children too.
//
// It costs nothing in the app's own tree: apps/<id> is 0710 to the per-app
// group (gateAppDir), and HOME and TMPDIR both live under it. What it does
// change is a file written to a literal /tmp, which is now group-writable to
// toft-apps — the primary group every app account shares — rather than
// world-readable. docs/invariants.md § App storage states that edge.
const (
	umaskShell  = "/bin/sh"
	umaskScript = `umask 0007; exec "$0" "$@"`
)

// spawnArgs is the stage's argument list up to the program it runs; the
// program and its arguments follow.
func spawnArgs() []string {
	return []string{umaskShell, "-c", umaskScript}
}

// lastLines is the newest n complete lines of a file, oldest first, read from
// its end so a 4 MiB file costs what n lines cost.
func lastLines(path string, n int) []string {
	if n <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	const block = 64 << 10
	var buf []byte
	end := info.Size()
	for end > 0 && bytes.Count(buf, []byte{'\n'}) <= n {
		start := end - block
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, end-start)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return nil
		}
		buf = append(chunk, buf...)
		end = start
	}
	text := strings.TrimRight(string(buf), "\n")
	if text == "" {
		return nil
	}
	all := strings.Split(text, "\n")
	if end > 0 && len(all) > 0 {
		all = all[1:] // the first piece may be the end of an earlier line
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// minSampleGap is the shortest interval a rate is derived over. Below it the
// quantisation of /proc's centisecond counters dominates the answer — two
// readings 200 ms apart differ by a handful of ticks, and the percentage that
// falls out of them is noise wearing a decimal point.
const minSampleGap = time.Second

// wipeScratch empties an app's scratch directory for a new start.
//
// **Moved aside, then removed, and the removal may fail.** A new release
// starts while the old process is still serving, and on NFS (EFS) a file the
// old process still holds open survives its unlink as `.nfs…`, which cannot be
// removed until it is closed — so an in-place RemoveAll failed with "device or
// resource busy" and the deploy with it, for any app that kept a scratch file
// open (an R process, measured). The rename always succeeds, the new process
// gets an empty directory, and whatever is still held goes at a later start.
func wipeScratch(scratch string) error {
	leftovers, _ := filepath.Glob(scratch + ".old-*")
	for _, dir := range leftovers {
		_ = os.RemoveAll(dir)
	}
	if _, err := os.Lstat(scratch); os.IsNotExist(err) {
		return nil
	}
	aside := fmt.Sprintf("%s.old-%d", scratch, time.Now().UnixNano())
	if err := os.Rename(scratch, aside); err != nil {
		return os.RemoveAll(scratch)
	}
	if err := os.RemoveAll(aside); err != nil {
		log.Printf("scratch: %s is still held open; it goes at a later start: %v", aside, err)
	}
	return nil
}

// OutputLine is one line a shared app printed, as the agent handed it over
// (AgentRuntime.StartOutput).
type OutputLine struct {
	AppID string
	// Workload is the release's process name, <slug>-<8 hex>.
	Workload string
	// DeploymentID is read off the release path, which only the platform
	// writes.
	DeploymentID string
	Stream       string // LogStreamStdout or LogStreamStderr
	// At is the runner's stamp, or nil when the line carried none.
	At   *time.Time
	Line string
	// RunID and Job name a host job run's line; empty for a service's.
	RunID, Job string
}

// AppOutput receives every line StartOutput is handed. It is declared here rather
// than imported for Narrator's reason: the sink lives in a package that would
// otherwise be an import cycle. Line must not block; a sink that cannot keep
// up drops and counts.
type AppOutput interface {
	Line(OutputLine)
}
