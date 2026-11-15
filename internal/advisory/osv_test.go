package advisory

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestParseOSVReadsAGHSARecord(t *testing.T) {
	doc := []byte(`{
	  "id": "GHSA-p6mc-m468-83gg",
	  "summary": "Prototype pollution in lodash",
	  "published": "2020-07-15T19:15:00Z",
	  "severity": [{"type": "CVSS_V3", "score": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}],
	  "affected": [{
	    "package": {"ecosystem": "npm", "name": "lodash"},
	    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "4.17.21"}]}]
	  }],
	  "references": [
	    {"type": "WEB", "url": "https://example.com/blog"},
	    {"type": "ADVISORY", "url": "https://github.com/advisories/GHSA-p6mc-m468-83gg"}
	  ],
	  "database_specific": {"severity": "HIGH"}
	}`)

	rec, ok, err := ParseOSV(doc, "osv")
	if err != nil || !ok {
		t.Fatalf("ParseOSV: ok=%v err=%v", ok, err)
	}
	if rec.ID != "GHSA-p6mc-m468-83gg" || rec.Kind != KindVuln {
		t.Errorf("rec = %+v", rec)
	}
	// The publisher's own word wins over a score derived from their vector.
	// That vector scores 9.8 (critical); the record says HIGH, and the record
	// is what the publisher asserted.
	if rec.Severity != SeverityHigh {
		t.Errorf("Severity = %q, want the stated %q", rec.Severity, SeverityHigh)
	}
	// The advisory page, not the first reference in the list.
	if rec.URL != "https://github.com/advisories/GHSA-p6mc-m468-83gg" {
		t.Errorf("URL = %q", rec.URL)
	}
	if len(rec.Affected) != 1 {
		t.Fatalf("Affected = %+v", rec.Affected)
	}
	a := rec.Affected[0]
	if a.Ecosystem != EcosystemNPM || a.Package != "lodash" ||
		a.Introduced != "0" || a.Fixed != "4.17.21" {
		t.Errorf("Affected[0] = %+v", a)
	}
	if Matches(EcosystemNPM, "4.17.15", a.Range) != Matched {
		t.Error("4.17.15 should be affected")
	}
	if Matches(EcosystemNPM, "4.17.21", a.Range) != NoMatch {
		t.Error("the fixed version should not be affected")
	}
}

// With no stated rating, the vector is what there is — otherwise a PYSEC-heavy
// corpus reads as mostly unscored and the threshold quietly stops meaning
// anything.
func TestParseOSVDerivesSeverityFromTheVectorOnly(t *testing.T) {
	doc := []byte(`{
	  "id": "PYSEC-2024-1",
	  "severity": [{"type": "CVSS_V3", "score": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}],
	  "affected": [{"package": {"ecosystem": "PyPI", "name": "Flask_Thing"},
	                "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "0"}, {"fixed": "1.0"}]}]}]
	}`)
	rec, ok, err := ParseOSV(doc, "osv")
	if err != nil || !ok {
		t.Fatalf("ParseOSV: ok=%v err=%v", ok, err)
	}
	if rec.Severity != SeverityCritical {
		t.Errorf("Severity = %q, want critical from the 9.8 vector", rec.Severity)
	}
	// PEP 503 normalization happens at ingest, so an advisory filed under one
	// spelling matches an app that pinned another.
	if rec.Affected[0].Package != "flask-thing" {
		t.Errorf("Package = %q, want the PEP 503 form", rec.Affected[0].Package)
	}
}

func TestParseOSVMalwareSignals(t *testing.T) {
	cases := map[string][]byte{
		"a MAL- id": []byte(`{"id":"MAL-2024-1","affected":[{"package":{"ecosystem":"npm","name":"evil"},
		  "versions":["1.0.0"]}]}`),
		"the malicious flag": []byte(`{"id":"GHSA-x","database_specific":{"malicious":true},
		  "affected":[{"package":{"ecosystem":"npm","name":"evil"},"versions":["1.0.0"]}]}`),
		"CWE-506": []byte(`{"id":"GHSA-y","database_specific":{"cwe_ids":["CWE-506"]},
		  "affected":[{"package":{"ecosystem":"npm","name":"evil"},"versions":["1.0.0"]}]}`),
	}
	for name, doc := range cases {
		rec, ok, err := ParseOSV(doc, "osv")
		if err != nil || !ok {
			t.Fatalf("%s: ok=%v err=%v", name, ok, err)
		}
		if rec.Kind != KindMalware {
			t.Errorf("%s: Kind = %q, want malware", name, rec.Kind)
		}
	}

	// And the negative: an ordinary advisory must not be reclassified.
	rec, _, _ := ParseOSV([]byte(`{"id":"GHSA-z","database_specific":{"cwe_ids":["CWE-79"]},
	  "affected":[{"package":{"ecosystem":"npm","name":"x"},"versions":["1.0.0"]}]}`), "osv")
	if rec.Kind != KindVuln {
		t.Errorf("Kind = %q, want vuln", rec.Kind)
	}
}

