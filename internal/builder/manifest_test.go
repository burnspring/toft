package builder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A repo with no manifest declares no jobs, and that is not an error: most
// apps have none, and failing here would break every existing deploy.
func TestNoManifestIsNotAnError(t *testing.T) {
	defs, err := ParseManifest(writeManifest(t, ""))
	if err != nil {
		t.Fatalf("a missing manifest should be fine: %v", err)
	}
	if len(defs) != 0 {
		t.Errorf("got %d jobs from nothing", len(defs))
	}
}

// A table this package does not read is not this package's business.
//
// `toft.toml` is shared: `[static]` belongs to internal/staticserve, and
// `[app]` is how the CLI works out which app a directory deploys to. Today's
// decoder happens to skip what the struct does not name, and "today it happens
// to work" is not a contract — without this test, the first developer to name
// their app in the file would break their own deploy with a manifest error from
// a change that added nothing this package reads.
func TestUnknownTablesAreIgnored(t *testing.T) {
	dir := writeManifest(t, `
[app]
slug = "phase-test"

[app.installs."https://staging.toft.corp"]
slug = "phase-test-stg"

[static]
root = "dist"

[something.nobody.has.written.yet]
whatever = 1

[jobs.nightly]
command = "python -m reports.nightly"
`)
	defs, err := ParseManifest(dir)
	if err != nil {
		t.Fatalf("an unknown table should not fail a deploy: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "nightly" {
		t.Fatalf("got %d jobs, want the one that was declared", len(defs))
	}
}

// The same file with no [jobs] at all is still a file with no jobs in it, not a
// file the platform refuses. This is the case a repository lands in the moment
// `toft link` writes an [app] table into a project that declares no automation.
func TestAManifestOfNothingButAppIsFine(t *testing.T) {
	defs, err := ParseManifest(writeManifest(t, "[app]\nslug = \"phase-test\"\n"))
	if err != nil {
		t.Fatalf("`toft link` must not break a deploy: %v", err)
	}
	if len(defs) != 0 {
		t.Errorf("got %d jobs, want none", len(defs))
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := writeManifest(t, `
[jobs.nightly-report]
command = "python -m reports.nightly"
schedule = "0 3 * * *"
timeout = "30m"
overlap = "skip"
notify = "failures"

[jobs.nightly-report.params.region]
type = "string"
default = "emea"
choices = ["emea", "amer"]
help = "Which region"

[jobs.cleanup]
args = ["job", "cleanup"]
timeout = 600
`)
	defs, err := ParseManifest(dir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(defs))
	}

	byName := map[string]int{}
	for i, d := range defs {
		byName[d.Name] = i
	}

	nightly := defs[byName["nightly-report"]]
	if nightly.Command != "python -m reports.nightly" || nightly.Schedule != "0 3 * * *" {
		t.Errorf("nightly = %+v", nightly)
	}
	if nightly.TimeoutSec != 1800 {
		t.Errorf("timeout not read: %d", nightly.TimeoutSec)
	}
	if nightly.Overlap != "skip" || nightly.Notify != "failures" {
		t.Errorf("overlap/notify not read: %q %q", nightly.Overlap, nightly.Notify)
	}
	// A scheduled job is seeded with a next fire time computed from now, so
	// creating it does not immediately fire for a window already past.
	if nightly.NextRunAt == nil {
		t.Error("a scheduled job should be seeded with a next run time")
	}
	var spec map[string]any
	if err := json.Unmarshal(nightly.ParamsSpec, &spec); err != nil || spec["region"] == nil ||
		!strings.Contains(string(nightly.ParamsSpec), `"choices":["emea","amer"]`) {
		t.Errorf("params not captured: %s", nightly.ParamsSpec)
	}

	cleanup := defs[byName["cleanup"]]
	if len(cleanup.Args) != 2 || cleanup.Command != "" || cleanup.TimeoutSec != 600 {
		t.Errorf("cleanup = %+v, want args and an integer timeout in seconds", cleanup)
	}
	// A job with no schedule is trigger-only, not broken.
	if defs[byName["cleanup"]].Schedule != "" {
		t.Error("cleanup should have no schedule")
	}
}

// A malformed manifest fails the deploy, and the message names the field. The
// developer is looking at a file they just edited; "invalid manifest" would
// make them guess.
func TestManifestErrorsNameTheField(t *testing.T) {
	cases := []struct {
		name, body, wantIn string
	}{
		{"no command", "[jobs.a]\nschedule = \"0 3 * * *\"\n", "command"},
		{"bad cron", "[jobs.a]\ncommand = \"x\"\nschedule = \"not a cron\"\n", "schedule"},
		{"window not a duration", "[jobs.a]\ncommand = \"x\"\nexpect_within = \"daily\"\n", "expect_within"},
		{"window too short", "[jobs.a]\ncommand = \"x\"\nexpect_within = \"5m\"\n", "expect_within"},
		{"unknown zone", "[jobs.a]\ncommand = \"x\"\nschedule = \"0 3 * * *\"\ntimezone = \"Nowhere/Bad\"\n", "timezone"},
		{"zone with no schedule", "[jobs.a]\ncommand = \"x\"\ntimezone = \"Europe/London\"\n", "timezone"},
		{"zone as a prefix", "[jobs.a]\ncommand = \"x\"\nschedule = \"CRON_TZ=Europe/London 0 3 * * *\"\n", "schedule"},
		{"timeout too long", "[jobs.a]\ncommand = \"x\"\ntimeout = 999999\n", "timeout"},
		{"timeout not a duration", "[jobs.a]\ncommand = \"x\"\ntimeout = \"soon\"\n", "timeout"},
		{"command and args", "[jobs.a]\ncommand = \"x\"\nargs = [\"y\"]\n", "args"},
		{"bad overlap", "[jobs.a]\ncommand = \"x\"\nschedule = \"0 3 * * *\"\noverlap = \"allow\"\n", "overlap"},
		{"overlap with no schedule", "[jobs.a]\ncommand = \"x\"\noverlap = \"skip\"\n", "overlap"},
		{"bad notify", "[jobs.a]\ncommand = \"x\"\nnotify = \"loud\"\n", "notify"},
		{"choice of the wrong type", "[jobs.a]\ncommand = \"x\"\n[jobs.a.params.p]\ntype = \"int\"\nchoices = [\"a\"]\n", "params.p.choices"},
		{"default outside the choices", "[jobs.a]\ncommand = \"x\"\n[jobs.a.params.p]\ndefault = \"c\"\nchoices = [\"a\", \"b\"]\n", "params.p.default"},
		{"memory declared", "[jobs.a]\ncommand = \"x\"\nmemory = 1024\n", "an administrator sets a job's memory"},
		{"cpu declared", "[jobs.a]\ncommand = \"x\"\ncpu = 500\n", "[jobs.a] cpu"},
		{"memory declared as zero", "[jobs.a]\ncommand = \"x\"\nmemory = 0\n", "memory"},
		{"bad param type", "[jobs.a]\ncommand = \"x\"\n[jobs.a.params.p]\ntype = \"blob\"\n", "params.p.type"},
		{"bad name", "[jobs.\"Not Valid\"]\ncommand = \"x\"\n", "job name"},
		{"not toml", "this is not toml {{{\n", "TOML"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(writeManifest(t, tc.body))
			if err == nil {
				t.Fatal("accepted an invalid manifest")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error should point at %q, got: %v", tc.wantIn, err)
			}
			// And it should always say which file, so the developer knows
			// where to look.
			if !strings.Contains(err.Error(), ManifestFile) {
				t.Errorf("error should name the file: %v", err)
			}
		})
	}
}

