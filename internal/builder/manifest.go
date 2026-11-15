package builder

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"toft/internal/database"
	"toft/internal/jobs"
	"toft/internal/schedule"
	"toft/internal/toolchain"
)

// toft.toml — the repo's own declaration of its background jobs.
//
// The manifest is one of two doors onto a job, and the one worth taking: a job
// declared here is part of the application rather than part of its deployment,
// so it lives and versions with the code it runs, and a rollback takes the
// command back with it. The other door is the Automation tab, which creates a
// `ui`-sourced row — the same row, minus that property. Neither reaches into
// the other: this file owns exactly what it declares, and `ReconcileManifestJobs`
// leaves everything else alone.
//
//	[jobs.nightly-report]
//	command  = "python -m reports.nightly"
//	schedule = "0 3 * * *"
//	timezone = "Europe/London"
//	timeout  = 1800
//	memory   = 1024
//	retries  = 2
//	expect_within = "26h"
//	viewer_runnable = true
//
//	[jobs.nightly-report.params.region]
//	type     = "string"
//	required = true
//
// The same file also carries the `[static]` table, which is read by
// internal/staticserve rather than here — the static workload is started from a
// binary that must not import the builder, and detection reads the table too so
// that a wake, a boot and a build cannot disagree about an app's front page.

// ManifestFile is the file the builder looks for at the root of a release.
const ManifestFile = "toft.toml"

// manifest is the shape this package reads out of toft.toml.
//
// **A top-level table this struct does not name is ignored, and that is a
// contract rather than an accident of the decoder.** The file is shared: it
// already carries `[static]`, which internal/staticserve reads and this package
// must not care about, and the CLI reads `[app]` out of it to work out which app
// a directory deploys to. A parser that refused what it did not recognise would
// mean the first developer to name their app in the file broke their own deploy
// with a manifest error, from a change that added nothing this package reads.
// TestUnknownTablesAreIgnored holds the property.
//
// What is *not* ignored is a malformed `[jobs]` table — a declaration the
// platform half understood is a job that silently does not exist, which is the
// failure declaring it was supposed to prevent.
type manifest struct {
	Jobs map[string]manifestJob `toml:"jobs"`
	// Batch is read by detection (batch.go); it is named here so a typo inside
	// it is reported rather than ignored.
	Batch *struct {
		Language string `toml:"language"`
	} `toml:"batch"`
	// Env is checked here and seeded at configure (envdecl.go). Decoded as
	// any so that every key under it is this package's to refuse.
	Env any `toml:"env"`
	// Restart and Render are the app's one-per-app schedules
	// (manifest_schedules.go).
	Restart *manifestSchedule `toml:"restart"`
	Render  *manifestSchedule `toml:"render"`
	// Storage is `[[storage]]` (manifest_storage.go).
	Storage []manifestStorage `toml:"storage"`
	// Runtime is read by ParseRuntimePins; named here so a key under it that
	// no reader knows — `ruby = "3"` — is reported, not ignored (BB-009 of
	// the second 2026-11-03 bash).
	Runtime *manifestRuntime `toml:"runtime"`
	// Notebook is read by readNotebookDeclaration (notebook.go); named here
	// so the table the refusals themselves tell an author to write is not
	// reported as read by nobody, and a typo inside it is.
	Notebook *struct {
		File   string `toml:"file"`
		Engine string `toml:"engine"`
	} `toml:"notebook"`
}

type manifestJob struct {
	Command string `toml:"command"`
	// Args is the argv of the build's single binary — a Go app's — instead of
	// a shell command line. Exactly one of the two.
	Args     []string `toml:"args"`
	Schedule string   `toml:"schedule"`
	// Timezone is the IANA zone the schedule is read in; absent is UTC. Stored
	// joined to the expression (schedule.Join), never as a column of its own.
	Timezone string `toml:"timezone"`
	// ExpectWithin is how long the job may go without a success before it is
	// reported overdue: "26h", "90m", "7d". ADR §105.
	ExpectWithin string `toml:"expect_within"`
	// Timeout is a duration ("45m", "1h30m") or an integer number of seconds.
	Timeout any `toml:"timeout"`
	// Memory and CPU are read only to be refused: a job's limits are an
	// administrator's (ADR §128), and a key the platform silently dropped
	// would be a number its author believes. Any value, any type, refused.
	Memory any `toml:"memory"`
	CPU    any `toml:"cpu"`
	// Overlap is what a scheduled firing does while the last run is still
	// going: "queue" (the default) waits — the claim holds it behind the
	// job's open run — and "skip" records a skipped run.
	Overlap string `toml:"overlap"`
	// Notify is who hears about a run's outcome: "none" (the default),
	// "failures" or "always".
	Notify string                    `toml:"notify"`
	Params map[string]manifestParam  `toml:"params"`
	Extra  map[string]toml.Primitive `toml:"-"`

	// Keys that were once read and are refused now, each with what replaced
	// it. Decoded rather than left undecoded so the refusal can name the
	// replacement instead of reporting an unknown key.
	Retries        any `toml:"retries"`
	ViewerRunnable any `toml:"viewer_runnable"`
	Enabled        any `toml:"enabled"`
}

type manifestParam struct {
	Type     string `toml:"type" json:"type"`
	Required bool   `toml:"required" json:"required"`
	Default  any    `toml:"default" json:"default,omitempty"`
	Help     string `toml:"help" json:"help,omitempty"`
	Choices  []any  `toml:"choices" json:"choices,omitempty"`
}

// manifestParamKeys are the keys a parameter's table may carry. Anything else
// is reported, like any other key nothing reads.
var manifestParamKeys = map[string]bool{"type": true, "required": true, "default": true, "help": true, "choices": true}

// ManifestError names the field that is wrong.
//
// A field-level message rather than "invalid manifest": the developer is
// looking at a file they just edited, and telling them *which* key and *why*
// is the difference between a fix and a guess.
type ManifestError struct {
	Job string
	// Table names a top-level table other than a job, "restart" or "render".
	Table string
	Field string
	Msg   string
	// Code and Hint, when set, are what the refusal carries to a program and
	// the fix it can paste. removed_key is the one code here today: a key that
	// was once read and has a replacement.
	Code string
	Hint string
}

// ManifestRemovedKey is the code of a refusal for a key Toft no longer reads.
const ManifestRemovedKey = "removed_key"

