// Package appout puts a shared app's own output on the platform's one stream.
//
// The agent hands every workload's output over its socket
// (AgentRuntime.StartOutput); this is where each line becomes one record —
// `kind=app`, the app, the release, the stream, the runner's stamp as its time
// — and where the cost of that is bounded.
// ADR §46 stands: nothing here forwards anything. A collector reading the
// platform's stdout is what turns this into "collected centrally".
//
// The cost is the point of most of this file. The platform's stdout is also
// the request path: every access line goes through the same handler's mutex,
// and a JSON answer under 2 KiB is not flushed until its access line is
// written. So the reader of the agent's stream never writes; it hands each
// line to a bounded queue, and one writer drains it a record at a time, so an
// access line waits behind at most one app record. In front of the queue, a per-app cap keeps it empty
// in the ordinary case, and what the cap and a full queue refuse is counted on
// the same stream.
package appout

import (
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"toft/internal/artifact"
	"toft/internal/runtime"
)

// The cap. Code constants, not a dial — ADR §14 keeps the dangerous end of a
// range off the settings page. A hundred lines a second is generous for an
// app whose output is a record; three thousand is half a minute of burst at
// that rate, which covers a startup banner and a stack trace.
const (
	capPerSecond = 100
	capBurst     = 3000
	// queueSize bounds what the writer may fall behind by. The files are the
	// real buffer; a held-back copy costs disk, which rotation bounds, never
	// memory.
	queueSize = 10000
	// dropReportEvery is how often an app still being refused is reported.
	dropReportEvery = time.Minute
	// switchReread is how often the uncapped switch is read.
	switchReread = 10 * time.Second
)

// The reasons a line was not copied.
const (
	ReasonCap          = "cap"
	ReasonBackpressure = "backpressure"
)

// Sink receives the lines the agent hands over. It satisfies runtime.AppOutput.
type Sink struct {
	emit  func(slog.Record) error
	now   func() time.Time
	queue chan slog.Record

	uncapped atomic.Bool

	mu      sync.Mutex
	buckets map[string]*bucket
	drops   map[string]*dropState
	totals  map[string]int64

	// OnDrop is told of every refused line, for the telemetry counter. Nil
	// is nothing.
	OnDrop func(appID, reason string, n int64)
}

// bucket is one app's token bucket.
type bucket struct {
	tokens float64
	at     time.Time
}

// dropState is what has been refused since the last report.
type dropState struct {
	n            int64
	reason       string
	from, to     time.Time
	slug         string
	lastReported time.Time
}

// New makes a sink that writes through emit — obs.Emit in the platform, a
// test's handler otherwise — and starts its writer. uncapped is read every
// ten seconds; nil is never.
func New(emit func(slog.Record) error, uncapped func() bool) *Sink {
	s := &Sink{
		emit:    emit,
		now:     time.Now,
		queue:   make(chan slog.Record, queueSize),
		buckets: map[string]*bucket{},
		drops:   map[string]*dropState{},
		totals:  map[string]int64{},
	}
	go s.write()
	if uncapped != nil {
		s.uncapped.Store(uncapped())
		go func() {
			for range time.Tick(switchReread) {
				s.uncapped.Store(uncapped())
			}
		}()
	}
	return s
}

// write is the one writer: a record at a time, never a batch, so an access
// line waits behind at most one app record.
func (s *Sink) write() {
	for r := range s.queue {
		_ = s.emit(r)
	}
}

// Line copies one line, or counts it.
func (s *Sink) Line(l runtime.OutputLine) {
	now := s.now()
	slug := slugOf(l.Workload, l.DeploymentID)

	s.mu.Lock()
	if !s.uncapped.Load() && !s.take(l.AppID, now) {
		s.refuse(l.AppID, slug, ReasonCap, now, l.At)
		s.mu.Unlock()
		return
	}
	// Before the first line admitted after a drop, the drop.
	if d := s.drops[l.AppID]; d != nil && d.n > 0 {
		if !s.enqueue(s.dropRecord(l.AppID, d, now)) {
			d.lastReported = time.Time{}
		} else {
			d.n = 0
		}
	}
	s.mu.Unlock()

	if !s.enqueue(lineRecord(l, slug, now)) {
		s.mu.Lock()
		s.refuse(l.AppID, slug, ReasonBackpressure, now, l.At)
		s.mu.Unlock()
	}
}

