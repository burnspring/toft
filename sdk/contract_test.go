package sdk_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"toft/internal/runner"
)

// The runner callback contract has four speakers and one number, and nothing
// compared them until a job said so.
//
// `runner.ContractVersion` moved to "2" when the workload started proving its
// identity with a projected ServiceAccount token instead of a minted bearer.
// That change is the *runner's* — an SDK never touches the bootstrap route — so
// all three SDK copies were left at "1", and because the platform reports skew
// rather than tolerating it, every `run.progress()` and `set_result()` in every
// language answered 409: "runner contract version mismatch: runner speaks 1,
// platform speaks 2 — a partial upgrade; redeploy the mismatched half". A
// correct message about the wrong halves, on a wire that was otherwise
// unchanged.
//
// The version is deliberately one number over all the callback routes, not one
// per route, so *this* is what an SDK owes it: the same literal, in step. An
// external test package because `internal/runner` imports this one.
//
// R joined when it grew a runner surface (current_run, run_progress): a
// language that posts to `/api/v1/runner/*` is in this table from that day.
var sdkContractVersion = map[string]*regexp.Regexp{
	filepath.Join("python", "toft", "client.py"): regexp.MustCompile(`RUNNER_CONTRACT_VERSION = "([^"]*)"`),
	// The exported twin, which drifted to "1" while every call sent "2".
	filepath.Join("python", "toft", "__init__.py"): regexp.MustCompile(`(?m)^CONTRACT_VERSION = "([^"]*)"`),
	filepath.Join("node", "index.js"):              regexp.MustCompile(`const CONTRACT_VERSION = "([^"]*)"`),
	filepath.Join("go", "run.go"):                  regexp.MustCompile(`const runnerContractVersion = "([^"]*)"`),
	filepath.Join("r", "R", "env.R"):               regexp.MustCompile(`RUNNER_CONTRACT_VERSION <- "([^"]*)"`),
}

func TestEverySDKSpeaksThePlatformsRunnerContract(t *testing.T) {
	for file, pattern := range sdkContractVersion {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		m := pattern.FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s: no contract version constant matching %s — it was renamed, or "+
				"the literal came back inline where nothing can compare it", file, pattern)
		}
		if got := string(m[1]); got != runner.ContractVersion {
			t.Errorf("%s speaks contract %q; the platform speaks %q. Every job callback "+
				"from this SDK answers 409 until they agree", file, got, runner.ContractVersion)
		}
	}
}

// The constant is only worth having if the callbacks use it. This is the shape
// the bug had: a named version beside three call sites that each wrote their
// own literal, which is a constant that documents rather than decides.
func TestNoSDKWritesTheContractVersionInline(t *testing.T) {
	inline := regexp.MustCompile(`(?i)(Runner-Version"?\s*[,:]\s*"[0-9]|"version"\s*:\s*"[0-9]|\bversion:\s*"[0-9])`)
	for file := range sdkContractVersion {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		if m := inline.Find(raw); m != nil {
			t.Errorf("%s writes the contract version inline (%q); use the constant, "+
				"or the next bump moves the constant and leaves the wire behind", file, m)
		}
	}
}

