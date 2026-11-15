package resource

import (
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"toft/internal/procstat"
)

// Resource accounting is tested on what makes the numbers trustworthy, not on
// whether the plumbing runs: counters survive a restart without double-counting
// or resetting, a sampling gap is recorded as a gap rather than interpolated, an
// abruptly killed workload retains what it reported, roll-ups sum to their
// intervals, and every collector emits the same shape for equivalent work.

type fakeStore struct {
	mu        sync.Mutex
	intervals []Interval
	failNext  int
}

func (f *fakeStore) RecordConsumption(ivs []Interval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errors.New("database unavailable")
	}
	f.intervals = append(f.intervals, ivs...)
	return nil
}

func (f *fakeStore) all() []Interval {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Interval, len(f.intervals))
	copy(out, f.intervals)
	return out
}

// clock drives time explicitly so a test never sleeps through an interval.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// newTestRecorder returns a Recorder whose flush loop never starts, so every
// flush in a test is one the test asked for.
func newTestRecorder(store Store, clk *clock) *Recorder {
	r := NewRecorder(store)
	r.now = clk.now
	r.once.Do(func() {}) // consume the lazy start
	return r
}

func approx(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want ~%v", label, got, want)
	}
}

// --- Counters, not samples ---

// The core arithmetic: a period total is the difference between cumulative
// readings, never the readings themselves. Summing samples would credit an app
// with its whole lifetime CPU on every tick.
func TestCPUIsDifferencedNotSummed(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	// A process that has already burned 100s of CPU before we first see it,
	// then uses 2s more across two 10s intervals.
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 100, MemoryBytes: 100 << 20, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 101, MemoryBytes: 100 << 20, ObservedAt: clk.advance(10 * time.Second)})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 102, MemoryBytes: 100 << 20, ObservedAt: clk.advance(10 * time.Second)})

	r.Flush()

	ivs := store.all()
	if len(ivs) != 1 {
		t.Fatalf("got %d intervals, want 1: %+v", len(ivs), ivs)
	}
	approx(t, "CPUSeconds", ivs[0].CPUSeconds, 2, 0.001)
}

// Memory is a level, not a counter, so it is integrated over time. An
// irregular cadence must not skew the average — otherwise a slow tick during a
// memory spike would dominate the period.
func TestMemoryIsTimeWeighted(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	const mb = 1 << 20
	// 100 MiB for 90s, then a 1 GiB spike for 10s.
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", MemoryBytes: 100 * mb, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", MemoryBytes: 100 * mb, ObservedAt: clk.advance(90 * time.Second)})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", MemoryBytes: 1024 * mb, ObservedAt: clk.advance(10 * time.Second)})

	r.Flush()
	iv := store.all()[0]

	// Trapezoid: 100 MiB over 90s, then the ramp 100→1024 over 10s.
	want := float64(100*mb)*90 + (float64(100*mb)+float64(1024*mb))/2*10
	approx(t, "MemoryByteSeconds", iv.MemoryByteSeconds, want, want*0.001)

	// The average is dragged up only a little; the peak records the spike.
	// This is exactly the split the PRD asks for: peak explains an
	// out-of-memory kill, the average explains cost.
	if iv.PeakMemoryBytes != 1024*mb {
		t.Errorf("PeakMemoryBytes = %d, want %d", iv.PeakMemoryBytes, 1024*mb)
	}
	avgMB := iv.AverageMemoryBytes() / mb
	if avgMB < 120 || avgMB > 200 {
		t.Errorf("average = %.0f MiB; a spike should not dominate the period average", avgMB)
	}
}

// --- Restarts ---

