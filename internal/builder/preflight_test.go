package builder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/deploysteps"
	"toft/internal/jobs"
	"toft/internal/toolchain"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A static site passes, names itself, and says what it did not check.
func TestPreflight_StaticSitePasses(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{"index.html": "<p>hi</p>"}), "shared", nil, config.Envelope{})
	if !p.OK() || p.Framework != string(FrameworkStatic) || p.Runtime != nil {
		t.Fatalf("preflight = %+v", p)
	}
	if len(p.NotChecked) == 0 {
		t.Error("a preflight must say what only a deploy can answer")
	}
}

// BB-014: a malformed [static] in a tree that detects as something else is a
// problem, not a table dropped without a word; `root = "."` is no problem.
func TestPreflight_AMalformedStaticTableIsSaid(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{
		"app.py": "print(1)", "requirements.txt": "flask\n",
		"toft.toml": "[static]\nroot = \"../site\"\n"}), "shared", nil, config.Envelope{})
	if p.OK() || !strings.Contains(p.Problems[0].Message, "[static] root") {
		t.Fatalf("preflight = %+v", p)
	}
	p = RunPreflight(writeTree(t, map[string]string{
		"index.html": "<p>hi</p>", "toft.toml": "[static]\nroot = \".\"\n"}), "shared", nil, config.Envelope{})
	if !p.OK() || p.Framework != string(FrameworkStatic) {
		t.Fatalf("root = \".\": preflight = %+v", p)
	}
}

// BB-038: every manifest problem carries a code and a hint.
func TestPreflight_ManifestProblemsAreCoded(t *testing.T) {
	for toml, want := range map[string]string{
		"[jobs.a]\ncommand = \"x\"\ntimeout = \"9d\"\n":                                   "invalid_timeout",
		"[jobs.a]\ncommand = \"x\"\nschedule = \"nope\"\n":                                "invalid_schedule",
		"[jobs.a]\ncommand = \"x\"\noverlap = \"maybe\"\n":                                "invalid_overlap",
		"[jobs.a]\ncommand = \"x\"\nnotify = \"loud\"\n":                                  "invalid_notify",
		"[jobs.Bad_Name]\ncommand = \"x\"\n":                                              "invalid_job_name",
		"[jobs.a]\ncommand = \"x\"\nschedule = \"0 3 * * *\"\ntimezone = \"Mars/Base\"\n": "invalid_timezone",
	} {
		p := RunPreflight(writeTree(t, map[string]string{"app.py": "x", "requirements.txt": "",
			"toft.toml": toml}), "shared", nil, config.Envelope{})
		if p.OK() || p.Problems[0].Code != want || p.Problems[0].Hint == "" {
			t.Errorf("%q: problems = %+v, want code %s with a hint", toml, p.Problems, want)
		}
	}
}

// BB-029: a pin that requires-python excludes is said, not passed silently.
func TestPreflight_APinRequiresPythonExcludesIsWarned(t *testing.T) {
	avail := toolchain.Available(toolchain.Python)
	if len(avail) == 0 {
		t.Skip("no Python on this host")
	}
	old := avail[len(avail)-1].Version
	series := strings.Join(strings.Split(old, ".")[:2], ".")
	p := RunPreflight(writeTree(t, map[string]string{"app.py": "x", "requirements.txt": "",
		"toft.toml":      "[runtime]\npython = \"" + series + "\"\n",
		"pyproject.toml": "[project]\nname = \"x\"\nrequires-python = \">99\"\n"}), "shared", nil, config.Envelope{})
	found := false
	for _, w := range p.WarningDetails {
		found = found || w.Code == WarningPinExcluded
	}
	if !found {
		t.Fatalf("no %s warning: %+v", WarningPinExcluded, p)
	}
}

// Nothing a deploy can serve is the deploy's own refusal, in its own key.
func TestPreflight_UnsupportedIsTheDeploysReason(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{"notes.txt": "x"}), "shared", nil, config.Envelope{})
	if p.OK() || p.Problems[0].Reason != string(deploysteps.ReasonUnsupportedFramework) {
		t.Fatalf("preflight = %+v", p)
	}
}

