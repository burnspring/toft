package advisory

import (
	"regexp"
	"strconv"
	"strings"
)

// PEP 440, which is not semver and disagrees with it in ways that matter here.
//
// Three of the differences decide real comparisons: a post-release sorts
// *above* the release it follows (1.0.0.post1 > 1.0.0, and "1.0.0post1"
// without the separator is the same version), a dev release sorts below
// everything else with the same release number, and the pre-release spellings
// are an alias set — "alpha", "a", "beta", "b", "c", "pre", "preview" and "rc"
// are four levels written eight ways. A semver comparator pointed at PyPI gets
// all three wrong and gets them wrong *quietly*, which is the only kind of
// wrong that matters in a security control.
//
// The grammar is PEP 440's own regular expression, transcribed rather than
// approximated. Approximating it is how "1.0.0-1" (a post-release) comes to be
// read as a prerelease and sorts below the version it is above.

var pep440Re = regexp.MustCompile(`^v?` +
	`(?:(?P<epoch>[0-9]+)!)?` +
	`(?P<release>[0-9]+(?:\.[0-9]+)*)` +
	`(?:[-_\.]?(?P<pre_l>alpha|a|beta|b|preview|pre|c|rc)[-_\.]?(?P<pre_n>[0-9]+)?)?` +
	`(?:(?:-(?P<post_n1>[0-9]+))|(?:[-_\.]?(?P<post_l>post|rev|r)[-_\.]?(?P<post_n2>[0-9]+)?))?` +
	`(?:[-_\.]?(?P<dev_l>dev)[-_\.]?(?P<dev_n>[0-9]+)?)?` +
	`(?:\+(?P<local>[a-z0-9]+(?:[-_\.][a-z0-9]+)*))?$`)

// pep440Version is a version reduced to the six fields PEP 440 orders by.
//
// The "has" flags are not redundant with a zero value: `1.0.post0` and `1.0`
// are different versions and the first is higher, so "absent" and "present and
// zero" have to stay tellable apart.
type pep440Version struct {
	epoch   int
	release []int

	hasPre  bool
	preKind string // normalized to "a", "b" or "rc"
	preNum  int

	hasPost bool
	postNum int

	hasDev bool
	devNum int

	local string
}

// parsePEP440 reads a version strictly, returning ok=false rather than
// guessing. Case is folded and a leading "v" dropped, both of which PEP 440's
// own normalization does.
func parsePEP440(raw string) (pep440Version, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	m := pep440Re.FindStringSubmatch(s)
	if m == nil {
		return pep440Version{}, false
	}
	group := func(name string) string { return m[pep440Re.SubexpIndex(name)] }

	out := pep440Version{}
	if e := group("epoch"); e != "" {
		n, err := strconv.Atoi(e)
		if err != nil {
			return pep440Version{}, false
		}
		out.epoch = n
	}
	for _, part := range strings.Split(group("release"), ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return pep440Version{}, false
		}
		out.release = append(out.release, n)
	}

	if l := group("pre_l"); l != "" {
		out.hasPre = true
		out.preKind = normalizePreKind(l)
		// An absent number is zero: "1.0rc" is "1.0rc0".
		out.preNum = atoiOrZero(group("pre_n"))
	}
	// Two spellings of a post-release. "-1" is the implicit form, which is the
	// one an approximate parser reads as a prerelease and sorts the wrong way.
	if n1 := group("post_n1"); n1 != "" {
		out.hasPost = true
		out.postNum = atoiOrZero(n1)
	} else if group("post_l") != "" {
		out.hasPost = true
		out.postNum = atoiOrZero(group("post_n2"))
	}
	// An absent number is zero here too: a bare "1.0.dev" is "1.0.dev0". The
	// literal is what says the segment is present, never the number, because
	// dev0 and no-dev-at-all are different versions.
	if group("dev_l") != "" {
		out.hasDev = true
		out.devNum = atoiOrZero(group("dev_n"))
	}
	out.local = group("local")
	return out, true
}

func normalizePreKind(l string) string {
	switch l {
	case "alpha", "a":
		return "a"
	case "beta", "b":
		return "b"
	default:
		// "c", "pre", "preview" and "rc" are one level, spelled four ways.
		return "rc"
	}
}

func atoiOrZero(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// comparePEP440 orders two parsed versions: -1, 0 or 1.
//
// It follows the reference implementation's sort key exactly, including the
// two infinities that make the ordering come out right:
//
//   - a version that is *only* a dev release sorts below every prerelease of
//     the same release number, so its prerelease slot is minus infinity;
//   - a version with no prerelease at all sorts above every prerelease of the
//     same release number, so its prerelease slot is plus infinity;
//   - an absent post-release is minus infinity, and an absent dev release is
//     plus infinity, which is what puts 1.0.post1.dev0 below 1.0.post1.
func comparePEP440(a, b pep440Version) int {
	if c := cmpInt(a.epoch, b.epoch); c != 0 {
		return c
	}
	if c := compareRelease(a.release, b.release); c != 0 {
		return c
	}

	if c := cmpInt(preClass(a), preClass(b)); c != 0 {
		return c
	}
	if a.hasPre && b.hasPre {
		if c := strings.Compare(a.preKind, b.preKind); c != 0 {
			return c
		}
		if c := cmpInt(a.preNum, b.preNum); c != 0 {
			return c
		}
	}

	// Absent post sorts below any post, including post0.
	switch {
	case !a.hasPost && b.hasPost:
		return -1
	case a.hasPost && !b.hasPost:
		return 1
	case a.hasPost && b.hasPost:
		if c := cmpInt(a.postNum, b.postNum); c != 0 {
			return c
		}
	}

	// Absent dev sorts *above* any dev, which is the mirror image of post and
	// the reason 1.0.dev1 < 1.0.
	switch {
	case a.hasDev && !b.hasDev:
		return -1
	case !a.hasDev && b.hasDev:
		return 1
	case a.hasDev && b.hasDev:
		if c := cmpInt(a.devNum, b.devNum); c != 0 {
			return c
		}
	}

	// A local version sorts above the same version without one. Compared as a
	// string rather than segment-wise: local versions do not appear in
	// advisory ranges, and the only property needed here is a total order that
	// puts them on the right side of the release.
	switch {
	case a.local == "" && b.local != "":
		return -1
	case a.local != "" && b.local == "":
		return 1
	}
	return strings.Compare(a.local, b.local)
}

// preClass is the prerelease slot's magnitude: -1 for a bare dev release, 0
// for an actual prerelease, 1 for neither.
func preClass(v pep440Version) int {
	switch {
	case !v.hasPre && !v.hasPost && v.hasDev:
		return -1
	case !v.hasPre:
		return 1
	}
	return 0
}

// compareRelease compares the dotted release segments, treating a missing
// component as zero so that 1.0 and 1.0.0 are one version rather than two.
func compareRelease(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if c := cmpInt(av, bv); c != 0 {
			return c
		}
	}
	return 0
}
