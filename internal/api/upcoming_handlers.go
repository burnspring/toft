package api

import (
	"net/http"
	"sort"
	"time"

	"toft/internal/database"
	"toft/internal/jobs"
	"toft/internal/schedule"
)

// upcomingMaxSpan bounds one request for firings: enough for a week view and
// the hour a changeover adds, and a bound on the expansion.
const upcomingMaxSpan = 8 * 24 * time.Hour

// upcomingRestartUpperSec is a scheduled restart's own bound: it has no
// history to take a median of, only restartFiringTimeout.
var upcomingRestartUpperSec = int(restartFiringTimeout / time.Second)

// upcomingFiring is one firing in the interval — or, for an expression firing
// more often than hourly, the one entry standing for all of them, with
// `last`, `count` and `gap_sec` set.
type upcomingFiring struct {
	Kind       string `json:"kind"` // job | render | restart
	ScheduleID string `json:"schedule_id"`
	AppID      string `json:"app_id"`
	AppName    string `json:"app_name"`
	AppSlug    string `json:"app_slug"`
	// Name is the job's; a re-render and a restart are one per app and have
	// none.
	Name string `json:"name,omitempty"`
	// At is the firing, or a recurrence's first, in UTC.
	At time.Time `json:"at"`
	// Zone is the schedule's own ("" is UTC), and Schedule its expression
	// without it, for the page to put in words.
	Zone     string `json:"zone"`
	Schedule string `json:"schedule"`
	// TypicalSec is the median of recent scheduled runs that started, and
	// Basis how many; UpperSec the firing's bound — a job's effective timeout,
	// a restart's three minutes. Either is null where there is nothing to
	// say.
	TypicalSec *int `json:"typical_sec"`
	UpperSec   *int `json:"upper_sec"`
	Basis      int  `json:"basis"`

	Last   *time.Time `json:"last,omitempty"`
	Count  int        `json:"count,omitempty"`
	GapSec int        `json:"gap_sec,omitempty"`
}

// upcomingSource is one schedule as the expansion needs it, whichever table
// it came from.
type upcomingSource struct {
	firing  upcomingFiring
	raw     string
	enabled bool
	locked  bool
	next    *time.Time
}

// handleUpcomingSchedules — GET /system/scheduled/upcoming?from=&to=. Every
// firing on the install in [from, to), expanded by schedule.Next and nothing
// else (ADR §156). The page sends instants, not a date, because "a day" is the
// viewer's day and only the page knows the viewer's zone (§154): on a
// changeover day the interval is 23 or 25 hours, which is right.
//
// Paused and lock-held schedules produce no firings — drawn, they would read
// as a promise — and are counted in `held` when they would otherwise fire in
// the interval. system_admin only, like /system/scheduled.
func (s *Server) handleUpcomingSchedules(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	from, ferr := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	to, terr := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if ferr != nil || terr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "from and to are both required, as RFC 3339 instants", "code": "bad_request",
			"hint": "?from=2026-11-10T00:00:00Z&to=2026-11-11T00:00:00Z"})
		return
	}
	from, to = from.UTC(), to.UTC()
	if !to.After(from) || to.Sub(from) > upcomingMaxSpan {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "to must be after from, and at most eight days later", "code": "bad_request",
			"hint": "ask for a window of eight days or less, with to later than from"})
		return
	}

	sources, err := s.upcomingSources()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	now := time.Now().UTC()
	firings, recurring := []upcomingFiring{}, []upcomingFiring{}
	held := map[string]int{"paused": 0, "locked": 0}
	for _, src := range sources {
		first := now
		switch {
		case !src.enabled:
			// Resuming sets the next firing from the moment of the resume, so
			// what a paused schedule would do is the expression from now.
			first = maxTime(now, from.Add(-time.Nanosecond))
			n, err := schedule.Next(src.raw, first)
			if err != nil {
				continue
			}
			first = n
		case src.next != nil:
			first = *src.next
		default:
			// Not yet seeded: the claim seeds it from now on its next pass.
			n, err := schedule.Next(src.raw, now)
			if err != nil {
				continue
			}
			first = n
		}
		exp, err := schedule.Expand(src.raw, first, now, from, to)
		if err != nil || exp.Empty() {
			continue
		}
		if !src.enabled {
			held["paused"]++
			continue
		}
		if src.locked {
			held["locked"]++
			continue
		}
		if rec := exp.Recurrence; rec != nil {
			f := src.firing
			last := rec.Last
			f.At, f.Last, f.Count, f.GapSec = rec.First, &last, rec.Count, int(rec.Gap/time.Second)
			recurring = append(recurring, f)
			continue
		}
		for _, at := range exp.At {
			f := src.firing
			f.At = at
			firings = append(firings, f)
		}
	}
	sort.SliceStable(firings, func(i, j int) bool { return firings[i].At.Before(firings[j].At) })
	sort.SliceStable(recurring, func(i, j int) bool { return recurring[i].At.Before(recurring[j].At) })
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "firings": firings, "recurring": recurring, "held": held,
	})
}

// upcomingSources gathers every schedule on the install with what a firing
// says about it: the estate lists, and the typical durations beside them.
func (s *Server) upcomingSources() ([]upcomingSource, error) {
	jobDefs, err := s.db.ListAllScheduledJobs()
	if err != nil {
		return nil, err
	}
	renders, err := s.db.ListAllRenderSchedules()
	if err != nil {
		return nil, err
	}
	restarts, err := s.db.ListAllRestartSchedules()
	if err != nil {
		return nil, err
	}
	jobTypical, err := s.db.TypicalJobDurations()
	if err != nil {
		return nil, err
	}
	renderTypical, err := s.db.TypicalRenderDurations()
	if err != nil {
		return nil, err
	}
	settings, _ := s.db.GetPlatformSettings()
	env := s.envelope()

	out := make([]upcomingSource, 0, len(jobDefs)+len(renders)+len(restarts))
	add := func(kind, id, appID, appName, appSlug, name, raw string, enabled, locked bool,
		next *time.Time, typical database.TypicalDuration, hasTypical bool, upper int) {
		zone, expr := schedule.Split(raw)
		f := upcomingFiring{Kind: kind, ScheduleID: id, AppID: appID, AppName: appName,
			AppSlug: appSlug, Name: name, Zone: zone, Schedule: expr}
		if hasTypical {
			sec := typical.Seconds
			f.TypicalSec, f.Basis = &sec, typical.Basis
		}
		if upper > 0 {
			f.UpperSec = &upper
		}
		out = append(out, upcomingSource{firing: f, raw: raw, enabled: enabled, locked: locked, next: next})
	}
	for i := range jobDefs {
		j := &jobDefs[i]
		if j.Schedule == "" {
			continue
		}
		typ, ok := jobTypical[j.ID]
		upper := jobs.EffectiveLimits(&j.JobDefinition, settings, env).TimeoutSec.Value
		add("job", j.ID, j.AppID, j.AppName, j.AppSlug, j.Name, j.Schedule,
			j.Enabled, j.AppLocked, j.NextRunAt, typ, ok, upper)
	}
	for _, e := range renders {
		typ, ok := renderTypical[e.AppID]
		add("render", e.ID, e.AppID, e.AppName, e.AppSlug, "", e.Schedule,
			e.Enabled, e.AppLocked, e.NextRunAt, typ, ok, 0)
	}
	for _, e := range restarts {
		add("restart", e.ID, e.AppID, e.AppName, e.AppSlug, "", e.Schedule,
			e.Enabled, e.AppLocked, e.NextRunAt, database.TypicalDuration{}, false, upcomingRestartUpperSec)
	}
	return out, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
