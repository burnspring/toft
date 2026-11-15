package database_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func sketchOf(t *testing.T, ids ...string) []byte {
	t.Helper()
	sk := database.NewAudienceSketch()
	for _, id := range ids {
		sk.Insert([]byte(id))
	}
	b, err := sk.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func utcDay(offset int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, offset)
}

// Union, not sum (PRD test 4): one cookie presented on seven consecutive days
// is one browser for the week and one each day; the same cookie on two apps is
// one browser across the estate.
func TestAudienceIsAUnionNotASum(t *testing.T) {
	db := dbtest.New(t)
	for d := -6; d <= 0; d++ {
		if err := db.MergeAudienceSketches([]database.AudienceSketch{
			{AppID: "app1", Day: utcDay(d), Sketch: sketchOf(t, "browser-a")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MergeAudienceSketches([]database.AudienceSketch{
		{AppID: "app2", Day: utcDay(0), Sketch: sketchOf(t, "browser-a")},
	}); err != nil {
		t.Fatal(err)
	}

	daily, err := db.AppAudienceDaily("app1", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 7 {
		t.Fatalf("days with a sketch = %d, want 7", len(daily))
	}
	for day, n := range daily {
		if n != 1 {
			t.Errorf("%s: %d browsers, want 1", day, n)
		}
	}
	total, err := db.AppAudienceTotal("app1", 30)
	if err != nil || total == nil || *total != 1 {
		t.Fatalf("window total = %v (%v), want 1 — a union, not seven", total, err)
	}

	buckets, totals, err := db.EstateAudience(utcDay(-6), utcDay(0), database.AudienceGrainDay)
	if err != nil {
		t.Fatal(err)
	}
	if totals.AnonymousBrowsers == nil || *totals.AnonymousBrowsers != 1 {
		t.Errorf("estate browsers = %v, want 1 (one cookie, two apps)", totals.AnonymousBrowsers)
	}
	if len(buckets) != 7 {
		t.Errorf("buckets = %d, want 7", len(buckets))
	}
}

// A bucket without a sketch is null, never zero (D9): the switch was off.
func TestAudienceBucketWithoutASketchIsNull(t *testing.T) {
	db := dbtest.New(t)
	if err := db.BumpAppUsage([]database.UsageBump{
		{AppID: "app1", UserID: "alice", Day: utcDay(-2), Requests: 3, PageLoads: 2},
		{AppID: "app1", UserID: "", Day: utcDay(0), Requests: 4, PageLoads: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.MergeAudienceSketches([]database.AudienceSketch{
		{AppID: "app1", Day: utcDay(0), Sketch: sketchOf(t, "x", "y")},
	}); err != nil {
		t.Fatal(err)
	}
	buckets, totals, err := db.EstateAudience(utcDay(-2), utcDay(0), database.AudienceGrainDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(buckets))
	}
	if buckets[0].AnonymousBrowsers != nil || buckets[1].AnonymousBrowsers != nil {
		t.Errorf("days with no sketch reported %v, %v; want null", buckets[0].AnonymousBrowsers, buckets[1].AnonymousBrowsers)
	}
	if buckets[0].People != 1 || buckets[0].PageLoads != 2 {
		t.Errorf("first bucket = %+v, want 1 person, 2 page loads", buckets[0])
	}
	if b := buckets[2].AnonymousBrowsers; b == nil || *b != 2 {
		t.Errorf("today's browsers = %v, want 2", b)
	}
	if totals.People != 1 || totals.PageLoads != 3 {
		t.Errorf("totals = %+v, want 1 person, 3 page loads", totals)
	}
}

// Weeks are ISO weeks, Monday-start, for the sketches and the counters alike.
func TestAudienceWeeksStartOnMonday(t *testing.T) {
	db := dbtest.New(t)
	// 2026-11-01 is a Sunday, 2026-11-02 a Monday.
	sun := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mon := sun.AddDate(0, 0, 1)
	if err := db.MergeAudienceSketches([]database.AudienceSketch{
		{AppID: "a", Day: sun, Sketch: sketchOf(t, "p")},
		{AppID: "a", Day: mon, Sketch: sketchOf(t, "q")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.BumpAppUsage([]database.UsageBump{
		{AppID: "a", UserID: "u", Day: sun, Requests: 1, PageLoads: 1},
		{AppID: "a", UserID: "u", Day: mon, Requests: 1, PageLoads: 1},
	}); err != nil {
		t.Fatal(err)
	}
	buckets, _, err := db.EstateAudience(sun, mon, database.AudienceGrainWeek)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 || buckets[0].Start != "2026-10-26" || buckets[1].Start != "2026-11-02" {
		t.Fatalf("buckets = %+v, want weeks of 2026-10-26 and 2026-11-02", buckets)
	}
	for _, b := range buckets {
		if b.AnonymousBrowsers == nil || *b.AnonymousBrowsers != 1 || b.PageLoads != 1 || b.People != 1 {
			t.Errorf("bucket %+v, want 1 browser, 1 page load, 1 person", b)
		}
	}
}

// Two replicas flushing one (app, day) lose no ids (PRD test 6): 5,000 ids
// split across two concurrent writers estimate within 2 %.
func TestAudienceConcurrentMergesLoseNothing(t *testing.T) {
	db := dbtest.New(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for batch := 0; batch < 10; batch++ {
				var ids []string
				for i := 0; i < 250; i++ {
					ids = append(ids, fmt.Sprintf("w%d-b%d-%d", w, batch, i))
				}
				if err := db.MergeAudienceSketches([]database.AudienceSketch{
					{AppID: "app1", Day: utcDay(0), Sketch: sketchOf(t, ids...)},
				}); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	total, err := db.AppAudienceTotal("app1", 1)
	if err != nil || total == nil {
		t.Fatalf("total = %v, %v", total, err)
	}
	if *total < 4900 || *total > 5100 {
		t.Errorf("estimate = %d, want 5000 ± 2%%", *total)
	}
}

// The access record's retention sweeps the sketches too (PRD test 12).
func TestAccessRecordSweepsAudienceSketches(t *testing.T) {
	db := dbtest.New(t)
	if err := db.MergeAudienceSketches([]database.AudienceSketch{
		{AppID: "app1", Day: utcDay(-400), Sketch: sketchOf(t, "old")},
		{AppID: "app1", Day: utcDay(0), Sketch: sketchOf(t, "new")},
	}); err != nil {
		t.Fatal(err)
	}
	pass, err := db.PruneAccessRecord(365, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Removed["app_audience_daily"] != 1 {
		t.Errorf("removed = %v, want one app_audience_daily row", pass.Removed)
	}
	daily, err := db.AppAudienceDaily("app1", 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 {
		t.Errorf("sketches left = %d, want today's only", len(daily))
	}
}