// **Every `/api/v1/app/*` path an SDK calls is a route the platform serves.**
//
// The guard above pins one number across four languages; this pins the thing
// that number is carried on. The R SDK's whole jobs surface called routes that
// do not exist — `POST /api/v1/app/runs`, which is registered for GET only and
// belongs to tasks, and a run read under `/runs/{id}`, which is registered only
// with `/logs` on the end. Both were exported, both were named in the feature
// catalog, and neither had ever worked. Nothing noticed because nothing
// compared the two halves: Go, Node and Python happened to be right, and being
// right by inspection is not a property anything holds.
//
// The comparison is textual on purpose. Reaching for the router would mean
// importing internal/api and standing up a server to enumerate it, which is
// heavier than the failure warrants — and the failure is not subtle.
//
// **Comments are stripped, then only string literals are read.** A path in a
// client is inside quotes; a path in a comment is prose. Both halves are
// needed, and the second alone is not enough — a doc comment naming a route
// that does not exist is exactly what the fix for this defect had to write
// down, and `sdk/r/R/jobs.R` says `/api/v1/app/runs/{id}` in prose to explain
// where it used to point. Scanning literals alone reported that sentence as
// the bug it describes.
func TestEverySDKPathIsARouteThePlatformServes(t *testing.T) {
	routes := appRoutes(t)
	for _, dir := range []string{"go", "node", "python", "r"} {
		for _, file := range sourceFiles(t, dir) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("reading %s: %v", file, err)
			}
			for _, hit := range appPathsIn(filepath.Ext(file), string(raw)) {
				path := normalizeSDKPath(hit)
				if path == "" || servedBy(routes, path) {
					continue
				}
				t.Errorf("%s calls %q, which no route under /api/v1/app serves.\n"+
					"    reduced to: %s\n    served:     %s",
					file, hit, path, strings.Join(sortedKeys(routes), " "))
			}
		}
	}
}

// stringLiteral spans a "…", '…' or `…` run with no newline in it, which is how
// all four languages spell a path.
var stringLiteral = regexp.MustCompile("\"[^\"\n]*\"|`[^`\n]*`|'[^'\n]*'")

// appPath is the path as a client writes it, interpolation included.
// `*` is in the class because rPaths has already substituted it for R's
// concatenated values — without it the placeholder was truncated off the end
// and `/api/v1/app/runs/*` was read back as the served `/api/v1/app/runs`.
var appPath = regexp.MustCompile(`/api/v1/app[A-Za-z0-9/_{}$%*.\-]*`)

func appPathsIn(ext, source string) []string {
	source = stripComments(ext, source)
	if ext == ".R" {
		return rPaths(source)
	}
	var out []string
	for _, literal := range stringLiteral.FindAllString(source, -1) {
		out = append(out, appPath.FindAllString(literal, -1)...)
	}
	return out
}

// pasteCall spans one `paste0( … )`, allowing one level of nested parentheses —
// which is as deep as `utils::URLencode(x, reserved = TRUE)` goes.
var pasteCall = regexp.MustCompile(`paste0\(((?:[^()]|\([^()]*\))*)\)`)

// rPaths reads R's paths, which are the only ones assembled by concatenation.
//
// **R is the odd language out, and it is the one that was broken.** Go, Node
// and Python interpolate inside the literal — `f"{RUNS_PATH}/{run_id}/logs"` —
// so the literal alone carries the whole shape. R writes
// `paste0("/api/v1/app/job-runs/", utils::URLencode(id), "/logs")`, where the
// value is a separate argument and the literal is a fragment. Reading only the
// literals there reduces `"/api/v1/app/runs/"` to `/api/v1/app/runs`, which is
// a served route, so the defect this test exists for matched and passed.
//
// So the arguments are joined: a quoted one contributes its text, anything else
// contributes a `*`. That is exactly the reduction normalizeSDKPath performs on
// the other three, arrived at one step later.
func rPaths(source string) []string {
	var out []string
	for _, call := range pasteCall.FindAllStringSubmatch(source, -1) {
		var b strings.Builder
		for _, arg := range splitArgs(call[1]) {
			if literal := stringLiteral.FindString(arg); literal != "" && strings.TrimSpace(arg) == literal {
				b.WriteString(strings.Trim(literal, `"'`+"`"))
				continue
			}
			b.WriteString("*")
		}
		out = append(out, appPath.FindAllString(b.String(), -1)...)
	}
	// And the paths that are whole literals, outside any concatenation.
	for _, literal := range stringLiteral.FindAllString(pasteCall.ReplaceAllString(source, ""), -1) {
		out = append(out, appPath.FindAllString(literal, -1)...)
	}
	return out
}