// CodeAndHint is the refusal's machine code and pasteable fix — its own when
// it set them, otherwise the ones its field implies. A check problem with no
// code is one a program cannot branch on, and most manifest refusals had
// none (BB-038): a bad timeout, cron, timezone, overlap, notify or job name
// all read only as prose.
func (e ManifestError) CodeAndHint() (string, string) {
	code, hint := e.Code, e.Hint
	if code != "" && hint != "" {
		return code, hint
	}
	field := e.Field
	if i := strings.IndexAny(field, ". "); i >= 0 && !strings.HasPrefix(field, "[") {
		field = field[:i] // params.region.type → params
	}
	var c, h string
	switch {
	case strings.HasPrefix(e.Msg, manifestWrongType) && strings.Contains(e.Msg, "must be a true or false"):
		c, h = "manifest_syntax", "write true or false, unquoted, for the key the message names"
	case strings.HasPrefix(e.Msg, manifestWrongType):
		c, h = "manifest_syntax", "write the value in the type the message names: text in quotes, a number or true/false without"
	case strings.HasPrefix(e.Msg, manifestUnparsed):
		c, h = "manifest_syntax", "fix the TOML at the line named; a value that is text needs quotes"
	case e.Job != "" && field == "" && strings.HasPrefix(e.Msg, "a job name"):
		c, h = "invalid_job_name", "rename the table to lowercase letters, digits and inner dashes, like [jobs.nightly-report]"
	case strings.HasPrefix(field, "[runtime]"):
		c, h = "invalid_runtime", runtimeHint(strings.TrimSpace(strings.TrimPrefix(field, "[runtime]")), e.Msg)
	case field == "timeout":
		c, h = "invalid_timeout", `write a duration between 1s and 7d, like timeout = "45m"`
	case field == "schedule":
		c, h = "invalid_schedule", `write a five-field cron expression, like schedule = "0 3 * * *"`
	case field == "timezone":
		c, h = "invalid_timezone", `write an IANA zone name, like timezone = "Europe/London"`
	case field == "overlap":
		c, h = "invalid_overlap", `write overlap = "skip" or overlap = "queue"`
	case field == "notify":
		c, h = "invalid_notify", `write notify = "none", "failures" or "always"`
	case field == "expect_within":
		c, h = "invalid_expect_within", `write a window between 1h and 31d, like expect_within = "26h"`
	case field == "params":
		c, h = "invalid_param", "each parameter takes type (string, int, float, bool), and its default and choices must be of that type"
	case field == "command" || field == "args":
		c, h = "invalid_job_command", "give the job exactly one of command = \"...\" or args = [...]"
	case strings.Contains(e.Msg, "[batch] and [static]"):
		c, h = "batch_and_static", "keep one: [batch] for an app that only runs jobs, [static] for a folder of files"
	case strings.Contains(e.Msg, "a static app runs no commands"):
		c, h = "manifest_invalid", "remove the [jobs] tables, or deploy the folder as an app in a language Toft supports"
	default:
		c, h = "manifest_invalid", codeHint(code)
		if h == "" {
			h = "fix the " + ManifestFile + " entry the message names"
		}
	}
	if code == "" {
		code = c
	}
	if hint == "" {
		hint = h
	}
	return code, hint
}

// FieldPath is the key the refusal names as one dotted path, or "" when it
// names none: storage[0].name as written, jobs.<name>.<key> for a job's,
// <table>.<key> for a section's, and runtime.<lang> for a [runtime] pin.
func (e ManifestError) FieldPath() string {
	field := e.Field
	if rest, ok := strings.CutPrefix(field, "[runtime]"); ok {
		if rest = strings.TrimSpace(rest); rest == "" {
			return "runtime"
		}
		return "runtime." + rest
	}
	if strings.HasPrefix(field, "[") {
		return ""
	}
	var prefix string
	switch {
	case e.Job != "":
		prefix = "jobs." + e.Job
	case e.Table != "":
		prefix = e.Table
	}
	switch {
	case prefix == "":
		return field
	case field == "":
		return prefix
	}
	return prefix + "." + field
}

func (e ManifestError) Error() string {
	where := ManifestFile
	if e.Job != "" {
		where += " [jobs." + e.Job + "]"
	}
	if e.Table != "" {
		where += " [" + e.Table + "]"
	}
	if e.Field != "" {
		where += " " + e.Field
	}
	// The fix travels with the refusal: a deploy's failure carries only this
	// sentence, and a refusal that names the problem without the fix sends
	// its reader to the docs for something the platform already knew.
	if e.Hint != "" {
		return where + ": " + e.Msg + " — " + e.Hint
	}
	return where + ": " + e.Msg
}

// ManifestErrors is every problem a manifest has, in the file's order. Its
// message is the first one's and errors.As finds the first ManifestError, so
// a deploy's refusal keeps the shape it had; `toft check` lists each
// (manifestProblems).
type ManifestErrors []error

func (e ManifestErrors) Error() string   { return e[0].Error() }
func (e ManifestErrors) Unwrap() []error { return e }

// joinManifestErrors is nil, the one error, or ManifestErrors.
func joinManifestErrors(errs []error) error {
	var flat []error
	for _, err := range errs {
		flat = append(flat, manifestProblems(err)...)
	}
	switch len(flat) {
	case 0:
		return nil
	case 1:
		return flat[0]
	}
	return ManifestErrors(flat)
}

// manifestProblems is each problem err holds.
func manifestProblems(err error) []error {
	if err == nil {
		return nil
	}
	if me, ok := err.(ManifestErrors); ok {
		return me
	}
	return []error{err}
}

// A job's timeout: the range a declaration may name. A job that declares no
// timeout takes the install's default at dispatch.
const (
	// The outer bound a declaration is held to. The install's own ceiling,
	// max_job_timeout_seconds, is at most this and is checked at check and
	// deploy, where the settings are in hand.
	JobTimeoutMinSec = 1
	JobTimeoutMaxSec = 7 * 24 * 3600
)

// JobNameOK keeps a name usable as a stable key, a URL segment, and a
// Kubernetes object name component. Exported for the same reason as the bounds
// above: both doors onto a job definition apply it.
func JobNameOK(name string) bool {
	if name == "" || len(name) > 60 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(name)-1:
		default:
			return false
		}
	}
	return true
}

// ParseManifest reads a release's toft.toml into job definitions.
//
// A missing file is not an error — most apps declare no jobs — but a malformed
// one is, and it fails the deploy. Accepting a manifest the platform half
// understood would mean a developer's job silently not existing, which is
// exactly the failure a declaration is supposed to prevent.
func ParseManifest(releaseDir string) ([]database.JobDefinition, error) {
	defs, _, err := ParseManifestWithWarnings(releaseDir)
	return defs, err
}