// The mistake this guards against is reading only the first introduced and the
// first fixed, which merges two disjoint intervals into one that covers the
// patched versions between them.
func TestParseOSVKeepsDisjointIntervalsApart(t *testing.T) {
	doc := []byte(`{
	  "id": "GHSA-two-ranges",
	  "affected": [{
	    "package": {"ecosystem": "npm", "name": "thing"},
	    "ranges": [{"type": "SEMVER", "events": [
	      {"introduced": "1.0.0"}, {"fixed": "1.5.0"},
	      {"introduced": "2.0.0"}, {"fixed": "2.1.0"}
	    ]}]
	  }]
	}`)
	rec, ok, err := ParseOSV(doc, "osv")
	if err != nil || !ok {
		t.Fatalf("ParseOSV: ok=%v err=%v", ok, err)
	}
	if len(rec.Affected) != 2 {
		t.Fatalf("want two intervals, got %+v", rec.Affected)
	}

	affected := func(v string) bool {
		for _, a := range rec.Affected {
			if Matches(EcosystemNPM, v, a.Range) == Matched {
				return true
			}
		}
		return false
	}
	for _, v := range []string{"1.0.0", "1.4.9", "2.0.5"} {
		if !affected(v) {
			t.Errorf("%s should be affected", v)
		}
	}
	// The gap between the two intervals is patched, and reading the events
	// naively would mark it bad.
	for _, v := range []string{"0.9.0", "1.5.0", "1.9.0", "2.1.0"} {
		if affected(v) {
			t.Errorf("%s should not be affected", v)
		}
	}
}

// An unbounded interval followed by a new introduced must not swallow it.
func TestParseOSVClosesAnUnfixedIntervalOnTheNextIntroduced(t *testing.T) {
	doc := []byte(`{
	  "id": "GHSA-unfixed-then-more",
	  "affected": [{"package": {"ecosystem": "npm", "name": "thing"},
	    "ranges": [{"type": "SEMVER", "events": [
	      {"introduced": "1.0.0"}, {"introduced": "2.0.0"}, {"fixed": "2.1.0"}]}]}]
	}`)
	rec, _, _ := ParseOSV(doc, "osv")
	if len(rec.Affected) != 2 {
		t.Fatalf("want two intervals, got %+v", rec.Affected)
	}
	if rec.Affected[0].Fixed != "" || rec.Affected[0].Introduced != "1.0.0" {
		t.Errorf("the first interval should stay unbounded above: %+v", rec.Affected[0])
	}
}

func TestParseOSVDropsWhatThisPlatformDoesNotInstall(t *testing.T) {
	// An ecosystem this platform never installs must fall out at ingest, so no
	// later code can compare its versions under npm's ordering rules.
	doc := []byte(`{"id":"DSA-1","affected":[{"package":{"ecosystem":"Debian:11","name":"openssl"},
	  "ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"1.1.1"}]}]}]}`)
	_, ok, err := ParseOSV(doc, "osv")
	if err != nil {
		t.Fatalf("ParseOSV: %v", err)
	}
	if ok {
		t.Error("a Debian-only advisory should produce no record")
	}

	// A GIT range is commit hashes, and this platform installs published
	// versions rather than checkouts.
	doc = []byte(`{"id":"GHSA-git","affected":[{"package":{"ecosystem":"Go","name":"example.com/x"},
	  "ranges":[{"type":"GIT","events":[{"introduced":"abc123"},{"fixed":"def456"}]}]}]}`)
	_, ok, _ = ParseOSV(doc, "osv")
	if ok {
		t.Error("a GIT-only range should produce no record")
	}
}

func TestParseOSVRefusesADocumentWithNoID(t *testing.T) {
	if _, _, err := ParseOSV([]byte(`{"summary":"x"}`), "osv"); err == nil {
		t.Error("expected an error for a record with no id")
	}
	if _, _, err := ParseOSV([]byte(`not json`), "osv"); err == nil {
		t.Error("expected an error for a document that is not JSON")
	}
}

