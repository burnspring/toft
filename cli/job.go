package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

// `toft job` — a job is a command the app declares in toft.toml, and each run
// of it is a process of its own with its own timeout and memory limit.
//
// A noun with subcommands rather than a flat verb, because it is a collection
// with its own runs. It is Toft's one automation concept: a schedule is a key
// on a job, and a run ends exactly once — nothing re-runs automatically, so
// running one again is `toft job rerun`, a deliberate act that records which
// run it repeats.

const jobUsage = `toft job — jobs: commands your app declares in toft.toml, each run a process
of its own with its own timeout and its own memory limit

Usage:
  toft job list                          The jobs this app declares: schedule,
                                         params, limits and where runs go
  toft job run                           The jobs and their params, and how to run one
  toft job run <job>                     Start one and wait for it
  toft job rerun <runID>                 Run one again, with the same params
  toft job wait <runID>                  Wait for a run that has already started
  toft job runs [job]                    Recent runs, of one job when named
  toft job runs <runID>                  One run in full, and what to try next
  toft job logs <runID>                  What one run printed
  toft job output <runID> [-o file]      What one run wrote with write_output,
                                         as bytes: to stdout, or to a file
  toft job cancel <runID>                Stop a run that is still going

A run id may be the eight characters ` + "`toft job runs`" + ` prints.

Flags:
  --app <slug>          Which app
  --param k=v           A parameter, repeatable. The install holds it to the
                        job's declared type and choices. A required one left
                        out is asked for when a terminal is attached
  --for <time>          The instant this run is for, e.g. re-running Tuesday's work:
                        2026-10-06T03:00:00+01:00, or 2026-10-06 / 2026-10-06T03:00
                        read as UTC. The run sees it as TOFT_SCHEDULED_FOR
  --idempotency-key K   Starts carrying the same key within 24 hours are one run
  --no-wait             Start it and return the run id
  --follow              Stream the run's log while waiting; with toft job logs,
                        keep reading until the run ends
  --timeout <d>         How long to wait for a run (default: as long as it takes)
  --limit <n>           How many runs to list
  --force               With toft job output -o, replace a file already there
  --json                Machine-readable output on stdout, a document per line: a
                        run that did not succeed prints the run, then the refusal

Exit codes for run, rerun and wait are the run's outcome: 0 succeeded; 1 failed,
timed_out, out_of_memory, cancelled or skipped. A full queue is 1 too — try
again later. A parameter the job refuses, or a job it does not declare, is 2.
`

// Run states. The set is the server's and it is closed
// (internal/database/jobs.go); every word survives into the output, because
// `timed_out` and `out_of_memory` send a developer to two different places.
const (
	jobRunQueued      = "queued"
	jobRunRunning     = "running"
	jobRunSucceeded   = "succeeded"
	jobRunFailed      = "failed"
	jobRunTimedOut    = "timed_out"
	jobRunOutOfMemory = "out_of_memory"
	jobRunCancelled   = "cancelled"
	jobRunSkipped     = "skipped"
)

// jobRunTerminal is the closed set of endings, and every one that is not
// `succeeded` is exit 1.
//
// Several statuses onto one code, and the *word* printed on the last line every
// time. A further exit code would move a distinction a person reads into a
// place a pipeline would have to learn, and exit 1 is already defined as "the
// platform did the thing and it failed". `skipped` is among them: a scheduled
// run the overlap rule stood down did no work, and a pipeline waiting on it
// must not read that as the work having been done.
func jobRunTerminal(status string) (terminal, ok bool) {
	switch status {
	case jobRunSucceeded:
		return true, true
	case jobRunFailed, jobRunCancelled, jobRunTimedOut, jobRunOutOfMemory, jobRunSkipped:
		return true, false
	}
	return false, false
}

// paramSpec is one declared parameter, as toft.toml wrote it.
type paramSpec struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Default  any    `json:"default,omitempty"`
	Help     string `json:"help,omitempty"`
	Choices  []any  `json:"choices,omitempty"`
}

// jobLimit is one effective figure and where it came from: `declared`,
// `install default`, `override` or `clamped` — for a clamp, what toft.toml had
// asked for — and, for memory and CPU, how the install holds it: `enforced`,
// `sampled` or `not_enforced`.
type jobLimit struct {
	Value       int    `json:"value"`
	Source      string `json:"source"`
	Declared    int    `json:"declared,omitempty"`
	Enforcement string `json:"enforcement,omitempty"`
}

type jobLimits struct {
	Timeout jobLimit `json:"timeout"`
	Memory  jobLimit `json:"memory"`
	CPU     jobLimit `json:"cpu"`
}

type jobDefinition struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Command    string               `json:"command,omitempty"`
	Args       []string             `json:"args,omitempty"`
	Schedule   string               `json:"schedule,omitempty"`
	ParamsSpec map[string]paramSpec `json:"params_spec,omitempty"`
	TimeoutSec int                  `json:"timeout_sec"`
	Overlap    string               `json:"overlap,omitempty"`
	Notify     string               `json:"notify,omitempty"`
	Enabled    bool                 `json:"enabled"`
	NextRunAt  *time.Time           `json:"next_run_at,omitempty"`
	// ExpectWithinSec is how long the job may go without a success before
	// its owners are told it is overdue (ADR §105).
	ExpectWithinSec *int `json:"expect_within_sec,omitempty"`
	// Limits are the effective timeout and memory, each with its source —
	// what will actually stop a run, which is not always what was declared.
	Limits *jobLimits `json:"limits,omitempty"`
	// Paused is the schedule switched off in Toft's UI, until the next deploy.
	Paused bool `json:"paused"`
	// HeldByLock is a schedule the app's lock holds: no firing runs, and the
	// platform leaves next_run_at out rather than promise one (D15).
	HeldByLock bool `json:"held_by_lock"`
}

// jobList is the envelope GET /apps/{id}/jobs answers with. The capability
// state travels with the list: an empty list on its own would read as "this
// app declares none" rather than "this install offers none".
type jobListBody struct {
	Jobs      []jobDefinition `json:"jobs"`
	Available *bool           `json:"jobs_available"`
	Because   string          `json:"unavailable_bec"`
	// Placement is where runs go: "cluster" or "this machine".
	Placement string `json:"placement"`
}

type jobRun struct {
	ID            string          `json:"id"`
	Job           string          `json:"job"`
	Status        string          `json:"status"`
	StartedBy     string          `json:"started_by"`
	StartedByUser string          `json:"started_by_user,omitempty"`
	RerunOf       string          `json:"rerun_of,omitempty"`
	Params        json.RawMessage `json:"params,omitempty"`
	// ScheduledFor is the instant the run is for (ADR §104).
	ScheduledFor    *time.Time `json:"scheduled_for,omitempty"`
	QueuedAt        time.Time  `json:"queued_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Deadline        *time.Time `json:"deadline,omitempty"`
	Progress        int        `json:"progress"`
	ProgressMessage string     `json:"progress_message,omitempty"`
	QueuePosition   int        `json:"queue_position,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	PeakMemoryMB    *int       `json:"peak_memory_mb,omitempty"`
	Deduplicated    bool       `json:"deduplicated,omitempty"`
	// ExecutionMode is where the run actually ran: `isolated` in a pod,
	// `shared` on the platform's own machine (ADR §106).
	ExecutionMode string `json:"execution_mode,omitempty"`

	// raw is the run as the install sent it. `--json` prints this rather than
	// the fields above, so a field this binary is too old to name — or one it
	// has no use for — still reaches the caller.
	raw json.RawMessage
}

func (r *jobRun) UnmarshalJSON(b []byte) error {
	type plain jobRun
	if err := json.Unmarshal(b, (*plain)(r)); err != nil {
		return err
	}
	r.raw = append(json.RawMessage(nil), b...)
	return nil
}

func (r jobRun) MarshalJSON() ([]byte, error) {
	if len(r.raw) > 0 {
		return r.raw, nil
	}
	type plain jobRun
	return json.Marshal(plain(r))
}

