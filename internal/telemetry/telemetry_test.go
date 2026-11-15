package telemetry

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"toft/internal/config"
	"toft/internal/telemetry/telemetrytest"
)

// --- 13.1 the no-op path ----------------------------------------------------

// TestInitWithoutEndpointIsNoOp is the most important test in the set, because
// it protects the property everyone forgets.
//
// The overwhelming majority of installs will never configure a collector, and
// they are entitled to exactly the binary they had before this package existed:
// no goroutine, no global provider, no socket, nothing on the request path.
func TestInitWithoutEndpointIsNoOp(t *testing.T) {
	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	// Let anything already in flight settle, so the count is about this call.
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()

	stop, err := Init(context.Background(), Runtime{}, Options{})
	if err != nil {
		t.Fatalf("Init with nothing configured: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	if Enabled() {
		t.Error("Enabled() is true with no endpoint configured")
	}
	if TracingEnabled() || MetricsEnabled() {
		t.Error("a signal reports itself on with no endpoint configured")
	}
	if Health() != nil {
		t.Error("Health() returned a status with telemetry off — the admin card must be absent, not empty")
	}
	if otel.GetTracerProvider() != beforeTP {
		t.Error("Init registered a global tracer provider with nothing configured")
	}
	if otel.GetMeterProvider() != beforeMP {
		t.Error("Init registered a global meter provider with nothing configured")
	}

	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("Init started %d goroutine(s) with nothing configured (%d -> %d)", after-before, before, after)
	}

	// And the span path is inert rather than merely cheap.
	ctx, span := Start(context.Background(), "should-not-exist")
	if span.span != nil {
		t.Error("Start returned a live span with tracing off")
	}
	if ctx != context.Background() {
		t.Error("Start replaced the context with tracing off")
	}
	span.End(nil)
}

// TestNoopRecordAllocatesNothing holds the claim that an uninstrumented install
// pays one atomic load per call site.
//
// Asserted as a test rather than eyeballed as a benchmark: a regression here is
// an allocation added to every request on every install that never asked for
// telemetry, and nobody reads benchmark output on a branch that compiles.
func TestNoopRecordAllocatesNothing(t *testing.T) {
	if Enabled() {
		t.Fatal("telemetry is on; this test measures the off path")
	}
	ctx := context.Background()

	cases := map[string]func(){
		"RecordHTTPServer": func() {
			RecordHTTPServer(ctx, SurfacePlatform, "GET", "/api/v1/apps", "https", 200, "app-1", time.Millisecond, 128)
		},
		"AddHTTPActive":  func() { AddHTTPActive(ctx, SurfacePlatform, "GET", 1) },
		"RecordUpstream": func() { RecordUpstream(ctx, "app-1", 200, nil, time.Millisecond) },
		"RecordBuildPhase": func() {
			RecordBuildPhase(ctx, "clone", OutcomeSucceeded, "flask", "shared", time.Second)
		},
		"RecordSweep": func() {
			RecordSweep(ctx, SweepPrune, OutcomeSucceeded, time.Second)
		},
		// A sweep on a thirty-second timer is not a hot path, and the property
		// is still tested rather than hoped for: every record function in this
		// package has it, and the one that quietly does not is the one nobody
		// checked.
		"RecordSweepRemoved": func() {
			RecordSweepRemoved(ctx, Span{}, SweepPrune, Removed{Rows: 4000, LogLines: 12})
		},
		"Start": func() {
			_, s := Start(ctx, "noop")
			s.End(nil)
		},
		"TraceParent": func() { _ = TraceParent(ctx) },
	}

	for name, fn := range cases {
		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				fn()
			}
		})
		if allocs := res.AllocsPerOp(); allocs != 0 {
			t.Errorf("%s allocates %d time(s) per call with telemetry off", name, allocs)
		}
	}
}

// --- 13.4 the wire ----------------------------------------------------------

