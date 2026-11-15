package schedule

import (
	"testing"
	"time"
)

func londonDay(t *testing.T, y int, m time.Month, d int) (time.Time, time.Time) {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(y, m, d, 0, 0, 0, 0, loc).UTC(), time.Date(y, m, d+1, 0, 0, 0, 0, loc).UTC()
}

// The two changeover days: one firing each, as the claim makes them, and the
// day itself 23 and 25 hours long.
func TestExpandFiresOnceOnEachChangeoverDay(t *testing.T) {
	expr := Join("Europe/London", "30 1 * * *")
	for _, c := range []struct {
		m     time.Month
		d     int
		hours float64
	}{{time.March, 29, 23}, {time.October, 25, 25}} {
		from, to := londonDay(t, 2026, c.m, c.d)
		if h := to.Sub(from).Hours(); h != c.hours {
			t.Fatalf("%v %d: day is %v hours, want %v", c.m, c.d, h, c.hours)
		}
		first, _ := Next(expr, from.Add(-24*time.Hour))
		now := from.Add(-48 * time.Hour)
		e, err := Expand(expr, first, now, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.At) != 1 || e.Recurrence != nil {
			t.Fatalf("%v %d: got %+v, want exactly one firing", c.m, c.d, e)
		}
		want, _ := Next(expr, from.Add(-time.Nanosecond))
		if !e.At[0].Equal(want) {
			t.Fatalf("%v %d: fired at %v, Next says %v", c.m, c.d, e.At[0], want)
		}
	}
}

// Every minute for eight days is a summary, never eleven thousand instants.
func TestExpandSummarisesAFrequentExpression(t *testing.T) {
	from := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(8 * 24 * time.Hour)
	first := from
	e, err := Expand("* * * * *", first, from.Add(-time.Hour), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if e.At != nil || e.Recurrence == nil {
		t.Fatalf("got %d instants and recurrence %v", len(e.At), e.Recurrence)
	}
	r := e.Recurrence
	if r.Count != 8*24*60 || r.Gap != time.Minute || !r.First.Equal(from) || !r.Last.Equal(to.Add(-time.Minute)) {
		t.Fatalf("recurrence %+v", r)
	}
}

func TestExpandAMonthlyExpressionInAWeek(t *testing.T) {
	from := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	to := from.Add(8 * 24 * time.Hour)
	first, _ := Next("0 3 1 * *", from)
	e, err := Expand("0 3 1 * *", first, from.Add(-time.Hour), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.At) != 1 || !e.At[0].Equal(time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v", e)
	}
	// And in a week it does not reach, nothing.
	e, _ = Expand("0 3 1 * *", first, from.Add(-time.Hour), from.Add(5*24*time.Hour), to)
	if !e.Empty() {
		t.Fatalf("got %+v, want nothing", e)
	}
}

// The stored firing is the truth: an overdue one in the interval is listed,
// and the next follows from now, as the claim advances it.
func TestExpandStartsAtTheStoredFiring(t *testing.T) {
	from := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	now := from.Add(10 * time.Hour)
	overdue := from.Add(2 * time.Hour)
	e, err := Expand("0 2,14 * * *", overdue, now, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.At) != 2 || !e.At[0].Equal(overdue) || !e.At[1].Equal(from.Add(14*time.Hour)) {
		t.Fatalf("got %v", e.At)
	}
	// A stored time the expression would not give (kept across a deploy) is
	// still the first firing.
	kept := from.Add(5*time.Hour + 17*time.Minute)
	e, _ = Expand("0 14 * * *", kept, from, from, to)
	if len(e.At) != 2 || !e.At[0].Equal(kept) {
		t.Fatalf("got %v", e.At)
	}
	// A stored time before the interval is not listed, and the chain still
	// reaches into the interval.
	e, _ = Expand("0 14 * * *", from.Add(-10*time.Hour), from.Add(-24*time.Hour), from, to)
	if len(e.At) != 1 || !e.At[0].Equal(from.Add(14*time.Hour)) {
		t.Fatalf("got %v", e.At)
	}
}

func BenchmarkExpandEveryMinuteForADay(b *testing.B) {
	from := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	for i := 0; i < b.N; i++ {
		_, _ = Expand("CRON_TZ=Europe/London * * * * *", from, from, from, to)
	}
}
