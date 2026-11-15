// Package advisory is what a third party has published about the packages the
// world runs, and the rules for deciding whether one of them describes a
// version this install is about to install.
//
// The half that matters most is **pure**: Matches takes an ecosystem, a
// version string and a range, imports nothing, and answers. That is ADR §29
// D4's argument moved one layer down — the matcher is the piece most likely to
// be got subtly wrong, so it is separable, table-driven, and tested against
// each ecosystem's own edge cases rather than against whatever the feed
// happened to ship the day it was written.
//
// The other half fetches snapshots and writes them down. It never reaches the
// database directly: it writes through a Store interface, which is what keeps
// `internal/database` free to import this package for its types (the way it
// imports `internal/sources`) without a cycle.
//
// # What this package is not
//
// It reads **names and versions** and compares them to **published
// advisories**. It does not read an app's code, and no copy anywhere may imply
// it does. It is also incomplete and lagging by construction: a package is
// malicious for days or weeks before anybody files a MAL- record, and a
// threshold that blocks critical advisories is not a statement that the
// install is free of high ones. Every number this produces is scoped to the
// snapshot it was computed against, which is why every surface carries the
// feed's own age beside the figure.
package advisory

import (
	"sort"
	"strings"
	"time"
)

// The ecosystems this install resolves. Lowercase is the canonical spelling
// everywhere — in the feed's normalized form, in `deployment_dependencies`,
// and in every API response — so a feed writing "PyPI" and a resolver writing
// "pypi" cannot disagree about whether a package is covered.
const (
	EcosystemNPM  = "npm"
	EcosystemPyPI = "pypi"
	EcosystemGo   = "go"
	EcosystemCRAN = "cran"
)

// Ecosystems is the set whose **silence is a verdict**, in display order. An
// ecosystem absent from here is not scanned, and the app's Dependencies tab
// says so rather than reporting no findings.
//
// **CRAN is deliberately absent, and this is a judgement about the data rather
// than an unfinished implementation.** Counted from OSV's own exports on
// 2026-08-30, and recounted against the upstream repository the same day: CRAN
// carries **14 advisories in its entire history**, across ten packages, all of
// them RSEC-*. The whole export is 12 KB against Go's 11.4 MB — and Go is the
// smallest of the three here. Not one of shiny, plumber, rmarkdown, knitr,
// dplyr or ggplot2 appears.
//
// That is not OSV being thin about R. `RConsortium/r-advisory-database` is the
// sole upstream and holds the same fourteen files, ~30 commits in three years;
// no second corpus exists that this platform could take instead. Sonatype's
// OSS Index does carry CRAN coordinates, and it is query-only — using it would
// mean POSTing every internal app's package list to a third party, which is the
// one thing `feed.go` is built to refuse.
//
// Listing it here would put "0 findings" on the Dependencies tab of every R app
// on every install, forever, and that zero would be a statement about OSV's
// coverage wearing the appearance of a statement about the app. *No status is
// ever a lie* is the rule that decides it: the tab says **not scanned**, which
// is true, rather than a clean bill of health nothing supports.
//
// What it waits on is the corpus being thick enough for an *absence* to carry
// information. It has never waited on the fourteen records themselves — those
// are matched, and Synced below is where that happens.
var Ecosystems = []string{EcosystemNPM, EcosystemPyPI, EcosystemGo}

// Synced is the set whose corpus this install **fetches and matches against**,
// and it is wider than Ecosystems by exactly one entry.
//
// The two lists answer different questions, and keeping them apart is the whole
// of the R position:
//
//   - Synced decides whether a finding can *appear*. A CRAN advisory that
//     exists is matched, shown, counted and blocks under the administrator's
//     threshold exactly like an npm one — the fourteen records are thin, not
//     wrong, and suppressing a real one because its neighbours are missing
//     would be a second policy nobody configured.
//   - Ecosystems decides whether an *absence* of findings means anything, which
//     is what `scanned` reports. R is not in it, so silence stays silence.
//
// **Recognition may only subtract**, which is the reachability rule applied to
// a corpus instead of a probe: matching CRAN can move a release from "nothing
// known" to "one advisory", and can never move it to "clean". So this reverses
// nothing the count above argued — it takes the half of the corpus that does
// carry information and stops throwing it away.
//
// Derived rather than written out, because every covered ecosystem must be
// fetched: a corpus that is not synced cannot be matched, and an Ecosystems
// entry with no snapshot behind it would report a permanent clean bill of
// health, which is the exact failure this file exists to prevent.
var Synced = append(append([]string{}, Ecosystems...), EcosystemCRAN)

// Syncs reports whether this install keeps a corpus for an ecosystem, which is
// membership of Synced above. It is the middle rung of three: KnownEcosystem
// can compare versions, Syncs holds advisories to compare them against, and
// Covered trusts the silence that comes back.
func Syncs(ecosystem string) bool {
	for _, e := range Synced {
		if e == ecosystem {
			return true
		}
	}
	return false
}