// A malformed manifest and an unread table are both reported — the second as
// the warning a deploy's result would carry.
func TestPreflight_Manifest(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{
		"index.html": "x", "toft.toml": "[reports]\nx = 1\n"}), "shared", nil, config.Envelope{})
	if !p.OK() || len(p.Warnings) == 0 {
		t.Fatalf("an unread table should warn and pass: %+v", p)
	}
	// A sentence, as the deploy's result carries it — never the bare key —
	// and the structured half beside it.
	if p.Warnings[0] != "reports in toft.toml is not read by Toft and was ignored" {
		t.Errorf("warning = %q", p.Warnings[0])
	}
	if d := p.WarningDetails; len(d) != 1 || d[0].Code != WarningManifestUnread || d[0].Table != "reports" {
		t.Errorf("warning_details = %+v", d)
	}
	p = RunPreflight(writeTree(t, map[string]string{
		"index.html": "x", "toft.toml": "[jobs\n"}), "shared", nil, config.Envelope{})
	if p.OK() || p.Problems[0].Reason != string(deploysteps.ReasonManifestInvalid) {
		t.Fatalf("a malformed manifest should refuse: %+v", p)
	}
}

// With the install's settings in hand, a declaration above the ceiling is the
// deploy's own refusal — limit_exceeds_ceiling with the fix — and every job's
// effective limits come back with their source.
func TestPreflight_JobCeilingsAndLimits(t *testing.T) {
	settings := &database.PlatformSettings{DefaultJobTimeoutSecs: 3600, MaxJobTimeoutSecs: 900}
	p := RunPreflight(writeTree(t, map[string]string{
		"main.py": "print(1)\n", "requirements.txt": "",
		"toft.toml": "[jobs.nightly]\ncommand = \"python main.py\"\ntimeout = \"45m\"\n"}), "shared", settings, config.Envelope{})
	if p.OK() || p.Problems[0].Code != "limit_exceeds_ceiling" ||
		p.Problems[0].Hint != `set timeout = "15m" in [jobs.nightly], or ask an admin to raise max_job_timeout_seconds` {
		t.Fatalf("problems = %+v", p.Problems)
	}
	// Refused, and only refused: the declaration is what job_limits shows, and
	// no limit_clamped warning — "clamped" is a ceiling lowered under a job
	// already deployed (jobs-regression #16).
	if len(p.JobLimits) != 1 || p.JobLimits[0].Limits.TimeoutSec != (jobs.Limit{Value: 2700, Source: "declared"}) {
		t.Errorf("job limits = %+v", p.JobLimits)
	}
	for _, d := range p.WarningDetails {
		if d.Code == WarningLimitClamped {
			t.Errorf("a refused declaration was also warned clamped: %+v", d)
		}
	}

	// The same declaration already deployed, under a ceiling lowered since:
	// not refused, clamped, and warned.
	deployed := database.JobDefinition{Name: "nightly", TimeoutSec: 2700}
	p = RunPreflight(writeTree(t, map[string]string{
		"main.py": "print(1)\n", "requirements.txt": "",
		"toft.toml": "[jobs.nightly]\ncommand = \"python main.py\"\ntimeout = \"45m\"\n"}), "shared", settings, config.Envelope{}, deployed)
	if !p.OK() || len(p.JobLimits) != 1 ||
		p.JobLimits[0].Limits.TimeoutSec != (jobs.Limit{Value: 900, Source: "clamped", Declared: 2700}) {
		t.Errorf("a deployed declaration under a lowered ceiling: ok=%v problems=%+v limits=%+v", p.OK(), p.Problems, p.JobLimits)
	}
	warned := false
	for _, d := range p.WarningDetails {
		warned = warned || d.Code == WarningLimitClamped && d.Table == "jobs.nightly"
	}
	if !warned {
		t.Errorf("and it is warned limit_clamped, naming its table: %+v", p.WarningDetails)
	}
}

// A [runtime] table on a static app is refused by resolveRuntime as a manifest
// error, and the preflight files it under the same key.
func TestPreflight_RuntimeTableOnStaticIsManifest(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{
		"index.html": "x", "toft.toml": "[runtime]\npython = \"3.13\"\n"}), "shared", nil, config.Envelope{})
	if p.OK() || p.Problems[0].Reason != string(deploysteps.ReasonManifestInvalid) {
		t.Fatalf("preflight = %+v", p)
	}
}

