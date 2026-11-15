package toolchain

import (
	"strings"
)

// PEP440Allows evaluates a PEP 440 version specifier set — pyproject.toml's
// `requires-python` — against a concrete interpreter version.
//
// **A reading, never a pin** (ADR §64). Toft resolves a Python version from
// `[runtime] python` in toft.toml alone; this exists so a build can say that the
// interpreter it chose is one the project itself excludes, and tell the owner
// how to pin. ok is false for a specifier this does not understand (a
// pre-release, a local version, arbitrary equality), and the caller then says
// nothing rather than guess.
//
// The grammar is the spec's: comma means AND; ==, !=, <=, >=, <, >, ~=; a
// trailing `.*` on == and != only. Comparison pads with zeros, so 3.13.5 is
// not `<3.13` and not `<=3.13`, exactly as pip and uv read it.
func PEP440Allows(spec, version string) (allowed, ok bool) {
	v, err := parseVersion(version)
	if err != nil {
		return false, false
	}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true, true
	}
	allowed = true
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			return false, false
		}
		op := ""
		for _, candidate := range []string{"===", "~=", "==", "!=", "<=", ">=", "<", ">"} {
			if strings.HasPrefix(clause, candidate) {
				op = candidate
				break
			}
		}
		if op == "" || op == "===" {
			return false, false
		}
		target := strings.TrimSpace(strings.TrimPrefix(clause, op))
		wildcard := strings.HasSuffix(target, ".*")
		if wildcard {
			if op != "==" && op != "!=" {
				return false, false
			}
			target = strings.TrimSuffix(target, ".*")
		}
		tv, err := parseVersion(strings.TrimPrefix(target, "v"))
		if err != nil {
			return false, false
		}
		width := 3
		if len(tv) > width {
			width = len(tv)
		}
		cmp := compare(v, tv, width)
		var match bool
		switch op {
		case "==":
			if wildcard {
				match = compare(v, tv, len(tv)) == 0
			} else {
				match = cmp == 0
			}
		case "!=":
			if wildcard {
				match = compare(v, tv, len(tv)) != 0
			} else {
				match = cmp != 0
			}
		case "<=":
			match = cmp <= 0
		case ">=":
			match = cmp >= 0
		case "<":
			match = cmp < 0
		case ">":
			match = cmp > 0
		case "~=":
			// ~=X.Y means >=X.Y, ==X.*: at least two components, and the
			// prefix is all but the last.
			if len(tv) < 2 {
				return false, false
			}
			match = cmp >= 0 && compare(v, tv[:len(tv)-1], len(tv)-1) == 0
		}
		if !match {
			allowed = false
		}
	}
	return allowed, true
}

// Compare orders two dotted versions at three components: -1, 0, +1. An
// unparseable version orders below every parseable one.
func Compare(a, b string) int {
	pa, errA := parseVersion(a)
	pb, errB := parseVersion(b)
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}
	return compare(pa, pb, 3)
}
