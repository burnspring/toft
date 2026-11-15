package toolchain

import (
	"os"
	"path/filepath"
	"testing"
)

// Discovery, not a language.
//
// The order is QUARTO_BIN's entries and then the bare name for PATH, which is
// the same shape TOFT_*_PATH has — an operator with two of these should
// not have to learn a second convention for the third.
func TestQuartoCandidateOrder(t *testing.T) {
	t.Setenv(QuartoBinEnv, "/opt/quarto/bin/quarto:/usr/local/quarto")

	got := quartoCandidates()
	want := []string{"/opt/quarto/bin/quarto", "/usr/local/quarto", "quarto"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}

func TestQuartoWithNothingDeclaredFallsBackToPath(t *testing.T) {
	t.Setenv(QuartoBinEnv, "")
	if got := quartoCandidates(); len(got) != 1 || got[0] != "quarto" {
		t.Errorf("candidates = %v, want just the bare name", got)
	}
}

// Absent is a complete configuration and never an error: nothing here returns
// one, and every caller's question is "is there a path".
func TestAbsentQuartoIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(QuartoBinEnv, filepath.Join(dir, "nothing-here"))
	// PATH as well, or this asserts nothing on a host that has Quarto — which
	// is every host this feature is developed on.
	t.Setenv("PATH", dir)

	// probeQuarto rather than Quarto(): the exported one is memoised for the
	// life of the process, which is right for it and useless here.
	got := probeQuarto()
	if got.path != "" || got.version != "" {
		t.Errorf("probe = %+v on a host with no Quarto, want empty", got)
	}
}

// What it says it is, from what the binary answered — never from the path it
// was found at, and never from a constant.
func TestQuartoVersionComesFromTheBinary(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "quarto")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 1.9.42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(QuartoBinEnv, fake)

	got := probeQuarto()
	if got.path != fake {
		t.Errorf("path = %q, want %q", got.path, fake)
	}
	if got.version != "1.9.42" {
		t.Errorf("version = %q, want 1.9.42", got.version)
	}
}

// A declared path that does not run falls through to the next candidate rather
// than becoming the answer — the same conservatism the language probe has.
func TestAnUnrunnableDeclaredQuartoIsSkipped(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken")
	if err := os.WriteFile(broken, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(dir, "quarto")
	if err := os.WriteFile(working, []byte("#!/bin/sh\necho 2.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(QuartoBinEnv, broken+":"+working)

	if got := probeQuarto(); got.path != working {
		t.Errorf("path = %q, want the working candidate %q", got.path, working)
	}
}
