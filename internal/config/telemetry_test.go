package config

import (
	"strings"
	"testing"
	"time"
)

// TestTelemetryAbsentByDefault: with nothing set, the capability is absent
// rather than dormant — no endpoint, no scrape listener, nothing to report.
func TestTelemetryAbsentByDefault(t *testing.T) {
	clearTelemetryEnv(t)

	cfg, err := loadTelemetry()
	if err != nil {
		t.Fatalf("loadTelemetry with a clean environment: %v", err)
	}
	if cfg.Enabled() || cfg.TracesEnabled() || cfg.MetricsEnabled() || cfg.ScrapeEnabled() {
		t.Errorf("telemetry reports itself on with nothing configured: %+v", cfg)
	}
	if cfg.SamplerArg != DefaultSamplerArg {
		t.Errorf("SamplerArg = %v, want %v", cfg.SamplerArg, DefaultSamplerArg)
	}
	if cfg.AppCardinalityCap != DefaultAppCardinalityCap {
		t.Errorf("AppCardinalityCap = %d, want %d", cfg.AppCardinalityCap, DefaultAppCardinalityCap)
	}
	if cfg.RecordEnduserID {
		t.Error("enduser.id recording is on by default; it must be a deliberate choice")
	}
}

// TestPartialTelemetryConfigurationRefused walks the refusals in § Configuration.
//
// Refusing beats half-starting, which is the rule ADR §14 already applies to
// email: an export that silently goes nowhere looks exactly like an install
// nobody has instrumented, and the operator finds out during the incident the
// telemetry was for.
func TestPartialTelemetryConfigurationRefused(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "credential with no endpoint",
			env:     map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer abc"},
			wantErr: "no OTLP endpoint",
		},
		{
			name:    "per-signal traces endpoint alone",
			env:     map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"},
			wantErr: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is not supported",
		},
		{
			name: "per-signal metrics endpoint beside the base",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://collector:4318",
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://other:4318/v1/metrics",
			},
			wantErr: "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT is not supported",
		},
		{
			name: "grpc transport",
			env: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317",
				"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
			},
			wantErr: "http/protobuf only",
		},
		{
			name:    "scrape token with no listener",
			env:     map[string]string{"METRICS_TOKEN": "abc"},
			wantErr: "METRICS_ADDR is not",
		},
		{
			name:    "sampler ratio out of range",
			env:     map[string]string{"OTEL_TRACES_SAMPLER_ARG": "1.5"},
			wantErr: "ratio in [0,1]",
		},
		{
			name:    "sampler ratio not a number",
			env:     map[string]string{"OTEL_TRACES_SAMPLER_ARG": "aggressive"},
			wantErr: "ratio in [0,1]",
		},
		{
			name:    "unknown exporter",
			env:     map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_TRACES_EXPORTER": "jaeger"},
			wantErr: `"otlp" or "none"`,
		},
		{
			name:    "unknown temporality",
			env:     map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "lowmemory"},
			wantErr: `"cumulative" or "delta"`,
		},
		{
			name:    "unsupported sampler",
			env:     map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_TRACES_SAMPLER": "always_on"},
			wantErr: "is not supported",
		},
		{
			name:    "cardinality cap of zero",
			env:     map[string]string{"TOFT_METRIC_APP_CARDINALITY_CAP": "0"},
			wantErr: "positive count",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTelemetryEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := loadTelemetry()
			if err == nil {
				t.Fatalf("loadTelemetry accepted %v", tc.env)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name the problem (%q)", err, tc.wantErr)
			}
		})
	}
}