// manifestReadBy names the tables something in this product reads, so that
// everything else can be reported as read by nobody.
//
// `jobs` is this package's. `static` is internal/staticserve's, `app` is the
// CLI's, and `runtime` is this package's too — ParseRuntimePins, a few hundred
// lines down. None may be warned about here: the file is shared, which is the
// whole reason an unknown table is ignored rather than refused.
//
// **`runtime` was missing from this list while being read**, so a build that
// honoured `node = "24"` said in the same log that the table was ignored. The
// warning also travels in `deploy.Warnings`, which is what a `--json` caller
// gets *instead of* the build log — so the false half reached the one audience
// that could not see the line contradicting it. What the sentence implies is
// deleting the pin, and deleting the pin is the single change that silently
// moves an app onto the install default.
//
// A list that names readers in another package cannot be derived from this one,
// so what holds it honest is the test beside it: every table here is one a
// manifest can carry without a warning, and `runtime` is asserted against the
// reader that actually reads it.
var manifestReadBy = map[string]bool{"jobs": true, "static": true, "app": true, "runtime": true, "batch": true, "env": true,
	"restart": true, "render": true, "storage": true, "notebook": true,
	// Read only to be refused: scheduled tasks are gone, and a [tasks] table
	// is a schedule its author believes exists.
	"tasks": true}

// ParseManifestWithWarnings is ParseManifest, and also what the file says that
// nothing reads.
//
// **Ignoring an unknown table stays the contract; being silent about it does
// not.** A table nothing reads is skipped, the deploy succeeds and reports
// nothing, and whatever the developer just declared does not exist. There is no signal to recover from: the only way to find out is to
// independently think to go and look. Refusing instead would break the sharing
// this file depends on — the first person to add `[app]` would fail their own
// deploy from a change that added nothing this package reads — so the answer is
// a line in the build log, which costs a reader nothing and is the difference
// between a quiet wrong outcome and a stated one.
//
// Keys *inside* a job are reported the same way and for the same reason: a
// `memry = 2048` is a memory limit that silently stayed at the default.
func ParseManifestWithWarnings(releaseDir string) ([]database.JobDefinition, []string, error) {
	d, warnings, err := ParseManifestAll(releaseDir)
	return d.Jobs, warnings, err
}

// ParseManifestAll is ParseManifestWithWarnings with the two one-per-app
// schedules beside the jobs — everything the deploy records at the flip.
func ParseManifestAll(releaseDir string) (ManifestDecl, []string, error) {
	return parseManifestAll(releaseDir, false)
}

// parseManifestAll is ParseManifestAll with one switch: tolerateRemoved reads
// a key this platform once read and has since removed — `retries`, a job's
// `memory`, a schedule's `enabled`, `[tasks]` — as an unread key, reported in
// warnings, rather than as a refusal. For a rollback (E-8); a deploy refuses.
func parseManifestAll(releaseDir string, tolerateRemoved bool) (ManifestDecl, []string, error) {
	var d ManifestDecl
	data, err := readInRelease(releaseDir, ManifestFile)
	if os.IsNotExist(err) {
		return d, nil, nil
	}
	if err != nil {
		return d, nil, fmt.Errorf("reading %s: %w", ManifestFile, err)
	}

	var m manifest
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return d, nil, tomlDecodeError(string(data), err)
	}
	warnings := unreadKeys(md)
	// Every section's problems, not the first section's (BB-010): `toft
	// check` lists them all, and the deploy refuses on the first. [tasks] is
	// one of them, listed first — and no longer the only one, which hid every
	// [jobs] problem behind it (L1, 2026-11-04 black-box list).
	var errs []error
	if md.IsDefined("tasks") && tolerateRemoved {
		warnings = append(warnings, "tasks")
	} else if md.IsDefined("tasks") {
		// Refused rather than warned about: a [tasks] table is a schedule its
		// author believes will fire, and a warning in a build log is how that
		// belief survives until the night it does not.
		example := "digest"
		for _, k := range md.Keys() {
			if len(k) >= 2 && k[0] == "tasks" {
				example = k[1]
				break
			}
		}
		errs = append(errs, ManifestError{Code: ManifestRemovedKey,
			Msg:  "declares [tasks], and scheduled tasks are gone: scheduled work is a job",
			Hint: fmt.Sprintf("replace [tasks.%s] with [jobs.%s], a command and a schedule", example, example)})
	}
	if md.IsDefined("batch") && md.IsDefined("static") {
		// Two answers to what this release is: one serves files and runs
		// nothing, the other serves nothing and runs commands — and a static
		// release has no shell to run one in.
		errs = append(errs, ManifestError{Msg: "declares both [batch] and [static]. A batch app " +
			"serves nothing and runs commands; a static app serves files and has no runtime to run " +
			"a command in. Keep the one this release is"})
	}
	if _, err := envDeclsFrom(m.Env); err != nil {
		errs = append(errs, err)
	}
	if d.Restart, err = parseManifestSchedule("restart", m.Restart, tolerateRemoved, &warnings); err != nil {
		errs = append(errs, err)
	}
	if d.Render, err = parseManifestSchedule("render", m.Render, tolerateRemoved, &warnings); err != nil {
		errs = append(errs, err)
	}
	if md.IsDefined("storage") {
		d.StorageDeclared = true
		if d.Storage, err = parseManifestStorage(m.Storage); err != nil {
			errs = append(errs, err)
		}
	}

	names := make([]string, 0, len(m.Jobs))
	for name := range m.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]database.JobDefinition, 0, len(m.Jobs))
	for _, name := range names {
		def, err := manifestJobToDefinition(name, m.Jobs[name], tolerateRemoved, &warnings)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		defs = append(defs, *def)
	}
	// The jobs that parsed are returned beside the error, so a check can hold
	// them to the rest of the tree — [static] beside [jobs] was hidden behind
	// an [env] problem (BB-065). A deploy refuses on the error either way.
	if len(defs) > 0 {
		d.Jobs = defs
	}
	if err := joinManifestErrors(errs); err != nil {
		return d, warnings, err
	}
	return d, warnings, nil
}

