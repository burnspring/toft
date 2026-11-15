package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"toft/internal/config"
	"toft/internal/telemetry/telemetrytest"
)

// mwRouter builds a router carrying the middleware, with routes shaped like the
// real ones. The real router lives in internal/api and is walked by a test
// there; what is under test here is the middleware itself.
func mwRouter(t *testing.T, trusted func(*http.Request) bool, h http.HandlerFunc) *chi.Mux {
	t.Helper()
	r := chi.NewRouter()
	r.Use(Middleware(SurfacePlatform, trusted))
	r.Get("/api/v1/apps/{appID}", h)
	r.Get("/apps/{slug}/*", h)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	return r
}

func TestMiddlewareRecordsRoutePatternNotPath(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	_, flush := start(t, c, nil)

	r := mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest("GET", "/api/v1/apps/8b0f6a5e-dead-beef-0000-000000000000", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	flush()

	span := c.WaitForSpan(t, "GET /api/v1/apps/{appID}", 2*time.Second)
	if got := span.Attributes["http.route"]; got != "/api/v1/apps/{appID}" {
		t.Errorf("http.route = %q, want the pattern", got)
	}
	if strings.Contains(span.Name, "8b0f6a5e") {
		t.Errorf("the span name carried the path parameter: %q", span.Name)
	}
}

// A request that matched nothing folds to __unmatched__ — one scanner walking
// /wp-admin/… for an hour must not be tens of thousands of series.
func TestUnmatchedRequestFoldsButKeepsThePathOnTheSpan(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	_, flush := start(t, c, nil)

	r := mwRouter(t, nil, nil)
	req := httptest.NewRequest("GET", "/wp-admin/setup-config.php", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	flush()

	span := c.WaitForSpan(t, "GET "+UnmatchedRoute, 2*time.Second)
	if span.Attributes["http.route"] != UnmatchedRoute {
		t.Errorf("http.route = %q, want %q", span.Attributes["http.route"], UnmatchedRoute)
	}
	// The path survives on the *span*, where it costs nothing and answers the
	// only question worth asking about an unmatched request.
	if span.Attributes["url.path"] != "/wp-admin/setup-config.php" {
		t.Errorf("url.path = %q, want the raw path on the span", span.Attributes["url.path"])
	}
}

// TestSpanEndsAfterTheResponse is the D2 property, and the reason it is a test
// rather than a comment is that "we were careful" does not hold it: an
// instrumentation that ends its span before the last byte is written measures
// something other than what the visitor waited for.
//
// The assertion is against the SDK's own end time, not the collector's arrival
// time. The batch schedule decides the latter, and asserting on it would be
// asserting the scheduler.
func TestSpanEndsAfterTheResponse(t *testing.T) {
	exp := &recordingExporter{}
	stop := startWithExporter(t, exp)

	var writtenAt time.Time
	r := mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
		// Stamped by the handler itself, after the body. Whatever the span's end
		// time turns out to be, it has to be at least this.
		writtenAt = time.Now()
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/apps/x", nil))
	stop()

	spans := exp.taken()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want exactly one server span", len(spans))
	}
	if spans[0].EndTime().Before(writtenAt) {
		t.Errorf("the span ended %s before the response was written; the duration it "+
			"reports is not the one the visitor experienced",
			writtenAt.Sub(spans[0].EndTime()))
	}
}

// startWithExporter runs the pipeline against an in-memory exporter and returns
// a drain. Used where the assertion is about span *timing*, which the OTLP
// round trip would only obscure.
func startWithExporter(t *testing.T, exp sdktrace.SpanExporter) func() {
	t.Helper()
	stop, err := Init(context.Background(), Runtime{
		Endpoint: "http://telemetry.test", Traces: true, SampleRatio: 1.0,
		Timeout: time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}, Options{
		Env:          config.Telemetry{ServiceName: "toft-test"},
		SpanExporter: exp,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	drain := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(ctx)
	}
	t.Cleanup(drain)
	return drain
}

type recordingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *recordingExporter) ExportSpans(_ context.Context, s []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, s...)
	return nil
}
func (e *recordingExporter) Shutdown(context.Context) error { return nil }

