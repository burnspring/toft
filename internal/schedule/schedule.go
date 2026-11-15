// Package schedule parses the cron expressions Toft accepts.
//
// A leaf package on purpose. Both the cron trigger (internal/cronjobs) and the
// job manifest (internal/builder) need to validate a schedule, and cronjobs
// already depends on builder for base-path handling — so putting the parser in
// either one makes them mutually dependent. The rule it encodes is small and
// belongs to neither: five fields, minute granularity, UTC unless the
// expression names a zone.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// parser accepts standard 5-field cron expressions.
//
// Deliberately not the 6-field variant with seconds: a minimum interval of one
// minute is a scheduling decision, and accepting seconds here would silently
// promise a resolution the dispatcher's tick cannot deliver.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ZonePrefix is how a schedule names its time zone: `CRON_TZ=Europe/London
// 0 3 * * *`.
//
// **In the expression rather than a column**, because the shared claim
// (database.ClaimDueSchedulesIn) hands its NextFunc the expression and nothing
// else, across three tables held to the same seven columns. A prefix reaches
// every one of them, and jobs, re-renders and restarts gain a zone for the
// price of a field. The cost is that the raw string carries it, so Split is the
// one way anything renders a schedule.
const ZonePrefix = "CRON_TZ="

// Split separates a schedule into its zone and its expression. An expression
// with no zone returns "" for it, which means UTC.
//
// `TZ=` is read as well because the cron library accepts it, so a value written
// either way means the same thing everywhere.
func Split(s string) (zone, expr string) {
	t := strings.TrimSpace(s)
	for _, p := range []string{ZonePrefix, "TZ="} {
		if !strings.HasPrefix(t, p) {
			continue
		}
		rest := t[len(p):]
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			return rest, ""
		}
		return rest[:i], strings.TrimSpace(rest[i:])
	}
	return "", t
}

// Join is Split's inverse. An empty zone, or "UTC", stores the bare expression,
// so a schedule nobody gave a zone stays byte-identical to what it always was.
func Join(zone, expr string) string {
	zone, expr = strings.TrimSpace(zone), strings.TrimSpace(expr)
	if zone == "" || zone == "UTC" || expr == "" {
		return expr
	}
	return ZonePrefix + zone + " " + expr
}

// ValidateZone reports whether zone is an IANA zone name this binary knows.
// The binary embeds time/tzdata, so the answer does not depend on the image.
func ValidateZone(zone string) error {
	if zone == "" {
		return nil
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return fmt.Errorf("%q is not a time zone name (use one like Europe/London)", zone)
	}
	return nil
}

func parse(s string) (cron.Schedule, *time.Location, error) {
	zone, expr := Split(s)
	if err := ValidateZone(zone); err != nil {
		return nil, nil, err
	}
	sched, err := parser.Parse(expr)
	if err != nil {
		return nil, nil, err
	}
	if zone == "" {
		return sched, nil, nil
	}
	loc, _ := time.LoadLocation(zone)
	return sched, loc, nil
}

// Validate reports whether s is a usable schedule: one that parses and fires.
// `0 0 31 2 *` parses — the library takes any day of month with any month —
// and never fires, which is refused here rather than discovered at the claim.
func Validate(s string) error {
	_, err := Next(s, time.Now())
	return err
}

// errNever is what Next says when the library finds no match: it returns the
// zero time with no error, which a caller writing it back as next_run_at would
// read as due on every tick.
func errNever(s string) error {
	return fmt.Errorf("%q never fires: no date matches it (a day of the month that month does not have?)", s)
}

