package api

import (
	"net/http"
	"strings"
)

// forwardedHeaders are every header a proxy uses to speak for the client.
// None of them is true on the callback listener, whose only callers are
// workloads dialling it directly.
var forwardedHeaders = []string{
	"Forwarded",
	"X-Forwarded-For",
	"X-Forwarded-Proto",
	"X-Forwarded-Host",
	"X-Forwarded-Port",
	"X-Forwarded-Prefix",
	"X-Real-IP",
	"True-Client-IP",
}

// CallbackHandler is the handler for the workload callback listener
// (CALLBACK_ADDR, the fourth listener; ADR §31 D4). It hands the platform
// router the routes a runner and the SDKs call at CALLBACK_URL —
// `/api/v1/runner/…` and `/api/v1/app/…` — and answers every other path 404,
// so code that can reach this port finds no browser route behind it.
//
// It exists for the NetworkPolicy, not for itself. The apps namespace is
// admitted to this port and never to :8080, which the ingress controller is
// forwarded to and whose X-Forwarded-* is believed from TRUSTED_PROXY_CIDRS —
// often the pod CIDR the apps share with the controller. Confined here, an app
// cannot forge the client address of a sign-in, a throttle key or an audit row.
//
// Forwarded headers are deleted before the platform router sees them, so this
// listener never honours one and never marks a request as via a trusted proxy,
// whatever TRUSTED_PROXY_CIDRS holds. realIPMiddleware already ignores them on
// these two subtrees; the deletion makes it a property of the listener rather
// than of a path test inside it.
func CallbackHandler(platform http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !onCallbackSurface(r) {
			http.NotFound(w, r)
			return
		}
		for _, h := range forwardedHeaders {
			r.Header.Del(h)
		}
		platform.ServeHTTP(w, r)
	})
}

// onCallbackSurface tests both spellings of the path: the decoded one, which
// realIPMiddleware and the handlers read, and RawPath, which chi routes on
// whenever it is set (a request that carried a percent-encoding). A path has to
// be inside the workload subtrees in both, so no encoding can be routed
// somewhere the filter did not look.
func onCallbackSurface(r *http.Request) bool {
	if !isWorkloadSurface(r) {
		return false
	}
	if p := r.URL.RawPath; p != "" {
		return strings.HasPrefix(p, "/api/v1/runner/") || strings.HasPrefix(p, "/api/v1/app/")
	}
	return true
}
