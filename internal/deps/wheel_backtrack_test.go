package deps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/advisory"
)

// fakePython is the interpreter a fake uv is told to resolve for; it never
// runs it.
const fakePython = "/opt/py/bin/python3"

// fakeUV puts a uv on PATH that answers the three compile shapes the Python
// resolver asks, the way uv 0.13 answered them for `marimo` on Python 3.15 —
// where pyyaml publishes no cp315 wheel:
//
//	--no-build                     → marimo 0.6.0, the set it backtracked to
//	--no-deps                      → marimo 0.25.2, the newest there is
//	--no-build --constraint ...    → refused, naming the wheel pyyaml lacks
//
// heldResolves makes the third succeed instead, which is a lower pick that was
// not caused by a missing wheel; conflict makes it fail without naming one.
func fakeUV(t *testing.T, held string) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
out=""; nodeps=0; constrained=0
while [ $# -gt 0 ]; do
  case "$1" in
    --output-file) out="$2"; shift ;;
    --no-deps) nodeps=1 ;;
    --constraint) constrained=1; shift ;;
  esac
  shift
done
if [ $nodeps = 1 ]; then printf 'marimo==0.25.2\nclick==8.1.7\n' > "$out"; exit 0; fi
if [ $constrained = 1 ]; then
  case "` + held + `" in
    resolves) printf 'marimo==0.25.2\nclick==8.1.7\n' > "$out"; exit 0 ;;
    conflict) echo "error: No solution found: because you require marimo<0.7 and marimo>=0.25.2" >&2; exit 1 ;;
    *) printf 'error: No solution found\n  cause: Because pyyaml>=6.0.1 has no usable wheels and marimo>=0.25.2 depends on pyyaml>=6.0.1\n  hint: Wheels are required for ` + "`pyyaml`" + ` because building from source is disabled for all packages (i.e., with ` + "`--no-build`" + `)\n' >&2; exit 1 ;;
  esac