// A restarted workload's counter starts from zero. Treating that as a normal
// reading would either credit a huge negative or, worse, silently reset the
// app's period total.
func TestRestartDoesNotDoubleCountOrReset(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	// 50s of CPU under one PID.
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 0, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 50, ObservedAt: clk.advance(60 * time.Second)})
	// The process is replaced; the new one has used 5s since *its* start.
	r.Record(Sample{AppID: "a", WorkloadID: "rel#2", CPUSeconds: 5, ObservedAt: clk.advance(60 * time.Second)})

	r.Flush()
	iv := store.all()[0]

	// 50 from the first run plus 5 from the second. Not 55-minus-nonsense, and
	// not 5 (which is what a naive reset would leave).
	approx(t, "CPUSeconds across a restart", iv.CPUSeconds, 55, 0.001)
}

// A counter that goes backwards without the workload ID changing is a restart
// the collector could not observe. Credit the new counter, and flag the window
// so nobody reads it as authoritative.
func TestUnobservedRestartIsFlaggedAsAGap(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 100, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 3, ObservedAt: clk.advance(30 * time.Second)})

	r.Flush()
	iv := store.all()[0]

	if iv.CPUSeconds < 0 {
		t.Errorf("CPUSeconds = %v; a backwards counter must never produce negative usage", iv.CPUSeconds)
	}
	approx(t, "CPUSeconds", iv.CPUSeconds, 3, 0.001)
	if !iv.Gap {
		t.Error("a backwards counter should mark the interval as a gap")
	}
}

// --- Gaps ---

// A workload's very first sample has nothing to difference against. Crediting
// its whole cumulative counter to that interval would attribute a week of
// pre-existing CPU to the ten seconds after a platform restart.
func TestFirstSampleIsAGapAndCreditsNothing(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 86400, MemoryBytes: 50 << 20, ObservedAt: clk.now()})
	clk.advance(10 * time.Second)
	r.Flush()

	iv := store.all()[0]
	if iv.CPUSeconds != 0 {
		t.Errorf("CPUSeconds = %v; the first reading has nothing to difference against", iv.CPUSeconds)
	}
	if !iv.Gap {
		t.Error("the first interval of a workload must be marked as a gap")
	}
	// Memory is a level, so the reading itself is still usable.
	if iv.PeakMemoryBytes != 50<<20 {
		t.Errorf("PeakMemoryBytes = %d, want the observed level", iv.PeakMemoryBytes)
	}
}

// After a platform restart the recorder has no memory, so the first interval
// is a gap — but the *next* one differences correctly against it. Nothing is
// invented and nothing is lost twice.
func TestCountersSurviveAPlatformRestart(t *testing.T) {
	clk := newClock()

	// Before the restart. A flush lands a moment after the last sample, as it
	// does in production where sampling and flushing are separate timers.
	before := &fakeStore{}
	r1 := newTestRecorder(before, clk)
	r1.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 10, MemoryBytes: 1 << 20, ObservedAt: clk.now()})
	r1.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 20, MemoryBytes: 1 << 20, ObservedAt: clk.advance(10 * time.Second)})
	clk.advance(time.Second)
	r1.Flush()

	// Platform restarts. The app keeps running; its counter keeps climbing.
	after := &fakeStore{}
	r2 := newTestRecorder(after, clk)
	r2.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 25, MemoryBytes: 1 << 20, ObservedAt: clk.advance(9 * time.Second)})
	clk.advance(time.Second)
	r2.Flush()
	r2.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 35, MemoryBytes: 1 << 20, ObservedAt: clk.advance(9 * time.Second)})
	clk.advance(time.Second)
	r2.Flush()

	pre := before.all()
	post := after.all()

	approx(t, "before the restart", pre[0].CPUSeconds, 10, 0.001)
	if !post[0].Gap {
		t.Error("the first interval after a restart should be a gap")
	}
	if post[0].CPUSeconds != 0 {
		t.Errorf("the gap interval credited %v CPU-seconds; it must credit none", post[0].CPUSeconds)
	}
	approx(t, "after the restart", post[1].CPUSeconds, 10, 0.001)

	// Total recorded is 20s. The 5s spanning the restart is genuinely lost —
	// that is the honest answer, and it is bounded by one interval.
	total := pre[0].CPUSeconds + post[0].CPUSeconds + post[1].CPUSeconds
	approx(t, "total across the restart", total, 20, 0.001)
}

