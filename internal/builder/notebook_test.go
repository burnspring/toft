package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/appint"
	"toft/internal/kube"
	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/staticserve"
	"toft/internal/tags"
	"toft/internal/toolchain"
)

func TestDetectNotebookFromASingleIpynb(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "requirements.txt", "pandas\n")
	write(t, dir, "analysis.ipynb", "{}")

	got := DetectFramework(dir)
	if got.Framework != FrameworkNotebook {
		t.Fatalf("framework = %q, want %q", got.Framework, FrameworkNotebook)
	}
	if got.Entrypoint != "analysis.ipynb" {
		t.Errorf("entrypoint = %q, want analysis.ipynb", got.Entrypoint)
	}

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	if plan.Engine != EngineNBConvert {
		t.Errorf("engine = %q, want %q", plan.Engine, EngineNBConvert)
	}
}

// A notebook repository needs no requirements.txt: the platform installs the
// renderer itself, so the first deploy of a notebook that imports nothing must
// not fail on a dependency only the platform needs.
func TestDetectNotebookWithNoRequirements(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report.ipynb", "{}")

	if got := DetectFramework(dir); got.Framework != FrameworkNotebook {
		t.Fatalf("framework = %q, want %q", got.Framework, FrameworkNotebook)
	}
}

// A .qmd has one engine and it is not chosen by anybody.
func TestQmdIsAlwaysQuarto(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report.qmd", "---\ntitle: Weekly\n---\n")

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	if plan.Framework != FrameworkNotebook || plan.Engine != EngineQuarto {
		t.Errorf("plan = %+v, want notebook/quarto", plan)
	}
}

func TestIpynbTakesQuartoWhenDeclared(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "analysis.ipynb", "{}")
	write(t, dir, ManifestFile, "[notebook]\nengine = \"quarto\"\n")

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	if plan.Engine != EngineQuarto {
		t.Errorf("engine = %q, want %q", plan.Engine, EngineQuarto)
	}
}

// Several candidates and no declaration fails, naming all of them — the rule
// and the wording staticserve already uses for several candidate documents.
func TestSeveralNotebooksIsARefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "weekly.ipynb", "{}")
	write(t, dir, "monthly.ipynb", "{}")
	write(t, dir, "notes.qmd", "---\n---\n")

	_, err := ResolveNotebook(dir)
	if err == nil {
		t.Fatal("three notebooks resolved without a refusal")
	}
	for _, name := range []string{"weekly.ipynb", "monthly.ipynb", "notes.qmd"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal does not name %s: %v", name, err)
		}
	}
	if !strings.Contains(err.Error(), "[notebook]") {
		t.Errorf("refusal does not name the table to declare: %v", err)
	}

	// Detection still says notebook, so the deploy refuses with this reason
	// rather than with "unsupported framework".
	if got := DetectFramework(dir); got.Framework != FrameworkNotebook {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkNotebook)
	}
	if !strings.Contains(UnsupportedMessage(dir), "weekly.ipynb") {
		t.Errorf("the unsupported message does not carry the reason:\n%s", UnsupportedMessage(dir))
	}
}

func TestDeclaredNotebookFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "weekly.ipynb", "{}")
	write(t, dir, "monthly.ipynb", "{}")
	write(t, dir, "reports/deep.qmd", "---\n---\n")
	write(t, dir, ManifestFile, "[notebook]\nfile = \"reports/deep.qmd\"\n")

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	if plan.File != "reports/deep.qmd" {
		t.Errorf("file = %q, want reports/deep.qmd", plan.File)
	}
	if plan.Engine != EngineQuarto {
		t.Errorf("engine = %q, want quarto", plan.Engine)
	}
}

// The same table `checkRelative` is tested with, in this package's error type.
func TestNotebookFileValidation(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantMsg string
	}{
		{"absolute", "/etc/passwd.ipynb", "absolute"},
		{"escaping", "../other/report.ipynb", "leaves it"},
		{"nothing", ".", "must name something"},
		{"missing", "reports/absent.ipynb", "does not contain"},
		{"not a notebook", "script.py", "which is not"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "present.ipynb", "{}")
			write(t, dir, ManifestFile, "[notebook]\nfile = \""+tc.file+"\"\n")

			_, err := ResolveNotebook(dir)
			if err == nil {
				t.Fatalf("file = %q resolved without a refusal", tc.file)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("refusal = %v, want it to mention %q", err, tc.wantMsg)
			}
			if !strings.Contains(err.Error(), "[notebook] file") {
				t.Errorf("refusal does not name the key: %v", err)
			}
		})
	}
}

