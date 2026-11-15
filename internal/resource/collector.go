package resource

import (
	"fmt"
	"log"
	"time"

	"toft/internal/procstat"
)

// SharedCollector samples shared-mode workloads by PID from the footprint
// Toft shares with them.
//
// **Measurement has one owner per workload** (ADR §132). A
// workload in a control group of its own — every isolated pod, and a shared
// workload the agent placed in a cgroup — has a runner that reads that group
// and reports for itself, and this collector skips it. A shared workload with
// no group of its own shares the platform's, so its runner reports nothing
// (TOFT_CONSUMPTION=host) and this reads its tree from outside, which is a
// legitimate vantage point precisely because the platform and the app are on
// the same machine. Both emit the same Sample, so nothing downstream learns
// which.
type SharedCollector struct {
	recorder *Recorder
	// processes reports the live workloads: app ID and job run keyed by the
	// PID to read. Supplied by the caller because only it knows which process
	// belongs to which app.
	processes func() ([]Workload, error)

	interval time.Duration
	stop     chan struct{}

	// sampler reads each workload as its whole process tree. A launcher that
	// wraps the workload — `npm run start` — leaves the real server one level
	// below the pid the process manager holds, and billing the wrapper bills
	// almost nothing. Its ReadStat and ReadChildren are swappable so tests can
	// drive a tree without real processes.
	sampler *procstat.TreeSampler
	// readRoot reads a wrapper's own figures, to take them off its tree's.
	readRoot func(pid int) (procstat.Stat, error)
}

// Workload names one running process and what it should be billed to.
type Workload struct {
	AppID    string
	JobRunID string
	// Name is the workload's stable identity across restarts (the release's
	// process name), and is combined with the PID to detect a restart.
	Name string
	PID  int
	// Wrapped says PID is a runner wrapping the app: what is billed is its
	// tree less the runner itself, which is the platform's binary and no part
	// of what the app used (the host watchdog's rule).
	Wrapped bool
}

func NewSharedCollector(recorder *Recorder, processes func() ([]Workload, error)) *SharedCollector {
	return &SharedCollector{
		recorder:  recorder,
		processes: processes,
		interval:  SampleInterval,
		stop:      make(chan struct{}),
		sampler:   procstat.NewTreeSampler(),
		readRoot:  procstat.Read,
	}
}

// Start begins sampling on its own goroutine. Collection is deliberately out
// of band: the existing rule is that measurement never adds cost to the
// request path, because measurement that degrades the thing it measures is
// worse than no measurement.
func (c *SharedCollector) Start() {
	go func() {
		t := time.NewTicker(c.interval)
		defer t.Stop()
		c.sample()
		for {
			select {
			case <-t.C:
				c.sample()
			case <-c.stop:
				return
			}
		}
	}()
}

func (c *SharedCollector) Stop() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
}

// sample reads every live workload once. A process that has gone away between
// listing and reading is skipped silently — that is a normal race, not an
// error, and its usage up to the previous sample is already recorded.
func (c *SharedCollector) sample() {
	workloads, err := c.processes()
	if err != nil {
		// A collection outage becomes a gap in the next interval rather than
		// invented usage.
		log.Printf("resource: listing workloads: %v", err)
		return
	}

	now := time.Now()
	live := make([]string, 0, len(workloads))
	for _, w := range workloads {
		if w.AppID == "" || w.PID <= 0 {
			continue
		}
		// The sampler is keyed by the workload's identity rather than its pid:
		// a restart must reach the same entry, so that the new pid is *seen* as
		// a restart and the old tree's carried CPU is dropped rather than added
		// to a counter that began again at zero.
		key := w.AppID + "\x00" + w.JobRunID + "\x00" + w.Name
		live = append(live, key)
		stat, err := c.sampler.Sample(key, w.PID)
		if err != nil {
			continue
		}
		if w.Wrapped {
			if self, err := c.readRoot(w.PID); err == nil {
				stat.CPUSeconds -= self.CPUSeconds
				stat.RSSBytes -= self.RSSBytes
			}
			if stat.CPUSeconds < 0 {
				stat.CPUSeconds = 0
			}
			if stat.RSSBytes < 0 {
				stat.RSSBytes = 0
			}
		}
		c.recorder.Record(Sample{
			AppID:    w.AppID,
			JobRunID: w.JobRunID,
			// Name plus PID: a restarted release keeps its name but gets a new
			// PID, which is what tells the recorder the counter has reset.
			WorkloadID:  fmt.Sprintf("%s#%d", w.Name, w.PID),
			CPUSeconds:  stat.CPUSeconds,
			MemoryBytes: stat.RSSBytes,
			ObservedAt:  now,
		})
	}

	// This pass sees the whole live set, so it is where a workload that has
	// gone stops being carried. Without it the sampler would hold every tree
	// the install has ever run.
	c.sampler.Retain(live)
}
