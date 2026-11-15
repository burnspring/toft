// Package telemetry is Toft's own observability: OpenTelemetry traces and
// metrics exported to a collector the operator already runs, and an optional
// Prometheus scrape endpoint on a listener of its own.
//
// Three properties govern everything in here, and they are the reason the
// package is shaped the way it is rather than the way an SDK tutorial would
// shape it.
//
// **Telemetry is never load-bearing.** It follows the notification rule exactly
// (ADR §14): no export may fail, block, or slow the request that produced it.
// The span queue is bounded and drops rather than blocks; an unreachable
// collector is a counter and a rate-limited log line, never a 500, never a
// stalled goroutine, never unbounded memory. `internal/telemetry/process.go` is
// where that is actually enforced.
//
// **Absent configuration means the capability is absent.** With no endpoint and
// no scrape address there is no SDK, no exporter goroutine, no middleware on
// either router, no `/metrics` listener, and no card in the admin UI. Every
// record function loads one pointer and returns before it touches an argument,
// so a call site costs one atomic load and no allocation — a property with a
// test (`TestNoopRecordAllocatesNothing`), not a hope.
//
// **Names and attributes are a contract.** An operator builds dashboards and
// alerts against them. The inventory in instruments.go is the specification,
// `TestMetricInventoryMatchesSnapshot` freezes it, and a rename is a breaking
// change with a documented migration.
//
// The whole pipeline is **rebuildable while the process runs**. An
// administrator changes the collector on the admin page, `Apply` builds a new
// pipeline, swaps it in, and drains the old one in the background — so the
// control behaves like every other admin control in this product and takes
// effect immediately. The scrape *listener* is the one thing that does not
// rebuild: it opens a socket, it is configured from the environment alone, and
// it outlives every reconfiguration.
//
// See ADR §31 for the decisions and `docs/dev-reference.md` § Telemetry for the
// mechanism tables.
package telemetry

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"toft/internal/config"
)

// Options is everything the pipeline needs beyond the resolved Runtime: facts
// about this build and this install that no configuration can know.
type Options struct {
	Version  string
	Revision string
	// ExecutionMode is shared | isolated | mixed — what this install can place.
	ExecutionMode string
	// Environment is the environment_tag setting (ADR §14) if one is set. Read
	// once, at startup, and a change to it needs a restart to reach a signal —
	// said out loud because that setting is otherwise runtime-changeable, and
	// because everything *else* about telemetry now reconfigures live.
	Environment string

	// Env is the environment half of the configuration — the part that stays
	// deployment configuration even now that the collector is a setting: the
	// service identity, the CA bundle, and the scrape listener.
	//
	// The listener is the interesting one. It opens a socket, which is why it
	// is not on the settings document: an administrator who could open a
	// network listener from a browser is a different security question from one
	// who can redirect an outbound export. ADR §31 D3.
	Env config.Telemetry

	// Secrets are values that must never appear in an exception message, no
	// matter what wrapped them. The live JWT_SECRET and ENCRYPTION_KEY and the
	// collector credential; see redact.go. Seeding this from the real config is
	// what makes TestExceptionMessageScrubbed catch a *future* secret that was
	// added to config.Config and not to the scrubber.
	Secrets []string

	// Reader and SpanExporter replace the configured exporters with test
	// doubles. Nil in every product caller — they are exported only because the
	// api package's router tests have to exercise the *real* middleware against
	// the *real* router, and that means starting a real pipeline from another
	// package. See internal/telemetry/telemetryharness.
	Reader       sdkmetric.Reader
	SpanExporter sdktrace.SpanExporter
}

// pipeline is one built configuration. Everything a record path touches hangs
// off it, so swapping the pointer swaps the whole thing at once: a request in
// flight finishes against the pipeline it started on rather than half against
// each.
type pipeline struct {
	tracer trace.Tracer
	inst   *Instruments
	status *Status

	tracing bool
	metrics bool

	// runtime is what this was built from, for the admin card and the log.
	runtime Runtime
	// appliedAt is when this configuration took effect. The counters are
	// "since" this rather than since process start — an administrator who has
	// just re-pointed the collector is asking about the new one, and carrying
	// the old one's failures forward would answer a question nobody asked.
	appliedAt time.Time

	shutdown []func(context.Context) error
}