func TestEngineMustBeAName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "analysis.ipynb", "{}")
	write(t, dir, ManifestFile, "[notebook]\nengine = \"papermill\"\n")

	_, err := ResolveNotebook(dir)
	if err == nil {
		t.Fatal("an unknown engine resolved without a refusal")
	}
	if !strings.Contains(err.Error(), "papermill") || !strings.Contains(err.Error(), "nbconvert") {
		t.Errorf("refusal = %v, want it to name what was asked for and what exists", err)
	}
}

func TestNBConvertCannotBeDeclaredForAQmd(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report.qmd", "---\n---\n")
	write(t, dir, ManifestFile, "[notebook]\nengine = \"nbconvert\"\n")

	if _, err := ResolveNotebook(dir); err == nil {
		t.Fatal("nbconvert on a .qmd resolved without a refusal")
	}
}

func TestDetectMarimo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "requirements.txt", "polars\n")
	write(t, dir, "app.py", "import marimo as mo\n\napp = mo.App(width=\"medium\")\n\n@app.cell\ndef _():\n    return\n")

	got := DetectFramework(dir)
	if got.Framework != FrameworkMarimo {
		t.Fatalf("framework = %q, want %q", got.Framework, FrameworkMarimo)
	}
	if got.Entrypoint != "app.py" {
		t.Errorf("entrypoint = %q, want app.py", got.Entrypoint)
	}
}

func TestDetectMarimoUnaliasedImport(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notebook.py", "import marimo\n\napp = marimo.App()\n")

	if got := DetectFramework(dir); got.Framework != FrameworkMarimo {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkMarimo)
	}
}

// The rule somebody will later "fix": marimo in requirements.txt is not the
// trigger. Unlike Streamlit, marimo is a library people import into ordinary
// programs, and a Flask app that happens to depend on it stays a Flask app.
func TestMarimoDependencyAloneIsNotMarimo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "requirements.txt", "flask\nmarimo\n")
	write(t, dir, "app.py", "from flask import Flask\napp = Flask(__name__)\n")

	if got := DetectFramework(dir); got.Framework != FrameworkFlask {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkFlask)
	}
}

// Importing the library without constructing an App is a program that uses
// marimo, not a marimo notebook.
func TestMarimoImportWithoutAppIsNotMarimo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "requirements.txt", "marimo\n")
	write(t, dir, "app.py", "import marimo as mo\n\nprint(mo.__version__)\n")

	if got := DetectFramework(dir); got.Framework != FrameworkPython {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkPython)
	}
}

func TestSeveralMarimoNotebooksIsARefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "one.py", "import marimo\napp = marimo.App()\n")
	write(t, dir, "two.py", "import marimo\napp = marimo.App()\n")

	_, err := ResolveNotebook(dir)
	if err == nil {
		t.Fatal("two marimo notebooks resolved without a refusal")
	}
	if !strings.Contains(err.Error(), "one.py") || !strings.Contains(err.Error(), "two.py") {
		t.Errorf("refusal does not name both: %v", err)
	}
	if got := DetectFramework(dir); got.Framework != FrameworkMarimo {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkMarimo)
	}
}

// One of each is two apps, and the deploy says so rather than picking.
func TestMarimoBesideANotebookIsARefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "explore.py", "import marimo\napp = marimo.App()\n")
	write(t, dir, "report.ipynb", "{}")

	_, err := ResolveNotebook(dir)
	if err == nil {
		t.Fatal("a marimo notebook beside a .ipynb resolved without a refusal")
	}
	if !strings.Contains(err.Error(), "explore.py") || !strings.Contains(err.Error(), "report.ipynb") {
		t.Errorf("refusal does not name both: %v", err)
	}
}

func TestMarimoBesideANotebookTableIsARefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "explore.py", "import marimo\napp = marimo.App()\n")
	write(t, dir, ManifestFile, "[notebook]\nengine = \"quarto\"\n")

	if _, err := ResolveNotebook(dir); err == nil {
		t.Fatal("a marimo notebook beside a [notebook] table resolved without a refusal")
	}
}

