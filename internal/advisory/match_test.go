package advisory

import "testing"

// The matcher's tests are table-driven and written against each ecosystem's
// own edge cases rather than against a corpus, because a corpus test passes
// for as long as the corpus happens not to contain the case that breaks it.

func TestCompareNPMSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.0.0", "1.0.10", -1},

		// The rule everybody gets wrong: a prerelease is *below* the release.
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha", 1},
		{"1.0.0-alpha.1", "1.0.0-alpha.2", -1},
		// Numeric identifiers compare numerically, so 2 < 10 rather than
		// "10" < "2".
		{"1.0.0-alpha.2", "1.0.0-alpha.10", -1},
		// A numeric identifier has lower precedence than an alphanumeric one.
		{"1.0.0-1", "1.0.0-alpha", -1},

		// Build metadata is ignored entirely.
		{"1.0.0+abc", "1.0.0+def", 0},
		{"1.0.0+abc", "1.0.0", 0},

		// Partial versions read missing components as zero, which is what
		// makes an advisory's "introduced: 0" mean what every feed intends.
		{"1", "1.0.0", 0},
		{"1.2", "1.2.0", 0},
		{"0", "0.0.0", 0},
	}
	for _, tt := range tests {
		got, ok := Compare(EcosystemNPM, tt.a, tt.b)
		if !ok {
			t.Errorf("Compare(npm, %q, %q) could not compare", tt.a, tt.b)
			continue
		}
		if got != tt.want {
			t.Errorf("Compare(npm, %q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		// Antisymmetry, checked on every row rather than in a case of its own:
		// a comparator that is wrong in one direction only is the kind of bug
		// that produces a range matching from one end and not the other.
		if back, _ := Compare(EcosystemNPM, tt.b, tt.a); back != -tt.want {
			t.Errorf("Compare(npm, %q, %q) = %d, want %d (antisymmetry)", tt.b, tt.a, back, -tt.want)
		}
	}
}

func TestCompareGoSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		// Go's leading "v" is decoration and must not change the ordering,
		// including when only one side carries it — advisory ranges and go.sum
		// do not always agree about writing it.
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.0", "1.0.0", 0},
		{"v1.2.3", "v1.2.4", -1},
		{"v2.0.0+incompatible", "v2.0.0", 0},
		{"v1.0.0-rc.1", "v1.0.0", -1},
		// A pseudo-version's timestamp component orders as a prerelease, which
		// is what puts an untagged commit below the release it precedes.
		{"v0.0.0-20260101000000-abcdefabcdef", "v0.0.1", -1},
	}
	for _, tt := range tests {
		got, ok := Compare(EcosystemGo, tt.a, tt.b)
		if !ok {
			t.Errorf("Compare(go, %q, %q) could not compare", tt.a, tt.b)
			continue
		}
		if got != tt.want {
			t.Errorf("Compare(go, %q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestComparePEP440(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		// Trailing zeros are not significant.
		{"1.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},

		// The three PEP 440 facts a semver comparator gets wrong, and gets
		// wrong quietly.
		{"1.0.0post1", "1.0.0", 1},    // the PRD's own example
		{"1.0.0.post1", "1.0.0", 1},   // the same version, written properly
		{"1.0.0-1", "1.0.0", 1},       // implicit post-release, not a prerelease
		{"1.0.0.dev1", "1.0.0", -1},   // dev sorts below the release
		{"1.0.0.dev1", "1.0.0a1", -1}, // and below every prerelease of it

		// The alias set: four levels written eight ways.
		{"1.0a1", "1.0alpha1", 0},
		{"1.0b1", "1.0beta1", 0},
		{"1.0rc1", "1.0c1", 0},
		{"1.0rc1", "1.0pre1", 0},
		{"1.0rc1", "1.0preview1", 0},
		{"1.0a1", "1.0b1", -1},
		{"1.0b1", "1.0rc1", -1},
		{"1.0rc1", "1.0", -1},

		// Separators inside a segment are noise.
		{"1.0.a1", "1.0a1", 0},
		{"1.0-a1", "1.0a1", 0},
		{"1.0_a1", "1.0a1", 0},

		// An absent number is zero, but absent-entirely is not the same as
		// zero: 1.0.post0 is above 1.0.
		{"1.0rc", "1.0rc0", 0},
		{"1.0.post0", "1.0", 1},

		// A dev release of a post-release sits below the post-release.
		{"1.0.post1.dev0", "1.0.post1", -1},
		{"1.0.post1.dev0", "1.0", 1},

		// Epochs outrank everything.
		{"1!1.0", "2.0", 1},
		{"1!1.0", "1!2.0", -1},

		// Case and the "v" prefix are folded by PEP 440's own normalization.
		{"1.0RC1", "1.0rc1", 0},
		{"v1.0", "1.0", 0},

		// A local version sorts above the release it decorates.
		{"1.0+local", "1.0", 1},
	}
	for _, tt := range tests {
		got, ok := Compare(EcosystemPyPI, tt.a, tt.b)
		if !ok {
			t.Errorf("Compare(pypi, %q, %q) could not compare", tt.a, tt.b)
			continue
		}
		if got != tt.want {
			t.Errorf("Compare(pypi, %q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if back, _ := Compare(EcosystemPyPI, tt.b, tt.a); back != -tt.want {
			t.Errorf("Compare(pypi, %q, %q) = %d, want %d (antisymmetry)", tt.b, tt.a, back, -tt.want)
		}
	}
}

// A version this binary cannot read is never quietly ordered. The whole point
// of the third value is that a garbage string cannot refuse a deploy.
func TestCompareRefusesWhatItCannotRead(t *testing.T) {
	for _, tt := range []struct{ ecosystem, a, b string }{
		{EcosystemNPM, "not-a-version", "1.0.0"},
		{EcosystemNPM, "1.0.0", ""},
		{EcosystemNPM, "1.0.0-", "1.0.0"},
		{EcosystemNPM, "1.01.0", "1.1.0"}, // leading zero is not semver
		{EcosystemNPM, "1.2.3.4", "1.2.3"},
		{EcosystemPyPI, "not-a-version", "1.0"},
		{EcosystemPyPI, "1.0.0", "%%%"},
		{"alpine", "1.0", "1.1"}, // an ecosystem this binary does not resolve
	} {
		if _, ok := Compare(tt.ecosystem, tt.a, tt.b); ok {
			t.Errorf("Compare(%s, %q, %q) claimed it could compare", tt.ecosystem, tt.a, tt.b)
		}
	}
}

func TestMatchesInterval(t *testing.T) {
	tests := []struct {
		name      string
		ecosystem string
		version   string
		r         Range
		want      Match
	}{
		{
			name: "inside a bounded interval", ecosystem: EcosystemNPM, version: "4.17.15",
			r:    Range{Introduced: "4.0.0", Fixed: "4.17.21"},
			want: Matched,
		},
		{
			// The fixed version is the first *good* one, so the interval is
			// half-open. Getting this inclusive would report every patched app
			// as vulnerable, which is the fastest way to have the feature
			// switched off.
			name: "the fixed version itself is not affected", ecosystem: EcosystemNPM, version: "4.17.21",
			r:    Range{Introduced: "4.0.0", Fixed: "4.17.21"},
			want: NoMatch,
		},
		{
			name: "the introduced version itself is affected", ecosystem: EcosystemNPM, version: "4.0.0",
			r:    Range{Introduced: "4.0.0", Fixed: "4.17.21"},
			want: Matched,
		},
		{
			name: "below the interval", ecosystem: EcosystemNPM, version: "3.9.0",
			r:    Range{Introduced: "4.0.0", Fixed: "4.17.21"},
			want: NoMatch,
		},
		{
			name: "introduced zero means from the beginning", ecosystem: EcosystemNPM, version: "0.0.1",
			r:    Range{Introduced: "0", Fixed: "1.0.0"},
			want: Matched,
		},
		{
			name: "no fix published leaves the range open above", ecosystem: EcosystemNPM, version: "9.9.9",
			r:    Range{Introduced: "1.0.0"},
			want: Matched,
		},
		{
			// Inclusive, unlike fixed. Feeds publish one or the other and
			// occasionally both, and reading last_affected exclusively would
			// under-report by exactly one version.
			name: "last_affected is inclusive", ecosystem: EcosystemNPM, version: "1.2.3",
			r:    Range{Introduced: "1.0.0", LastAffected: "1.2.3"},
			want: Matched,
		},
		{
			name: "past last_affected", ecosystem: EcosystemNPM, version: "1.2.4",
			r:    Range{Introduced: "1.0.0", LastAffected: "1.2.3"},
			want: NoMatch,
		},
		{
			name: "an explicit enumeration matches literally", ecosystem: EcosystemNPM, version: "1.0.0",
			r:    Range{Versions: []string{"0.9.0", "1.0.0"}},
			want: Matched,
		},
		{
			name: "an enumeration that does not list it", ecosystem: EcosystemNPM, version: "1.0.1",
			r:    Range{Versions: []string{"0.9.0", "1.0.0"}},
			want: NoMatch,
		},
		{
			// A record with no version information at all must cover nothing.
			// Reading it as "every version" would blanket an estate on the
			// strength of a malformed advisory.
			name: "a range with no bounds covers nothing", ecosystem: EcosystemNPM, version: "1.0.0",
			r:    Range{},
			want: NoMatch,
		},
		{
			name: "a prerelease inside the interval", ecosystem: EcosystemNPM, version: "2.0.0-rc1",
			r:    Range{Introduced: "1.0.0", Fixed: "2.0.0"},
			want: Matched,
		},
		{
			name: "PyPI post-release past the fix", ecosystem: EcosystemPyPI, version: "1.0.0.post1",
			r:    Range{Introduced: "0", Fixed: "1.0.0"},
			want: NoMatch,
		},
		{
			name: "PyPI dev release before the fix", ecosystem: EcosystemPyPI, version: "1.0.0.dev1",
			r:    Range{Introduced: "0", Fixed: "1.0.0"},
			want: Matched,
		},
		{
			name: "an unreadable version is never affected", ecosystem: EcosystemNPM, version: "banana",
			r:    Range{Introduced: "1.0.0", Fixed: "2.0.0"},
			want: Incomparable,
		},
		{
			name: "an unreadable bound is never affected", ecosystem: EcosystemNPM, version: "1.5.0",
			r:    Range{Introduced: "1.0.0", Fixed: "banana"},
			want: Incomparable,
		},
		{
			// Incomparability must not survive an exact enumeration hit: the
			// feed listed the literal string, and that is an answer.
			name: "an enumeration beats incomparability", ecosystem: EcosystemNPM, version: "banana",
			r:    Range{Versions: []string{"banana"}, Introduced: "1.0.0", Fixed: "2.0.0"},
			want: Matched,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Matches(tt.ecosystem, tt.version, tt.r); got != tt.want {
				t.Errorf("Matches(%s, %q, %+v) = %v, want %v", tt.ecosystem, tt.version, tt.r, got, tt.want)
			}
		})
	}
}

func TestNormalizePackage(t *testing.T) {
	tests := []struct {
		ecosystem, in, want string
	}{
		// PEP 503: fold case, collapse runs of -_. into a single -.
		{EcosystemPyPI, "Flask_SQLAlchemy", "flask-sqlalchemy"},
		{EcosystemPyPI, "Flask.SQLAlchemy", "flask-sqlalchemy"},
		{EcosystemPyPI, "flask--sqlalchemy", "flask-sqlalchemy"},
		{EcosystemPyPI, "zope.interface", "zope-interface"},
		{EcosystemPyPI, "  Django  ", "django"},

		// npm is not folded: scoped names are compared literally.
		{EcosystemNPM, "@acme/widget", "@acme/widget"},
		{EcosystemNPM, "Lodash", "Lodash"},

		// A Go module path is a case-sensitive URL and the /v2 is part of it.
		{EcosystemGo, "github.com/Acme/Widget/v2", "github.com/Acme/Widget/v2"},
	}
	for _, tt := range tests {
		if got := NormalizePackage(tt.ecosystem, tt.in); got != tt.want {
			t.Errorf("NormalizePackage(%s, %q) = %q, want %q", tt.ecosystem, tt.in, got, tt.want)
		}
	}
}

func TestNormalizeEcosystem(t *testing.T) {
	tests := []struct{ in, want string }{
		{"npm", EcosystemNPM},
		{"PyPI", EcosystemPyPI},
		{"Go", EcosystemGo},
		{" npm ", EcosystemNPM},
		// OSV qualifies some ecosystems with a release; the part before the
		// colon is the ecosystem.
		{"Alpine:v3.16", ""},
		// An ecosystem this platform does not install must fall out of the
		// corpus at ingest, so a package can never be compared under another
		// ecosystem's ordering rules.
		{"Debian", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeEcosystem(tt.in); got != tt.want {
			t.Errorf("NormalizeEcosystem(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Severity is ordered, and unknown is outside the order rather than at one end
// of it. Both halves matter: a threshold cannot be met by an advisory nobody
// scored, and "unknown" is not a threshold anybody can set.
func TestSeverityOrdering(t *testing.T) {
	if !AtOrAbove(SeverityCritical, SeverityHigh) {
		t.Error("critical should meet a high threshold")
	}
	if AtOrAbove(SeverityModerate, SeverityHigh) {
		t.Error("moderate should not meet a high threshold")
	}
	if !AtOrAbove(SeverityHigh, SeverityHigh) {
		t.Error("the threshold itself should be met")
	}
	for _, threshold := range []string{SeverityLow, SeverityModerate, SeverityHigh, SeverityCritical} {
		if AtOrAbove(SeverityUnknown, threshold) {
			t.Errorf("an unscored advisory must not meet the %s threshold", threshold)
		}
	}
	if AtOrAbove(SeverityCritical, SeverityUnknown) {
		t.Error("unknown is not a threshold")
	}
	if ValidSeverity(SeverityUnknown) {
		t.Error("unknown is a stored value, not a settable threshold")
	}
}

func TestSeverityFromCVSS(t *testing.T) {
	for _, tt := range []struct {
		score float64
		want  string
	}{
		{9.8, SeverityCritical},
		{9.0, SeverityCritical},
		{8.9, SeverityHigh},
		{7.0, SeverityHigh},
		{6.9, SeverityModerate},
		{4.0, SeverityModerate},
		{3.9, SeverityLow},
		{0.1, SeverityLow},
		{0, SeverityUnknown},
	} {
		if got := SeverityFromCVSS(tt.score); got != tt.want {
			t.Errorf("SeverityFromCVSS(%v) = %q, want %q", tt.score, got, tt.want)
		}
	}
}

// CRAN's version ordering, which is neither semver nor PEP 440.
//
// R package versions are integers separated by "." **or "-"**, and R's own
// compareVersion treats the two separators as equivalent. Borrowing semver
// would reject the dash form outright; borrowing PEP 440 would read "-3" as a
// pre-release marker and order 1.2-3 *below* 1.2, which is backwards.
//
// The row that matters most is 1.2-10 against 1.2-3. Under any lexical
// comparison 10 sorts below 3, so an advisory covering "< 1.2-9" would silently
// stop matching the versions it was written for — a hole in a control with
// nothing to show for it.
func TestCRANVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.2-3", "1.2-10", -1},
		{"1.2-10", "1.2-3", 1},
		{"1.2-3", "1.2-3", 0},
		// The two separators are the same separator.
		{"1.2-3", "1.2.3", 0},
		// A shorter version is padded with zeros: 1.2 and 1.2.0 are equal.
		{"1.2", "1.2.0", 0},
		{"1.2", "1.2-1", -1},
		{"0.9.9", "1.0", -1},
		{"2.10", "2.9", 1},
	} {
		got, ok := Compare(EcosystemCRAN, tc.a, tc.b)
		if !ok {
			t.Errorf("Compare(cran, %q, %q) could not parse", tc.a, tc.b)
			continue
		}
		if got != tc.want {
			t.Errorf("Compare(cran, %q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// A version this binary cannot read is refused rather than guessed at, so it
// surfaces as an unreadable version in a log instead of a package that quietly
// stopped being checked under someone else's ordering rules.
func TestAnUnreadableCRANVersionIsRefused(t *testing.T) {
	for _, v := range []string{"", "1.2.beta", "devel", "1.2-3a", "-1"} {
		if _, ok := Compare(EcosystemCRAN, v, "1.0"); ok {
			t.Errorf("Compare(cran, %q, ...) claimed to parse it", v)
		}
	}
}

// CRAN normalises, so a CRAN advisory that does arrive is matched under CRAN's
// rules — while staying out of Ecosystems, which is the separate claim that the
// corpus is worth gating on.
func TestCRANNormalisesButIsNotScanned(t *testing.T) {
	if got := NormalizeEcosystem("CRAN"); got != EcosystemCRAN {
		t.Errorf("NormalizeEcosystem(CRAN) = %q, want %q", got, EcosystemCRAN)
	}
	if !KnownEcosystem(EcosystemCRAN) {
		t.Error("KnownEcosystem(cran) is false, so a CRAN advisory would be compared under no rules")
	}
	for _, e := range Ecosystems {
		if e == EcosystemCRAN {
			t.Error("cran is listed in Ecosystems. OSV carries 14 CRAN advisories in total, so " +
				"every R app would show a permanent '0 findings' — a statement about the feed " +
				"wearing the appearance of one about the app")
		}
	}
}