// splitArgs splits one argument list on top-level commas.
func splitArgs(args string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range args {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, args[start:i])
				start = i + 1
			}
		}
	}
	return append(out, args[start:])
}

// stripComments removes what the language treats as prose, so a sentence about
// a route is not read as a call to one.
//
// Deliberately blunt: a `#` or `//` inside a string literal takes the rest of
// that line with it. The cost of that is a missed call site — never a false
// report — and no path in these four packages is written after one.
func stripComments(ext, source string) string {
	line := "#"
	if ext == ".go" || ext == ".js" {
		line = "//"
		source = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(source, "")
	}
	if ext == ".py" {
		// Docstrings are string literals holding prose, so they survive the
		// literal scan and have to go the way a comment does.
		source = regexp.MustCompile(`(?s)"""".*?""""|"""".*?"""`).ReplaceAllString(source, "")
		source = regexp.MustCompile(`(?s)"""(?:[^"]|"[^"]|""[^"])*"""`).ReplaceAllString(source, "")
		source = regexp.MustCompile(`(?s)\x27\x27\x27(?:[^\x27]|\x27[^\x27]|\x27\x27[^\x27])*\x27\x27\x27`).ReplaceAllString(source, "")
	}
	out := make([]string, 0, 64)
	for _, l := range strings.Split(source, "\n") {
		if i := strings.Index(l, line); i >= 0 {
			l = l[:i]
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// servedBy accepts an exact route, and a *static* path that is the prefix of
// one.
//
// The prefix rule is for the base an SDK keeps in a constant and appends to:
// `JOBS_PATH = "/api/v1/app/jobs"` is not a call, it is the left-hand side of
// one, and the call site that completes it is matched on its own.
//
// **It applies only to a path with no interpolation in it**, and that
// restriction is the whole of the rule's safety. A path carrying a `*` has a
// value substituted into it, which makes it a call site rather than a base —
// and allowing prefixes there swallowed the defect this test was written for:
// `/api/v1/app/runs/*` is a prefix of the served `/api/v1/app/runs/*/logs`, so
// the R SDK reading a run from a route that does not exist matched a route that
// does, and the test passed against the bug.
func servedBy(routes map[string]bool, path string) bool {
	if routes[path] {
		return true
	}
	if strings.Contains(path, "*") {
		return false
	}
	for route := range routes {
		if strings.HasPrefix(route, path+"/") {
			return true
		}
	}
	return false
}

// appRoutes is every path registered under /api/v1/app, read out of router.go
// and reduced the same way an SDK's is.
func appRoutes(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "internal", "api", "router.go"))
	if err != nil {
		t.Fatalf("reading router.go: %v", err)
	}
	block := regexp.MustCompile(`(?s)r\.Route\("/api/v1/app",.*?\n\t\}\)`).Find(raw)
	if block == nil {
		t.Fatal("could not find the /api/v1/app route block in router.go")
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`r\.(?:Get|Post|Put|Patch|Delete)\("([^"]*)"`).
		FindAllStringSubmatch(string(block), -1) {
		out[normalizeSDKPath("/api/v1/app"+m[1])] = true
	}
	if len(out) < 10 {
		t.Fatalf("only %d routes found under /api/v1/app; the regex has stopped matching "+
			"and this test would pass by comparing against nothing", len(out))
	}
	return out
}

// normalizeSDKPath reduces one path — a client's or the router's — to the form
// the two compare in: every interpolated segment becomes `*`, and a trailing
// slash or query goes.
//
// Four languages spell interpolation four ways, so the reduction is by *shape*
// rather than syntax: a segment carrying `{`, `$` or `%` is a value, and a bare
// word is part of the route.
func normalizeSDKPath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimSuffix(path, "/")
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if strings.ContainsAny(seg, "{$%") {
			segments[i] = "*"
		}
	}
	return strings.Join(segments, "/")
}

func sourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Tests name paths to stub them, which is not the client calling
			// one; `build` and `dist` are copies of the source beside it, and
			// reporting the same defect twice from two paths helps nobody.
			switch d.Name() {
			case "test", "tests", "build", "dist", "__pycache__":
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".js", ".py", ".R":
			base := filepath.Base(path)
			if strings.HasSuffix(path, "_test.go") || strings.Contains(base, ".test.") ||
				strings.HasPrefix(base, "test_") {
				return nil
			}
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking sdk/%s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no source files under sdk/%s; this test would pass by reading nothing", dir)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// **And in R, the method too.**
//
// The test above compares paths, which is all four languages' common
// denominator: Go, Node and Python each reach the wire through a transport that
// takes the verb somewhere other than beside the path, and pairing them would
// mean following a call graph rather than reading a line.
//
// R is different — `toft_request("POST", "/api/v1/app/runs")` puts both in one
// call — and R is where the failure was. `POST /api/v1/app/runs` is a path that
// exists, registered for GET only and belonging to the *task* surface, so a
// path-only check reports nothing while every `job_start()` answers 405. That
// is the harder half of the same defect and this is the language it can be
// caught in cheaply.
func TestRSDKUsesAMethodEachRouteAccepts(t *testing.T) {
	routes := appRoutesByMethod(t)

	call := regexp.MustCompile(`toft_request\(\s*"([A-Z]+)"\s*,\s*((?:[^()]|\([^()]*\)|\((?:[^()]|\([^()]*\))*\))*)`)
	for _, file := range sourceFiles(t, "r") {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, m := range call.FindAllStringSubmatch(stripComments(".R", string(raw)), -1) {
			method, rest := m[1], m[2]
			for _, hit := range appPathsIn(".R", rest) {
				path := normalizeSDKPath(hit)
				if path == "" {
					continue
				}
				if allowed := routes[path]; len(allowed) > 0 && !allowed[method] {
					t.Errorf("%s sends %s %s, which is registered for %s only",
						file, method, path, strings.Join(sortedKeys(allowed), ", "))
				}
			}
		}
	}
}

// appRoutesByMethod is the same block read one level finer: path to the set of
// methods registered on it.
func appRoutesByMethod(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "internal", "api", "router.go"))
	if err != nil {
		t.Fatalf("reading router.go: %v", err)
	}
	block := regexp.MustCompile(`(?s)r\.Route\("/api/v1/app",.*?\n\t\}\)`).Find(raw)
	if block == nil {
		t.Fatal("could not find the /api/v1/app route block in router.go")
	}
	out := map[string]map[string]bool{}
	for _, m := range regexp.MustCompile(`r\.(Get|Post|Put|Patch|Delete)\("([^"]*)"`).
		FindAllStringSubmatch(string(block), -1) {
		path := normalizeSDKPath("/api/v1/app" + m[2])
		if out[path] == nil {
			out[path] = map[string]bool{}
		}
		out[path][strings.ToUpper(m[1])] = true
	}
	if len(out) < 10 {
		t.Fatalf("only %d routes found; the regex has stopped matching and this "+
			"test would pass by comparing against nothing", len(out))
	}
	return out
}

// The run's instant reaches every SDK under the name the runner exports. A
// language that spelled it differently would read nothing, every time, and a
// job written against it would treat every run as one nobody gave an instant —
// which is the one reading of the variable that can never be noticed.
func TestEverySDKReadsTheScheduledForTheRunnerExports(t *testing.T) {
	for _, file := range []string{
		filepath.Join("python", "toft", "client.py"),
		filepath.Join("node", "index.js"),
		filepath.Join("go", "env.go"),
		filepath.Join("r", "R", "env.R"),
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		if !strings.Contains(string(raw), runner.EnvScheduledFor) {
			t.Errorf("%s does not read %s", file, runner.EnvScheduledFor)
		}
	}
}