// A static tree with [jobs] is the deploy's manifest refusal, and the
// preflight makes it too.
func TestPreflight_JobsOnStaticAreRefused(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{
		"index.html": "x", "toft.toml": "[jobs.nightly]\ncommand = \"echo hi\"\n"}), "shared", nil, config.Envelope{})
	if p.OK() || !strings.Contains(p.Problems[0].Message, "static app runs no") || p.Problems[0].Reason != string(deploysteps.ReasonManifestInvalid) {
		t.Fatalf("preflight = %+v", p)
	}
	// With the install's settings, the refused jobs still get no limits: they
	// will never run (drills 2026-10-30).
	settings := database.DefaultPlatformSettings()
	p = RunPreflight(writeTree(t, map[string]string{
		"index.html": "x", "toft.toml": "[jobs.nightly]\ncommand = \"echo hi\"\n"}), "shared", &settings, config.Envelope{})
	if p.OK() || len(p.JobLimits) != 0 {
		t.Fatalf("job_limits on a static app = %+v, want none beside the refusal", p.JobLimits)
	}
}

// A go.mod asking for a Go this install lacks passes the check, as the deploy
// does (D5) -- but with a warning naming both versions, where it used to say
// nothing and the deploy then spent a build failing to fetch the toolchain.
func TestPreflight_GoModAskingForAMissingGoWarns(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.99\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}), "shared", nil, config.Envelope{})
	if !p.OK() {
		t.Fatalf("an unsatisfied go line is not a refusal: %+v", p)
	}
	var found *Warning
	for i := range p.WarningDetails {
		if p.WarningDetails[i].Code == WarningGoToolchainFetched {
			found = &p.WarningDetails[i]
		}
	}
	if found == nil {
		t.Fatalf("no %s warning: %+v", WarningGoToolchainFetched, p.WarningDetails)
	}
	if !strings.Contains(found.Message, "1.99") {
		t.Errorf("the warning does not name the version go.mod asks for: %q", found.Message)
	}
}

// toft check says what [restart] and [render] would record, and refuses what
// the deploy would refuse (ADR §144).
func TestPreflight_Schedules(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{
		"index.html": "x",
		"toft.toml":  "[restart]\nschedule = \"0 4 * * *\"\n"}), "shared", nil, config.Envelope{})
	if p.OK() || p.Problems[0].Code != ManifestRestartChangesNothing || p.Restart != nil {
		t.Fatalf("a static app's [restart] = %+v", p)
	}

	p = RunPreflight(writeTree(t, map[string]string{
		"report.ipynb": `{"cells":[],"metadata":{},"nbformat":4,"nbformat_minor":5}`,
		"toft.toml":    "[render]\nschedule = \"0 6 * * 1\"\ntimezone = \"Europe/London\"\n"}), "shared", nil, config.Envelope{})
	if p.Render == nil || p.Render.Schedule != "0 6 * * 1" || p.Render.Timezone != "Europe/London" ||
		p.Render.NextRunAt == nil || p.Render.Stored != "CRON_TZ=Europe/London 0 6 * * 1" {
		t.Fatalf("a notebook's [render] = %+v (problems %+v)", p.Render, p.Problems)
	}
}

