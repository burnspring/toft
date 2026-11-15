package builder

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/staticserve"
)

// --- detection ---------------------------------------------------------------

func TestDetectStatic(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<h1>hi</h1>")
	write(t, dir, "styles.css", "body{}")

	got := DetectFramework(dir)

	if got.Framework != FrameworkStatic {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkStatic)
	}
	if got.Entrypoint != "index.html" {
		t.Errorf("entrypoint = %q, want index.html", got.Entrypoint)
	}
	if got.HasBuild {
		t.Error("a static app has nothing to build")
	}
}

// The trap the whole ordering exists for.
//
// A Vite or CRA repo that fails to deploy today has an index.html at its root
// holding `<div id="root">` and a script tag pointing at TypeScript source.
// Detected as static it would deploy *successfully* and serve a blank page
// forever, with a build log full of green ticks — which is worse than the
// failure it gets today.
func TestAProgramIsNeverDetectedAsStatic(t *testing.T) {
	cases := map[string]string{
		"package.json":     `{"devDependencies":{"vite":"5.0.0"},"scripts":{"build":"vite build"}}`,
		"requirements.txt": "pandas\n",
		"pyproject.toml":   "[project]\nname='x'\n",
		"Pipfile":          "[packages]\n",
		"go.mod":           "module x\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "index.html", `<div id="root"></div><script src="/src/main.tsx"></script>`)
			write(t, dir, name, body)

			if got := DetectFramework(dir); got.Framework == FrameworkStatic {
				t.Fatalf("a repo with a %s was detected as static", name)
			}
		})
	}
}

// …and the refusal names what ruled static out, so the answer is not "add an
// index.html" to somebody whose index.html is already there.
func TestTheUnsupportedMessageNamesTheDisqualifier(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<div id=root></div>")
	write(t, dir, "package.json", `{"devDependencies":{"vite":"5.0.0"}}`)

	msg := UnsupportedMessage(dir)
	for _, want := range []string{"package.json", "[static]", "root ="} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
}