fi
printf 'marimo==0.6.0\nclick==8.1.7\n' > "$out"
`
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// D3: a --no-build resolution that backtracked past a missing wheel listed a
// set the build never installs — marimo 0.6.0 with two critical advisories,
// while the build installed 0.25.2. The listed set is not judged, the Python
// ecosystem is unresolved, and under block the release is refused with a way
// forward that is not "commit a lockfile".
func TestAWheelBacktrackIsNotJudgedAsTheInstalledSet(t *testing.T) {
	fakeUV(t, "")
	dir := writeRelease(t, map[string]string{"requirements.txt": "marimo\n"})

	res, err := ResolveWith(context.Background(), dir, Options{Python: fakePython}, nil)
	var unread *UnreadError
	if !errors.As(err, &unread) {
		t.Fatalf("err = %v, want an UnreadError", err)
	}
	var back *WheelBacktrackError
	if !errors.As(unread.Problems[0].Err, &back) {
		t.Fatalf("problem = %v, want a WheelBacktrackError", unread.Problems[0].Err)
	}
	if back.Package != "marimo" || back.Listed != "0.6.0" || back.Available != "0.25.2" || back.NoWheel != "pyyaml" {
		t.Errorf("backtrack = %+v", back)
	}
	for _, d := range res.Dependencies {
		if d.Package == "marimo" {
			t.Errorf("the backtracked set was judged: %+v", res.Dependencies)
		}
	}
	if len(res.Ecosystems) != 0 {
		t.Errorf("Ecosystems = %v, want PyPI not listed as read", res.Ecosystems)
	}
	if !unresolved(res.Gaps, advisory.EcosystemPyPI) {
		t.Errorf("Gaps = %+v, want PyPI unresolved", res.Gaps)
	}
	// The gap says why, so the deploy's summary does not call a file it read
	// unreadable (docs/bugs 2026-10-28 #4).
	for _, g := range res.Gaps {
		if g.Kind == GapUnresolved && !strings.Contains(g.Reason, "pyyaml publishes no wheel") {
			t.Errorf("unresolved gap = %+v, want the backtrack as its reason", g)
		}
	}

	msg := UnreadRefusal(base(), unread, true)
	if !strings.HasPrefix(msg, "REFUSED:") || !strings.Contains(msg, "pyyaml") ||
		!strings.Contains(msg, "pin marimo==0.6.0") || strings.Contains(msg, "package-lock.json") ||
		!strings.Contains(msg, "earlier Python line under [runtime]") {
		t.Errorf("block refusal = %q", msg)
	}
	// The file was read, and the sentence is one (drills 2026-10-29: "could
	// not be read" about a file it read, and "The way forward is to for …").
	if strings.Contains(msg, "could not be read") || !strings.Contains(msg, "could not be checked") ||
		strings.Contains(msg, "is to for") || !strings.Contains(msg, "The way forward: for requirements.txt, pin") {
		t.Errorf("block refusal wording = %q", msg)
	}
}

// A lower pick that is a real upper bound — the build meets it too — is the
// installed set, and stays judged.
func TestARealConflictIsStillTheInstalledSet(t *testing.T) {
	for _, held := range []string{"conflict", "resolves"} {
		t.Run(held, func(t *testing.T) {
			fakeUV(t, held)
			dir := writeRelease(t, map[string]string{"requirements.txt": "marimo<0.7\n"})
			res, err := ResolveWith(context.Background(), dir, Options{Python: fakePython}, nil)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			found := false
			for _, d := range res.Dependencies {
				found = found || (d.Package == "marimo" && d.Version == "0.6.0")
			}
			if !found {
				t.Errorf("Dependencies = %+v, want marimo 0.6.0 judged", res.Dependencies)
			}
		})
	}
}

// D3 follow-up: every uv call the PyPI resolution makes is handed the release's
// interpreter, so the set judged is the one the build installs for that
// Python — not the one uv's own discovery would have picked.
func TestEveryPythonResolutionIsForTheReleasesInterpreter(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
echo "$*" >> ` + calls + `
out=""; nodeps=0
while [ $# -gt 0 ]; do
  case "$1" in
    --output-file) out="$2"; shift ;;
    --no-deps) nodeps=1 ;;
  esac
  shift
done
if [ $nodeps = 1 ]; then printf 'marimo==0.25.2\n' > "$out"; exit 0; fi
printf 'marimo==0.6.0\n' > "$out"
`
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := writeRelease(t, map[string]string{"requirements.txt": "marimo\n"})

	if _, err := ResolveWith(context.Background(), dir, Options{Python: "/opt/py312/bin/python3"}, nil); err != nil {
		t.Fatalf("ResolveWith: %v", err)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("uv was called %d times, want compile, --no-deps and the held compile:\n%s", len(lines), raw)
	}
	for _, l := range lines {
		if !strings.Contains(l, "--python /opt/py312/bin/python3") {
			t.Errorf("a uv call without the release's interpreter: %q", l)
		}
	}

	// No interpreter at all: uv is not asked to choose one — it is not run
	// (security review 2026-11-14, DG-2).
	os.Remove(calls)
	res, err := Resolve(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if raw, _ := os.ReadFile(calls); len(raw) != 0 {
		t.Errorf("a resolution with no interpreter ran uv: %s", raw)
	}
	unresolved := false
	for _, g := range res.Gaps {
		unresolved = unresolved || (g.Kind == GapUnresolved && g.Ecosystem == advisory.EcosystemPyPI)
	}
	if !unresolved {
		t.Errorf("gaps = %+v, want Python unresolved", res.Gaps)
	}
}

