package main

import "testing"

func TestScheduleZoneRoundTrip(t *testing.T) {
	stored := joinSchedule("Europe/London", "0 6 * * *")
	if stored != "CRON_TZ=Europe/London 0 6 * * *" {
		t.Fatalf("join = %q", stored)
	}
	if z, e := splitSchedule(stored); z != "Europe/London" || e != "0 6 * * *" {
		t.Errorf("split = %q, %q", z, e)
	}
	if scheduleText(stored) != "0 6 * * * (Europe/London)" || scheduleText("0 3 * * *") != "0 3 * * *" {
		t.Errorf("text = %q / %q", scheduleText(stored), scheduleText("0 3 * * *"))
	}
	if joinSchedule("UTC", "0 3 * * *") != "0 3 * * *" {
		t.Error("UTC must store the bare expression")
	}
}