// Covered reports whether an *absence* of findings is a verdict for an
// ecosystem, which is membership of Ecosystems above.
//
// The distinction from Syncs is the whole of the CRAN decision, and it is why
// an inventory and a scan are two facts rather than one. `cran` is *synced* —
// its fourteen advisories are held and matched, so a finding on one of them
// appears like any other — and it is not *covered*, because a corpus that
// carries almost nothing cannot make "no findings" mean anything. So an
// inventory row in an uncovered ecosystem is recorded, displayed and matched,
// and never counted towards "this release was scanned": a release whose whole
// package list is R has been read completely and checked against fourteen
// records, and the two halves of that sentence are what the surfaces have to
// keep apart.
func Covered(ecosystem string) bool {
	for _, e := range Ecosystems {
		if e == ecosystem {
			return true
		}
	}
	return false
}

// KnownEcosystem reports whether this binary can compare versions in an
// ecosystem at all — the outermost of the three rungs.
//
// CRAN answers true: the ordering rules and the normalisation exist, so a CRAN
// advisory is matched correctly rather than compared under somebody else's
// rules. That is a different question from whether a corpus is kept (Syncs) and
// from whether its silence is trusted (Covered).
func KnownEcosystem(e string) bool {
	switch e {
	case EcosystemNPM, EcosystemPyPI, EcosystemGo, EcosystemCRAN:
		return true
	}
	return false
}

// The two advisory classes.
//
// **Malware is not a severity.** There is no low-severity credential stealer,
// and a single threshold set to `critical` would let a malware advisory scored
// `moderate` — which happens, because scoring malware on a vulnerability scale
// is a category error the scorers make too — through silently. Keeping the
// class separate costs one boolean setting and removes that entire mistake.
const (
	KindVuln    = "vuln"
	KindMalware = "malware"
)

// Severity levels, and the empty string for an advisory that carries none.
//
// Unknown is common and must have a **defined** treatment rather than an
// accidental one: it never blocks under a severity threshold, it is listed as
// unknown, and it is counted separately, so a feed degrading into unscored
// records is visible rather than read as an improvement.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityModerate = "moderate"
	SeverityLow      = "low"
	SeverityUnknown  = ""
)

// severityRank orders the four named levels. Unknown is deliberately absent:
// it has no rank, and code that wants to compare it must go through
// AtOrAbove, which answers false for it.
var severityRank = map[string]int{
	SeverityLow:      1,
	SeverityModerate: 2,
	SeverityHigh:     3,
	SeverityCritical: 4,
}

// ValidSeverity reports whether a value is one of the four named levels.
// Unknown is not one: it is a valid *stored* value and not a valid threshold,
// which is the distinction this function draws.
func ValidSeverity(s string) bool { _, ok := severityRank[s]; return ok }

// AtOrAbove reports whether a finding's severity meets a threshold.
//
// An unknown severity is **never** at or above anything, including "low". That
// is the whole of the defined treatment: an advisory nobody scored cannot be
// asserted to clear a bar somebody set, and the alternative — treating unknown
// as the floor, or as the ceiling — is either a control that misses things
// quietly or one that blocks deploys on no evidence.
func AtOrAbove(severity, threshold string) bool {
	s, ok := severityRank[severity]
	if !ok {
		return false
	}
	t, ok := severityRank[threshold]
	if !ok {
		return false
	}
	return s >= t
}

// NormalizeSeverity maps what feeds actually write onto the four levels.
//
// Anything unrecognized becomes unknown rather than being guessed at. A feed
// that starts writing a fifth word should show up as a rising unknown count on
// the estate view — which is a thing somebody notices — and not as a silent
// reclassification into whichever level looked closest.
func NormalizeSeverity(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "critical":
		return SeverityCritical
	case "high":
		return SeverityHigh
	case "moderate", "medium":
		return SeverityModerate
	case "low":
		return SeverityLow
	}
	return SeverityUnknown
}

// SeverityFromCVSS derives a level from a CVSS base score, for the feeds that
// publish a vector and no qualitative rating.
//
// The bands are the CVSS v3.1 qualitative scale, unaltered. Deriving rather
// than leaving it unknown is worth it because GHSA publishes vectors for
// nearly everything and a corpus that read as mostly-unknown would make the
// threshold meaningless — but the derivation is only ever applied when the feed
// gave no rating of its own, never over one it did.
func SeverityFromCVSS(score float64) string {
	switch {
	case score >= 9.0:
		return SeverityCritical
	case score >= 7.0:
		return SeverityHigh
	case score >= 4.0:
		return SeverityModerate
	case score > 0:
		return SeverityLow
	}
	return SeverityUnknown
}