func (e *recordingExporter) taken() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}

// TestTraceparentBelievedOnlyFromATrustedPeer is ADR §31 D6.
//
// Parent-based sampling means a caller that sets the sampled bit decides that
// Toft records the trace. Inside a mesh that is correct; at a public edge
// it is a lever on the operator's bill, and this platform's edge is public.
func TestTraceparentBelievedOnlyFromATrustedPeer(t *testing.T) {
	const inbound = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	const inboundTrace = "4bf92f3577b34da6a3ce929d0e0e4736"

	t.Run("trusted peer: the parent is honoured", func(t *testing.T) {
		c := telemetrytest.NewCollector(t)
		_, flush := start(t, c, nil)

		r := mwRouter(t, func(*http.Request) bool { return true },
			func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		req := httptest.NewRequest("GET", "/api/v1/apps/x", nil)
		req.Header.Set("traceparent", inbound)
		r.ServeHTTP(httptest.NewRecorder(), req)
		flush()

		span := c.WaitForSpan(t, "GET /api/v1/apps/{appID}", 2*time.Second)
		if span.TraceID != inboundTrace {
			t.Errorf("trace id = %q, want the caller's %q", span.TraceID, inboundTrace)
		}
	})

	t.Run("untrusted peer: a fresh root", func(t *testing.T) {
		c := telemetrytest.NewCollector(t)
		_, flush := start(t, c, nil)

		// nil is the "no trusted networks configured" case, which is also what
		// TRUSTED_PROXY_CIDRS= (present but empty) produces: trust nothing.
		r := mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		req := httptest.NewRequest("GET", "/api/v1/apps/x", nil)
		req.Header.Set("traceparent", inbound)
		r.ServeHTTP(httptest.NewRecorder(), req)
		flush()

		span := c.WaitForSpan(t, "GET /api/v1/apps/{appID}", 2*time.Second)
		if span.TraceID == inboundTrace {
			t.Error("a stranger's trace id was adopted; they can now choose what the operator's collector is billed for")
		}
		if span.ParentID != "" && span.ParentID != "0000000000000000" {
			t.Errorf("the span has a parent (%q) from an untrusted caller", span.ParentID)
		}
	})
}

// The surface is what was *served*, not which listener took the call — the
// distinction that keeps somebody's Flask app out of the platform's own error
// rate on a single-origin install (ADR §15).
func TestSurfaceFollowsTheResolvedApp(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	_, flush := start(t, c, nil)

	r := mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
		SetRequestApp(r.Context(), "8b0f6a5e-0000-0000-0000-000000000000")
		w.WriteHeader(http.StatusBadGateway)
	})
	req := httptest.NewRequest("GET", "/apps/reports/index.html", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	flush()

	span := c.WaitForSpan(t, "GET /apps/{slug}/*", 2*time.Second)
	if span.Attributes["toft.surface"] != SurfaceApp {
		t.Errorf("surface = %q, want %q — an app's traffic recorded as the platform's "+
			"puts it inside the number an operator pages on",
			span.Attributes["toft.surface"], SurfaceApp)
	}
	if span.Attributes["toft.app.id"] == "" {
		t.Error("the app id was not recorded on a gateway span")
	}
	if span.Attributes["error.type"] != ErrHTTPStatus {
		t.Errorf("error.type = %q, want a 502 to count as an error", span.Attributes["error.type"])
	}
}

