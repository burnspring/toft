package api

import (
	"strings"

	"toft/internal/database"
)

// Who may put a deployed app inside an iframe.
//
// Before this, nobody had said: the proxy sent no framing header, so any site
// on the internet could embed any app on the install. What kept that from being
// a clickjacking hole was the cookie rather than a decision —
// `toft_app_session` is SameSite=Lax, so a cross-site frame carries no
// session and the app renders to a stranger — and "bounded by something else"
// is not the same as "chosen".
//
// **The default is the install itself.** `'self'` is the app's own origin, and
// the platform origin is named beside it because the app page embeds a live
// preview of the app and, on a two-origin install, that is a cross-origin
// frame. Anything beyond those two is the app owner's list.
//
// Not `X-Frame-Options`: it cannot express a list, and every browser that
// implements `frame-ancestors` ignores it when both are present. An install
// that must defend a browser older than CSP2 has a different problem than this.
func (s *Server) frameAncestorsPolicy(app *database.App) string {
	sources := []string{"'self'"}
	if base := strings.TrimSuffix(s.cfg.BaseURL, "/"); base != "" && s.cfg.SeparateAppsListener() {
		sources = append(sources, base)
	}
	sources = append(sources, app.FrameAncestors...)
	return "frame-ancestors " + strings.Join(sources, " ")
}
