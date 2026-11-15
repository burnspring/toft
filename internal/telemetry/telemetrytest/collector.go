// Package telemetrytest is an in-process OTLP/HTTP collector for tests.
//
// It is the workhorse of the telemetry test strategy, and it exists because the
// alternatives are both bad. Asserting against the SDK's in-memory exporter
// skips the wire entirely — which is where the redaction stage, the protobuf
// encoding and the endpoint configuration live, and those are three of the
// things most worth testing. Asserting against a real collector in Docker means
// tests that do not run in CI, do not run on a laptop, and are not
// deterministic.
//
// So: an httptest.Server that speaks the protocol, decodes the payload, and
// hands the result back as plain Go structs. No Docker, no ports to collide
// over, no sleep-and-hope.
//
// It also plays the two failure roles the "never load-bearing" claim has to be
// tested against: a collector that refuses, and — much worse — a collector that
// accepts a connection and never answers.
package telemetrytest

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	colltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Span is one decoded span, flattened to what an assertion actually wants.
type Span struct {
	Name       string
	TraceID    string
	SpanID     string
	ParentID   string
	Kind       string
	StatusCode string
	Attributes map[string]string
	Events     []Event
	// Resource is the resource the span arrived with, so a test can assert
	// service.name and service.version without a second mechanism.
	Resource map[string]string
}

// Event is one span event, which is where an exception's type and message live.
type Event struct {
	Name       string
	Attributes map[string]string
}

// Metric is one decoded metric.
type Metric struct {
	Name string
	Unit string
	// Points is one entry per attribute set, each carrying the attributes and,
	// for a histogram, the count. Enough to assert "this series exists with
	// these labels", which is the only thing worth asserting about a metric in
	// a unit test — the value is the SDK's arithmetic, not ours.
	Points []Point
}

// Point is one data point's attribute set and count.
type Point struct {
	Attributes map[string]string
	Count      uint64
	Sum        float64
}

// Collector is the fake. Safe for concurrent use; every accessor copies.
type Collector struct {
	srv *httptest.Server

	mu      much
	spans   []Span
	metrics []Metric

	hang   chan struct{}
	failMu sync.Mutex
	fail   int
	// requests counts every accepted POST, failed ones included — the figure
	// TestExportFailuresAreCountedNotLogged needs to know retries happened.
	requests int
	// headers is the request headers of every POST, in arrival order.
	headers []http.Header
}

// much is a plain mutex under a name that makes the embedded field read as
// `c.mu.Lock()` rather than promoting Lock onto the Collector itself, which
// would make the whole struct look like a lock to anything holding one.
type much struct{ sync.Mutex }

// NewCollector starts a collector and stops it when the test ends.
func NewCollector(t *testing.T) *Collector {
	t.Helper()
	// hang starts nil: a collector that blocked by default would make every
	// test look like a hung-collector test.
	c := &Collector{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", c.handleTraces)
	mux.HandleFunc("/v1/metrics", c.handleMetrics)
	c.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		c.Release()
		c.srv.Close()
	})
	return c
}

// URL is what OTEL_EXPORTER_OTLP_ENDPOINT should be set to.
func (c *Collector) URL() string { return c.srv.URL }

// Hang makes every subsequent request accept and never answer, until Release.
//
// This is the failure mode that actually takes a platform down: a refused
// connection fails fast and is boring, but a collector that holds the socket
// open pins whatever goroutine is waiting on it. If a request goroutine can
// ever be that goroutine, the platform stops serving when the observability
// stack does.
func (c *Collector) Hang() {
	c.failMu.Lock()
	defer c.failMu.Unlock()
	if c.hang == nil {
		c.hang = make(chan struct{})
	}
}

// Release lets hung requests finish.
func (c *Collector) Release() {
	c.failMu.Lock()
	defer c.failMu.Unlock()
	if c.hang != nil {
		select {
		case <-c.hang:
		default:
			close(c.hang)
		}
	}
}

// Fail makes every subsequent request answer with this status. 0 restores
// normal service.
func (c *Collector) Fail(status int) {
	c.failMu.Lock()
	c.fail = status
	c.failMu.Unlock()
}

// Spans returns everything received so far.
func (c *Collector) Spans() []Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Span(nil), c.spans...)
}

// Metrics returns every metric received so far.
func (c *Collector) Metrics() []Metric {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Metric(nil), c.metrics...)
}

// Requests is the number of export POSTs accepted, failures included.
func (c *Collector) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

// RequestHeaders returns the headers of every request received so far.
func (c *Collector) RequestHeaders() []http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]http.Header, len(c.headers))
	copy(out, c.headers)
	return out
}