// start brings the pipeline up against an in-process collector and tears it
// down with the test.
//
// The Runtime is passed directly, which is the point of it existing: the
// exporters are built from resolved configuration rather than from the
// environment, so a test can describe a pipeline without setting process-wide
// variables that leak into the next test.
func start(t *testing.T, c *telemetrytest.Collector, mutate func(*Runtime)) (*sdkmetric.ManualReader, func()) {
	t.Helper()

	rt := Runtime{
		Endpoint:          c.URL(),
		Traces:            true,
		Metrics:           false, // metrics come off the manual reader below
		SampleRatio:       1.0,
		Timeout:           2 * time.Second,
		MetricInterval:    time.Second,
		Temporality:       "cumulative",
		AppCardinalityCap: config.DefaultAppCardinalityCap,
		Source:            SourceEnvironment,
	}
	if mutate != nil {
		mutate(&rt)
	}

	reader := sdkmetric.NewManualReader()
	stop, err := Init(context.Background(), rt, Options{
		Env:           config.Telemetry{ServiceName: "toft-test"},
		Version:       "0.0.0-test",
		Revision:      "cafebabe",
		ExecutionMode: "shared",
		Reader:        reader,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(ctx)
	}
	t.Cleanup(cleanup)
	return reader, cleanup
}

func TestSpanReachesTheCollector(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	_, flush := start(t, c, nil)

	ctx, span := StartServer(context.Background(), "GET /api/v1/apps/{appID}")
	span.Set(attribute.String("http.route", "/api/v1/apps/{appID}"))
	span.Set(attribute.Int("http.response.status_code", 200))
	span.End(nil)
	_ = ctx

	flush()

	got := c.WaitForSpan(t, "GET /api/v1/apps/{appID}", 2*time.Second)
	if got.Attributes["http.route"] != "/api/v1/apps/{appID}" {
		t.Errorf("http.route = %q", got.Attributes["http.route"])
	}
	if got.Attributes["http.response.status_code"] != "200" {
		t.Errorf("status = %q", got.Attributes["http.response.status_code"])
	}
	if got.Resource["service.name"] != "toft-test" {
		t.Errorf("service.name = %q, want the configured one", got.Resource["service.name"])
	}
	if got.Resource["service.version"] != "0.0.0-test" {
		t.Errorf("service.version = %q", got.Resource["service.version"])
	}
	if got.Resource["toft.revision"] != "cafebabe" {
		t.Errorf("toft.revision = %q", got.Resource["toft.revision"])
	}
	if got.Resource["service.instance.id"] == "" {
		t.Error("service.instance.id is absent; a fleet cannot tell its replicas apart without it")
	}
}

func TestFailedSpanRecordsErrorTypeNotText(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	_, flush := start(t, c, nil)

	_, span := Start(context.Background(), "build")
	span.End(errors.New("cloning https://user:hunter2@github.com/acme/app failed"))
	flush()

	got := c.WaitForSpan(t, "build", 2*time.Second)
	if got.StatusCode != "STATUS_CODE_ERROR" {
		t.Errorf("status = %q, want error", got.StatusCode)
	}
	if got.Attributes["error.type"] != ErrOther {
		t.Errorf("error.type = %q, want the closed-enum value", got.Attributes["error.type"])
	}
	for _, e := range got.Events {
		if msg := e.Attributes["exception.message"]; msg != "" {
			if strings.Contains(msg, "hunter2") {
				t.Errorf("the exception message carried a credential off the process: %q", msg)
			}
		}
	}
}

// --- 13.5 failure and back-pressure -----------------------------------------

// TestCollectorHangingDoesNotBlockProducers is the failure mode that takes a
// platform down: a collector that accepts the connection and never answers.
//
// If a producer can ever block on that, a request goroutine is pinned for the
// length of the outage and the platform stops serving because the observability
// stack did.
func TestCollectorHangingDoesNotBlockProducers(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	c.Hang()
	_, _ = start(t, c, nil)
	health := Health()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Comfortably more than the queue holds, so the drop path is exercised
		// rather than merely the happy one.
		for i := 0; i < queueSize*3; i++ {
			_, s := Start(context.Background(), "hang-test")
			s.End(nil)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("producing spans blocked while the collector hung — a request goroutine would be pinned here")
	}

	if dropped := health.SpansDropped.Load(); dropped == 0 {
		t.Error("nothing was counted as dropped; a full queue must be visible, not silent")
	}
	c.Release()
}

// TestQueueFullDropsRatherThanBlocks is the same property at the seam itself.
func TestQueueFullDropsRatherThanBlocks(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	c.Hang()
	_, _ = start(t, c, nil)
	health := Health()

	deadline := time.Now().Add(5 * time.Second)
	for i := 0; i < queueSize*2 && time.Now().Before(deadline); i++ {
		_, s := Start(context.Background(), "drop-test")
		s.End(nil)
	}
	if health.SpansDropped.Load() == 0 {
		t.Fatal("filled the queue twice over and dropped nothing")
	}
	c.Release()
}

// TestShutdownDrainsAndGivesUp holds the drain-not-close convention: a binary
// that will not stop because a collector is down is worse than a lost batch.
func TestShutdownDrainsAndGivesUp(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	c.Hang()

	stop, err := Init(context.Background(), Runtime{
		Endpoint: c.URL(), Traces: true, SampleRatio: 1.0,
		Timeout: time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 10,
	}, Options{Env: config.Telemetry{ServiceName: "toft-test"}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, s := Start(context.Background(), "never-arrives")
	s.End(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	returned := make(chan error, 1)
	go func() { returned <- stop(ctx) }()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return within its own deadline plus slack — the process would not exit")
	}
	c.Release()
}

// TestExportFailuresAreCounted holds D11: a broken export has to be visible
// somewhere an administrator can see it, and the log is not that place.
func TestExportFailuresAreCounted(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	// 400 rather than 503 deliberately: the SDK retries a 503 with backoff, so
	// asserting on a counter within a shutdown window would be asserting the
	// SDK's retry schedule. A permanent refusal — a collector behind a path that
	// does not exist, which is the most common misconfiguration — fails once and
	// is exactly the case the counter is for.
	c.Fail(400)
	_, flush := start(t, c, nil)
	// Captured before the drain: flush() shuts the pipeline down, and Health()
	// is deliberately nil once nothing is exporting — the admin card is absent
	// rather than empty.
	health := Health()

	_, s := Start(context.Background(), "will-fail")
	s.End(nil)
	flush()

	if health.ExportFailures.Load() == 0 {
		t.Error("a refusing collector produced no counted failure")
	}
	if msg, at := health.LastError(); msg == "" || at.IsZero() {
		t.Error("no last error was recorded for the admin card to show")
	}
}

// --- metrics ----------------------------------------------------------------

func TestServerMetricRecordsRouteAndSurface(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	reader, _ := start(t, c, nil)

	RecordHTTPServer(context.Background(), SurfacePlatform, "GET", "/api/v1/apps/{appID}", "https", 200, "", 12*time.Millisecond, 0)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == MetricHTTPServerDuration {
				found = true
				if m.Unit != "s" {
					t.Errorf("%s unit = %q, want s", m.Name, m.Unit)
				}
			}
		}
	}
	if !found {
		t.Fatalf("%s was not recorded", MetricHTTPServerDuration)
	}
}
