package toolchain

import (
	"strings"
	"testing"
)

func TestConstraintMatching(t *testing.T) {
	for _, tc := range []struct {
		lang, c, v string
		want       bool
	}{
		{Python, ">=3.12,<3.14", "3.13.7", true},
		{Python, ">=3.12,<3.14", "3.14.0", false},
		{Python, ">=3.12,<3.14", "3.11.9", false},
		{Python, ">=3.12,<3.14", "3.12.0", true},
		// Patch never matters: 3.13.0 and 3.13.9 are the same answer.
		{Python, "3.13", "3.13.9", true},
		{Python, "3.13", "3.14.0", false},
		{Node, "24", "24.13.0", true},
		{Node, "24", "22.20.0", false},
		{Node, ">=20", "24.13.0", true},
		{Node, ">=20", "18.20.0", false},
		{Go, ">=1.26", "1.26.5", true},
		{Go, ">=1.26", "1.25.9", false},
		{Go, ">=1.26", "1.27.0", true},
	} {
		c, err := ParseConstraint(tc.lang, tc.c)
		if err != nil {
			t.Fatalf("ParseConstraint(%s, %q): %v", tc.lang, tc.c, err)
		}
		if got := c.Match(tc.v); got != tc.want {
			t.Errorf("%s %q matches %s = %v, want %v", tc.lang, tc.c, tc.v, got, tc.want)
		}
	}
}

// D13. Each of these would appear to work, which is what makes refusing them
// worth the words — and the refusal names the spelling to use instead.
func TestRefusedOperators(t *testing.T) {
	for _, c := range []string{"^3.12", "~=3.12", "~3.12", ">=3.12||<3.9", "!=3.11", "3.*", "3.13.*", ">=3.13.*", "==3.*.1"} {
		_, err := ParseConstraint(Python, c)
		if err == nil {
			t.Errorf("%q was accepted; it infers a range from position, which is wrong for Python", c)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), "write the ") &&
			!strings.Contains(err.Error(), "Omit the pin") {
			t.Errorf("%q was refused without saying what to write instead: %v", c, err)
		}
	}
}

// A pin that names more components than the language is matched on is refused
// rather than truncated: >=3.12.1 silently becoming >=3.12 means something
// other than what somebody wrote, which is the failure a pin exists to prevent.
func TestComponentCountIsRefusedNotTruncated(t *testing.T) {
	if _, err := ParseConstraint(Python, ">=3.12.1"); err == nil {
		t.Error("a patch-level Python pin was accepted")
	}
	if _, err := ParseConstraint(Node, "24.1"); err == nil {
		t.Error("a minor-level Node pin was accepted; Node is matched on major")
	}
	if _, err := ParseConstraint(Python, ">=3"); err == nil {
		t.Error("a major-only Python pin was accepted; Python is matched on major.minor")
	}
	err := func() error { _, e := ParseConstraint(Node, "24.1"); return e }()
	if !strings.Contains(err.Error(), "major version") {
		t.Errorf("the refusal does not say what Node is matched on: %v", err)
	}
}

func TestEmptyConstraintIsAbsentNotUniversal(t *testing.T) {
	c, err := ParseConstraint(Python, "")
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsZero() {
		t.Error("an empty range is a pin")
	}
}

// D14: highest-satisfying, not first-listed and not nearest.
func TestResolveTakesTheHighestSatisfying(t *testing.T) {
	set := []Version{
		{Language: Python, Version: "3.11.9", Path: "/a"},
		{Language: Python, Version: "3.13.7", Path: "/c"},
		{Language: Python, Version: "3.12.9", Path: "/b", Default: true},
	}
	got, err := Resolve(Python, ">=3.11,<3.14", set, "this host")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "3.13.7" {
		t.Errorf("resolved %s, want the highest satisfying (3.13.7)", got.Version)
	}

	// With no pin, the install default — not the highest.
	got, err = Resolve(Python, "", set, "this host")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "3.12.9" {
		t.Errorf("with no pin resolved %s, want the install default 3.12.9", got.Version)
	}
}

// D3: refused, never approximated — and the message carries both halves,
// because it is how the available set reaches a developer who is not an admin.
func TestNothingSatisfyingIsARefusalThatNamesTheSet(t *testing.T) {
	set := []Version{
		{Language: Python, Version: "3.12.9"},
		{Language: Python, Version: "3.13.7"},
	}
	_, err := Resolve(Python, ">=3.14", set, "this host")
	if err == nil {
		t.Fatal("a pin nothing satisfies was approximated instead of refused")
	}
	msg := err.Error()
	for _, want := range []string{">=3.14", "3.12", "3.13", "this host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not carry %q", msg, want)
		}
	}
}

