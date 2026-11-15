package api

import (
	"net/http"

	"toft/internal/builder"
	"toft/internal/docs"
	"toft/internal/version"
)

// handlePublicConfig exposes the handful of server-side settings a client needs
// in order to build correct links. Deployed apps live on their own origin, and
// the frontend cannot derive that from window.location.
//
// **`version` discloses this install's version to anything that can reach the
// endpoint**, which is unauthenticated by necessity — a login page has to render
// before anyone is signed in. That is accepted for a product that sits inside a
// network, and it buys the one thing a CLI cannot work out for itself: whether
// the install it is talking to is newer than the binary talking to it, which is
// the normal state of affairs for a laptop and is worth one dimmed line rather
// than a stream of steps rendered as their keys with no explanation. An install
// that disagrees moves the field behind auth and the CLI reads it after login.
func (s *Server) handlePublicConfig(w http.ResponseWriter, r *http.Request) {
	appsBase := s.cfg.AppsBaseURL
	if !s.cfg.SeparateAppsListener() {
		// Same origin — relative links are correct and avoid hardcoding a host.
		appsBase = ""
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"apps_base_url":        appsBase,
		"apps_path_prefix":     builder.BasePath(""),
		"separate_apps_origin": s.cfg.SeparateAppsListener(),
		// The published version only, never the revision: a commit hash names a
		// line of source to anyone who can read the repository, and nothing
		// outside this install needs it to compare two version numbers.
		"version": version.Version,
		// The manuals' folder for this minor, or "" under DOCS_URL=off. The
		// landing page links to it, and the site is public whatever this
		// install is, so naming it tells a visitor nothing about this install.
		"docs_url": docs.SiteFor(s.cfg.DocsURL, version.Version),
	})
}

// handleRobots refuses every crawler, on every path.
//
// The platform served no `robots.txt` until there was something at `/` worth
// indexing, and now that there is, the answer is still no. An internal app
// hosting platform has no reason to be found by search: everything but the
// landing page is behind a session, so what a crawler could index is one
// marketing page and a login form, and what it would *publish* is that this
// organization runs one of these at this address.
//
// `Disallow: /` rather than a file that allows `/` only, because the narrower
// rule is the one that needs an argument and nobody has made it yet. It is
// strictly a reduction in exposure, it is honoured by convention rather than
// enforced, and an install that wants to be found can change one handler.
//
// On the platform listener alone. The apps origin serves deployed apps and
// nothing else, and a `robots.txt` there would be answering for somebody else's
// app about paths this handler knows nothing about.
func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
}
