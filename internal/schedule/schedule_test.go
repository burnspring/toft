package schedule

import (
	"testing"
	"time"
)

// firingsOn counts the firings Next produces whose instant falls on the given
// local calendar day — walking the schedule the way the claim does, each next
// computed from the previous firing.
func firingsOn(t *testing.T, expr string, loc *time.Location, y int, m time.Month, d int) []time.Time {
	t.Helper()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc).Add(-36 * time.Hour)
	end := time.Date(y, m, d, 0, 0, 0, 0, loc).Add(48 * time.Hour)
	var out []time.Time
	at := start
	for {
		n, err := Next(expr, at)
		if err != nil {
			t.Fatalf("Next(%q, %s): %v", expr, at, err)
		}
		if !n.Before(end) {
			return out
		}
		if l := n.In(loc); l.Year() == y && l.Month() == m && l.Day() == d {
			out = append(out, n)
		}
		at = n
	}
}

// The test whose failure would be silent: a zoned schedule fires exactly once
// on each changeover day. The cron library alone skips the spring day and fires
// twice on the autumn one, and nothing anywhere would say so.
func TestZonedScheduleFiresOncePerDayAcrossDST(t *testing.T) {
	cases := []struct {
		zone string
		expr string
		days [][3]int
	}{
		{"Europe/London", "30 1 * * *", [][3]int{{2026, 3, 29}, {2026, 10, 25}}},
		{"America/New_York", "30 2 * * *", [][3]int{{2026, 3, 8}, {2026, 11, 1}}},
		{"America/New_York", "30 1 * * *", [][3]int{{2026, 3, 8}, {2026, 11, 1}}},
	}
	for _, c := range cases {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range c.days {
			got := firingsOn(t, Join(c.zone, c.expr), loc, d[0], time.Month(d[1]), d[2])
			if len(got) != 1 {
				t.Errorf("%s %q on %d-%02d-%02d fired %d times: %v", c.zone, c.expr, d[0], d[1], d[2], len(got), got)
			}
		}
	}
}

// Across an ordinary week a zoned schedule fires at its wall-clock time, which
// is what a person who wrote 06:00 London meant in both halves of the year.
func TestZonedScheduleKeepsItsWallClockTime(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	for _, at := range []time.Time{
		time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	} {
		n, err := Next(Join("Europe/London", "0 6 * * *"), at)
		if err != nil {
			t.Fatal(err)
		}
		if l := n.In(loc); l.Hour() != 6 || l.Minute() != 0 {
			t.Errorf("after %s fired at %s local, want 06:00", at, l)
		}
	}
}

// A schedule with no zone is UTC and byte-identical to what it always was.
func TestUnzonedScheduleIsUTC(t *testing.T) {
	if Join("", "0 3 * * *") != "0 3 * * *" || Join("UTC", "0 3 * * *") != "0 3 * * *" {
		t.Error("an unzoned schedule must store the bare expression")
	}
	n, err := Next("0 3 * * *", time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 7, 2, 3, 0, 0, 0, time.UTC); !n.Equal(want) {
		t.Errorf("next = %s, want %s", n, want)
	}
}

func TestSplitAndJoinRoundTrip(t *testing.T) {
	for _, s := range []string{"CRON_TZ=Europe/London 0 3 * * *", "TZ=Asia/Tokyo */5 * * * *", "0 3 * * *"} {
		zone, expr := Split(s)
		if expr == "" {
			t.Errorf("Split(%q) lost the expression", s)
		}
		if zone == "" && s != expr {
			t.Errorf("Split(%q) = %q, %q", s, zone, expr)
		}
	}
	if z, e := Split(Join("Europe/London", "0 3 * * *")); z != "Europe/London" || e != "0 3 * * *" {
		t.Errorf("round trip = %q, %q", z, e)
	}
}

func TestAnUnknownZoneIsRefused(t *testing.T) {
	if err := Validate("CRON_TZ=Nowhere/Bad 0 3 * * *"); err == nil {
		t.Error("an unknown zone was accepted")
	}
	if err := Validate("CRON_TZ=Europe/London not cron"); err == nil {
		t.Error("a bad expression with a good zone was accepted")
	}
}