// --- Incremental reporting ---

// The case that matters most: a workload killed abruptly must retain what it
// reported. Recording only at exit would lose the record of an out-of-memory
// kill precisely when it is most wanted.
func TestAbruptlyKilledWorkloadRetainsWhatItReported(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	const mb = 1 << 20
	// Two full intervals of climbing memory, flushed as they go.
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 0, MemoryBytes: 200 * mb, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 5, MemoryBytes: 400 * mb, ObservedAt: clk.advance(10 * time.Second)})
	r.Flush()
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 10, MemoryBytes: 900 * mb, ObservedAt: clk.advance(10 * time.Second)})
	r.Flush()

	// ...and then the kernel kills it. Nothing further is reported.

	ivs := store.all()
	if len(ivs) < 2 {
		t.Fatalf("got %d intervals; incremental reporting should have written as it went", len(ivs))
	}

	var peak int64
	var cpu float64
	for _, iv := range ivs {
		if iv.PeakMemoryBytes > peak {
			peak = iv.PeakMemoryBytes
		}
		cpu += iv.CPUSeconds
	}
	// The evidence of the cause survives the kill.
	if peak != 900*mb {
		t.Errorf("peak memory = %d MiB, want 900 MiB — the evidence of the kill was lost", peak/mb)
	}
	approx(t, "CPU retained", cpu, 10, 0.001)
}

// Flushing must not lose the anchor: the interval after a flush differences
// against the last reading, not against zero.
func TestFlushDoesNotRestartTheCounter(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 100, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 110, ObservedAt: clk.advance(10 * time.Second)})
	r.Flush()
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 115, ObservedAt: clk.advance(10 * time.Second)})
	r.Flush()

	ivs := store.all()
	if len(ivs) != 2 {
		t.Fatalf("got %d intervals, want 2", len(ivs))
	}
	approx(t, "first interval", ivs[0].CPUSeconds, 10, 0.001)
	approx(t, "second interval", ivs[1].CPUSeconds, 5, 0.001)
	if ivs[1].Gap {
		t.Error("the interval after a flush is continuous, not a gap")
	}
}

// Roll-ups sum to their intervals: the whole point of storing byte-seconds
// rather than an average is that they add up without re-weighting.
func TestIntervalsSumToTheirTotal(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	const mb = 1 << 20
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 0, MemoryBytes: 100 * mb, ObservedAt: clk.now()})
	for i := 1; i <= 6; i++ {
		r.Record(Sample{AppID: "a", WorkloadID: "rel#1",
			CPUSeconds:  float64(i) * 3,
			MemoryBytes: int64(100*mb) * int64(i),
			ObservedAt:  clk.advance(10 * time.Second)})
		r.Flush()
	}

	var cpu, byteSeconds float64
	var start, end time.Time
	for i, iv := range store.all() {
		cpu += iv.CPUSeconds
		byteSeconds += iv.MemoryByteSeconds
		if i == 0 {
			start = iv.Start
		}
		end = iv.End
	}

	approx(t, "summed CPU", cpu, 18, 0.001)

	// The same figures computed as one long window must match the sum of the
	// pieces — otherwise a daily roll-up would disagree with the intervals it
	// was built from.
	oneShot := &fakeStore{}
	clk2 := newClock()
	r2 := newTestRecorder(oneShot, clk2)
	r2.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 0, MemoryBytes: 100 * mb, ObservedAt: clk2.now()})
	for i := 1; i <= 6; i++ {
		r2.Record(Sample{AppID: "a", WorkloadID: "rel#1",
			CPUSeconds:  float64(i) * 3,
			MemoryBytes: int64(100*mb) * int64(i),
			ObservedAt:  clk2.advance(10 * time.Second)})
	}
	r2.Flush()
	whole := oneShot.all()[0]

	approx(t, "roll-up CPU vs intervals", cpu, whole.CPUSeconds, 0.001)
	approx(t, "roll-up byte-seconds vs intervals", byteSeconds, whole.MemoryByteSeconds, byteSeconds*0.001)
	if end.Sub(start) != whole.Duration() {
		t.Errorf("summed duration %s != whole-window duration %s", end.Sub(start), whole.Duration())
	}
}

