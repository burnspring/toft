package advisory

import "testing"

// **The three rungs, and they are three different questions.** Collapsing any
// pair is how this feature acquires a hole nobody can see:
//
//   - KnownEcosystem: versions in it can be compared at all.
//   - Syncs: a corpus is held, so a finding can appear.
//   - Covered: an *absence* of findings is a verdict, which is `scanned`.
//
// CRAN is the ecosystem that separates them, and it answers true, true, false.
func TestTheThreeRungsAreDistinct(t *testing.T) {
	for _, e := range Ecosystems {
		if !KnownEcosystem(e) || !Syncs(e) || !Covered(e) {
			t.Errorf("%s is covered, so it must be known and synced", e)
		}
	}

	if !KnownEcosystem(EcosystemCRAN) {
		t.Error("cran's version ordering exists, so it is known")
	}
	if !Syncs(EcosystemCRAN) {
		t.Error("cran's corpus is fetched, so its fourteen advisories can be matched")
	}
	if Covered(EcosystemCRAN) {
		t.Error("cran's silence is not a verdict — this is the whole of the R position")
	}

	// An ecosystem this binary cannot order versions in must reach neither of
	// the inner rungs, or a Debian package would be compared under someone
	// else's rules.
	for _, e := range []string{"debian", "alpine", "maven", ""} {
		if KnownEcosystem(e) || Syncs(e) || Covered(e) {
			t.Errorf("%q is not an ecosystem this binary resolves", e)
		}
	}
}

// Synced is derived from Ecosystems rather than written out, and this is why:
// a covered ecosystem with no corpus behind it would report a permanent clean
// bill of health, which is the exact failure the whole design is written
// against. The subset relation is structural, and this states it.
func TestEveryCoveredEcosystemIsSynced(t *testing.T) {
	if len(Synced) != len(Ecosystems)+1 {
		t.Fatalf("Synced = %v, want Ecosystems plus cran", Synced)
	}
	for _, e := range Ecosystems {
		if !Syncs(e) {
			t.Errorf("%s is covered but not synced — its silence would mean nothing", e)
		}
	}

	// And the one entry that is not: cran is in the wider list and only there.
	if Covered(EcosystemCRAN) || Synced[len(Synced)-1] != EcosystemCRAN {
		t.Errorf("cran belongs to Synced alone, got Ecosystems=%v Synced=%v",
			Ecosystems, Synced)
	}
	for i, e := range Ecosystems {
		if Synced[i] != e {
			t.Errorf("Synced[%d] = %q, want %q — display order is shared", i, Synced[i], e)
		}
	}
}

// The feed publishes CRAN under its own spelling, like every other ecosystem,
// and this map is the one place that is reconciled.
func TestSnapshotURLCoversEverySyncedEcosystem(t *testing.T) {
	for _, e := range Synced {
		if _, err := SnapshotURL(DefaultEndpoint, e); err != nil {
			t.Errorf("no snapshot path for %s, which this install syncs: %v", e, err)
		}
	}
	got, err := SnapshotURL("https://osv.example.com", EcosystemCRAN)
	if err != nil {
		t.Fatalf("SnapshotURL: %v", err)
	}
	if got != "https://osv.example.com/CRAN/all.zip" {
		t.Errorf("SnapshotURL = %q", got)
	}
}

// A real record from the corpus, end to end: OSV's spelling normalises, R's
// version ordering decides, and a pinned lockfile is what makes a 2023
// advisory still bite in 2026.
func TestACRANAdvisoryParsesAndMatchesAPinnedVersion(t *testing.T) {
	doc := []byte(`{"id":"RSEC-2023-3","summary":"jsonlite denial of service",
	  "affected":[{"package":{"ecosystem":"CRAN","name":"jsonlite"},
	  "ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0.9.12"},{"fixed":"1.8.8"}]}]}]}`)
	rec, ok, err := ParseOSV(doc, "osv")
	if err != nil || !ok {
		t.Fatalf("ParseOSV: ok=%v err=%v", ok, err)
	}
	if len(rec.Affected) != 1 || rec.Affected[0].Ecosystem != EcosystemCRAN {
		t.Fatalf("a CRAN record must survive ingest, got %+v", rec.Affected)
	}
	// R names are case-sensitive and must not be folded the way PyPI's are.
	if rec.Affected[0].Package != "jsonlite" {
		t.Errorf("package = %q", rec.Affected[0].Package)
	}

	r := rec.Affected[0].Range
	if got := Matches(EcosystemCRAN, "1.8.4", r); got != Matched {
		t.Errorf("a lock pinning 1.8.4 is affected, got %v", got)
	}
	if got := Matches(EcosystemCRAN, "2.0.0", r); got != NoMatch {
		t.Errorf("today's CRAN version is past it, got %v", got)
	}
	// The separator R treats as equivalent, on the interval's own boundary.
	if got := Matches(EcosystemCRAN, "1.8-4", r); got != Matched {
		t.Errorf("1.8-4 is 1.8.4 in R, got %v", got)
	}
}
