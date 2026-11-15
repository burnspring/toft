package procstat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// maxTreeProcesses bounds one walk. A workload that has forked more than this
// is measured on its first thousand processes rather than made to cost an
// unbounded number of /proc reads on the sampling goroutine — the same
// judgement that keeps `Read` off `ps`.
const maxTreeProcesses = 1024

// Children reports the direct children of a process.
//
// **Every thread's children, not the main thread's.** /proc/[pid]/task/[tid]/
// children is per *thread*, and a process that spawns from a worker thread —
// which a Node server with a libuv pool may well do — would otherwise have a
// child that no walk from the main thread can see. The task directory is
// listed and each thread's children file is read.
//
// A kernel built without CONFIG_PROC_CHILDREN has no such file. That is not an
// error worth propagating: the caller gets no children, the tree is the root
// alone, and the reading degrades to exactly what it was before trees existed.
func Children(pid int) []int {
	taskDir := fmt.Sprintf("/proc/%d/task", pid)
	tids, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}

	var out []int
	for _, tid := range tids {
		data, err := os.ReadFile(filepath.Join(taskDir, tid.Name(), "children"))
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if child, err := strconv.Atoi(f); err == nil {
				out = append(out, child)
			}
		}
	}
	return out
}

// TreeSampler reads a workload's whole process tree as one reading, and is
// **stateful because a tree's CPU is not otherwise monotonic**.
//
// # Why the state is not optional
//
// A single process's cumulative CPU only ever rises, so a caller can difference
// two readings and get usage. The sum over a tree cannot: when a child exits,
// its CPU leaves the sum, and the total *falls*. Both callers read that fall as
// a counter reset — `resource.window.fold` credits the whole new counter and
// flags a gap, `runtime.AgentRuntime.reading` throws the sample away — so an app
// that periodically shells out would be billed for its own lifetime again on
// every subprocess exit. Carrying the CPU of departed processes forward is what
// makes the tree behave like the single counter the callers already handle.
//
// It also removes a bias rather than merely a bug: a short-lived child that
// lived and died between two samples used to contribute nothing to anybody's
// reading. Its CPU is real and is now counted.
//
// # What a reading is
//
// CPU is the sum over the tree plus everything that has left it, and is exact.
// **Memory is the sum of RSS and overstates**: pages shared between a parent
// and its children — the interpreter's text, its shared libraries — are counted
// once per process. Measured on a `npm run start` tree that is about 40 MB
// against a 695 MB total, and the share falls as the workload grows. The
// accurate figure is PSS from /proc/[pid]/smaps_rollup, which costs the kernel
// a page-table walk per process per sample; that is the trade to revisit if the
// overstatement ever matters, and it is the wrong default for a sampler that
// runs every fifteen seconds against every workload on the box.
//
// A sampler is safe for concurrent use.
type TreeSampler struct {
	mu    sync.Mutex
	trees map[string]*tree

	// ReadStat and ReadChildren are the seams the walk is tested through: a
	// process cannot be made to fork or exit on cue. Both default to this
	// package's real readers.
	ReadStat     func(pid int) (Stat, error)
	ReadChildren func(pid int) []int
}

// tree is one workload's accumulated state between samples.
type tree struct {
	// root is the pid the caller named. A different one is a different run of
	// the workload, and resets everything below.
	root int
	// live maps each process seen last time to the CPU it had then, which is
	// what lets a departure be noticed and its CPU kept.
	live map[int]float64
	// departed is the CPU of every process that has left this tree.
	departed float64
	// last is the previous total handed out. A reading is never allowed below
	// it — see Sample.
	last float64
}

func NewTreeSampler() *TreeSampler {
	return &TreeSampler{
		trees:        make(map[string]*tree),
		ReadStat:     Read,
		ReadChildren: Children,
	}
}

// Sample reads the tree rooted at rootPID as one process.
//
// key is the workload's stable identity across restarts — the caller's name for
// it, not its pid — so that a redeploy under the same name is told apart from a
// new workload by rootPID rather than by the map.
//
// An unreadable *root* is an error, exactly as `Read` on a dead pid is: the
// workload is gone and there is no reading. An unreadable descendant is
// skipped, because a child exiting mid-walk is the normal case and not a
// failure to report.
func (t *TreeSampler) Sample(key string, rootPID int) (Stat, error) {
	if rootPID <= 0 {
		return Stat{}, fmt.Errorf("procstat: invalid root pid %d", rootPID)
	}

	rootStat, err := t.ReadStat(rootPID)
	if err != nil {
		return Stat{}, err
	}

	seen := map[int]float64{rootPID: rootStat.CPUSeconds}
	cpu := rootStat.CPUSeconds
	rss := rootStat.RSSBytes

	// Breadth-first over the children files, iteratively: a walk driven by a
	// file the kernel writes is not one to recurse on.
	queue := t.ReadChildren(rootPID)
	for len(queue) > 0 && len(seen) < maxTreeProcesses {
		pid := queue[0]
		queue = queue[1:]
		if _, dup := seen[pid]; dup || pid <= 0 {
			continue
		}
		stat, err := t.ReadStat(pid)
		if err != nil {
			// Gone between being listed and being read. Its CPU up to the
			// previous sample is already carried below.
			continue
		}
		seen[pid] = stat.CPUSeconds
		cpu += stat.CPUSeconds
		rss += stat.RSSBytes
		queue = append(queue, t.ReadChildren(pid)...)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	st, ok := t.trees[key]
	if !ok || st.root != rootPID {
		// A workload this sampler has not seen, or a new run of one it has.
		// Its counters start from this reading; nothing is carried across a
		// restart, because the kernel's counters did not carry either.
		t.trees[key] = &tree{root: rootPID, live: seen, last: cpu}
		return Stat{CPUSeconds: cpu, RSSBytes: rss}, nil
	}

	for pid, was := range st.live {
		if _, still := seen[pid]; !still {
			st.departed += was
		}
	}
	st.live = seen
	total := cpu + st.departed

	// **The floor, and it is deliberately belt and braces.** The carry above
	// makes a fall impossible in every case it models; pid reuse inside one
	// tree between two samples is the case it does not, and there the total
	// could dip by one child's CPU. A reading that has gone backwards is worth
	// more as a repeated one than as the counter reset both callers would
	// otherwise infer from it.
	if total < st.last {
		total = st.last
	}
	st.last = total

	return Stat{CPUSeconds: total, RSSBytes: rss}, nil
}

// Forget drops a workload's accumulated state.
//
// The same rule `AgentRuntime.forgetSample` follows and for the same reason: a
// name whose workload is gone would otherwise hold its departed CPU for the
// platform's life, and the next workload to take that name would inherit it.
func (t *TreeSampler) Forget(key string) {
	t.mu.Lock()
	delete(t.trees, key)
	t.mu.Unlock()
}

// Retain drops every workload except the ones named, and is what a caller that
// already enumerates the live set uses instead of tracking departures itself.
func (t *TreeSampler) Retain(keys []string) {
	live := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		live[k] = struct{}{}
	}
	t.mu.Lock()
	for k := range t.trees {
		if _, ok := live[k]; !ok {
			delete(t.trees, k)
		}
	}
	t.mu.Unlock()
}

// Tracked reports the keys this sampler holds state for, sorted. It exists for
// the callers' pruning tests: Forget and Retain are the kind of rule that passes
// by doing nothing at all, and the map they failed to prune is invisible from
// every other method.
func (t *TreeSampler) Tracked() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.trees))
	for k := range t.trees {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
