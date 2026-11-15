package main

import (
	"net/http"
	"strings"
)

// Where a deployed app is, which is a question only the install can answer.
//
// An app's address is not the install's address with a path on the end. On a
// two-origin install the platform serves `:8080` and deployed apps are proxied
// on a different origin entirely (`APPS_BASE_URL`), and the platform origin
// does not route `/apps/*` at all — it falls through to the SPA. So a client
// that derived the URL got a 200 and a page of Toft's own HTML, which is
// the worst shape a wrong answer can take: an agent that fetched it to see
// whether its deploy worked would be told yes, by the wrong server.
//
// So the URL is read, not constructed. `/api/v1/config` publishes both halves —
// `apps_base_url`, deliberately empty when the two origins are one, and
// `apps_path_prefix` — and this is the only place either is spelled.
type installConfig struct {
	AppsBaseURL    string `json:"apps_base_url"`
	AppsPathPrefix string `json:"apps_path_prefix"`
	Version        string `json:"version"`
}

// config is the probe, once per invocation.
//
// Unauthenticated by design (it renders a login page), so it costs nothing and
// works before there is a credential. **A failure here is silence**: every
// caller is on a path that has already talked to the install successfully, so
// this failing means a blip rather than an outage, and a courtesy read that
// could fail a deploy would not be one. What it falls back to is the old
// derivation, which is right on the single-origin install most people run.
func (s *session) config() installConfig {
	if !s.cfgRead {
		s.cfgRead = true
		var cfg installConfig
		if err := s.client.Do(http.MethodGet, "/api/v1/config", nil, &cfg); err == nil {
			s.cfg = cfg
		}
	}
	return s.cfg
}

// publicURL is where the app answers as the install says — its own hostname
// once a move to one has finished — falling back to appURL. What `status`,
// `deploy` and `create` report; a status that kept /apps/<slug> after
// `hostname --on` named an address that only redirects (BB-026).
func (s *session) publicURL(app *appSummary) string {
	if app.PublicURL != "" {
		return app.PublicURL
	}
	return s.appURL(app)
}

// appURL is where a person or a script reaches this app.
//
// An install too old to publish the fields answers with neither, and the zero
// value is exactly the derivation this replaced — which is the safe direction,
// because a client newer than its install is the normal state of affairs.
func (s *session) appURL(app *appSummary) string {
	cfg := s.config()

	base := strings.TrimRight(cfg.AppsBaseURL, "/")
	if base == "" {
		base = strings.TrimRight(s.InstallURL, "/")
	}

	prefix := strings.Trim(cfg.AppsPathPrefix, "/")
	if prefix == "" {
		prefix = "apps"
	}
	return base + "/" + prefix + "/" + app.Slug
}
