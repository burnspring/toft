package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/toolchain"
)

// fakeInterpreters writes shell scripts that answer `--version` like the real
// tools, declares them, and drops the probe cache at both ends.
//
// Both ends, because the cache is process-wide by design: without the deferred
// reset every later test in this package would resolve Node against a directory
// that no longer exists, which is a worse failure than the one it was hiding.
func fakeInterpreters(t *testing.T, lang string, versions ...string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	paths := map[string]string{}
	var declared []string
	for _, v := range versions {
		bin := filepath.Join(dir, lang+"-"+v)
		greeting := "v" + v
		if lang == toolchain.Python {
			greeting = "Python " + v
		}
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+greeting+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		paths[toolchain.SeriesOf(lang, v)] = bin
		declared = append(declared, bin)
	}
	// Nothing on PATH, so every version here is a declared one and none is the
	// default — which is the case a multi-version image is built to have and the
	// one where picking the wrong entry is invisible.
	t.Setenv("PATH", t.TempDir())
	t.Setenv(strings.ToUpper("toft_"+lang+"_path"), strings.Join(declared, ":"))
	toolchain.ResetForTest()
	t.Cleanup(toolchain.ResetForTest)
	return paths
}

func TestTheStampRoundTripsAndRefusesNonsense(t *testing.T) {
	dir := t.TempDir()

	if lang, v := ReleaseRuntime(dir); lang != "" || v != "" {
		t.Errorf("an unstamped release reported %q %q; a release built before the stamp "+
			"existed has to read as saying nothing, or every one of them becomes unrunnable", lang, v)
	}
	if err := WriteRuntimeStamp(dir, toolchain.Node, "20.20.2"); err != nil {
		t.Fatal(err)
	}
	if lang, v := ReleaseRuntime(dir); lang != toolchain.Node || v != "20.20.2" {
		t.Errorf("read back %q %q, want node 20.20.2", lang, v)
	}
	if got := ReleaseSeries(toolchain.Node, dir); got != "20" {
		t.Errorf("series = %q, want 20 — Node is matched on its major alone", got)
	}
	if got := ReleaseSeries(toolchain.Python, dir); got != "" {
		t.Errorf("a node stamp answered %q for Python; a stamp names one language and "+
			"reading it as another is how a venv gets rebuilt for a version nobody chose", got)
	}

	// A file somebody committed at that name, or a truncated write.
	if err := os.WriteFile(filepath.Join(dir, RuntimeStamp), []byte("node lts/iron\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lang, v := ReleaseRuntime(dir); lang != "" || v != "" {
		t.Errorf("read %q %q out of a stamp that is not a version; a release that says "+
			"something unreadable says nothing, and is started the way it always was", lang, v)
	}

	// Empty removes rather than blanks: a repair that resolved nothing must not
	// leave a stamp claiming it did.
	if err := WriteRuntimeStamp(dir, toolchain.Node, "20.20.2"); err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeStamp(dir, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, RuntimeStamp)); !os.IsNotExist(err) {
		t.Error("an empty stamp left the file behind")
	}
}

// The launcher's half: a Node release runs under the Node it was built against,
// and the lever is a PATH prefix because npm is itself a Node script.
func TestANodeReleaseRunsUnderTheNodeItWasBuiltAgainst(t *testing.T) {
	bins := fakeInterpreters(t, toolchain.Node, "24.20.0", "22.23.2", "20.20.2")

	dir := t.TempDir()
	if got := NodePathPrefix(dir); got != "" {
		t.Errorf("an unstamped release asked for the PATH prefix %q; it has to start the "+
			"way it started before this install had a second Node", got)
	}

	if err := WriteRuntimeStamp(dir, toolchain.Node, "20.20.2"); err != nil {
		t.Fatal(err)
	}
	want := filepath.Dir(bins["20"])
	if got := NodePathPrefix(dir); got != want {
		t.Errorf("NodePathPrefix = %q, want %q — a node_modules filled by 20 and executed "+
			"by 24 fails at require, naming a .node file rather than a version", got, want)
	}

	// The whole point of a prefix rather than a binary: npm has to come from the
	// same directory, or the release is installed by one major and run by another.
	if _, err := os.Stat(filepath.Join(NodePathPrefix(dir), "node-20.20.2")); err != nil {
		t.Errorf("the prefix does not hold the interpreter it was derived from: %v", err)
	}

	if broken, _ := Unrunnable("node", releaseWith(t, dir)); broken {
		t.Error("a release stamped for a Node this host has was called unrunnable")
	}
}

