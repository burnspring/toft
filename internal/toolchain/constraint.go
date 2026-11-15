package toolchain

import (
	"fmt"
	"strings"
)

// A range, and why it is spelled the way it is.
//
//	">=3.12,<3.14"     a range
//	">=20"             open above
//	"24"               exact, shorthand for "==24"
//
// Comma means AND. **No `^`, no `~`, no `~=`, no `||`** — not because they are
// unfamiliar, but because they are wrong here (ADR §64 D13).
//
// `^3.12` in node-semver means `>=3.12.0 <4.0.0`: it infers compatibility from
// position, on the assumption that its subject follows semver. **Python does
// not.** A Python minor bump is a breaking change — 3.12 and 3.13 have
// different ABIs and different syntax — and there has been no major bump since
// 3.0 and will not be another. So `^` would match 3.13 through 3.99 for a
// project that meant 3.12 only. PEP 440's `~=` has the mirror-image trap in the
// other direction.
//
// Plain comparators mean the same thing in node-semver and PEP 440 both, need
// no per-language grammar, and `>=24,<25` says exactly what `^24` was reaching
// for.

// operators, longest first so that ">=" is not read as ">".
var operators = []string{">=", "<=", "==", ">", "<"}

// refusedOperators are the ones that would appear to work. Each carries the
// spelling to use instead, because a refusal that only says no makes somebody
// go and read a grammar.
var refusedOperators = []struct{ token, why string }{
	{"^", "`^` infers a compatible range from position, which is a semver assumption and " +
		"wrong for Python. Write the range you mean: `>=3.12,<3.13`"},
	{"~=", "`~=` is PEP 440's compatible-release operator and means something different in " +
		"every other ecosystem. Write the range you mean: `>=3.12,<3.13`"},
	{"~", "`~` infers a range from position. Write the range you mean: `>=3.12,<3.13`"},
	{"||", "there is no OR. A version range here is a single interval — write the interval, " +
		"or drop the pin"},
	{"!=", "there is no exclusion. Write the interval you want instead"},
}

// wildcardRefusal is said for a `*` anywhere but a trailing `.*` on `==` — the
// one place PEP 440 allows it, and the one place it means a release line, which
// is what a pin here already means.
const wildcardRefusal = "a wildcard is read only as a trailing `.*` on `==`, as in " +
	"`==3.13.*`. Omit the pin to take this install's default, or write a range"

type term struct {
	op string
	v  []int
	// prefix is a `==X.Y.*` term: it matches every version whose leading
	// components are v, compared at len(v) rather than the language's unit.
	prefix bool
}

// Constraint is a parsed range, bound to the language whose matching unit it
// compares at.
type Constraint struct {
	lang  string
	raw   string
	terms []term
}

// Raw is the range as the developer wrote it, for a message that quotes them
// back rather than a normalized form they would not recognise.
func (c Constraint) Raw() string { return c.raw }

// IsZero reports the absence of a pin, which is not the same as a pin that
// matches everything: an app with no [runtime] table takes the install default,
// and the build log says so in those words.
func (c Constraint) IsZero() bool { return len(c.terms) == 0 }

// ParseConstraint reads a range for one language.
//
// The component count is checked against the language's matching unit and a
// mismatch is refused rather than truncated. `>=3.12.1` on Python would
// otherwise silently become `>=3.12`, which is a different statement from the
// one somebody wrote — and quietly meaning something other than what a pin says
// is the exact failure a pin exists to prevent.
func ParseConstraint(lang, s string) (Constraint, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Constraint{lang: lang}, nil
	}
	c := Constraint{lang: lang, raw: raw}

	for _, bad := range refusedOperators {
		if strings.Contains(raw, bad.token) {
			return Constraint{}, fmt.Errorf("%q is not a version range this platform reads: %s", raw, bad.why)
		}
	}

	unit := MatchUnit(lang)
	for _, piece := range strings.Split(raw, ",") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			return Constraint{}, fmt.Errorf("%q has an empty comparator; comma means AND, and "+
				"each side of it has to say something", raw)
		}
		op, explicit := "==", false
		for _, candidate := range operators {
			if strings.HasPrefix(piece, candidate) {
				op, explicit = candidate, true
				piece = strings.TrimSpace(strings.TrimPrefix(piece, candidate))
				break
			}
		}
		if op == "==" && strings.HasPrefix(piece, "=") {
			piece = strings.TrimSpace(strings.TrimPrefix(piece, "="))
		}
		// `==3.13.*` is PEP 440's spelling of the release line, and it is what
		// a Python developer copies out of requires-python. Accepted at or above
		// the matching unit's width — `==3.*` is a range, `==3.13.*` the line —
		// and refused anywhere else a `*` appears, the bare shorthand included:
		// PEP 440 has no wildcard without its operator either.
		prefix := false
		if op == "==" && explicit && strings.HasSuffix(piece, ".*") {
			prefix = true
			piece = strings.TrimSuffix(piece, ".*")
		}
		if strings.Contains(piece, "*") {
			return Constraint{}, fmt.Errorf("%q is not a version range this platform reads: %s", raw, wildcardRefusal)
		}
		parts, err := parseVersion(piece)
		if err != nil {
			return Constraint{}, fmt.Errorf("%q is not a version range this platform reads: %v", raw, err)
		}
		if prefix && len(parts) <= unit {
			c.terms = append(c.terms, term{op: op, v: parts, prefix: true})
			continue
		}
		if len(parts) != unit {
			example := exampleFor(lang)
			if !strings.Contains(raw, ",") {
				example = rewriteAtUnit(op, explicit, parts, unit)
			}
			return Constraint{}, fmt.Errorf("%q names %s where %s is matched on %s: write %q",
				raw, pluralWord(len(parts), "1 version component", fmt.Sprintf("%d version components", len(parts))),
				Label(lang), unitPhrase(lang), example)
		}
		c.terms = append(c.terms, term{op: op, v: parts})
	}
	return c, nil
}

