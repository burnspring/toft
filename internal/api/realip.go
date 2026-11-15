package api

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// ctxKeyViaProxy marks a request whose socket peer was inside
// TRUSTED_PROXY_CIDRS — which is to say, one that reached us through a proxy
// we are willing to believe about the client and the public address.
type ctxKeyViaProxy struct{}

// viaTrustedProxy reports whether this request arrived through a trusted
// proxy. False when the middleware is not installed (no trusted networks
// configured) and false for a direct connection, which are the two cases where
// no X-Forwarded-* header may be believed.
func viaTrustedProxy(r *http.Request) bool {
	if r == nil {
		return false
	}
	v, _ := r.Context().Value(ctxKeyViaProxy{}).(bool)
	return v
}

// inboundTraceTrusted decides whether a request's inbound trace context is
// believed (ADR §31). The trusted-peer test is necessary and not sufficient: a
// proxy appends X-Forwarded-For itself, but relays traceparent verbatim from
// whoever sent it. So TRUST_INBOUND_TRACE_CONTEXT, off by default, has to be
// on as well — the operator's assertion that their edge strips or re-issues
// the header.
func (s *Server) inboundTraceTrusted(r *http.Request) bool {
	return s.cfg.Telemetry.TrustInboundTrace && viaTrustedProxy(r)
}

// realIPMiddleware replaces chi's middleware.RealIP, which trusted
// True-Client-IP / X-Real-IP / X-Forwarded-For from anyone. Toft binds
// 0.0.0.0 directly by default, so an attacker could pick their own rate-limit
// key and the IP written into the audit log by rotating a header.
//
// Here the rewrite only happens when the socket peer is inside
// TRUSTED_PROXY_CIDRS, and X-Forwarded-For is parsed right-to-left, skipping
// trusted hops — the rightmost address a trusted proxy vouches for wins.
// Without the env var the middleware is not installed at all and RemoteAddr
// stays the socket peer.
//
// **The workload surface believes no proxy.** `/api/v1/runner` and
// `/api/v1/app` are called by runners and the SDK at CALLBACK_URL, never by a
// browser through the ingress, so a forwarded header there says nothing true
// that the socket peer does not — and an isolated app reaches the platform's
// Service directly, from inside a range an operator may have had to trust (the
// pod CIDR, when the controller cannot be told apart from the apps). Ignoring
// X-Forwarded-* on those two subtrees keeps an app from naming its own address
// in the audit rows its own calls write, whatever the range holds.
func realIPMiddleware(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if peerIsTrusted(r.RemoteAddr, trusted) && !isWorkloadSurface(r) {
				// Marked before RemoteAddr is overwritten. Everything
				// downstream that wants to know "did this arrive through our
				// proxy" has to read this flag rather than re-test the peer,
				// because after the next three lines the peer address is the
				// *client's* and would fail the same test.
				r = r.WithContext(context.WithValue(r.Context(), ctxKeyViaProxy{}, true))

				// Every line, joined: HAProxy's `option forwardfor` appends
				// its own line after any the client brought, so the first
				// line alone is the client's word.
				if ip := clientIPFromXFF(strings.Join(r.Header.Values("X-Forwarded-For"), ","), trusted); ip != "" {
					r.RemoteAddr = ip
				} else if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(xr) != nil {
					r.RemoteAddr = xr
				}
				setAccessClientIP(r.Context(), clientIP(r))
			}

			next.ServeHTTP(w, r)
		})
	}
}

// isWorkloadSurface is a request for the routes only a workload calls. Tested
// on the decoded path, which is the safe direction: a path that decodes into
// these subtrees but routes elsewhere loses its forwarded headers, and nothing
// that routes into them can fail to decode into them.
func isWorkloadSurface(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/api/v1/runner/") || strings.HasPrefix(p, "/api/v1/app/")
}

func peerIsTrusted(remoteAddr string, trusted []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ipInAny(ip, trusted)
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIPFromXFF walks X-Forwarded-For right to left: entries appended by
// our trusted proxies are skipped, and the first address a trusted proxy
// received the request from is the client. Anything further left is
// client-supplied and must be ignored. Returns "" (meaning: keep the socket
// peer) when the header is absent, malformed, or contains only trusted hops.
func clientIPFromXFF(xff string, trusted []*net.IPNet) string {
	if xff == "" {
		return ""
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ipStr := strings.TrimSpace(parts[i])
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return "" // malformed chain — trust none of it
		}
		if ipInAny(ip, trusted) {
			continue
		}
		return ipStr
	}
	return ""
}
