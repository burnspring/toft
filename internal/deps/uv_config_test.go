package deps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/advisory"
)

// hostPython is a real interpreter for a test that runs the real uv, or a skip.
func hostPython(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	return p
}

func needUV(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv is not on PATH")
	}
}

// DG-1/DG-2 (security review 2026-11-14): every argv the scan hands uv carries
// --no-config and --python, on every path that runs uv — the main compile, the
// wheel backtrack's three shapes and the earlier-line probe.
func TestEveryUvArgvCarriesNoConfigAndPython(t *testing.T) {
	if got := uvCompile("/py", "--quiet"); strings.Join(got, " ") != "pip compile --no-config --quiet --python /py" {
		t.Errorf("uvCompile = %q", got)
	}

	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	// A flag file, not a variable: the scan's uv runs under resolveEnv's
	// allow-list, which would drop one.
	nowheel := filepath.Join(t.TempDir(), "nowheel")
	// --no-build without a constraint and without --python-version fails for a
	// missing wheel, so the earlier-line probe runs too; everything else lists.
	script := `#!/bin/sh
echo "$@" >> ` + calls + `
out=""; nodeps=0; probe=0
for a in "$@"; do case "$a" in --no-deps) nodeps=1 ;; --python-version) probe=1 ;; esac; done
while [ $# -gt 0 ]; do case "$1" in --output-file) out="$2"; shift ;; esac; shift; done
if [ $probe = 1 ]; then exit 0; fi
if [ -e ` + nowheel + ` ] && [ $nodeps = 0 ]; then
  printf 'error: No solution found when resolving dependencies\n  cause: Because pyyaml==5.3 has no usable wheels and you require pyyaml==5.3, we can conclude that your requirements are unsatisfiable.\n' >&2; exit 1
fi
if [ $nodeps = 1 ]; then printf 'marimo==0.25.2\n' > "$out"; exit 0; fi
printf 'marimo==0.6.0\n' > "$out"
`
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := writeRelease(t, map[string]string{"requirements.txt": "marimo\n"})

	_, _ = ResolveWith(context.Background(), dir, Options{Python: fakePython}, nil)
	if err := os.WriteFile(nowheel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveWith(context.Background(), dir, Options{Python: fakePython, EarlierPythons: []string{"3.12"}}, nil)
	var unread *UnreadError
	if !errors.As(err, &unread) {
		t.Fatalf("err = %v, want the missing wheel refused", err)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 5 {
		t.Fatalf("uv was called %d times, want the backtrack and the probe:\n%s", len(lines), raw)
	}
	probed := false
	for _, l := range lines {
		probed = probed || strings.Contains(l, "--python-version")
		if !strings.Contains(l, "--no-config") || !strings.Contains(l, "--python "+fakePython) {
			t.Errorf("a uv call without --no-config and the interpreter: %q", l)
		}
	}
	if !probed {
		t.Errorf("the earlier-line probe never ran:\n%s", raw)
	}
	if env := strings.Join(resolveEnv(), "\n"); !strings.Contains(env, "UV_NO_CONFIG=1") {
		t.Errorf("resolveEnv carries no UV_NO_CONFIG=1:\n%s", env)
	}
	if env := strings.Join(resolveEnv(), "\n"); !strings.Contains(env, "GOENV=off") {
		t.Errorf("resolveEnv carries no GOENV=off (LB-10):\n%s", env)
	}
}

// DG-2: a release cannot choose the interpreter the scan runs as the platform —
// neither through a uv.toml naming one, nor through a committed .venv.
func TestTheScanNeverRunsAnInterpreterTheReleaseChose(t *testing.T) {
	needUV(t)
	real := hostPython(t)
	marker := filepath.Join(t.TempDir(), "ran")
	evil := "#!/bin/sh\ntouch " + marker + "\nexec " + real + " \"$@\"\n"
	cases := map[string]map[string]string{
		"uv.toml": {
			"requirements.txt": "",
			"x":                evil,
			"uv.toml":          "[pip]\npython = \"./x\"\n",
		},
		"pyproject": {
			"requirements.txt": "",
			"x":                evil,
			"pyproject.toml":   "[project]\nname = \"demo\"\nversion = \"0.1\"\n\n[tool.uv.pip]\npython = \"./x\"\n",
		},
		".venv": {
			"requirements.txt":  "",
			".venv/bin/python":  evil,
			".venv/bin/python3": evil,
			".venv/pyvenv.cfg":  "home = " + filepath.Dir(real) + "\nversion = 3.12.0\n",
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeRelease(t, files)
			for _, f := range []string{"x", ".venv/bin/python", ".venv/bin/python3"} {
				_ = os.Chmod(filepath.Join(dir, f), 0o755)
			}
			for _, py := range []string{"", real} {
				_, _ = ResolveWith(context.Background(), dir, Options{Python: py}, nil)
				if _, err := os.Stat(marker); err == nil {
					t.Fatalf("with Python %q the scan ran the release's interpreter", py)
				}
			}
		})
	}
}

// DG-1: an app's own uv configuration cannot keep a package out of the judged
// set — the install does not honour no-emit-package, so neither may the scan.
func TestNoEmitPackageInTheAppsConfigIsIgnored(t *testing.T) {
	needUV(t)
	py := hostPython(t)
	dir := writeRelease(t, map[string]string{
		"requirements.txt": "flask==3.0.0\n",
		"pyproject.toml":   "[tool.uv.pip]\nno-emit-package = [\"werkzeug\"]\n",
		"uv.toml":          "[pip]\nno-emit-package = [\"werkzeug\"]\n",
	})
	res, err := ResolveWith(context.Background(), dir, Options{Python: py}, nil)
	if err != nil {
		t.Skipf("resolution could not run here (no index?): %v", err)
	}
	found := false
	for _, d := range res.Dependencies {
		found = found || (d.Ecosystem == advisory.EcosystemPyPI && d.Package == "werkzeug")
	}
	if !found {
		if len(res.Dependencies) == 0 {
			t.Skipf("nothing resolved (no index?): gaps %+v", res.Gaps)
		}
		t.Errorf("werkzeug missing from the judged set: %+v", res.Dependencies)
	}
}

// LB-9 (security review 2026-11-15): an existing --output-file is uv's
// preference list, so a `.toft-resolved.txt` committed to the repository
// steered the scan to older pins than the build installs. The main compile
// carries --upgrade, and the committed file is gone before uv runs.
func TestUvNeverSeesACommittedResolvedFile(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in --output-file) out="$2"; shift ;; esac
  echo "arg $1" >> ` + calls + `
  shift
done
if [ -n "$out" ]; then
  if [ -e "$out" ]; then echo "existed" >> ` + calls + `; fi
  printf 'marimo==0.6.0\n' > "$out"
fi
`
	if err := os.WriteFile(filepath.Join(bin, "uv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := writeRelease(t, map[string]string{
		"requirements.txt": "marimo>=0.5\n",
		resolvedFile:       "marimo==0.5.0\n",
	})
	_, _ = ResolveWith(context.Background(), dir, Options{Python: fakePython}, nil)
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "existed") {
		t.Errorf("uv ran with the committed %s in place:\n%s", resolvedFile, raw)
	}
	if !strings.Contains(string(raw), "arg --upgrade") {
		t.Errorf("the compile carries no --upgrade:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, resolvedFile)); !os.IsNotExist(err) {
		t.Errorf("%s outlived the scan: %v", resolvedFile, err)
	}
}
