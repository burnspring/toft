package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"toft/cli/internal/bundle"

	"toft/cli/internal/client"
)

func inDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The language is read from the tree the way the platform's own detector reads
// it, so nobody has to name what the build works out for itself.
func TestTheLanguageIsDetectedFromTheTree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"go", map[string]string{"go.mod": "module m\n"}, "go"},
		{"node", map[string]string{"package.json": "{}"}, "node"},
		{"python requirements", map[string]string{"requirements.txt": "flask\n"}, "python"},
		{"python pyproject", map[string]string{"pyproject.toml": "[project]\n"}, "python"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := detectLanguage(inDir(t, tc.files))
			if err != nil || got != tc.want {
				t.Errorf("detectLanguage = %q, %v — want %q", got, err, tc.want)
			}
		})
	}
}

// A tree that is two things at once is not guessed at. A Go service with a
// bundled frontend is the ordinary case, and picking one silently is how
// somebody vendors the wrong SDK and then reads a compile error about the other.
func TestAnAmbiguousOrSilentTreeIsRefusedWithTheFlagThatFixesIt(t *testing.T) {
	both := inDir(t, map[string]string{"go.mod": "module m\n", "package.json": "{}"})
	_, err := detectLanguage(both)
	if err == nil || !strings.Contains(err.Error(), "--lang") {
		t.Errorf("a mixed tree gave %v, want a refusal naming --lang", err)
	}
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Errorf("err = %T, want a usage error so the exit code is 2", err)
	}

	if _, err := detectLanguage(inDir(t, nil)); err == nil {
		t.Error("an empty tree was detected as something")
	}
}

// A path is the one part of the install's answer that names a location on this
// machine. The install is trusted and the check is here anyway: a client that
// writes wherever it is told is a client a compromised install can write through.
func TestAPathThatWouldEscapeTheTargetIsRefused(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../escaped.go", "/etc/passwd", "a/../../out.go"} {
		if _, err := writeSDKFiles(root, ".toft/sdk-go", map[string]string{path: "x"}); err == nil {
			t.Errorf("writeSDKFiles accepted %q", path)
		}
	}

	written, err := writeSDKFiles(root, ".toft/sdk-go", map[string]string{
		"go.mod": "module toft\n", "toft.go": "package toft\n",
	})
	if err != nil || written != 2 {
		t.Fatalf("writeSDKFiles = %d, %v", written, err)
	}
	body, err := os.ReadFile(filepath.Join(root, ".toft", "sdk-go", "toft.go"))
	if err != nil || string(body) != "package toft\n" {
		t.Errorf("toft.go = %q, %v", body, err)
	}
}

// A workspace may not declare less than a module it uses, and it uses two: the
// app's, and the SDK sitting at the floor the install reported.
func TestTheWorkspaceDeclaresTheHigherDirectiveAndAddsNoRequire(t *testing.T) {
	for _, tc := range []struct{ goMod, floor, want string }{
		{"module m\n\ngo 1.24.0\n", "1.21", "go 1.24.0"},
		{"module m\n\ngo 1.19\n", "1.21", "go 1.21"},
		{"module m\n", "1.21", "go 1.21"},
		{"", "1.21", "go 1.21"}, // no go.mod at all
	} {
		files := map[string]string{}
		if tc.goMod != "" {
			files["go.mod"] = tc.goMod
		}
		root := inDir(t, files)

		note, err := writeGoWorkspace(root, ".toft/sdk-go", tc.floor)
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(root, "go.work"))
		if err != nil {
			t.Fatal(err)
		}
		got := string(body)
		if !strings.Contains(got, tc.want) {
			t.Errorf("go.work for %q = %q, want %q", tc.goMod, got, tc.want)
		}
		if !strings.Contains(got, "./.toft/sdk-go") {
			t.Errorf("go.work = %q, want a relative use — an absolute one breaks for the next clone", got)
		}
		if !strings.Contains(note, "No require line") {
			t.Errorf("note = %q, want it to say the app's go.mod was not touched", note)
		}
		// And the app's own manifest is untouched.
		if tc.goMod != "" {
			after, _ := os.ReadFile(filepath.Join(root, "go.mod"))
			if string(after) != tc.goMod {
				t.Errorf("go.mod = %q, want it byte-identical", after)
			}
		}
	}
}