// Keys that were once read are refused with what replaced them, as
// removed_key, and so is a [tasks] table: each is something its author
// believes is in force.
func TestRemovedKeysAreRefusedWithTheirReplacement(t *testing.T) {
	for body, want := range map[string]string{
		"[jobs.a]\ncommand = \"x\"\nretries = 2\n":            "toft job rerun",
		"[jobs.a]\ncommand = \"x\"\nviewer_runnable = true\n": "toft.who",
		"[jobs.a]\ncommand = \"x\"\nenabled = false\n":        "Automation tab",
		"[jobs.a]\ncommand = \"x\"\nmemory = 1024\n":          "an administrator sets a job's memory",
		"[jobs.a]\ncommand = \"x\"\ncpu = 500\n":              "an administrator sets a job's CPU",
		"[tasks.digest]\npath = \"/d\"\n":                     "[jobs.digest]",
	} {
		_, err := ParseManifest(writeManifest(t, body))
		var me ManifestError
		if !errors.As(err, &me) || me.Code != ManifestRemovedKey || !strings.Contains(me.Hint, want) {
			t.Errorf("%q: %v, want removed_key with a hint naming %q", body, err, want)
		}
	}
}

// A declaration that names no timeout says so: the install's default applies
// at dispatch, and an effective limit shows its source.
func TestManifestDefaults(t *testing.T) {
	defs, err := ParseManifest(writeManifest(t, "[jobs.a]\ncommand = \"echo hi\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := defs[0]
	if d.TimeoutSec != 0 || d.OverrideMemoryMB != 0 || d.OverrideCPUMillicores != 0 {
		t.Errorf("defaults = timeout %d, overrides %d MB %dm", d.TimeoutSec, d.OverrideMemoryMB, d.OverrideCPUMillicores)
	}
	if !d.Enabled {
		t.Error("a declared job should be enabled")
	}
}

// **An unknown table is still ignored, and is no longer silent.**
//
// The contract is that toft.toml is shared — `[static]` is
// internal/staticserve's, `[app]` is the CLI's — so a table this package does
// not recognize must not fail the deploy. What it must not do either is say
// nothing: whatever a developer declared there does not exist, the deploy
// reports success, and there is no signal to recover from.
func TestUnreadTablesAreReportedAndStillIgnored(t *testing.T) {
	dir := writeManifest(t, `
[app]
slug = "demo"

[static]
root = "dist"

[reports]
digest = "weekly"

[jobs.nightly]
command = "python worker.py"
memry = 2048

[jobs.nightly.params.region]
type = "string"
choises = ["emea"]
`)
	defs, warnings, err := ParseManifestWithWarnings(dir)
	if err != nil {
		t.Fatalf("an unknown table failed the deploy: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "nightly" {
		t.Fatalf("the job was not read: %+v", defs)
	}

	got := strings.Join(warnings, " ")
	// The table nobody reads.
	if !strings.Contains(got, "reports") {
		t.Errorf("[reports] was ignored silently, which is the whole defect: %q", got)
	}
	// A typo inside a parameter's own table: a closed set that stayed open.
	if !strings.Contains(got, "jobs.nightly.params.region.choises") {
		t.Errorf("a misspelled parameter key was ignored silently: %q", got)
	}
	// The typo'd key inside a job, which is a memory limit that quietly stayed
	// at the default.
	if !strings.Contains(got, "jobs.nightly.memry") {
		t.Errorf("a misspelled job key was ignored silently: %q", got)
	}
	// And the tables that belong to other readers are not warned about, or every
	// app naming itself in this file gets a warning it cannot act on.
	for _, quiet := range []string{"static", "app", "runtime"} {
		if strings.Contains(got, quiet) {
			t.Errorf("warned about [%s], which another package reads: %q", quiet, got)
		}
	}
}

// BB-018: `[app]` is the CLI's, and its keys are a closed set like every other
// table's — a misspelled one is reported, the CLI's own are not.
func TestAnUnknownKeyUnderAppIsReported(t *testing.T) {
	dir := writeManifest(t, `
[app]
slug = "demo"
install = "https://toft.corp"
name = "Demo"
colour = "red"

[app.installs."https://staging.corp"]
slug = "demo-stg"
slg = "typo"
`)
	_, warnings, err := ParseManifestWithWarnings(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(warnings, " ")
	if !strings.Contains(got, "app.colour") {
		t.Errorf("[app] colour was ignored silently: %q", got)
	}
	if !strings.Contains(got, "slg") {
		t.Errorf("a misspelled key inside an install entry was ignored silently: %q", got)
	}
	for _, quiet := range []string{"app.slug", "app.install ", "app.name", "demo-stg"} {
		if strings.Contains(got, quiet) {
			t.Errorf("warned about %s, which the CLI reads: %q", quiet, got)
		}
	}
}

// **A table this product reads is not a table it warns about**, and `[runtime]`
// was both at once.
//
// The pin is honoured — `node = "24"` builds against 24, `"22"` against 22 —
// and the same build log said the table was ignored. The warning also travels
// in `deploy.Warnings`, which is what a `--json` caller gets *instead of* the
// build log, so the false half reached the one audience that could not see the
// line contradicting it. What it implies is deleting the pin, and deleting the
// pin is the single change that silently moves an app onto the install default.
//
// The two halves are asserted together here on purpose: the reader that reads
// it, and the warning that must not mention it. They lived in one file, a few
// hundred lines apart, and disagreed.
func TestARuntimePinIsReadAndNotWarnedAbout(t *testing.T) {
	dir := writeManifest(t, `
[runtime]
node = "24"
`)

	pins, err := ParseRuntimePins(dir)
	if err != nil {
		t.Fatalf("ParseRuntimePins: %v", err)
	}
	if pins["node"] != "24" {
		t.Fatalf("the pin was not read: %+v — if this is now correct, the warning below is", pins)
	}

	_, warnings, err := ParseManifestWithWarnings(dir)
	if err != nil {
		t.Fatalf("ParseManifestWithWarnings: %v", err)
	}
	if got := strings.Join(warnings, " "); strings.Contains(got, "runtime") {
		t.Errorf("warned that [runtime] is ignored, in the same package that reads it: %q", got)
	}
}

// A job's parameter names are the author's, so they are not reported as
// unrecognized — the one place under [jobs] where an unknown key is the point.
func TestDeclaredParameterNamesAreNotWarnedAbout(t *testing.T) {
	dir := writeManifest(t, `
[jobs.nightly]
command = "python worker.py"

[jobs.nightly.params.region]
type = "string"
required = true
`)
	_, warnings, err := ParseManifestWithWarnings(dir)
	if err != nil {
		t.Fatalf("ParseManifestWithWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warned about a declared parameter: %q", warnings)
	}
}

// A zone travels joined to the expression, and the first fire time is computed
// in it.
func TestManifestTimezoneIsJoinedToTheSchedule(t *testing.T) {
	defs, err := ParseManifest(writeManifest(t,
		"[jobs.a]\ncommand = \"x\"\nschedule = \"0 6 * * *\"\ntimezone = \"Europe/London\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if defs[0].Schedule != "CRON_TZ=Europe/London 0 6 * * *" {
		t.Errorf("schedule = %q", defs[0].Schedule)
	}
	loc, _ := time.LoadLocation("Europe/London")
	if defs[0].NextRunAt == nil || defs[0].NextRunAt.In(loc).Hour() != 6 {
		t.Errorf("next run = %v, want 06:00 London", defs[0].NextRunAt)
	}
}

func TestManifestExpectWithin(t *testing.T) {
	defs, err := ParseManifest(writeManifest(t, "[jobs.a]\ncommand = \"x\"\nexpect_within = \"2d\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if defs[0].ExpectWithinSec == nil || *defs[0].ExpectWithinSec != 2*24*3600 {
		t.Errorf("expect_within = %v, want two days", defs[0].ExpectWithinSec)
	}
}

// A parse error names a line the file has. The library's counter has already
// stepped past the newline when the newline is what it did not expect, so a
// table left open on the last line of a three-line toft.toml was "line 4".
func TestManifestParseErrorNamesALineTheFileHas(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ManifestFile), []byte("[jobs.a]\ncommand = \"x\"\n[jobs.b\n"), 0o644)
	_, err := ParseManifest(dir)
	if err == nil || !strings.Contains(err.Error(), "line 3") || strings.Contains(err.Error(), "line 4") {
		t.Errorf("err = %v, want line 3", err)
	}
}

// BB-044: a timeout in whole days is a duration, and the bound names the value.
func TestAJobTimeoutInDays(t *testing.T) {
	if secs, err := parseJobTimeout("7d"); err != nil || secs != 7*24*3600 {
		t.Fatalf("7d = %d, %v", secs, err)
	}
	_, err := parseJobTimeout("8d")
	if err == nil || !strings.Contains(err.Error(), "7 days") || !strings.Contains(err.Error(), "8d") {
		t.Fatalf("8d: %v, want the 7-day bound and the value", err)
	}
	if _, err := parseJobTimeout("xd"); err == nil {
		t.Fatal("xd was accepted")
	}
}

// BB-035: `node = 22` is told it needs quotes, not Go's type names.
func TestAnUnquotedPinIsToldToQuoteIt(t *testing.T) {
	_, err := ParseRuntimePins(writeManifest(t, "[runtime]\nnode = 22\n"))
	if err == nil || strings.Contains(err.Error(), "int64") || !strings.Contains(err.Error(), "quoted") {
		t.Fatalf("err = %v", err)
	}
}

// BB-009 (second 2026-11-03 bash): a key under [runtime] no reader knows was
// accepted in silence — `ruby = "3"` is a pin that does not exist.
func TestAnUnknownKeyUnderRuntimeIsReported(t *testing.T) {
	dir := writeManifest(t, `
[runtime]
python = "3.13"
ruby = "3"

[batch]
language = "python"
lang = "py"
`)
	_, warnings, err := ParseManifestWithWarnings(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(warnings, " ")
	for _, want := range []string{"runtime.ruby", "batch.lang"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was not reported: %q", want, got)
		}
	}
	if strings.Contains(got, "runtime.python") || strings.Contains(got, "batch.language") {
		t.Errorf("a key that is read was reported: %q", got)
	}
}

// A rollback republishes a file nobody can edit: a key removed from the
// manifest since that release was built is a warning on a rollback and a
// refusal on a deploy (code review 2026-11-11, E-8).
func TestARollbackIsNotRefusedForAKeyRemovedSinceItWasBuilt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ManifestFile), []byte("[jobs.nightly]\ncommand = \"python nightly.py\"\nretries = 2\n"), 0o644)
	b := &Builder{}
	app := &database.App{Framework: "flask"}
	var logged []string
	appendLog := func(s string) { logged = append(logged, s) }

	if _, err := b.checkManifest(app, &database.Deployment{}, dir, appendLog); err == nil {
		t.Fatal("a deploy with a removed key was not refused")
	}
	rollback := &database.Deployment{SourceDeployID: "deploy-old"}
	decl, err := b.checkManifest(app, rollback, dir, appendLog)
	if err != nil {
		t.Fatalf("the rollback was refused: %v", err)
	}
	if len(decl.Jobs) != 1 || decl.Jobs[0].Name != "nightly" {
		t.Errorf("jobs = %+v", decl.Jobs)
	}
	if len(logged) == 0 || !strings.Contains(strings.Join(logged, "\n"), "retries") {
		t.Errorf("no warning named the key: %q", logged)
	}
	// Any other error still stands on a rollback.
	os.WriteFile(filepath.Join(dir, ManifestFile), []byte("[jobs.nightly]\nretries = 2\n"), 0o644)
	if _, err := b.checkManifest(app, rollback, dir, appendLog); err == nil {
		t.Fatal("a job with no command passed on a rollback")
	}
}

// A scheduled job with a required parameter can never fire — a schedule starts
// a run with no parameters — so the file is refused rather than deploying a job
// whose every firing is a skip with no run row (code review 2026-11-11, C-6).
func TestAScheduledJobWithARequiredParameterIsRefused(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ManifestFile), []byte(`[jobs.report]
command = "python report.py"
schedule = "0 6 * * *"
[jobs.report.params.region]
required = true
`), 0o644)
	_, _, err := ParseManifestAll(dir)
	if err == nil || !strings.Contains(err.Error(), "could never fire") {
		t.Fatalf("err = %v, want a refusal naming the required parameter", err)
	}
	// Without the schedule it is fine: the app starts it with its parameters.
	os.WriteFile(filepath.Join(dir, ManifestFile), []byte(`[jobs.report]
command = "python report.py"
[jobs.report.params.region]
required = true
`), 0o644)
	if _, _, err := ParseManifestAll(dir); err != nil {
		t.Fatalf("an unscheduled job with a required parameter was refused: %v", err)
	}
}
