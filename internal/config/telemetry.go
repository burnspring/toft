package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Telemetry is the platform's own observability configuration: OTLP export of
// traces and metrics, and an optional Prometheus scrape endpoint.
//
// It is *deployment* configuration rather than an audited setting, which
// inverts the decision email settings made (ADR §14) and is argued in ADR §31.
// The short form: a collector address is a fact about how the install was stood
// up, in the same class as DATABASE_URL; the person who fixes a broken export
// is the operator who owns the collector and is already editing a unit file;
// and the variable names below are an industry standard that every runbook and
// every sidecar injector already knows. Inventing TELEMETRY_ENDPOINT to match
// the local ARTIFACT_* style would be a self-inflicted surprise.
//
// The consequence accepted with it: changing any of this needs a restart, and
// audit_log records none of it, because nobody changed it from inside the
// product.
type Telemetry struct {
	// Endpoint is the master switch. Unset means the capability is absent: no
	// SDK, no exporter goroutine, no instrumentation on the request path, no
	// scrape listener, nothing in the UI.
	Endpoint string

	// Protocol is http/protobuf and nothing else. grpc is refused at startup
	// rather than silently ignored — see ADR §31 D1.
	Protocol string

	// Headers carries the collector credential (OTEL_EXPORTER_OTLP_HEADERS,
	// `key=value,key=value`). Treated as a secret everywhere: never logged,
	// never returned by an API, and never reachable by a child process.
	Headers string
	// Certificate is a CA bundle path for a collector behind a private CA.
	Certificate string
	Timeout     time.Duration

	ServiceName       string
	ServiceNamespace  string
	ServiceInstanceID string
	// ResourceAttributes is OTEL_RESOURCE_ATTRIBUTES verbatim, merged over the
	// resource the platform builds for itself so an operator can add team= or
	// region= without a code change.
	ResourceAttributes string

	// SamplerArg is the head sampling ratio. Parent-based, so a trusted caller's
	// decision is honoured — see PropagateFrom in internal/telemetry for why
	// "trusted" is the same trusted-proxy rule as X-Forwarded-*.
	SamplerArg float64

	// TracesExporter and MetricsExporter are "otlp" or "none", so one signal can
	// be switched off without giving up the other.
	TracesExporter  string
	MetricsExporter string

	MetricExportInterval time.Duration
	// Temporality is "cumulative" (default, what Prometheus-shaped backends
	// want) or "delta" (what a Datadog-shaped one wants).
	Temporality string

	// RecordEnduserID puts the signed-in user's id on *spans* — never on a
	// metric, at any setting. Off by default: it is a UUID rather than a name,
	// but a trace backend is not covered by this platform's authorization model
	// and an operator should have to choose. ADR §31 D7.
	RecordEnduserID bool
	// DBTrace opens a span per database query. A debugging tool, not production
	// instrumentation: the pool gauges below are what an install runs with.
	DBTrace bool

	// MetricsAddr is the scrape listener — the third listener, unset by default
	// and loopback by convention. It cannot be a path on :8080: a single-origin
	// install (ADR §15) runs deployed apps on the platform's origin, so any
	// authorization in front of /metrics is passed by an app's own front-end,
	// because it *is* the viewer. ADR §31 D4, the same argument that gave
	// Headlamp a hostname of its own (ADR §19).
	MetricsAddr string
	// MetricsToken is compared with subtle.ConstantTimeCompare against a Bearer
	// header. Optional, because on a loopback bind the process boundary is the
	// control; mandatory in practice off loopback, where the startup warning is
	// otherwise the only thing standing there.
	MetricsToken string

	// PropagateToApps sends traceparent upstream to a deployed app. Off by
	// default: an app is not part of the platform's trace, and the header is a
	// free channel into somebody else's tooling.
	PropagateToApps bool

	// TrustInboundTrace believes an inbound traceparent/tracestate/baggage from
	// a peer inside TRUSTED_PROXY_CIDRS (TRUST_INBOUND_TRACE_CONTEXT). Off by
	// default: being relayed by a trusted proxy does not make a header the
	// proxy's own — nginx, Caddy, ALB and the rest forward traceparent
	// verbatim — so the operator who turns this on is asserting that their
	// edge strips or re-issues it. ADR §31.
	TrustInboundTrace bool

	// AppCardinalityCap bounds the per-app metric dimension. The first N app ids
	// seen get their own series and the rest fold into __other__, counted rather
	// than dropped silently. ADR §31 D5.
	AppCardinalityCap int
}

// Enabled reports whether telemetry was configured at all. Everything about the
// capability hangs off this one answer: false means absent, not dormant.
func (t Telemetry) Enabled() bool {
	return t.Endpoint != ""
}

// TracesEnabled and MetricsEnabled report whether each signal is exported.
func (t Telemetry) TracesEnabled() bool {
	return t.Enabled() && t.TracesExporter != "none"
}

