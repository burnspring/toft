package telemetry

import (
	"context"
	"log"
	"sync"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The queue's shape. These are the OpenTelemetry SDK's own defaults, restated
// here so they are a decision rather than an inheritance — and so the one that
// matters can be argued: `queueSize` is the memory this pipeline is allowed to
// cost while a collector is unreachable, and it is bounded because the
// alternative is an observability feature that takes the platform down when the
// observability stack goes down.
const (
	queueSize     = 2048
	batchSize     = 512
	batchSchedule = 5 * time.Second
	exportTimeout = 10 * time.Second
)

// boundedProcessor is the span processor: a bounded queue, a batching worker,
// and a producer that can never block.
//
// It is written here rather than taken from sdktrace.NewBatchSpanProcessor for
// one reason, and it is not taste. D11 requires that a broken export be visible
// in the admin UI, and D2 requires that a full queue *count* its drops rather
// than dropping them silently — the SDK's processor keeps both figures private,
// so an install could only report "telemetry is configured", never "telemetry
// is configured and has been failing for five minutes", which is the only
// version of that statement worth putting on a screen.
//
// Everything else about it matches the SDK's behaviour deliberately, including
// the four constants above.
type boundedProcessor struct {
	exporter sdktrace.SpanExporter
	status   *Status

	queue chan sdktrace.ReadOnlySpan
	stop  chan struct{}
	done  chan struct{}

	// flush requests a synchronous drain; the worker closes the reply channel
	// when the queue it could see is gone.
	flush chan chan struct{}

	stopOnce sync.Once
}

func newBoundedProcessor(exp sdktrace.SpanExporter, status *Status) *boundedProcessor {
	p := &boundedProcessor{
		exporter: exp,
		status:   status,
		queue:    make(chan sdktrace.ReadOnlySpan, queueSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		flush:    make(chan chan struct{}),
	}
	go p.run()
	return p
}

// OnStart does nothing. Every attribute this pipeline records is known at end,
// and a processor that touched a span on the way *in* would be doing work on
// the request path — which is the one thing this package may not do.
func (p *boundedProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd hands the span to the worker, or drops it.
//
// The default branch is the whole design. A producer that blocks here is a
// request goroutine pinned by a hung collector, which is the failure mode that
// takes a platform down — and it is the one `TestCollectorHangsDoesNotBlock`
// exists to hold.
func (p *boundedProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	select {
	case p.queue <- s:
	default:
		p.status.SpansDropped.Add(1)
		RecordTelemetryDropped(SignalTraces, DropReasonQueueFull)
	}
}

func (p *boundedProcessor) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.stop) })
	select {
	case <-p.done:
	case <-ctx.Done():
		// The caller's deadline is the cap, and main gives it five seconds. A
		// binary that will not stop because a collector is down is worse than a
		// lost batch.
		return ctx.Err()
	}
	return p.exporter.Shutdown(ctx)
}

func (p *boundedProcessor) ForceFlush(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case p.flush <- reply:
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *boundedProcessor) run() {
	defer close(p.done)

	t := time.NewTicker(batchSchedule)
	defer t.Stop()

	batch := make([]sdktrace.ReadOnlySpan, 0, batchSize)
	send := func() {
		if len(batch) == 0 {
			return
		}
		p.export(batch)
		batch = batch[:0]
	}

	for {
		select {
		case s := <-p.queue:
			batch = append(batch, s)
			if len(batch) >= batchSize {
				send()
			}
		case <-t.C:
			send()
		case reply := <-p.flush:
			// Drain whatever is queued now, then send. Anything arriving after
			// this point belongs to the next batch, which is what "flush" means
			// everywhere else in this codebase too.
			for {
				select {
				case s := <-p.queue:
					batch = append(batch, s)
					if len(batch) >= batchSize {
						send()
					}
					continue
				default:
				}
				break
			}
			send()
			close(reply)
		case <-p.stop:
			for {
				select {
				case s := <-p.queue:
					batch = append(batch, s)
					if len(batch) >= batchSize {
						send()
					}
					continue
				default:
				}
				break
			}
			send()
			return
		}
	}
}

// export ships one batch, under its own deadline.
//
// The deadline is not the caller's: there is no caller. A batch that cannot be
// delivered in ten seconds is abandoned and counted, because the alternative is
// a worker that stops draining the queue and turns a slow collector into a
// hundred percent drop rate.
func (p *boundedProcessor) export(batch []sdktrace.ReadOnlySpan) {
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()

	if err := p.exporter.ExportSpans(ctx, batch); err != nil {
		p.status.ExportFailures.Add(1)
		p.status.recordError(err)
		RecordTelemetryExportFailure(SignalTraces, ErrorType(err))
		p.status.logFailure("traces", err)
		return
	}
	p.status.SpansExported.Add(int64(len(batch)))
}

// --- log rate limiting ------------------------------------------------------

// logEvery bounds how often a failing pipeline may write to the log.
//
// An observability feature that floods the log it exists to complement is
// self-defeating, and a collector that is down is down for minutes, not for one
// batch. One line a minute per signal is enough to notice and few enough to
// ignore; the counters carry the real story, and the admin card reads those.
const logEvery = time.Minute

type failureLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (f *failureLog) allow(signal string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = map[string]time.Time{}
	}
	now := time.Now()
	if t, ok := f.last[signal]; ok && now.Sub(t) < logEvery {
		return false
	}
	f.last[signal] = now
	return true
}

func (s *Status) logFailure(signal string, err error) {
	if s.logs.allow(signal) {
		log.Printf("telemetry: %s export failing: %v (further messages suppressed for %s; "+
			"counts are on Admin → Info)", signal, err, logEvery)
	}
}