// unreadKeys is what the file carries that nothing in this product reads, as
// the shortest name that identifies it: a top-level table by its own name, and
// anything under `[jobs]` by its full dotted path, because "jobs" alone would
// point at the one table that *is* read.
//
// Sorted, because this ends up in a build log somebody compares between two
// deploys, and map order would make an unchanged manifest look changed.
func unreadKeys(md toml.MetaData) []string {
	seen := map[string]bool{}
	for _, key := range md.Undecoded() {
		if len(key) == 0 {
			continue
		}
		top := key[0]
		if !manifestReadBy[top] {
			seen[top] = true
			continue
		}
		// A key under `[restart]` or `[render]` that is not one of theirs: a
		// `timezon = ...` is a schedule silently read in UTC.
		if (top == "restart" || top == "render" || top == "storage" || top == "runtime" || top == "batch" || top == "notebook") && len(key) >= 2 {
			seen[key.String()] = true
			continue
		}
		// `[app]` is the CLI's (cli/internal/config AppTable), and its keys are
		// a closed set: `colour = "red"` there was the one unknown key in the
		// file nothing reported. An install's URL under `installs` is the
		// author's; the key inside it is the CLI's.
		if top == "app" && len(key) >= 2 {
			switch {
			// `name` is read by `toft create` alone, as the new app's display
			// name when --name is not passed.
			case key[1] == "slug" || key[1] == "install" || key[1] == "name":
			case key[1] == "installs":
				if len(key) >= 4 && key[3] != "slug" {
					seen[key.String()] = true
				}
			default:
				seen[key.String()] = true
			}
			continue
		}
		// A key under a table that is read. A job's parameter *names* are the
		// author's, so `params.<name>` is never reported — but the keys inside a
		// parameter are this package's, and a `choises = [...]` is a closed set
		// that silently stayed open.
		if top != "jobs" || len(key) <= 2 {
			continue
		}
		if i := slices.Index(key, "params"); i >= 0 {
			if len(key) > i+2 && !manifestParamKeys[key[i+2]] {
				seen[key.String()] = true
			}
			continue
		}
		seen[key.String()] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func manifestJobToDefinition(name string, job manifestJob, tolerateRemoved bool, warnings *[]string) (*database.JobDefinition, error) {
	// Every problem in the table, not the first: `toft check` lists them all,
	// and a deploy refuses on the first (ManifestErrors) — fixing one key at a
	// time against a check that hid the next was BB-010 (third 2026-11-03
	// list).
	var errs []error
	bad := func(err error) { errs = append(errs, err) }
	if !JobNameOK(name) {
		bad(ManifestError{Job: name, Msg: "a job name must be lowercase letters, " +
			"digits, and inner dashes, up to 60 characters"})
	}
	for _, removed := range []struct {
		set       bool
		key, hint string
	}{
		{job.Retries != nil, "retries", "nothing re-runs by itself any more; run a failed run again " +
			"with toft job rerun <runID>, or start the job again from your app"},
		{job.ViewerRunnable != nil, "viewer_runnable", "the app's own button decides who may start a job: " +
			"check toft.who(headers) and start it with toft.viewer(headers).jobs.start(...)"},
		{job.Enabled != nil, "enabled", "pause a job's schedule on the app's Automation tab, or remove " +
			"its schedule from toft.toml"},
		{job.Memory != nil, "memory", "an administrator sets a job's memory, on the app's Automation " +
			"tab; until then it runs at the install's job default"},
		{job.CPU != nil, "cpu", "an administrator sets a job's CPU, on the app's Automation tab; " +
			"until then it runs at the install's job default"},
	} {
		if removed.set && tolerateRemoved {
			*warnings = append(*warnings, "jobs."+name+"."+removed.key)
		} else if removed.set {
			bad(ManifestError{Job: name, Field: removed.key, Code: ManifestRemovedKey,
				Msg: "is no longer read", Hint: "remove " + removed.key + " from [jobs." + name + "]: " + removed.hint})
		}
	}

	command := strings.TrimSpace(job.Command)
	switch {
	case command == "" && len(job.Args) == 0:
		bad(ManifestError{Job: name, Field: "command", Msg: "is required",
			Hint: fmt.Sprintf(`add command = "python -m %s" to [jobs.%s], or args = [...] for a single binary`,
				strings.ReplaceAll(name, "-", "_"), name)})
	case command != "" && len(job.Args) > 0:
		bad(ManifestError{Job: name, Field: "args",
			Msg: "and command are two ways to say what runs; keep one"})
	}
	for _, a := range job.Args {
		if strings.TrimSpace(a) == "" {
			bad(ManifestError{Job: name, Field: "args", Msg: "has an empty argument"})
			break
		}
	}

	def := &database.JobDefinition{
		Name:     name,
		Command:  job.Command,
		Args:     job.Args,
		Schedule: schedule.Join(job.Timezone, job.Schedule),
		Enabled:  true,
	}

	switch job.Overlap {
	case "", database.JobOverlapQueue, database.JobOverlapSkip:
		def.Overlap = job.Overlap
	default:
		bad(ManifestError{Job: name, Field: "overlap", Msg: `must be "skip" or "queue"`})
	}
	switch job.Notify {
	case "", database.JobNotifyNone, database.JobNotifyFailures, database.JobNotifyAlways:
		def.Notify = job.Notify
	default:
		bad(ManifestError{Job: name, Field: "notify", Msg: `must be "none", "failures" or "always"`})
	}

	zoneBad := false
	if strings.TrimSpace(job.Timezone) != "" {
		if strings.TrimSpace(job.Schedule) == "" {
			bad(ManifestError{Job: name, Field: "timezone",
				Msg:  "is the zone a schedule is read in, and this job has no schedule",
				Hint: fmt.Sprintf(`add a schedule to [jobs.%s], like schedule = "0 3 * * *", or remove timezone`, name)})
		} else if err := schedule.ValidateZone(strings.TrimSpace(job.Timezone)); err != nil {
			zoneBad = true
			bad(ManifestError{Job: name, Field: "timezone", Msg: err.Error()})
		}
	}
	if job.Schedule != "" {
		if z, rest := schedule.Split(job.Schedule); z != "" {
			// One way to say it in the file. A prefix typed into `schedule`
			// beside a `timezone` key would be two answers to one question.
			bad(ManifestError{Job: name, Field: "schedule",
				Msg:  "names its zone with a prefix; put the zone in `timezone` instead",
				Hint: zonePrefixHint(z, rest)})
		} else if zoneBad {
			// The zone is said once, under timezone; the expression is still
			// checked on its own, so a bad cron beside a bad zone is the second
			// problem rather than hidden behind the first (BB-065).
			if err := schedule.Validate(job.Schedule); err != nil {
				bad(ManifestError{Job: name, Field: "schedule",
					Msg: "is not a valid 5-field cron expression: " + err.Error()})
			}
		} else if err := schedule.Validate(def.Schedule); err != nil {
			bad(ManifestError{Job: name, Field: "schedule",
				Msg: "is not a valid 5-field cron expression: " + err.Error()})
		} else if at, err := schedule.Next(def.Schedule, time.Now().UTC()); err == nil {
			// Seeded here rather than left for the scheduler's seed pass, so the
			// job is schedulable the instant the deploy finishes instead of up to
			// a tick later. Computed from now, so declaring a schedule never fires
			// for a window that has already passed. It is the value for a new or
			// changed schedule only: ReconcileManifestJobs keeps the stored one of
			// a live schedule the deploy left alone, because "now" here is seconds
			// before the write and a tick can fall between them.
			def.NextRunAt = &at
		}
	}
	if job.Overlap != "" && job.Schedule == "" {
		bad(ManifestError{Job: name, Field: "overlap",
			Msg:  "is what a scheduled firing does while the last run is going, and this job has no schedule",
			Code: "invalid_overlap", Hint: fmt.Sprintf(`add a schedule to [jobs.%s], like schedule = "0 3 * * *", or remove overlap`, name)})
	}

	// Bounds rather than silent clamping: a developer who wrote `timeout =
	// "30d"` meant something, and quietly running their job for less is worse
	// than telling them. The install's own ceiling is checked against these at
	// check and deploy, with the settings in hand.
	if job.Timeout != nil {
		secs, err := parseJobTimeout(job.Timeout)
		if err != nil {
			bad(ManifestError{Job: name, Field: "timeout", Msg: err.Error()})
		}
		def.TimeoutSec = secs
	}
	if strings.TrimSpace(job.ExpectWithin) != "" {
		secs, err := ParseExpectWithin(job.ExpectWithin)
		if err != nil {
			bad(ManifestError{Job: name, Field: "expect_within", Msg: err.Error()})
		}
		def.ExpectWithinSec = &secs
	}

	if len(job.Params) > 0 {
		pnames := make([]string, 0, len(job.Params))
		for pname := range job.Params {
			pnames = append(pnames, pname)
		}
		sort.Strings(pnames)
		for _, pname := range pnames {
			if err := checkManifestParam(name, pname, job.Params[pname]); err != nil {
				bad(err)
			}
			// A schedule starts a run with no parameters, so a required one
			// — which by rule has no default — can never be satisfied: the
			// job deployed cleanly and every firing was a skip with no run
			// row (code review 2026-11-11, C-6).
			if strings.TrimSpace(job.Schedule) != "" && job.Params[pname].Required {
				bad(ManifestError{Job: name, Field: "params." + pname,
					Msg:  "is required, and a scheduled job starts with no parameters, so the schedule could never fire",
					Hint: "give " + pname + " a default, or remove the schedule and start the job from your app with its parameters"})
			}
		}
		spec, err := json.Marshal(job.Params)
		if err != nil {
			bad(ManifestError{Job: name, Field: "params", Msg: err.Error()})
		}
		def.ParamsSpec = spec
	}

	if len(errs) > 0 {
		return nil, joinManifestErrors(errs)
	}
	return def, nil
}

// checkManifestParam holds one declared parameter to its own declaration: a
// known type, choices of that type, and a default that is one of them.
func checkManifestParam(job, pname string, p manifestParam) error {
	field := "params." + pname
	switch p.Type {
	case "", "string", "int", "float", "bool":
	default:
		return ManifestError{Job: job, Field: field + ".type", Msg: "must be one of string, int, float, or bool"}
	}
	if p.Required && p.Default != nil {
		return ManifestError{Job: job, Field: field,
			Msg: "is required and has a default; a default means it may be left out, so drop one"}
	}
	for _, c := range p.Choices {
		if !manifestValueIs(p.Type, c) {
			return ManifestError{Job: job, Field: field + ".choices",
				Msg: fmt.Sprintf("holds %v, which is not a %s", c, typeOrString(p.Type))}
		}
	}
	if p.Default != nil {
		if !manifestValueIs(p.Type, p.Default) {
			return ManifestError{Job: job, Field: field + ".default",
				Msg: fmt.Sprintf("is %v, which is not a %s", p.Default, typeOrString(p.Type))}
		}
		if len(p.Choices) > 0 && !slices.ContainsFunc(p.Choices, func(c any) bool {
			return fmt.Sprint(c) == fmt.Sprint(p.Default)
		}) {
			return ManifestError{Job: job, Field: field + ".default",
				Msg: fmt.Sprintf("is %v, which is not one of its choices", p.Default)}
		}
	}
	return nil
}

func typeOrString(t string) string {
	if t == "" {
		return "string"
	}
	return t
}

// manifestValueIs reports whether a TOML value has the declared type. TOML
// gives integers as int64 and floats as float64; an int is acceptable where a
// float is declared.
func manifestValueIs(typ string, v any) bool {
	switch typ {
	case "", "string":
		_, ok := v.(string)
		return ok
	case "int":
		_, ok := v.(int64)
		return ok
	case "float":
		switch v.(type) {
		case float64, int64:
			return true
		}
		return false
	case "bool":
		_, ok := v.(bool)
		return ok
	}
	return false
}

// parseJobTimeout reads a job's timeout: a Go-style duration string ("45m",
// "1h30m", "90s", "2h") or an integer number of seconds.
func parseJobTimeout(v any) (int, error) {
	const bounds = "must be between 1 second and 7 days"
	var secs int
	switch x := v.(type) {
	case int64:
		secs = int(x)
	case string:
		x = strings.TrimSpace(x)
		var d time.Duration
		// Whole days, as expect_within takes them: the bound is stated in
		// days, so "8d" was refused as no duration at all while "999h" was
		// told the limit is 7 days (BB-044).
		if days, ok := strings.CutSuffix(x, "d"); ok {
			n, err := strconv.Atoi(days)
			if err != nil {
				return 0, fmt.Errorf(`must be a duration like "45m", "2h" or "7d", or a number of seconds`)
			}
			d = time.Duration(n) * 24 * time.Hour
		} else {
			var err error
			if d, err = time.ParseDuration(x); err != nil {
				return 0, fmt.Errorf(`must be a duration like "45m", "2h" or "7d", or a number of seconds`)
			}
		}
		if d%time.Second != 0 {
			return 0, fmt.Errorf("must be a whole number of seconds")
		}
		secs = int(d / time.Second)
	default:
		return 0, fmt.Errorf(`must be a duration like "45m" or "2h", or a number of seconds`)
	}
	if secs < JobTimeoutMinSec || secs > JobTimeoutMaxSec {
		return 0, fmt.Errorf("%s, not %v", bounds, v)
	}
	return secs, nil
}

// checkManifest reads and judges a release's declared jobs, and records none of
// them: applyManifestJobs does that, at the flip.
//
// Called at the detect step. A parse failure fails the deploy: the alternative
// is deploying code whose jobs the platform decided not to understand, and the
// developer discovering it at 3am when the report does not arrive.
//
// **Judged early, recorded late.** The definitions were written here, minutes
// before the release they belong to was published, so for the length of every
// build the app's jobs were the new toft.toml's while every run executed the
// old release's code — and a build that then failed left the new definitions
// standing over the old release until the next deploy that succeeded
// (drills on trytoft, 2026-10-23; ADR §121).
func (b *Builder) checkManifest(app *database.App, deploy *database.Deployment, releaseDir string, appendLog func(string)) (ManifestDecl, error) {
	// A rollback republishes a file nobody can edit: a key removed from the
	// manifest since that release was built is one the platform no longer
	// reads anyway, so it is reported as unread rather than refused — the
	// refusal made a release deployed cleanly in October unreachable from
	// November (code review 2026-11-11, E-8). Every other error stands.
	decl, warnings, err := parseManifestAll(releaseDir, deploy != nil && deploy.SourceDeployID != "")
	defs := decl.Jobs
	// Said before the error check, because a manifest that fails to parse is
	// exactly when knowing what else is in the file is worth most — and a
	// refusal that swallowed the warnings would report the second problem only
	// after somebody fixed the first.
	//
	// **And recorded, not only logged.** A `--json` deploy never starts the log
	// follower, so the build log is the one channel its reader does not have —
	// which made the warning invisible to the caller most likely to have
	// written a table nobody reads. Written to the row here rather than
	// published on a frame, so a watcher that reconnects after the deploy ends
	// still receives it: see the column's migration.
	unread := make([]Warning, 0, len(warnings))
	for _, key := range warnings {
		w := UnreadKeyWarning(key)
		appendLog("==> Warning: " + w.Message)
		unread = append(unread, w)
	}
	b.addDeployWarningDetails(deploy, unread)
	if err != nil {
		return ManifestDecl{}, err
	}

	if err := checkJobsFit(app.Framework, defs); err != nil {
		return ManifestDecl{}, err
	}
	if err := checkSchedulesFit(app.Framework, decl); err != nil {
		return ManifestDecl{}, err
	}
	if b.db != nil {
		if settings, serr := b.db.GetPlatformSettings(); serr == nil {
			existing, _ := b.db.ListJobDefinitions(app.ID)
			if err := jobs.CheckCeilingsAgainst(defs, existing, settings); err != nil {
				return ManifestDecl{}, CeilingManifestError(err)
			}
			// What an administrator capped since: the deploy goes, and says so
			// where its result is read.
			// One job at a time, so each warning names its table, as toft
			// check's do (BB-017).
			var clamps []Warning
			for i := range defs {
				for _, c := range jobs.ClampWarnings(defs[i:i+1], settings) {
					appendLog("==> Warning: " + c)
					clamps = append(clamps, Warning{Code: WarningLimitClamped, Table: "jobs." + defs[i].Name, Message: c})
				}
			}
			b.addDeployWarningDetails(deploy, clamps)
		}
	}
	// The storage plan, against the table as it is now: judged here, after the
	// clone and before dependencies, and written at the flip.
	if decl.plan, err = b.planStorage(app, deploy, decl, appendLog); err != nil {
		return ManifestDecl{}, err
	}
	if len(defs) == 0 && IsBatchFramework(app.Framework) {
		// Not refused — the release may be on its way to declaring them — but
		// said, because a batch app with no jobs does nothing at all.
		appendLog("==> This batch app declares no [jobs] in toft.toml. It runs nothing until a " +
			"job is declared there.")
	}
	return decl, nil
}

// applyManifestJobs records the jobs checkManifest judged, once the release
// that declares them is the one serving. A run queued after this executes the
// code its definition came from; one queued before it ran the previous
// release under the previous definitions, which is the pair it belongs to.
func (b *Builder) applyManifestJobs(app *database.App, defs []database.JobDefinition, keepPauses bool, appendLog func(string)) error {
	if err := b.db.ReconcileManifestJobsKeeping(app.ID, defs, keepPauses); err != nil {
		return fmt.Errorf("recording declared jobs: %w", err)
	}
	if len(defs) > 0 {
		names := make([]string, 0, len(defs))
		for _, d := range defs {
			names = append(names, d.Name)
		}
		appendLog(fmt.Sprintf("==> Declared jobs: %s", strings.Join(names, ", ")))
	}
	return nil
}

// The `[runtime]` table — the repo's own statement about which language version
// it needs.
//
//	[runtime]
//	python = ">=3.12,<3.14"
//	node   = "24"
//
// **The declaration lives in the repo, not the dashboard** (ADR §64 D1), the
// same argument `[jobs]` above already makes: it versions and rolls back with
// the code it describes, and a rollback takes the pin back with it.
//
// **There is no `go` key, and that is not a gap** (D4). Since Go 1.21 a module
// states its requirement in `go.mod` — the `go` directive for the language
// version, an optional `toolchain` directive for the toolchain — and that file
// is the ecosystem's, honoured by every Go tool, already in every repo we would
// deploy. A `go` key here would be a second source of truth free to disagree
// with the first, and the platform would then have to decide which wins, which
// is a decision with no good answer. So one is refused rather than ignored:
// somebody who wrote it meant it, and silently not reading it is the failure
// mode this whole feature exists to close.
type manifestRuntime struct {
	Python string `toml:"python"`
	Node   string `toml:"node"`
	Go     string `toml:"go"`
	R      string `toml:"r"`
}

// RuntimePins is a release's declared ranges, keyed by language. Absent
// languages are absent from the map — an app pins what it cares about.
type RuntimePins map[string]string

// tomlError restates a TOML parse error with the line counted from the byte
// offset the library reports, rather than its own line counter — which has
// already stepped past the newline when the newline is what was unexpected,
// so an unterminated `[jobs.a` on the last line of a three-line file was
// reported at line 4, a line that does not exist.
// The two openings of a decode refusal. A file the decoder could not read is
// one that does not parse; a value of the wrong TOML type is in a file that
// parses perfectly well, and saying "could not be parsed" of `fallback = "yes"`
// sent its author looking for a syntax error that was not there (D-27, fourth
// 2026-11-03 black-box list). Both are manifest_syntax: the fix is the same
// kind — quotes added or taken away.
const (
	manifestUnparsed  = "could not be parsed as TOML"
	manifestWrongType = "has a value of the wrong type"
)

// tomlDecodeError is a decode failure as the manifest's refusal, opened by
// which of the two it is.
func tomlDecodeError(data string, err error) ManifestError {
	if tomlTypeMismatch.MatchString(err.Error()) {
		return ManifestError{Msg: manifestWrongType + ": " + tomlError(data, err)}
	}
	return ManifestError{Msg: manifestUnparsed + ": " + tomlError(data, err)}
}

func tomlError(data string, err error) string {
	// A value of the wrong TOML type is reported by the decoder in Go's terms
	// ("TOML value has type int64; destination has type string"), which a
	// toft.toml author cannot act on: `node = 22` needs quotes (BB-035).
	if m := tomlTypeMismatch.FindStringSubmatch(err.Error()); m != nil {
		key := m[2]
		leaf := key[strings.LastIndex(key, ".")+1:]
		if m[4] == "string" {
			return fmt.Sprintf("toml: line %s: %s must be a quoted string — write %s = \"...\"", m[1], key, leaf)
		}
		return fmt.Sprintf("toml: line %s: %s is a %s here, and must be a %s", m[1], key, tomlTypeWord(m[3]), tomlTypeWord(m[4]))
	}
	var pe toml.ParseError
	if !errors.As(err, &pe) || pe.Position.Start < 0 || pe.Position.Start > len(data) {
		return err.Error()
	}
	line := 1 + strings.Count(data[:pe.Position.Start], "\n")
	if pe.LastKey == "" {
		return fmt.Sprintf("toml: line %d: %s", line, pe.Message)
	}
	return fmt.Sprintf("toml: line %d (last key %q): %s", line, pe.LastKey, pe.Message)
}

var tomlTypeMismatch = regexp.MustCompile(`line (\d+) \(last key "([^"]+)"\): incompatible types: TOML value has type ([\w\[\]{}.]+); (?:Go )?destination has type ([\w\[\]{}.]+)`)

// tomlTypeWord says a Go type the decoder named in a manifest author's words.
func tomlTypeWord(goType string) string {
	switch {
	case strings.HasPrefix(goType, "int"):
		return "number"
	case strings.HasPrefix(goType, "float"):
		return "decimal number"
	case goType == "string":
		return "quoted string"
	case goType == "bool", goType == "boolean":
		return "true or false"
	case strings.HasPrefix(goType, "[]"):
		return "list"
	case strings.HasPrefix(goType, "map"):
		return "table"
	}
	return goType
}

// ParseRuntimePins reads the `[runtime]` table out of a release.
//
// A missing file, or a file with no `[runtime]` table, is not an error: most
// apps declare nothing and take the install's default. What *is* an error is a
// table this package half understood — a range it could not parse, or a `go`
// key — because a pin the platform decided not to read is a pin that silently
// does not exist, which is worse than never having written one.
func ParseRuntimePins(releaseDir string) (RuntimePins, error) {
	data, err := readInRelease(releaseDir, ManifestFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ManifestFile, err)
	}

	var m struct {
		Runtime *manifestRuntime `toml:"runtime"`
	}
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, tomlDecodeError(string(data), err)
	}
	if m.Runtime == nil {
		return nil, nil
	}

	if strings.TrimSpace(m.Runtime.Go) != "" {
		return nil, ManifestError{Field: "[runtime] go", Msg: "is not read here. A Go module " +
			"already states the version it needs in go.mod — the `go` directive for the " +
			"language version, `toolchain` for the toolchain — and that is the file Go's own " +
			"tooling honours. A second declaration here could only disagree with it. Remove " +
			"this key and set the `go` line in go.mod instead"}
	}

	pins := RuntimePins{}
	for lang, raw := range map[string]string{
		toolchain.Python: m.Runtime.Python,
		toolchain.Node:   m.Runtime.Node,
		toolchain.R:      m.Runtime.R,
	} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Parsed here rather than at resolution time so a malformed range is
		// refused before anything has been built, and the message names the
		// key the developer is looking at.
		if _, err := toolchain.ParseConstraint(lang, raw); err != nil {
			return nil, ManifestError{Field: "[runtime] " + lang, Msg: err.Error()}
		}
		pins[lang] = raw
	}
	if len(pins) == 0 {
		return nil, nil
	}
	return pins, nil
}