type jobLogLine struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
	// Raw: see logLine.Raw.
	Raw string `json:"raw,omitempty"`
}

func cmdJob(e *env, args []string) error {
	if len(args) == 0 {
		ui.Help(os.Stderr, jobUsage)
		return usageCodef("missing_argument", "toft job needs a subcommand: list, run, rerun, wait, runs, logs, output or cancel")
	}
	switch args[0] {
	case "list":
		return jobList(e, args[1:])
	case "run":
		return jobRunCmd(e, args[1:])
	case "rerun":
		return jobRerun(e, args[1:])
	case "wait":
		return jobWait(e, args[1:])
	case "runs":
		return jobRuns(e, args[1:])
	case "logs":
		return jobLogs(e, args[1:])
	case "output":
		return jobOutput(e, args[1:])
	case "cancel":
		return jobCancel(e, args[1:])
	case "-h", "--help", "help":
		ui.Help(os.Stdout, jobUsage)
		return nil
	}
	return unknownSubcommand("job", args, "list, run, rerun, wait, runs, logs, output or cancel")
}

// noSuchName turns "you named something that is not there" into exit 2.
//
// **A name that does not resolve is usage, not failure, and the exit code is
// the whole of what an unattended caller reads.** Exit 1 is defined as *the
// platform did the thing and it failed*, so a misspelled run id answered with
// 1 tells an agent a run occurred: it goes looking for logs that do not exist,
// or reports a failed job to a person, when the correct act was to fix the
// argument.
//
// The install's sentence is kept, and its refusal travels with it so its hint
// is printed; the client's own next step is appended because only the client
// knows which command lists the valid answers.
func noSuchName(err error, lister string) error {
	var apiErr *client.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		msg, hint := apiErr.Message, apiErr.Hint
		if hint == "" {
			// A hint of its own, so --json carries it as every other run
			// command's refusal does (BB-082).
			msg += " — `" + lister + "` names them"
			hint = "`" + lister + "` names them"
		}
		return &usageError{msg: msg, code: orDefault(apiErr.Code, "not_found"), hint: hint, from: apiErr}
	}
	return err
}

// fetchJobs reads the app's declared jobs.
func fetchJobs(s *session, appID string) (*jobListBody, error) {
	var body jobListBody
	if err := s.client.Do(http.MethodGet, "/api/v1/apps/"+appID+"/jobs", nil, &body); err != nil {
		return nil, err
	}
	return &body, nil
}

func jobList(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job list", jobUsage)
	g.register(fs)
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	body, err := fetchJobs(s, app.ID)
	if err != nil {
		return err
	}
	holdByLock(body.Jobs, app.locked())
	if g.json {
		return encodeJobs(e, body.Jobs)
	}
	sayJobsVenue(e, body, app)
	if len(body.Jobs) == 0 {
		sayNoJobs(e, app)
		return nil
	}
	for i, d := range body.Jobs {
		if i > 0 {
			e.out.Answer("")
		}
		if e.out.Rich() {
			renderJobRich(e, d, app.locked())
		} else {
			renderJob(e, d, app.locked())
		}
	}
	return nil
}

// encodeJobs is the --json shape of the job list: the install's rows, with
// the schedule split into its expression and its zone — the install stores
// them as one string, `CRON_TZ=Europe/London 0 3 * * *`, which no reader
// should have to parse — and the overdue window always present, null when
// the job declares none (BB-043).
func encodeJobs(e *env, defs []jobDefinition) error {
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		raw, err := json.Marshal(d)
		if err != nil {
			return err
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if d.Schedule != "" {
			zone, expr := splitSchedule(d.Schedule)
			row["schedule"] = expr
			row["timezone"] = orDefault(zone, "UTC")
		}
		// `params` is the documented name (BB-010); `params_spec` is what
		// the install calls it, kept for a caller already reading it. Both
		// always present, {} when the job declares none.
		spec := map[string]paramSpec{}
		for k, v := range d.ParamsSpec {
			spec[k] = v
		}
		row["params"] = spec
		row["params_spec"] = spec
		// A timeout is a deadline the platform holds every run to, in either
		// execution mode: the install sends no word for it, so the row says
		// what is always true (BB-010).
		if limits, ok := row["limits"].(map[string]any); ok {
			if t, ok := limits["timeout"].(map[string]any); ok && t["enforcement"] == nil {
				t["enforcement"] = "enforced"
			}
		}
		row["expect_within_sec"] = nil
		row["expect_within"] = nil
		if d.ExpectWithinSec != nil {
			row["expect_within_sec"] = *d.ExpectWithinSec
			row["expect_within"] = expectWithinText(*d.ExpectWithinSec)
		}
		out = append(out, row)
	}
	return writeNDJSON(e.out.Stdout, out)
}

// sayJobsVenue is the line every rendering of jobs leads with (ADR §33): what
// a run is, and where on this install it goes — or, when it goes nowhere, the
// install's own sentence saying why. A static app has none to lead into:
// sayNoJobs says it runs no jobs, and a sentence about how a run goes would be
// contradicted by the next line (drills 2026-10-28 #7).
func sayJobsVenue(e *env, body *jobListBody, app *appSummary) {
	if app.Framework == "static" && len(body.Jobs) == 0 {
		return
	}
	if body.Available != nil && !*body.Available {
		if body.Because != "" {
			e.out.Say("%s", body.Because)
		} else {
			e.out.Say("This install runs no jobs.")
		}
		return
	}
	where := ""
	switch body.Placement {
	case "cluster":
		where = " Runs go to the cluster."
	case "this machine":
		where = " Runs go to this install's own machine."
	}
	e.out.Say("A job runs as a process of its own, with its own timeout and memory limit.%s", where)
}

// heldByLock is a schedule's state while its app is locked: no firing runs
// until an administrator lifts the lock.
const heldByLock = "held by the app's lock — no firing runs until it is lifted"

// holdByLock makes a locked app's schedules read as held in every rendering,
// --json included: held_by_lock set and no next_run_at. The platform says so
// itself; this covers one that predates the field (D15).
func holdByLock(defs []jobDefinition, locked bool) {
	for i := range defs {
		if locked && defs[i].Schedule != "" && !defs[i].Paused {
			defs[i].HeldByLock = true
		}
		if defs[i].HeldByLock {
			defs[i].NextRunAt = nil
		}
	}
}

// sayNoJobs is the empty job list, with the remedy that applies to this app.
// A static app refuses a [jobs] table (manifest_invalid), so advising one sent
// the caller into that refusal (docs/bugs 2026-10-28 D14).
func sayNoJobs(e *env, app *appSummary) {
	if app.Framework == "static" {
		e.out.Say("%s is a static app, which runs no jobs — a [jobs] table needs an app in a language "+
			"Toft supports, and a deploy of one here is refused.", app.Slug)
		return
	}
	e.out.Say("%s declares none — add a [jobs.<name>] table to toft.toml and deploy.", app.Slug)
}

// renderJob is one job as `toft job list` prints it: the name and when it
// runs, then its params, then the limits that will stop a run — each with the
// source, because a developer who cannot see which limit applies finds out by
// having a run stopped by it.
//
// locked is the app's lock: the scheduler skips every firing of a locked
// app, so its next firing is said as held rather than promised (docs/bugs
// 2026-10-28 D15).
func renderJob(e *env, d jobDefinition, locked bool) {
	head := []string{"on demand"}
	if d.Schedule != "" {
		head = []string{scheduleText(d.Schedule)}
		if d.Paused {
			head = append(head, e.out.Fail("paused until next deploy"))
		} else if locked || d.HeldByLock {
			head = append(head, e.out.Fail(heldByLock))
		} else if d.NextRunAt != nil {
			head = append(head, "next "+nextRunTime(d.Schedule, *d.NextRunAt))
		}
		if d.Overlap != "" {
			head = append(head, "overlap "+d.Overlap)
		}
	}
	if d.ExpectWithinSec != nil && *d.ExpectWithinSec > 0 {
		head = append(head, fmt.Sprintf("alert after %s without a success", expectWithinText(*d.ExpectWithinSec)))
	}
	if d.Notify != "" && d.Notify != "none" {
		head = append(head, "notify "+d.Notify)
	}
	e.out.Answer("%s  %s", e.out.Bold(d.Name), strings.Join(head, " · "))
	if d.Limits != nil {
		e.out.Answer("  %s · %s · %s", limitText("timeout", d.Limits.Timeout, durationText),
			limitText("memory", d.Limits.Memory, mbText), limitText("cpu", d.Limits.CPU, cpuText))
	}
	for _, name := range sortedParams(d.ParamsSpec) {
		e.out.Answer("  --param %s", paramText(name, d.ParamsSpec[name]))
	}
}

