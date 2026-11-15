package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// **An install that has configured nothing optional produces no error rows.**
//
// The first of the tests whose failure would be silent, and the one this whole
// surface most needs. A capability an install does not offer — no artifact
// store, no isolated backend, no telemetry, no mail — is a *correct* install,
// and the row for it must read absent. Nothing crashes if it reads error
// instead: the report renders, the page loads, and the document tells an
// assessor the install is broken for being smaller than it could be. Nobody
// inside the company notices, because nobody inside the company runs an install
// with nothing configured.
//
// This is ADR §30's own enforcement copied deliberately — the checks page has
// the same test for the same reason, and that is the surface this one is most
// likely to drift away from.
func TestNothingConfiguredProducesNoErrors(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The bare install: every optional capability answers absent, and the few
	// things that always exist answer ok.
	bare := CollectorFunc(func(_ context.Context, item Evidence) Result {
		switch item.ID {
		case "install_identity", "install_settings", "install_checks",
			"users_and_roles", "groups_and_members", "apps_and_visibility",
			"auth_providers", "auth_providers_public":
			return OK(map[string]any{"present": true})
		}
		return Absent("this install does not offer that capability")
	})

	rep := Assemble(context.Background(), reg, bare)

	if n := rep.ByOutcome[OutcomeError]; n != 0 {
		for _, r := range rep.Results {
			if r.Outcome == OutcomeError {
				t.Errorf("%s reported an error on an install with nothing configured: %s",
					r.ID, r.Reason)
			}
		}
		t.Fatalf("%d error row(s) on a correct, minimal install. Absent is not a failure, "+
			"and an assessor reading this document cannot tell the difference", n)
	}
	if len(rep.Results) != len(reg.Evidence) {
		t.Errorf("got %d rows for %d evidence items — nothing may be skipped",
			len(rep.Results), len(reg.Evidence))
	}
}

// **Every evidence item produces a row, whatever the collector does.**
//
// Silent because the failure is an omission: a report missing three rows still
// renders, still totals, and still prints a coverage figure — one that is now a
// lie, and it is the first number an assessor reads.
func TestNothingIsEverSkipped(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A collector that behaves as badly as one can: panics on some items,
	// returns a zero Result on others.
	bad := CollectorFunc(func(_ context.Context, item Evidence) Result {
		switch item.Credential {
		case CredSession:
			panic("a read blew up")
		case CredNone:
			return Result{} // no outcome at all
		}
		return OK(nil)
	})

	rep := Assemble(context.Background(), reg, bad)

	if len(rep.Results) != len(reg.Evidence) {
		t.Fatalf("got %d rows for %d evidence items", len(rep.Results), len(reg.Evidence))
	}
	total := 0
	for _, n := range rep.ByOutcome {
		total += n
	}
	if total != len(reg.Evidence) {
		t.Errorf("outcome counts sum to %d, want %d", total, len(reg.Evidence))
	}
	for _, r := range rep.Results {
		if r.Outcome == "" {
			t.Errorf("%s has no outcome — a fifth state got out of the walk", r.ID)
		}
		if r.Label == "" {
			t.Errorf("%s lost its label; the registry owns the description", r.ID)
		}
	}
	// The panicking reads and the empty ones both have to be errors, with a
	// reason. A contained panic that produced a blank row would be worse than
	// the panic.
	if rep.ByOutcome[OutcomeError] == 0 {
		t.Error("a collector that panicked and one that returned nothing both produced " +
			"no error rows — the containment swallowed them")
	}
}

// A collector cannot relabel an item. The registry owns the description, so the
// control section and the evidence appendix cannot describe the same item
// differently.
func TestTheRegistryOwnsTheDescription(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	liar := CollectorFunc(func(_ context.Context, _ Evidence) Result {
		r := OK(nil)
		r.ID = "not-this-id"
		r.Label = "something else entirely"
		r.Route = "GET /somewhere/made/up"
		return r
	})
	rep := Assemble(context.Background(), reg, liar)
	for i, r := range rep.Results {
		want := reg.Evidence[i]
		if r.ID != want.ID || r.Label != want.Label || r.Route != want.Source.Route {
			t.Fatalf("row %d took its identity from the collector: got %s/%q, want %s/%q",
				i, r.ID, r.Label, want.ID, want.Label)
		}
	}
}

