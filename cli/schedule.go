package main

import "strings"

// A schedule's time zone travels inside its cron expression as
// `CRON_TZ=Europe/London 0 6 * * *` (ADR §103). The platform's
// internal/schedule decides that; these mirror it so nothing `toft` prints shows
// the prefix and nothing it sends spells a zone two ways.

// splitSchedule returns the zone ("" for UTC) and the bare expression.
func splitSchedule(s string) (zone, expr string) {
	t := strings.TrimSpace(s)
	for _, p := range []string{"CRON_TZ=", "TZ="} {
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

// joinSchedule is the stored form; no zone, or UTC, stores the bare expression.
func joinSchedule(zone, expr string) string {
	zone, expr = strings.TrimSpace(zone), strings.TrimSpace(expr)
	if zone == "" || zone == "UTC" || expr == "" {
		return expr
	}
	return "CRON_TZ=" + zone + " " + expr
}

// scheduleText is how a schedule is printed: the expression, and its zone in
// parentheses when it is not UTC.
func scheduleText(s string) string {
	zone, expr := splitSchedule(s)
	if zone == "" || expr == "" {
		return expr
	}
	return expr + " (" + zone + ")"
}

// isBatchApp reports whether an app is a batch app (ADR §107): it runs jobs and
// serves nothing, so it has no page to open and no service to restart.
func isBatchApp(framework string) bool { return strings.HasPrefix(framework, "batch-") }

// appWhere is the line beside an app's name: its address, or for a batch app
// the fact that it has none worth opening.
func appWhere(s *session, app *appSummary) string {
	if isBatchApp(app.Framework) {
		return "batch app — runs jobs, serves no pages (toft job list)"
	}
	return s.appURL(app)
}
