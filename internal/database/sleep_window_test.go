package database

import (
	"testing"
	"time"
)

func TestIsolatedSleepAllowedAt(t *testing.T) {
	at := func(hhmm string) time.Time {
		m, err := ParseClock(hhmm)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(2026, 11, 12, m/60, m%60, 0, 0, time.UTC)
	}
	overnight := PlatformSettings{IsolatedSleepWindowEnabled: true,
		IsolatedSleepWindowStart: "19:00", IsolatedSleepWindowEnd: "07:00"}
	daytime := PlatformSettings{IsolatedSleepWindowEnabled: true,
		IsolatedSleepWindowStart: "12:00", IsolatedSleepWindowEnd: "13:30"}

	for _, tc := range []struct {
		name string
		s    PlatformSettings
		at   string
		want bool
	}{
		{"window off allows any hour", PlatformSettings{}, "10:00", true},
		{"overnight, evening", overnight, "19:00", true},
		{"overnight, after midnight", overnight, "03:15", true},
		{"overnight, the end is outside", overnight, "07:00", false},
		{"overnight, midday", overnight, "12:00", false},
		{"same-day, inside", daytime, "13:29", true},
		{"same-day, before", daytime, "11:59", false},
		{"start equal to end opens never", PlatformSettings{IsolatedSleepWindowEnabled: true,
			IsolatedSleepWindowStart: "05:00", IsolatedSleepWindowEnd: "05:00"}, "05:00", false},
		{"unreadable bounds open never", PlatformSettings{IsolatedSleepWindowEnabled: true,
			IsolatedSleepWindowStart: "7pm", IsolatedSleepWindowEnd: "07:00"}, "20:00", false},
	} {
		if got := tc.s.IsolatedSleepAllowedAt(at(tc.at)); got != tc.want {
			t.Errorf("%s: at %s = %v, want %v", tc.name, tc.at, got, tc.want)
		}
	}

	// Read in the display zone, not the server's: 19:30 in New York is
	// 00:30 UTC the next day.
	ny := overnight
	ny.DisplayTimeZone = "America/New_York"
	if !ny.IsolatedSleepAllowedAt(time.Date(2026, 11, 13, 0, 30, 0, 0, time.UTC)) {
		t.Error("19:30 New York time read as outside a 19:00–07:00 window")
	}
	if ny.IsolatedSleepAllowedAt(time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)) {
		t.Error("10:00 New York time read as inside a 19:00–07:00 window")
	}
}

func TestParseClock(t *testing.T) {
	for _, bad := range []string{"", "7:00", "24:00", "12:60", "ab:cd", "12-00"} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) accepted", bad)
		}
	}
	if m, err := ParseClock("23:59"); err != nil || m != 23*60+59 {
		t.Errorf("ParseClock(23:59) = %d, %v", m, err)
	}
}