// --- Attribution ---

// A job run is attributed to both the job and its app, so a two-hour job's
// cost is visible beside the app that triggered it without being double-
// counted into the app's own workload.
func TestJobRunsAccumulateSeparatelyFromTheirApp(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "web#1", CPUSeconds: 0, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", JobRunID: "run-7", WorkloadID: "job#1", CPUSeconds: 0, ObservedAt: clk.now()})
	clk.advance(10 * time.Second)
	r.Record(Sample{AppID: "a", WorkloadID: "web#1", CPUSeconds: 1, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", JobRunID: "run-7", WorkloadID: "job#1", CPUSeconds: 9, ObservedAt: clk.now()})

	r.Flush()

	byRun := map[string]float64{}
	for _, iv := range store.all() {
		if iv.AppID != "a" {
			t.Errorf("interval not attributed to its app: %+v", iv)
		}
		byRun[iv.JobRunID] += iv.CPUSeconds
	}
	approx(t, "the app's own workload", byRun[""], 1, 0.001)
	approx(t, "the job run", byRun["run-7"], 9, 0.001)
}

func TestSamplesWithoutAnAppAreIgnored(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{WorkloadID: "orphan#1", CPUSeconds: 100, ObservedAt: clk.now()})
	clk.advance(10 * time.Second)
	r.Flush()

	if n := len(store.all()); n != 0 {
		t.Errorf("recorded %d unattributable interval(s)", n)
	}
}

// Deleting an app must stop it emitting empty intervals forever.
func TestForgetDropsAWorkload(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 5, MemoryBytes: 1 << 20, ObservedAt: clk.now()})
	r.Forget("a", "")
	clk.advance(10 * time.Second)
	r.Flush()

	if n := len(store.all()); n != 0 {
		t.Errorf("a forgotten workload still wrote %d interval(s)", n)
	}
}

// A workload that stops being sampled — an app slept, stopped or deleted, a run
// that never reported — writes nothing more, and its window is dropped; one
// whose samples arrive less often than the flush loses nothing (JOBS-7).
func TestAWorkloadNoLongerSampledWritesNothing(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 5, MemoryBytes: 1 << 20, ObservedAt: clk.now()})
	clk.advance(5 * time.Second)
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 6, MemoryBytes: 1 << 20, ObservedAt: clk.now()})
	clk.advance(5 * time.Second)
	r.Flush()
	before := len(store.all())
	if before != 1 {
		t.Fatalf("wrote %d interval(s) while sampled, want 1", before)
	}

	// The workload goes: no more samples, ever.
	for i := 0; i < 30; i++ {
		clk.advance(FlushInterval)
		r.Flush()
	}
	if n := len(store.all()); n != before {
		t.Errorf("a workload no longer sampled wrote %d more interval(s)", n-before)
	}
	r.mu.Lock()
	open := len(r.open)
	r.mu.Unlock()
	if open != 0 {
		t.Errorf("%d window(s) still open for a workload gone for five minutes", open)
	}
}

// The collector samples every 15 s and the recorder flushes every 10 s, so a
// live workload's flush often has no new sample: the window waits for the
// next one and the CPU between them is still counted, once.
func TestASlowCadenceLosesNoUsage(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	cpu := 0.0
	for i := 0; i < 12; i++ { // three minutes of samples every 15 s
		r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: cpu, MemoryBytes: 1 << 20, ObservedAt: clk.now()})
		cpu += 1.5
		for _, step := range []time.Duration{10 * time.Second, 5 * time.Second} {
			clk.advance(step)
			r.Flush()
		}
	}
	total := 0.0
	for _, iv := range store.all() {
		if iv.CPUSeconds == 0 && iv.PeakMemoryBytes == 0 {
			t.Errorf("an empty interval was written: %+v", iv)
		}
		total += iv.CPUSeconds
	}
	// Eleven differences of 1.5 s between twelve samples.
	approx(t, "cpu", total, 16.5, 0.001)
}

