// Package telemetryharness starts the real telemetry pipeline for a test in
// another package, against in-memory exporters.
//
// It is separate from internal/telemetry/telemetrytest, which is the fake
// *collector*, and the split is not cosmetic: telemetry's own tests import the
// collector, so a collector package that imported telemetry back would be an
// import cycle. The collector knows only the OTLP wire format; this knows how
// to turn the pipeline on.
package telemetryharness

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"toft/internal/config"
	"toft/internal/telemetry"
)

// Harness brings the real pipeline up for a test in another package.
//
// It exists because two of the properties worth testing can only be tested
// against the *real* router: that no metric ever carries a raw path (which
// needs every route chi actually registers), and that the platform serves no
// /metrics on its own origin (which needs the platform router). Both live in
// internal/api, and neither can be asserted from inside internal/telemetry.
//
// The exporters are in-memory, so nothing here touches a socket.
type Harness struct {
	spans  *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
}

// Enable starts telemetry and stops it when the test ends.
func Enable(t *testing.T) *Harness {
	t.Helper()

	h := &Harness{
		spans:  tracetest.NewInMemoryExporter(),
		reader: sdkmetric.NewManualReader(),
	}
	rt := telemetry.Runtime{
		// An endpoint is set so Enabled() is true; nothing ever dials it,
		// because both exporters below are in memory.
		Endpoint:          "http://telemetry.test",
		Traces:            true,
		Metrics:           true,
		SampleRatio:       1.0,
		Timeout:           time.Second,
		MetricInterval:    time.Minute,
		Temporality:       "cumulative",
		AppCardinalityCap: config.DefaultAppCardinalityCap,
		Source:            telemetry.SourceEnvironment,
	}
	stop, err := telemetry.Init(context.Background(), rt, telemetry.Options{
		Env:          config.Telemetry{ServiceName: "toft-test"},
		Version:      "0.0.0-test",
		Reader:       h.reader,
		SpanExporter: h.spans,
	})
	if err != nil {
		t.Fatalf("starting telemetry for the test: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(ctx)
	})
	return h
}

// Spans drains the queue and returns everything recorded so far.
func (h *Harness) Spans(t *testing.T) []sdktrace.ReadOnlySpan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := telemetry.Flush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}
	return h.spans.GetSpans().Snapshots()
}

// Metrics collects the current values off the manual reader.
func (h *Harness) Metrics(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	return rm
}

// AttributeValues returns every distinct value of one attribute key across
// every data point of one instrument.
//
// This is what the "no raw path ever reaches a metric" test asserts against:
// the question is not "is this one value right" but "is any value in the whole
// set wrong", and only the second one catches a route that slipped through on a
// path nobody thought to request.
func (h *Harness) AttributeValues(t *testing.T, instrument, key string) []string {
	t.Helper()
	var out []string
	rm := h.Metrics(t)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != instrument {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					if v, ok := p.Attributes.Value(attribute.Key(key)); ok {
						out = append(out, v.Emit())
					}
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					if v, ok := p.Attributes.Value(attribute.Key(key)); ok {
						out = append(out, v.Emit())
					}
				}
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					if v, ok := p.Attributes.Value(attribute.Key(key)); ok {
						out = append(out, v.Emit())
					}
				}
			}
		}
	}
	return out
}