// LanguageOf is which language family a framework builds and runs in, or "" for
// a framework that has none.
//
// Static is the "" case and it is a real one: a folder of HTML runs no commands
// and has no interpreter, which is why a `[runtime]` table on one is refused
// rather than accepted as a no-op.
func LanguageOf(f Framework) string {
	switch f {
	case FrameworkNode, FrameworkNextJS, FrameworkBatchNode:
		return toolchain.Node
	case FrameworkGo, FrameworkBatchGo:
		return toolchain.Go
	case FrameworkBatchPython:
		return toolchain.Python
	case FrameworkBatchR:
		return toolchain.R
	case FrameworkFlask, FrameworkFastAPI, FrameworkStreamlit, FrameworkPython,
		FrameworkNotebook, FrameworkMarimo:
		// Both notebook frameworks are Python, and `notebook` is the one worth
		// pausing on: it is Python at *build* time and a file server at run
		// time, and this answer is about the build. The release carries a venv
		// whether or not anything at run time opens it, so a `[runtime] python`
		// pin on one is a real pin — it decides which interpreter executes the
		// cells.
		return toolchain.Python
	case FrameworkShiny, FrameworkPlumber, FrameworkRMarkdown, FrameworkQuarto:
		// The two R shapes that are processes. The two that are documents are R
		// at build time and a file server at run time, on the same terms as
		// `notebook` above — and they answer here too, because the release
		// carries an renv library whether or not anything at run time opens it,
		// so a `[runtime] r` pin decides which interpreter renders.
		return toolchain.R
	}
	return ""
}