// --- Durability of the buffer ---

// One retry, then drop. Consumption data is worth a retry — it may be the only
// record of a job that has already exited — but unbounded buffering through a
// long outage would grow memory inside the platform it is meant to measure.
func TestFlushRetriesOnceThenDrops(t *testing.T) {
	store := &fakeStore{failNext: 1}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 0, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: 7, ObservedAt: clk.advance(10 * time.Second)})
	r.Flush() // fails; batch held

	if n := len(store.all()); n != 0 {
		t.Fatalf("store recorded %d interval(s) despite failing", n)
	}

	clk.advance(10 * time.Second)
	r.Flush() // succeeds; the held batch lands

	var cpu float64
	for _, iv := range store.all() {
		cpu += iv.CPUSeconds
	}
	approx(t, "CPU recovered after a failed flush", cpu, 7, 0.001)
}

// --- The collector ---

// Both collectors must emit the same shape for equivalent work, or a mixed
// install cannot be compared. The shared collector's job is to turn a /proc
// reading into exactly the Sample an isolated runner would self-report.
func TestSharedCollectorEmitsTheSameShapeAsSelfReporting(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	const mb = 1 << 20
	cpu := 0.0
	c := NewSharedCollector(r, func() ([]Workload, error) {
		return []Workload{{AppID: "a", Name: "demo-abc123", PID: 4242}}, nil
	})
	c.sampler.ReadChildren = func(int) []int { return nil }
	c.sampler.ReadStat = func(pid int) (procstat.Stat, error) {
		if pid != 4242 {
			return procstat.Stat{}, os.ErrNotExist
		}
		cpu += 2
		return procstat.Stat{CPUSeconds: cpu, RSSBytes: 256 * mb}, nil
	}

	c.sample()
	c.sample()
	c.sample()
	r.Flush()

	ivs := store.all()
	if len(ivs) != 1 {
		t.Fatalf("got %d intervals, want 1", len(ivs))
	}
	// Same differencing as a self-reported counter: 6 cumulative, 4 in-window.
	approx(t, "collected CPU", ivs[0].CPUSeconds, 4, 0.001)
	if ivs[0].PeakMemoryBytes != 256*mb {
		t.Errorf("PeakMemoryBytes = %d MiB, want 256", ivs[0].PeakMemoryBytes/mb)
	}
	if ivs[0].AppID != "a" {
		t.Errorf("AppID = %q", ivs[0].AppID)
	}
}

// A process that exits between listing and reading is a normal race, not an
// error: its usage up to the previous sample is already recorded, and one
// unreadable process must not stop the others being sampled.
func TestCollectorSkipsProcessesThatHaveGone(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	c := NewSharedCollector(r, func() ([]Workload, error) {
		return []Workload{
			{AppID: "gone", Name: "gone-1", PID: 1},
			{AppID: "alive", Name: "alive-1", PID: 2},
		}, nil
	})
	calls := 0
	c.sampler.ReadChildren = func(int) []int { return nil }
	c.sampler.ReadStat = func(pid int) (procstat.Stat, error) {
		if pid == 1 {
			return procstat.Stat{}, os.ErrNotExist
		}
		calls++
		return procstat.Stat{CPUSeconds: float64(calls), RSSBytes: 1 << 20}, nil
	}

	c.sample()
	clk.advance(10 * time.Second)
	c.sample()
	r.Flush()

	apps := map[string]bool{}
	for _, iv := range store.all() {
		apps[iv.AppID] = true
	}
	if apps["gone"] {
		t.Error("an unreadable process should record nothing")
	}
	if !apps["alive"] {
		t.Error("a readable process beside an unreadable one was skipped")
	}
}