// BB-011 (second 2026-11-03 bash): uv hints "Wheels are required for `x`"
// under every --no-build refusal, so a plain upper bound — marimo capping
// pymdown-extensions below 12 on Python 3.13 — was read as a missing wheel
// and refused the app. Only the cause line decides.
func TestAVersionConflictUnderTheNoBuildHintIsNoMissingWheel(t *testing.T) {
	conflict := "error: No solution found when resolving dependencies:\n" +
		"  cause: Because marimo>=0.23.16 depends on pymdown-extensions>=10.21.2,<12 and pymdown-extensions>=12.3, we can conclude that marimo>=0.23.16 cannot be used.\n" +
		"hint: Wheels are required for `marimo` because building from source is disabled for all packages (i.e., with `--no-build`)"
	if name, ok := missingWheel(conflict, nil); ok {
		t.Fatalf("a version conflict read as a missing wheel for %q", name)
	}
	missing := "error: No solution found\n" +
		"  cause: Because pyyaml>=6.0.1 has no usable wheels and marimo>=0.25.2 depends on pyyaml>=6.0.1\n" +
		"hint: Wheels are required for `pyyaml` because building from source is disabled for all packages (i.e., with `--no-build`)"
	if name, ok := missingWheel(missing, func(string) string { return "6.0.3" }); !ok || name != "pyyaml" {
		t.Fatalf("missing wheel = %q %v", name, ok)
	}
}

// BB-005 (third 2026-11-03 list): uv 0.13's real refusal for marimo with
// pymdown-extensions held at 12.3 on Python 3.13 is a version conflict whose
// derivation says, in passing, that old marimo releases "have no usable
// wheels". None of them is marimo's newest, so no wheel is missing; a range
// that holds the newest release is.
func TestOldReleasesWithoutWheelsInAConflictAreNoMissingWheel(t *testing.T) {
	raw, err := os.ReadFile("testdata/uv-marimo-pymdown-conflict-py313.txt")
	if err != nil {
		t.Fatal(err)
	}
	newest := map[string]string{"marimo": "0.25.2", "pymdown-extensions": "12.3"}
	if name, ok := missingWheel(string(raw), func(n string) string { return newest[n] }); ok {
		t.Fatalf("the marimo/pymdown conflict read as a missing wheel for %q", name)
	}
	// The same refusal with marimo's newest unknown stays a refusal.
	if _, ok := missingWheel(string(raw), func(string) string { return "" }); !ok {
		t.Fatalf("an unconfirmed newest version was read as confirmed")
	}
	// The newest release inside a wheelless "all of:" list is a missing wheel.
	newest["marimo"] = "0.13.12"
	if name, ok := missingWheel(string(raw), func(n string) string { return newest[n] }); !ok || name != "marimo" {
		t.Fatalf("missing wheel = %q %v, want marimo", name, ok)
	}
}

// BB-004 (third 2026-11-03 list): pyyaml==5.3 publishes only an sdist, so the
// --no-build listing fails outright with nothing to backtrack to. That is the
// backtrack's missing wheel one step closer, and under block it is refused
// with a way out — it deployed unscanned with a warning.
func TestAListingThatFailsForAMissingWheelIsRefusedUnderBlock(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf 'error: No solution found when resolving dependencies\\n  cause: Because pyyaml==5.3 has no usable wheels and you require pyyaml==5.3, we can conclude that your requirements are unsatisfiable.\\n\\nhint: Wheels are required for `pyyaml` because building from source is disabled for all packages (i.e., with `--no-build`)\\n' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, reqs := range []string{"pyyaml==5.3\nflask\n", "pyyaml==5.3\nflask==3.0.0\n"} {
		dir := writeRelease(t, map[string]string{"requirements.txt": reqs})
		res, err := ResolveWith(context.Background(), dir, Options{Python: fakePython}, nil)
		var unread *UnreadError
		if !errors.As(err, &unread) {
			t.Fatalf("%q: err = %v, want an UnreadError", reqs, err)
		}
		var nw *NoWheelError
		if !errors.As(unread.Problems[0].Err, &nw) || nw.Package != "pyyaml" {
			t.Fatalf("%q: problem = %v, want a NoWheelError for pyyaml", reqs, unread.Problems[0].Err)
		}
		if !unresolved(res.Gaps, advisory.EcosystemPyPI) || len(res.Ecosystems) != 0 {
			t.Errorf("%q: gaps %+v ecosystems %v, want PyPI unresolved", reqs, res.Gaps, res.Ecosystems)
		}
		msg := UnreadRefusal(base(), unread, true)
		if !strings.HasPrefix(msg, "REFUSED:") || !strings.Contains(msg, "pin a release of pyyaml") {
			t.Errorf("%q: refusal = %q", reqs, msg)
		}
	}
}