var (
	// mu serialises Apply against itself. Readers never take it: they load the
	// pointer below.
	mu sync.Mutex

	// live is the whole of the package's hot state. nil means telemetry is off,
	// which is the overwhelmingly common case and costs exactly one atomic load
	// on every instrumented path.
	live atomic.Pointer[pipeline]

	// base is the immutable half of Options, kept so Apply can rebuild without
	// the caller passing it again.
	base Options

	// spanProcessor is kept so a drain can be asked for without going through
	// the whole provider. Tests use it; nothing in the product does, because
	// shutdown already drains.
	spanProcessor atomic.Pointer[boundedProcessor]

	// propagator is W3C trace context. It is *not* installed globally on the
	// inbound path: extraction happens in the HTTP middleware and only for a
	// peer inside TRUSTED_PROXY_CIDRS, because a sampling decision taken by an
	// untrusted client is a lever on the operator's bill. See httpmw.go.
	propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

	// recordEnduserID is D7's switch, read on the span path.
	recordEnduserID atomic.Bool

	// stopScrape tears down the third listener. Set once, by Init.
	stopScrape func(context.Context) error
)

// Enabled reports whether any signal is being produced. Call sites that build
// an expensive attribute set — or install a middleware at all — ask this first.
func Enabled() bool { return live.Load() != nil }

// TracingEnabled and MetricsEnabled report the two signals separately, because
// an operator may switch either off while keeping the other.
func TracingEnabled() bool { p := live.Load(); return p != nil && p.tracing }
func MetricsEnabled() bool { p := live.Load(); return p != nil && p.metrics }

// Health returns the export counters behind the admin Info card, or nil when
// telemetry is off — in which case the card is absent rather than empty.
func Health() *Status {
	p := live.Load()
	if p == nil {
		return nil
	}
	return p.status
}

// Current reports the configuration in force and when it took effect.
func Current() (Runtime, time.Time, bool) {
	p := live.Load()
	if p == nil {
		return Runtime{}, time.Time{}, false
	}
	return p.runtime, p.appliedAt, true
}

// Init starts the scrape listener and applies the first configuration, and
// returns a shutdown function.
//
// With nothing configured it returns a no-op shutdown having started no
// goroutine, registered no global provider and opened no socket. That is the
// most important behaviour in the package and the one with the strictest test:
// the overwhelming majority of installs never configure a collector, and they
// are entitled to the binary they had before this package existed.
func Init(ctx context.Context, rt Runtime, opts Options) (func(context.Context) error, error) {
	mu.Lock()
	base = opts
	mu.Unlock()

	// The listener first, and once. It is environment-only and outlives every
	// reconfiguration, so it is deliberately not part of the swappable
	// pipeline: a settings save must not be able to close a socket Prometheus
	// is in the middle of scraping.
	if opts.Env.ScrapeEnabled() {
		stop, err := startScrapeListener(opts.Env)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		stopScrape = stop
		mu.Unlock()
	}

	if err := Apply(ctx, rt); err != nil {
		return nil, err
	}
	return shutdown, nil
}