// Everything else on the install is not a notebook, and answering that question
// must cost nothing and refuse nothing.
func TestOrdinaryReposAreNotNotebooks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "requirements.txt", "flask\n")
	write(t, dir, "app.py", "from flask import Flask\n")
	write(t, dir, ManifestFile, "[jobs.nightly]\ncommand = \"echo hi\"\n")

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook refused an ordinary repo: %v", err)
	}
	if plan != nil {
		t.Fatalf("plan = %+v, want nil", plan)
	}
}

// A notebook is a Python build in both halves, which is what decides the venv,
// the runner image family and whether a `[runtime] python` pin means anything.
func TestNotebookFrameworksAreAPythonBuild(t *testing.T) {
	for _, f := range []Framework{FrameworkNotebook, FrameworkMarimo} {
		if got := LanguageOf(f); got != "python" {
			t.Errorf("LanguageOf(%s) = %q, want python", f, got)
		}
	}
}

// A malformed toft.toml is refused, with its line number, by ParseManifest
// during the same deploy. Reading it here as "this is not a notebook" would
// mean a TOML typo reporting itself in the wrong vocabulary.
func TestMalformedManifestIsNotANotebookRefusal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report.ipynb", "{}")
	write(t, dir, ManifestFile, "[notebook\nfile = broken")

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	if plan == nil || plan.File != "report.ipynb" {
		t.Fatalf("plan = %+v, want the one notebook at the root", plan)
	}
	if _, err := ParseManifest(dir); err == nil {
		t.Error("ParseManifest accepted a malformed manifest, so nothing refuses it")
	}
}

// --- The three whose failure would be silent ---

// The render directory's name is written in three packages, because the two
// launchers must not import this one. If they drift, a notebook app serves a
// directory nothing wrote — a 404 with a green deploy behind it.
func TestRenderDirectoryNameIsTheSameEverywhere(t *testing.T) {
	if runtime.NotebookRenderDir != RenderDir {
		t.Errorf("runtime.NotebookRenderDir = %q, builder.RenderDir = %q",
			runtime.NotebookRenderDir, RenderDir)
	}
	// The isolated launcher spells it inline, in the one place a constant
	// cannot reach. Read out of the source rather than imported, the way
	// landing-frameworks.test.ts reads the detector.
	src, err := os.ReadFile(filepath.Join("..", "runner", "run.go"))
	if err != nil {
		t.Fatalf("reading the isolated launcher: %v", err)
	}
	if !strings.Contains(string(src), `"`+RenderDir+`"`) {
		t.Errorf("internal/runner/run.go does not mention %q", RenderDir)
	}
}

// Detection re-runs against the release directory at every wake from idle
// sleep, every crash restore and every boot — before the render exists and
// again after it does. A rule that read the *output* would answer `notebook` on
// the build and something else on the restart, and an app's framework would
// change overnight with nothing logged.
func TestDetectionIsStableAcrossTheRender(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  Framework
	}{
		{"ipynb", map[string]string{"analysis.ipynb": "{}", "requirements.txt": "pandas\n"}, FrameworkNotebook},
		{"qmd", map[string]string{"report.qmd": "---\n---\n"}, FrameworkNotebook},
		{"marimo", map[string]string{"nb.py": "import marimo\napp = marimo.App()\n"}, FrameworkMarimo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				write(t, dir, name, body)
			}

			before := DetectFramework(dir)
			if before.Framework != tc.want {
				t.Fatalf("framework = %q, want %q", before.Framework, tc.want)
			}

			// What a render leaves behind, plus what a build leaves behind: a
			// document root full of HTML, and a virtualenv. Either one, read as
			// a detection input, would change the answer.
			write(t, dir, filepath.Join(RenderDir, RenderEntry), "<html><body>report</body></html>")
			write(t, dir, filepath.Join(RenderDir, renderMarker), "written by toft\n")
			write(t, dir, filepath.Join(RenderDir, "figure-1.html"), "<html></html>")
			write(t, dir, filepath.Join("venv", "pyvenv.cfg"), "home = /usr/bin\n")

			after := DetectFramework(dir)
			if after != before {
				t.Errorf("detection changed across the render:\n  before %+v\n  after  %+v", before, after)
			}
		})
	}
}

