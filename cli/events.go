package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

const eventsUsage = `toft events — what has gone wrong with this app, newest first

A different question from the log: the log is what the app said, and this is
what the platform noticed. It is the command to run after a restart that did
not take.

Flags:
  --app <slug>        Which app
  --since <t>         Only events since then: RFC3339, or a duration like 2h
  --severity <s>      Only this severity
  --kind <k>          Only this kind
  --limit <n>         At most this many
  --json              The list as JSON
`

// eventSeverities and eventKinds are what the install records: database's
// EventSeverity* and the Kind* constants of internal/events, plus the
// integrations' notify_failed. A kind the install grows is added here too —
// TestEventKindsAreTheInstalls holds the two together.
var eventSeverities = []string{"error", "warning", "info"}

var eventKinds = []string{
	"deploy_failed", "git_auth_failed", "git_fetch_failed", "app_crashed", "app_flapping",
	"app_restarted", "app_not_restarting", "app_memory_limit", "app_out_of_memory",
	"app_cpu_throttled", "job_run_failed", "limit_clamped", "releases_pruned", "source_blocked",
	"schedule_disabled", "schedule_removed", "job_schedule_skipped", "job_run_overdue",
	"dependency_blocked", "dependency_advisory", "notify_failed",
}

// oneOf refuses a filter value outside its list, naming the list. Empty is
// no filter.
func oneOf(flag, value string, valid []string) error {
	if value == "" {
		return nil
	}
	for _, v := range valid {
		if v == value {
			return nil
		}
	}
	return usageCodef("invalid_"+flag, "--%s %q is not one this install records — one of %s",
		flag, value, strings.Join(valid, ", "))
}

// appEvent is the part of an event row this client reads. The server sends
// more; a field this binary does not know is not a field it should invent a
// rendering for.
type appEvent struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Severity     string    `json:"severity"`
	Source       string    `json:"source"`
	Summary      string    `json:"summary"`
	Detail       string    `json:"detail"`
	DeploymentID string    `json:"deployment_id,omitempty"`
	OccurredAt   time.Time `json:"occurred_at"`
}

func cmdEvents(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("events", eventsUsage)
	g.register(fs)
	since := fs.String("since", "", "only events since then")
	severity := fs.String("severity", "", "only this severity")
	kind := fs.String("kind", "", "only this kind")
	limit := fs.Int("limit", 0, "at most this many")
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}

	// The install filters on whatever it is sent, so a misspelt kind answered
	// "nothing has gone wrong" — the wrong question, silently (BB-046).
	if err := oneOf("severity", *severity, eventSeverities); err != nil {
		return err
	}
	if err := oneOf("kind", *kind, eventKinds); err != nil {
		return err
	}

	// Parsed here rather than sent on trust: the endpoint ignores a `since` it
	// cannot read, which would silently answer a different question from the
	// one that was asked.
	sinceTime, err := parseSince(*since)
	if err != nil {
		return err
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	appValue, err := pickApp(e, s)
	if err != nil {
		return err
	}
	app, err := s.resolveApp(appValue.Value)
	if err != nil {
		return err
	}

	params := map[string]string{}
	if !sinceTime.IsZero() {
		params["since"] = sinceTime.UTC().Format(time.RFC3339)
	}
	if *severity != "" {
		params["severity"] = *severity
	}
	if *kind != "" {
		params["kind"] = *kind
	}
	if *limit > 0 {
		params["limit"] = strconv.Itoa(*limit)
	}

	var events []appEvent
	path := client.Query("/api/v1/apps/"+app.ID+"/events", params)
	if err := s.client.Do(http.MethodGet, path, nil, &events); err != nil {
		return err
	}

	if g.json {
		return writeNDJSON(e.out.Stdout, events)
	}
	if len(events) == 0 {
		e.out.Say("Nothing has gone wrong with %s%s.", app.Slug, sinceSuffix(*since))
		return nil
	}
	if e.out.Rich() {
		printEventsRich(e, events)
		return nil
	}
	for _, ev := range events {
		// The summary and detail quote what an app or its build wrote — a
		// failed build's log tail — and no app's bytes drive the terminal
		// (security review 2026-11-14, CS-5).
		ev.Summary, ev.Detail = plainText(ev.Summary), plainText(ev.Detail)
		e.out.Answer("%s  %-8s %-18s %s",
			e.out.Dimmed(ev.OccurredAt.Local().Format("2006-01-02 15:04:05 MST")),
			severityWord(e, ev.Severity), ev.Kind, ev.Summary)
		if ev.Detail != "" {
			// Line by line, for the reason Dim is: a multi-line detail
			// rendered as one block came back padded into a rectangle. On
			// stdout with its headline — the detail is the answer too, the
			// rerun remedy included, and on stderr a pipe kept the headline
			// and lost it (docs/bugs 2026-10-28 D19).
			for _, line := range strings.Split(ev.Detail, "\n") {
				e.out.Answer("    %s", e.out.Dimmed(line))
			}
		}
	}
	return nil
}

