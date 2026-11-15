package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The four outcomes an evidence item can end in.
//
// **Absent is the one that carries an argument.** A capability this install does
// not offer — no artifact store, no isolated backend, no telemetry — is a
// correct install, and a report that coloured it amber would be reporting an
// install as broken for being smaller than it could be. That is ADR §30's
// rule ("not configured is a skip, never a warning") applied to a second
// surface, and it matters more here than there: the reader of this document is
// an assessor who does not know which capabilities are optional, and a row that
// looks like a finding will be filed as one.
//
// Refused and Error are kept apart for the same reason. "This account may not
// read it" and "this read failed" are different facts about the install, and
// collapsing them would leave an assessor unable to tell an access-control
// boundary from an outage.
type Outcome string

const (
	OutcomeOK      Outcome = "ok"
	OutcomeAbsent  Outcome = "absent"
	OutcomeRefused Outcome = "refused"
	OutcomeError   Outcome = "error"
)

// Result is one evidence item, read.
type Result struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	Credential string    `json:"credential"`
	Route      string    `json:"route,omitempty"`
	Screen     string    `json:"screen,omitempty"`
	Reading    string    `json:"reading,omitempty"`
	Outcome    Outcome   `json:"outcome"`
	Reason     string    `json:"reason,omitempty"`
	Value      any       `json:"value,omitempty"`
	TakenAt    time.Time `json:"taken_at"`
}

// OK builds a successful reading. Helpers rather than struct literals at every
// collector site, so the shape of a Result is decided here and a collector
// cannot invent a fifth outcome by leaving the field blank.
func OK(value any) Result { return Result{Outcome: OutcomeOK, Value: value} }

// Absent is a capability this install does not offer. The reason names it,
// because "absent" alone reads as a gap to somebody who does not know the
// capability was optional.
func Absent(reason string) Result { return Result{Outcome: OutcomeAbsent, Reason: reason} }

// Refused is an authorization boundary, not a failure.
func Refused(reason string) Result { return Result{Outcome: OutcomeRefused, Reason: reason} }

// Errorf is a read that should have worked and did not.
func Errorf(reason string) Result { return Result{Outcome: OutcomeError, Reason: reason} }

// Collector reads one evidence item.
//
// The registry says WHAT the evidence is and this says HOW it is read, which is
// the same split `checkDef` already makes between the check list and the
// functions in the handler. It keeps this package free of the database: the
// registry, the assembly and the rendering are all testable without one, and
// the api package supplies the reads.
type Collector interface {
	Collect(ctx context.Context, item Evidence) Result
}

// CollectorFunc adapts a function.
type CollectorFunc func(ctx context.Context, item Evidence) Result

func (f CollectorFunc) Collect(ctx context.Context, item Evidence) Result {
	return f(ctx, item)
}

// Report is the whole reading, in registry order.
//
// It is computed per request and **filed nowhere**: no table, no retention dial,
// no sweep. ADR §30's storage argument holds here unchanged — what this feature
// argues with is §30's *reasoning* that a point-in-time report has no value,
// and that argument needs an export, not a row. The export is this struct,
// serialized, with a timestamp; the platform keeps no copy.
type Report struct {
	CollectedAt time.Time `json:"collected_at"`

	// Install identity, so an exported report says which machine it describes.
	// An artifact that does not name its subject is one nobody can file.
	InstallID string `json:"install_id,omitempty"`
	Version   string `json:"version,omitempty"`
	Hostname  string `json:"hostname,omitempty"`

	// Who asked. An assessment artifact with no author is worth less than one
	// with an author who can be asked about it.
	CollectedBy string `json:"collected_by,omitempty"`

	Catalog      string          `json:"catalog"`
	OSCALVersion string          `json:"oscal_version"`
	Results      []Result        `json:"results"`
	ByOutcome    map[Outcome]int `json:"by_outcome"`
	Coverage     Coverage        `json:"coverage"`

	index map[string]int
}

// Result returns one reading by evidence id.
func (rep *Report) Result(id string) (Result, bool) {
	i, ok := rep.index[id]
	if !ok {
		return Result{}, false
	}
	return rep.Results[i], true
}

// Assemble walks every evidence item in the registry and collects it.
//
// **Nothing is ever skipped.** Every item in the registry produces a row
// whatever happens to it, including one whose collector panics or returns
// nothing — those become OutcomeError with a reason rather than disappearing.
// A report that silently omitted an item would be a document whose coverage
// count was a lie, and the coverage count is the number an assessor reads
// first.
func Assemble(ctx context.Context, reg *Registry, c Collector) *Report {
	rep := &Report{
		CollectedAt:  time.Now().UTC(),
		Catalog:      reg.Catalog.Name,
		OSCALVersion: reg.Catalog.OSCALVersion,
		ByOutcome:    map[Outcome]int{},
		Coverage:     reg.Coverage(),
		index:        make(map[string]int, len(reg.Evidence)),
	}

	for _, item := range reg.Evidence {
		res := collectOne(ctx, c, item)

		// The registry owns the item's description; a collector supplies only
		// the outcome and the value. That stops a collector relabelling an
		// item, which would let the document say one thing in its control
		// section and another in its evidence appendix.
		res.ID = item.ID
		res.Label = item.Label
		res.Credential = item.Credential
		res.Route = item.Source.Route
		res.Screen = item.Screen.Label
		res.Reading = item.Reading
		if res.TakenAt.IsZero() {
			res.TakenAt = time.Now().UTC()
		}

		rep.index[item.ID] = len(rep.Results)
		rep.Results = append(rep.Results, res)
		rep.ByOutcome[res.Outcome]++
	}
	return rep
}

