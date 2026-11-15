package usage

import (
	"strings"
	"testing"

	"toft/internal/database"
)

type fakeStore struct {
	batches    [][]database.UsageBump
	keyBatches [][]database.APIKeyUsageBump
	capBatches [][]database.CapabilityBump
	sketches   [][]database.AudienceSketch
}

func (f *fakeStore) MergeAudienceSketches(rows []database.AudienceSketch) error {
	f.sketches = append(f.sketches, rows)
	return nil
}

func (f *fakeStore) BumpAppUsage(rows []database.UsageBump) error {
	f.batches = append(f.batches, rows)
	return nil
}

func (f *fakeStore) BumpAPIKeyUsage(rows []database.APIKeyUsageBump) error {
	f.keyBatches = append(f.keyBatches, rows)
	return nil
}

func (f *fakeStore) BumpCapabilityUsage(rows []database.CapabilityBump) error {
	f.capBatches = append(f.capBatches, rows)
	return nil
}

func TestFlushAggregatesAndClears(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store)

	r.Record("app1", "alice", "", true)
	r.Record("app1", "alice", "", false)
	r.Record("app1", "", "", true)
	r.Record("app2", "bob", "", false)

	r.Flush()

	if len(store.batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(store.batches))
	}
	got, loads := map[string]int64{}, map[string]int64{}
	for _, b := range store.batches[0] {
		got[b.AppID+"/"+b.UserID] = b.Requests
		loads[b.AppID+"/"+b.UserID] = b.PageLoads
	}
	want := map[string]int64{"app1/alice": 2, "app1/": 1, "app2/bob": 1}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s = %d, want %d", k, got[k], n)
		}
	}
	// Page loads ride the same row: one of alice's two requests was one.
	wantLoads := map[string]int64{"app1/alice": 1, "app1/": 1, "app2/bob": 0}
	for k, n := range wantLoads {
		if loads[k] != n {
			t.Errorf("%s page loads = %d, want %d", k, loads[k], n)
		}
	}

	// A second flush with nothing recorded must not call the store.
	r.Flush()
	if len(store.batches) != 1 {
		t.Errorf("empty flush reached the store: %d batches", len(store.batches))
	}
}

// The capability counter aggregates per app and per capability, and never per
// viewer: "which apps are reading viewer identity" is a governance question,
// while "which people did app X look up" would be a surveillance log.
func TestCapabilityCountsAggregatePerAppAndClear(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store)

	r.RecordCapability("app1", "viewer.identity")
	r.RecordCapability("app1", "viewer.identity")
	r.RecordCapability("app2", "viewer.identity")

	r.Flush()

	if len(store.capBatches) != 1 {
		t.Fatalf("capability batches = %d, want 1", len(store.capBatches))
	}
	got := map[string]int64{}
	for _, b := range store.capBatches[0] {
		got[b.AppID+"/"+b.Capability] = b.Requests
	}
	if got["app1/viewer.identity"] != 2 || got["app2/viewer.identity"] != 1 {
		t.Errorf("counts = %v, want app1=2 app2=1", got)
	}

	// A second flush with nothing recorded writes nothing.
	r.Flush()
	if len(store.capBatches) != 1 {
		t.Errorf("capability batches = %d after an empty flush, want 1", len(store.capBatches))
	}
}

// A visitor id goes into a sketch per app per day, and the same id any number
// of times is one browser.
func TestVisitorSketchesCountDistinctPerApp(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store)
	for i := 0; i < 50; i++ {
		r.RecordVisitor("app1", "same-browser")
	}
	r.RecordVisitor("app1", "other-browser")
	r.RecordVisitor("app2", "same-browser")
	r.Flush()

	if len(store.sketches) != 1 || len(store.sketches[0]) != 2 {
		t.Fatalf("sketch batches = %v, want one batch of two (one per app)", len(store.sketches))
	}
	got := map[string]uint64{}
	for _, row := range store.sketches[0] {
		sk := database.NewAudienceSketch()
		if err := sk.UnmarshalBinary(row.Sketch); err != nil {
			t.Fatal(err)
		}
		got[row.AppID] = sk.Estimate()
	}
	if got["app1"] != 2 || got["app2"] != 1 {
		t.Errorf("estimates = %v, want app1=2 app2=1", got)
	}
	r.Flush()
	if len(store.sketches) != 1 {
		t.Errorf("an empty flush wrote sketches")
	}
}

// Sketch rows reach the store in one order — by app, then day — so two flushes
// of overlapping keys take their row locks in the same sequence and cannot
// deadlock (code review 2026-11-11, D-2).
func TestAudienceSketchesAreFlushedInOneOrder(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store)
	for _, app := range []string{"zeta", "alpha", "mid", "beta"} {
		r.RecordVisitor(app, "browser-"+app)
	}
	r.Flush()
	if len(store.sketches) != 1 || len(store.sketches[0]) != 4 {
		t.Fatalf("sketch batches = %v", store.sketches)
	}
	var got []string
	for _, row := range store.sketches[0] {
		got = append(got, row.AppID)
	}
	if strings.Join(got, ",") != "alpha,beta,mid,zeta" {
		t.Errorf("rows reached the store as %v, want sorted by app", got)
	}
}
