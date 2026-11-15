package docs

import (
	"strings"
	"testing"

	"toft/internal/version"
)

// The failure this guards is silent: a link built from a constant, or from the
// catalog, compiles and renders and points every install at documentation for
// a version it is not running, and no screen would ever say so.
func TestTheLinkIsTheRunningMinor(t *testing.T) {
	m := Minor(version.Version)
	if m == "" {
		t.Fatalf("version.Version %q has no minor", version.Version)
	}
	got := SiteFor("https://example.github.io/toft-docs", version.Version)
	if want := "https://example.github.io/toft-docs/" + m + "/"; got != want {
		t.Errorf("SiteFor = %q, want %q", got, want)
	}
	for _, s := range Sections("https://example.github.io/toft-docs/", version.Version) {
		if !strings.HasPrefix(s.URL, got) || !strings.HasSuffix(s.URL, "/"+s.ID+"/") {
			t.Errorf("section %s links %q, not under %q", s.ID, s.URL, got)
		}
	}
}

func TestMinor(t *testing.T) {
	for v, want := range map[string]string{
		"0.7.0": "0.7", "0.7.12": "0.7", "v1.2.3": "1.2", "10.20.3-rc1": "10.20",
		"": "", "0": "", "x.y.z": "", "1..2": "", "dev": "",
	} {
		if got := Minor(v); got != want {
			t.Errorf("Minor(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestNoVersionLinksTheSiteRootAndOffLinksNothing(t *testing.T) {
	if got := SiteFor("https://d.example/docs", "dev"); got != "https://d.example/docs/" {
		t.Errorf("SiteFor with no minor = %q", got)
	}
	if SiteFor("", "0.7.0") != "" || Sections("", "0.7.0") != nil {
		t.Error("DOCS_URL=off still produced a link")
	}
	if n := len(Sections("https://d.example", "0.7.0")); n != 3 {
		t.Errorf("%d sections, want the three guides", n)
	}
}

func TestTheAgentSkillsPageIsUnderTheVersionAndAbsentUnderOff(t *testing.T) {
	if got := AgentSkills("https://d.example/docs", "0.7.2"); got != "https://d.example/docs/0.7/cli/cli/agents/" {
		t.Errorf("AgentSkills = %q", got)
	}
	if AgentSkills("", "0.7.2") != "" {
		t.Error("DOCS_URL=off still links the agent skills")
	}
}
