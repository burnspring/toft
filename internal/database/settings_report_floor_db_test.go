package database_test

import (
	"testing"

	"toft/internal/database/dbtest"
)

// DB1-1: the report delivery floor was never written nor read, so it was 15
// whatever an administrator saved.
func TestTheReportDeliveryFloorRoundTrips(t *testing.T) {
	db := dbtest.New(t)
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.ReportDeliveryMinIntervalMinutes = 0
	if err := db.SetPlatformSettings(&s, "builtin-admin", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.ReportDeliveryMinIntervalMinutes != 0 {
		t.Fatalf("floor = %d after saving 0", got.ReportDeliveryMinIntervalMinutes)
	}
}