// Apply builds a pipeline from rt, swaps it in, and drains whatever was there.
//
// The swap happens *before* the drain, and the drain runs in the background. An
// administrator pressing Save must not wait on a collector that has stopped
// answering — which is, after all, the most likely reason they are on that page
// changing the endpoint in the first place.
func Apply(ctx context.Context, rt Runtime) error {
	mu.Lock()
	defer mu.Unlock()

	opts := base

	wantTraces := rt.Enabled() && rt.Traces
	wantMetrics := rt.Enabled() && rt.Metrics
	if opts.SpanExporter != nil {
		wantTraces = true
	}
	if opts.Reader != nil {
		wantMetrics = true
	}
	// The scrape endpoint is a second reason to build a meter provider. An
	// install with Prometheus and no collector is the common on-prem shape, and
	// making it configure an OTLP endpoint it does not have would be a fiction.
	if opts.Env.ScrapeEnabled() {
		wantMetrics = true
	}

	old := live.Load()

	if !wantTraces && !wantMetrics {
		live.Store(nil)
		spanProcessor.Store(nil)
		// The observable sources stay registered. They are registered once, at
		// startup, so clearing them here would leave the gauges absent after an
		// off-then-on until restart; with no pipeline live nothing reads them.
		// shutdown clears them, because there the database is about to close.
		drainInBackground(old)
		return nil
	}

	res, err := buildResource(ctx, opts)
	if err != nil {
		return fmt.Errorf("telemetry: building the resource: %w", err)
	}

	next := &pipeline{
		status:    newStatus(),
		runtime:   rt,
		appliedAt: time.Now().UTC(),
	}
	// A fresh slice, never an append to opts.Secrets, which aliases base. The
	// literals are every credential this pipeline can send: the boot-time
	// secrets, each value of the saved headers, and each value of the
	// environment's — whose whole `k=v,k=v` string never appears verbatim in
	// an error, while a single value a collector quotes back can.
	secrets := make([]string, 0, len(opts.Secrets)+len(rt.Headers)+2)
	secrets = append(secrets, opts.Secrets...)
	for _, v := range rt.Headers {
		secrets = append(secrets, v)
	}
	for _, v := range parseHeaders(opts.Env.Headers) {
		secrets = append(secrets, v)
	}
	newScrubber(secrets)
	recordEnduserID.Store(rt.RecordEnduserID)
	appCap := rt.AppCardinalityCap
	if appCap < 1 {
		appCap = config.DefaultAppCardinalityCap
	}
	appCardinality.Store(newAppCap(appCap))

	if wantTraces {
		exp := opts.SpanExporter
		if exp == nil {
			exp, err = otlptracehttp.New(ctx, traceExporterOptions(rt)...)
			if err != nil {
				return fmt.Errorf("telemetry: building the trace exporter: %w", err)
			}
		}
		// Redaction sits between the queue and the wire, so it runs over every
		// span that leaves this process regardless of which call site produced
		// it. See redact.go for why that is a stage and not a convention.
		proc := newBoundedProcessor(redacting{next: exp}, next.status)
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			// Parent-based, so a trusted caller's decision carries. The untrusted
			// half of that sentence is enforced in the middleware, not here.
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rt.SampleRatio))),
			sdktrace.WithSpanProcessor(proc),
		)
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagator)
		next.tracer = tp.Tracer("toft")
		next.tracing = true
		next.shutdown = append(next.shutdown, tp.Shutdown)
		spanProcessor.Store(proc)
	} else {
		spanProcessor.Store(nil)
	}

	if wantMetrics {
		var readers []sdkmetric.Reader
		if opts.Reader != nil {
			readers = append(readers, opts.Reader)
		} else if rt.Enabled() && rt.Metrics {
			mexp, err := otlpmetrichttp.New(ctx, metricExporterOptions(rt)...)
			if err != nil {
				return fmt.Errorf("telemetry: building the metric exporter: %w", err)
			}
			interval := rt.MetricInterval
			if interval <= 0 {
				interval = time.Minute
			}
			readers = append(readers, sdkmetric.NewPeriodicReader(
				countingMetricExporter{Exporter: mexp, status: next.status},
				sdkmetric.WithInterval(interval),
				sdkmetric.WithTimeout(rt.Timeout),
			))
		}
		if opts.Env.ScrapeEnabled() {
			// A fresh registry and exporter per configuration: a Prometheus
			// Reader belongs to exactly one meter provider, so a rebuild has to
			// bring its own. The listener resolves the current registry on every
			// scrape, which is what keeps the socket stable across the swap.
			reader, err := newScrapeReader()
			if err != nil {
				return fmt.Errorf("telemetry: building the scrape reader: %w", err)
			}
			readers = append(readers, reader)
		}

		// The SDK's own per-instrument limit defaults to 2000 attribute sets,
		// below what the inventory budgets for the largest instrument; past it
		// new sets fold into otel.metric.overflow, which is neither the
		// __other__ fold nor counted as dropped. So the limit is the budget.
		mopts := []sdkmetric.Option{
			sdkmetric.WithResource(res),
			sdkmetric.WithCardinalityLimit(largestSeries()),
		}
		for _, r := range readers {
			mopts = append(mopts, sdkmetric.WithReader(r))
		}
		mp := sdkmetric.NewMeterProvider(mopts...)
		otel.SetMeterProvider(mp)

		next.inst, err = newInstruments(mp.Meter("toft"))
		if err != nil {
			return fmt.Errorf("telemetry: building the instruments: %w", err)
		}
		if err := startRuntimeMetrics(); err != nil {
			// Go runtime metrics are the least load-bearing signal here, so a
			// failure to register them is reported and stepped over rather than
			// taken as a reason to have no telemetry at all.
			log.Printf("telemetry: Go runtime metrics are unavailable: %v", err)
		}
		buildInfo(opts)
		next.metrics = true
		next.shutdown = append(next.shutdown, mp.Shutdown)
	}

	live.Store(next)
	drainInBackground(old)
	return nil
}

// drainTimeout bounds a background drain of a replaced pipeline.
const drainTimeout = 10 * time.Second

// drainInBackground tears down a superseded pipeline without making anybody
// wait for it.
//
// Bounded, because the collector it is trying to reach may be exactly the one
// that stopped answering. Its last batch is lost when it times out, which is
// the correct trade: this pipeline has already been replaced, and the
// alternative is a settings page that hangs.
func drainInBackground(p *pipeline) {
	if p == nil || len(p.shutdown) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		for i := len(p.shutdown) - 1; i >= 0; i-- {
			// Keep going on an error. The meter provider, shut down first,
			// fails its last export exactly when the collector is down, and
			// stopping there would leak the span processor's goroutine,
			// ticker and client — one set per save.
			if err := p.shutdown[i](ctx); err != nil {
				log.Printf("telemetry: the superseded pipeline did not drain: %v", err)
				continue
			}
		}
	}()
}

