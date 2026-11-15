package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The callback listener serves the workload subtrees and nothing else, and no
// forwarded header survives onto it — even from a peer inside
// TRUSTED_PROXY_CIDRS, which is where an isolated app sits when the range is
// the pod CIDR it shares with the ingress controller (DEPLOY-3).
func TestTheCallbackListenerServesOnlyWorkloadsAndBelievesNoProxy(t *testing.T) {
	trusted := mustCIDRs(t, "10.244.0.0/16")
	var reached bool
	var remote, xff string
	var via bool
	platform := realIPMiddleware(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached, remote, via = true, r.RemoteAddr, viaTrustedProxy(r)
		xff = r.Header.Get("X-Forwarded-For") + r.Header.Get("X-Real-IP") + r.Header.Get("Forwarded")
	}))
	h := CallbackHandler(platform)

	for target, want := range map[string]bool{
		"/api/v1/runner/bootstrap":      true,
		"/api/v1/app/self":              true,
		"/api/v1/app/store/s/objects/x": true,
		"/api/v1/apps":                  false, // a browser route
		"/api/v1/auth/login":            false,
		"/api/v1/config":                false,
		"/":                             false, // the SPA
		"/__api__/v1/content":           false, // the Connect rewrite
		"/api/v1/application":           false, // a prefix, not the subtree
		"/api/v1/app":                   false,
		"/api/v1/app%2F..%2Fapps":       false, // routed on the raw path
		"/store-data/token":             false,
	} {
		reached, remote, via, xff = false, "", false, ""
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.RemoteAddr = "10.244.3.7:4000"
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		req.Header.Set("X-Real-IP", "198.51.100.7")
		req.Header.Set("Forwarded", "for=198.51.100.7")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if reached != want {
			t.Errorf("%s: reached the platform router = %v, want %v (status %d)", target, reached, want, rec.Code)
			continue
		}
		if !want {
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: status %d, want 404", target, rec.Code)
			}
			continue
		}
		if remote != "10.244.3.7:4000" || via || xff != "" {
			t.Errorf("%s: RemoteAddr=%q via=%v forwarded=%q, want the socket peer, no proxy, no header",
				target, remote, via, xff)
		}
	}
}
