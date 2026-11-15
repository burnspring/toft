package sources

import (
	"strings"
	"testing"
)

// The matcher is the part that must be tested adversarially: every bypass of
// this feature lives in this file's subject.

func mustPattern(t *testing.T, raw string) Pattern {
	t.Helper()
	p, err := ParsePattern(raw)
	if err != nil {
		t.Fatalf("ParsePattern(%q) = %v, want nil", raw, err)
	}
	return p
}

func TestPatternCanonicalForm(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/*":           "github.com/acme/*",
		"github.com/acme":             "github.com/acme",
		"gitlab.internal":             "gitlab.internal/*",
		"gitlab.internal/":            "gitlab.internal/*",
		"GitLab.Internal/Dept/Team/*": "gitlab.internal/dept/team/*",
		"github.com:443/acme/*":       "github.com/acme/*",
		"github.com./acme/*":          "github.com/acme/*",
		"https://github.com/acme/*":   "github.com/acme/*",
		"github.com/oss/lib.git":      "github.com/oss/lib",
		"git.internal:8443/team/*":    "git.internal:8443/team/*",
	}
	for in, want := range cases {
		if got := mustPattern(t, in).String(); got != want {
			t.Errorf("ParsePattern(%q).String() = %q, want %q", in, got, want)
		}
	}
}

func TestPatternRefusals(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"*",
		"*.github.com/acme/*",
		"github.*/acme",
		"github.com/*/tool",
		"github.com/ac*me/tool",
		"gіthub.com/acme/*", // Cyrillic і
		"ssh://git.internal/team/*",
		"github.com/acme/tool?ref=main",
		"user@github.com/acme",
	} {
		if p, err := ParsePattern(raw); err == nil {
			t.Errorf("ParsePattern(%q) = %v, want an error", raw, p)
		}
	}
}

func TestMatching(t *testing.T) {
	acme := mustPattern(t, "github.com/acme/*")
	host := mustPattern(t, "gitlab.internal/*")
	sub := mustPattern(t, "gitlab.internal/dept/*")
	team := mustPattern(t, "gitlab.internal/dept/team/*")
	exact := mustPattern(t, "gitlab.internal/dept/team")
	one := mustPattern(t, "github.com/oss-vendor/lib")

	cases := []struct {
		pattern Pattern
		url     string
		want    bool
	}{
		// The lookalike-host family. Every one of these is the reason hosts are
		// compared for equality rather than by suffix.
		{acme, "https://github.com.evil.internal/acme/x", false},
		{acme, "https://evil-github.com/acme/x", false},
		{acme, "https://notgithub.com/acme/x", false},
		{host, "https://gitlab.internal.evil.com/x/y", false},

		// The lookalike-organization family, for the same reason on the path.
		{acme, "https://github.com/acme-evil/x", false},
		{acme, "https://github.com/acmex/y", false},

		// An organization root is not a repository, and a subtree pattern's
		// strict length inequality is what says so.
		{acme, "https://github.com/acme", false},
		{acme, "https://github.com/acme/dashboard", true},
		{acme, "https://github.com/acme/team/dashboard", true},

		// The spellings that must all normalize to the same repository.
		{acme, "https://github.com/ACME/X.git", true},
		{acme, "https://github.com:443/acme/x", true},
		{acme, "https://github.com./acme/x/", true},
		{acme, "https://github.com/acme/x", true},
		{acme, "https://github.com/acme/x.GIT", true},

		// Nesting: a subgroup is matched by every prefix above it, and by no
		// exact pattern.
		{sub, "https://gitlab.internal/dept/team/repo", true},
		{team, "https://gitlab.internal/dept/team/repo", true},
		{exact, "https://gitlab.internal/dept/team/repo", false},
		{exact, "https://gitlab.internal/dept/team", true},
		{exact, "https://gitlab.internal/dept/team.git", true},
		{host, "https://gitlab.internal/dept/team/repo", true},

		// An exact entry is exactly one repository.
		{one, "https://github.com/oss-vendor/lib", true},
		{one, "https://github.com/oss-vendor/lib/sub", false},
		{one, "https://github.com/oss-vendor/library", false},

		// A port is part of the host unless it is the https default.
		{mustPattern(t, "git.internal:8443/team/*"), "https://git.internal:8443/team/x", true},
		{mustPattern(t, "git.internal:8443/team/*"), "https://git.internal/team/x", false},
		{mustPattern(t, "git.internal/team/*"), "https://git.internal:8443/team/x", false},
	}
	for _, tc := range cases {
		c, err := Normalize(tc.url)
		if err != nil {
			t.Fatalf("Normalize(%q) = %v", tc.url, err)
		}
		if got := tc.pattern.Matches(c); got != tc.want {
			t.Errorf("%s matches %s = %v, want %v", tc.pattern, tc.url, got, tc.want)
		}
	}
}

