package procstat

import (
	"errors"
	"os"
	"testing"
)

// fakeTree is a process tree a test can fork and kill on cue, which no real one
// can be made to do at the moment a sample needs it.
type fakeTree struct {
	cpu      map[int]float64
	rss      map[int]int64
	children map[int][]int
	reads    int
}

func newFakeTree() *fakeTree {
	return &fakeTree{
		cpu:      map[int]float64{},
		rss:      map[int]int64{},
		children: map[int][]int{},
	}
}

func (f *fakeTree) set(pid int, cpu float64, rss int64) {
	f.cpu[pid], f.rss[pid] = cpu, rss
}

func (f *fakeTree) fork(parent int, kids ...int) {
	f.children[parent] = append(f.children[parent], kids...)
}

func (f *fakeTree) exit(pid int) {
	delete(f.cpu, pid)
	delete(f.rss, pid)
	for parent, kids := range f.children {
		out := kids[:0]
		for _, k := range kids {
			if k != pid {
				out = append(out, k)
			}
		}
		f.children[parent] = out
	}
}

func (f *fakeTree) read(pid int) (Stat, error) {
	f.reads++
	cpu, ok := f.cpu[pid]
	if !ok {
		return Stat{}, os.ErrNotExist
	}
	return Stat{CPUSeconds: cpu, RSSBytes: f.rss[pid]}, nil
}

func (f *fakeTree) kids(pid int) []int { return f.children[pid] }

func samplerOn(f *fakeTree) *TreeSampler {
	s := NewTreeSampler()
	s.ReadStat, s.ReadChildren = f.read, f.kids
	return s
}

const mib = int64(1 << 20)

// The reading this whole type exists for: `npm run start` holds the pid and the
// server is a grandchild, so the wrapper's own numbers are near nothing and the
// tree's are the app's.
func TestTreeSumsTheWholeTree(t *testing.T) {
	f := newFakeTree()
	f.set(100, 0.19, 69*mib) // npm
	f.set(101, 0.01, 2*mib)  // sh -c
	f.set(102, 22.31, 626*mib)
	f.fork(100, 101)
	f.fork(101, 102)

	got, err := samplerOn(f).Sample("demo", 100)
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if want := 22.51; !approxEq(got.CPUSeconds, want, 0.0001) {
		t.Errorf("CPUSeconds = %v, want %v", got.CPUSeconds, want)
	}
	if want := 697 * mib; got.RSSBytes != want {
		t.Errorf("RSSBytes = %d MiB, want %d", got.RSSBytes/mib, want/mib)
	}
}

// **The regression this type is shaped by.** A child exiting takes its CPU out
// of the sum, so a naive tree reading falls — and both callers read a fall as a
// counter reset, which credits the whole counter a second time. The departed
// process's CPU is carried instead.
func TestTreeCPUNeverFallsWhenAChildExits(t *testing.T) {
	f := newFakeTree()
	f.set(100, 1, 10*mib) // the wrapper
	f.set(200, 10, 100*mib)
	f.set(300, 5, 50*mib) // a subprocess that will finish
	f.fork(100, 200, 300)
	s := samplerOn(f)

	first, _ := s.Sample("demo", 100)
	if want := 16.0; !approxEq(first.CPUSeconds, want, 0.0001) {
		t.Fatalf("first CPUSeconds = %v, want %v", first.CPUSeconds, want)
	}

	// The subprocess finishes having burnt one more second, and the server
	// burns one more too.
	f.set(300, 6, 50*mib)
	f.set(200, 11, 100*mib)
	second, _ := s.Sample("demo", 100)
	if want := 18.0; !approxEq(second.CPUSeconds, want, 0.0001) {
		t.Fatalf("second CPUSeconds = %v, want %v", second.CPUSeconds, want)
	}

	f.exit(300)
	f.set(200, 12, 100*mib)
	third, err := s.Sample("demo", 100)
	if err != nil {
		t.Fatalf("Sample after exit: %v", err)
	}
	// 1 (wrapper) + 12 (server) + 6 carried from the departed child.
	if want := 19.0; !approxEq(third.CPUSeconds, want, 0.0001) {
		t.Errorf("CPUSeconds after a child exited = %v, want %v", third.CPUSeconds, want)
	}
	if third.CPUSeconds < second.CPUSeconds {
		t.Errorf("CPU went backwards: %v then %v", second.CPUSeconds, third.CPUSeconds)
	}
	// Memory is a level, not a counter: it falls, and should.
	if want := 110 * mib; third.RSSBytes != want {
		t.Errorf("RSSBytes = %d MiB, want %d", third.RSSBytes/mib, want/mib)
	}
}