// BB-005 and BB-013 (second 2026-11-03 bash): every problem carries a code
// and a hint — the [env] and [[storage]] refusals had a code only, and
// unsupported_framework neither — and a hint answers its own message.
func TestPreflight_EveryProblemHasACodeAndAHint(t *testing.T) {
	py := map[string]string{"app.py": "print(1)", "requirements.txt": "flask\n"}
	with := func(base map[string]string, extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, c := range []struct {
		name     string
		tree     map[string]string
		code     string
		hintHas  string
		hintLack string
	}{
		{"env list", with(py, map[string]string{"toft.toml": "[env]\nS = [1, 2]\n"}), "env_entry_invalid", "[env]", ""},
		{"env name", with(py, map[string]string{"toft.toml": "[env]\n\"1BAD\" = \"x\"\n"}), "invalid_env_name", "rename", ""},
		{"env reserved", with(py, map[string]string{"toft.toml": "[env]\nPORT = \"1\"\n"}), "reserved_env_name", "rename", ""},
		{"storage access", with(py, map[string]string{"toft.toml": "[[storage]]\nname = \"x\"\naccess = \"bogus\"\n"}), "storage_entry_invalid", "access", ""},
		{"static jobs", map[string]string{"index.html": "<p>", "toft.toml": "[jobs.a]\ncommand = \"x\"\n"}, "manifest_invalid", "[jobs]", ""},
		{"empty", map[string]string{}, "unsupported_framework", "empty", ""},
		{"python no entry", map[string]string{"requirements.txt": "flask\n"}, "unsupported_framework", "app.py", ""},
		{"two ipynb", map[string]string{"a.ipynb": "{}", "b.ipynb": "{}"}, "unsupported_framework", "[notebook] file", ""},
		{"two Rmd", map[string]string{"a.Rmd": "x", "b.Rmd": "x"}, "unsupported_framework", "[notebook] file", ""},
		{"bare R", map[string]string{"x.R": "#*\n1\n"}, "unsupported_framework", "plumber", ""},
		{"go library", map[string]string{"go.mod": "module x\n\ngo 1.22\n", "lib.go": "package lib\n"}, "unsupported_framework", "main package", ""},
		{"node width", map[string]string{"server.js": "x", "package.json": `{"scripts":{"start":"node server.js"}}`, "toft.toml": "[runtime]\nnode = \"22.1.0\"\n"}, "invalid_runtime", `node = "22"`, "python"},
		{"python width", with(py, map[string]string{"toft.toml": "[runtime]\npython = \"3.13.18\"\n"}), "invalid_runtime", `python = "3.13"`, "node"},
		{"go key", with(py, map[string]string{"toft.toml": "[runtime]\ngo = \"1.26\"\n"}), "invalid_runtime", "go.mod", "python ="},
		{"requires-python", map[string]string{"app.py": "x", "pyproject.toml": "[project]\nname = \"x\"\nrequires-python = \">=3.20\"\n"}, "runtime_version", "requires-python", "remove the pin"},
		{"overlap", with(py, map[string]string{"toft.toml": "[jobs.slow]\ncommand = \"x\"\noverlap = \"skip\"\n"}), "invalid_overlap", "add a schedule", ""},
	} {
		p := RunPreflight(writeTree(t, c.tree), "shared", nil, config.Envelope{})
		if p.OK() {
			t.Errorf("%s: no problem: %+v", c.name, p)
			continue
		}
		for _, pp := range p.Problems {
			if pp.Code == "" || pp.Hint == "" {
				t.Errorf("%s: a problem without both: %+v", c.name, pp)
			}
		}
		pp := p.Problems[0]
		if pp.Code != c.code || !strings.Contains(pp.Hint, c.hintHas) || (c.hintLack != "" && strings.Contains(pp.Hint, c.hintLack)) {
			t.Errorf("%s: code %q hint %q (message %q)", c.name, pp.Code, pp.Hint, pp.Message)
		}
	}
}

// A [notebook] table is read, so it is never reported as read by nobody — the
// refusals themselves tell an author to write one.
func TestTheNotebookTableIsNotUnread(t *testing.T) {
	dir := writeManifest(t, "[notebook]\nfile = \"a.ipynb\"\nfiel = \"b\"\n")
	_, warnings, err := ParseManifestWithWarnings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != "notebook.fiel" {
		t.Fatalf("warnings = %q", warnings)
	}
}

// BB-010 (third 2026-11-03 list): toft check listed only the first manifest
// problem, so a table with five removed keys took five round trips. Every
// problem is a problem of its own, with its code and hint; the deploy still
// refuses on the first.
func TestPreflightListsEveryManifestProblem(t *testing.T) {
	cases := []struct {
		name, toml string
		codes      []string
	}{
		{"removed keys", "[jobs.a]\ncommand = \"python a.py\"\nretries = 2\nmemory = 512\ncpu = 1\nviewer_runnable = true\nenabled = false\n",
			[]string{"removed_key", "removed_key", "removed_key", "removed_key", "removed_key"}},
		{"two jobs", "[jobs.a]\ncommand = \"python a.py\"\ntimeout = \"bad\"\n[jobs.b]\ncommand = \"python b.py\"\nnotify = \"x\"\n",
			[]string{"invalid_timeout", "invalid_notify"}},
		{"env", "[env]\nD = 5\nE = { foo = 1 }\n",
			[]string{EnvEntryInvalidCode, EnvEntryInvalidCode}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"app.py": "from flask import Flask\napp = Flask(__name__)\n",
				"requirements.txt": "flask\n", ManifestFile: c.toml})
			p := RunPreflight(dir, "shared", nil, config.Envelope{})
			var got []string
			for _, pr := range p.Problems {
				if pr.Reason != "manifest_invalid" {
					continue
				}
				if pr.Hint == "" {
					t.Errorf("problem without a hint: %+v", pr)
				}
				got = append(got, pr.Code)
			}
			if strings.Join(got, ",") != strings.Join(c.codes, ",") {
				t.Errorf("codes = %v, want %v (%+v)", got, c.codes, p.Problems)
			}
			// The deploy's refusal is the first problem's sentence alone.
			_, _, err := ParseManifestAll(dir)
			var me ManifestError
			if err == nil || !errors.As(err, &me) || !strings.HasPrefix(err.Error(), p.Problems[0].Message) {
				t.Errorf("deploy refusal = %v, want the first problem", err)
			}
		})
	}
}

