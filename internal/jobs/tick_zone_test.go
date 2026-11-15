package jobs

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/schedule"
)

// BB-050, fifth 2026-11-03 black-box list: a schedule's run is recorded in its
// schedule's zone, so every read of it keeps the offset TOFT_SCHEDULED_FOR has.
func TestAScheduledTickKeepsItsSchedulesZone(t *testing.T) {
	tick := time.Date(2026, 11, 3, 17, 34, 0, 0, time.UTC)
	sched := schedule.Join("America/Bogota", "*/2 * * * *")
	at := TickInZone(tick, sched)
	if got := at.Format(time.RFC3339); got != "2026-11-03T12:34:00-05:00" {
		t.Errorf("tick = %s", got)
	}
	if off := database.ScheduledOffset(&at); off != "-05:00" {
		t.Errorf("recorded offset = %q", off)
	}
	if got := database.InScheduledOffset(&at, "-05:00").Format(time.RFC3339); got != "2026-11-03T12:34:00-05:00" {
		t.Errorf("read back = %s", got)
	}
	if got := TickInZone(tick, "*/2 * * * *"); got.Location() != time.UTC {
		t.Errorf("a schedule naming no zone = %v, want UTC", got)
	}
}