// GoModPin reads the `go` directive out of a release's go.mod and returns it as
// a range.
//
// `go 1.25` becomes `>=1.25`, and the direction is Go's own promise rather than
// this platform's guess: a module asking for an older language version builds
// correctly under a newer toolchain, because the `go` line sets language
// semantics and compatibility is something Go keeps. Node and Python have no
// equivalent promise, which is the whole reason they need a set of interpreters
// and this needs only a floor.
//
// A release with no go.mod, or one whose `go` line is absent or unreadable, has
// no pin. That is not an error: it is a Go program somebody is deploying, and
// the toolchain on the host will build it or say why not.
func GoModPin(releaseDir string) string {
	data, err := readInRelease(releaseDir, "go.mod")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "go ")
		if !ok {
			continue
		}
		// `go 1.25.3` is legal since 1.21 and names a patch release; the pin is
		// the line, and patch never matters.
		fields := strings.SplitN(strings.TrimSpace(rest), ".", 3)
		if len(fields) < 2 {
			return ""
		}
		v := fields[0] + "." + fields[1]
		if _, err := toolchain.ParseConstraint(toolchain.Go, ">="+v); err != nil {
			return ""
		}
		return ">=" + v
	}
	return ""
}

// The bounds on expect_within, in seconds: an hour, because a shorter window is
// a scheduler's job rather than an alert's, and 31 days, because a monthly job
// is the longest anybody declares.
const (
	ExpectWithinMinSec = 3600
	ExpectWithinMaxSec = 31 * 24 * 3600
)