// BB-011 (third 2026-11-03 list): the deploy carries go_toolchain_fetched as
// the check does — one function writes both — and a go build that could not
// fetch the toolchain names the versions this install has, where go's own
// refusal names only the one it could not download.
func TestAnUnsatisfiedGoLineWarnsTheDeployAndItsFailureNamesTheInstallsGo(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":  "module example.com/app\n\ngo 1.99\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	var b Builder
	rt, err := b.resolveRuntime(FrameworkGo, dir, "shared", func(string) {})
	if err != nil || rt == nil || rt.Unsatisfied == "" {
		t.Fatalf("rt = %+v, err = %v", rt, err)
	}
	w, ok := goToolchainFetched(rt)
	if !ok || w.Code != WarningGoToolchainFetched || !strings.Contains(w.Message, "go.mod pins Go >=1.99") {
		t.Fatalf("warning = %+v", w)
	}
	have := toolchain.Available(toolchain.Go)
	if len(have) == 0 {
		t.Skip("no Go on this host")
	}
	msg := goBuildError(errors.New("go: download go1.99.0 for linux/amd64: toolchain not available"), rt).Error()
	if !strings.Contains(msg, "toolchain not available") || !strings.Contains(msg, have[0].Series()) ||
		!strings.Contains(msg, "lower the go line") {
		t.Errorf("failure = %q, want go's refusal and this install's %s", msg, have[0].Series())
	}
	if got := goBuildError(errors.New("x"), &resolvedRuntime{}).Error(); got != "go build failed: x" {
		t.Errorf("a satisfied go line's failure = %q", got)
	}
}

