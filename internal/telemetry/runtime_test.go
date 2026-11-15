package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/telemetry/telemetrytest"
)

// TestSavedDocumentWinsOverTheEnvironment is the precedence rule, and it is the
// opposite of the sign-in providers'.
//
// The difference is not an inconsistency: an environment-configured provider
// wins over a database row because it exists to solve "nobody can sign in to
// fix the database row". Telemetry has no such bootstrap problem — nobody is
// ever locked out of a collector — so the surface an administrator can reach is
// the one that should decide.
func TestSavedDocumentWinsOverTheEnvironment(t *testing.T) {
	env := config.Telemetry{
		Endpoint:             "http://from-the-environment:4318",
		Headers:              "authorization=Bearer env-token",
		TracesExporter:       "otlp",
		MetricsExporter:      "otlp",
		SamplerArg:           0.05,
		Temporality:          "cumulative",
		Timeout:              10 * time.Second,
		MetricExportInterval: time.Minute,
		AppCardinalityCap:    500,
	}

	t.Run("never saved: the environment is the whole configuration", func(t *testing.T) {
		rt := Resolve(env, database.DefaultTelemetrySettings(), "")
		if rt.Source != SourceEnvironment {
			t.Errorf("source = %q, want %q", rt.Source, SourceEnvironment)
		}
		if rt.Endpoint != env.Endpoint {
			t.Errorf("endpoint = %q, want the environment's", rt.Endpoint)
		}
		if rt.Headers["authorization"] != "Bearer env-token" {
			t.Errorf("the environment's credential was not carried: %v", rt.Headers)
		}
	})

	t.Run("saved: the document wins", func(t *testing.T) {
		doc := database.TelemetrySettings{
			Configured: true, Enabled: true,
			Endpoint: "http://from-the-settings:4318",
			Traces:   true, Metrics: false,
			SampleRatio: 0.5, Temporality: "delta",
		}
		rt := Resolve(env, doc, "authorization=Bearer saved-token")
		if rt.Source != SourceSettings {
			t.Errorf("source = %q, want %q", rt.Source, SourceSettings)
		}
		if rt.Endpoint != doc.Endpoint {
			t.Errorf("endpoint = %q, want the document's", rt.Endpoint)
		}
		if rt.Headers["authorization"] != "Bearer saved-token" {
			t.Errorf("the document's credential was not carried: %v", rt.Headers)
		}
		if rt.Metrics {
			t.Error("the document switched metrics off and they are on")
		}
		if rt.SampleRatio != 0.5 || rt.Temporality != "delta" {
			t.Errorf("ratio/temporality came from the environment: %v %q", rt.SampleRatio, rt.Temporality)
		}
		// Carried from the environment whichever source wins — these are host
		// facts and a capacity bound, not decisions on the page.
		if rt.Timeout != env.Timeout || rt.AppCardinalityCap != env.AppCardinalityCap {
			t.Error("a host fact was lost when the document won")
		}
	})

	t.Run("saved and switched off: off despite the environment", func(t *testing.T) {
		doc := database.TelemetrySettings{Configured: true, Enabled: false}
		rt := Resolve(env, doc, "")
		if rt.Enabled() {
			t.Error("export is on after an administrator switched it off — a switch that " +
				"an environment variable overrides is a switch that lies")
		}
		if rt.Endpoint != "" {
			t.Errorf("endpoint = %q, want none", rt.Endpoint)
		}
	})
}

func TestSignalURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{"http://collector:4318", "http://collector:4318/v1/traces"},
		{"http://collector:4318/", "http://collector:4318/v1/traces"},
		// A collector behind a prefix is a real deployment, and the signal path
		// is appended to the prefix rather than replacing it.
		{"https://obs.example.com/otlp", "https://obs.example.com/otlp/v1/traces"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := signalURL(tc.base, "/v1/traces"); got != tc.want {
			t.Errorf("signalURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	got := parseHeaders("authorization=Bearer abc, x-tenant = acme ,,broken")
	if got["authorization"] != "Bearer abc" {
		t.Errorf("authorization = %q", got["authorization"])
	}
	if got["x-tenant"] != "acme" {
		t.Errorf("x-tenant = %q", got["x-tenant"])
	}
	// A malformed entry is skipped rather than failing the set: this runs on
	// every save and every restart, and refusing to export because one header
	// had no `=` in it is a worse outcome than exporting without it.
	if _, ok := got["broken"]; ok {
		t.Error("an entry with no = became a header")
	}
	if parseHeaders("") != nil {
		t.Error("an empty credential produced a non-nil header map")
	}
}

// TestApplyRebuildsThePipelineInPlace is the property that makes this a settings
// page rather than a form that asks somebody to restart the platform.
func TestApplyRebuildsThePipelineInPlace(t *testing.T) {
	first := telemetrytest.NewCollector(t)
	second := telemetrytest.NewCollector(t)

	base := Runtime{
		Traces: true, SampleRatio: 1.0,
		Timeout: 2 * time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}
	rt := base
	rt.Endpoint = first.URL()

	stop, err := Init(context.Background(), rt, Options{
		Env: config.Telemetry{ServiceName: "toft-test"},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stop(ctx)
	})

	_, s := Start(context.Background(), "before")
	s.End(nil)
	flushNow(t)
	if _, ok := first.SpanNamed("before"); !ok {
		t.Fatal("the first collector received nothing")
	}

	// Re-point, exactly as a settings save does.
	next := base
	next.Endpoint = second.URL()
	if err := Apply(context.Background(), next); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	_, s2 := Start(context.Background(), "after")
	s2.End(nil)
	flushNow(t)

	if _, ok := second.SpanNamed("after"); !ok {
		t.Error("the new collector received nothing after Apply — the swap did not take effect")
	}
	if _, ok := first.SpanNamed("after"); ok {
		t.Error("the superseded collector is still receiving spans")
	}
	if rt, _, ok := Current(); !ok || rt.Endpoint != second.URL() {
		t.Errorf("Current() still reports %q", rt.Endpoint)
	}
}

// TestApplyOffStopsExporting: switching export off on the page has to stop it,
// not merely stop new configuration from being read.
func TestApplyOffStopsExporting(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	stop, err := Init(context.Background(), Runtime{
		Endpoint: c.URL(), Traces: true, SampleRatio: 1.0,
		Timeout: 2 * time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}, Options{Env: config.Telemetry{ServiceName: "toft-test"}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	if err := Apply(context.Background(), Runtime{}); err != nil {
		t.Fatalf("Apply(off): %v", err)
	}
	if Enabled() {
		t.Fatal("telemetry reports itself on after being switched off")
	}

	_, s := Start(context.Background(), "should-not-exist")
	s.End(nil)
	time.Sleep(50 * time.Millisecond)
	if _, ok := c.SpanNamed("should-not-exist"); ok {
		t.Error("a span was exported after export was switched off")
	}
}

func flushNow(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// The exporter POSTs from the worker goroutine; ForceFlush returns when the
	// batch has been handed over, not when the collector has recorded it.
	time.Sleep(100 * time.Millisecond)
}

// TestExportReachesTheCollector is the regression test for a button that lied.
//
// The first version of TestExport sent an empty batch, on the reasoning that
// the question was only whether the collector accepts a POST. The OTLP
// exporters return nil without sending anything when handed nothing, so it
// reported every configuration as healthy — including an endpoint on a port
// nothing was listening on. A test button that cannot fail is worse than no
// button, because it converts "I am not sure" into "I checked".
func TestExportReachesTheCollector(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	rt := Runtime{
		Endpoint: c.URL(), Traces: true, SampleRatio: 1.0,
		Timeout: 2 * time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}
	stop, err := Init(context.Background(), rt, Options{
		Env: config.Telemetry{ServiceName: "toft-test"},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	if err := TestExport(context.Background()); err != nil {
		t.Fatalf("TestExport against a working collector: %v", err)
	}
	if _, ok := c.SpanNamed(TestSpanName); !ok {
		t.Errorf("the collector received no %q span — the test exported nothing and would "+
			"have reported success whatever the endpoint was", TestSpanName)
	}

	// And it must fail against an endpoint nothing is serving.
	dead := rt
	dead.Endpoint = "http://127.0.0.1:1"
	if err := Apply(context.Background(), dead); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := TestExport(context.Background()); err == nil {
		t.Error("TestExport reported success against a closed port")
	}
}

// TestExportScrubsTheCollectorsWords: an administrator debugging a 401 must not
// end up with the bearer token on their screen, and from there in a screenshot
// in a ticket.
func TestExportErrorIsScrubbed(t *testing.T) {
	newScrubber([]string{"super-secret-collector-token"})
	t.Cleanup(func() { newScrubber(nil) })

	got := scrubErr(errors.New("401 Unauthorized: authorization=Bearer super-secret-collector-token"))
	if strings.Contains(got.Error(), "super-secret-collector-token") {
		t.Errorf("the collector's error carried the credential: %q", got)
	}
}

// TestApplyRacesNoRequest: a settings save replaces the app-cardinality
// registry while gateway requests read it on every hop. Run under -race, a
// plain pointer store here is a reported race; the registry is atomic like the
// rest of the package's hot state.
func TestApplyRacesNoRequest(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	rt := Runtime{
		Endpoint: c.URL(), Traces: true, SampleRatio: 1.0,
		Timeout: 2 * time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}
	stop, err := Init(context.Background(), rt, Options{Env: config.Telemetry{ServiceName: "toft-test"}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	done := make(chan struct{})
	reading := make(chan struct{})
	go func() {
		defer close(reading)
		for {
			select {
			case <-done:
				return
			default:
				if got := AppAttr("app-1"); got != "app-1" && got != OverflowApp {
					t.Errorf("AppAttr = %q", got)
				}
			}
		}
	}()
	for i := 0; i < 5; i++ {
		rt.AppCardinalityCap = 50 + i
		if err := Apply(context.Background(), rt); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	close(done)
	<-reading
}

// TL-8(b): the observable sources are registered once, at startup, so an
// off-then-on on the settings page must not leave them cleared.
func TestApplyOffKeepsTheObservers(t *testing.T) {
	c := telemetrytest.NewCollector(t)
	ObserveBuildQueue(func() int64 { return 7 })
	t.Cleanup(func() { buildQueueSource.Store(nil) })
	stop, err := Init(context.Background(), Runtime{
		Endpoint: c.URL(), Traces: true, SampleRatio: 1.0,
		Timeout: 2 * time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}, Options{Env: config.Telemetry{ServiceName: "toft-test"}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	if err := Apply(context.Background(), Runtime{}); err != nil {
		t.Fatalf("Apply(off): %v", err)
	}
	if buildQueueSource.Load() == nil {
		t.Error("switching export off cleared the build-queue source; switching it back on " +
			"would leave toft.build.queue.depth absent until restart")
	}
}

// TL-8(c): a shutdown that fails — the meter provider's, when the collector is
// down — must not stop the rest of the superseded pipeline from draining.
func TestDrainRunsEveryShutdown(t *testing.T) {
	ran := make(chan struct{})
	p := &pipeline{shutdown: []func(context.Context) error{
		func(context.Context) error { close(ran); return nil },
		func(context.Context) error { return errors.New("collector unreachable") },
	}}
	drainInBackground(p)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the first-added shutdown never ran after the last-added one failed")
	}
}

// TL-8(d): the scrubber learns the credential the saved document carries, not
// only the boot-time secrets.
func TestSavedCredentialIsScrubbed(t *testing.T) {
	const secret = "k3y-value-0123456789"
	stop, err := Init(context.Background(), Runtime{
		Endpoint: "http://telemetry.test", Traces: true, SampleRatio: 1.0,
		Timeout: time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
		Source: SourceSettings, Headers: map[string]string{"api-key": secret},
	}, Options{
		Env:          config.Telemetry{ServiceName: "toft-test"},
		SpanExporter: &recordingExporter{},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	if got := scrub("401: bad key " + secret); strings.Contains(got, secret) {
		t.Errorf("scrub left the saved credential in place: %q", got)
	}
}

// The environment's headers are scrubbed value by value too: the whole
// `k=v,k=v` string never appears verbatim in an error.
func TestEnvironmentCredentialIsScrubbedByValue(t *testing.T) {
	const secret = "dd-api-key-value-0123"
	stop, err := Init(context.Background(), Runtime{
		Endpoint: "http://telemetry.test", Traces: true, SampleRatio: 1.0,
		Timeout: time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}, Options{
		Env: config.Telemetry{ServiceName: "toft-test",
			Headers: "authorization=Bearer abcdefgh,DD-API-KEY=" + secret},
		SpanExporter: &recordingExporter{},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	if got := scrub("rejected " + secret); strings.Contains(got, secret) {
		t.Errorf("scrub left the environment's credential in place: %q", got)
	}
}

// TestSavedDocumentWithoutCredentialSendsNone is TL-4. The OTLP exporters read
// OTEL_EXPORTER_OTLP_HEADERS themselves; a saved document with no credential
// must not let the environment's ride to the endpoint the administrator named.
func TestSavedDocumentWithoutCredentialSendsNone(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer env-token")
	c := telemetrytest.NewCollector(t)
	stop, err := Init(context.Background(), Runtime{
		Source: SourceSettings, Endpoint: c.URL(), Traces: true, SampleRatio: 1.0,
		Timeout: 2 * time.Second, MetricInterval: time.Minute,
		Temporality: "cumulative", AppCardinalityCap: 100,
	}, Options{Env: config.Telemetry{ServiceName: "toft-test"}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { stop(context.Background()) })

	if err := TestExport(context.Background()); err != nil {
		t.Fatalf("TestExport: %v", err)
	}
	hs := c.RequestHeaders()
	if len(hs) == 0 {
		t.Fatal("the collector received nothing")
	}
	for _, h := range hs {
		if v := h.Get("Authorization"); v != "" {
			t.Errorf("the environment's credential reached the saved endpoint: Authorization=%q", v)
		}
	}
}

// The environment source keeps the SDK's own read of the headers.
func TestEnvironmentSourceKeepsTheSDKsHeaders(t *testing.T) {
	if h := exporterHeaders(Runtime{Source: SourceEnvironment}); h != nil {
		t.Errorf("exporterHeaders = %v for the environment source with no headers, want nil", h)
	}
	if h := exporterHeaders(Runtime{Source: SourceSettings}); h == nil || len(h) != 0 {
		t.Errorf("exporterHeaders = %v for a saved document with none, want an empty map", h)
	}
}
