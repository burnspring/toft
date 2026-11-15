package telemetry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Status is the pipeline's account of itself: what it has exported, what it has
// dropped, what has failed, and the last thing that went wrong.
//
// It exists because of D11. A broken export is reported in exactly one place —
// a read-only card on the admin Info page — and it is never a notification. The
// notification switches (ADR §14, §24) govern what the platform says about apps
// and about people; a collector that is down is the operator's own stack
// failing to hear from us, and adding an audience for it would mean paging
// somebody about telemetry from the system that is failing to send telemetry.
type Status struct {
	SpansExported  atomic.Int64
	SpansDropped   atomic.Int64
	ExportFailures atomic.Int64

	MetricExports  atomic.Int64
	MetricFailures atomic.Int64

	mu        sync.Mutex
	lastErr   string
	lastErrAt time.Time

	logs failureLog
}

func newStatus() *Status { return &Status{} }

// recordError keeps the most recent failure, scrubbed. It is rendered to an
// administrator, so it goes through the same scrubber a span's exception
// message does — a collector's own error text can quote the Authorization
// header it was sent with.
func (s *Status) recordError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.lastErr = scrub(err.Error())
	s.lastErrAt = time.Now().UTC()
	s.mu.Unlock()
}

// LastError returns the most recent export failure and when it happened.
func (s *Status) LastError() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr, s.lastErrAt
}

// countingMetricExporter is the metrics half of the same accounting. The
// periodic reader owns the retry and the timeout; all that is added here is the
// answer to "is it working", which nothing else in the SDK will tell an
// administrator.
type countingMetricExporter struct {
	sdkmetric.Exporter
	status *Status
}

func (e countingMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err != nil {
		e.status.MetricFailures.Add(1)
		e.status.recordError(err)
		RecordTelemetryExportFailure(SignalMetrics, ErrorType(err))
		e.status.logFailure("metrics", err)
		return err
	}
	e.status.MetricExports.Add(1)
	return nil
}

// Snapshot is what GET /api/v1/system/telemetry serves and the admin Info card
// renders. Deliberately small: whether each signal is on, where it points
// (host and port only — never the path, never the headers), and whether it is
// working.
type Snapshot struct {
	TracesEnabled  bool    `json:"tracesEnabled"`
	SamplingRatio  float64 `json:"samplingRatio"`
	MetricsEnabled bool    `json:"metricsEnabled"`
	// ExportIntervalSeconds is the metric export period.
	ExportIntervalSeconds int `json:"exportIntervalSeconds"`
	// Endpoint is host:port, with the scheme. The path and the headers are
	// never included: one is noise and the other is the operator's credential.
	Endpoint string `json:"endpoint"`

	ScrapeEnabled bool   `json:"scrapeEnabled"`
	ScrapeAddr    string `json:"scrapeAddr"`
	ScrapeToken   bool   `json:"scrapeTokenSet"`

	SpansExported  int64 `json:"spansExported"`
	SpansDropped   int64 `json:"spansDropped"`
	ExportFailures int64 `json:"exportFailures"`
	MetricExports  int64 `json:"metricExports"`
	MetricFailures int64 `json:"metricFailures"`

	LastError   string     `json:"lastError,omitempty"`
	LastErrorAt *time.Time `json:"lastErrorAt,omitempty"`

	// StartedAt lets the card say "since" rather than implying the counters are
	// an all-time figure. They reset with the process, like every other
	// in-memory counter in this codebase.
	StartedAt time.Time `json:"startedAt"`
}
