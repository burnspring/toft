package advisory

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Reading OSV, which is the schema every feed this platform can use speaks.
//
// The parse is deliberately tolerant in one direction and strict in the other.
// A field this binary does not understand is ignored, because the schema grows
// and an install should not stop syncing when it does. An *ecosystem* it does
// not understand is dropped at ingest rather than stored, because a package
// stored under an unknown ecosystem could only ever be compared under some
// other ecosystem's ordering rules — a silent miss, which is the direction
// that never gets noticed.

// osvRecord is the subset of the OSV schema this platform reads. Everything
// absent from it is deliberately not read, not merely unimplemented.
type osvRecord struct {
	ID        string   `json:"id"`
	Summary   string   `json:"summary"`
	Details   string   `json:"details"`
	Aliases   []string `json:"aliases"`
	Published string   `json:"published"`
	Modified  string   `json:"modified"`
	Withdrawn string   `json:"withdrawn"`

	Severity []osvSeverity `json:"severity"`
	Affected []osvAffected `json:"affected"`

	References []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"references"`

	DatabaseSpecific json.RawMessage `json:"database_specific"`
}

type osvSeverity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

type osvAffected struct {
	Package struct {
		Ecosystem string `json:"ecosystem"`
		Name      string `json:"name"`
		PURL      string `json:"purl"`
	} `json:"package"`
	Ranges []struct {
		Type   string `json:"type"`
		Events []struct {
			Introduced   string `json:"introduced"`
			Fixed        string `json:"fixed"`
			LastAffected string `json:"last_affected"`
			Limit        string `json:"limit"`
		} `json:"events"`
	} `json:"ranges"`
	Versions         []string        `json:"versions"`
	DatabaseSpecific json.RawMessage `json:"database_specific"`
}

// osvDatabaseSpecific is the vendor extension block, which is where the two
// facts this platform most needs actually live: GitHub's qualitative severity,
// and whether the record is about malware.
type osvDatabaseSpecific struct {
	Severity  string   `json:"severity"`
	CWEIDs    []string `json:"cwe_ids"`
	Malicious *bool    `json:"malicious"`
	Type      string   `json:"type"`
}

// ParseOSV reads one OSV document into a Record, or returns ok=false when
// there is nothing in it this install could act on.
//
// "Nothing to act on" is a normal outcome, not an error: an OSV corpus for an
// ecosystem this platform installs still contains records whose only affected
// package is in an ecosystem it does not, and a sync that logged an error for
// each of those would bury the ones that matter.
func ParseOSV(raw []byte, source string) (Record, bool, error) {
	var doc osvRecord
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Record{}, false, err
	}
	if strings.TrimSpace(doc.ID) == "" {
		return Record{}, false, fmt.Errorf("advisory has no id")
	}

	rec := Record{
		ID:      strings.TrimSpace(doc.ID),
		Source:  source,
		Kind:    KindVuln,
		Summary: firstNonEmpty(truncate(doc.Summary, maxSummary), truncate(doc.Details, 400)),
		URL:     advisoryURL(doc),
	}
	rec.PublishedAt = parseTime(doc.Published)
	rec.WithdrawnAt = parseTime(doc.Withdrawn)

	var top osvDatabaseSpecific
	if len(doc.DatabaseSpecific) > 0 {
		_ = json.Unmarshal(doc.DatabaseSpecific, &top)
	}
	if isMalware(rec.ID, top) {
		rec.Kind = KindMalware
	}

	// Severity, in the order of how much the publisher actually asserted:
	// a qualitative rating they wrote, then a vector they wrote, then nothing.
	// A derived score never overrides a stated word.
	rec.Severity = NormalizeSeverity(top.Severity)
	for _, s := range doc.Severity {
		if strings.EqualFold(s.Type, "CVSS_V3") && rec.CVSSVector == "" {
			rec.CVSSVector = s.Score
		}
	}
	if rec.Severity == SeverityUnknown && rec.CVSSVector != "" {
		rec.Severity = SeverityFromVector(rec.CVSSVector)
	}

	for _, aff := range doc.Affected {
		ecosystem := NormalizeEcosystem(aff.Package.Ecosystem)
		if ecosystem == "" {
			// Not this platform's ecosystem. Dropped here rather than stored
			// and filtered later, so nothing downstream can compare a Debian
			// package version under npm's ordering rules.
			continue
		}
		name := NormalizePackage(ecosystem, aff.Package.Name)
		if name == "" {
			continue
		}
		// A per-package severity overrides the record's, which is how GHSA
		// expresses one advisory affecting two packages differently.
		for _, r := range affectedRanges(aff) {
			rec.Affected = append(rec.Affected, Affected{
				Ecosystem: ecosystem,
				Package:   name,
				Range:     r,
			})
		}
	}
	if len(rec.Affected) == 0 {
		return Record{}, false, nil
	}
	return rec, true, nil
}

// affectedRanges turns OSV's event stream into intervals.
//
// OSV describes a range as an ordered list of events — introduced, fixed,
// last_affected — and an advisory routinely carries several disjoint intervals
// in one list ("broken in 1.x, fixed in 1.5; broken again in 2.0, fixed in
// 2.1"). Reading only the first introduced and the first fixed, which is the
// obvious mistake, merges those into one interval that covers 1.5–2.0 as
// affected when it is not.
//
// A `limit` event is ignored: it bounds what the range *describes* rather than
// what is affected, and treating it as a fix would mark patched versions bad.
func affectedRanges(aff osvAffected) []Range {
	var out []Range
	for _, r := range aff.Ranges {
		// GIT ranges are commit hashes, which this platform never resolves to:
		// it installs published package versions, not checkouts.
		if strings.EqualFold(r.Type, "GIT") {
			continue
		}
		var current *Range
		for _, ev := range r.Events {
			switch {
			case ev.Introduced != "":
				// A new introduced event closes the previous interval, however
				// it ended — including an interval with no fix, which is an
				// unbounded range that the next introduced does not extend.
				if current != nil {
					out = append(out, *current)
				}
				current = &Range{Introduced: ev.Introduced}
			case ev.Fixed != "":
				if current == nil {
					current = &Range{}
				}
				current.Fixed = ev.Fixed
				out = append(out, *current)
				current = nil
			case ev.LastAffected != "":
				if current == nil {
					current = &Range{}
				}
				current.LastAffected = ev.LastAffected
				out = append(out, *current)
				current = nil
			}
		}
		if current != nil {
			out = append(out, *current)
		}
	}

	// An explicit enumeration rides on its own range when there are no
	// intervals, and is attached to the first otherwise. Either way it is
	// checked independently of the bounds, so which one it lands on changes
	// nothing about the answer.
	if len(aff.Versions) > 0 {
		if len(out) == 0 {
			return []Range{{Versions: aff.Versions}}
		}
		out[0].Versions = aff.Versions
	}
	return out
}

// isMalware decides the class, and the three signals are all the ones any feed
// this platform reads actually publishes.
//
// The id prefix is the load-bearing one: OpenSSF's malicious-packages feed is
// where the overwhelming majority of these come from and every record in it is
// a MAL-. The other two catch GHSA's own malware advisories, which are filed
// under a GHSA- id and say so in the vendor block.
func isMalware(id string, ds osvDatabaseSpecific) bool {
	if strings.HasPrefix(strings.ToUpper(id), "MAL-") {
		return true
	}
	if ds.Malicious != nil && *ds.Malicious {
		return true
	}
	if strings.EqualFold(ds.Type, "malware") {
		return true
	}
	for _, cwe := range ds.CWEIDs {
		// CWE-506, Embedded Malicious Code.
		if strings.EqualFold(strings.TrimSpace(cwe), "CWE-506") {
			return true
		}
	}
	return false
}

// advisoryURL picks the link a person should follow. The publisher's own
// advisory page is the useful one; a commit or a bug tracker entry is not what
// somebody triaging wants first.
//
// A reference is a string from a downloaded feed that the UI renders as an
// href, so only an absolute http or https URL is taken; a javascript: or
// data: reference falls through to the osv.dev link.
func advisoryURL(doc osvRecord) string {
	for _, want := range []string{"ADVISORY", "WEB", "REPORT"} {
		for _, ref := range doc.References {
			if strings.EqualFold(ref.Type, want) && webURL(ref.URL) {
				return strings.TrimSpace(ref.URL)
			}
		}
	}
	for _, ref := range doc.References {
		if webURL(ref.URL) {
			return strings.TrimSpace(ref.URL)
		}
	}
	// Every OSV id resolves at osv.dev, so a record with no references still
	// gets a link — a citation that cannot be clicked is a claim.
	return "https://osv.dev/vulnerability/" + doc.ID
}

func parseTime(s string) *time.Time {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// maxSummary keeps a summary under the corpus column's own clip, so the cut
// that lands is this one, on a rune boundary, rather than the store's.
const maxSummary = 1000

// truncate cuts s to at most n bytes on a rune boundary. A cut inside a
// multi-byte character is invalid UTF-8, which Postgres refuses — and one such
// record failed its whole ecosystem's sync, every time, leaving the corpus
// stale. NUL is dropped for the same reason: valid UTF-8, refused as text.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}

// webURL reports whether raw is an absolute http(s) URL with a host.
func webURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "http")
}