func TestResolveAgainstAnEmptySet(t *testing.T) {
	_, err := Resolve(Node, "24", nil, "the node runner image")
	if err == nil {
		t.Fatal("resolved against nothing")
	}
	if !strings.Contains(err.Error(), "the node runner image") {
		t.Errorf("refusal does not say which set was searched: %v", err)
	}
}

func TestMatchSeries(t *testing.T) {
	if !MatchSeries(Python, "3.13.7", "3.13") {
		t.Error("3.13.7 and 3.13 are the same Python line")
	}
	if MatchSeries(Python, "3.13.7", "3.12") {
		t.Error("3.13 and 3.12 are different Python lines")
	}
	if !MatchSeries(Node, "24.13.0", "24") {
		t.Error("24.13.0 and 24 are the same Node line")
	}
}

// PEP 440's `==X.Y.*` is the release line, which is what a pin already means,
// so it is read rather than refused — it is what somebody copies out of
// requires-python.
func TestTrailingWildcardOnEquals(t *testing.T) {
	for _, tc := range []struct {
		lang, c, v string
		want       bool
	}{
		{Python, "==3.13.*", "3.13.7", true},
		{Python, "==3.13.*", "3.12.9", false},
		{Python, "== 3.13.*", "3.13.0", true},
		{Python, "==3.*", "3.11.2", true},
		{Python, "==3.*", "4.0.0", false},
		{Python, "==3.13.*,>=3.12", "3.13.1", true},
		{Node, "==24.*", "24.13.0", true},
		{Node, "==24.*", "22.1.0", false},
	} {
		c, err := ParseConstraint(tc.lang, tc.c)
		if err != nil {
			t.Fatalf("ParseConstraint(%s, %q): %v", tc.lang, tc.c, err)
		}
		if got := c.Match(tc.v); got != tc.want {
			t.Errorf("%s %q matches %s = %v, want %v", tc.lang, tc.c, tc.v, got, tc.want)
		}
	}
	// Wider than the matching unit is still a refusal, not a truncation.
	if _, err := ParseConstraint(Python, "==3.13.1.*"); err == nil {
		t.Error("==3.13.1.* was accepted; Python is matched on major.minor")
	}
}

func TestPEP440Allows(t *testing.T) {
	for _, tc := range []struct {
		spec, v      string
		allowed, ok bool
	}{
		{">=3.10", "3.13.5", true, true},
		{">=3.9,<3.13", "3.13.5", false, true},
		{"<3.13", "3.12.9", true, true},
		{"<=3.12", "3.12.4", false, true}, // the spec's reading, and pip's
		{"==3.12.*", "3.12.4", true, true},
		{"==3.12.*", "3.13.0", false, true},
		{"!=3.11.*", "3.11.2", false, true},
		{"!=3.11.*,>=3.10", "3.12.0", true, true},
		{"~=3.11", "3.12.1", true, true},
		{"~=3.11", "4.0.0", false, true},
		{"~=3.11.2", "3.12.0", false, true},
		{">3.12", "3.12.0", false, true},
		{"", "3.12.0", true, true},
		{">=3.10rc1", "3.12.0", false, false},
		{"===3.12.0", "3.12.0", false, false},
		{">=3.*", "3.12.0", false, false},
		{"3.12", "3.12.0", false, false},
	} {
		allowed, ok := PEP440Allows(tc.spec, tc.v)
		if allowed != tc.allowed || ok != tc.ok {
			t.Errorf("PEP440Allows(%q, %s) = %v, %v; want %v, %v", tc.spec, tc.v, allowed, ok, tc.allowed, tc.ok)
		}
	}
}

// BB-035: the hint is the line the author wrote, at the width it is matched on.
func TestTheWidthHintIsTheAuthorsOwnLine(t *testing.T) {
	for _, tc := range []struct{ lang, raw, want string }{
		{Node, "22.1.0", `write "22"`},
		{Node, ">=22.1", `write ">=22"`},
		{Python, "3.13.18", `write "3.13"`},
		{Python, "3", `write "==3.*"`},
		{Python, ">=3", `write ">=3.0"`},
	} {
		_, err := ParseConstraint(tc.lang, tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %q: %v, want %s", tc.lang, tc.raw, err, tc.want)
		}
	}
}