// BB-012 (third 2026-11-03 list): toft check's problem defects — one problem
// per fault, the code a manifest fault is, an engine list the code accepts.
func TestPreflightProblemDefectsBB012(t *testing.T) {
	problems := func(files map[string]string) []PreflightProblem {
		return RunPreflight(writeTree(t, files), "shared", nil, config.Envelope{}).Problems
	}
	// An unquoted pin is one manifest_syntax problem, not two.
	for _, pin := range []string{"node = 22", "r = 4.5"} {
		ps := problems(map[string]string{"app.R": "library(shiny)\n", "package.json": `{"name":"a"}`, "index.js": "",
			"toft.toml": "[runtime]\n" + pin + "\n"})
		if len(ps) != 1 || ps[0].Code != "manifest_syntax" {
			t.Errorf("%s: problems = %+v", pin, ps)
		}
	}
	// A wrongly typed [static] key on a valid static tree: the key, in the
	// manifest's words, and no unsupported_framework beside it.
	ps := problems(map[string]string{"index.html": "<p>hi</p>", "toft.toml": "[static]\nfallback = \"yes\"\n"})
	if len(ps) != 1 || ps[0].Reason != "manifest_invalid" || !strings.Contains(ps[0].Message, "static.fallback") ||
		!strings.Contains(ps[0].Message, "true or false") || strings.Contains(ps[0].Message, "destination has type") ||
		// It parses: the value is the wrong type, and the message says so (D-27).
		strings.Contains(ps[0].Message, "could not be parsed") || !strings.Contains(ps[0].Message, "wrong type") {
		t.Errorf("[static] fallback: problems = %+v", ps)
	}
	// [notebook] engine: an unknown value is manifest_invalid and names the
	// four the code accepts; knitr and jupyter are accepted.
	nb := map[string]string{"nb.ipynb": `{"cells":[],"metadata":{},"nbformat":4,"nbformat_minor":5}`, "requirements.txt": "jupyter\n"}
	nb["toft.toml"] = "[notebook]\nengine = \"latex\"\n"
	ps = problems(nb)
	if len(ps) != 1 || ps[0].Reason != "manifest_invalid" || ps[0].Code != "invalid_notebook_engine" ||
		!strings.Contains(ps[0].Message, `"knitr"`) || !strings.Contains(ps[0].Message, `"jupyter"`) ||
		!strings.Contains(ps[0].Hint, `"nbconvert"`) {
		t.Errorf("engine latex: problems = %+v", ps)
	}
	nb["toft.toml"] = "[notebook]\nengine = \"jupyter\"\n"
	if ps = problems(nb); len(ps) != 0 {
		t.Errorf("engine jupyter: problems = %+v", ps)
	}
	if ps = problems(map[string]string{"doc.qmd": "---\ntitle: x\n---\n```{r}\n1\n```\n",
		"toft.toml": "[notebook]\nengine = \"knitr\"\n"}); len(ps) != 0 {
		t.Errorf("engine knitr: problems = %+v", ps)
	}
}

// BB-013 (third 2026-11-03 list): a hint is the fix for its own message, and
// never the message again.
func TestPreflightHintsAnswerTheirMessage(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{"app.py": "from flask import Flask\n", "requirements.txt": "flask\n",
		"toft.toml": "[jobs.a]\ncommand = \"x\"\nschedule = \"CRON_TZ=Europe/London 0 3 * * *\"\n" +
			"[jobs.b]\ncommand = \"x\"\ntimezone = \"Europe/London\"\n" +
			"[restart]\nschedule = \"*/30 * * * *\"\n"}), "shared", nil, config.Envelope{})
	want := map[string]string{
		"[jobs.a] schedule":  `write timezone = "Europe/London" and schedule = "0 3 * * *"`,
		"[jobs.b] timezone":  "add a schedule to [jobs.b]",
		"[restart] schedule": "fire at most hourly",
	}
	for _, pr := range p.Problems {
		if strings.Contains(pr.Message, pr.Hint) {
			t.Errorf("the hint repeats the message: %+v", pr)
		}
		for where, hint := range want {
			if strings.Contains(pr.Message, where) {
				if !strings.Contains(pr.Hint, hint) {
					t.Errorf("%s: hint %q, want %q", where, pr.Hint, hint)
				}
				delete(want, where)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("not reported: %v (%+v)", want, p.Problems)
	}

	// A pin nothing satisfies is hinted with a line this install has.
	have := EffectiveVersions(toolchain.Python, "shared")
	if len(have) == 0 {
		t.Skip("no Python on this host")
	}
	p = RunPreflight(writeTree(t, map[string]string{"app.py": "from flask import Flask\n", "requirements.txt": "flask\n",
		"toft.toml": "[runtime]\npython = \"3.9\"\n"}), "shared", nil, config.Envelope{})
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0].Hint, `python = "`+have[0].Series()+`"`) ||
		strings.Contains(p.Problems[0].Hint, "the message names") {
		t.Errorf("runtime_version: %+v", p.Problems)
	}
}