// A workspace an app wrote is a file with its own reasons — a second module in
// the repository, most often — and merging into it by hand is how a client
// corrupts one.
func TestAnExistingWorkspaceIsLeftAloneWithTheCommandThatFixesIt(t *testing.T) {
	root := inDir(t, map[string]string{
		"go.mod":  "module m\n\ngo 1.24.0\n",
		"go.work": "go 1.24.0\n\nuse (\n\t.\n\t./svc\n)\n",
	})
	note, err := writeGoWorkspace(root, ".toft/sdk-go", "1.21")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "go work use ./.toft/sdk-go") {
		t.Errorf("note = %q, want the command that adds it", note)
	}
	body, _ := os.ReadFile(filepath.Join(root, "go.work"))
	if !strings.Contains(string(body), "./svc") {
		t.Errorf("go.work = %q, want the app's own workspace untouched", body)
	}

	// Run twice: one that already names it says so and does nothing.
	again := inDir(t, map[string]string{
		"go.work": "go 1.24.0\n\nuse (\n\t.\n\t./.toft/sdk-go\n)\n",
	})
	note, err = writeGoWorkspace(again, ".toft/sdk-go", "1.21")
	if err != nil || !strings.Contains(note, "already uses it") {
		t.Errorf("note = %q, %v — want the idempotent answer", note, err)
	}
}

// The one arrangement that builds here and fails on the platform: a workspace
// that ships naming a directory that does not.
func TestIgnoringOnlyOneOfThePairIsWarnedAbout(t *testing.T) {
	trap := inDir(t, map[string]string{".gitignore": ".toft/\n"})
	warning := shippingWarning(trap, ".toft", "go")
	if !strings.Contains(warning, "did not ship") {
		t.Errorf("warning = %q, want the case that fails on the platform named", warning)
	}

	harmless := inDir(t, map[string]string{".gitignore": "go.work\n"})
	if w := shippingWarning(harmless, ".toft", "go"); !strings.Contains(w, "harmless") {
		t.Errorf("warning = %q, want the reverse case described as harmless", w)
	}

	for _, gitignore := range []string{"", ".toft/\ngo.work\n"} {
		quiet := inDir(t, map[string]string{".gitignore": gitignore})
		if w := shippingWarning(quiet, ".toft", "go"); w != "" {
			t.Errorf("gitignore %q warned %q, want silence — both or neither is fine", gitignore, w)
		}
	}

	// Only Go has a workspace to get wrong.
	if w := shippingWarning(trap, ".toft", "python"); w != "" {
		t.Errorf("python warned %q", w)
	}
}

// Not semver: "1.21" and "1.21.0" are the same version, and a missing field is
// zero. Stated here as well as in the platform's sdk package, which is the
// module boundary this comparison sits either side of.
func TestGoVersionsCompareFieldByField(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.21", "1.21.0", 0},
		{"1.21", "1.22", -1},
		{"1.24.0", "1.21", 1},
		{"1.9", "1.10", -1},
		{"", "1.21", -1},
	} {
		if got := compareGoVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if got := goDirectiveOf("module m\n\n// go 1.99\ngo 1.24.0\n"); got != "1.24.0" {
		t.Errorf("goDirectiveOf = %q — a comment is not a directive", got)
	}
}

// The next step is the whole point of the two languages that have no workspace:
// a directory nobody installs is a directory that changes nothing.
func TestPythonAndNodeAreToldHowToInstallWhatLanded(t *testing.T) {
	python := strings.Join(sdkNextSteps("python", ".toft/sdk-python"), "\n")
	if !strings.Contains(python, "pip install -e .toft/sdk-python") {
		t.Errorf("python next steps = %q", python)
	}
	node := strings.Join(sdkNextSteps("node", ".toft/sdk-node"), "\n")
	if !strings.Contains(node, `"toft-sdk": "file:.toft/sdk-node"`) {
		t.Errorf("node next steps = %q", node)
	}
	if steps := sdkNextSteps("go", ".toft/sdk-go"); len(steps) != 0 {
		t.Errorf("go next steps = %v, want none — the workspace already did it", steps)
	}
}