// The refusal half, and the loop it is one end of.
func TestANodeReleaseBuiltForAVersionThisHostLostIsRebuilt(t *testing.T) {
	fakeInterpreters(t, toolchain.Node, "24.20.0")

	dir := releaseWith(t, t.TempDir())
	if err := WriteRuntimeStamp(dir, toolchain.Node, "20.20.2"); err != nil {
		t.Fatal(err)
	}

	broken, why := Unrunnable("node", dir)
	if !broken {
		t.Fatal("a tree built for Node 20 was called runnable on a host carrying only 24 — " +
			"nothing in node_modules dangles when the interpreter is gone, which is why " +
			"this has to be asked of the stamp")
	}
	for _, want := range []string{"20", "no longer"} {
		if !strings.Contains(why, want) {
			t.Errorf("the reason %q does not carry %q", why, want)
		}
	}

	// What the repair then rebuilds against, and what it must write back. Leave
	// the stamp alone and the next start asks the same question, gets the same
	// answer, and rebuilds again — forever.
	v := RepairInterpreter(toolchain.Node, dir)
	if v.Series() != "24" {
		t.Fatalf("the repair resolved %q; with nothing of the stamped series here it has to "+
			"fall back to what this image does have, or the app never comes back", v.Version)
	}
	if err := WriteRuntimeStamp(dir, toolchain.Node, v.Version); err != nil {
		t.Fatal(err)
	}
	if broken, why := Unrunnable("node", dir); broken {
		t.Errorf("the rebuilt release is still unrunnable (%s): a repair that does not "+
			"re-stamp is a loop, not a repair", why)
	}
}

// Python's half. The venv records its interpreter, so the launcher needs
// nothing — but the *repair* discards that venv, and rebuilding it with no
// --python is uv's discovery order choosing a version somebody pinned.
func TestAPythonRepairRebuildsTheVenvAgainstTheStampedInterpreter(t *testing.T) {
	bins := fakeInterpreters(t, toolchain.Python, "3.14.2", "3.13.14", "3.12.11")

	dir := t.TempDir()
	if err := WriteRuntimeStamp(dir, toolchain.Python, "3.12.11"); err != nil {
		t.Fatal(err)
	}
	if got := RepairInterpreter(toolchain.Python, dir).Path; got != bins["3.12"] {
		t.Errorf("the repair would build with %q, want %q", got, bins["3.12"])
	}

	steps, cleanup, err := buildSteps(&BootstrapResponse{Framework: "fastapi"}, dir)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) == 0 || steps[0].name != "uv" {
		t.Fatalf("the first step is not uv: %+v", steps)
	}
	args := strings.Join(steps[0].args, " ")
	if !strings.Contains(args, "--python "+bins["3.12"]) {
		t.Errorf("uv venv is invoked as %q, with no --python naming the stamped "+
			"interpreter: a bare `uv venv` runs uv's own discovery, which on an image "+
			"carrying three interpreters will choose one", args)
	}
}

// releaseWith gives a directory the shape Unrunnable's node case needs, so the
// stamp is the only thing under test.
func releaseWith(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The half a PATH prefix cannot reach.
//
// Go's exec.Command resolves a bare name against the *parent's* PATH, so the
// prefix the launchers write decides what npm's own shebang finds and does not
// decide which npm is started. The first Node 20 deploy this platform accepted
// ran npm 12 out of /usr/local/bin on Node 20.20.2 — the right interpreter and
// the wrong tool, which is the same split from the other side.
func TestTheToolBesideAnInterpreterIsNamedAbsolutely(t *testing.T) {
	bins := fakeInterpreters(t, toolchain.Node, "24.20.0", "20.20.2")
	binDir := filepath.Dir(bins["20"])
	npm := filepath.Join(binDir, "npm")
	if err := os.WriteFile(npm, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if got := ToolBeside(NodePathPrefix(dir), "npm"); got != "npm" {
		t.Errorf("an unstamped release named %q; with nothing to resolve against, the bare "+
			"name is what every release has always been started with", got)
	}

	if err := WriteRuntimeStamp(dir, toolchain.Node, "20.20.2"); err != nil {
		t.Fatal(err)
	}
	if got := ToolBeside(NodePathPrefix(dir), "npm"); got != npm {
		t.Errorf("ToolBeside = %q, want %q", got, npm)
	}

	// An image carrying an interpreter without its package manager behaves as
	// it did before rather than failing with a path that is not there.
	if got := ToolBeside(binDir, "pnpm"); got != "pnpm" {
		t.Errorf("a tool that is not beside the interpreter resolved to %q, want the bare name", got)
	}
}

// A build that leaves a link at the stamp's name does not get the platform to
// write through it: the link is replaced by a regular file and the target is
// untouched; a link planted afterwards is not read either (code review
// 2026-11-11, F-1).
func TestTheStampNeverFollowsALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(target, []byte("root:x:0:0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, RuntimeStamp)); err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeStamp(dir, toolchain.Python, "3.13.2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "root:x:0:0\n" {
		t.Fatalf("the link's target was written: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dir, RuntimeStamp)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("stamp after the write: %v %v, want a regular file", fi, err)
	}
	if lang, v := ReleaseRuntime(dir); lang != toolchain.Python || v != "3.13.2" {
		t.Errorf("read back %q %q", lang, v)
	}

	// Removal through a link removes the link, not the target.
	os.Remove(filepath.Join(dir, RuntimeStamp))
	os.Symlink(target, filepath.Join(dir, RuntimeStamp))
	if err := WriteRuntimeStamp(dir, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the target was removed: %v", err)
	}
	// And a link planted at the name is not read as a stamp.
	os.WriteFile(target, []byte("python 3.13.2\n"), 0o644)
	os.Symlink(target, filepath.Join(dir, RuntimeStamp))
	if lang, v := ReleaseRuntime(dir); lang != "" || v != "" {
		t.Errorf("a link was read as a stamp: %q %q", lang, v)
	}
}