// renderJobRich is renderJob drawn for a person: the schedule tinted and the
// next run as a distance, a pause in the caution tone, the schedule's rules on
// a line of their own, and a limit the install lowered marked as such.
func renderJobRich(e *env, d jobDefinition, locked bool) {
	when := e.out.Dimmed("on demand")
	if d.Schedule != "" {
		when = e.out.Info("⏱ " + scheduleText(d.Schedule))
		if d.Paused {
			when += "  " + e.out.Caution("■ paused until next deploy")
		} else if locked || d.HeldByLock {
			when += "  " + e.out.Caution("■ "+heldByLock)
		} else if d.NextRunAt != nil {
			when += "  " + e.out.Dimmed("next "+nextRunTime(d.Schedule, *d.NextRunAt)+" · "+untilText(*d.NextRunAt))
		}
	}
	e.out.Answer("%s  %s", e.out.Bold(d.Name), when)

	var rules []string
	if d.Schedule != "" && d.Overlap != "" {
		rules = append(rules, "overlap "+d.Overlap)
	}
	if d.ExpectWithinSec != nil && *d.ExpectWithinSec > 0 {
		rules = append(rules, fmt.Sprintf("alert after %s without a success", expectWithinText(*d.ExpectWithinSec)))
	}
	if d.Notify != "" && d.Notify != "none" {
		rules = append(rules, "notify "+d.Notify)
	}
	if len(rules) > 0 {
		e.out.Field(9, "rules", strings.Join(rules, " · "))
	}
	if d.Limits != nil {
		e.out.Field(9, "limits", limitRich(e, "timeout", d.Limits.Timeout, durationText)+
			e.out.Dimmed(" · ")+limitRich(e, "memory", d.Limits.Memory, mbText)+
			e.out.Dimmed(" · ")+limitRich(e, "cpu", d.Limits.CPU, cpuText))
	}
	for _, name := range sortedParams(d.ParamsSpec) {
		e.out.Field(9, "param", paramRich(e, name, d.ParamsSpec[name]))
	}
}

// limitRich is limitText with its source dimmed — or, where the install has
// lowered what was declared, in the caution tone, since that is the limit that
// stops a run its developer did not expect stopped.
func limitRich(e *env, label string, l jobLimit, unit func(int) string) string {
	text := limitText(label, l, unit)
	open := strings.Index(text, " (")
	if open < 0 {
		return text
	}
	source := text[open+1:]
	if l.Source == "clamped" || l.Source == "install ceiling" || l.Enforcement == "not_enforced" || l.Enforcement == "sampled" {
		return text[:open] + " " + e.out.Caution(source)
	}
	return text[:open] + " " + e.out.Dimmed(source)
}

// paramRich is paramText with the flag spelling bold and the description dim.
func paramRich(e *env, name string, p paramSpec) string {
	flag, rest, _ := strings.Cut(paramText(name, p), "  ")
	if rest == "" {
		return e.out.Bold("--param " + flag)
	}
	return e.out.Bold("--param "+flag) + "  " + e.out.Dimmed(rest)
}

// untilText is how far off an instant is, the way a column says it: "in 5m".
func untilText(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= time.Minute:
		return "due now"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

// runBadge is a run's status drawn for a person: a glyph and a tone from the
// run's own closed set — `out_of_memory` among them, which the kit does not
// know — beside the text standing for it, which for an open run is how far
// along it is. Off a terminal it is jobStatusWord.
func runBadge(e *env, status, text string) string {
	if !e.out.Rich() {
		return jobStatusWord(e, status, text)
	}
	switch status {
	case jobRunSucceeded:
		return e.out.OK("✓ " + text)
	case jobRunFailed, jobRunTimedOut, jobRunOutOfMemory:
		return e.out.Fail("✕ " + text)
	case jobRunCancelled, jobRunSkipped:
		return e.out.Dimmed("○ " + text)
	case jobRunQueued:
		return e.out.Info("◌ " + text)
	case jobRunRunning:
		return e.out.Info("◐ " + text)
	}
	return text
}

// limitText is "timeout 45m (declared)", "timeout 1h (install default)", or
// "timeout 15m (clamped by admin, declared 45m)".
func limitText(label string, l jobLimit, unit func(int) string) string {
	source := orDefault(l.Source, "install default")
	if l.Source == "clamped" {
		source = "clamped by admin"
		if l.Declared > 0 {
			source += ", declared " + unit(l.Declared)
		}
	}
	switch l.Enforcement {
	case "sampled":
		source += ", sampled rather than held by the kernel"
	case "not_enforced":
		source += ", not enforced on this install"
	}
	return fmt.Sprintf("%s %s (%s)", label, unit(l.Value), source)
}

func mbText(mb int) string { return fmt.Sprintf("%d MB", mb) }

// cpuText is a CPU limit in cores, the unit a person thinks in: "1 core",
// "0.5 cores", and "none" for an install that set no limit.
func cpuText(m int) string {
	switch {
	case m <= 0:
		return "none"
	case m == 1000:
		return "1 core"
	}
	return strconv.FormatFloat(float64(m)/1000, 'f', -1, 64) + " cores"
}

func durationText(sec int) string { return shortDuration(time.Duration(sec) * time.Second) }

// shortDuration is a duration the way toft.toml writes one: 45m, 1h, 1h30m,
// 1m30s. Every unit that is not zero is written — "90s" was shown as 1m, a
// rounding nobody declared (BB-017, third 2026-11-03 list).
func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	out := ""
	if h > 0 {
		out += fmt.Sprintf("%dh", h)
	}
	if m > 0 {
		out += fmt.Sprintf("%dm", m)
	}
	if sec > 0 {
		out += fmt.Sprintf("%ds", sec)
	}
	return out
}

func sortedParams(spec map[string]paramSpec) []string {
	names := make([]string, 0, len(spec))
	for n := range spec {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// paramText is one parameter as a person needs it to type the flag:
// `region=<string>  required, one of emea|amer|apac — Which region`.
func paramText(name string, p paramSpec) string {
	line := name + "=<" + orDefault(p.Type, "string") + ">"
	var notes []string
	switch {
	case p.Required:
		notes = append(notes, "required")
	case p.Default != nil:
		notes = append(notes, "default "+valueText(p.Default))
	default:
		notes = append(notes, "optional")
	}
	if len(p.Choices) > 0 {
		notes = append(notes, "one of "+choicesText(p.Choices))
	}
	line += "  " + strings.Join(notes, ", ")
	if p.Help != "" {
		line += " — " + p.Help
	}
	return line
}

func valueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func choicesText(choices []any) string {
	words := make([]string, len(choices))
	for i, c := range choices {
		words[i] = valueText(c)
	}
	return strings.Join(words, "|")
}

// runTime is an instant as a person reads it on this machine: Oct 23 02:00 BST.
func runTime(t time.Time) string { return t.Local().Format("Jan 2 15:04 MST") }

// nextRunTime is a schedule's next firing in the schedule's own zone — the one
// printed beside it — so America/Bogota's 19:20 is not shown as the reader's
// 19:20 EST (D-43). A schedule with no zone, or a zone this machine cannot
// load, is in the reader's local time, as every other time here is.
func nextRunTime(schedule string, t time.Time) string {
	if zone, _ := splitSchedule(schedule); zone != "" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return t.In(loc).Format("Jan 2 15:04 MST")
		}
	}
	return runTime(t)
}