// Every place the product switches on `app.framework`, both frameworks, so a
// site added later fails here rather than falling into a `default`.
//
// The two disagree at six of these, which is the whole argument for their being
// two frameworks rather than one with a mode key. The table is the
// specification; a row nobody filled in is a workload started as Next.js.
func TestFrameworkSwitchesAreExhaustive(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "analysis.ipynb", "{}")

	type answer struct{ notebook, marimo string }
	yes, no := "yes", "no"
	bool2 := func(b bool) string {
		if b {
			return yes
		}
		return no
	}

	sites := []struct {
		site string
		got  answer
		want answer
	}{
		{"builder.LanguageOf",
			answer{LanguageOf(FrameworkNotebook), LanguageOf(FrameworkMarimo)},
			answer{"python", "python"}},
		{"kube.languageFamily",
			answer{kube.LanguageFamilyForTest("notebook"), kube.LanguageFamilyForTest("marimo")},
			answer{"python", "python"}},
		{"builder.KeepsPrefix",
			answer{bool2(KeepsPrefix("notebook")), bool2(KeepsPrefix("marimo"))},
			answer{no, yes}},
		{"builder.NeedsTrailingSlash",
			answer{bool2(NeedsTrailingSlash("notebook")), bool2(NeedsTrailingSlash("marimo"))},
			answer{yes, yes}},
		{"appint.ViewerCapabilities",
			answer{
				bool2(len(appint.ViewerCapabilities(true, "private", "notebook")) > 0),
				bool2(len(appint.ViewerCapabilities(true, "private", "marimo")) > 0),
			},
			answer{no, yes}},
		{"runner.Unrunnable",
			answer{unrunnableReason("notebook", dir), unrunnableReason("marimo", dir)},
			answer{"venv", "venv"}},
		{"tags.FrameworkValue",
			answer{tagValue("notebook"), tagValue("marimo")},
			answer{"Notebook", "marimo"}},
	}

	for _, s := range sites {
		if s.got != s.want {
			t.Errorf("%s: got %+v, want %+v", s.site, s.got, s.want)
		}
	}

	// The two launchers and buildSteps are asserted where they live —
	// internal/runtime and internal/runner both have a notebook_test.go — and
	// installDependencies is covered by the render tests below. Naming them
	// here so the list in D9 can be read off one place.
}

func unrunnableReason(framework, dir string) string {
	broken, what := runner.Unrunnable(framework, dir)
	if !broken {
		return "runnable"
	}
	if strings.Contains(what, "virtual environment") {
		return "venv"
	}
	return what
}

func tagValue(framework string) string {
	v, _, ok := tags.FrameworkValue(framework)
	if !ok {
		return "unknown"
	}
	return v
}

// --- The render ---

// A directory of that name that the author committed fails the deploy by name.
// Overwriting one somebody put in their repository is worse than refusing it.
func TestACommittedRenderDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report.ipynb", "{}")
	write(t, dir, filepath.Join(RenderDir, "notes.txt"), "mine\n")

	err := prepareRenderDir(filepath.Join(dir, RenderDir))
	if err == nil {
		t.Fatal("a committed toft-render/ was accepted")
	}
	if !strings.Contains(err.Error(), RenderDir) {
		t.Errorf("refusal does not name the directory: %v", err)
	}
	if !fileExists(filepath.Join(dir, RenderDir, "notes.txt")) {
		t.Error("the author's own file was removed by a refusal")
	}
}

// A repair rebuilds a release in place, and the tree it rebuilds already holds
// the render this same code produced. Refusing that would make every notebook
// app unrepairable, which is why the marker exists.
func TestThePlatformsOwnRenderDirectoryIsReplaced(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, filepath.Join(RenderDir, RenderEntry), "<html>old</html>")
	write(t, dir, filepath.Join(RenderDir, renderMarker), "written by toft\n")

	if err := prepareRenderDir(filepath.Join(dir, RenderDir)); err != nil {
		t.Fatalf("prepareRenderDir: %v", err)
	}
	if fileExists(filepath.Join(dir, RenderDir, RenderEntry)) {
		t.Error("the previous render was built over rather than replaced")
	}
}

