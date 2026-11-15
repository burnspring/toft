package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

// TestSpanName is what the test export puts in the operator's trace backend.
//
// It is a real span with a real name rather than an empty batch, and the
// distinction is not cosmetic: the OTLP exporters return nil without sending
// anything at all when handed nothing to send, so an "empty batch" test would
// report every collector as healthy — including a hostname that does not
// resolve. That was the first version of this function and it was worse than
// having no button.
//
// The name is deliberately recognisable. An administrator who presses the
// button and then goes looking in Jaeger should find exactly this and know it
// was them.
const TestSpanName = "toft.telemetry.test"

// TestExport proves the configured collector accepts an export, synchronously,
// and returns its own error.
//
// This is the one export in the whole package that blocks its caller, and the
// exception is deliberate: an administrator has pressed a button and is waiting
// for exactly this answer. Everywhere else, blocking on a collector is the
// failure mode the design exists to prevent.
//
// It builds its own short-lived exporter rather than borrowing the running
// pipeline's, because that pipeline is asynchronous by construction: pushing a
// span through it would tell the caller only that a queue accepted it, which it
// always does.
//
// It uses the *saved* configuration rather than a submitted form, for the reason
// mail's SendTest does — what it proves is that the configuration this install
// is actually running works.
func TestExport(ctx context.Context) error {
	p := live.Load()
	if p == nil {
		return errors.New("telemetry is not switched on, so there is nothing to test")
	}
	rt := p.runtime
	if !rt.Enabled() {
		return errors.New("no collector endpoint is configured")
	}

	// Traces when they are on: /v1/traces is the path most often typed wrong,
	// and a collector configured for metrics only is a real and confusing
	// misconfiguration that this catches. Metrics otherwise, so an install
	// exporting only metrics can still test the endpoint it actually uses.
	if rt.Traces {
		return testTraces(ctx, rt)
	}
	return testMetrics(ctx, rt)
}

func testTraces(ctx context.Context, rt Runtime) error {
	exp, err := otlptracehttp.New(ctx, traceExporterOptions(rt)...)
	if err != nil {
		return fmt.Errorf("building an exporter: %w", scrubErr(err))
	}
	defer shutdownQuietly(exp.Shutdown)

	now := time.Now()
	stub := tracetest.SpanStub{
		Name: TestSpanName,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{0x0f, 0xa1, 0x15, 0xe7, 0x7e, 0x57},
			SpanID:     trace.SpanID{0x0f, 0xa1, 0x15, 0xe7},
			TraceFlags: trace.FlagsSampled,
		}),
		SpanKind:  trace.SpanKindInternal,
		StartTime: now,
		EndTime:   now,
		Attributes: []attribute.KeyValue{
			// On the allow-list, and true: this span carries an outcome and
			// nothing else. It goes out through the same exporter as everything
			// else but *not* through the redaction stage, because it is
			// constructed here rather than by a call site — so it must not carry
			// anything that would need redacting.
			attribute.String("toft.outcome", OutcomeSucceeded),
		},
		Resource: resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(base.Env.ServiceName),
		),
		InstrumentationScope: instrumentation.Scope{Name: "toft"},
	}

	if err := exp.ExportSpans(ctx, []sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		return scrubErr(err)
	}
	return nil
}

func testMetrics(ctx context.Context, rt Runtime) error {
	exp, err := otlpmetrichttp.New(ctx, metricExporterOptions(rt)...)
	if err != nil {
		return fmt.Errorf("building an exporter: %w", scrubErr(err))
	}
	defer shutdownQuietly(exp.Shutdown)

	now := time.Now()
	rm := metricdata.ResourceMetrics{
		Resource: resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(base.Env.ServiceName),
		),
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Scope: instrumentation.Scope{Name: "toft"},
			Metrics: []metricdata.Metrics{{
				// Not one of the declared instruments, deliberately: a test
				// press must not move a number an operator has an alert on.
				Name:        "toft.telemetry.test",
				Description: "Emitted once each time an administrator tests the export",
				Unit:        "{test}",
				Data: metricdata.Sum[int64]{
					Temporality: metricdata.CumulativeTemporality,
					IsMonotonic: true,
					DataPoints: []metricdata.DataPoint[int64]{{
						StartTime: now, Time: now, Value: 1,
					}},
				},
			}},
		}},
	}

	if err := exp.Export(ctx, &rm); err != nil {
		return scrubErr(err)
	}
	return nil
}

// shutdownQuietly closes a short-lived exporter. Its error is discarded on
// purpose: the caller already has the answer they asked for, and a shutdown
// failure after a successful export is not a failed test.
func shutdownQuietly(fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = fn(ctx)
}

// scrubErr puts the collector's own words through the same scrubber a span's
// exception message goes through.
//
// A collector's error text can quote the request that produced it, headers
// included — which is how an administrator debugging a 401 ends up with the
// bearer token on their screen and, worse, in the screenshot they paste into a
// ticket.
func scrubErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(scrub(err.Error()))
}