// rewriteAtUnit is what the author wrote, said at the width the language is
// matched on: node "22.1.0" is "22" and python "3.13.18" is "3.13" — the
// line they meant, not a fixed example naming a different one (BB-035). Too
// short a version becomes its release line: python "3" is "==3.*", and a
// bound is padded, ">=3" being ">=3.0".
func rewriteAtUnit(op string, explicit bool, parts []int, unit int) string {
	nums := make([]string, 0, unit)
	for i := 0; i < unit && i < len(parts); i++ {
		nums = append(nums, fmt.Sprint(parts[i]))
	}
	prefix := ""
	if explicit {
		prefix = op
	}
	if len(parts) < unit {
		if op == "==" {
			return "==" + strings.Join(nums, ".") + ".*"
		}
		for len(nums) < unit {
			nums = append(nums, "0")
		}
	}
	return prefix + strings.Join(nums, ".")
}

func unitPhrase(lang string) string {
	if MatchUnit(lang) == 1 {
		return "its major version alone — patch and minor releases of one line are not " +
			"distinguished here"
	}
	return "major.minor — patch releases of one line are not distinguished here"
}

func exampleFor(lang string) string {
	switch lang {
	case Node:
		return "24"
	case Go:
		return ">=1.26"
	default:
		return ">=3.12,<3.14"
	}
}

// Match reports whether a version satisfies every term, compared at the
// language's matching unit.
func (c Constraint) Match(v string) bool {
	if c.IsZero() {
		return true
	}
	parts, err := parseVersion(v)
	if err != nil {
		return false
	}
	unit := MatchUnit(c.lang)
	for _, t := range c.terms {
		if t.prefix {
			if compare(parts, t.v, len(t.v)) != 0 {
				return false
			}
			continue
		}
		cmp := compare(parts, t.v, unit)
		ok := false
		switch t.op {
		case ">=":
			ok = cmp >= 0
		case "<=":
			ok = cmp <= 0
		case ">":
			ok = cmp > 0
		case "<":
			ok = cmp < 0
		case "==":
			ok = cmp == 0
		}
		if !ok {
			return false
		}
	}
	return true
}

// NoMatchError is a refusal that carries both halves of what a developer needs:
// the range they asked for and the set this install actually has.
//
// The set travels in the error rather than being looked up by whoever prints
// it, because /system/health is admin-only and the developer being refused is
// not an administrator — the refusal message is how the available set reaches
// them, and it is the only new surface this feature needs (D9).
type NoMatchError struct {
	Language  string
	Range     string
	Available []Version
	// Where names the set that was searched, so a refusal on an isolated app
	// does not read as a statement about the build host.
	Where string
}

func (e *NoMatchError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("pins %s %s, and %s has no %s at all",
			Label(e.Language), e.Range, e.Where, Label(e.Language))
	}
	have := make([]string, 0, len(e.Available))
	for _, v := range e.Available {
		have = append(have, v.Series())
	}
	return fmt.Sprintf("pins %s %s; %s has %s",
		Label(e.Language), e.Range, e.Where, strings.Join(have, ", "))
}

// Resolve picks the highest version in `from` that satisfies the range.
//
// **Highest-satisfying, and the answer is recorded** (D14). A range makes the
// outcome depend on install state, so a redeploy months later can legitimately
// resolve differently; writing the resolved version down and printing it is
// what makes that visible rather than mysterious.
//
// **Nothing satisfying is a refusal, never an approximation** (D3). No
// nearest-match, no rounding, no falling back to the default. A build that
// quietly substituted 3.13 for a range meaning 3.12 would reintroduce the exact
// class of failure the pin exists to prevent, while printing a success.
func Resolve(lang, constraint string, from []Version, where string) (Version, error) {
	c, err := ParseConstraint(lang, constraint)
	if err != nil {
		return Version{}, err
	}
	return ResolveParsed(c, from, where)
}

// ResolveParsed is Resolve for a range that has already been parsed — the
// builder parses at manifest-read time so a malformed range is refused before
// anything is built.
func ResolveParsed(c Constraint, from []Version, where string) (Version, error) {
	sorted := make([]Version, len(from))
	copy(sorted, from)
	sortDescending(sorted)

	if c.IsZero() {
		for _, v := range sorted {
			if v.Default {
				return v, nil
			}
		}
		if len(sorted) > 0 {
			return sorted[0], nil
		}
		return Version{}, &NoMatchError{Language: c.lang, Range: "", Where: where}
	}

	for _, v := range sorted {
		if c.Match(v.Version) {
			return v, nil
		}
	}
	return Version{}, &NoMatchError{
		Language: c.lang, Range: c.raw, Available: sorted, Where: where,
	}
}

// MatchSeries reports whether two versions are the same release line — the
// question the runner asks about its own image at bootstrap, and the builder
// asks when it looks for a host interpreter matching a declared runner version.
func MatchSeries(lang, a, b string) bool {
	pa, errA := parseVersion(a)
	pb, errB := parseVersion(b)
	if errA != nil || errB != nil {
		return false
	}
	return compare(pa, pb, MatchUnit(lang)) == 0
}
