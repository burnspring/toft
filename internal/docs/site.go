// Package docs is the one place a link to Toft's documentation is composed.
//
// The manuals are a public site published one folder per minor (ADR, *The
// manual*): `<site>/0.7/`, `<site>/0.8/`. An install links to the folder for
// the version it runs, so the version comes from version.Version — never from
// configuration, where an operator who wrote `…/0.7` would keep 0.7 docs through
// every upgrade, and never from the catalog, which describes the gallery.
//
// Nothing here makes a request. A link is only ever followed by a browser.
package docs

import (
	"strconv"
	"strings"
)

// Section is one of the three guides the site holds.
type Section struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	URL     string `json:"url"`
}

// sections is the specification: a fourth guide is a change to the site and
// this list together, and the Documentation page renders exactly these.
var sections = []Section{
	{ID: "user", Name: "User guide", Summary: "Deploying, running and sharing an app, and finding your way around Toft."},
	{ID: "admin", Name: "Admin guide", Summary: "Running an install: identity, policy, capabilities, operations and security."},
	{ID: "cli", Name: "CLI & SDK guide", Summary: "The toft command and the app SDKs, for a developer at a terminal."},
}

// Minor is "X.Y" from a version "X.Y.Z", or "" when version is not of that shape.
func Minor(version string) string {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return ""
	}
	for _, p := range parts[:2] {
		if _, err := strconv.Atoi(p); err != nil || p == "" {
			return ""
		}
	}
	return parts[0] + "." + parts[1]
}

// SiteFor is the folder of site documenting version: "<site>/<minor>/". A
// version that has no minor yields "<site>/", where the site sends a reader to
// the newest documentation. An empty site — DOCS_URL=off — yields "".
func SiteFor(site, version string) string {
	if site == "" {
		return ""
	}
	site = strings.TrimRight(site, "/")
	if m := Minor(version); m != "" {
		return site + "/" + m + "/"
	}
	return site + "/"
}

// Sections is the three guides at the folder SiteFor names, or nil when site
// is empty.
func Sections(site, version string) []Section {
	root := SiteFor(site, version)
	if root == "" {
		return nil
	}
	out := make([]Section, len(sections))
	for i, s := range sections {
		s.URL = root + s.ID + "/"
		out[i] = s
	}
	return out
}

// AgentSkillsPath is the page that lists the two agent skills published with
// the client (toft-cli and toft-sdk) and how to install them. A page of the
// site rather than a link to the client's repository, so it follows the
// install's version and a mirror like everything else here.
const AgentSkillsPath = "cli/cli/agents/"

// AgentSkills is that page at the folder SiteFor names, or "" under off.
func AgentSkills(site, version string) string {
	root := SiteFor(site, version)
	if root == "" {
		return ""
	}
	return root + AgentSkillsPath
}
