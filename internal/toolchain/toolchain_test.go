package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeInterpreter writes a script that answers a --version probe, so the tests
// exercise the real exec path rather than a stub around it.
func fakeInterpreter(t *testing.T, name, greeting string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the probe execs a shell script")
	}
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\necho '" + greeting + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseVersionOutput(t *testing.T) {
	for _, tc := range []struct{ lang, out, want string }{
		{Python, "Python 3.13.7\n", "3.13.7"},
		{Python, "Python 3.9.25", "3.9.25"},
		{Node, "v24.13.0\n", "24.13.0"},
		{Go, "go version go1.26.5 linux/amd64\n", "1.26.5"},
		{Go, "go version devel +abc linux/amd64", ""},
		// Both R spellings, which are the reason parseVersionOutput has an R
		// case at all: each begins with a letter, so the leading-numeric-run
		// trim alone reduces them to "" and the platform refuses to boot on a
		// declared TOFT_R_PATH.
		{R, `R version 4.5.3 (2026-03-11) -- "Reassured Reassurer"`, "4.5.3"},
		{R, "Rscript (R) version 4.5.3 (2026-03-11)\n", "4.5.3"},
		{R, "R version 4.4.1 (2024-06-14)", "4.4.1"},
		{R, "bash: Rscript: command not found", ""},
		{Python, "bash: python3: command not found", ""},
	} {
		if got := parseVersionOutput(tc.lang, tc.out); got != tc.want {
			t.Errorf("parseVersionOutput(%s, %q) = %q, want %q", tc.lang, tc.out, got, tc.want)
		}
	}
}

// The unit a pin is judged at, which is the whole of D8. Node pins a line;
// Python and Go pin a line that happens to have two numbers in it.
func TestSeriesIsTheMatchingUnit(t *testing.T) {
	for _, tc := range []struct{ lang, v, want string }{
		{Node, "24.13.0", "24"},
		{Python, "3.13.7", "3.13"},
		{Go, "1.26.5", "1.26"},
		{Python, "3.13", "3.13"},
	} {
		if got := series(tc.lang, tc.v); got != tc.want {
			t.Errorf("series(%s, %s) = %q, want %q", tc.lang, tc.v, got, tc.want)
		}
	}
}

// The probe enumerates: PATH plus every declared path, each confirmed by exec,
// highest first, deduplicated on the matching unit.
func TestAvailableEnumeratesDeclaredPaths(t *testing.T) {
	a := fakeInterpreter(t, "python3.12", "Python 3.12.9")
	b := fakeInterpreter(t, "python3.13", "Python 3.13.7")
	dup := fakeInterpreter(t, "python3.13-again", "Python 3.13.2")

	t.Setenv("TOFT_PYTHON_PATH", strings.Join([]string{a, b, dup}, string(os.PathListSeparator)))
	t.Setenv("PATH", "/nonexistent-so-there-is-no-default")
	resetForTest()
	t.Cleanup(resetForTest)

	got := Available(Python)
	if len(got) != 2 {
		t.Fatalf("got %d versions, want 2 (3.13 and 3.12, with the second 3.13 deduplicated): %v", len(got), got)
	}
	if got[0].Version != "3.13.7" || got[1].Version != "3.12.9" {
		t.Errorf("not sorted highest-first: %v", got)
	}
	if got[0].Path != b {
		t.Errorf("path not carried: %v", got[0])
	}
	if err := Err(Python); err != nil {
		t.Errorf("unexpected probe error: %v", err)
	}
}

// D11: the operator's own statement about their own image, so a typo fails at
// boot rather than becoming a confusing deploy refusal three weeks later.
func TestADeclaredPathThatWillNotExecIsAnError(t *testing.T) {
	t.Setenv("TOFT_NODE_PATH", "/definitely/not/here/node")
	resetForTest()
	t.Cleanup(resetForTest)

	err := Err(Node)
	if err == nil {
		t.Fatal("a declared path that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "TOFT_NODE_PATH") {
		t.Errorf("the error does not name the variable the operator has to fix: %v", err)
	}
	if Validate() == nil {
		t.Error("Validate accepted an install that cannot run what it declares")
	}
}

// The default keeps its flag, because it is what an app with no pin gets.
func TestTheDefaultIsMarked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho v22.1.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TOFT_NODE_PATH", "")
	resetForTest()
	t.Cleanup(resetForTest)

	got := Available(Node)
	if len(got) != 1 || !got[0].Default || got[0].Version != "22.1.0" {
		t.Fatalf("got %v, want one default 22.1.0", got)
	}
}

