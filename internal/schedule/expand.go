package schedule

import (
	"time"
)

// RecurrenceThreshold is the gap below which an expansion stops listing
// firings and describes a recurrence instead (ADR §156). An hour, because it
// is already the floor §144 puts on restarts and re-renders — so only a job
// can fall below it — and it is the resolution at which a maintenance window
// is planned.
const RecurrenceThreshold = time.Hour

// maxExpansion bounds one expression's walk. Eight days of every-minute
// firings, plus the hour a changeover adds, is 11,580; nothing the API
// accepts asks for more, and the bound turns a mistake into a short answer
// rather than an unbounded loop.
const maxExpansion = 12000

// Recurrence summarises an expression that fires more often than
// RecurrenceThreshold in an interval: its first and last firing there, how
// many, and the shortest gap between two of them.
type Recurrence struct {
	First time.Time
	Last  time.Time
	Count int
	Gap   time.Duration
}

// Expansion is one expression's firings in an interval: either each instant,
// or — when two of them are closer than RecurrenceThreshold — a Recurrence and
// no list.
type Expansion struct {
	At         []time.Time
	Recurrence *Recurrence
}

// Empty reports whether nothing fires in the interval.
func (e Expansion) Empty() bool { return len(e.At) == 0 && e.Recurrence == nil }

// Expand lists the firings of s in [from, to), as the scheduler will make
// them.
//
// first is the firing the scheduler has stored (next_run_at). It is included
// when it falls in the interval even if it is already past `now` — an overdue
// schedule fires on the next tick. Every later firing follows the claim's own
// rule: Next from the moment it fires, which for an overdue first firing is
// now, not first. Nothing here recomputes the first firing from the
// expression, because a deploy keeps an unchanged schedule's stored time
// (§98) and resuming one sets it from the moment of the resume.
func Expand(s string, first, now, from, to time.Time) (Expansion, error) {
	var out Expansion
	sched, loc, err := parse(s)
	if err != nil {
		return out, err
	}
	var list []time.Time
	if !first.Before(from) && first.Before(to) {
		list = append(list, first.UTC())
	}
	cursor := first
	if cursor.Before(now) {
		cursor = now
	}
	// Jump straight to the interval: every firing after the cursor is the
	// chain Next produces, so the first one at or after `from` is Next of the
	// instant just before it.
	if cursor.Before(from) {
		cursor = from.Add(-time.Nanosecond)
	}
	var rec *Recurrence
	for steps := 0; steps < maxExpansion; steps++ {
		n, err := next(s, sched, loc, cursor)
		if err != nil || !n.Before(to) {
			break
		}
		cursor = n
		if rec != nil {
			if d := n.Sub(rec.Last); d < rec.Gap {
				rec.Gap = d
			}
			rec.Last, rec.Count = n, rec.Count+1
			continue
		}
		if k := len(list); k > 0 && n.Sub(list[k-1]) < RecurrenceThreshold {
			rec = &Recurrence{First: list[0], Last: n, Count: k + 1, Gap: n.Sub(list[k-1])}
			for i := 1; i < k; i++ {
				if d := list[i].Sub(list[i-1]); d < rec.Gap {
					rec.Gap = d
				}
			}
			list = nil
			continue
		}
		list = append(list, n)
	}
	out.At, out.Recurrence = list, rec
	return out, nil
}
