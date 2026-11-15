package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/toolchain"
)

// toft check says what the build would: an R tree with no renv.lock installs no
// packages. It passed in silence while the build log said the app would fail at
// its first library() (drills on trytoft, 2026-10-23).
func TestAnRTreeWithNoRenvLockIsWarnedAtCheck(t *testing.T) {
	dir := t.TempDir()
	if w, ok := rPackagesUnlocked(dir, toolchain.R, FrameworkShiny); !ok || w.Code != WarningNoRenvLock {
		t.Errorf("no renv.lock: warned = %v, code %q", ok, w.Code)
	}
	if _, ok := rPackagesUnlocked(dir, toolchain.Python, FrameworkFlask); ok {
		t.Error("a Python tree was warned about renv.lock")
	}
	if err := os.WriteFile(filepath.Join(dir, "renv.lock"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := rPackagesUnlocked(dir, toolchain.R, FrameworkShiny); ok {
		t.Error("an R tree with renv.lock was warned")
	}
}

// BB-013 (third 2026-11-03 list): a document has no first library() call to
// fail at — the deploy renders it, and fails there.
func TestNoRenvLockOnADocumentSaysTheRenderFails(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []Framework{FrameworkRMarkdown, FrameworkQuarto} {
		w, ok := rPackagesUnlocked(dir, toolchain.R, f)
		if !ok || !strings.Contains(w.Message, "render step") || strings.Contains(w.Message, "library() call") {
			t.Errorf("%s: %q", f, w.Message)
		}
	}
	if w, _ := rPackagesUnlocked(dir, toolchain.R, FrameworkShiny); !strings.Contains(w.Message, "first library() call") {
		t.Errorf("shiny: %q", w.Message)
	}
}

// A batch app's no_renv_lock says the job fails, not "the app" — a batch app
// has no process of its own (D-16).
func TestABatchAppsNoRenvLockNamesTheJob(t *testing.T) {
	w, ok := rPackagesUnlocked(t.TempDir(), toolchain.R, FrameworkBatchR)
	if !ok || !strings.Contains(w.Message, "each job will fail") || strings.Contains(w.Message, "the app will fail") {
		t.Errorf("warning = %+v", w)
	}
}