// M4 (2026-11-04 black-box list): a [static] root this tree does not hold is a
// check problem, as it is the deploy's refusal.
func TestPreflight_AStaticRootThatIsNotThereIsAProblem(t *testing.T) {
	p := RunPreflight(writeTree(t, map[string]string{
		"index.html": "<p>hi</p>", "toft.toml": "[static]\nroot = \"nope\"\n"}), "shared", nil, config.Envelope{})
	if p.OK() || p.Problems[0].Code != "invalid_static" || p.Problems[0].Field != "static.root" ||
		!strings.Contains(p.Problems[0].Message, `"nope"`) || p.Problems[0].Hint == "" {
		t.Fatalf("preflight = %+v", p)
	}
	p = RunPreflight(writeTree(t, map[string]string{
		"dist/index.html": "<p>hi</p>", "toft.toml": "[static]\nroot = \"dist\"\n"}), "shared", nil, config.Envelope{})
	if !p.OK() {
		t.Fatalf("a root that is there: preflight = %+v", p)
	}
}

// L1 (2026-11-04 black-box list): a [tasks] table is one problem among the
// file's others, not the only one listed.
func TestPreflight_TasksDoesNotMaskTheJobsProblems(t *testing.T) {
	toml := "[jobs.a]\ncommand = \"x\"\nretries = 2\nmemory = \"1G\"\ncpu = 2\nviewer_runnable = true\nenabled = false\n" +
		"[tasks.x]\npath = \"/d\"\n"
	p := RunPreflight(writeTree(t, map[string]string{"app.py": "x", "requirements.txt": "", "toft.toml": toml}),
		"shared", nil, config.Envelope{})
	if len(p.Problems) < 6 || p.Problems[0].Code != "removed_key" || !strings.Contains(p.Problems[0].Message, "[tasks]") {
		t.Fatalf("problems = %+v, want [tasks] and the five [jobs.a] keys", p.Problems)
	}
}

// A go.work naming a directory the tree does not hold — a git-ignored
// .toft/sdk-go — is warned at check, which passed it before the deploy failed
// to load the module (BB-028).
func TestAGoWorkNamingAMissingDirIsWarned(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.work"), []byte("go 1.22\n\nuse (\n\t.\n\t./.toft/sdk-go // vendored\n)\nuse ./tools\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "tools"), 0o755)
	var got []Warning
	for _, w := range RunPreflight(dir, "shared", nil, config.Envelope{}).WarningDetails {
		if w.Code == WarningGoWorkDirExcluded {
			got = append(got, w)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0].Message, ".toft/sdk-go") {
		t.Fatalf("warnings = %+v, want one naming .toft/sdk-go", got)
	}
	// One shape for the check, the deploy and the CLI's own: table and path
	// both (BB-007).
	if got[0].Table != "go.work" || got[0].Path != ".toft/sdk-go" {
		t.Errorf("warning = %+v, want table go.work and path .toft/sdk-go", got[0])
	}
	raw, _ := json.Marshal(got[0])
	if !strings.Contains(string(raw), `"path":".toft/sdk-go"`) {
		t.Errorf("json = %s, want a path field", raw)
	}
}

// A [notebook] file the tree lacks no longer hides the table's other
// problem: engine = "magic" is listed beside it (BB-009).
func TestPreflight_MissingNotebookFileDoesNotHideTheEngine(t *testing.T) {
	toml := "[render]\nschedule = \"0 1 * * *\"\n[notebook]\nfile = \"x.ipynb\"\nengine = \"magic\"\n"
	p := RunPreflight(writeTree(t, map[string]string{"report.ipynb": "{}", "requirements.txt": "", "toft.toml": toml}),
		"shared", nil, config.Envelope{})
	codes := map[string]bool{}
	for _, pr := range p.Problems {
		codes[pr.Code] = true
	}
	if !codes["invalid_notebook_file"] || !codes["invalid_notebook_engine"] {
		t.Fatalf("problems = %+v, want invalid_notebook_file and invalid_notebook_engine", p.Problems)
	}
}

// The [runtime] go refusal names a Go this build host has, not a fixed
// "go 1.25" the install may not offer (BB-008).
func TestRuntimeGoHintNamesAnOfferedVersion(t *testing.T) {
	h := runtimeHint("go", "")
	if set := toolchain.Available(toolchain.Go); len(set) > 0 && !strings.HasSuffix(h, "like go "+set[0].Series()) {
		t.Errorf("hint = %q, want it to end with go %s", h, set[0].Series())
	}
}