// shutdown drains and then gives up.
//
// The cap belongs to the caller's context, and main gives it five seconds. A
// binary that will not stop because a collector is down is worse than a lost
// batch, and this is the same drain-not-close reasoning the HTTP listeners
// already follow.
func shutdown(ctx context.Context) error {
	mu.Lock()
	p := live.Load()
	live.Store(nil)
	spanProcessor.Store(nil)
	scrape := stopScrape
	stopScrape = nil
	mu.Unlock()

	// The observable gauges read the database and the build queue. A meter
	// provider's Shutdown performs one last collection, and by then main's
	// deferred db.Close() may already have run — which produced a "sql:
	// database is closed" line on every clean shutdown. Clearing the sources
	// first is the fix, and it is the right one rather than a silenced log:
	// after this point there is nothing left to observe.
	clearObservers()

	var firstErr error
	if p != nil {
		for i := len(p.shutdown) - 1; i >= 0; i-- {
			if err := p.shutdown[i](ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if scrape != nil {
		if err := scrape(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// traceExporterOptions and metricExporterOptions build the exporters from the
// resolved Runtime rather than from the environment.
//
// The SDK would read OTEL_* itself, and the first version of this package let
// it — which was fine while the environment was the only source. It cannot be
// now: a settings document the exporter did not read would be a page that
// looked applied and was not.
// exporterHeaders is the credential an exporter is built with, or nil to
// leave the SDK's own environment read in place.
//
// The OTLP exporters read OTEL_EXPORTER_OTLP_HEADERS (and the per-signal
// variants) from the process environment before applying our options. For a
// saved document that is wrong: with no credential saved, the environment's
// would ride on every export to whatever endpoint the administrator named. So
// a saved document always sets the headers, to an empty map when it has none.
// The environment source keeps the SDK's read, because an install configured
// only with OTEL_EXPORTER_OTLP_TRACES_HEADERS gets its credential from there.
func exporterHeaders(rt Runtime) map[string]string {
	if rt.Source == SourceSettings {
		if rt.Headers == nil {
			return map[string]string{}
		}
		return rt.Headers
	}
	if len(rt.Headers) > 0 {
		return rt.Headers
	}
	return nil
}

func traceExporterOptions(rt Runtime) []otlptracehttp.Option {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(signalURL(rt.Endpoint, "/v1/traces")),
	}
	if h := exporterHeaders(rt); h != nil {
		opts = append(opts, otlptracehttp.WithHeaders(h))
	}
	if rt.Certificate != "" {
		if tlsCfg, err := tlsConfigFromFile(rt.Certificate); err == nil {
			opts = append(opts, otlptracehttp.WithTLSClientConfig(tlsCfg))
		} else {
			log.Printf("telemetry: OTEL_EXPORTER_OTLP_CERTIFICATE: %v", err)
		}
	}
	if rt.Timeout > 0 {
		opts = append(opts, otlptracehttp.WithTimeout(rt.Timeout))
	}
	return opts
}

func metricExporterOptions(rt Runtime) []otlpmetrichttp.Option {
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(signalURL(rt.Endpoint, "/v1/metrics")),
	}
	if h := exporterHeaders(rt); h != nil {
		opts = append(opts, otlpmetrichttp.WithHeaders(h))
	}
	if rt.Certificate != "" {
		if tlsCfg, err := tlsConfigFromFile(rt.Certificate); err == nil {
			opts = append(opts, otlpmetrichttp.WithTLSClientConfig(tlsCfg))
		}
	}
	if rt.Timeout > 0 {
		opts = append(opts, otlpmetrichttp.WithTimeout(rt.Timeout))
	}
	if rt.Temporality == "delta" {
		opts = append(opts, otlpmetrichttp.WithTemporalitySelector(deltaTemporality))
	}
	return opts
}

// deltaTemporality is what a Datadog-shaped backend wants: counters and
// histograms report the change since the last export rather than a running
// total. Gauges are unaffected, because a gauge has no accumulation to reset.
func deltaTemporality(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	switch kind {
	case sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindObservableCounter:
		return metricdata.DeltaTemporality
	default:
		return metricdata.CumulativeTemporality
	}
}

// Flush drains the span queue now and waits for the batch to be exported.
//
// Nothing on a request path calls this and nothing should: the whole point of
// the queue is that a producer never waits on the wire. It exists for tests,
// which would otherwise have to sleep for the batch schedule and hope.
func Flush(ctx context.Context) error {
	p := spanProcessor.Load()
	if p == nil {
		return nil
	}
	return p.ForceFlush(ctx)
}

// --- spans ------------------------------------------------------------------

// Span is the handle a call site holds. Its zero value is inert, which is what
// lets every instrumented function keep exactly one shape whether or not this
// install exports anything.
type Span struct{ span trace.Span }

// Start opens a span. When tracing is off it returns the context unchanged and
// an inert Span, allocating nothing.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, Span) {
	p := live.Load()
	if p == nil || !p.tracing {
		return ctx, Span{}
	}
	ctx, s := p.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, Span{span: s}
}

// StartServer opens a span for an inbound request. Separate from Start only
// because the kind matters to every trace backend's waterfall.
func StartServer(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, Span) {
	p := live.Load()
	if p == nil || !p.tracing {
		return ctx, Span{}
	}
	ctx, s := p.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
	return ctx, Span{span: s}
}

// StartClient opens a span for an outbound call the platform makes.
func StartClient(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, Span) {
	p := live.Load()
	if p == nil || !p.tracing {
		return ctx, Span{}
	}
	ctx, s := p.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx, Span{span: s}
}

// Set adds attributes. Anything whose key is not on the allow-list in redact.go
// is dropped on the way out, so a call site cannot leak by being careless — but
// a call site that sets a forbidden key is still a bug, and the test that walks
// the allow-list is what finds it.
func (s Span) Set(attrs ...attribute.KeyValue) {
	if s.span == nil {
		return
	}
	s.span.SetAttributes(attrs...)
}

// SetName renames a live span. The HTTP middleware needs this: chi does not
// know which route pattern matched until the handler has run, and a span named
// after a raw path is the cardinality bug D5 exists to prevent.
func (s Span) SetName(name string) {
	if s.span == nil {
		return
	}
	s.span.SetName(name)
}

// End closes the span, marking it an error when one is passed.
//
// The error's *type* is what is recorded; its text goes through the scrubber
// and is truncated, because a message is the one free-text field in the whole
// pipeline and therefore the one leak vector (D7).
func (s Span) End(err error) {
	if s.span == nil {
		return
	}
	if err != nil {
		s.span.SetStatus(codes.Error, "")
		s.span.SetAttributes(attribute.String("error.type", ErrorType(err)))
		s.span.RecordError(err)
	}
	s.span.End()
}

// EndOK closes a span that carries an outcome rather than a Go error — a build
// that failed is not an error returned to anybody, but it is still the thing an
// operator is looking for in the waterfall.
func (s Span) EndOK(outcome string) {
	if s.span == nil {
		return
	}
	if outcome != "" {
		s.span.SetAttributes(attribute.String("toft.outcome", outcome))
	}
	if outcome == OutcomeFailed {
		s.span.SetStatus(codes.Error, "")
	}
	s.span.End()
}

// TraceParent renders the current span as a W3C traceparent, or "" when there
// is none.
//
// This is the whole of D9: an isolated run is handed a trace id and nothing
// else. A trace id is not a credential, and the alternative — shipping exporter
// configuration and the operator's collector token into a workload — is what
// D8 refuses in the same breath as it refuses os.Environ().
func TraceParent(ctx context.Context) string {
	p := live.Load()
	if p == nil || !p.tracing {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// --- shared vocabulary ------------------------------------------------------

// Outcomes are a closed enum, matching the status constants already in
// docs/dev-reference.md. Closed because an attribute whose values come from
// anywhere else is an unbounded label, which is how a metrics rollout takes
// down the monitoring system it was meant to protect.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeSkipped   = "skipped"
	OutcomeAbandoned = "abandoned"
	OutcomeBlocked   = "blocked"
	OutcomeCancelled = "cancelled"
)

// Surfaces. Two, and they are the split ADR §15 is about: `platform` is the API
// and the SPA, `app` is the deployed-app gateway. On a single-origin install
// they share a port and still keep separate series, because the question "is
// the platform healthy" and the question "is somebody's Flask app healthy" have
// different answers and different owners.
const (
	SurfacePlatform = "platform"
	SurfaceApp      = "app"
)

// Outcome maps a Go error to the closed outcome enum.
func Outcome(err error) string {
	if err != nil {
		return OutcomeFailed
	}
	return OutcomeSucceeded
}

// seconds is the only clock conversion on a record path.
func seconds(d time.Duration) float64 { return d.Seconds() }
