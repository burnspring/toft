package advisory

import (
	"math"
	"strings"
)

// CVSS v3 base scoring, because a corpus of mostly-unknown severities makes a
// threshold meaningless.
//
// Most GHSA records carry a qualitative rating of their own and that is always
// preferred — this is only consulted when a feed published a vector and no
// word. PYSEC and a good deal of the Go database are in exactly that position,
// and without this an install with `dependency_block_severity = critical`
// would find that most of its Python findings were unscored and therefore
// never blocking, which is a control that looks switched on and is not.
//
// v2 and v4 vectors are deliberately **not** scored here. v4's base score is a
// lookup over macro-vectors rather than a formula, and approximating it would
// be inventing a severity the publisher did not state — the exact thing the
// unknown level exists to avoid. They read as unknown, which is visible on the
// estate view as a count rather than invisible as a pass.

// cvss3Base computes a CVSS v3.0/v3.1 base score from a vector string, and
// reports whether the vector was one it could read.
//
// The formula is the specification's, transcribed rather than approximated: an
// approximation here would shift advisories across a threshold band, which is
// the one error nobody would ever notice.
func cvss3Base(vector string) (float64, bool) {
	v := strings.ToUpper(strings.TrimSpace(vector))
	if !strings.HasPrefix(v, "CVSS:3.0/") && !strings.HasPrefix(v, "CVSS:3.1/") {
		return 0, false
	}

	metrics := map[string]string{}
	for _, part := range strings.Split(v, "/")[1:] {
		k, val, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		metrics[k] = val
	}

	// Scope changes two of the weightings, so it is read first.
	changed := metrics["S"] == "C"

	av, ok := lookup(map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2}, metrics["AV"])
	if !ok {
		return 0, false
	}
	ac, ok := lookup(map[string]float64{"L": 0.77, "H": 0.44}, metrics["AC"])
	if !ok {
		return 0, false
	}
	prWeights := map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}
	if changed {
		// Privileges Required is worth more when the scope changes, because the
		// attacker's foothold buys them something outside it.
		prWeights = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.50}
	}
	pr, ok := lookup(prWeights, metrics["PR"])
	if !ok {
		return 0, false
	}
	ui, ok := lookup(map[string]float64{"N": 0.85, "R": 0.62}, metrics["UI"])
	if !ok {
		return 0, false
	}
	cia := map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
	c, okC := lookup(cia, metrics["C"])
	i, okI := lookup(cia, metrics["I"])
	a, okA := lookup(cia, metrics["A"])
	if !okC || !okI || !okA {
		return 0, false
	}
	if _, ok := metrics["S"]; !ok {
		return 0, false
	}

	iss := 1 - ((1 - c) * (1 - i) * (1 - a))
	var impact float64
	if changed {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	} else {
		impact = 6.42 * iss
	}
	if impact <= 0 {
		// No impact is no vulnerability, whatever the exploitability says.
		return 0, true
	}

	exploitability := 8.22 * av * ac * pr * ui
	score := impact + exploitability
	if changed {
		score *= 1.08
	}
	return roundUp(math.Min(score, 10)), true
}

func lookup(table map[string]float64, key string) (float64, bool) {
	v, ok := table[key]
	return v, ok
}

// roundUp is the specification's own rounding: the smallest number to one
// decimal place that is greater than or equal to the input. Plain rounding
// gives a different answer on the band boundaries, which is precisely where it
// changes a severity.
func roundUp(x float64) float64 {
	i := int(math.Round(x * 100000))
	if i%10000 == 0 {
		return float64(i) / 100000.0
	}
	return (math.Floor(float64(i)/10000) + 1) / 10.0
}

// SeverityFromVector derives a qualitative level from a CVSS vector, or
// unknown when the vector is one this binary does not score.
func SeverityFromVector(vector string) string {
	score, ok := cvss3Base(vector)
	if !ok {
		return SeverityUnknown
	}
	return SeverityFromCVSS(score)
}