func TestCVSS3BaseScore(t *testing.T) {
	// The published examples, which is the only honest way to test a formula
	// transcribed from a specification.
	tests := []struct {
		vector string
		want   float64
	}{
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H", 7.5},
		{"CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:N/A:N", 5.9},
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", 6.1},
		// Log4Shell, which is the Scope:Changed branch and its 1.08
		// multiplier — the half of the formula the unchanged cases never
		// exercise, and the half an approximation gets wrong.
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H", 10.0},
		{"CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8},
		// No impact is no score, whatever the exploitability says.
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N", 0},
	}
	for _, tt := range tests {
		got, ok := cvss3Base(tt.vector)
		if !ok {
			t.Errorf("cvss3Base(%q) could not read the vector", tt.vector)
			continue
		}
		if got != tt.want {
			t.Errorf("cvss3Base(%q) = %v, want %v", tt.vector, got, tt.want)
		}
	}
}

// v2 and v4 are not scored, and reading as unknown is the point: a severity
// nobody published must not be invented, and an unknown is visible as a count
// rather than invisible as a pass.
func TestCVSSRefusesWhatItDoesNotScore(t *testing.T) {
	for _, v := range []string{
		"CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N",
		"AV:N/AC:L/Au:N/C:P/I:P/A:P", // v2
		"nonsense",
		"",
		"CVSS:3.1/AV:Z/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", // an invalid metric value
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/C:H/I:H/A:H",     // no scope
	} {
		if _, ok := cvss3Base(v); ok {
			t.Errorf("cvss3Base(%q) claimed it could score the vector", v)
		}
		if got := SeverityFromVector(v); got != SeverityUnknown {
			t.Errorf("SeverityFromVector(%q) = %q, want unknown", v, got)
		}
	}
}

func TestReadSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "all.zip")
	writeZip(t, path, map[string]string{
		"GHSA-good.json": `{"id":"GHSA-good","affected":[{"package":{"ecosystem":"npm","name":"lodash"},
		  "ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"4.17.21"}]}]}]}`,
		"DSA-other.json": `{"id":"DSA-other","affected":[{"package":{"ecosystem":"Debian","name":"x"},
		  "versions":["1.0"]}]}`,
		"broken.json": `{not json`,
		"README.txt":  `ignored, not a .json`,
	})

	snap, err := ReadSnapshot(path, EcosystemNPM, "osv")
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if len(snap.Records) != 1 || snap.Records[0].ID != "GHSA-good" {
		t.Errorf("Records = %+v", snap.Records)
	}
	// Both counted, and separately: "not for us" and "would not parse" are
	// different facts, and a sudden move in either is how a feed reshaping
	// itself becomes visible.
	if snap.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", snap.Skipped)
	}
	if snap.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1", snap.Unreadable)
	}
}

// A corpus that produced nothing but failures is not an empty corpus, and
// swapping it in would replace a good one with nothing.
func TestReadSnapshotRefusesAnAllFailedArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.zip")
	writeZip(t, path, map[string]string{
		"a.json": `{not json`,
		"b.json": `{also not json`,
	})
	if _, err := ReadSnapshot(path, EcosystemNPM, "osv"); err == nil {
		t.Fatal("expected a refusal rather than an empty corpus")
	}
}

// DG-3 (security review 2026-11-14): an archive with nothing for the target
// ecosystem is the wrong archive, not an empty corpus — refused, like the
// all-failed one, so the stored corpus stays.
func TestReadSnapshotRefusesAnArchiveWithNothingForTheEcosystem(t *testing.T) {
	dir := t.TempDir()
	pypi := filepath.Join(dir, "pypi.zip")
	writeZip(t, pypi, map[string]string{
		"PYSEC-1.json": `{"id":"PYSEC-1","affected":[{"package":{"ecosystem":"PyPI","name":"flask"},
		  "ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"2.0"}]}]}]}`,
	})
	if _, err := ReadSnapshot(pypi, EcosystemNPM, "osv"); err == nil {
		t.Error("a PyPI archive read as npm was accepted")
	}
	if snap, err := ReadSnapshot(pypi, EcosystemPyPI, "osv"); err != nil || len(snap.Records) != 1 {
		t.Errorf("the PyPI archive read as PyPI: %+v, %v", snap.Records, err)
	}
	empty := filepath.Join(dir, "empty.zip")
	writeZip(t, empty, map[string]string{})
	if _, err := ReadSnapshot(empty, EcosystemNPM, "osv"); err == nil {
		t.Error("an empty archive was accepted")
	}
}

// DG-6 (security review 2026-11-14): one advisory id in two files is one
// record — the last read — or the corpus swap fails on a repeated key.
func TestReadSnapshotKeepsOneRecordPerID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "all.zip")
	rec := `{"id":"GHSA-dup","affected":[{"package":{"ecosystem":"npm","name":"lodash"},
	  "ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"4.17.21"}]}]}]}`
	writeZip(t, path, map[string]string{"a.json": rec, "b.json": rec,
		"c.json": `{"id":"GHSA-other","affected":[{"package":{"ecosystem":"npm","name":"x"},
		  "ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"1.0.0"}]}]}]}`})
	snap, err := ReadSnapshot(path, EcosystemNPM, "osv")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Records) != 2 {
		t.Errorf("Records = %d, want 2 (one per id): %+v", len(snap.Records), snap.Records)
	}
}

func TestSnapshotURL(t *testing.T) {
	// The feed spells PyPI its way and this platform spells it another, and
	// the map between them is the one place that is reconciled.
	got, err := SnapshotURL("https://osv.example.com/", EcosystemPyPI)
	if err != nil {
		t.Fatalf("SnapshotURL: %v", err)
	}
	if got != "https://osv.example.com/PyPI/all.zip" {
		t.Errorf("SnapshotURL = %q", got)
	}
	if _, err := SnapshotURL(DefaultEndpoint, "alpine"); err == nil {
		t.Error("expected an error for an ecosystem with no snapshot")
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating the archive: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("adding %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the archive: %v", err)
	}
}