// collectOne contains one collector, so a panic in a read is that row's outcome
// rather than the whole report's.
//
// The same containment runChecks applies for the same reason: this is a
// diagnostic surface, and a diagnostic that can take the page down with it is
// one an administrator hesitates to open.
func collectOne(ctx context.Context, c Collector, item Evidence) (res Result) {
	return contain(func() Result { return c.Collect(ctx, item) })
}

// contain runs one read, so a panic is that row's outcome and an empty answer
// is an error with a reason, for both artifacts.
func contain(read func() Result) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Errorf("the read panicked and was contained")
		}
	}()
	res = read()
	if res.Outcome == "" {
		// A collector that returned nothing is a bug in the collector, and it
		// is reported as this row's error rather than as a missing row — the
		// whole point of the walk is that the registry decides what appears.
		return Errorf("the collector returned no outcome")
	}
	return res
}

// Outcomes returns the outcome counts in a stable order, for rendering.
func (rep *Report) Outcomes() []OutcomeCount {
	out := make([]OutcomeCount, 0, len(rep.ByOutcome))
	for o, n := range rep.ByOutcome {
		out = append(out, OutcomeCount{Outcome: o, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Outcome < out[j].Outcome
	})
	return out
}

type OutcomeCount struct {
	Outcome Outcome `json:"outcome"`
	N       int     `json:"n"`
}

// Export is the frozen reading: the report, plus what makes it an artifact
// somebody can file and come back to.
//
// It is a wrapper rather than fields on Report because the two are different
// things. Report is what the page renders and is always "now"; this is a
// document with a date on it, and the digest below only makes sense once the
// content has stopped moving.
type Export struct {
	Artifact string `json:"artifact"`
	Format   int    `json:"format"`

	// Digest is a SHA-256 over the `report` member in RFC 8785 canonical form
	// (Canonical), and it is **tamper evidence, not drift detection**. A reader
	// with the file and any JCS implementation can recompute it; that is the
	// whole of what it claims. DigestRule says so inside the file, for the
	// reader who has nothing else.
	//
	// It therefore covers the collection timestamp too, so two exports of an
	// unchanged install differ. Excluding the clock so that two reports could be
	// compared for "did the install change" was considered and dropped: it is a
	// second feature wearing the same field, and a digest that deliberately
	// ignores part of its document is one nobody can verify by hashing the
	// document.
	Digest     string `json:"sha256"`
	DigestRule string `json:"sha256_rule"`

	Report *Report `json:"report"`
}

// ExportFormat is bumped when the shape changes in a way that would make an
// older reader wrong about a newer file. It is on the document rather than
// implied by a version string because whoever opens an archived export in three
// years will not have this binary.
//
// 2: the digest moved from `encoding/json`'s bytes to RFC 8785, and
// `sha256_rule` names it.
const ExportFormat = 2

// DigestRule is the sentence each export carries beside its digest.
const DigestRule = "SHA-256 of the report member serialized as RFC 8785 (JCS) canonical JSON"

// Freeze serializes a report into an export document.
//
// It marshals fully before returning, so a document that cannot be produced is
// an error the caller can still turn into a 500 — rather than a truncated file
// that has already begun streaming. An evidence bundle that stops halfway is
// worse than one that never started, because it looks complete.
func Freeze(rep *Report) ([]byte, error) {
	return freeze("toft-compliance-report", rep)
}

// freeze is the one digest rule, for both artifacts: the member marshalled
// whole before the first byte, and a SHA-256 over its canonical form — so how
// the file happens to be indented or escaped is not part of what is signed.
func freeze(artifact string, member any) ([]byte, error) {
	body, err := json.Marshal(member)
	if err != nil {
		return nil, fmt.Errorf("serializing the report: %w", err)
	}
	canon, err := Canonical(body)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing the report: %w", err)
	}
	sum := sha256.Sum256(canon)

	return json.MarshalIndent(struct {
		Artifact   string          `json:"artifact"`
		Format     int             `json:"format"`
		Digest     string          `json:"sha256"`
		DigestRule string          `json:"sha256_rule"`
		Report     json.RawMessage `json:"report"`
	}{artifact, ExportFormat, hex.EncodeToString(sum[:]), DigestRule, body}, "", " ")
}

// ExportFilename names the file the browser saves.
//
// The install and the date, because a directory of assessment artifacts sorts
// by neither if they are all called report.json — and an artifact that does not
// say which machine and which day it describes is one nobody can file.
func ExportFilename(rep *Report) string {
	host := rep.Hostname
	if host == "" {
		host = "toft"
	}
	return "compliance-" + SafeFilenamePart(host) + "-" +
		rep.CollectedAt.Format("2006-01-02") + ".json"
}

// SafeFilenamePart keeps a value to characters that survive a
// Content-Disposition header and a filesystem.
//
// Exported because both artifacts interpolate an operator-supplied string into
// a quoted header parameter — a hostname for the export, a version for the PDF
// — and one sanitizer between them is the point: a quote in either would end
// the quoted string and let the rest be read as header parameters.
func SafeFilenamePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "toft"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
