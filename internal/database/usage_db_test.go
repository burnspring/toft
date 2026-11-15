package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

func day(offset int) time.Time {
	return time.Now().UTC().AddDate(0, 0, offset)
}

func TestUsageUpsertAndAggregation(t *testing.T) {
	db := dbtest.New(t)

	bumps := []database.UsageBump{
		{AppID: "app1", UserID: "alice", Day: day(0), Requests: 3, PageLoads: 1},
		{AppID: "app1", UserID: "bob", Day: day(0), Requests: 1},
		{AppID: "app1", UserID: "", Day: day(0), Requests: 5, PageLoads: 2}, // anonymous
		{AppID: "app1", UserID: "alice", Day: day(-1), Requests: 2},
		{AppID: "app2", UserID: "alice", Day: day(0), Requests: 9}, // other app
	}
	if err := db.BumpAppUsage(bumps); err != nil {
		t.Fatalf("BumpAppUsage: %v", err)
	}
	// A second flush for the same keys must add, not replace.
	if err := db.BumpAppUsage([]database.UsageBump{
		{AppID: "app1", UserID: "alice", Day: day(0), Requests: 4, PageLoads: 1},
	}); err != nil {
		t.Fatalf("BumpAppUsage upsert: %v", err)
	}

	series, err := db.GetAppUsageDaily("app1", 30)
	if err != nil {
		t.Fatalf("GetAppUsageDaily: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("series length = %d, want 2 days", len(series))
	}
	today := series[1] // ordered oldest first
	if today.Requests != 13 {
		t.Errorf("today's requests = %d, want 13 (3+1+5+4)", today.Requests)
	}
	if today.Viewers != 2 {
		t.Errorf("today's viewers = %d, want 2 (anonymous doesn't count)", today.Viewers)
	}
	if today.PageLoads != 4 {
		t.Errorf("today's page loads = %d, want 4 (1+2+1, signed-in and anonymous alike, added on upsert)", today.PageLoads)
	}

	totals, err := db.GetAppUsageTotals("app1", 30)
	if err != nil {
		t.Fatalf("GetAppUsageTotals: %v", err)
	}
	if totals.Requests != 15 {
		t.Errorf("total requests = %d, want 15", totals.Requests)
	}
	if totals.Viewers != 2 {
		t.Errorf("total viewers = %d, want 2 (alice counted once across days)", totals.Viewers)
	}
	if totals.PageLoads != 4 {
		t.Errorf("total page loads = %d, want 4", totals.PageLoads)
	}

	// A one-day window must exclude yesterday.
	oneDay, err := db.GetAppUsageTotals("app1", 1)
	if err != nil {
		t.Fatalf("GetAppUsageTotals(1): %v", err)
	}
	if oneDay.Requests != 13 {
		t.Errorf("1-day requests = %d, want 13", oneDay.Requests)
	}
}