// The producer half of D7. The span is started before any authentication
// middleware runs, so who the request was *for* can only arrive from inside the
// chain — and whether it then reaches the wire is the redaction stage's call,
// never the call site's. Both directions matter: an install that did not ask
// for this must not export it, and one that did must actually get it.
func TestEnduserIDReachesTheSpanOnlyWhenAskedFor(t *testing.T) {
	const userID = "8b0f6a5e-0000-0000-0000-000000000000"

	shoot := func(t *testing.T, on bool) telemetrytest.Span {
		t.Helper()
		c := telemetrytest.NewCollector(t)
		_, flush := start(t, c, func(rt *Runtime) { rt.RecordEnduserID = on })

		r := mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
			SetRequestEnduser(r.Context(), userID)
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest("GET", "/api/v1/apps/8b0f6a5e-dead-beef-0000-000000000000", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
		flush()
		return c.WaitForSpan(t, "GET /api/v1/apps/{appID}", 2*time.Second)
	}

	t.Run("off by default", func(t *testing.T) {
		span := shoot(t, false)
		if got := span.Attributes[string(enduserIDKey)]; got != "" {
			t.Errorf("enduser.id = %q reached the collector with the flag off", got)
		}
		// The rest of the span is unaffected — this is a filter on one
		// attribute, not a reason to lose the span.
		if span.Attributes["http.route"] != "/api/v1/apps/{appID}" {
			t.Errorf("http.route = %q", span.Attributes["http.route"])
		}
	})

	t.Run("on when the operator asks", func(t *testing.T) {
		span := shoot(t, true)
		if got := span.Attributes[string(enduserIDKey)]; got != userID {
			t.Errorf("enduser.id = %q, want %q", got, userID)
		}
	})
}

// A WebSocket upgrade writes its 101 straight to a hijacked socket. A recorder
// that only sees WriteHeader reports 0, and one that then calls 0 "200" puts a
// lie in the metric.
func TestHijackedConnectionRecordsSwitchingProtocols(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	_, flush := start(t, c, nil)

	r := mwRouter(t, nil, func(w http.ResponseWriter, req *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the telemetry recorder hid http.Hijacker — every WebSocket on the install would break")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
		conn.Close()
	})

	srv := httptest.NewServer(r)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/apps/x")
	if err == nil {
		resp.Body.Close()
	}
	flush()

	span := c.WaitForSpan(t, "GET /api/v1/apps/{appID}", 2*time.Second)
	if span.Attributes["http.response.status_code"] != "101" {
		t.Errorf("status = %q, want 101 for an upgraded connection",
			span.Attributes["http.response.status_code"])
	}
}

// The recorder must not hide http.Flusher either, or SSE and log streaming stop
// working — a failure that no test making ordinary requests would ever catch.
func TestRecorderKeepsFlusher(t *testing.T) {
	var sawFlusher bool
	h := Middleware(SurfacePlatform, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawFlusher = w.(http.Flusher)
	}))
	// With telemetry off the middleware passes the writer straight through, so
	// give it a pipeline for the duration of the assertion.
	live.Store(&pipeline{status: newStatus()})
	defer live.Store(nil)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !sawFlusher {
		t.Error("the telemetry recorder is not an http.Flusher")
	}
}

// startInMemory runs the pipeline with both signals in memory: spans into a
// recording exporter, metrics off a manual reader. Used where the assertion is
// about what one request recorded on both, with no wire in between.
func startInMemory(t *testing.T) (*sdkmetric.ManualReader, *recordingExporter, func()) {
	t.Helper()
	exp := &recordingExporter{}
	reader := sdkmetric.NewManualReader()
	stop, err := Init(context.Background(), Runtime{
		Endpoint: "http://telemetry.test", Traces: true, SampleRatio: 1.0,
		Timeout: time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}, Options{
		Env:          config.Telemetry{ServiceName: "toft-test"},
		SpanExporter: exp,
		Reader:       reader,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	drain := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(ctx)
	}
	t.Cleanup(drain)
	return reader, exp, drain
}

// histogramPoints returns the data points of one float histogram.
func histogramPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				out = append(out, h.DataPoints...)
			}
		}
	}
	return out
}

// sumOf adds every data point of one int64 sum (the in-flight gauge).
func sumOf(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, p := range s.DataPoints {
					total += p.Value
					found = true
				}
			}
		}
	}
	return total, found
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.Emit(), true
		}
	}
	return "", false
}