// waitFlags are the flags shared by the three verbs that wait on a run.
type waitFlags struct {
	noWait  *bool
	follow  *bool
	timeout *time.Duration
}

func registerWaitFlags(fs *flagSet, canNoWait bool) waitFlags {
	var w waitFlags
	if canNoWait {
		w.noWait = fs.Bool("no-wait", false, "start it and return the run id")
	} else {
		no := false
		w.noWait = &no
	}
	w.follow = fs.Bool("follow", false, "stream the run's log while waiting")
	// --logs is what --follow was called before; kept so a script written
	// against it still streams rather than being refused.
	fs.BoolVar(w.follow, "logs", false, "the same as --follow")
	w.timeout = fs.Duration("timeout", 0, "how long to wait")
	return w
}

func jobRunCmd(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job run", jobUsage)
	g.register(fs)
	var params paramList
	fs.Var(&params, "param", "a parameter, k=v, repeatable")
	forFlag := fs.String("for", "", "the instant this run is for")
	idemKey := fs.String("idempotency-key", "", "starts with the same key are one run")
	wf := registerWaitFlags(fs, true)
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	name := operandOr(taken, fs)
	var scheduledFor *time.Time
	if *forFlag != "" {
		at, err := parseRunFor(*forFlag)
		if err != nil {
			return usageCodef("invalid_time", "%v", err)
		}
		scheduledFor = &at
	}

	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	if name == "" {
		picked, err := pickJob(e, s, app, &g)
		if err != nil || picked == "" {
			return orMenu(err, func() error { return jobRunMenu(e, s, app, &g) })
		}
		name = picked
	}

	values := params.values()
	if input(e, &g) {
		if err := promptMissingParams(e, s, app, name, values); err != nil {
			return err
		}
	}

	body := map[string]any{}
	if len(values) > 0 {
		body["params"] = values
	}
	if scheduledFor != nil {
		// The text as typed when it names its own zone: the offset is part
		// of what the job is told (TOFT_SCHEDULED_FOR, an SDK's
		// scheduledForText), so it is the install's to keep, never this
		// client's to normalise (BB-045, fourth 2026-11-03 list). A time with
		// no zone is UTC, written out as such.
		body["scheduled_for"] = scheduledFor.Format(time.RFC3339)
		if _, err := time.Parse(time.RFC3339, *forFlag); err == nil {
			body["scheduled_for"] = *forFlag
		}
	}
	var header http.Header
	if *idemKey != "" {
		header = http.Header{"Idempotency-Key": {*idemKey}}
	}
	var run jobRun
	path := "/api/v1/apps/" + app.ID + "/jobs/" + url.PathEscape(name) + "/runs"
	if err := s.client.DoWithHeaders(http.MethodPost, path, header, body, &run); err != nil {
		return err
	}
	// Under --json the run document says all of this; a sentence before it
	// is one more line a reader of the stream has to skip.
	switch {
	case g.json:
	case run.Deduplicated:
		e.out.Say("A start with the same key is already run %s — this is that run, not a new one.", shortRunID(run.ID))
	case e.out.Rich():
		e.out.Say("%s %s started as %s", e.out.Info("◐"), e.out.Bold(run.Job), e.out.Bold(shortRunID(run.ID)))
	default:
		e.out.Say("Started %s as %s.", run.Job, shortRunID(run.ID))
	}
	return awaitStarted(e, s, app, &g, &run, wf)
}

// pickJob offers the app's declared jobs to a person on every stream, for a
// `toft job run` that named none. Empty when nobody can be asked, nothing is
// declared, or nothing was chosen — the caller then prints the menu it always
// has.
func pickJob(e *env, s *session, app *appSummary, g *globalFlags) (string, error) {
	if !input(e, g) {
		return "", nil
	}
	body, err := fetchJobs(s, app.ID)
	if err != nil {
		return "", err
	}
	var choices []ui.Choice
	for _, d := range body.Jobs {
		detail := ""
		if len(d.ParamsSpec) > 0 {
			detail = strings.Join(sortedParams(d.ParamsSpec), " ")
		}
		choices = append(choices, ui.Choice{Label: d.Name, Detail: detail})
	}
	if len(choices) == 0 {
		return "", nil
	}
	picked, err := e.out.Pick(e.ctx, "Which job?", choices, e.input(), e.interrupt)
	if errors.Is(err, ui.ErrNotAnswered) {
		return "", nil
	}
	return picked.Label, err
}

func orMenu(err error, menu func() error) error {
	if err != nil {
		return err
	}
	return menu()
}

// jobRunMenu is `toft job run` with no job: the jobs this app has, each with
// what its params take, and the command that starts one. Asking is the
// common case of not knowing the name, and a refusal that only said "needs a
// job name" sent the caller to a second command to learn it.
func jobRunMenu(e *env, s *session, app *appSummary, g *globalFlags) error {
	body, err := fetchJobs(s, app.ID)
	if err != nil {
		return err
	}
	holdByLock(body.Jobs, app.locked())
	if g.json {
		return encodeJobs(e, body.Jobs)
	}
	sayJobsVenue(e, body, app)
	if len(body.Jobs) == 0 {
		sayNoJobs(e, app)
		return nil
	}
	for _, d := range body.Jobs {
		e.out.Answer("%s", e.out.Bold(d.Name))
		for _, name := range sortedParams(d.ParamsSpec) {
			if e.out.Rich() {
				e.out.Answer("  %s", paramRich(e, name, d.ParamsSpec[name]))
				continue
			}
			e.out.Answer("  --param %s", paramText(name, d.ParamsSpec[name]))
		}
	}
	first := body.Jobs[0]
	example := "toft job run " + first.Name
	for _, name := range sortedParams(first.ParamsSpec) {
		if p := first.ParamsSpec[name]; p.Required {
			example += " --param " + name + "=" + exampleValue(p)
		}
	}
	e.out.Say("")
	if e.out.Rich() {
		e.out.Next("%s", example)
		return nil
	}
	e.out.Say("Run one: %s", example)
	return nil
}

func exampleValue(p paramSpec) string {
	switch {
	case len(p.Choices) > 0:
		return valueText(p.Choices[0])
	case p.Type == "int":
		return "1"
	case p.Type == "float":
		return "1.0"
	case p.Type == "bool":
		return "true"
	}
	return "..."
}

// promptMissingParams asks for each required parameter the flags did not give,
// using the declaration's own help and choices — only when input(e, g): a
// person on every stream. CI, --yes, TOFT_PLAIN, and any stream piped go to the
// install instead, which refuses with param_required and the command that
// would have worked: the answer a pipe should get. Silent when the job cannot be
// read: the install is the judge of a start, and it will say what is missing.
func promptMissingParams(e *env, s *session, app *appSummary, name string, values map[string]string) error {
	body, err := fetchJobs(s, app.ID)
	if err != nil {
		return nil
	}
	var def *jobDefinition
	for i := range body.Jobs {
		if body.Jobs[i].Name == name {
			def = &body.Jobs[i]
		}
	}
	if def == nil {
		return nil
	}
	for _, pname := range sortedParams(def.ParamsSpec) {
		p := def.ParamsSpec[pname]
		if _, given := values[pname]; given || !p.Required {
			continue
		}
		v, err := askParam(e, name, pname, p)
		if err != nil {
			return err
		}
		values[pname] = v
	}
	return nil
}

func askParam(e *env, job, name string, p paramSpec) (string, error) {
	var choices []string
	for _, c := range p.Choices {
		choices = append(choices, valueText(c))
	}
	v, err := e.out.Param(e.ctx, name, p.Help, orDefault(p.Type, "string"), choices, e.input(), e.interrupt)
	if err != nil {
		// Left unanswered: refuse as the install would, with the flag that
		// answers it.
		return "", &usageError{code: "param_required",
			msg: fmt.Sprintf("this job requires the parameter %q — toft job run %s --param %s=%s",
				name, job, name, exampleValue(p))}
	}
	return v, nil
}

func inChoiceWords(answer string, choices []any) bool {
	for _, c := range choices {
		if valueText(c) == answer {
			return true
		}
	}
	return false
}