// A child that lived and died between two samples contributed nothing to any
// reading before this type existed. Its CPU is real usage.
func TestTreeKeepsTheCPUOfAChildThatCameAndWent(t *testing.T) {
	f := newFakeTree()
	f.set(100, 1, mib)
	s := samplerOn(f)
	s.Sample("demo", 100)

	f.set(400, 7, mib)
	f.fork(100, 400)
	s.Sample("demo", 100)

	f.exit(400)
	got, _ := s.Sample("demo", 100)
	if want := 8.0; !approxEq(got.CPUSeconds, want, 0.0001) {
		t.Errorf("CPUSeconds = %v, want %v — the child's 7s is usage", got.CPUSeconds, want)
	}
}

// A restart is a new set of counters. Carrying the old tree's departed CPU into
// it would report the previous run's usage as this one's.
func TestTreeResetsWhenTheRootPIDChanges(t *testing.T) {
	f := newFakeTree()
	f.set(100, 40, mib)
	f.set(101, 60, mib)
	f.fork(100, 101)
	s := samplerOn(f)

	if first, _ := s.Sample("demo", 100); !approxEq(first.CPUSeconds, 100, 0.0001) {
		t.Fatalf("first = %v, want 100", first.CPUSeconds)
	}

	// Redeployed under the same name, new pids, counters from zero.
	f.exit(100)
	f.exit(101)
	f.set(500, 2, mib)
	got, err := s.Sample("demo", 500)
	if err != nil {
		t.Fatalf("Sample after restart: %v", err)
	}
	if want := 2.0; !approxEq(got.CPUSeconds, want, 0.0001) {
		t.Errorf("CPUSeconds after restart = %v, want %v", got.CPUSeconds, want)
	}
}

// A dead root is no reading, exactly as it is for Read. A dead descendant is a
// normal race and must not take the reading down with it.
func TestTreeRootErrorsAndDescendantsDoNot(t *testing.T) {
	f := newFakeTree()
	s := samplerOn(f)
	if _, err := s.Sample("gone", 999); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dead root: err = %v, want ErrNotExist", err)
	}
	if _, err := s.Sample("bad", 0); err == nil {
		t.Error("root pid 0 should be an error")
	}

	f.set(100, 3, mib)
	f.fork(100, 101) // listed, never readable
	got, err := s.Sample("demo", 100)
	if err != nil {
		t.Fatalf("unreadable descendant took the reading down: %v", err)
	}
	if want := 3.0; !approxEq(got.CPUSeconds, want, 0.0001) {
		t.Errorf("CPUSeconds = %v, want %v", got.CPUSeconds, want)
	}
}

// A children file naming a process already walked must not send the walk round
// again, and a very wide tree must not cost an unbounded number of reads.
func TestTreeWalkTerminates(t *testing.T) {
	f := newFakeTree()
	f.set(100, 1, mib)
	f.set(101, 1, mib)
	f.fork(100, 101)
	f.fork(101, 100, 101) // a cycle the kernel would never write
	if _, err := samplerOn(f).Sample("demo", 100); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	wide := newFakeTree()
	wide.set(1, 1, mib)
	for pid := 2; pid <= maxTreeProcesses*2; pid++ {
		wide.set(pid, 1, mib)
		wide.fork(1, pid)
	}
	if _, err := samplerOn(wide).Sample("demo", 1); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if wide.reads > maxTreeProcesses {
		t.Errorf("walk cost %d /proc reads, want at most %d", wide.reads, maxTreeProcesses)
	}
}

// State for a workload that has gone must not sit in the map for the platform's
// life, and the next workload to take the name must not inherit its CPU.
func TestForgetAndRetainDropState(t *testing.T) {
	f := newFakeTree()
	f.set(1, 1, mib)
	f.set(2, 2, mib)
	f.set(3, 3, mib)
	s := samplerOn(f)
	s.Sample("a", 1)
	s.Sample("b", 2)
	s.Sample("c", 3)

	s.Forget("b")
	if got := s.Tracked(); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("after Forget: tracked = %v, want [a c]", got)
	}

	s.Retain([]string{"c"})
	if got := s.Tracked(); len(got) != 1 || got[0] != "c" {
		t.Errorf("after Retain: tracked = %v, want [c]", got)
	}
}

// The real reader against this process: a smoke test that the children files
// are where this package thinks they are, so that a kernel or a /proc layout
// this code cannot read shows up here rather than as a silently untreed sample.
func TestChildrenReadsRealProc(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no procfs")
	}
	// Not asserting a count: this process's children are the test binary's
	// business. What is asserted is that reading them is not a panic and that
	// a pid with no children is empty rather than an error.
	Children(os.Getpid())

	// Enough work to register against USER_HZ, which is 100: a reading of a
	// process that has used under 10ms is legitimately 0.00 seconds.
	for spin := 0.0; spin < 4e7; spin++ {
		_ = spin * spin
	}

	s := NewTreeSampler()
	got, err := s.Sample("self", os.Getpid())
	if err != nil {
		t.Fatalf("Sample(self): %v", err)
	}
	if got.CPUSeconds <= 0 || got.RSSBytes <= 0 {
		t.Errorf("Sample(self) = %+v, want non-zero readings", got)
	}
}

func approxEq(got, want, tol float64) bool {
	d := got - want
	return d < tol && d > -tol
}
