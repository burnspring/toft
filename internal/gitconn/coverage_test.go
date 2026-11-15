package gitconn

import (
	"strings"
	"testing"

	"toft/internal/database"
)

func TestParseRepo(t *testing.T) {
	for _, c := range []struct {
		url         string
		owner, name string
		wantErr     bool
	}{
		{url: "https://github.com/acme/reports", owner: "acme", name: "reports"},
		{url: "https://github.com/acme/reports.git", owner: "acme", name: "reports"},
		{url: "https://github.com/acme/reports/", owner: "acme", name: "reports"},
		// GitLab nests subgroups, and the owner is everything before the last
		// segment. GitHub never does, so one rule reads both.
		{url: "https://gitlab.com/acme/team/payroll", owner: "acme/team", name: "payroll"},
		{url: "https://github.com/acme", wantErr: true},
		{url: "https://github.com/", wantErr: true},
		{url: "not a url at all", wantErr: true},
	} {
		repo, err := ParseRepo(c.url)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s parsed as %s, want a refusal", c.url, repo)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.url, err)
			continue
		}
		if repo.Owner != c.owner || repo.Name != c.name {
			t.Errorf("%s = %s, want %s/%s", c.url, repo, c.owner, c.name)
		}
	}
}

// The matcher is pure, and the property that matters most is that an owner
// compares **whole**: `acme` admitting `acme-evil` is the same class of mistake
// as `HasSuffix` on a host, which the source policy already refuses to make.
func TestCovers(t *testing.T) {
	repo := Repo{Owner: "acme", Name: "reports"}
	for _, c := range []struct {
		pattern string
		want    bool
	}{
		{"acme/*", true},
		{"acme/reports", true},
		{"ACME/Reports", true}, // both hosts fold case
		{"acme-evil/*", false},
		{"acm/*", false},
		{"acme/report", false},
		{"acme/reportsx", false},
		{"*", false},        // no owner at all
		{"acme/", false},    // no name at all
		{"/reports", false}, // no owner at all
		{"", false},
	} {
		if got := Covers(c.pattern, repo); got != c.want {
			t.Errorf("Covers(%q, %s) = %v, want %v", c.pattern, repo, got, c.want)
		}
	}
}

// A refusal a person can act on says what would work, and this one has to name
// both the repository and the connection: neither alone tells them whether to
// change the URL or to ask an administrator.
func TestCoverageRefusalNamesBoth(t *testing.T) {
	conn := &database.GitConnection{DisplayName: "Acme GitHub", BaseURL: "https://github.com", Coverage: "acme/*\nlabs/payroll"}

	if why := CoverageRefusal(conn, "https://github.com/acme/reports"); why != "" {
		t.Errorf("a covered repository was refused: %q", why)
	}
	if why := CoverageRefusal(conn, "https://github.com/labs/payroll"); why != "" {
		t.Errorf("a named repository was refused: %q", why)
	}

	why := CoverageRefusal(conn, "https://github.com/elsewhere/thing")
	for _, want := range []string{"elsewhere/thing", "Acme GitHub", "acme/*", "labs/payroll"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal does not name %q: %q", want, why)
		}
	}

	// **A connection covering nothing covers nothing**, which is the direction
	// a boundary has to fail in. Empty patterns are not a wildcard.
	empty := &database.GitConnection{DisplayName: "New", Coverage: ""}
	if why := CoverageRefusal(empty, "https://github.com/anything/at-all"); why == "" {
		t.Error("a connection with no coverage admitted a repository")
	}
}

// A connection's credential goes only to its own host: coverage speaks in
// owner/repo, so the same path on another host must be refused.
func TestCoverageRefusesAnotherHost(t *testing.T) {
	conn := &database.GitConnection{DisplayName: "Acme GitHub", BaseURL: "https://github.com", Coverage: "acme/*"}
	for _, u := range []string{
		"https://attacker.example/acme/payroll.git",
		"https://github.com.evil.example/acme/payroll.git",
		"http://github.com/acme/payroll.git",
		"https://github.com:8443/acme/payroll.git",
	} {
		if CoverageRefusal(conn, u) == "" {
			t.Errorf("%s was admitted for a github.com connection", u)
		}
	}
	for _, u := range []string{"https://github.com/acme/payroll.git", "https://GitHub.com/acme/payroll"} {
		if why := CoverageRefusal(conn, u); why != "" {
			t.Errorf("%s was refused: %s", u, why)
		}
	}
}