// ParseExpectWithin reads a window as written in toft.toml or the UI: a Go
// duration ("26h", "90m") or whole days ("7d"). One function for both doors, so
// the two cannot disagree about what "2d" means.
func ParseExpectWithin(v string) (int, error) {
	v = strings.TrimSpace(v)
	var d time.Duration
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration — write one like 26h or 7d", v)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration — write one like 26h or 7d", v)
		}
		d = parsed
	}
	secs := int(d / time.Second)
	if secs < ExpectWithinMinSec || secs > ExpectWithinMaxSec {
		return 0, fmt.Errorf("must be between 1h and 31d, not %s", v)
	}
	return secs, nil
}

// addDeployWarnings appends to what the deploy has already noticed — the
// manifest's keys nobody reads, then the runtime's — and records the whole list.
//
// On the caller's copy as well as in the column: the terminal progress frame is
// assembled from that struct, and re-reading the row to build it would be a
// query for a value this function already has.
func (b *Builder) addDeployWarnings(deploy *database.Deployment, sentences []string) {
	details := make([]Warning, 0, len(sentences))
	for _, s := range sentences {
		details = append(details, Warning{Message: s})
	}
	b.addDeployWarningDetails(deploy, details)
}

// addDeployWarningDetails is addDeployWarnings for warnings that carry a code,
// kept with it so the deploy's result has the warning_details toft check
// gives for the same tree.
func (b *Builder) addDeployWarningDetails(deploy *database.Deployment, warnings []Warning) {
	if len(warnings) == 0 || deploy == nil {
		return
	}
	deploy.Warnings = EncodeWarningDetails(append(decodeStoredWarnings(deploy.Warnings), warnings...))
	if b.db != nil {
		if err := b.db.SetDeploymentWarnings(deploy.ID, deploy.Warnings); err != nil {
			// Logged and not fatal. A warning that could not be stored must not
			// turn a deploy that was going to succeed into one that failed.
			log.Printf("deploy %s: recording warnings: %v", deploy.ID, err)
		}
	}
}

