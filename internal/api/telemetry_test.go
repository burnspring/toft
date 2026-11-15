package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"toft/internal/config"
	"toft/internal/telemetry"
	"toft/internal/telemetry/telemetryharness"
)

// randomParam is what every path parameter is filled with. Distinctive enough
// that finding it anywhere in a metric attribute is unambiguous.
const randomParam = "zzq7f3x-not-a-pattern"

// TestRouteAttributeIsAlwaysAPattern is the test that catches r.URL.Path
// sneaking into a metric.
//
// It walks the *real* router — the same walk TestEveryRouteIsClassified does —
// fires a request at every route with a junk value in every path parameter, and
// asserts that no recorded http.route contains it. That is the whole
// cardinality argument in one assertion: an app id, a slug, a run id or a
// deployment id reaching this attribute would mint a series per value, and the
// standard way a metrics rollout takes down the monitoring system it was
// supposed to protect is exactly this.
//
// The handlers panic on the nil database, chi's Recoverer turns that into a
// 500, and the middleware records the route regardless — which is the point:
// what is under test is the attribute, not the handler.
func TestRouteAttributeIsAlwaysAPattern(t *testing.T) {
	h := telemetryharness.Enable(t)
	router := routerForWalk(t)

	var fired int
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		req := httptest.NewRequest(method, fillPattern(route), nil)
		router.ServeHTTP(httptest.NewRecorder(), req)
		fired++
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	if fired == 0 {
		t.Fatal("walked no routes")
	}

	values := h.AttributeValues(t, telemetry.MetricHTTPServerDuration, "http.route")
	if len(values) == 0 {
		t.Fatal("no http.route values were recorded; the middleware is not installed")
	}
	for _, v := range values {
		if strings.Contains(v, randomParam) {
			t.Errorf("http.route = %q — a path parameter reached a metric attribute", v)
		}
	}
	t.Logf("fired %d routes, recorded %d distinct http.route series", fired, len(values))
}

// TestMetricsIsNotServedOnThePlatformOrigin is ADR §31 D4, asserted rather than
// asserted-in-prose.
//
// On a single-origin install (ADR §15) a deployed app's JavaScript runs on the
// platform's origin and can fetch('/metrics') with the viewer's cookie —
// whatever authorization were put in front of it, because it *is* the viewer.
// The scrape endpoint therefore lives on a listener of its own, and this is the
// test that stops somebody helpfully adding it back here.
func TestMetricsIsNotServedOnThePlatformOrigin(t *testing.T) {
	telemetryharness.Enable(t)
	router := routerForWalk(t)

	for _, path := range []string{"/metrics", "/api/v1/metrics", "/api/v1/system/metrics"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d; the scrape endpoint must not be reachable on the platform origin",
				path, rec.Code)
		}
	}

	// And no route by that name is registered, which is the version of the
	// statement that survives somebody changing the NotFound handler.
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasSuffix(route, "/metrics") {
			t.Errorf("%s %s is registered on the platform router", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fillPattern turns a chi pattern into a concrete path, putting a junk value in
// every parameter and wildcard.
func fillPattern(route string) string {
	var b strings.Builder
	for _, seg := range strings.Split(route, "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		switch {
		case strings.HasPrefix(seg, "{"):
			b.WriteString(randomParam)
		case seg == "*":
			b.WriteString(randomParam)
		default:
			b.WriteString(seg)
		}
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

// TestTelemetryEnabledAfterBootRecordsRequests is TL-9. Admin → Telemetry
// applies a saved collector immediately, so a router built while telemetry was
// off must still carry the middleware: an install that booted with nothing
// configured and then saved a collector would otherwise record no HTTP signal
// until restart.
func TestTelemetryEnabledAfterBootRecordsRequests(t *testing.T) {
	if telemetry.Enabled() {
		t.Fatal("telemetry is on before the router is built; the test proves nothing")
	}
	router := routerForWalk(t)

	h := telemetryharness.Enable(t)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/config", nil))

	if values := h.AttributeValues(t, telemetry.MetricHTTPServerDuration, "http.route"); len(values) == 0 {
		t.Error("no http.server.request.duration point was recorded for a router built " +
			"before telemetry was switched on")
	}
}

// TestInboundTraceNeedsItsOwnSwitch is TL-5. A trusted proxy relays
// traceparent verbatim from whoever sent it, so the trusted-peer test alone
// would let any internet client choose the trace id and force sampling.
// TRUST_INBOUND_TRACE_CONTEXT has to be on as well.
func TestInboundTraceNeedsItsOwnSwitch(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const inbound = "00-" + traceID + "-00f067aa0ba902b7-01"
	_, loopback, _ := net.ParseCIDR("127.0.0.1/32")

	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			spans := tracetest.NewInMemoryExporter()
			stop, err := telemetry.Init(context.Background(), telemetry.Runtime{
				Endpoint: "http://telemetry.test", Traces: true,
				SampleRatio: 0, // only a believed sampled parent can produce a span
				Timeout:     time.Second, MetricInterval: time.Minute,
				Temporality: "cumulative", AppCardinalityCap: 100,
				Source: telemetry.SourceEnvironment,
			}, telemetry.Options{
				Env:          config.Telemetry{ServiceName: "toft-test"},
				SpanExporter: spans,
			})
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			defer stop(context.Background())

			s := &Server{cfg: &config.Config{
				TrustedProxyCIDRs: []*net.IPNet{loopback},
				Telemetry:         config.Telemetry{TrustInboundTrace: on},
			}}
			router := s.Router(http.NotFoundHandler())
			req := httptest.NewRequest("GET", "/api/v1/config", nil)
			req.RemoteAddr = "127.0.0.1:40000" // a trusted peer either way
			req.Header.Set("traceparent", inbound)
			router.ServeHTTP(httptest.NewRecorder(), req)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetry.Flush(ctx); err != nil {
				t.Fatalf("flush: %v", err)
			}
			got := spans.GetSpans().Snapshots()
			if !on {
				if len(got) != 0 {
					t.Errorf("with the switch off a trusted peer's traceparent was believed: %d span(s), trace %s",
						len(got), got[0].SpanContext().TraceID())
				}
				return
			}
			if len(got) == 0 {
				t.Fatal("with the switch on the sampled parent was not honoured")
			}
			if id := got[0].SpanContext().TraceID().String(); id != traceID {
				t.Errorf("trace id = %s, want the inbound %s", id, traceID)
			}
		})
	}
}
