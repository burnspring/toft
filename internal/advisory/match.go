package advisory

import "strings"

// The matcher. This file imports nothing but `strings`, and that is the point:
// it is the piece most likely to be got subtly wrong, so it is separable from
// the feed, from the database and from the policy, and it is tested against
// each ecosystem's own edge cases rather than against a corpus.

// Compare orders two versions within one ecosystem.
//
// ok is false when either string cannot be parsed by that ecosystem's rules.
// It is a separate return rather than an error or a silent zero because the
// callers want different things from it: Matches turns it into Incomparable,
// which is never a match, and the deploy gate counts those and logs the count
// (database.FindingsForCounted), so a version this binary cannot read shows up
// as a line in a build log rather than as a package that quietly stopped being
// checked. The estate and sweep reads skip them without a count.
func Compare(ecosystem, a, b string) (int, bool) {
	switch ecosystem {
	case EcosystemPyPI:
		pa, okA := parsePEP440(a)
		pb, okB := parsePEP440(b)
		if !okA || !okB {
			return 0, false
		}
		return comparePEP440(pa, pb), true
	case EcosystemNPM, EcosystemGo:
		sa, okA := parseSemver(a)
		sb, okB := parseSemver(b)
		if !okA || !okB {
			return 0, false
		}
		return compareSemver(sa, sb), true
	case EcosystemCRAN:
		pa, okA := parseCRAN(a)
		pb, okB := parseCRAN(b)
		if !okA || !okB {
			return 0, false
		}
		return compareCRAN(pa, pb), true
	}
	return 0, false
}

// R package versions are a sequence of integers separated by "." **or "-"**,
// and the two separators are equivalent: R's own R_system_version and
// compareVersion treat 1.2-3 and 1.2.3 as the same version. That is neither
// semver nor PEP 440 — semver's parser rejects the dash form outright, and
// PEP 440 would read "-3" as a pre-release marker and order it *below* 1.2,
// which is backwards — so CRAN cannot borrow either implementation.
//
// The failure that would come of borrowing is not a parse error. It is 1.2-10
// ordering below 1.2-3 under a lexical comparison, so an advisory covering
// "< 1.2-9" would silently stop matching the versions it was written for.
func parseCRAN(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	// A leading or trailing separator is not a version with an empty component,
	// it is not a version at all — FieldsFunc would silently drop it and read
	// "-1" as 1.
	if strings.HasPrefix(v, ".") || strings.HasPrefix(v, "-") ||
		strings.HasSuffix(v, ".") || strings.HasSuffix(v, "-") {
		return nil, false
	}
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' })
	if len(fields) == 0 {
		return nil, false
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		// Parsed by hand rather than with strconv, to keep this file's one
		// stated property: it imports nothing but strings, because it is the
		// piece most likely to be got subtly wrong and is meant to stay
		// separable from the feed, the database and the policy.
		if f == "" {
			return nil, false
		}
		n := 0
		for _, r := range f {
			if r < '0' || r > '9' {
				// A non-numeric component is not an R version. Refused rather
				// than guessed at, so it is logged as unreadable instead of
				// quietly comparing as something else.
				return nil, false
			}
			n = n*10 + int(r-'0')
			if n > 1<<30 {
				return nil, false
			}
		}
		out = append(out, n)
	}
	return out, true
}

