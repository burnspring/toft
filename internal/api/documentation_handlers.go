package api

import (
	"net/http"

	"toft/internal/docs"
	"toft/internal/version"
)

// The Documentation page's one route.
//
// Toft's manuals are one public site, published one folder per minor, and this
// install only links to it (ADR, *The manual*). So the answer is three links to
// the folder for the version this binary is — composed by docs.SiteFor and
// nowhere else — or none at all when the operator set DOCS_URL=off.
//
// **It makes no request.** Not to check the folder exists, not to list the
// versions the site has. A link is followed by a reader's browser; the server
// stays as quiet towards the docs host as it is towards the gallery when the
// gallery is off. A version not yet published is answered by the site's own
// 404 page, which names the version and links to the newest.
//
// Every signed-in principal reads it, API keys included, and every caller gets
// the same answer: the admin guide is public, and hiding a link to a public
// page is not a control.

type documentationResponse struct {
	// URL is the folder for this install's minor, or null under DOCS_URL=off.
	URL *string `json:"url"`
	// Version is the minor the links describe, present either way so the page
	// can say which Toft it is about.
	Version  string         `json:"version"`
	Sections []docs.Section `json:"sections"`
	// AgentSkills is the site's page on the two coding-agent skills, null
	// under DOCS_URL=off.
	AgentSkills *string `json:"agent_skills_url"`
}

func (s *Server) handleListDocumentation(w http.ResponseWriter, r *http.Request) {
	resp := documentationResponse{
		Version:  docs.Minor(version.Version),
		Sections: docs.Sections(s.cfg.DocsURL, version.Version),
	}
	if root := docs.SiteFor(s.cfg.DocsURL, version.Version); root != "" {
		resp.URL = &root
	}
	if skills := docs.AgentSkills(s.cfg.DocsURL, version.Version); skills != "" {
		resp.AgentSkills = &skills
	}
	if resp.Sections == nil {
		resp.Sections = []docs.Section{}
	}
	writeJSON(w, http.StatusOK, resp)
}