// Refused by name, never quietly substituted: an author who asked for Quarto
// asked for a different document, and rendering it with nbconvert would deploy
// something else under the same name.
func TestQuartoAbsentIsRefusedByName(t *testing.T) {
	if toolchain.HasQuarto() {
		t.Skip("this host has Quarto, so the absent case cannot be observed here")
	}
	for _, tc := range []struct{ name, file, manifest string }{
		{"declared", "analysis.ipynb", "[notebook]\nengine = \"quarto\"\n"},
		{"by extension", "report.qmd", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.file, "{}")
			if tc.manifest != "" {
				write(t, dir, ManifestFile, tc.manifest)
			}
			plan, err := ResolveNotebook(dir)
			if err != nil {
				t.Fatalf("ResolveNotebook: %v", err)
			}
			if plan.Engine != EngineQuarto {
				t.Fatalf("engine = %q, want quarto", plan.Engine)
			}
			_, _, _, err = renderCommand(plan, dir)
			if err == nil {
				t.Fatal("a Quarto render was built on a host with no Quarto")
			}
			if !strings.Contains(err.Error(), toolchain.QuartoBinEnv) {
				t.Errorf("refusal does not name %s: %v", toolchain.QuartoBinEnv, err)
			}
		})
	}
}

// An .ipynb with nothing declared renders on an install with no Quarto. The
// engine is refused, not the framework.
func TestNBConvertIsUntouchedByQuartosAbsence(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "analysis.ipynb", "{}")

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	name, args, _, err := renderCommand(plan, dir)
	if err != nil {
		t.Fatalf("renderCommand: %v", err)
	}
	if name != "venv/bin/python" {
		t.Errorf("command = %q, want the release's own interpreter", name)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-m nbconvert", "--to html", "--template lab", "--execute",
		"--output " + RenderEntry, "--output-dir " + RenderDir, "analysis.ipynb"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args = %q, want it to contain %q", joined, want)
		}
	}
}

// Names, never values. The build log is read by whoever deployed the app, and
// the list is the honest statement of what the document may end up carrying.
func TestTheRenderSaysWhichVariablesItExported(t *testing.T) {
	var lines []string
	lg := buildLog{
		step:   func(s string) { lines = append(lines, s) },
		output: func(string) {},
	}
	reportRenderEnv(map[string]string{"DATABASE_URL": "postgres://u:hunter2@db/x", "REGION": "eu"}, lg)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "DATABASE_URL, REGION") {
		t.Errorf("the names are not reported:\n%s", joined)
	}
	if strings.Contains(joined, "hunter2") || strings.Contains(joined, "postgres://") {
		t.Errorf("a value reached the build log:\n%s", joined)
	}
	if !strings.Contains(joined, "published") {
		t.Errorf("the log does not say the output is published:\n%s", joined)
	}
}

// The install's figure, and the compiled-in ten minutes when there is nothing
// to read it from — a settings read that fails must not fail a deploy.
func TestRenderTimeoutFallsBackToTheBuildDefault(t *testing.T) {
	b := &Builder{}
	if got := b.notebookRenderTimeout(); got != commandTimeout {
		t.Errorf("timeout = %s, want the build default %s", got, commandTimeout)
	}
}

// The whole of a notebook build, against the real renderer.
//
// Skipped where uv or a network is missing, because it installs nbconvert into
// a venv and executes a notebook — but where it can run, it is the only test
// that proves the four decisions together: the platform installs the renderer,
// the render reads the app's environment, the output lands in the directory the
// launchers point at, and the release's own files stay out of it.
func TestARealNotebookRendersEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: this installs packages and executes a notebook")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("no uv on PATH")
	}

	dir := t.TempDir()
	write(t, dir, "requirements.txt", "")
	write(t, dir, "report.ipynb", notebookReadingEnv("TOFT_TEST_SECRET"))

	b := &Builder{opts: Options{}}
	var log []string
	lg := buildLog{
		step:   func(s string) { log = append(log, s) },
		output: func(s string) { log = append(log, s) },
	}

	env := map[string]string{"TOFT_TEST_SECRET": "the-value-from-the-environment"}
	buildEnv := map[string]string{"TOFT_TEST_SECRET": env["TOFT_TEST_SECRET"]}

	if err := b.runCommand("", dir, "uv", []string{"venv", "venv"}, buildEnv, lg.output); err != nil {
		t.Skipf("uv venv failed, so there is no interpreter to render with: %v", err)
	}
	b.installRenderPackages("", FrameworkNotebook, dir, buildEnv, lg)
	if !fileExists(filepath.Join(dir, "venv", "bin", "python")) {
		t.Skip("no venv interpreter")
	}

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	if err := b.renderNotebook("", plan, dir, env, buildEnv, lg); err != nil {
		t.Fatalf("renderNotebook: %v\n%s", err, strings.Join(log, "\n"))
	}

	body, err := os.ReadFile(filepath.Join(dir, RenderDir, RenderEntry))
	if err != nil {
		t.Fatalf("reading the render: %v", err)
	}
	// The point of the feature: the notebook executed, and it executed with the
	// app's environment. If this line ever fails, a report cannot read a
	// database URL and the framework is a screenshot.
	if !strings.Contains(string(body), "the-value-from-the-environment") {
		t.Error("the rendered document does not carry what the notebook read out of the environment")
	}

	// And the release's own files are not in the document root.
	entries, err := os.ReadDir(filepath.Join(dir, RenderDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "requirements.txt", "report.ipynb", "venv":
			t.Errorf("%s is inside the document root", e.Name())
		}
	}

	// Detection is unchanged by everything that just happened.
	if got := DetectFramework(dir); got.Framework != FrameworkNotebook || got.Entrypoint != "report.ipynb" {
		t.Errorf("after the render, detection = %+v", got)
	}

	// And a second render over the platform's own output is allowed, which is
	// what a repair and a re-deploy both need.
	if err := b.renderNotebook("", plan, dir, env, buildEnv, lg); err != nil {
		t.Errorf("re-rendering over the platform's own output: %v", err)
	}
}

