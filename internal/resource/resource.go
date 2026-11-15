// Package resource records what workloads actually consume.
//
// Toft is a shared environment that counts *requests*, not resources. An
// administrator cannot say which app is starving the others, which team to
// charge, whether a limit is set anywhere near real usage, or what a two-hour
// job cost. Per-process metrics exist but are instantaneous and unretained — a
// dashboard reading, never a record.
//
// Three decisions shape everything here.
//
// **Counters, not samples.** An instantaneous CPU percentage cannot be summed
// into a period total. What is recorded is cumulative CPU-seconds,
// time-weighted average memory (which yields byte-hours), and peak memory.
// Peak explains an out-of-memory kill; the average explains cost.
//
// **Two collectors, one metric model.** Shared-mode workloads have no runner
// wrapping them — Toft execs them directly — so the platform samples
// those processes by PID from the footprint it shares with them. An isolated
// workload self-reports over the callback channel it already holds. Both emit
// the same shape, so a mixed install stays comparable and the storage and
// reporting layers never learn which mode a number came from.
//
// **Report incrementally, not at exit.** An abruptly killed workload — an
// out-of-memory kill being the case that matters most — would otherwise lose
// its record precisely when it is most wanted.
//
// Kept separate from request-usage accounting (internal/usage): different
// questions, different sources. It mirrors that package's accumulate-and-flush
// shape, and follows the same rule that sampling never touches the request
// path.
package resource

import (
	"log"
	"sync"
	"time"
)

// FlushInterval matches the request-usage flush cadence. It is also the bound
// on how much an abruptly killed workload loses: at most one interval.
const FlushInterval = 10 * time.Second

// SampleInterval is how often the shared collector reads the process table.
// More often buys resolution nobody asks for and costs a subprocess each time.
const SampleInterval = 15 * time.Second

// forgetAfter is how long a workload goes without a sample before its window is
// dropped. Several sample intervals, so a slow cadence (the collector samples
// less often than the recorder flushes) is never read as a workload gone, and
// short enough that a slept, stopped or deleted app — or a run that never
// reported its end — stops being accounted for within a minute.
const forgetAfter = 4 * SampleInterval

// Sample is one observation of a workload, in the shape both collectors emit.
//
// CPUSeconds is *cumulative since the process started*, not since the last
// sample. That is what makes the metric robust: a missed interval loses
// resolution but not total, and a restart is visible as the counter going
// backwards rather than as invented usage.
type Sample struct {
	AppID string
	// JobRunID attributes the sample to a background run as well as its app.
	// Empty for an app's own workload.
	JobRunID string
	// WorkloadID distinguishes one run of a workload from the next, so a
	// restart can be told from a counter glitch. For the shared backend this
	// is the release's process name plus its PID.
	WorkloadID string
	// CPUSeconds is cumulative CPU time consumed by this workload since it
	// started.
	CPUSeconds float64
	// MemoryBytes is the reading at ObservedAt. Averaging is time-weighted
	// over the interval, so an irregular sampling cadence does not skew it.
	MemoryBytes int64
	// Throttling is the CPU throttling counters, cumulative like CPUSeconds,
	// or nil where the workload's cgroup keeps none: absent, never zero.
	Throttling *Throttling
	ObservedAt time.Time
}

// Throttling is cpu.stat's three throttling counters: enforcement periods
// run in, periods held at the CPU limit, and the time held.
type Throttling struct {
	Periods          int64
	ThrottledPeriods int64
	ThrottledSeconds float64
}

// Interval is one accumulated window for one workload, and is what gets
// persisted.
type Interval struct {
	AppID    string
	JobRunID string
	Start    time.Time
	End      time.Time
	// CPUSeconds consumed *within this window* — the difference between
	// cumulative readings, never a raw counter.
	CPUSeconds float64
	// MemoryByteSeconds is the time-weighted integral of memory over the
	// window. Dividing by the window's length gives average bytes; the raw
	// figure sums across windows without weighting errors, which is why it,
	// rather than an average, is what is stored.
	MemoryByteSeconds float64
	PeakMemoryBytes   int64
	// Throttling is the window's throttling deltas, nil where no reading in
	// it carried any.
	Throttling *Throttling
	// Gap marks a window whose start had no prior reading to difference
	// against — the first sample of a workload, or the sample after a
	// collection outage. Its CPU figure is zero rather than interpolated: a
	// gap is recorded as a gap, because inventing the missing usage is how a
	// capacity report quietly becomes fiction.
	Gap bool
}

// Duration of the accumulated window.
func (i Interval) Duration() time.Duration { return i.End.Sub(i.Start) }

// AverageMemoryBytes is the time-weighted mean over the window. Zero-length
// windows report their peak, which is the only reading they have.
func (i Interval) AverageMemoryBytes() float64 {
	secs := i.Duration().Seconds()
	if secs <= 0 {
		return float64(i.PeakMemoryBytes)
	}
	return i.MemoryByteSeconds / secs
}