// The declaration overrides the disqualifier: a repo that commits its build
// output knows something the detector cannot, which is what the manifest is for.
func TestADeclarationOverridesTheDisqualifier(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"devDependencies":{"vite":"5.0.0"},"scripts":{"build":"vite build"}}`)
	write(t, dir, "src/main.tsx", "// source")
	write(t, dir, "dist/index.html", "<h1>built</h1>")
	write(t, dir, staticserve.ManifestFile, "[static]\nroot = \"dist\"\n")

	got := DetectFramework(dir)
	if got.Framework != FrameworkStatic {
		t.Fatalf("framework = %q, want static — the declaration is the override", got.Framework)
	}
	if got.Entrypoint != "index.html" {
		t.Errorf("entrypoint = %q, want the one inside dist/", got.Entrypoint)
	}
}

// A tree with no HTML in it is not a static site; it is an unsupported
// repository, and the message it already gets is the better one.
func TestAFolderWithNoDocumentIsNotStatic(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.txt", "hello")

	if got := DetectFramework(dir); got.Framework != FrameworkUnsupported {
		t.Errorf("framework = %q, want unsupported", got.Framework)
	}
}

// --- the entry survives a wake, in both execution modes ----------------------

// The regression that would otherwise be found by a person whose front page
// quietly changed overnight.
//
// `Builder.Start` re-runs DetectFramework against the release directory at every
// wake from idle sleep, every restore after a crash and every platform boot —
// deliberately, so a restore stays honest about a release built before the
// entrypoint columns existed. If `[static] entry` were read only by the builder
// and written to the app row, that re-detection would overwrite it with
// index.html and the declared entry would silently revert.
//
// Both modes are asserted because they answer the question by different routes:
// shared mode re-detects from the directory it still has, and an isolated pod
// has no platform filesystem at all, so the bootstrap answers from the app row
// that detection wrote. Both must land on the declared file, and so must the
// workload itself, which is the only one of the three a visitor sees.
func TestADeclaredEntrySurvivesAWakeInBothModes(t *testing.T) {
	release := t.TempDir()
	write(t, release, "index.html", "the wrong front page")
	write(t, release, "report.html", "the declared front page")
	write(t, release, staticserve.ManifestFile, "[static]\nentry = \"report.html\"\n")

	const want = "report.html"

	// Deploy time. This is what lands in apps.entrypoint.
	built := DetectFramework(release)
	if built.Framework != FrameworkStatic || built.Entrypoint != want {
		t.Fatalf("at build: framework %q entry %q, want static/%s",
			built.Framework, built.Entrypoint, want)
	}

	// Shared mode, waking: Start's re-detection, against the same
	// directory, must not disagree with the build.
	if woken := DetectFramework(release); woken.Entrypoint != want {
		t.Errorf("after a shared-mode wake the entry reverted to %q", woken.Entrypoint)
	}

	// Isolated mode, waking: the pod is recreated and its bootstrap is rendered
	// from the app row, which carries what detection persisted.
	rowEntrypoint := built.Entrypoint
	if rowEntrypoint != want {
		t.Errorf("the bootstrap would answer %q", rowEntrypoint)
	}

	// And what is actually served, in either mode: both launchers hand the
	// release directory to the same resolver, so this is the answer a visitor
	// gets — and the reason none of the above can drift apart from it.
	cfg, err := staticserve.Resolve(release)
	if err != nil {
		t.Fatalf("the workload could not resolve the release: %v", err)
	}
	if cfg.Entry != want {
		t.Errorf("the workload serves %q at the mount path, want %s", cfg.Entry, want)
	}
}

// --- the build step ----------------------------------------------------------

func TestStaticBuildStepNamesWhatBecamePublic(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dist/index.html", `<link href="/styles.css">`)
	write(t, dir, "dist/styles.css", "body{}")
	write(t, dir, "src/secret.js", "// not published")
	write(t, dir, staticserve.ManifestFile, "[static]\nroot = \"dist\"\n")

	var log []string
	if err := describeStaticRelease(dir, func(s string) { log = append(log, s) }); err != nil {
		t.Fatalf("describeStaticRelease: %v", err)
	}
	joined := strings.Join(log, "\n")

	for _, want := range []string{"dist/", "2 files", "index.html"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the log does not say %q:\n%s", want, joined)
		}
	}
	// The lint, in the log, at the moment the author is looking at the log.
	if !strings.Contains(joined, "/styles.css") {
		t.Errorf("the root-absolute URL was not warned about:\n%s", joined)
	}
}

// A declaration that is right about intent and wrong about this release fails
// the deploy naming the directory, rather than deploying an app whose front
// page is a 404 forever.
func TestStaticBuildStepRefusesAMissingRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<h1>source</h1>")
	write(t, dir, staticserve.ManifestFile, "[static]\nroot = \"dist\"\n")

	err := describeStaticRelease(dir, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "dist") {
		t.Fatalf("err = %v, want the missing directory named", err)
	}
}

// A static app runs no commands, so a declared job on one is refused rather
// than accepted and left to fail at 3am.
//
// Not a policy choice dressed up as a limitation: the static family image is an
// empty base with no /bin/sh in it, and a job's command runs as `/bin/sh -c`.
// There is no runtime in which the command could mean anything.
func TestJobsDeclaredOnAStaticAppFailTheDeploy(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<h1>site</h1>")
	write(t, dir, staticserve.ManifestFile,
		"[static]\n\n[jobs.nightly]\ncommand = \"python -m reports\"\n")

	app := &database.App{ID: "a1", Slug: "site", Framework: string(FrameworkStatic)}

	// No database is reached: the refusal comes before anything is recorded,
	// which is what makes a zero-value Builder enough here.
	_, err := (&Builder{}).checkManifest(app, nil, dir, func(string) {})
	if err == nil {
		t.Fatal("a static app accepted a declared job")
	}
	for _, want := range []string{"nightly", "no commands"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %s", want, err)
		}
	}

	// The manifest itself is well formed — this is about the framework, not
	// about the file, and the same declaration on an app with a runtime is a
	// perfectly ordinary job.
	defs, err := ParseManifest(dir)
	if err != nil || len(defs) != 1 {
		t.Fatalf("ParseManifest = %v, %v — the manifest is valid; only static refuses it", defs, err)
	}
}