// notebookReadingEnv is the smallest .ipynb that proves the environment
// reached the kernel: one code cell that prints a variable.
func notebookReadingEnv(name string) string {
	return `{
 "cells": [
  {
   "cell_type": "code",
   "execution_count": null,
   "metadata": {},
   "outputs": [],
   "source": ["import os\n", "print(os.environ.get(\"` + name + `\", \"absent\"))"]
  }
 ],
 "metadata": {
  "kernelspec": {"display_name": "Python 3", "language": "python", "name": "python3"},
  "language_info": {"name": "python", "version": "3.11.0"}
 },
 "nbformat": 4,
 "nbformat_minor": 5
}`
}

// A .qmd, against the real Quarto, on a host that has one.
//
// Two things it proves that no unit test can. Quarto's output is a *directory*
// — index.html plus a report_files/ of CSS and JS — unlike nbconvert's single
// self-contained file, and that only works because the document root is the
// render directory rather than one file inside it. And QUARTO_PYTHON reaches
// the kernel: without it Quarto would run `{python}` cells through whatever
// python3 is on PATH, which is the *platform's* interpreter, so a report
// importing anything the app declared would fail on an import that is installed.
func TestARealQuartoDocumentRendersEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: this installs packages and renders a document")
	}
	if !toolchain.HasQuarto() {
		t.Skip("no Quarto on this host")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("no uv on PATH")
	}

	dir := t.TempDir()
	write(t, dir, "requirements.txt", "")
	write(t, dir, "report.qmd", "---\ntitle: \"Weekly\"\nformat: html\n---\n\n"+
		"```{python}\nimport os, sys\n"+
		"print(os.environ.get(\"TOFT_TEST_SECRET\", \"absent\"))\n"+
		"print(sys.executable)\n```\n")

	b := &Builder{opts: Options{}}
	var log []string
	lg := buildLog{
		step:   func(s string) { log = append(log, s) },
		output: func(s string) { log = append(log, s) },
	}

	env := map[string]string{"TOFT_TEST_SECRET": "the-value-from-the-environment"}
	buildEnv := map[string]string{"TOFT_TEST_SECRET": env["TOFT_TEST_SECRET"]}

	if err := b.runCommand("", dir, "uv", []string{"venv", "venv"}, buildEnv, lg.output); err != nil {
		t.Skipf("uv venv failed: %v", err)
	}
	b.installRenderPackages("", FrameworkNotebook, dir, buildEnv, lg)

	plan, err := ResolveNotebook(dir)
	if err != nil {
		t.Fatalf("ResolveNotebook: %v", err)
	}
	if plan.Engine != EngineQuarto {
		t.Fatalf("engine = %q, want quarto", plan.Engine)
	}
	if err := b.renderNotebook("", plan, dir, env, buildEnv, lg); err != nil {
		t.Fatalf("renderNotebook: %v\n%s", err, strings.Join(log, "\n"))
	}

	body, err := os.ReadFile(filepath.Join(dir, RenderDir, RenderEntry))
	if err != nil {
		t.Fatalf("reading the render: %v", err)
	}
	if !strings.Contains(string(body), "the-value-from-the-environment") {
		t.Error("the rendered document does not carry what the notebook read out of the environment")
	}
	// The release's own interpreter, not the platform's.
	if !strings.Contains(string(body), filepath.Join(dir, "venv")) {
		t.Errorf("the cells ran under an interpreter other than the release's venv:\n%s",
			firstLines(string(body), 0))
	}

	// The whole render directory is the document root, and staticserve resolves
	// it with no declaration anywhere — which is what makes Quarto's sidecar
	// assets work at all.
	cfg, err := staticserve.Resolve(filepath.Join(dir, RenderDir))
	if err != nil {
		t.Fatalf("the render directory does not resolve as a static release: %v", err)
	}
	if cfg.Entry != RenderEntry {
		t.Errorf("entry = %q, want %q", cfg.Entry, RenderEntry)
	}
	if cfg.Files < 2 {
		t.Errorf("the document root holds %d files; Quarto's sidecar assets are missing", cfg.Files)
	}
}