// Store is the flush sink; implemented by *database.DB.
type Store interface {
	RecordConsumption(intervals []Interval) error
}

// Recorder accumulates samples in memory and flushes completed intervals on a
// timer. Sampling and flushing are both out of band — nothing here is ever
// called from a request path.
type Recorder struct {
	store Store

	mu      sync.Mutex
	open    map[key]*window
	pending []Interval
	once    sync.Once
	stop    chan struct{}
	// now is swappable so tests can drive time rather than sleep through it.
	now func() time.Time
}

type key struct {
	appID    string
	jobRunID string
}

// window is the in-progress accumulation for one workload.
type window struct {
	start time.Time
	// last is the previous sample, which the next one is differenced against.
	last Sample
	// seen marks that `last` holds a real reading. Without it the first sample
	// of a workload would be differenced against a zero and credited with its
	// whole cumulative counter in one interval.
	seen bool
	// sampled marks that a reading arrived since the window opened. A window
	// rolled forward at a flush starts without one, and until one comes it
	// holds nothing observed: written, it would be an interval of zero usage
	// that dilutes every average, for as long as the workload is gone.
	sampled           bool
	cpuSeconds        float64
	memoryByteSeconds float64
	peakMemoryBytes   int64
	throttling        *Throttling
	gap               bool
	// lasts is the previous reading of each workload that has reported into
	// this window, by WorkloadID. Two workloads of one app report at once on
	// every isolated deploy — the new pod's reporter starts before the old
	// pod is deleted — and differencing each against the one `last` read
	// every alternation as a restart, crediting a whole lifetime counter
	// each time (code review 2026-11-11, H-3). Each is differenced against
	// its own previous reading instead.
	lasts map[string]Sample
}

func NewRecorder(store Store) *Recorder {
	return &Recorder{
		store: store,
		open:  make(map[key]*window),
		stop:  make(chan struct{}),
		now:   time.Now,
	}
}

// Record folds one observation into the open interval for its workload. Safe
// to call from any collector; the flush loop starts lazily on first use, so an
// idle Recorder (a test, an install with no running apps) costs nothing.
func (r *Recorder) Record(s Sample) {
	if s.AppID == "" {
		return
	}
	if s.ObservedAt.IsZero() {
		s.ObservedAt = r.now()
	}
	r.once.Do(func() { go r.loop() })

	k := key{appID: s.AppID, jobRunID: s.JobRunID}

	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.open[k]
	if !ok {
		w = &window{start: s.ObservedAt, gap: true}
		r.open[k] = w
	}
	w.fold(s)
}

// fold differences one sample against the previous reading for the same
// workload.
func (w *window) fold(s Sample) {
	w.sampled = true
	if s.MemoryBytes > w.peakMemoryBytes {
		w.peakMemoryBytes = s.MemoryBytes
	}

	if w.lasts == nil {
		w.lasts = map[string]Sample{}
	}
	if !w.seen {
		// Nothing to difference against yet. Record the reading and wait: the
		// interval carries the gap flag until a second sample arrives.
		w.last, w.seen = s, true
		w.lasts[s.WorkloadID] = s
		return
	}

	elapsed := s.ObservedAt.Sub(w.last.ObservedAt).Seconds()
	if elapsed < 0 {
		// Clock went backwards. Re-anchor rather than subtract nonsense.
		w.last = s
		w.lasts[s.WorkloadID] = s
		return
	}

	prev, known := w.lasts[s.WorkloadID]
	w.foldThrottling(s, prev, known)
	switch {
	case !known:
		// A workload this window has not seen: the counter is usage since
		// *its* start and can be taken whole — a restart, or the new pod of a
		// deploy reporting beside the old one.
		w.cpuSeconds += s.CPUSeconds
	case s.CPUSeconds >= prev.CPUSeconds:
		w.cpuSeconds += s.CPUSeconds - prev.CPUSeconds
	default:
		// The counter went backwards without the workload ID changing — a
		// restart the collector could not see. Credit the new counter rather
		// than a negative, and mark the window so the reading is not read as
		// authoritative.
		w.cpuSeconds += s.CPUSeconds
		w.gap = true
	}

	// Trapezoid over the interval: memory is a level, not a counter, and
	// weighting by elapsed time is what keeps an irregular cadence from
	// skewing the average. Against the latest reading of any workload: the
	// app's memory is what its workloads hold together.
	w.memoryByteSeconds += (float64(w.last.MemoryBytes) + float64(s.MemoryBytes)) / 2 * elapsed
	w.last = s
	w.lasts[s.WorkloadID] = s
}