// Record is one advisory, normalized out of whatever the feed shipped.
type Record struct {
	ID          string     `json:"id"`
	Source      string     `json:"source,omitempty"`
	Kind        string     `json:"kind"`
	Severity    string     `json:"severity"`
	CVSSVector  string     `json:"cvss_vector,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	URL         string     `json:"url,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
	// Affected is every range this advisory declares, across every ecosystem
	// and package it covers.
	Affected []Affected `json:"affected,omitempty"`
}

// Malware reports whether this advisory is about a package that is malicious
// by intent rather than one that has a bug.
func (r Record) Malware() bool { return r.Kind == KindMalware }

// Affected is one (ecosystem, package) with one set of version bounds.
type Affected struct {
	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Range
}

// Range is the version interval an advisory declares, in the strings the feed
// published.
//
// Stored and compared as strings, never parsed into a sortable canonical form
// at ingest. npm semver, PEP 440 and Go semver each order versions
// differently, and a canonical form computed once at sync time would be a
// fourth opinion about version ordering — one that could only be fixed by
// re-syncing the whole corpus.
type Range struct {
	// Introduced is the inclusive lower bound. Empty, or "0", means "from the
	// beginning".
	Introduced string `json:"introduced,omitempty"`
	// Fixed is the **exclusive** upper bound: the first version that is not
	// affected. Empty means no fix has been published, which is exactly what
	// `dependency_block_only_fixable` keys off — so it must stay
	// distinguishable from a fix of "0" rather than being folded into it.
	Fixed string `json:"fixed,omitempty"`
	// LastAffected is the inclusive upper bound, for feeds that publish the
	// last bad version instead of the first good one. Both may be present.
	LastAffected string `json:"last_affected,omitempty"`
	// Versions is an explicit enumeration, for advisories that list rather
	// than bound.
	Versions []string `json:"versions,omitempty"`
}

// Fixable reports whether the advisory names a version to upgrade to.
//
// This is what separates a refusal a developer can act on from one whose only
// available response is a waiver — and a control whose only response is a
// waiver trains everyone to waive.
func (r Range) Fixable() bool { return strings.TrimSpace(r.Fixed) != "" }

// Finding is one advisory matched against one installed package: the shape
// every surface in this feature renders.
//
// It is **derived at read time**, never stored. A stored verdict would be
// wrong the moment the feed moved, and would need invalidation logic nobody
// would maintain — the same rule the gallery follows for "installed".
type Finding struct {
	AdvisoryID string `json:"advisory_id"`
	Kind       string `json:"kind"`
	Severity   string `json:"severity"`
	Summary    string `json:"summary,omitempty"`
	URL        string `json:"url,omitempty"`

	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	// FixedIn is the version to upgrade to, empty when none is published.
	FixedIn string `json:"fixed_in,omitempty"`
	// Path is the transitive chain, "your-app > webpack > lodash". Not
	// decoration: "update lodash" is not actionable when the app's own
	// manifest has never named lodash, and the chain is the only thing that
	// says which of *their* dependencies to bump.
	Path string `json:"path,omitempty"`
	// Direct and Dev come from the inventory row.
	Direct bool `json:"direct"`
	Dev    bool `json:"dev"`
	// Source is the inventory row's (deps.SourceResolved and its siblings):
	// whether the package was the app's choice, Toft's, or installed while the
	// app ran — which decides whose fix it is.
	Source string `json:"source,omitempty"`

	// WouldBlock reports that this finding is a reason to refuse a deploy
	// under the policy that derived it — **whether or not that policy is
	// actually enforcing**.
	//
	// Named for what it means rather than "blocks", because in `warn` mode it
	// is true and nothing is refused. That is the point: it is what lets a tab
	// and a preview say "this would block" before anybody switches the mode,
	// and it is exactly the field a reader is tempted to render as "blocked".
	// The install-level answer is `enforcing`, which travels beside it.
	WouldBlock bool `json:"would_block"`
	// Waived carries the accepted risk, when one applies. A waiver silences
	// the refusal and never the record, so a waived finding is still here.
	Waived    bool       `json:"waived"`
	WaivedBy  string     `json:"waived_by,omitempty"`
	WaivedFor string     `json:"waived_reason,omitempty"`
	WaiverEnd *time.Time `json:"waiver_expires_at,omitempty"`
}

// SortFindings orders a finding set the way every table in this feature reads
// it: malware first, then whatever blocks a deploy, then by severity, then
// alphabetically so the order is stable across reloads.
//
// One ordering, in one place, because the app tab, the estate view, the build
// log and the refusal all show the same set and a reader who learned the order
// on one should not have to relearn it on the next.
func SortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		a, b := f[i], f[j]
		if a.Malware() != b.Malware() {
			return a.Malware()
		}
		if a.WouldBlock != b.WouldBlock {
			return a.WouldBlock
		}
		if ra, rb := severityRank[a.Severity], severityRank[b.Severity]; ra != rb {
			return ra > rb
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.AdvisoryID < b.AdvisoryID
	})
}

// Malware reports whether this finding is about a malicious package.
func (f Finding) Malware() bool { return f.Kind == KindMalware }
