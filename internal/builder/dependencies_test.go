package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/runtime"
)

// pythonRelease writes the smallest tree Repair will treat as a Python release
// and returns it. venvTarget is what `venv/bin/python` is pointed at — the whole
// question this feature turns on is whether that path resolves.
func pythonRelease(t *testing.T, venvTarget string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app = None\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(venvTarget, filepath.Join(dir, "venv", "bin", "python")); err != nil {
		t.Fatal(err)
	}
	// A marker inside the venv, to tell "rebuilt" from "built over". The wheels
	// in a stale venv's site-packages are compiled for the other interpreter's
	// ABI, so a fresh interpreter pointed at them is a subtler failure than a
	// missing one — the old tree has to be gone, not overwritten.
	if err := os.WriteFile(filepath.Join(dir, "venv", "stale-marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func flaskSpec(dir string) runtime.WorkloadSpec {
	return runtime.WorkloadSpec{
		AppID: "app-1", Slug: "demo", Name: "demo-abc123",
		Framework: "flask", Entrypoint: "app.py", BasePath: "/apps/demo",
		Release: runtime.LocalRelease(dir), Env: map[string]string{},
	}
}

// A tree that runs here is left exactly as it was fetched.
//
// The repair is conditional and that is not an optimisation detail — it is what
// makes it a repair rather than a build step. Every start in shared mode goes
// through this, and the ordinary case is a release this host built minutes ago.
func TestRepairLeavesARunnableTreeAlone(t *testing.T) {
	real, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH; nothing to point a working venv at")
	}
	dir := pythonRelease(t, real)

	// A zero-value Builder is deliberate: if this reaches a resolver, a
	// database or a command, it has stopped being conditional.
	if err := (&Builder{}).Repair(flaskSpec(dir), dir); err != nil {
		t.Fatalf("Repair on a runnable release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "venv", "stale-marker")); err != nil {
		t.Errorf("the venv was rebuilt for a release that could already run: %v", err)
	}
}

// A release pinned to a version this host does not have fails the start, naming
// the pin — it is never rebuilt against something else.
//
// This is the sharpest form of *refused, never approximated*, because it is the
// one case where approximating would look like success: the venv would be
// rebuilt, the app would start, and it would be running on an interpreter its
// author had explicitly ruled out. Arriving at the restart rather than at the
// next deploy is the point — the host is what changed.
func TestRepairRefusesAPinThisHostCannotSatisfy(t *testing.T) {
	dir := pythonRelease(t, "/nonexistent/python3.11")
	if err := os.WriteFile(filepath.Join(dir, ManifestFile),
		[]byte("[runtime]\npython = \">=3.99,<4\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := (&Builder{}).Repair(flaskSpec(dir), dir)
	if err == nil {
		t.Fatal("Repair rebuilt a release whose pin nothing on this host satisfies")
	}
	if !strings.Contains(err.Error(), "3.99") {
		t.Errorf("the refusal does not name the pin: %v", err)
	}
	// And it discarded nothing it could not replace.
	if _, statErr := os.Stat(filepath.Join(dir, "requirements.txt")); statErr != nil {
		t.Errorf("the release was damaged by a refused repair: %v", statErr)
	}
}

// The repair itself: a venv bound to an interpreter that is gone is discarded and
// rebuilt, and the app's own dependencies are installed into the new one.
//
// This is the regression the whole decision exists for. `uv venv` records the
// absolute path of the interpreter it was built against, so moving the
// platform's Python leaves every already-built Python release with a
// `venv/bin/python` that dangles — and the release is restored byte-for-byte on
// the next start, so it comes back on every restart, wake and node move until
// something rebuilds it.
func TestRepairRebuildsAVenvBoundToAMissingInterpreter(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not installed; the rebuild half cannot run")
	}
	dir := pythonRelease(t, "/nonexistent/python3.11")

	if err := (&Builder{}).Repair(flaskSpec(dir), dir); err != nil {
		t.Fatalf("Repair: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "venv", "bin", "python")); err != nil {
		t.Fatalf("venv/bin/python still does not resolve after a repair: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "venv", "stale-marker")); err == nil {
		t.Error("the stale venv was built over rather than discarded — its site-packages " +
			"holds wheels compiled for the interpreter that went away")
	}
}

// An R build's environment turns colour off, so renv's escapes do not reach
// the build log — unless the app's own build environment says otherwise
// (docs/bugs 2026-10-28 D21).
func TestTheRBuildEnvironmentTurnsColourOff(t *testing.T) {
	env := map[string]string{}
	uncoloured(env, map[string]string{})
	if env["NO_COLOR"] != "1" || env["R_CLI_NUM_COLORS"] != "1" {
		t.Errorf("env = %v, want NO_COLOR and R_CLI_NUM_COLORS set", env)
	}
	env = map[string]string{"NO_COLOR": ""}
	uncoloured(env, map[string]string{"NO_COLOR": ""})
	if env["NO_COLOR"] != "" {
		t.Errorf("the app's own NO_COLOR was overwritten: %v", env)
	}
}