// checkJobsFit refuses jobs a release cannot run: any job on a static app, and
// `args` on a release with no single built binary. One function for the deploy
// and the preflight, so `toft check` cannot pass what the deploy then refuses.
//
// A static app runs no commands. That is not a policy choice dressed up as a
// limitation: the static family image is an empty base holding the runner
// binary and nothing else — no interpreter, no `/bin/sh` — and a job's command
// is run as `/bin/sh -c`.
func checkJobsFit(framework string, defs []database.JobDefinition) error {
	if len(defs) > 0 && framework == string(FrameworkStatic) {
		names := make([]string, 0, len(defs))
		for _, d := range defs {
			names = append(names, d.Name)
		}
		return staticJobsError(names)
	}
	if HasSingleBinary(framework) {
		return nil
	}
	for _, d := range defs {
		if len(d.Args) > 0 {
			return ManifestError{Job: d.Name, Field: "args",
				Msg:  fmt.Sprintf("runs the build's single binary, and a %s release has none", framework),
				Hint: fmt.Sprintf(`use command = "..." in [jobs.%s] instead`, d.Name)}
		}
	}
	return nil
}

// HasSingleBinary reports whether a framework's build produces one binary that
// `args` can run: Go, served or batch.
func HasSingleBinary(framework string) bool {
	return framework == string(FrameworkGo) || framework == string(FrameworkBatchGo)
}

// CeilingManifestError is a declaration above the install's ceiling, as a
// manifest refusal with the code programs branch on.
func CeilingManifestError(err error) error {
	var ce *jobs.CeilingError
	if !errors.As(err, &ce) {
		return err
	}
	return ManifestError{Job: ce.Job, Field: ce.Key, Code: "limit_exceeds_ceiling",
		Msg: ce.Describe(), Hint: ce.Hint()}
}

// staticJobsError is the refusal of a [jobs] table on a static app — one
// sentence for the deploy (checkManifest) and the preflight, so `toft
// check` cannot pass what the deploy then refuses.
func staticJobsError(names []string) error {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return ManifestError{Msg: fmt.Sprintf("declares %s (%s), and a static app runs no "+
		"commands — it is a folder of files served by the platform, with no runtime to run "+
		"one in. Remove the [jobs] tables, or deploy this as an app in a language "+
		"Toft supports", plural(len(sorted), "a job", "jobs"), strings.Join(sorted, ", "))}
}

// Warning is one thing a deploy noticed and carried on past, in the shape a
// program branches on: a code, the toft.toml table when there is one, and the
// sentence. The deploy's own result still carries the sentences alone; the
// preflight carries both.
type Warning struct {
	Code  string `json:"code"`
	Table string `json:"table,omitempty"`
	// Path is the file or directory of the tree the warning is about, when it
	// is about one: a go.work `use` directory the tree lacks. The CLI's own
	// warnings of the same code carry the same two fields (BB-007).
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// Warning codes. The list is the specification: a new kind of warning names
// itself here.
const (
	WarningManifestUnread = "manifest_unread"
	// A Node version stated in package.json engines or .nvmrc, which Toft
	// does not read, on a tree with no [runtime] node (nodeVersionUnread).
	WarningNodeVersionUnread = "node_version_unread"
	// A go.mod asking for a Go this install does not have: not refused (D5),
	// but the build will ask the go command to fetch it, and fail if it cannot.
	WarningGoToolchainFetched = "go_toolchain_fetched"
	// A job declared above a ceiling an administrator lowered after the
	// declaration was deployed: the job runs at the ceiling, and says so.
	WarningLimitClamped = "limit_clamped"
	// An R tree with no renv.lock: the build installs no packages and says
	// the app will fail at its first library() — said at check, not only in
	// the build log (rPackagesUnlocked).
	WarningNoRenvLock = "no_renv_lock"
	// A [runtime] python pin that pyproject.toml's requires-python excludes
	// (pinExcludedByRequiresPython).
	WarningPinExcluded = "pin_excluded_by_requires_python"
	// A [restart] or [render] schedule an earlier release declared and this
	// one does not: removed at the flip, Table naming which (BB-017).
	WarningScheduleRemoved = "schedule_removed"
	// The release's [jobs], or its [restart]/[render], could not be written at
	// the flip: the previous release's stay.
	WarningJobsNotRecorded      = "jobs_not_recorded"
	WarningSchedulesNotRecorded = "schedules_not_recorded"
	// No advisory data has loaded on this install, so dependencies were listed
	// and judged against nothing.
	WarningAdvisoryDataMissing = "advisory_data_missing"
	// A covered ecosystem the release resolved into has no corpus loaded on
	// this install, though another has: its packages were judged against
	// nothing (security review 2026-11-14, DG-5).
	WarningAdvisoryCorpusMissing = "advisory_corpus_missing"
	// What the dependency resolution did not read, one per deps.Gap kind
	// (gapWarnings): a lockfile passed over, no lockfile at all, a vendored
	// directory, a manifest not resolved, only direct packages listed.
	WarningLockfileNotRead   = "lockfile_not_read"
	WarningNoLockfile        = "no_lockfile"
	WarningVendoredUnchecked = "vendored_not_checked"
	WarningDepsUnresolved    = "dependencies_unresolved"
	WarningDepsDirectOnly    = "dependencies_direct_only"
)

// UnreadKeyWarning is a toft.toml key nothing reads, as a warning — the only
// spelling of its sentence.
func UnreadKeyWarning(key string) Warning {
	return Warning{Code: WarningManifestUnread, Table: key,
		Message: fmt.Sprintf("%s in %s is not read by Toft and was ignored", key, ManifestFile)}
}