// awaitStarted is what run and rerun do once the install has the run: return
// its id under --no-wait, or wait for it and exit on its outcome.
func awaitStarted(e *env, s *session, app *appSummary, g *globalFlags, run *jobRun, wf waitFlags) error {
	if *wf.noWait {
		if g.json {
			return json.NewEncoder(e.out.Stdout).Encode(run)
		}
		e.out.Answer("%s", run.ID)
		e.out.Dim("Wait for it with: toft job wait %s", shortRunID(run.ID))
		return nil
	}
	return waitAndReport(e, s, app, g, run.ID, wf, startDeduplicated(run))
}

// startDeduplicated is the start answer's `deduplicated`, when it carried one.
// It is a fact about the start, not the run, so a re-read of the run does not
// have it: the waited --json document is given it back, always present as the
// start answer has it (BB-008).
func startDeduplicated(run *jobRun) *bool {
	var probe struct {
		Deduplicated *bool `json:"deduplicated"`
	}
	if len(run.raw) > 0 && json.Unmarshal(run.raw, &probe) == nil && probe.Deduplicated != nil {
		return probe.Deduplicated
	}
	if run.Deduplicated {
		return &run.Deduplicated
	}
	return nil
}

// withDeduplicated sets `deduplicated` on a run document as the install sent it.
func withDeduplicated(run *jobRun, dedup bool) {
	run.Deduplicated = dedup
	if len(run.raw) == 0 {
		return
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(run.raw, &doc) != nil {
		return
	}
	doc["deduplicated"], _ = json.Marshal(dedup)
	if b, err := json.Marshal(doc); err == nil {
		run.raw = b
	}
}

// waitAndReport waits for a run and makes its outcome the exit code, in both
// renderings. dedup, when set, is the start answer's `deduplicated`.
func waitAndReport(e *env, s *session, app *appSummary, g *globalFlags, runID string, wf waitFlags, dedup *bool) error {
	final, err := waitForJobRun(e, s, app, runID, *wf.timeout, *wf.follow)
	if g.json {
		if final != nil && final.ID != "" {
			if dedup != nil {
				withDeduplicated(final, *dedup)
			}
			if encodeErr := json.NewEncoder(e.out.Stdout).Encode(final); encodeErr != nil && err == nil {
				err = encodeErr
			}
		}
		if err != nil {
			return err
		}
		// **The run's ending decides the exit code in both renderings.** The
		// `--json` branch once returned before the reporter that makes that
		// decision, so a failed run printed `{"status":"failed"}` and exited
		// 0 — the mode a pipeline uses was the mode that lost the failure.
		//
		// The document is not given an exit field to match. It is the
		// platform's run object and already carries `exit_code`, which is the
		// *job's* — a second number beside it meaning the client's would be
		// two exit codes in one document.
		return jobRunOutcome(final)
	}
	if err != nil {
		return err
	}
	return reportJobRun(e, s, app, final, *wf.follow)
}

// jobRunOutcome is the run's ending as an error, and it is the one place that
// decision is made — the reporter renders it, the JSON path returns it, and
// neither has a copy of the rule.
func jobRunOutcome(run *jobRun) error {
	if _, ok := jobRunTerminal(run.Status); ok {
		return nil
	}
	return &runEndedError{id: shortRunID(run.ID), status: run.Status, hint: nextCommand(run)}
}

// runEndedError is a run that ended other than succeeded. Its status is the
// --json document's `code` — failed, timed_out, out_of_memory, cancelled — so
// a pipeline branches on the ending as it does on every other refusal.
//
// hint is the next step, the reporter's Next line — under --json the refusal
// document's `hint` (BB-021, third 2026-11-03 list).
type runEndedError struct{ id, status, hint string }

func (e *runEndedError) Error() string { return "run " + e.id + " " + e.status }

// jobPollInterval is how often a wait reads the run; zero is the client's
// default. A variable so a test can watch a run move through its states
// without sleeping two seconds a step.
var jobPollInterval time.Duration

// waitForJobRun polls the run to a terminal state, saying where it stands
// whenever that changes — its place in the queue, then its progress.
//
// **Waiting is the default**, and that is the decision that makes the command
// usable in a pipeline. Fire-and-forget is the safer-looking default and the
// wrong one: the overwhelming majority of uses are a step that must not proceed
// until the job is done, and a `0` for a job that later fails is a green
// pipeline over a broken migration.
func waitForJobRun(e *env, s *session, app *appSummary, runID string, timeout time.Duration, follow bool) (*jobRun, error) {
	var last jobRun
	after := 0
	said := ""
	// On a live run the narration is one line repainted in place — the run,
	// where it is, and a clock that moves between polls — with --follow's log
	// above it. Off one, a Dim line each time the words change, as always.
	var mu sync.Mutex
	var line *ui.Region
	line = e.out.NewRegion(e.ctx, func() string {
		mu.Lock()
		defer mu.Unlock()
		if last.ID == "" {
			return e.out.Dimmed(shortRunID(runID) + "  waiting")
		}
		return e.out.Dimmed(shortRunID(last.ID) + "  " + waitingText(&last, time.Now()))
	})
	logOut := func(l jobLogLine) { e.out.Say("  %s", e.logText(l.Line)) }
	if line != nil {
		w := line.Writer()
		logOut = func(l jobLogLine) { fmt.Fprintf(w, "  %s\n", e.logText(l.Line)) }
	}
	pollErr := client.Poll(e.ctx, client.PollOptions{Timeout: timeout, Interval: jobPollInterval}, func(context.Context) (bool, error) {
		got, err := getRun(s, app.ID, runID)
		if err != nil {
			return false, err
		}
		mu.Lock()
		last = *got
		mu.Unlock()
		line.Refresh()
		if follow {
			after = printJobLogTail(s, app, runID, after, logOut)
		}
		terminal, _ := jobRunTerminal(got.Status)
		if !terminal && line == nil {
			if now := waitingText(got, time.Now()); now != said {
				e.out.Dim("%s  %s", shortRunID(got.ID), now)
				said = now
			}
		}
		return terminal, nil
	})
	line.Stop()
	if pollErr != nil {
		if errors.Is(pollErr, client.ErrPollTimeout) {
			id := shortRunID(orDefault(last.ID, runID))
			// wait_timeout and retryable, as the SDKs name it (BB-037).
			return &last, &failedError{code: "wait_timeout", retryable: true,
				msg: fmt.Sprintf("run %s is still %s after %s — it carries on without you; "+
					"`toft job wait %s` picks the wait back up", id, orDefault(last.Status, "unknown"), timeout, id)}
		}
		return &last, noSuchName(pollErr, "toft job runs")
	}
	return &last, nil
}

// waitingText is a run that has not ended, in a few words: `queued · 2nd`, or
// `running · 4m · 40% loading rows`.
func waitingText(r *jobRun, now time.Time) string {
	switch r.Status {
	case jobRunQueued:
		if r.QueuePosition > 0 {
			return "queued · " + ordinal(r.QueuePosition)
		}
		return "queued"
	case jobRunRunning:
		parts := []string{"running"}
		if r.StartedAt != nil {
			parts = append(parts, shortDuration(now.Sub(*r.StartedAt).Truncate(time.Second)))
		}
		progress := ""
		if r.Progress > 0 {
			progress = fmt.Sprintf("%d%%", r.Progress)
		}
		if r.ProgressMessage != "" {
			progress = strings.TrimSpace(progress + " " + plainText(r.ProgressMessage))
		}
		if progress != "" {
			parts = append(parts, progress)
		}
		return strings.Join(parts, " · ")
	}
	return r.Status
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// reportJobRun prints the ending and decides the exit code.
func reportJobRun(e *env, s *session, app *appSummary, run *jobRun, showedLogs bool) error {
	id := shortRunID(run.ID)
	if _, ok := jobRunTerminal(run.Status); ok {
		line := run.Status
		if d := runDuration(run); d != "" {
			line += " in " + d
		}
		if e.out.Rich() {
			e.out.Answer("%s  %s", e.out.Bold(id), runBadge(e, run.Status, line))
			return nil
		}
		e.out.Answer("%s  %s", id, e.out.OK(line))
		return nil
	}

	// The word is on the last line every time, and the reason beside it:
	// `out_of_memory` and `timed_out` send a developer to two different places.
	line := run.Status
	if reason := runReason(run); reason != "" {
		line += " — " + reason
	}
	if e.out.Rich() {
		e.out.Answer("%s  %s", e.out.Bold(id), runBadge(e, run.Status, line))
	} else {
		e.out.Answer("%s  %s", id, e.out.Fail(line))
	}
	// A failed run prints its tail unasked, on the model of a failed deploy:
	// having to re-run the command to see why is the wrong answer to the wrong
	// question.
	if !showedLogs && run.Status != jobRunSkipped {
		printJobLogTail(s, app, run.ID, 0, func(l jobLogLine) { e.out.Say("  %s", e.logText(l.Line)) })
	}
	if e.out.Rich() {
		e.out.Next("%s", nextCommand(run))
	} else {
		e.out.Say("Next: %s", nextCommand(run))
	}
	return jobRunOutcome(run)
}

// runReason is why a run ended other than in success, in words: the install's
// own when it gave them, otherwise the exit code.
func runReason(r *jobRun) string {
	// The reason quotes the app's last stderr line, and no app's bytes drive
	// the terminal (security review 2026-11-14, CS-5).
	if r.FailureReason != "" {
		return plainText(r.FailureReason)
	}
	if r.ExitCode != nil {
		return fmt.Sprintf("exit %d", *r.ExitCode)
	}
	return ""
}

func runDuration(r *jobRun) string {
	if r.StartedAt == nil || r.FinishedAt == nil {
		return ""
	}
	return shortDuration(r.FinishedAt.Sub(*r.StartedAt).Truncate(time.Second))
}

// nextCommand is the one command worth typing after looking at a run.
func nextCommand(r *jobRun) string {
	id := shortRunID(r.ID)
	switch r.Status {
	case jobRunQueued, jobRunRunning:
		return "toft job wait " + id
	case jobRunSucceeded:
		return "toft job logs " + id
	case jobRunSkipped, jobRunCancelled:
		return "toft job rerun " + id
	case jobRunTimedOut:
		return "toft job logs " + id + " shows how far it got; raise timeout under [jobs." + r.Job +
			"] in toft.toml and deploy, or toft job rerun " + id
	case jobRunOutOfMemory:
		// A job's memory is an administrator's, on the app's Automation tab
		// — toft.toml's memory key is removed_key (BB-031, fourth 2026-11-03
		// black-box list).
		return "toft job logs " + id + " shows how far it got; ask an administrator to raise " + r.Job +
			"'s memory on the app's Automation tab, then toft job rerun " + id
	}
	return "toft job logs " + id + ", then toft job rerun " + id
}

// shortRunID is the eight characters every run command accepts.
func shortRunID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func getRun(s *session, appID, runID string) (*jobRun, error) {
	var run jobRun
	path := fmt.Sprintf("/api/v1/apps/%s/job-runs/%s", appID, url.PathEscape(runID))
	if err := s.client.Do(http.MethodGet, path, nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// printJobLogTail writes whatever is new and returns the sequence to ask from
// next. Silent on failure: this is narration, and narration that could fail the
// command it narrates would be worse than none.
func printJobLogTail(s *session, app *appSummary, runID string, after int, print func(jobLogLine)) int {
	lines, _, err := fetchJobLogs(s, app.ID, runID, after)
	if err != nil {
		return after
	}
	for _, l := range lines {
		print(l)
		if l.Seq > after {
			after = l.Seq
		}
	}
	return after
}

func fetchJobLogs(s *session, appID, runID string, after int) ([]jobLogLine, string, error) {
	var body struct {
		Lines  []jobLogLine `json:"lines"`
		Status string       `json:"status"`
	}
	path := client.Query(fmt.Sprintf("/api/v1/apps/%s/job-runs/%s/logs", appID, url.PathEscape(runID)),
		map[string]string{"after": strconv.Itoa(after)})
	if err := s.client.Do(http.MethodGet, path, nil, &body); err != nil {
		return nil, "", err
	}
	return body.Lines, body.Status, nil
}

// jobRerun is `toft job rerun <runID>`: a new run of the same job with the same
// params and scheduled_for, recorded as a rerun of the one named. It waits as
// `toft job run` does.
func jobRerun(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job rerun", jobUsage)
	g.register(fs)
	wf := registerWaitFlags(fs, true)
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	runID := operandOr(taken, fs)
	if runID == "" {
		return usageCodef("missing_argument", "toft job rerun needs a run id — `toft job runs` lists them")
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	var run jobRun
	path := fmt.Sprintf("/api/v1/apps/%s/job-runs/%s/rerun", app.ID, url.PathEscape(runID))
	if err := s.client.Do(http.MethodPost, path, nil, &run); err != nil {
		return noSuchName(err, "toft job runs")
	}
	// A rerun is always a new run; an install that left deduplicated off its
	// answer still gets the field toft job run always carries (L7).
	if startDeduplicated(&run) == nil {
		withDeduplicated(&run, false)
	}
	if !g.json {
		if e.out.Rich() {
			e.out.Say("%s %s started as %s %s", e.out.Info("◐"), e.out.Bold(run.Job), e.out.Bold(shortRunID(run.ID)),
				e.out.Dimmed("· a rerun of "+shortRunID(orDefault(run.RerunOf, runID))))
		} else {
			e.out.Say("Started %s as %s, a rerun of %s.", run.Job, shortRunID(run.ID), shortRunID(orDefault(run.RerunOf, runID)))
		}
	}
	return awaitStarted(e, s, app, &g, &run, wf)
}

// jobWait is `toft job wait <runID>`: a run somebody else started — a
// schedule, the app, an earlier `--no-wait` — waited for as if this command had
// started it, with the same exit code.
func jobWait(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job wait", jobUsage)
	g.register(fs)
	wf := registerWaitFlags(fs, false)
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	runID := operandOr(taken, fs)
	if runID == "" {
		return usageCodef("missing_argument", "toft job wait needs a run id — `toft job runs` lists them")
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	return waitAndReport(e, s, app, &g, runID, wf, nil)
}

func jobRuns(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job runs", jobUsage)
	g.register(fs)
	limit := fs.Int("limit", 0, "how many runs")
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	// Named and below 1 is refused, not sent: the install reads it as "the
	// default", 50, which is not what --limit 0 asked for (L11).
	if flagsSet(fs)["limit"] && *limit < 1 {
		return usageCodef("invalid_limit", "--limit takes a count of 1 or more, not %d", *limit)
	}
	operand := operandOr(taken, fs)
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}

	// One operand, two meanings: a job's name narrows the list, a run's id
	// shows that run. A name is tried first — the app's own vocabulary — and
	// only something that is not one is looked up as a run.
	if operand != "" {
		isJob, err := declaresJob(s, app.ID, operand)
		if err != nil {
			return err
		}
		if !isJob {
			run, err := getRun(s, app.ID, operand)
			var apiErr *client.Error
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				return usageCodef("no_such_job", "this app has no job and no run called %q — "+
					"`toft job list` names its jobs, `toft job runs` its runs", operand)
			}
			if err != nil {
				return err
			}
			if g.json {
				return json.NewEncoder(e.out.Stdout).Encode(run)
			}
			renderRunDetail(e, run)
			return nil
		}
	}

	params := map[string]string{}
	if *limit > 0 {
		params["limit"] = strconv.Itoa(*limit)
	}
	if operand != "" {
		params["job"] = operand
	}
	var body struct {
		Runs []jobRun `json:"runs"`
	}
	path := client.Query("/api/v1/apps/"+app.ID+"/job-runs", params)
	if err := s.client.Do(http.MethodGet, path, nil, &body); err != nil {
		return err
	}
	runs := body.Runs
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].QueuedAt.After(runs[j].QueuedAt) })
	if g.json {
		// The runs as the install sent them, every field — one run per line,
		// NDJSON like every other --json answer (BB-015, third list). No runs
		// is no lines.
		enc := json.NewEncoder(e.out.Stdout)
		for _, r := range runs {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}
	if len(runs) == 0 {
		e.out.Say("No runs yet.")
		return nil
	}
	renderRunRows(e, runs, time.Now())
	return nil
}

// declaresJob reports whether the app declares a job by this name.
func declaresJob(s *session, appID, name string) (bool, error) {
	body, err := fetchJobs(s, appID)
	if err != nil {
		return false, err
	}
	for _, j := range body.Jobs {
		if j.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// renderRunRows is the run list: one row per run, its reason in words, and for
// a run that did not succeed the command that runs it again —
//
//	9b21c0aa  nightly-report  failed  app  Oct 23 02:00 BST  exit 3, connection refused — toft job rerun 9b21c0aa
//
// Widths are measured on the plain words and the colour added after, because
// an escape sequence inside a padded column is invisible width.
func renderRunRows(e *env, runs []jobRun, now time.Time) {
	if e.out.Rich() {
		renderRunTable(e, runs, now)
		return
	}
	type row struct{ id, job, state, by, when, tail, status string }
	rows := make([]row, 0, len(runs))
	wJob, wState, wBy := 0, 0, 0
	for i := range runs {
		r := &runs[i]
		when := r.QueuedAt
		if r.StartedAt != nil {
			when = *r.StartedAt
		}
		rw := row{id: shortRunID(r.ID), job: r.Job, by: r.StartedBy, status: r.Status, when: runTime(when)}
		if terminal, _ := jobRunTerminal(r.Status); terminal {
			rw.state = r.Status
		} else {
			rw.state = waitingText(r, now)
		}
		switch r.Status {
		case jobRunSucceeded:
			rw.tail = runDuration(r)
		case jobRunFailed, jobRunTimedOut, jobRunOutOfMemory:
			rw.tail = strings.TrimSpace(runReason(r) + " — toft job rerun " + rw.id)
			rw.tail = strings.TrimPrefix(rw.tail, "— ")
		case jobRunCancelled, jobRunSkipped:
			rw.tail = runReason(r)
		}
		wJob, wState, wBy = max(wJob, len(rw.job)), max(wState, len([]rune(rw.state))), max(wBy, len(rw.by))
		rows = append(rows, rw)
	}
	for _, rw := range rows {
		state := rw.state + strings.Repeat(" ", wState-len([]rune(rw.state)))
		line := fmt.Sprintf("%s  %-*s  %s  %-*s  %s", rw.id, wJob, rw.job, jobStatusWord(e, rw.status, state),
			wBy, rw.by, e.out.Dimmed(rw.when))
		if rw.tail != "" {
			line += "  " + rw.tail
		}
		e.out.Answer("%s", strings.TrimRight(line, " "))
	}
}

// renderRunTable is the run list drawn for a person: a header, a badge for
// each run's state, when it started as a distance, and the result as the
// column that gives way to a narrow terminal. A running run's progress moves
// into that column, so the status stays short; and the rerun command, which
// the plain rows repeat on every failure, is said once under the table for the
// newest one — beside each row it pushed the reason itself off the edge.
func renderRunTable(e *env, runs []jobRun, now time.Time) {
	t := e.out.NewTable("run", "job", "status", "by", "started", "result").Flex(5)
	var newestFailure string
	for i := range runs {
		r := &runs[i]
		when := r.QueuedAt
		if r.StartedAt != nil {
			when = *r.StartedAt
		}
		id := shortRunID(r.ID)
		state, result := r.Status, ""
		switch r.Status {
		case jobRunQueued:
			state = waitingText(r, now)
		case jobRunRunning:
			state = "running"
			if r.StartedAt != nil {
				state += " · " + shortDuration(now.Sub(*r.StartedAt).Truncate(time.Second))
			}
			progress := ""
			if r.Progress > 0 {
				progress = fmt.Sprintf("%d%%", r.Progress)
			}
			result = e.out.Dimmed(strings.TrimSpace(progress + " " + plainText(r.ProgressMessage)))
		case jobRunSucceeded:
			result = e.out.Dimmed(runDuration(r))
		case jobRunFailed, jobRunTimedOut, jobRunOutOfMemory:
			result = runReason(r)
			if newestFailure == "" {
				newestFailure = id
			}
		case jobRunCancelled, jobRunSkipped:
			result = e.out.Dimmed(runReason(r))
		}
		t.Row(e.out.Bold(id), r.Job, runBadge(e, r.Status, state), e.out.Dimmed(r.StartedBy),
			e.out.Dimmed(ui.Ago(when)), result)
	}
	t.Print()
	if newestFailure != "" {
		e.out.Say("")
		e.out.Next("toft job runs %s  %s", newestFailure,
			e.out.Dimmed("· why it failed — then toft job rerun "+newestFailure))
	}
}

// jobStatusWord colours text by the status it stands for.
func jobStatusWord(e *env, status, text string) string {
	terminal, healthy := jobRunTerminal(status)
	switch {
	case healthy:
		return e.out.OK(text)
	case terminal:
		return e.out.Fail(text)
	}
	return text
}

// renderRunDetail is one run in full — what was asked of it, how it ended and
// why, what it used — and the command to try next.
func renderRunDetail(e *env, r *jobRun) {
	id := shortRunID(r.ID)
	state := r.Status
	if terminal, _ := jobRunTerminal(r.Status); !terminal {
		state = waitingText(r, time.Now())
	}
	if e.out.Rich() {
		e.out.Answer("%s  %s  %s", e.out.Bold(id), e.out.Bold(r.Job), runBadge(e, r.Status, state))
	} else {
		e.out.Answer("%s  %s  %s", id, r.Job, jobStatusWord(e, r.Status, state))
	}
	// Thirteen wide with no gutter is the plain `%-12s %s` exactly.
	field := func(label, value string) {
		if value != "" {
			e.out.Field(13, label, value)
		}
	}
	// at is an instant, with its distance beside it on a terminal.
	at := func(t time.Time) string {
		if e.out.Rich() {
			return runTime(t) + e.out.Dimmed(" · "+ui.Ago(t))
		}
		return runTime(t)
	}
	by := r.StartedBy
	if r.StartedByUser != "" {
		by += " (" + r.StartedByUser + ")"
	}
	field("started by", by)
	if r.RerunOf != "" {
		field("rerun of", shortRunID(r.RerunOf))
	}
	if r.ScheduledFor != nil {
		field("for", at(*r.ScheduledFor))
	}
	field("queued", at(r.QueuedAt))
	if r.StartedAt != nil {
		field("started", at(*r.StartedAt))
	}
	if r.FinishedAt != nil {
		finished := at(*r.FinishedAt)
		if d := runDuration(r); d != "" {
			finished += " (" + d + ")"
		}
		field("finished", finished)
	}
	field("params", paramsText(r.Params))
	if r.ExitCode != nil {
		field("exit code", strconv.Itoa(*r.ExitCode))
	}
	if r.Status != jobRunSucceeded {
		reason := plainText(r.FailureReason)
		if e.out.Rich() && reason != "" {
			reason = e.out.Fail(reason)
		}
		field("reason", reason)
	}
	if r.PeakMemoryMB != nil {
		field("peak memory", mbText(*r.PeakMemoryMB))
	}
	next := nextCommand(r)
	if e.out.Rich() {
		next = e.out.Bold(next)
	}
	field("next", next)
}

// paramsText is a run's params as the flags that would send them again.
func paramsText(raw json.RawMessage) string {
	var values map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return ""
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = plainText(k + "=" + valueText(values[k]))
	}
	return strings.Join(parts, " ")
}

func jobLogs(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job logs", jobUsage)
	g.register(fs)
	follow := fs.Bool("follow", false, "keep reading")
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	runID := operandOr(taken, fs)
	if runID == "" {
		return usageCodef("missing_argument", "toft job logs needs a run id — `toft job runs` lists them")
	}

	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}

	// The route answers one page — a thousand lines from ?after — and says
	// nothing about what follows, so both forms read pages until an empty one:
	// a run that printed five thousand lines and failed used to show its first
	// thousand and exit 0, cutting off exactly the end where the failure was
	// (code review 2026-11-11, H-1).
	after := 0
	drain := func() (string, error) {
		var status string
		for {
			lines, st, err := fetchJobLogs(s, app.ID, runID, after)
			if err != nil {
				return "", noSuchName(err, "toft job runs")
			}
			status = st
			for _, l := range lines {
				if err := writeJobLogLine(e, g.json, l); err != nil {
					return "", err
				}
				if l.Seq > after {
					after = l.Seq
				}
			}
			if len(lines) == 0 {
				return status, nil
			}
		}
	}
	if !*follow {
		_, err := drain()
		return err
	}

	// Following polls with ?after=<seq>, which the handler already takes. ADR
	// §42 took the polling trade for log following and nothing here changes it.
	// Every poll drains to an empty page, so the terminal status is only acted
	// on once the last page has been printed.
	return client.Poll(e.ctx, client.PollOptions{Interval: jobPollInterval}, func(context.Context) (bool, error) {
		status, err := drain()
		if err != nil {
			return false, err
		}
		terminal, _ := jobRunTerminal(status)
		return terminal, nil
	})
}

// writeJobLogLine is one line of a run's log: under --json one document per
// line, NDJSON as `toft logs --json` is (BB-015, third list), --follow or not.
func writeJobLogLine(e *env, asJSON bool, l jobLogLine) error {
	if asJSON {
		l.Line, l.Raw = jsonLogText(l.Line)
		return json.NewEncoder(e.out.Stdout).Encode(l)
	}
	e.out.Answer("%s", e.logText(l.Line))
	return nil
}

// jobOutput is a run's output object, as the bytes the job wrote — the SDKs'
// run.output(). The install answers with a presigned URL; the bytes are read
// from there without the credential, which never travels to the store. Text or
// JSON alike: `toft api` on the same route could only show JSON.
func jobOutput(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job output", jobUsage)
	g.register(fs)
	dest := fs.String("o", "-", "where to write it; - is stdout")
	force := fs.Bool("force", false, "replace a file already at -o")
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	runID := operandOr(taken, fs)
	if runID == "" {
		return usageCodef("missing_argument", "toft job output needs a run id — `toft job runs` lists them")
	}
	if *dest != "-" {
		if err := refuseClobber(*dest, *force); err != nil {
			return err
		}
	}

	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	where, err := s.client.Locate(fmt.Sprintf("/api/v1/apps/%s/job-runs/%s/output", app.ID, url.PathEscape(runID)))
	if err != nil {
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.Code != "" && apiErr.Code != "not_found" {
			return err // no_output, output_expired, no_object_store: the run exists
		}
		return noSuchName(err, "toft job runs")
	}
	body, _, err := fetch(e, &signedRequest{Method: http.MethodGet, URL: where})
	if err != nil {
		return err
	}
	defer body.Close()
	if *dest == "-" {
		_, err = io.Copy(e.out.Stdout, body)
		return err
	}
	n, err := writeBeside(*dest, body)
	if err != nil {
		return err
	}
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{"run": runID, "path": *dest, "bytes": n})
	}
	e.out.Answer("%s  %d bytes", *dest, n)
	return nil
}