// compareCRAN orders two parsed R versions. A shorter version is padded with
// zeros, which is R's own rule: 1.2 and 1.2.0 are the same version.
func compareCRAN(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Match is a three-valued answer, because "this range does not cover that
// version" and "this binary could not read one of them" are different facts
// and collapsing them is how a control comes to have a hole nobody can see.
type Match int

const (
	// NoMatch: the version is outside the range.
	NoMatch Match = iota
	// Matched: the version is affected.
	Matched
	// Incomparable: a version string one side or the other could not be
	// parsed. Never treated as affected — refusing a deploy on a string the
	// platform does not understand is a refusal nobody can act on — and never
	// silently discarded either: the deploy gate logs the count and records
	// it on the verdict (database.FindingsForCounted).
	Incomparable
)

// Matches reports whether an installed version falls inside an advisory's
// declared range.
//
// The interval is half-open, [introduced, fixed), which is how every feed
// means it: `fixed` names the first version that is *not* affected. An empty
// `introduced` (or the "0" that feeds write for it) means from the beginning;
// an empty `fixed` means no fix has been published, which is what makes the
// range unbounded above rather than empty.
//
// An explicit `versions` enumeration is checked first and independently: an
// advisory may carry both, and a version listed there is affected regardless
// of whether the bounds also cover it.
func Matches(ecosystem, version string, r Range) Match {
	version = strings.TrimSpace(version)
	if version == "" {
		return Incomparable
	}

	// An exact enumeration is a string comparison, not a version comparison —
	// the feed listed literal versions, and comparing them by ordering rules
	// would be inventing a question nobody asked.
	for _, v := range r.Versions {
		if strings.EqualFold(strings.TrimSpace(v), version) {
			return Matched
		}
	}

	// A range with no bounds at all and no enumeration covers nothing. Feeds
	// do emit these — an advisory that names an ecosystem and package with no
	// version information — and reading it as "every version" would blanket an
	// estate on the strength of a malformed record.
	if !hasBound(r) {
		return NoMatch
	}

	if lo := strings.TrimSpace(r.Introduced); lo != "" && lo != "0" {
		c, ok := Compare(ecosystem, version, lo)
		if !ok {
			return Incomparable
		}
		if c < 0 {
			return NoMatch
		}
	}
	if hi := strings.TrimSpace(r.Fixed); hi != "" {
		c, ok := Compare(ecosystem, version, hi)
		if !ok {
			return Incomparable
		}
		// Exclusive: the fixed version is the first good one.
		if c >= 0 {
			return NoMatch
		}
	}
	if last := strings.TrimSpace(r.LastAffected); last != "" {
		c, ok := Compare(ecosystem, version, last)
		if !ok {
			return Incomparable
		}
		// Inclusive: the last affected version is still affected.
		if c > 0 {
			return NoMatch
		}
	}
	return Matched
}

// hasBound reports whether a range says anything about versions at all.
func hasBound(r Range) bool {
	return strings.TrimSpace(r.Introduced) != "" ||
		strings.TrimSpace(r.Fixed) != "" ||
		strings.TrimSpace(r.LastAffected) != ""
}

// NormalizePackage puts a package name into the one spelling this install
// stores and compares.
//
// Each ecosystem's rule is its own, and getting one wrong is a miss rather
// than a false positive — which is the direction that makes it invisible:
//
//   - **PyPI folds** case and treats runs of "-", "_" and "." as a single "-"
//     (PEP 503). "Flask_SQLAlchemy", "flask-sqlalchemy" and "Flask.SQLAlchemy"
//     are one project, and an advisory filed under one spelling has to match
//     an app that pinned another.
//   - **npm does not fold.** Names are lowercase by registry policy for
//     anything published since 2017, but scoped names are compared literally
//     and "@Acme/x" is not a thing the registry serves. Folding here would be
//     harmless today and wrong the moment it stops being true.
//   - **Go is a module path** — a URL, case-sensitive, and the major version
//     suffix ("/v2") is part of the name. Lowercasing it would merge two
//     genuinely different modules on a case-sensitive host.
func NormalizePackage(ecosystem, name string) string {
	name = strings.TrimSpace(name)
	switch ecosystem {
	case EcosystemPyPI:
		lower := strings.ToLower(name)
		var b strings.Builder
		b.Grow(len(lower))
		prevSep := false
		for _, r := range lower {
			if r == '-' || r == '_' || r == '.' {
				if !prevSep {
					b.WriteByte('-')
					prevSep = true
				}
				continue
			}
			prevSep = false
			b.WriteRune(r)
		}
		return strings.Trim(b.String(), "-")
	default:
		return name
	}
}

// NormalizeEcosystem maps the spellings feeds use onto this package's
// constants, and returns "" for one this binary does not resolve.
//
// Returning empty rather than the raw string is deliberate: an unknown
// ecosystem must fall out of the corpus at ingest, so that a package can never
// be silently compared under another ecosystem's ordering rules. OSV's
// "Alpine", "Debian" and the rest are real, and none of them is a thing this
// platform installs.
func NormalizeEcosystem(raw string) string {
	e := strings.ToLower(strings.TrimSpace(raw))
	// OSV qualifies some ecosystems with a release ("Alpine:v3.16"); the part
	// before the colon is the ecosystem.
	if i := strings.IndexByte(e, ':'); i >= 0 {
		e = e[:i]
	}
	switch e {
	case "npm":
		return EcosystemNPM
	case "pypi":
		return EcosystemPyPI
	case "go":
		return EcosystemGo
	case "cran":
		return EcosystemCRAN
	}
	return ""
}
