package advisory

import (
	"strconv"
	"strings"
)

// Semantic versioning, as npm and Go both spell it.
//
// One implementation for two ecosystems because the ordering rules are the
// same one document (semver.org); what differs is decoration, and the
// decoration is stripped before comparison. Go writes a leading "v" and an
// occasional "+incompatible"; npm writes neither. The major version in a Go
// module path ("example.com/x/v2") is part of the *package name*, not the
// version string, so it never reaches here.
//
// Build metadata is ignored in comparison, per the specification, which is
// what makes 1.0.0+abc and 1.0.0+def the same version rather than two
// versions with an arbitrary order between them.

// semverVersion is a parsed version. The prerelease identifiers are kept as
// written so that the numeric/alphanumeric distinction the specification draws
// survives to comparison time.
type semverVersion struct {
	major, minor, patch int
	// pre is the dot-separated prerelease identifiers, empty for a release.
	pre []string
}

// parseSemver reads a version strictly. It returns ok=false rather than
// guessing, and every caller treats "cannot compare" as "no match" and says so
// somewhere a person will read it — a matcher that invents an ordering for a
// string it does not understand is a matcher that can refuse a deploy on no
// evidence.
func parseSemver(raw string) (semverVersion, bool) {
	s := strings.TrimSpace(raw)
	// Go's decoration, both of which are noise to an ordering.
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimSuffix(s, "+incompatible")
	if s == "" {
		return semverVersion{}, false
	}
	// Build metadata is ignored, per the specification.
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}

	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
		// "1.0.0-" is not a prerelease, it is a malformed version. Accepting it
		// would make it sort below 1.0.0 on the strength of a stray hyphen.
		if pre == "" {
			return semverVersion{}, false
		}
	}

	parts := strings.Split(s, ".")
	// A bare "1" or "1.2" is not strict semver, and npm's own registry never
	// publishes one — but an advisory range routinely writes "0" for
	// "introduced at the beginning", and both npm and Go tolerate partial
	// versions in ranges. Missing components read as zero, which is the only
	// reading that makes "introduced: 0" mean what every feed intends by it.
	if len(parts) > 3 {
		return semverVersion{}, false
	}
	out := semverVersion{}
	into := []*int{&out.major, &out.minor, &out.patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			// A leading zero is forbidden by the specification, and tolerating
			// it would make "1.01.0" and "1.1.0" two spellings of one version.
			return semverVersion{}, false
		}
		*into[i] = n
	}
	if pre != "" {
		out.pre = strings.Split(pre, ".")
		for _, id := range out.pre {
			if id == "" {
				return semverVersion{}, false
			}
		}
	}
	return out, true
}

// compareSemver orders two parsed versions: -1, 0 or 1.
func compareSemver(a, b semverVersion) int {
	if c := cmpInt(a.major, b.major); c != 0 {
		return c
	}
	if c := cmpInt(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmpInt(a.patch, b.patch); c != 0 {
		return c
	}
	return comparePrerelease(a.pre, b.pre)
}

// comparePrerelease implements the specification's rule 11, which is the part
// everybody gets wrong: a version *with* a prerelease is lower than the same
// version without one (1.0.0-rc1 < 1.0.0), numeric identifiers compare
// numerically and are lower than alphanumeric ones, and when every shared
// identifier is equal the version with more of them is higher.
func comparePrerelease(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		// A release outranks any prerelease of the same version.
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aNum := numericIdentifier(a[i])
		bn, bNum := numericIdentifier(b[i])
		switch {
		case aNum && bNum:
			if c := cmpInt(an, bn); c != 0 {
				return c
			}
		case aNum:
			// Numeric identifiers always have lower precedence.
			return -1
		case bNum:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a), len(b))
}

// numericIdentifier reports whether a prerelease identifier is all digits, and
// its value. A leading zero disqualifies it, per the specification, which is
// why this is not just strconv.Atoi.
func numericIdentifier(s string) (int, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