// A listing failure is a collection outage. It must produce no samples at all,
// so the next interval carries a gap rather than interpolated usage.
func TestCollectorOutageProducesNoSamples(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	c := NewSharedCollector(r, func() ([]Workload, error) {
		return nil, errors.New("process manager unavailable")
	})
	c.sample()
	clk.advance(10 * time.Second)
	r.Flush()

	if n := len(store.all()); n != 0 {
		t.Errorf("a collection outage wrote %d interval(s)", n)
	}
}

// **The over-billing this change had to avoid.** Summing a tree makes CPU fall
// when a child exits, and `fold` reads a fall under an unchanged workload ID as
// a restart it could not see — crediting the whole new counter. An app that
// shells out on a timer would have been billed for its own lifetime again on
// every subprocess exit. The sampler carries departed CPU so the counter the
// recorder sees only ever rises.
func TestCollectorDoesNotRebillWhenASubprocessExits(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	const wrapper, server, child = 100, 200, 300
	cpu := map[int]float64{wrapper: 1, server: 10, child: 5}
	kids := map[int][]int{wrapper: {server, child}}

	c := NewSharedCollector(r, func() ([]Workload, error) {
		return []Workload{{AppID: "a", Name: "demo-abc123", PID: wrapper}}, nil
	})
	c.sampler.ReadChildren = func(pid int) []int { return kids[pid] }
	c.sampler.ReadStat = func(pid int) (procstat.Stat, error) {
		v, ok := cpu[pid]
		if !ok {
			return procstat.Stat{}, os.ErrNotExist
		}
		return procstat.Stat{CPUSeconds: v, RSSBytes: 1 << 20}, nil
	}

	c.sample() // 1 + 10 + 5 = 16, the baseline
	cpu[server], cpu[child] = 11, 6
	c.sample() // 18: one second each
	delete(cpu, child)
	kids[wrapper] = []int{server}
	cpu[server] = 12
	c.sample() // 1 + 12 + 6 carried = 19: one more second
	r.Flush()

	ivs := store.all()
	if len(ivs) != 1 {
		t.Fatalf("got %d intervals, want 1", len(ivs))
	}
	// Three seconds of real work. Without the carry the third reading is 13,
	// which is a fall, and the window would have been credited 2 + 13 = 15.
	approx(t, "billed CPU", ivs[0].CPUSeconds, 3, 0.001)
}

// The sampler must not hold a tree for every workload the install has ever run.
func TestCollectorForgetsWorkloadsThatHaveGone(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	live := []Workload{
		{AppID: "a", Name: "a-1", PID: 1},
		{AppID: "b", Name: "b-1", PID: 2},
	}
	c := NewSharedCollector(r, func() ([]Workload, error) { return live, nil })
	c.sampler.ReadChildren = func(int) []int { return nil }
	c.sampler.ReadStat = func(pid int) (procstat.Stat, error) {
		return procstat.Stat{CPUSeconds: float64(pid), RSSBytes: 1 << 20}, nil
	}

	c.sample()
	if got := len(c.sampler.Tracked()); got != 2 {
		t.Fatalf("tracking %d trees, want 2", got)
	}
	live = live[:1]
	c.sample()
	if got := len(c.sampler.Tracked()); got != 1 {
		t.Errorf("tracking %d trees after one workload went, want 1", got)
	}
}

