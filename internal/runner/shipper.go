package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

const (
	// shipInterval batches consumption reports.
	shipInterval = 5 * time.Second
	// logShipInterval batches log lines. It is the bound on what an abruptly
	// killed workload loses — at most one interval, which is the whole reason
	// shipping is incremental rather than at exit — and the lag a person
	// following `toft job run --logs` sees. It was shipInterval's five seconds,
	// which made a run with a five-second timeout stream nothing at all: its
	// first batch was due at the moment it was stopped. An empty interval
	// costs no request, so a quiet workload pays nothing for the shorter one.
	logShipInterval = time.Second
	// flushTimeout bounds one shipment.
	flushTimeout = 30 * time.Second
	// maxBufferedLines caps what one interval may hold. Ingest needs a
	// per-run rate cap: a job in a tight print loop must not be able to turn
	// log shipping into a denial of service against the platform it reports
	// to, and dropping with an explicit count is honest where silently
	// buffering forever is not.
	maxBufferedLines = 2000
	// maxLineBytes truncates a single monstrous line rather than shipping it.
	maxLineBytes = 16 * 1024
	// maxShipmentBytes is the encoded size one shipment stays under. The
	// ingest route refuses a body over 4 MiB (maxLogBatchBytes in
	// internal/api), and an interval may hold 2000 lines of 16 KiB each — eight
	// times that, before JSON escaping inflates it further. So a full interval
	// goes as several shipments rather than one that is refused whole: the cap
	// above is the rate limit, this is only the envelope.
	maxShipmentBytes = 3 << 20
)

// logShipper batches a workload's output and ships it over the callback
// channel. One path, behaving identically across both isolated backends —
// native cloud log drivers still run in parallel as the operator's debug view,
// but they are not the source of truth the UI reads.
type logShipper struct {
	client *Client
	// isRun is what makes a service stop shipping its own output.
	//
	// A run's lines are its deliverable and go to job_run_logs. A service has
	// no run to attach to, so the platform discarded its whole batch at ingest
	// — which meant every isolated service on the install sent its entire
	// stdout across the network so that the platform could write it into its
	// *own* log one line at a time and move on. The tee below is what makes
	// the pod's log worth reading, and it is where a service's own output
	// belongs; there is nothing for the wire to carry.
	//
	// Fixed at construction, which is after bootstrap and strictly before any
	// app output exists: Pump is only ever fed a process the runner has
	// already started, so there is no window in which this is not yet known.
	isRun bool

	mu      sync.Mutex
	buf     []LogLine
	seq     int
	dropped int
	closed  bool

	stop chan struct{}
	done chan struct{}
}

func newLogShipper(c *Client, isRun bool) *logShipper {
	s := &logShipper{
		client: c,
		isRun:  isRun,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *logShipper) loop() {
	defer close(s.done)
	t := time.NewTicker(logShipInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush(context.Background())
		case <-s.stop:
			return
		}
	}
}

// Log buffers one line of the platform's own output — the stage marks, the
// version checks, the reason a start failed.
func (s *logShipper) Log(stream, line string) {
	s.record(LogSourcePlatform, stream, line)
}

// record buffers one line and tees it to the runner's own stdout.
//
// The tee is what makes a pod's log worth reading. Nothing the runner handles
// reaches the container's stdout by itself: app output is piped into the
// shipper rather than inherited, and everything the runner says goes to the
// shipper too, so without this the pod's log is very close to empty — and a
// *service's* shipped copy is discarded at ingest, because a service has no run
// to attach lines to. Both copies existed; neither was reachable.
//
// A failed write is ignored. Nothing the runner can usefully do about its own
// stdout, and a log line may never be the reason a workload stops.
func (s *logShipper) record(source, stream, line string) {
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes] + " …[truncated]"
	}

	at := time.Now().UTC()
	fmt.Fprintln(logTee, LogMarker(at, source, stream)+line)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// The tee always; the wire only for what the platform keeps. See isRun.
	if source == LogSourceApp && !s.isRun {
		return
	}
	if len(s.buf) >= maxBufferedLines {
		// Drop the newest rather than the oldest: the beginning of a failure
		// is what explains it, and the tail of a runaway loop is noise.
		s.dropped++
		return
	}
	s.seq++
	s.buf = append(s.buf, LogLine{
		Seq: s.seq, At: at, Source: source, Stream: stream, Line: line,
	})
}

func (s *logShipper) Logf(stream, format string, args ...any) {
	s.Log(stream, fmt.Sprintf(format, args...))
}

// logTee is where the marked copy of every line the runner handles goes.
//
// os.Stdout, which is the pod's log — and a variable rather than the constant
// it looks like because a *service's* app output now goes nowhere else. A test
// that wants to see what a service printed has to read what the pod would.
var logTee io.Writer = os.Stdout

// Pump streams a reader's lines into the buffer until it closes. This is the
// app's own output, and the only route it has into the runner — which is what
// makes it the one place a forged platform marker can be defused.
func (s *logShipper) Pump(stream string, r io.Reader) {
	scanLines(r, func(line string) {
		s.record(LogSourceApp, stream, NeutralizeLine(line))
	})
}