// take spends one token from the app's bucket, refilling it for the time
// since it was last read.
func (s *Sink) take(appID string, now time.Time) bool {
	b := s.buckets[appID]
	if b == nil {
		b = &bucket{tokens: capBurst, at: now}
		s.buckets[appID] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * capPerSecond
	if b.tokens > capBurst {
		b.tokens = capBurst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// refuse counts one line not copied, and reports at least once a minute while
// an app keeps being refused.
func (s *Sink) refuse(appID, slug, reason string, now time.Time, at *time.Time) {
	d := s.drops[appID]
	if d == nil {
		d = &dropState{}
		s.drops[appID] = d
	}
	stamp := now
	if at != nil {
		stamp = *at
	}
	if d.n == 0 {
		d.from = stamp
	}
	d.n++
	d.to, d.reason, d.slug = stamp, reason, slug
	s.totals[appID]++
	if s.OnDrop != nil {
		s.OnDrop(appID, reason, 1)
	}
	if now.Sub(d.lastReported) >= dropReportEvery && s.enqueue(s.dropRecord(appID, d, now)) {
		d.n = 0
	}
}

// dropRecord says how many of one app's lines were not copied, between when,
// and why.
func (s *Sink) dropRecord(appID string, d *dropState, now time.Time) slog.Record {
	d.lastReported = now
	r := slog.NewRecord(now, slog.LevelInfo, "app output not copied", 0)
	r.AddAttrs(slog.String("kind", "app_dropped"), slog.String("app_id", appID))
	if d.slug != "" {
		r.AddAttrs(slog.String("slug", d.slug))
	}
	r.AddAttrs(slog.Int64("dropped", d.n),
		slog.Time("from", d.from), slog.Time("to", d.to), slog.String("reason", d.reason))
	return r
}

func (s *Sink) enqueue(r slog.Record) bool {
	select {
	case s.queue <- r:
		return true
	default:
		return false
	}
}

// DroppedSinceStart is how many of an app's lines this process has not
// copied. Held in memory and reset on restart, as presence is.
func (s *Sink) DroppedSinceStart(appID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totals[appID]
}

// Uncapped is whether the cap is lifted right now.
func (s *Sink) Uncapped() bool { return s.uncapped.Load() }

// lineRecord is one app line as a record. **The line is a field value, never
// raw bytes**: an app that prints a whole access-line JSON object produces a
// kind=app record with that text escaped in msg, so it cannot forge a
// platform line.
func lineRecord(l runtime.OutputLine, slug string, now time.Time) slog.Record {
	at, source := now, ""
	if l.At != nil {
		at = *l.At
	} else {
		source = "copy"
	}
	r := slog.NewRecord(at, slog.LevelInfo, Mask(l.Line), 0)
	r.AddAttrs(slog.String("kind", "app"), slog.String("app_id", l.AppID))
	if slug != "" {
		r.AddAttrs(slog.String("slug", slug))
	}
	r.AddAttrs(slog.String("workload", l.Workload), slog.String("stream", l.Stream))
	if l.RunID != "" {
		r.AddAttrs(slog.String("run_id", l.RunID), slog.String("job", l.Job))
	}
	if source != "" {
		r.AddAttrs(slog.String("time_source", source))
	}
	return r
}

// slugOf is the release name with its -<8 hex> removed, checked against the
// deployment AgentRuntime read off the release path: the slug the release
// started under, with no database read. A name of another shape gives none.
func slugOf(workload, deploymentID string) string {
	i := strings.LastIndexByte(workload, '-')
	if i <= 0 {
		return ""
	}
	short := strings.ReplaceAll(deploymentID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	if workload[i+1:] != short {
		return ""
	}
	return workload[:i]
}

// tokenShape is the three credentials Toft issues, by their prefixes.
var tokenShape = regexp.MustCompile(`\b(tofta_|toftu_|toftd_)[A-Za-z0-9_\-]{8,}`)

// Mask replaces what Toft minted and can recognise by its shape: a presigned
// URL's signature, and the secret after a tofta_, toftu_ or toftd_ prefix.
// Nothing else — Toft knows nothing about an app's own database password, and
// a pattern that pretended to would be a promise the code cannot keep. The
// role key an app is given has no shape and is not masked.
func Mask(line string) string {
	line = artifact.MaskSignedURLs(line)
	if !strings.Contains(line, "toft") {
		return line
	}
	return tokenShape.ReplaceAllString(line, "${1}REDACTED")
}