// ADR §132: shared consumption is counted once. A shared
// app the agent placed with no cgroup of its own runs under a runner that
// reports nothing, and is billed here as its tree less the runner — the
// platform's binary, ten megabytes of no part of what the app used.
func TestAWrappedWorkloadIsBilledWithoutItsRunner(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	const runnerPID, app = 100, 200
	cpu := map[int]float64{runnerPID: 2, app: 10}
	rss := map[int]int64{runnerPID: 10 << 20, app: 300 << 20}
	c := NewSharedCollector(r, func() ([]Workload, error) {
		return []Workload{{AppID: "a", Name: "demo-abc123", PID: runnerPID, Wrapped: true}}, nil
	})
	c.sampler.ReadChildren = func(pid int) []int {
		if pid == runnerPID {
			return []int{app}
		}
		return nil
	}
	read := func(pid int) (procstat.Stat, error) {
		return procstat.Stat{CPUSeconds: cpu[pid], RSSBytes: rss[pid]}, nil
	}
	c.sampler.ReadStat = read
	c.readRoot = read

	c.sample()
	clk.advance(10 * time.Second)
	cpu[runnerPID], cpu[app] = 3, 14 // the runner's second is not the app's
	c.sample()
	r.Flush()

	ivs := store.all()
	if len(ivs) != 1 {
		t.Fatalf("got %d intervals, want 1", len(ivs))
	}
	approx(t, "billed CPU", ivs[0].CPUSeconds, 4, 0.001)
	if peak := ivs[0].PeakMemoryBytes; peak != 300<<20 {
		t.Errorf("peak memory %d, want the app's 300 MiB without the runner's", peak)
	}
}

// Two workloads of one app reporting at once — every isolated deploy, where
// the new pod reports before the old one is deleted — are each differenced
// against their own previous reading, not read as a restart on every
// alternation (code review 2026-11-11, H-3).
func TestOverlappingWorkloadsAreDifferencedSeparately(t *testing.T) {
	store := &fakeStore{}
	clk := newClock()
	r := newTestRecorder(store, clk)

	r.Record(Sample{AppID: "a", WorkloadID: "old", CPUSeconds: 10000, ObservedAt: clk.now()})
	r.Record(Sample{AppID: "a", WorkloadID: "old", CPUSeconds: 10001, ObservedAt: clk.advance(5 * time.Second)})
	r.Record(Sample{AppID: "a", WorkloadID: "new", CPUSeconds: 1, ObservedAt: clk.advance(2 * time.Second)})
	r.Record(Sample{AppID: "a", WorkloadID: "old", CPUSeconds: 10002, ObservedAt: clk.advance(3 * time.Second)})
	r.Record(Sample{AppID: "a", WorkloadID: "new", CPUSeconds: 2, ObservedAt: clk.advance(2 * time.Second)})
	r.Record(Sample{AppID: "a", WorkloadID: "old", CPUSeconds: 10003, ObservedAt: clk.advance(3 * time.Second)})
	r.Flush()

	iv := store.all()[0]
	// old: 3 since its first reading here; new: 1 whole (since its start) + 1.
	approx(t, "CPU across an overlapping deploy", iv.CPUSeconds, 5, 0.001)
}

// A batch that fails twice is dropped; only the newest batch is ever held, so
// an outage cannot grow the buffer without bound (code review 2026-11-11,
// H-4).
func TestASecondFailureDropsTheHeldBatch(t *testing.T) {
	store := &fakeStore{failNext: 3}
	clk := newClock()
	r := newTestRecorder(store, clk)
	for i := 0; i < 5; i++ {
		r.Record(Sample{AppID: "a", WorkloadID: "rel#1", CPUSeconds: float64(i * 10), ObservedAt: clk.advance(10 * time.Second)})
		r.Flush()
	}
	// The first flush writes nothing (one reading, nothing to difference).
	// The next three fail, each 10 CPU-seconds: held after each is {1}, {2}
	// (1 dropped), {3} (2 dropped); the fifth lands {3, 4} = 20.
	var cpu float64
	for _, iv := range store.all() {
		cpu += iv.CPUSeconds
	}
	approx(t, "CPU after an outage of three flushes", cpu, 20, 0.001)
	r.mu.Lock()
	pending := len(r.pending)
	r.mu.Unlock()
	if pending != 0 {
		t.Errorf("%d interval(s) still pending after a successful flush", pending)
	}
}