// firstLines is for a failure message: a rendered document is a megabyte and
// none of it after the first screen explains anything.
func firstLines(s string, _ int) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// The one list that is written twice, because the runner binary must not
// import the builder.
//
// The two build paths must produce the same tree — an app that behaves
// differently depending on where it happened to be built is the failure both
// paths exist to prevent — and this is the only place a notebook release's venv
// can come apart. It is the same guard the SDK's `Kept in step with` comment
// asks for and never got.
func TestBothBuildPathsInstallTheSameRenderPackages(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "runner", "run.go"))
	if err != nil {
		t.Fatalf("reading the isolated build path: %v", err)
	}
	body := string(src)

	for f, pkgs := range renderPackages {
		for _, pkg := range pkgs {
			if !strings.Contains(body, `"`+pkg+`"`) {
				t.Errorf("internal/builder installs %q for a %s release and internal/runner "+
					"does not: a repaired venv would be missing it", pkg, f)
			}
		}
	}
}

// A shared-mode repair rebuilds the venv and leaves the document alone.
//
// The counterpart of internal/runner's TestRepairingANotebookDoesNotReRender,
// from the side that shares `installDependencies` with the deploy. A venv is
// bound to an absolute interpreter path and dangles when that path moves; a
// rendered document is a file and dangles from nothing. Re-executing every cell
// on a restart would also be a re-render *outside a deploy*, whose output the
// next start throws away — a start replaces the tree.
func TestRepairingANotebookLeavesTheRenderedDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: this rebuilds a virtualenv")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("no uv on PATH")
	}

	dir := t.TempDir()
	write(t, dir, "requirements.txt", "")
	write(t, dir, "report.ipynb", "{}")
	write(t, dir, filepath.Join(RenderDir, RenderEntry), "<html>the deployed document</html>")
	write(t, dir, filepath.Join(RenderDir, renderMarker), "written by toft\n")

	// A venv whose interpreter is gone: exactly what a platform image upgrade
	// leaves behind, and the only thing a repair is for.
	if err := os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/python3.11", filepath.Join(dir, "venv", "bin", "python")); err != nil {
		t.Fatal(err)
	}

	spec := runtime.WorkloadSpec{
		AppID: "app-1", Slug: "report", Name: "report-abc123",
		Framework: string(FrameworkNotebook), Entrypoint: "report.ipynb",
		BasePath: "/apps/report", Release: runtime.LocalRelease(dir),
		Env: map[string]string{},
	}
	if broken, _ := runner.Unrunnable(spec.Framework, dir); !broken {
		t.Fatal("a dangling venv was called runnable, so this asserts nothing")
	}
	if err := (&Builder{}).Repair(spec, dir); err != nil {
		t.Fatalf("Repair: %v", err)
	}

	if !fileExists(filepath.Join(dir, "venv", "bin", "python")) {
		t.Error("the venv was not rebuilt")
	}
	body, err := os.ReadFile(filepath.Join(dir, RenderDir, RenderEntry))
	if err != nil {
		t.Fatalf("the rendered document did not survive the repair: %v", err)
	}
	if string(body) != "<html>the deployed document</html>" {
		t.Errorf("the document was re-rendered by a repair: %q", body)
	}
}