func jobCancel(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("job cancel", jobUsage)
	g.register(fs)
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	runID := operandOr(taken, fs)
	if runID == "" {
		return usageCodef("missing_argument", "toft job cancel needs a run id — `toft job runs` lists them")
	}

	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	// The install answers with the run as it now stands and whether this call
	// changed it. One older than that answers 204 and nothing, which is read
	// as the cancel it always reported.
	var raw json.RawMessage
	path := fmt.Sprintf("/api/v1/apps/%s/job-runs/%s/cancel", app.ID, url.PathEscape(runID))
	if err := s.client.Do(http.MethodPost, path, nil, &raw); err != nil {
		return noSuchName(err, "toft job runs")
	}
	var answer struct {
		Status  string `json:"status"`
		Changed *bool  `json:"changed"`
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		json.Unmarshal(raw, &answer)
		json.Unmarshal(raw, &doc)
	}
	changed := answer.Changed == nil || *answer.Changed
	if g.json {
		doc["changed"] = changed
		return json.NewEncoder(e.out.Stdout).Encode(doc)
	}
	if !changed {
		// Success, not a refusal: the run is not going, which is what was
		// asked for. It is said, rather than "cancelled" over a run that
		// succeeded.
		e.out.Answer("%s  already finished (%s)", runID, answer.Status)
		return nil
	}
	e.out.Answer("%s  %s", runID, orDefault(answer.Status, "cancelled"))
	return nil
}