// printEventsRich is the events as a person scans them: a card per event, the
// severity and kind on top, the sentence under it, and the detail dimmed below
// that — unless it only repeats the sentence, which a failed job's often does.
func printEventsRich(e *env, events []appEvent) {
	for i, ev := range events {
		if i > 0 {
			e.out.Answer("")
		}
		ev.Summary, ev.Detail = plainText(ev.Summary), plainText(ev.Detail)
		e.out.Answer("%s  %s  %s", e.out.Badge(ev.Severity), e.out.Bold(ev.Kind),
			e.out.Dimmed(ui.Ago(ev.OccurredAt)+" · "+ev.OccurredAt.Local().Format("2006-01-02 15:04:05 MST")))
		e.out.Answer("  %s", e.out.Wrap(ev.Summary, 2))
		detail := strings.TrimSpace(ev.Detail)
		if detail == "" {
			continue
		}
		var lines []string
		for _, line := range strings.Split(detail, "\n") {
			line = strings.TrimRight(line, " \t\r")
			if line == "" || strings.Contains(ev.Summary, line) {
				continue
			}
			lines = append(lines, line)
		}
		// A failed deploy's detail is the build log's tail, which is a page
		// under one card. The first lines and the count of the rest; the
		// whole of it is `--json`, or the log.
		const shown = 4
		for i, line := range lines {
			if i == shown {
				e.out.Answer("  %s", e.out.Dimmed(fmt.Sprintf("… %d more %s — %s", len(lines)-shown,
					plural(len(lines)-shown, "line", "lines"), eventMoreHint(ev))))
				break
			}
			e.out.Answer("  %s", e.out.Faint(e.out.Wrap(line, 2)))
		}
	}
}

// severityWord colours the severity without inventing a vocabulary: an unknown
// word from a newer install prints as itself rather than being bucketed.
func severityWord(e *env, severity string) string {
	switch severity {
	case "error", "critical":
		return e.out.Fail(severity)
	case "warning":
		return e.out.Caution(severity)
	}
	return severity
}

// eventMoreHint is where the rest of a clipped detail is.
func eventMoreHint(ev appEvent) string {
	if ev.DeploymentID != "" {
		return "toft logs --deploy " + ev.DeploymentID
	}
	return "--json has all of it"
}

func sinceSuffix(since string) string {
	if since == "" {
		return ""
	}
	return " since " + since
}

// parseSince reads either an instant or a duration back from now.
//
// Both spellings exist because both are what people have: a pipeline that
// recorded when it started has an RFC3339 timestamp, and a person at a terminal
// has "the last two hours". A value that is neither is exit 2 naming the flag,
// never a filter silently ignored.
func parseSince(since string) (time.Time, error) {
	if strings.TrimSpace(since) == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, since); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(since); err == nil {
		if d < 0 {
			d = -d
		}
		return time.Now().Add(-d), nil
	}
	return time.Time{}, usageCodef("invalid_duration", "--since %q is neither a timestamp nor a duration — "+
		"try an RFC3339 time like %s, or a duration like 2h", since,
		time.Now().UTC().Format(time.RFC3339))
}