func (t Telemetry) MetricsEnabled() bool {
	return t.Enabled() && t.MetricsExporter != "none"
}

// ScrapeEnabled reports whether the third listener runs. Deliberately
// independent of Enabled: an install with Prometheus and no collector is the
// common on-prem shape, and it should not have to configure an OTLP endpoint it
// does not have in order to be scraped.
func (t Telemetry) ScrapeEnabled() bool { return t.MetricsAddr != "" }

// DefaultSamplerArg is the head sampling ratio when none is given. Low enough
// that a busy install does not pay for a trace of every request, and the errors
// and slow requests that actually get looked at are recorded regardless.
const DefaultSamplerArg = 0.05

// DefaultAppCardinalityCap bounds the per-app metric dimension by default. See
// ADR §31 D5 for the budget this number is part of.
const DefaultAppCardinalityCap = 500

// loadTelemetry reads the OTEL_* and METRICS_* environment.
//
// It refuses a half-configured pipeline rather than half-starting one, which is
// the rule ADR §14 already applies to email: a credential with no endpoint, a
// token with no listener, or a transport this build does not speak are all
// mistakes that would otherwise look like a working install exporting nothing.
func loadTelemetry() (Telemetry, error) {
	t := Telemetry{
		Endpoint:             strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")),
		Protocol:             strings.ToLower(getEnv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")),
		Headers:              os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"),
		Certificate:          getEnv("OTEL_EXPORTER_OTLP_CERTIFICATE", ""),
		ServiceName:          getEnv("OTEL_SERVICE_NAME", "toft"),
		ServiceNamespace:     getEnv("OTEL_SERVICE_NAMESPACE", ""),
		ServiceInstanceID:    getEnv("OTEL_SERVICE_INSTANCE_ID", ""),
		ResourceAttributes:   getEnv("OTEL_RESOURCE_ATTRIBUTES", ""),
		TracesExporter:       strings.ToLower(getEnv("OTEL_TRACES_EXPORTER", "otlp")),
		MetricsExporter:      strings.ToLower(getEnv("OTEL_METRICS_EXPORTER", "otlp")),
		Temporality:          strings.ToLower(getEnv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "cumulative")),
		RecordEnduserID:      isTrue(os.Getenv("OTEL_RECORD_ENDUSER_ID")),
		DBTrace:              isTrue(os.Getenv("OTEL_DB_TRACE")),
		MetricsAddr:          strings.TrimSpace(getEnv("METRICS_ADDR", "")),
		MetricsToken:         getEnv("METRICS_TOKEN", ""),
		PropagateToApps:      isTrue(os.Getenv("PROPAGATE_TRACE_TO_APPS")),
		Timeout:              10 * time.Second,
		MetricExportInterval: 60 * time.Second,
		SamplerArg:           DefaultSamplerArg,
		AppCardinalityCap:    DefaultAppCardinalityCap,
	}

	// The per-signal endpoints are refused rather than honoured. The exporters
	// are built from one base URL with /v1/traces and /v1/metrics appended, so
	// a per-signal value was either ignored (alone: no pipeline at all) or
	// overridden (beside the base) — a working-looking install exporting
	// nothing, or exporting somewhere else.
	for _, k := range []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return t, fmt.Errorf("%s is not supported: set OTEL_EXPORTER_OTLP_ENDPOINT to the "+
				"collector's base URL; /v1/traces and /v1/metrics are appended", k)
		}
	}

	// Toft's own name, so envFlag: a value it cannot read is refused rather
	// than taken as off (see isTrue below for the line between the readers).
	trustInbound, err := envFlag("TRUST_INBOUND_TRACE_CONTEXT")
	if err != nil {
		return t, err
	}
	t.TrustInboundTrace = trustInbound

	// The credential is the one variable whose *presence* without an endpoint is
	// a mistake worth stopping for. Everything else defaults harmlessly; a
	// bearer token sitting in the environment of a process that exports nowhere
	// is somebody's export that is silently not happening.
	if t.Headers != "" && !t.Enabled() {
		return t, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS is set but no OTLP endpoint is: " +
			"set OTEL_EXPORTER_OTLP_ENDPOINT, or unset the headers")
	}
	if t.MetricsToken != "" && t.MetricsAddr == "" {
		return t, fmt.Errorf("METRICS_TOKEN is set but METRICS_ADDR is not: there is no scrape " +
			"listener for the token to protect")
	}

	// The transport and signal switches are refused only when there is
	// something to export. A stray OTEL_* variable in the environment of an
	// install that has configured no endpoint describes nothing, and refusing to
	// boot over it would turn a cluster-wide sidecar injector's default into an
	// outage on the one deployment that never asked for telemetry.
	if t.Enabled() {
		if t.Protocol != "http/protobuf" {
			return t, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL=%q is not supported: this build speaks "+
				"http/protobuf only (a collector receives HTTP and forwards over gRPC — see ADR §31 D1)", t.Protocol)
		}
		for _, sig := range []struct{ name, value string }{
			{"OTEL_TRACES_EXPORTER", t.TracesExporter},
			{"OTEL_METRICS_EXPORTER", t.MetricsExporter},
		} {
			if sig.value != "otlp" && sig.value != "none" {
				return t, fmt.Errorf("%s must be \"otlp\" or \"none\" (got %q)", sig.name, sig.value)
			}
		}
		if t.Temporality != "cumulative" && t.Temporality != "delta" {
			return t, fmt.Errorf("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE must be "+
				"\"cumulative\" or \"delta\" (got %q)", t.Temporality)
		}
		// The sampler name is accepted for compatibility with every runbook that
		// sets it, and anything else is refused rather than quietly doing
		// something different: parent-based ratio sampling is what the propagator
		// in internal/telemetry is built around.
		if v := strings.ToLower(getEnv("OTEL_TRACES_SAMPLER", "parentbased_traceidratio")); v != "parentbased_traceidratio" {
			return t, fmt.Errorf("OTEL_TRACES_SAMPLER=%q is not supported: this build samples "+
				"parentbased_traceidratio, and honours an inbound decision only from a trusted proxy", v)
		}
	}

	if v := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); v != "" {
		ratio, err := strconv.ParseFloat(v, 64)
		if err != nil || ratio < 0 || ratio > 1 {
			return t, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be a ratio in [0,1] (got %q)", v)
		}
		t.SamplerArg = ratio
	}

	if v := os.Getenv("OTEL_EXPORTER_OTLP_TIMEOUT"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms <= 0 {
			return t, fmt.Errorf("OTEL_EXPORTER_OTLP_TIMEOUT must be milliseconds > 0 (got %q)", v)
		}
		t.Timeout = time.Duration(ms) * time.Millisecond
	}
	if v := os.Getenv("OTEL_METRIC_EXPORT_INTERVAL"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms <= 0 {
			return t, fmt.Errorf("OTEL_METRIC_EXPORT_INTERVAL must be milliseconds > 0 (got %q)", v)
		}
		t.MetricExportInterval = time.Duration(ms) * time.Millisecond
	}
	if v := os.Getenv("TOFT_METRIC_APP_CARDINALITY_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return t, fmt.Errorf("TOFT_METRIC_APP_CARDINALITY_CAP must be a positive count (got %q)", v)
		}
		t.AppCardinalityCap = n
	}

	return t, nil
}