// jobSession is the four lines every subcommand starts with.
func jobSession(e *env, g *globalFlags) (*session, *appSummary, error) {
	s, err := e.setup(g)
	if err != nil {
		return nil, nil, err
	}
	appValue, err := pickApp(e, s)
	if err != nil {
		return nil, nil, err
	}
	app, err := s.resolveApp(appValue.Value)
	if err != nil {
		return nil, nil, err
	}
	return s, app, nil
}

// takeOperand pulls a leading non-flag argument off the front.
//
// Go's `flag` stops parsing at the first non-flag argument, so `toft job run
// nightly --param rows=500` — the word order everybody types — would parse
// `nightly` and then silently treat every flag after it as a positional. Taking
// the operand first means both orders work and neither loses a flag. The same
// reason `toft store` dispatches its own subcommands before parsing anything.
func takeOperand(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// operandOr falls back to a positional left after parsing, so `toft job run
// --app demo nightly` works too.
func operandOr(taken string, fs *flagSet) string {
	if taken != "" {
		return taken
	}
	if fs.NArg() > 0 {
		return fs.Arg(0)
	}
	return ""
}

// paramList collects repeated --param k=v.
//
// **Values are sent as strings and never interpolated.** The install holds
// each to the job's declared type — "5" for an int parameter arrives as 5 — so
// the client has no typing of its own to get wrong. There is no --params-json
// and no shell-expanded form, because the invariant is that parameters reach
// the workload as environment and are never interpolated into a command
// string, and the way a client keeps an invariant it does not itself enforce
// is by having no syntax that suggests otherwise.
type paramList []string

func (p *paramList) String() string { return strings.Join(*p, ",") }

func (p *paramList) refusalCode() string { return "invalid_param" }

func (p *paramList) Set(v string) error {
	k, _, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("--param %q needs a k=v pair", v)
	}
	// An empty name would reach the job as a parameter nothing can declare —
	// the same typo as a missing =, so the same refusal.
	if strings.TrimSpace(k) == "" {
		return fmt.Errorf("--param %q has no name before the = — write name=value", v)
	}
	*p = append(*p, v)
	return nil
}

// values splits on the *first* = only: everything after it is the value,
// verbatim, including any further = signs.
func (p paramList) values() map[string]string {
	out := map[string]string{}
	for _, kv := range p {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// parseRunFor reads --for: a full RFC 3339 instant, or a date or date-time with
// no offset, which is read as UTC — said in the usage, because a time that
// silently meant this machine's zone would name a different instant on the
// next person's laptop. A minute-precision time may carry its offset too
// (2026-10-06T03:00-05:00): the zoneless minute form was accepted, its zoned
// twin refused, and the refusal's own example has seconds.
func parseRunFor(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if at, err := time.Parse(layout, v); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("--for %q is not a time — use 2026-10-06T03:00:00+01:00, or 2026-10-06T03:00 for UTC", v)
}

// expectWithinText writes a window the way it is declared: whole days as "7d",
// otherwise hours.
func expectWithinText(sec int) string {
	if sec >= 2*86400 && sec%86400 == 0 {
		return fmt.Sprintf("%dd", sec/86400)
	}
	if sec%3600 == 0 {
		return fmt.Sprintf("%dh", sec/3600)
	}
	return fmt.Sprintf("%dm", sec/60)
}