// Flush ships whatever is buffered. A failure drops the batch rather than
// retrying forever: logs are best-effort, and unbounded retry inside a
// workload would grow memory for the length of a platform outage.
//
// **The caller's cancellation is not inherited**, only its values. The flush
// that matters most is the one after the workload was told to stop — a
// timeout, a cancel, a node drain — and the context the runner holds is
// exactly the one that SIGTERM cancelled. Shipping on it failed at once and
// dropped the batch, so a timed-out run kept none of what it printed; the
// lines that explain a timeout are the last ones. flushTimeout still bounds
// the attempt.
func (s *logShipper) Flush(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()

	s.mu.Lock()
	if len(s.buf) == 0 && s.dropped == 0 {
		s.mu.Unlock()
		return
	}
	batch := LogBatch{Lines: s.buf, Dropped: s.dropped}
	s.buf, s.dropped = nil, 0
	s.mu.Unlock()

	for _, part := range splitShipment(batch, maxShipmentBytes) {
		if err := s.client.ShipLogs(ctx, part); err != nil {
			log.Printf("runner: dropping %d log line(s): %v", len(part.Lines), err)
		}
	}
}

// splitShipment cuts one interval's batch into consecutive shipments whose
// lines encode to under limit bytes each, in order. Each line is sized by
// encoding it, because JSON escaping can grow a line to six times its length.
// The drop count rides on the last shipment only, so the notice the platform
// writes still follows the batch's last line. A shipment that fails is lost on
// its own; the ones after it are still sent.
func splitShipment(batch LogBatch, limit int) []LogBatch {
	var parts []LogBatch
	var cur []LogLine
	size := 0
	for _, l := range batch.Lines {
		n := limit // a line that cannot be encoded goes alone
		if b, err := json.Marshal(l); err == nil {
			n = len(b) + 1
		}
		if len(cur) > 0 && size+n > limit {
			parts = append(parts, LogBatch{Lines: cur})
			cur, size = nil, 0
		}
		cur = append(cur, l)
		size += n
	}
	if len(cur) > 0 || len(parts) == 0 {
		parts = append(parts, LogBatch{Lines: cur})
	}
	parts[len(parts)-1].Dropped = batch.Dropped
	return parts
}

func (s *logShipper) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	close(s.stop)
	<-s.done

	s.Flush(context.Background())
}

// startConsumptionReporter reads the workload's own control group on the same
// batched interval as logs and ships the figures home.
//
// The runner parents the app process and shares its control group, so these
// are the app's numbers, not the runner's overhead. Reporting incrementally
// rather than at exit is what keeps the record of an out-of-memory kill —
// the case where the numbers matter most and where an at-exit report would
// never be sent.
func startConsumptionReporter(client *Client, shipper *logShipper) func() {
	// A host run with no cgroup of its own: the one this runner could read is
	// the platform's, and the platform measures the run itself.
	if os.Getenv(EnvConsumption) == ConsumptionByHost {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, err := readCgroup(); err != nil {
			// Not fatal. A workload that cannot read its own control group
			// still runs; it simply reports no consumption, and the platform
			// records that period as a gap rather than as zero usage.
			shipper.Logf("stderr", "runner: consumption reporting unavailable: %v", err)
			return
		}

		var peak int64
		var repeats repeatLog
		t := time.NewTicker(shipInterval)
		defer t.Stop()

		report := func() {
			stat, err := readCgroup()
			if err != nil {
				return
			}
			if stat.PeakBytes > peak {
				peak = stat.PeakBytes
			}
			if stat.MemoryBytes > peak {
				peak = stat.MemoryBytes
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			rep := ConsumptionReport{
				CPUSeconds:  stat.CPUSeconds,
				MemoryBytes: stat.MemoryBytes,
				PeakBytes:   peak,
				ObservedAt:  time.Now().UTC(),
			}
			if stat.Throttled {
				rep.Periods, rep.ThrottledPeriods = &stat.Periods, &stat.ThrottledPeriods
				rep.ThrottledSeconds = &stat.ThrottledSeconds
			}
			if err := client.ReportConsumption(ctx, rep); err != nil {
				repeats.say("runner: consumption report: %v", err)
			} else {
				repeats.recovered("runner: consumption reporting recovered")
			}
		}

		for {
			select {
			case <-t.C:
				report()
			case <-stop:
				report() // one last reading on the way out
				return
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// jsonUnmarshal is wrapped so run.go need not import encoding/json directly
// for one call.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// repeatSummaryInterval is how often a failure that keeps happening identically
// is allowed to say so again.
const repeatSummaryInterval = 5 * time.Minute

// repeatLog collapses a failure that repeats unchanged.
//
// Every five seconds forever is what a workload reporting consumption does, so
// a fault it cannot recover from writes an identical line every five seconds
// for the life of the pod — 14,832 of them on one app here, which pushed the
// output worth reading out of the pane and cost real disk on a busy install.
// The first occurrence is said in full, because the first one is the diagnosis;
// after that the same message is held until it changes, the interval elapses,
// or it stops — and what is printed then carries the count, so nothing is
// quietly lost.
//
// Not a backoff on the *work*: consumption keeps being reported on its own
// interval, because the fault may be the platform's and may end. Only the
// narration is rationed.
type repeatLog struct {
	last     string
	count    int
	lastSaid time.Time
}

func (r *repeatLog) say(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()
	if msg != r.last {
		if r.count > 1 {
			log.Printf("%s (repeated %d times)", r.last, r.count)
		}
		r.last, r.count, r.lastSaid = msg, 1, now
		log.Print(msg)
		return
	}
	r.count++
	if now.Sub(r.lastSaid) >= repeatSummaryInterval {
		log.Printf("%s (repeated %d times)", msg, r.count)
		r.count, r.lastSaid = 0, now
	}
}

// recovered closes off a run of failures, so a log that went quiet is
// distinguishable from one whose problem went away.
func (r *repeatLog) recovered(msg string) {
	if r.last == "" {
		return
	}
	if r.count > 1 {
		log.Printf("%s (repeated %d times)", r.last, r.count)
	}
	log.Print(msg)
	r.last, r.count = "", 0
}