// MetricsBindWarning returns the sentence to log when the scrape listener is
// bound somewhere a stranger can reach it with no token, or "" when there is
// nothing to say.
//
// A warning rather than a refusal, deliberately: Prometheus on another host is
// a normal topology, and refusing it would push operators to put /metrics back
// on the platform's origin, which is the thing D4 exists to prevent.
func (t Telemetry) MetricsBindWarning() string {
	if !t.ScrapeEnabled() || t.MetricsToken != "" {
		return ""
	}
	host, _, err := splitHostPortLoose(t.MetricsAddr)
	if err == nil && (host == "" || IsLoopbackHost(host)) {
		return ""
	}
	return fmt.Sprintf("METRICS_ADDR=%s is not loopback and METRICS_TOKEN is unset — "+
		"anything that can reach that address can read every metric this install exports, "+
		"which enumerates every app and route on it. Set METRICS_TOKEN.", t.MetricsAddr)
}

// splitHostPortLoose tolerates ":9090", which is a legal listen address meaning
// every interface — and which is exactly the case the warning above is for.
func splitHostPortLoose(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port in %q", addr)
	}
	host = strings.Trim(addr[:i], "[]")
	return host, addr[i+1:], nil
}

// isTrue is the lenient reader, for the names this platform borrows rather than
// owns. It accepts the same spellings envFlag does — stated once, in
// config.go's flagOn/flagOff — and differs from it in exactly one place: a
// value in neither list is off here, and refused there.
//
// That is the line between the two readers, and it is a stated one rather than
// the accident it used to be. A name in somebody else's runbook —
// OTEL_RECORD_ENDUSER_ID, OTEL_DB_TRACE — must behave as their documentation
// says, and refusing a value their specification tolerates would be this
// platform overruling it. A name Toft owns gets envFlag, because Toft
// can afford to refuse it.
//
// PROPAGATE_TRACE_TO_APPS is the one exception and it is deliberate: the name
// is Toft's, it has read 0 correctly since it was written, and its off
// position is the safe one, so moving it would turn a switch that has never
// misled anybody into one that can refuse to start. Revisit if a second
// Toft-owned name ever joins it — two is a pattern and one is an accident.
func isTrue(v string) bool {
	on, err := parseFlag(v)
	return err == nil && on
}
