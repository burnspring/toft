package telemetry

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"toft/internal/config"
)

// The scrape endpoint: a listener of its own, serving exactly one path.
//
// It cannot be a route on :8080, and the reason is ADR §15 rather than taste.
// Both of this box's installs run single-origin, which means a deployed app's
// JavaScript executes on the platform's origin and can `fetch('/metrics')`
// carrying the viewer's cookie. Whatever authorization were put in front of
// that path, an app's front-end passes it — because it *is* the viewer.
// Toft's metrics are not catastrophic to leak, but they enumerate every
// app, every route and the shape of the install, and "an app can read the
// platform's operational metrics" is exactly the class of statement ADR §19
// refused to make about Headlamp.
//
// So: a third listener, and CLAUDE.md's "Two listeners" note becomes three.
//
// This listener has no session auth, no origin check, no rate limiter and no
// chi middleware stack. It is not part of the API surface, it never sees a
// principal, and TestEveryRouteIsClassified — the guard that fails the build on
// an unclassified /api/v1 route — does not apply to it, because it is not on
// that router. Said out loud here and in dev-reference.md, because the next
// person to read keyroutes.go will reasonably wonder.

// scrapeRegistry is the registry the OTel Prometheus exporter writes into. Its
// own registry rather than prometheus.DefaultRegisterer: the default one
// carries the client library's process and Go collectors, and this endpoint
// should serve exactly what the inventory declares — nothing arrives on it
// because a dependency happened to import client_golang.
var scrapeRegistry atomic.Pointer[*promclient.Registry]

func newScrapeReader() (sdkmetric.Reader, error) {
	reg := promclient.NewRegistry()
	exp, err := promexporter.New(
		promexporter.WithRegisterer(reg),
		// The scope info series says which instrumentation library produced a
		// metric. There is one, it is this package, and a constant label on
		// every series is cardinality spent on nothing.
		promexporter.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, err
	}
	// Stored as a pointer-to-pointer so the handler can tell "no registry yet"
	// from "a registry that happens to be empty" — the first is a 503 and the
	// second is a legitimately quiet install.
	scrapeRegistry.Store(&reg)
	return exp, nil
}

// startScrapeListener brings up the third listener and returns its shutdown.
func startScrapeListener(cfg config.Telemetry) (func(context.Context) error, error) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", scrapeAuth(cfg.MetricsToken, http.HandlerFunc(serveScrape)))
	// Everything else is 404, with no body worth reading. This is not an API
	// and it must not grow into one; if a second path is ever wanted (§ open
	// questions asked about splitting the per-app series out), that is a
	// decision with an ADR entry, not a line added here.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	ln, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		return nil, fmt.Errorf("telemetry: METRICS_ADDR=%s: %w", cfg.MetricsAddr, err)
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("telemetry: scrape listener on %s stopped: %v", cfg.MetricsAddr, err)
		}
	}()
	log.Printf("Metrics scrape endpoint on %s/metrics%s", cfg.MetricsAddr, tokenNote(cfg))

	return srv.Shutdown, nil
}

func tokenNote(cfg config.Telemetry) string {
	if cfg.MetricsToken != "" {
		return " (bearer token required)"
	}
	return " (no token — anything that can reach the address can read it)"
}

// serveScrape resolves the *current* registry on every request.
//
// The listener is created once and outlives every reconfiguration — a settings
// save must not close a socket Prometheus is in the middle of scraping — but a
// Prometheus Reader belongs to exactly one meter provider, so rebuilding the
// pipeline necessarily builds a new registry. Resolving here is what lets those
// two facts coexist.
//
// A scrape arriving in the gap between "telemetry was switched off" and "the
// listener was told" gets 503 rather than an empty exposition. An empty
// exposition is indistinguishable from every series having disappeared, which
// is what an absent-series alert fires on.
func serveScrape(w http.ResponseWriter, r *http.Request) {
	reg := scrapeRegistry.Load()
	if reg == nil {
		http.Error(w, "metrics are not being collected on this install", http.StatusServiceUnavailable)
		return
	}
	promhttp.HandlerFor(*reg, promhttp.HandlerOpts{
		// A scrape that fails half way is worse than one that fails outright: a
		// partial exposition looks like a series that stopped existing.
		ErrorHandling: promhttp.HTTPErrorOnError,
	}).ServeHTTP(w, r)
}

// scrapeAuth is the whole of the endpoint's access control.
//
// Optional, because on a loopback bind the process boundary is the control and
// a token would be ceremony. Constant-time, because a token compared with ==
// leaks itself one byte at a time to a patient scraper, and this one guards the
// shape of the whole install.
func scrapeAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			// No WWW-Authenticate: this is not a browser surface and a challenge
			// would only tell a scanner what kind of credential to go and find.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