// TL-1: an absolute-form request line puts any scheme the client likes on
// r.URL, and net/http serves it. The metric declares two values.
func TestSchemeIsNeverTakenFromTheRequestLine(t *testing.T) {
	reader, exp, drain := startInMemory(t)

	r := mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest("GET", "/api/v1/apps/x", nil)
	req.URL.Scheme = "x1"
	req.URL.Host = "h"
	r.ServeHTTP(httptest.NewRecorder(), req)

	points := histogramPoints(t, reader, "http.server.request.duration")
	if len(points) == 0 {
		t.Fatal("no http.server.request.duration point recorded")
	}
	for _, p := range points {
		if v, _ := p.Attributes.Value("url.scheme"); v.AsString() != "http" {
			t.Errorf("metric url.scheme = %q, want http", v.AsString())
		}
	}
	drain()
	spans := exp.taken()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if v, _ := spanAttr(spans[0], "url.scheme"); v != "http" {
		t.Errorf("span url.scheme = %q, want http", v)
	}
}

// TestPanickingHandlerIsStillRecorded is TL-3. ReverseProxy ends a response
// the client abandoned with panic(http.ErrAbortHandler), and any other panic
// is turned into a 500 by chi's Recoverer, outside this middleware. Neither
// may leave the in-flight gauge raised or the span unended.
func TestPanickingHandlerIsStillRecorded(t *testing.T) {
	reader, exp, drain := startInMemory(t)

	serve := func(h http.HandlerFunc, path string) {
		defer func() { _ = recover() }()
		mwRouter(t, nil, h).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	serve(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic(http.ErrAbortHandler)
	}, "/api/v1/apps/aborted")
	serve(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}, "/api/v1/apps/panicked")

	if n, _ := sumOf(t, reader, MetricHTTPServerActive); n != 0 {
		t.Errorf("http.server.active_requests = %d after both requests ended, want 0", n)
	}
	var count uint64
	statuses := map[int64]uint64{}
	for _, p := range histogramPoints(t, reader, MetricHTTPServerDuration) {
		count += p.Count
		v, _ := p.Attributes.Value("http.response.status_code")
		statuses[v.AsInt64()] += p.Count
	}
	if count != 2 {
		t.Errorf("the duration histogram counted %d requests, want 2", count)
	}
	if statuses[200] != 1 || statuses[500] != 1 {
		t.Errorf("statuses = %v, want one 200 (latched before the abort) and one 500", statuses)
	}
	drain()
	if spans := exp.taken(); len(spans) != 2 {
		t.Errorf("got %d ended spans, want 2", len(spans))
	}
}

// A settings save while a request is in flight swaps in a pipeline that never
// saw the increment; the decrement must not land on it.
func TestInFlightGaugeSurvivesASwap(t *testing.T) {
	_, _, _ = startInMemory(t)

	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
		}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/apps/x", nil))
	}()
	<-entered

	// A ManualReader belongs to one meter provider, so the new pipeline
	// brings its own.
	reader2 := sdkmetric.NewManualReader()
	mu.Lock()
	base.Reader = reader2
	mu.Unlock()
	if err := Apply(context.Background(), Runtime{
		Endpoint: "http://telemetry.test", Traces: true, SampleRatio: 1.0,
		Timeout: time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	close(release)
	<-done

	if n, _ := sumOf(t, reader2, MetricHTTPServerActive); n < 0 {
		t.Errorf("the new pipeline's in-flight gauge is %d; it never saw the increment", n)
	}
}

// TestInterimResponseIsNotTheStatus is TL-7: a 103 Early Hints relayed from an
// app before a 500 is a 500, inside the error rate.
func TestInterimResponseIsNotTheStatus(t *testing.T) {
	reader, _, _ := startInMemory(t)
	mwRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusInternalServerError)
	}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/apps/x", nil))

	points := histogramPoints(t, reader, MetricHTTPServerDuration)
	if len(points) != 1 {
		t.Fatalf("got %d points, want 1", len(points))
	}
	if v, _ := points[0].Attributes.Value("http.response.status_code"); v.AsInt64() != 500 {
		t.Errorf("status = %d, want 500 — the interim 103 is not the answer", v.AsInt64())
	}
}