// BB-039 (fourth 2026-11-03 list): an earlier Python line is suggested only
// when the package publishes a wheel for one this install offers — pyyaml==5.3
// has a Linux wheel for none — and the refusal quotes uv, never the command
// line that names the release's path on the server.
func TestANoWheelRefusalSuggestsOnlyALineWithAWheel(t *testing.T) {
	refuse := `printf 'error: No solution found when resolving dependencies\n  cause: Because pyyaml==5.3 has no usable wheels and you require pyyaml==5.3, we can conclude that your requirements are unsatisfiable.\n' >&2; exit 1`
	for _, tc := range []struct {
		name, script string
		earlier      []string
		want, never  string
	}{
		{"no line has one", "#!/bin/sh\n" + refuse + "\n", []string{"3.14", "3.13"},
			"pin a release of pyyaml that publishes a wheel for Linux", "earlier"},
		{"an earlier line has one", "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = 3.13 ] && exit 0; done\n" + refuse + "\n",
			[]string{"3.14", "3.13"}, `python = "3.13"`, "for Linux"},
		{"no earlier line offered", "#!/bin/sh\n" + refuse + "\n", nil, "for Linux", "earlier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(tc.script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			dir := writeRelease(t, map[string]string{"requirements.txt": "pyyaml==5.3\n"})
			_, err := ResolveWith(context.Background(), dir, Options{Python: fakePython, EarlierPythons: tc.earlier}, nil)
			var unread *UnreadError
			if !errors.As(err, &unread) {
				t.Fatalf("err = %v, want an UnreadError", err)
			}
			msg := UnreadRefusal(base(), unread, true)
			if !strings.Contains(msg, tc.want) || strings.Contains(msg, tc.never) {
				t.Errorf("refusal = %q, want %q and never %q", msg, tc.want, tc.never)
			}
			if strings.Contains(msg, dir) || strings.Contains(msg, ".toft-resolved.txt") || strings.Contains(msg, "--output-file") {
				t.Errorf("the refusal quotes the command line or the server's path: %q", msg)
			}
		})
	}
}

// D-09 (third 2026-11-03 list): a direct finding carries its path — the app
// and the package — and a transitive one the chain uv's `# via` records, on
// the PyPI resolver as on npm's.
func TestAResolvedPythonDependencyCarriesItsPath(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
out=""; nodeps=0
while [ $# -gt 0 ]; do
  case "$1" in
    --output-file) out="$2"; shift ;;
    --no-deps) nodeps=1 ;;
  esac
  shift
done
if [ $nodeps = 1 ]; then printf 'flask==3.1.0\njinja2==3.1.6\nmarkupsafe==3.0.2\n' > "$out"; exit 0; fi
printf 'flask==3.1.0\n    # via -r requirements.txt\njinja2==3.1.6\n    # via flask\nmarkupsafe==3.0.2\n    # via\n    #   jinja2\n    #   werkzeug\n' > "$out"
`
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := writeRelease(t, map[string]string{"requirements.txt": "flask\n"})
	res, err := ResolveWith(context.Background(), dir, Options{Python: fakePython}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"flask":      "(this app) > flask",
		"jinja2":     "(this app) > flask > jinja2",
		"markupsafe": "(this app) > flask > jinja2 > markupsafe",
	}
	for _, d := range res.Dependencies {
		if d.Path != want[d.Package] || d.Direct != (d.Package == "flask") {
			t.Errorf("%s: direct %v path %q, want %q", d.Package, d.Direct, d.Path, want[d.Package])
		}
	}
	if len(res.Dependencies) != 3 {
		t.Errorf("deps = %+v", res.Dependencies)
	}
}
