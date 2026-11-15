package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// remoteAddrSeenBy runs one request through realIPMiddleware and reports the
// RemoteAddr the handler saw.
func remoteAddrSeenBy(t *testing.T, trusted []*net.IPNet, peer string, hdr map[string]string) (remote string) {
	t.Helper()
	h := realIPMiddleware(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote = r.RemoteAddr
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return remote
}

// Forwarded headers from an untrusted peer must be ignored — otherwise the
// rate-limit key and the audit-log IP are attacker-chosen. Task 1.6.
func TestRealIPIgnoresHeadersFromUntrustedPeers(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")

	for _, hdr := range []map[string]string{
		{"X-Forwarded-For": "1.2.3.4"},
		{"X-Real-IP": "1.2.3.4"},
		{"True-Client-IP": "1.2.3.4"},
	} {
		remote := remoteAddrSeenBy(t, trusted, "203.0.113.9:4000", hdr)
		if remote != "203.0.113.9:4000" {
			t.Errorf("header %v from untrusted peer rewrote RemoteAddr to %q", hdr, remote)
		}
	}
}

func TestRealIPResolvesClientThroughTrustedChain(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")

	// client (198.51.100.7) → proxy (10.0.0.2) → us. The client prepended a
	// spoofed entry; right-to-left parsing must not reach it.
	remote := remoteAddrSeenBy(t, trusted, "10.0.0.2:9000", map[string]string{
		"X-Forwarded-For": "6.6.6.6, 198.51.100.7, 10.0.0.3",
	})
	if remote != "198.51.100.7" {
		t.Errorf("RemoteAddr = %q, want the rightmost untrusted hop 198.51.100.7", remote)
	}
}

func TestRealIPKeepsPeerWhenChainIsAllTrustedOrMalformed(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")

	remote := remoteAddrSeenBy(t, trusted, "10.0.0.2:9000", map[string]string{
		"X-Forwarded-For": "10.0.0.5, 10.0.0.3",
	})
	if remote != "10.0.0.2:9000" {
		t.Errorf("all-trusted chain: RemoteAddr = %q, want socket peer", remote)
	}

	remote = remoteAddrSeenBy(t, trusted, "10.0.0.2:9000", map[string]string{
		"X-Forwarded-For": "not-an-ip, 198.51.100.7",
	})
	if remote != "198.51.100.7" {
		t.Errorf("rightmost valid untrusted hop should win, got %q", remote)
	}
	remote = remoteAddrSeenBy(t, trusted, "10.0.0.2:9000", map[string]string{
		"X-Forwarded-For": "198.51.100.7, not-an-ip",
	})
	if remote != "10.0.0.2:9000" {
		t.Errorf("malformed rightmost hop must poison the chain, got %q", remote)
	}
}

func TestRealIPFallsBackToXRealIPFromTrustedPeer(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")

	remote := remoteAddrSeenBy(t, trusted, "10.0.0.2:9000", map[string]string{
		"X-Real-IP": "198.51.100.7",
	})
	if remote != "198.51.100.7" {
		t.Errorf("X-Real-IP from trusted peer: RemoteAddr = %q", remote)
	}
}

// An isolated app reaches the platform's Service directly, from a range an
// operator may have had to trust. On the routes only a workload calls, a
// forwarded header is never believed, so the app cannot name its own address
// in the audit rows its calls write (DEPLOY-3, the platform half).
func TestTheWorkloadSurfaceBelievesNoProxy(t *testing.T) {
	trusted := mustCIDRs(t, "10.244.0.0/16")
	for _, path := range []string{"/api/v1/app/self", "/api/v1/runner/bootstrap", "/api/v1/apps"} {
		var remote string
		var via bool
		h := realIPMiddleware(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			remote, via = r.RemoteAddr, viaTrustedProxy(r)
		}))
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "10.244.3.7:4000"
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		h.ServeHTTP(httptest.NewRecorder(), req)
		workload := path != "/api/v1/apps"
		if workload && (remote != "10.244.3.7:4000" || via) {
			t.Errorf("%s: RemoteAddr = %q via=%v, want the socket peer and no proxy", path, remote, via)
		}
		if !workload && remote != "198.51.100.7" {
			t.Errorf("%s: RemoteAddr = %q, want the forwarded client", path, remote)
		}
	}
}

// A proxy that appends its own X-Forwarded-For line (HAProxy's `option
// forwardfor`) leaves the client's line first; every line is read, so the
// client's own claim is the far-left entry and never wins.
func TestRealIPReadsEveryForwardedForLine(t *testing.T) {
	trusted := mustCIDRs(t, "10.0.0.0/8")
	var remote string
	h := realIPMiddleware(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote = r.RemoteAddr
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:4000"
	req.Header.Add("X-Forwarded-For", "6.6.6.6")
	req.Header.Add("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if remote != "203.0.113.9" {
		t.Fatalf("RemoteAddr = %q, want the address the proxy appended (203.0.113.9)", remote)
	}
}