// A runner image's version is declared, never probed, because the decision is
// made before any pod exists. Go has no such variable and must not acquire one.
func TestConfiguredIsDeclaredAndGoHasNone(t *testing.T) {
	t.Setenv("RUNNER_PYTHON_VERSION", "3.13")
	t.Setenv("RUNNER_NODE_VERSION", "")
	got := Configured(Python)
	if len(got) != 1 || got[0].Version != "3.13" || got[0].Path != "" {
		t.Fatalf("got %v, want one declared 3.13 with no path", got)
	}
	if len(Configured(Node)) != 0 {
		t.Error("an unset RUNNER_NODE_VERSION produced a version")
	}
	if len(Configured(Go)) != 0 {
		t.Error("Go acquired a runner declaration; it leaves its toolchain on the build host")
	}
	t.Setenv("RUNNER_PYTHON_VERSION", "trixie")
	if ConfiguredError(Python) == nil {
		t.Error("a RUNNER_PYTHON_VERSION that is not a version was accepted")
	}
}

// An image may carry several interpreters, so the declaration is a list and the
// first entry is the image's default (D19).
//
// The single-value spelling is the same statement with one entry, which is what
// lets every install that set RUNNER_R_VERSION=4.5 keep meaning it.
func TestConfiguredReadsASetAndTheFirstIsTheDefault(t *testing.T) {
	t.Setenv("RUNNER_R_VERSION", "4.5,4.4,4.3")
	got := Configured(R)
	if len(got) != 3 {
		t.Fatalf("got %v, want three declared versions", got)
	}
	for i, want := range []string{"4.5", "4.4", "4.3"} {
		if got[i].Version != want {
			t.Errorf("entry %d is %q, want %q — order is the operator's and the first is the "+
				"image's default", i, got[i].Version, want)
		}
		if got[i].Path != "" {
			t.Errorf("entry %d carries a path; a declared version names no file on this host", i)
		}
	}
	if !got[0].Default || got[1].Default || got[2].Default {
		t.Errorf("got defaults %v/%v/%v, want only the first — an app with no pin takes the "+
			"interpreter on the image's PATH, and position is what says which that is",
			got[0].Default, got[1].Default, got[2].Default)
	}

	// One series, however many ways it is spelled: a resolution must not depend
	// on which of two equivalent entries came first.
	t.Setenv("RUNNER_R_VERSION", "4.5.0,4.5.1")
	if got := Configured(R); len(got) != 1 {
		t.Errorf("got %v, want one — 4.5.0 and 4.5.1 are one answer to every question this "+
			"platform asks", got)
	}

	// A malformed entry voids the declaration rather than being dropped from
	// it, so an operator never runs against a set they did not write.
	t.Setenv("RUNNER_R_VERSION", "4.5,trixie")
	if got := Configured(R); got != nil {
		t.Errorf("got %v, want nil — a bad entry must not leave a silently shorter set", got)
	}
	if err := ConfiguredError(R); err == nil {
		t.Error("a RUNNER_R_VERSION with an unparseable entry was accepted")
	} else if !strings.Contains(err.Error(), "trixie") {
		t.Errorf("the refusal is %q and does not name the entry that caused it", err)
	}

	// A trailing or doubled comma is a typo, and one that would otherwise be
	// invisible: it names nothing and would silently shorten the set.
	t.Setenv("RUNNER_R_VERSION", "4.5,,4.3")
	if ConfiguredError(R) == nil {
		t.Error("an empty entry between commas was accepted")
	}
}

// This package must stay a leaf, so runtime.BaseEnv's allow-list is restated
// rather than imported. This is what stops the two drifting.
func TestBaseEnvMatchesRuntime(t *testing.T) {
	src, err := os.ReadFile("../runtime/env.go")
	if err != nil {
		t.Skipf("cannot read internal/runtime/env.go: %v", err)
	}
	for _, k := range []string{"PATH", "HOME", "LANG", "TMPDIR", "USER", "SHELL"} {
		if !strings.Contains(string(src), `"`+k+`"`) {
			t.Errorf("runtime.BaseEnv no longer allows %q — internal/toolchain's copy has drifted", k)
		}
	}
	t.Setenv("PATH", "/x")
	t.Setenv("JWT_SECRET", "should-not-be-here")
	for _, kv := range baseEnv() {
		if strings.HasPrefix(kv, "JWT_SECRET=") {
			t.Fatal("the probe's environment carries the platform's secrets")
		}
	}
}

func TestGoToolchainPosture(t *testing.T) {
	t.Setenv("GOTOOLCHAIN", "")
	if GoToolchainPosture() != "" {
		t.Error("unset GOTOOLCHAIN reported as configured; 'nobody decided' is its own fact")
	}
	t.Setenv("GOTOOLCHAIN", "local")
	if GoToolchainPosture() != "local" {
		t.Error("GOTOOLCHAIN=local not reported")
	}
}