// SpanNamed returns the first span with this name, and whether there was one.
func (c *Collector) SpanNamed(name string) (Span, bool) {
	for _, s := range c.Spans() {
		if s.Name == name {
			return s, true
		}
	}
	return Span{}, false
}

// MetricNamed returns the first metric with this name.
func (c *Collector) MetricNamed(name string) (Metric, bool) {
	for _, m := range c.Metrics() {
		if m.Name == name {
			return m, true
		}
	}
	return Metric{}, false
}

// WaitForSpan polls until a span with this name arrives or the deadline passes.
// Polling rather than a channel because the batch processor decides when to
// send, and a test that assumed otherwise would be asserting the scheduler.
func (c *Collector) WaitForSpan(t *testing.T, name string, within time.Duration) Span {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s, ok := c.SpanNamed(name); ok {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no span named %q within %s (got %d spans: %v)", name, within, len(c.Spans()), spanNames(c.Spans()))
	return Span{}
}

func spanNames(spans []Span) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}

func (c *Collector) intercept(w http.ResponseWriter, r *http.Request) bool {
	c.mu.Lock()
	c.requests++
	c.headers = append(c.headers, r.Header.Clone())
	c.mu.Unlock()

	c.failMu.Lock()
	hang, fail := c.hang, c.fail
	c.failMu.Unlock()

	if hang != nil {
		select {
		case <-hang:
		case <-r.Context().Done():
			return true
		}
	}
	if fail != 0 {
		http.Error(w, "collector refused", fail)
		return true
	}
	return false
}

func (c *Collector) handleTraces(w http.ResponseWriter, r *http.Request) {
	if c.intercept(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req colltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var decoded []Span
	for _, rs := range req.ResourceSpans {
		res := attrMap(rs.GetResource().GetAttributes())
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				decoded = append(decoded, decodeSpan(s, res))
			}
		}
	}
	c.mu.Lock()
	c.spans = append(c.spans, decoded...)
	c.mu.Unlock()

	writeProto(w, &colltracepb.ExportTraceServiceResponse{})
}

func (c *Collector) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if c.intercept(w, r) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req collmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var decoded []Metric
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				decoded = append(decoded, decodeMetric(m))
			}
		}
	}
	c.mu.Lock()
	c.metrics = append(c.metrics, decoded...)
	c.mu.Unlock()

	writeProto(w, &collmetricspb.ExportMetricsServiceResponse{})
}

func readBody(r *http.Request) ([]byte, error) {
	// The exporter gzips by default, and a collector that only understood
	// identity encoding would silently pass every test while failing against
	// anything real.
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	}
	return io.ReadAll(r.Body)
}

func writeProto(w http.ResponseWriter, m proto.Message) {
	b, err := proto.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(b)
}

func decodeSpan(s *tracepb.Span, res map[string]string) Span {
	out := Span{
		Name:       s.Name,
		TraceID:    hex(s.TraceId),
		SpanID:     hex(s.SpanId),
		ParentID:   hex(s.ParentSpanId),
		Kind:       s.Kind.String(),
		StatusCode: s.GetStatus().GetCode().String(),
		Attributes: attrMap(s.Attributes),
		Resource:   res,
	}
	for _, e := range s.Events {
		out.Events = append(out.Events, Event{Name: e.Name, Attributes: attrMap(e.Attributes)})
	}
	return out
}

func decodeMetric(m *metricspb.Metric) Metric {
	out := Metric{Name: m.Name, Unit: m.Unit}
	switch d := m.Data.(type) {
	case *metricspb.Metric_Sum:
		for _, p := range d.Sum.DataPoints {
			out.Points = append(out.Points, Point{
				Attributes: attrMap(p.Attributes),
				Sum:        numberValue(p),
			})
		}
	case *metricspb.Metric_Gauge:
		for _, p := range d.Gauge.DataPoints {
			out.Points = append(out.Points, Point{
				Attributes: attrMap(p.Attributes),
				Sum:        numberValue(p),
			})
		}
	case *metricspb.Metric_Histogram:
		for _, p := range d.Histogram.DataPoints {
			out.Points = append(out.Points, Point{
				Attributes: attrMap(p.Attributes),
				Count:      p.GetCount(),
				Sum:        p.GetSum(),
			})
		}
	}
	return out
}

func numberValue(p *metricspb.NumberDataPoint) float64 {
	switch v := p.Value.(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	}
	return 0
}

func attrMap(attrs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		out[kv.Key] = renderValue(kv.Value)
	}
	return out
}

func renderValue(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return itoa(x.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return ftoa(x.DoubleValue)
	}
	return ""
}
