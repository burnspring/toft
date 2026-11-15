package telemetry

import (
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// UpstreamTransport times the deployed app's own half of a gateway request.
//
// The gateway's server span already says how long the request took; this says
// how much of that was somebody else's Flask process. The difference is the
// whole question when an app is slow: an owner told "the platform is slow"
// looks in the wrong place, and an operator told "your app is slow" needs to be
// able to prove it.
//
// It wraps the ReverseProxy's Transport rather than timing around ServeHTTP,
// because ServeHTTP includes the response body streaming back to the visitor —
// which on an SSE subscription or a large download is minutes of perfectly
// healthy connection, and would drown every real upstream stall.
//
// The app id has already been through the capped registry by the time it gets
// here (the gateway resolves it once, in appProxyGuard).
func UpstreamTransport(base http.RoundTripper, appID string) http.RoundTripper {
	if live.Load() == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &upstreamTransport{base: base, appID: appID}
}

type upstreamTransport struct {
	base  http.RoundTripper
	appID string
}

func (t *upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, span := StartClient(r.Context(), "app.upstream",
		attribute.String("toft.app.id", t.appID),
		attribute.String("http.request.method", normalizeMethod(r.Method)),
	)
	start := time.Now()
	resp, err := t.base.RoundTrip(r.WithContext(ctx))
	elapsed := time.Since(start)

	status := 0
	if resp != nil {
		status = resp.StatusCode
		span.Set(attribute.Int("http.response.status_code", status))
	}
	span.End(err)

	RecordUpstream(ctx, t.appID, status, err, elapsed)
	return resp, err
}