// Next computes the first firing strictly after `after`, as an instant in UTC.
//
// The claim passes *now* rather than the missed time, which is what makes a
// schedule due during downtime fire once, late, rather than once per missed
// firing.
//
// **A zone is applied on the wall clock, never handed to the cron library.**
// The library computes in the zone itself and gets daylight saving wrong both
// ways — measured with `CRON_TZ=Europe/London 30 1 * * *`, it skips 29 March
// 2026 outright and fires twice on 25 October. So the expression is matched
// against wall-clock time written as if it were UTC, where no transition
// exists, and only the match is placed in the zone. That is exactly one firing
// per scheduled wall-clock time: one inside the spring gap moves forward by the
// gap, and one that occurs twice fires once, at the instant time.Date gives it.
func Next(s string, after time.Time) (time.Time, error) {
	sched, loc, err := parse(s)
	if err != nil {
		return time.Time{}, err
	}
	return next(s, sched, loc, after)
}

// next is Next on an expression already parsed, so an expansion parses once.
func next(s string, sched cron.Schedule, loc *time.Location, after time.Time) (time.Time, error) {
	if loc == nil {
		n := sched.Next(after.UTC())
		if n.IsZero() {
			return n, errNever(s)
		}
		return n, nil
	}
	w := after.In(loc)
	wall := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
	// A search, bounded. A candidate resolves to an instant not after `after`
	// in two places only: the second pass of a repeated hour, where time.Date
	// gives the first pass, and — west of UTC — the spring gap, where it gives
	// the hour *before* rather than after. An every-minute expression has
	// sixty such candidates in a row at each, so the bound has to clear a
	// whole hour of them; four did not, and a `*/5` New York schedule was
	// switched off for good at 01:55 EST on changeover day (code review
	// 2026-11-11, C-1).
	for i := 0; i < 240; i++ {
		n := sched.Next(wall)
		if n.IsZero() {
			return n, errNever(s)
		}
		at := placeWall(n, loc)
		if at.After(after) {
			return at, nil
		}
		wall = n
	}
	return time.Time{}, fmt.Errorf("no firing of %q found after %s", s, after.UTC().Format(time.RFC3339))
}

// placeWall is the instant at which the wall-clock time n (read as UTC)
// occurs in loc. A wall time inside a spring gap does not occur; it is moved
// forward by the gap, in every zone. time.Date does that east of UTC and the
// opposite west of it (New York 02:30 on changeover day came back as 01:30
// EST), so the two are compared and the earlier answer is corrected.
func placeWall(n time.Time, loc *time.Location) time.Time {
	at := time.Date(n.Year(), n.Month(), n.Day(), n.Hour(), n.Minute(), 0, 0, loc)
	got := at.In(loc)
	asked := time.Date(n.Year(), n.Month(), n.Day(), n.Hour(), n.Minute(), 0, 0, time.UTC)
	have := time.Date(got.Year(), got.Month(), got.Day(), got.Hour(), got.Minute(), 0, 0, time.UTC)
	if have.Before(asked) {
		at = at.Add(asked.Sub(have))
	}
	return at.UTC()
}

// MinGap is the shortest interval between two consecutive firings of s across
// window, starting now. A schedule that fires once or never in the window
// returns window itself.
//
// For the hourly floor on `[restart]` and `[render]`: a restart every minute
// is an outage with a schedule attached, and a re-render every five minutes is
// a release every five minutes. The caller passes eight days, so a weekly
// expression is checked across its own boundary. It stops at the first gap
// below floor, so an every-minute expression costs two firings, not eleven
// thousand.
func MinGap(s string, window, floor time.Duration) (time.Duration, error) {
	from := time.Now().UTC()
	prev, err := Next(s, from)
	if err != nil {
		return 0, err
	}
	// The window starts at the first firing, not now: anchored at now, a
	// day-of-month expression with a sub-hourly burst had no firing inside
	// most windows and passed, and the same file was refused by any deploy
	// made in the eight days before the burst (code review 2026-11-11, C-5).
	end := prev.Add(window)
	gap := window
	for prev.Before(end) {
		n, err := Next(s, prev)
		if err != nil || n.IsZero() || n.After(end) {
			break
		}
		if d := n.Sub(prev); d < gap {
			gap = d
			if gap < floor {
				return gap, nil
			}
		}
		prev = n
	}
	return gap, nil
}