// A non-loopback scrape bind is a warning, never a refusal.
//
// Prometheus on another host is a normal topology, and refusing it would push
// operators towards putting /metrics back on the platform's origin — which is
// the one thing D4 exists to prevent.
func TestNonLoopbackScrapeBindWarnsRatherThanRefuses(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv("METRICS_ADDR", "0.0.0.0:9090")

	cfg, err := loadTelemetry()
	if err != nil {
		t.Fatalf("a non-loopback METRICS_ADDR was refused: %v", err)
	}
	warning := cfg.MetricsBindWarning()
	if warning == "" {
		t.Fatal("no warning for a token-less bind on every interface")
	}
	if !strings.Contains(warning, "METRICS_TOKEN") {
		t.Errorf("the warning does not name the fix: %q", warning)
	}

	// With a token there is nothing to warn about.
	t.Setenv("METRICS_TOKEN", "abc")
	cfg, err = loadTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.MetricsBindWarning(); w != "" {
		t.Errorf("warned about a bind that has a token: %q", w)
	}

	// Nor on loopback, where the process boundary is the control.
	t.Setenv("METRICS_ADDR", "127.0.0.1:9090")
	t.Setenv("METRICS_TOKEN", "")
	cfg, err = loadTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.MetricsBindWarning(); w != "" {
		t.Errorf("warned about a loopback bind: %q", w)
	}
}

// The scrape endpoint stands on its own: an install with Prometheus and no
// collector should not have to invent an OTLP endpoint it does not have.
func TestScrapeEndpointNeedsNoCollector(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv("METRICS_ADDR", "127.0.0.1:9090")

	cfg, err := loadTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ScrapeEnabled() {
		t.Error("METRICS_ADDR did not enable the scrape endpoint")
	}
	if cfg.Enabled() {
		t.Error("METRICS_ADDR turned OTLP export on; the two are separate decisions")
	}
}

func TestTelemetryDurationsParse(t *testing.T) {
	clearTelemetryEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "2500")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "15000")

	cfg, err := loadTelemetry()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 2500*time.Millisecond {
		t.Errorf("Timeout = %v, want 2.5s", cfg.Timeout)
	}
	if cfg.MetricExportInterval != 15*time.Second {
		t.Errorf("MetricExportInterval = %v, want 15s", cfg.MetricExportInterval)
	}
}

// clearTelemetryEnv unsets everything this loader reads, so a test is about its
// own environment rather than about whatever the shell that ran `go test`
// happened to export — which on a box where the dev stack sets OTEL_* by
// default is not a hypothetical.
func clearTelemetryEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_SERVICE_NAME", "OTEL_SERVICE_NAMESPACE",
		"OTEL_SERVICE_INSTANCE_ID", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER",
		"OTEL_METRIC_EXPORT_INTERVAL", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE",
		"OTEL_RECORD_ENDUSER_ID", "OTEL_DB_TRACE", "METRICS_ADDR", "METRICS_TOKEN",
		"PROPAGATE_TRACE_TO_APPS", "TRUST_INBOUND_TRACE_CONTEXT", "TOFT_METRIC_APP_CARDINALITY_CAP",
	} {
		t.Setenv(k, "")
	}
}

// TRUST_INBOUND_TRACE_CONTEXT is Toft's own name: off by default, on when set,
// and refused rather than read as off when it says something unreadable.
func TestTrustInboundTraceContextSwitch(t *testing.T) {
	clearTelemetryEnv(t)
	tc, err := loadTelemetry()
	if err != nil {
		t.Fatalf("loadTelemetry: %v", err)
	}
	if tc.TrustInboundTrace {
		t.Error("an inbound trace context is believed by default")
	}

	t.Setenv("TRUST_INBOUND_TRACE_CONTEXT", "true")
	if tc, err = loadTelemetry(); err != nil || !tc.TrustInboundTrace {
		t.Errorf("TRUST_INBOUND_TRACE_CONTEXT=true: on=%v err=%v", tc.TrustInboundTrace, err)
	}

	t.Setenv("TRUST_INBOUND_TRACE_CONTEXT", "perhaps")
	if _, err = loadTelemetry(); err == nil || !strings.Contains(err.Error(), "TRUST_INBOUND_TRACE_CONTEXT") {
		t.Errorf("an unreadable value was not refused: %v", err)
	}
}