func TestNormalizeRefusals(t *testing.T) {
	for _, raw := range []string{
		"",
		"http://github.com/acme/x",
		"ssh://git@github.com/acme/x",
		"https://user:pass@github.com/acme/x",
		"https://gіthub.com/acme/x", // Cyrillic і
		"https://github.com//acme/x",
		"https://github.com/acme/../evil",
		"https://github.com/./x",
	} {
		if c, err := Normalize(raw); err == nil {
			t.Errorf("Normalize(%q) = %v, want an error", raw, c)
		}
	}
}

func TestPolicyModes(t *testing.T) {
	allow := Policy{Mode: ModeAllowlist, Patterns: []Pattern{
		mustPattern(t, "github.com/acme/*"),
		mustPattern(t, "gitlab.internal/*"),
	}}

	if err := allow.Allows("https://github.com/acme/tool"); err != nil {
		t.Errorf("an allowed repository was refused: %v", err)
	}
	if err := allow.Allows("https://github.com/randomguy/tool"); err == nil {
		t.Error("a repository on no pattern was allowed")
	}

	any := Policy{Mode: ModeAny}
	if err := any.Allows("https://github.com/randomguy/tool"); err != nil {
		t.Errorf("mode any refused a repository: %v", err)
	}
	// Mode any is today's behavior and must stay it, including for URLs this
	// package cannot parse — git and ValidateRepoURL answer those.
	if err := any.Allows("https://github.com/acme/../x"); err != nil {
		t.Errorf("mode any refused an odd but previously-accepted URL: %v", err)
	}
	// The one rule that applies in every mode.
	if err := any.Allows("https://gіthub.com/acme/x"); err == nil {
		t.Error("mode any accepted a non-ASCII host")
	}

	none := Policy{Mode: ModeNone}
	if err := none.Allows("https://github.com/acme/tool"); err == nil {
		t.Error("mode none allowed a repository")
	}

	// An empty allowlist is saveable and refuses everything, with copy of its
	// own rather than the "approved sources: " sentence and nothing after it.
	empty := Policy{Mode: ModeAllowlist}
	err := empty.Allows("https://github.com/acme/tool")
	if err == nil {
		t.Fatal("an empty allowlist allowed a repository")
	}
	if msg := err.Error(); !contains(msg, "nothing is on it yet") {
		t.Errorf("empty-allowlist refusal reads %q", msg)
	}
}

func TestRefusalNamesTheSourceAndTheWayOut(t *testing.T) {
	p := Policy{Mode: ModeAllowlist, Patterns: []Pattern{
		mustPattern(t, "github.com/acme/*"),
		mustPattern(t, "gitlab.internal"),
	}}
	err := p.Allows("https://github.com/randomguy/tool.git")
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	for _, want := range []string{
		"github.com/randomguy/tool",
		"github.com/acme/*",
		"gitlab.internal/*",
	} {
		if !contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
