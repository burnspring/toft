package telemetry

import (
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
)

// startRuntimeMetrics registers the Go runtime instrumentation: GC, heap,
// goroutines, scheduler.
//
// Taken from contrib rather than hand-rolled, which is the opposite of the
// decision made about the HTTP middleware (ADR §31 D1) and for a coherent
// reason: the HTTP middleware's value is in *its opinions about attributes*,
// which this codebase has already decided for itself, whereas the runtime
// metrics have no attributes at all — they are a transcription of
// runtime/metrics under the names every Go dashboard in the operator's stack
// already uses.
//
// The read interval is the SDK's default. It samples runtime/metrics, which is
// cheap and lock-free; the only reason to state it here is that it is a
// decision rather than an omission.
func startRuntimeMetrics() error {
	return otelruntime.Start(otelruntime.WithMinimumReadMemStatsInterval(15 * time.Second))
}