// foldThrottling differences the throttling counters the way CPU is: a new
// workload or a counter that went backwards is a restart, taken whole — a
// gap, never a negative.
func (w *window) foldThrottling(s Sample, before Sample, known bool) {
	if s.Throttling == nil {
		return
	}
	cur, prev := *s.Throttling, before.Throttling
	var d Throttling
	switch {
	case !known:
		d = cur
	case prev == nil:
		// The first reading of this workload that carried the counters:
		// nothing to difference against, so nothing is credited.
		return
	case cur.Periods < prev.Periods || cur.ThrottledPeriods < prev.ThrottledPeriods:
		d = cur
	default:
		d = Throttling{Periods: cur.Periods - prev.Periods, ThrottledPeriods: cur.ThrottledPeriods - prev.ThrottledPeriods,
			ThrottledSeconds: max(cur.ThrottledSeconds-prev.ThrottledSeconds, 0)}
	}
	if w.throttling == nil {
		w.throttling = &Throttling{}
	}
	w.throttling.Periods += d.Periods
	w.throttling.ThrottledPeriods += d.ThrottledPeriods
	w.throttling.ThrottledSeconds += d.ThrottledSeconds
}

func (r *Recorder) loop() {
	t := time.NewTicker(FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.Flush()
		case <-r.stop:
			r.Flush()
			return
		}
	}
}

// Close flushes and stops the timer. Called on shutdown so the last interval
// is not lost with the process.
func (r *Recorder) Close() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
}

// Flush closes every open interval and writes it. The next sample for a
// workload opens a fresh window anchored at the last reading, so cumulative
// counters carry across the boundary and nothing is double-counted.
func (r *Recorder) Flush() {
	now := r.now()

	r.mu.Lock()
	var intervals []Interval
	for k, w := range r.open {
		if !w.seen {
			// Never got a reading at all; nothing to record and nothing to
			// carry forward.
			delete(r.open, k)
			continue
		}
		if !w.sampled {
			// Nothing observed since the last flush. Either the cadence is
			// slower than the flush — the window stays open, unwritten, and
			// the next sample's interval covers the whole span — or the
			// workload has gone, and its window goes with it. Nothing else
			// closes the window of an app that slept, stopped, crashed or was
			// deleted, or of a run that never reported.
			if now.Sub(w.last.ObservedAt) > forgetAfter {
				delete(r.open, k)
			}
			continue
		}
		if iv, ok := w.close(k, now); ok {
			intervals = append(intervals, iv)
		}
		// Roll the window forward from the last readings. Dropping the
		// samples here instead would make the next interval look like a fresh
		// start and re-credit a whole cumulative counter. A workload that has
		// not reported in forgetAfter is gone, and its reading goes with it.
		lasts := make(map[string]Sample, len(w.lasts))
		for id, last := range w.lasts {
			if now.Sub(last.ObservedAt) <= forgetAfter {
				lasts[id] = last
			}
		}
		r.open[k] = &window{start: now, last: w.last, seen: true, lasts: lasts}
	}
	held := len(r.pending)
	intervals = append(r.pending, intervals...)
	r.pending = nil
	r.mu.Unlock()

	if len(intervals) == 0 {
		return
	}
	if err := r.store.RecordConsumption(intervals); err != nil {
		// Retry once on the next tick. Consumption data is worth one retry —
		// it is the record of a job that may already have exited — but not
		// unbounded buffering through a long database outage: the batch that
		// has now failed twice is dropped, and only this tick's is held. (The
		// old "is anything pending" test was always false here, so every
		// batch was held for ever and the INSERT outgrew the protocol's
		// parameter limit — code review 2026-11-11, H-4.)
		fresh := intervals[held:]
		if held > 0 {
			log.Printf("resource: dropping %d interval(s) that failed to land twice: %v", held, err)
		}
		log.Printf("resource: deferring %d interval(s) after flush error: %v", len(fresh), err)
		r.mu.Lock()
		r.pending = append([]Interval(nil), fresh...)
		r.mu.Unlock()
	}
}

// close turns an accumulated window into an interval, or reports that there is
// nothing worth writing.
func (w *window) close(k key, end time.Time) (Interval, bool) {
	iv := Interval{
		AppID:             k.appID,
		JobRunID:          k.jobRunID,
		Start:             w.start,
		End:               end,
		CPUSeconds:        w.cpuSeconds,
		MemoryByteSeconds: w.memoryByteSeconds,
		PeakMemoryBytes:   w.peakMemoryBytes,
		Throttling:        w.throttling,
		Gap:               w.gap,
	}
	// A window with no elapsed time and no usage is noise.
	if iv.Duration() <= 0 && iv.CPUSeconds == 0 && iv.PeakMemoryBytes == 0 {
		return Interval{}, false
	}
	return iv, true
}

// Forget drops a workload's open window without writing it. Call it when a
// workload is deleted outright, so a stopped app does not keep emitting
// zero-usage intervals forever.
func (r *Recorder) Forget(appID, jobRunID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.open, key{appID: appID, jobRunID: jobRunID})
}
