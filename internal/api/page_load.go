package api

import (
	"net/http"
	"strings"
)

// isPageLoad reports whether a gateway request is a page load — a top-level
// document navigation, the unit the Usage tab and the anonymous audience count
// (ADR §150). A page that pulls forty assets and polls a
// websocket is still one.
//
// A modern browser says so itself: Sec-Fetch-Dest is "document" for a
// top-level navigation and only then. **"iframe" is never a page load**,
// because Toft's own Preview tab frames the app, and an owner glancing at
// their preview is not a reader; an app embedded on an intranet page
// undercounts instead, which is stated rather than designed around. With no
// Sec-Fetch-Dest at all (an old browser, curl), a GET whose Accept puts
// text/html first is taken as one.
//
// Like isReachabilityProber, it decides nothing about authorization.
func isPageLoad(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	accept := strings.TrimSpace(r.Header.Get("Accept"))
	first, _, _ := strings.Cut(accept, ",")
	first, _, _ = strings.Cut(first, ";")
	return strings.EqualFold(strings.TrimSpace(first), "text/html")
}