// A language this install does not carry is a flag the caller got wrong, and
// the exit code is the only part of that a pipeline reads.
func TestAnUnknownLanguageIsAUsageErrorRatherThanAFailedRun(t *testing.T) {
	err := sdkLanguageError(&client.Error{
		Status: 404, Code: "sdk_language_unknown",
		Message: "no SDK for ruby. This install carries: go, node, python",
	})
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("err = %T, want a usage error so the exit code is 2", err)
	}
	if !strings.Contains(err.Error(), "go, node, python") {
		t.Errorf("err = %q, want the install's own list carried through", err)
	}
	// The hint names a command that exists (L6).
	if usage.from == nil || !strings.Contains(usage.from.Hint, "toft sdk vendor --lang python") {
		t.Errorf("hint = %+v", usage.from)
	}

	// Everything else stays what it was: a 403 is a credential problem and must
	// keep exiting 3, not become a typo.
	other := &client.Error{Status: 403, Code: "forbidden", Message: "no"}
	if got := sdkLanguageError(other); got != error(other) {
		t.Errorf("sdkLanguageError rewrote %v", got)
	}
}

// R is detected, and it beats Python rather than colliding with it.
//
// The precedence is the platform's, not a preference: detectR runs before
// detectPython in internal/builder, so an app.R beside a requirements.txt is an
// R app when it is built. Refusing it here as ambiguous would have refused the
// commonest R repository shape — an R app whose data step is a Python script —
// and told the developer to pick, with the platform's answer already decided.
func TestAnRTreeIsDetectedAndBeatsPython(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"app.R":              {"app.R": "library(shiny)\n"},
		"plumber.R":          {"plumber.R": "#* @get /\n"},
		"ui.R + server.R":    {"ui.R": "x", "server.R": "y"},
		"renv.lock alone":    {"renv.lock": "{}"},
		"an .Rmd":            {"report.Rmd": "---\ntitle: x\n---\n"},
		"a .qmd":             {"report.qmd": "---\ntitle: x\n---\n"},
		"R beside python":    {"app.R": "library(shiny)\n", "requirements.txt": "pandas\n"},
		"renv beside python": {"renv.lock": "{}", "pyproject.toml": "[project]\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := detectLanguage(inDir(t, files))
			if err != nil || got != "r" {
				t.Errorf("detectLanguage = %q, %v — want \"r\"", got, err)
			}
		})
	}

	// A lone ui.R is somebody part-way through, not an R app — the same
	// judgement internal/builder's detectShiny makes.
	if _, err := detectLanguage(inDir(t, map[string]string{"ui.R": "x"})); err == nil {
		t.Error("a lone ui.R was detected as an R app")
	}
}

// A vendored Python SDK is for the developer's environment: it goes into
// .gitignore, once, so the bundle the next deploy builds does not carry it.
func TestAVendoredPythonSDKIsIgnoredOnce(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".toft", "sdk-python", "toft"), 0o755)
	os.WriteFile(filepath.Join(dir, ".toft", "sdk-python", "toft", "__init__.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("x"), 0o644)

	line, err := ignoreVendored(dir, filepath.Join(".toft", "sdk-python"))
	if err != nil || line != "/.toft/sdk-python/" {
		t.Fatalf("ignoreVendored = %q, %v", line, err)
	}
	again, err := ignoreVendored(dir, filepath.Join(".toft", "sdk-python"))
	if err != nil || again != "" {
		t.Errorf("a second vendor added %q again", again)
	}
	body, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if !strings.HasPrefix(string(body), "*.log\n") || strings.Count(string(body), "/.toft/sdk-python/") != 1 {
		t.Errorf(".gitignore is now %q", body)
	}
	built, err := bundle.Build(dir, bundle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(built.Path)
	for _, f := range built.Files {
		if strings.HasPrefix(f.Rel, ".toft/") {
			t.Errorf("the bundle carries %s", f.Rel)
		}
	}
	if out, _ := ignoreVendored(dir, "../elsewhere"); out != "" {
		t.Errorf("a directory outside the tree was ignored: %q", out)
	}
}