// Results are addressable by id, because the control sections cite evidence by
// id and have to resolve it.
func TestResultsAreAddressable(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rep := Assemble(context.Background(), reg,
		CollectorFunc(func(_ context.Context, _ Evidence) Result { return OK(nil) }))

	for _, q := range reg.Requirements {
		for _, id := range q.EvidenceIDs {
			if _, ok := rep.Result(id); !ok {
				t.Errorf("%s cites evidence %q and the report cannot resolve it", q.ID, id)
			}
		}
	}
}

// **The digest is recomputable from the file alone.**
//
// That is the whole of what it claims, and the only claim worth testing: an
// assessor holding an archived export can hash its `report` member and compare,
// with no access to the install it came from and no copy of this binary.
//
// It is deliberately NOT a way to tell two assessments apart — it covers the
// collection timestamp, so two exports of an unchanged install differ. An
// earlier draft claimed the opposite and this test caught it: a digest that
// ignores part of its own document cannot be verified by hashing the document.
func TestTheDigestIsRecomputableFromTheFile(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rep := Assemble(context.Background(), reg,
		CollectorFunc(func(_ context.Context, _ Evidence) Result { return OK("x") }))
	rep.Hostname = "example"

	raw, err := Freeze(rep)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}

	// What a reader has: the bytes. Nothing else.
	var doc struct {
		Artifact string          `json:"artifact"`
		Format   int             `json:"format"`
		Digest   string          `json:"sha256"`
		Report   json.RawMessage `json:"report"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshalling the export: %v", err)
	}
	if doc.Artifact == "" || doc.Format != ExportFormat {
		t.Error("the export does not name itself and its format; an archived document " +
			"has to be readable by somebody who does not have this binary")
	}

	// The member as it sits in the file, canonicalized: no Go types involved,
	// so whitespace and escaping in the file are not part of what is checked.
	// TestTheExportDigestRecomputesWithoutThisBinary does the same from Python.
	canonical, err := Canonical(doc.Report)
	if err != nil {
		t.Fatalf("canonicalizing the report member: %v", err)
	}
	sum := sha256.Sum256(canonical)
	if got := hex.EncodeToString(sum[:]); got != doc.Digest {
		t.Errorf("the digest cannot be recomputed from the file:\n  in file: %s\n  computed: %s",
			doc.Digest, got)
	}
}

// A changed reading changes the digest. The other half of the property above,
// and the half that makes it worth anything.
func TestAChangedReadingChangesTheDigest(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	digest := func(v string) string {
		rep := Assemble(context.Background(), reg,
			CollectorFunc(func(_ context.Context, _ Evidence) Result {
				return OK(map[string]any{"setting": v})
			}))
		for i := range rep.Results {
			rep.Results[i].TakenAt = time.Time{}
		}
		raw, err := Freeze(rep)
		if err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		var e Export
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return e.Digest
	}
	if digest("before") == digest("after") {
		t.Error("changing what the install reports left the digest unchanged, which " +
			"makes it useless for telling two archived reports apart")
	}
}

// The filename says which machine and which day, and it survives a hostname
// somebody chose. The hostname is operator-supplied and goes into a quoted
// Content-Disposition parameter, so a quote in it would end the string early.
func TestExportFilenameIsSafeAndSaysWhatItIs(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"toft-prod", "compliance-toft-prod-"},
		{"HOST.example.com", "compliance-host.example.com-"},
		{`ev"il; x=y`, "compliance-ev-il--x-y-"},
		{"", "compliance-toft-"},
		{"---", "compliance-toft-"},
	} {
		rep := &Report{Hostname: tc.host, CollectedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
		got := ExportFilename(rep)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("ExportFilename(%q) = %q, want prefix %q", tc.host, got, tc.want)
		}
		if strings.ContainsAny(got, `"\;`) {
			t.Errorf("ExportFilename(%q) = %q, which would break out of the "+
				"Content-Disposition parameter it is interpolated into", tc.host, got)
		}
		if !strings.HasSuffix(got, "2026-09-01.json") {
			t.Errorf("ExportFilename(%q) = %q, which does not name the day it describes",
				tc.host, got)
		}
	}
}