// The library accepts any day of month with any month and returns the zero time
// for one that never comes; that is an error here, on both paths, and Validate
// refuses it — or the claim writes the zero back and fires every tick (JOBS-8).
func TestAScheduleThatNeverFiresIsRefused(t *testing.T) {
	for _, expr := range []string{"0 0 31 2 *", "0 0 30 2 *", "0 0 31 4 *", "CRON_TZ=Europe/London 0 0 31 2 *"} {
		if at, err := Next(expr, time.Now()); err == nil {
			t.Errorf("Next(%q) = %v with no error, want one", expr, at)
		}
		if err := Validate(expr); err == nil {
			t.Errorf("Validate(%q) accepted a schedule that never fires", expr)
		}
	}
	if err := Validate("0 0 29 2 *"); err != nil {
		t.Errorf("Validate(29 February) = %v; it fires in a leap year", err)
	}
}

func TestMinGapFindsTheShortestInterval(t *testing.T) {
	week := 8 * 24 * time.Hour
	for _, c := range []struct {
		expr string
		want time.Duration
	}{
		{"*/5 * * * *", 5 * time.Minute},
		{"0 * * * *", time.Hour},
		{"0 4 * * *", 24 * time.Hour},
		// Once in most eight-day windows: no gap inside it, so the window.
		{"0 6 * * 1", 0},
		// Two firings an hour apart on one day only: caught across the week.
		{"0 3,4 * * 2", time.Hour},
		{"30 2,3 * * *", time.Hour},
		// A burst on one day of the month: measured from its first firing,
		// not from an eight-day window anchored at now, which held it only
		// when now fell inside the eight days before the first (C-5).
		{"*/5 4 1 * *", 5 * time.Minute},
	} {
		got, err := MinGap(c.expr, week, time.Hour)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if c.want == 0 && got < 7*24*time.Hour || c.want != 0 && got != c.want {
			t.Errorf("MinGap(%q) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// West of UTC, time.Date resolves a wall time inside the spring gap an hour
// *earlier*, so at the last firing before the gap every sub-hourly candidate
// landed not-after `after`, four of them exhausted the old bound, and the
// claim then switched the schedule off for good (code review 2026-11-11, C-1).
// The gap candidate now moves forward, as the comment on Next always promised,
// and the loop is a search.
func TestSubHourlyZonedScheduleSurvivesSpringForwardWestOfUTC(t *testing.T) {
	for _, expr := range []string{"*/5 * * * *", "* * * * *", "*/15 * * * *"} {
		s := "CRON_TZ=America/New_York " + expr
		// 2026-03-08 01:59:05 EST, the claim of the last firing before the gap.
		after := time.Date(2026, 3, 8, 6, 59, 5, 0, time.UTC)
		got, err := Next(s, after)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		// 02:00 does not exist; it fires at 03:00 EDT = 07:00Z.
		if want := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
			t.Errorf("%s: next after %s = %s, want %s", s, after, got, want)
		}
	}
	// A daily firing written inside the gap moves forward by the gap, not an
	// hour early: 02:30 on 2026-03-08 is 03:30 EDT.
	got, err := Next("CRON_TZ=America/New_York 30 2 * * *", time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("30 2 in the gap = %s, want %s", got, want)
	}
}

// At fall-back the second pass of the repeated hour resolves every candidate
// to the first pass, already past; a sub-hourly schedule asked then must find
// the next hour rather than fail (code review 2026-11-11, C-4).
func TestSubHourlyZonedScheduleSurvivesFallBackWestOfUTC(t *testing.T) {
	// 2026-11-01 01:30:05 CST, the second pass of 01:xx in Chicago.
	after := time.Date(2026, 11, 1, 7, 30, 5, 0, time.UTC)
	got, err := Next("CRON_TZ=America/Chicago */5 * * * *", after)
	if err != nil {
		t.Fatal(err)
	}
	// Each repeated wall time fires once, so the next is 02:00 CST = 08:00Z.
	if want := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("next = %s, want %s", got, want)
	}
	if err := Validate("CRON_TZ=America/Chicago * * * * *"); err != nil {
		t.Errorf("every-minute Chicago refused: %v", err)
	}
}
