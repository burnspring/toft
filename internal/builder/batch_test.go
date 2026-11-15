package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func batchRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A Python program with no web entrypoint is refused as a service and accepted
// as a batch app — the declaration is the whole difference.
func TestBatchPythonNeedsNoEntrypoint(t *testing.T) {
	files := map[string]string{"requirements.txt": "requests\n", "etl.py": "print(1)\n"}
	if got := DetectFramework(batchRepo(t, files)).Framework; got != FrameworkUnsupported {
		t.Fatalf("without [batch] it detected as %q; a Python repo with no entrypoint is refused", got)
	}
	files["toft.toml"] = "[batch]\n"
	if got := DetectFramework(batchRepo(t, files)).Framework; got != FrameworkBatchPython {
		t.Errorf("with [batch] it detected as %q, want batch-python", got)
	}
}

// A Go main and a Python main.py are what the service detectors read as a web
// app; declared batch, they are not.
func TestBatchWinsOverEveryServiceShape(t *testing.T) {
	cases := map[Framework]map[string]string{
		FrameworkBatchGo:     {"go.mod": "module x\n\ngo 1.22\n", "main.go": "package main\nfunc main(){}\n"},
		FrameworkBatchPython: {"requirements.txt": "flask\n", "main.py": "print(1)\n"},
		FrameworkBatchNode:   {"package.json": `{"scripts":{"start":"node index.js"}}`},
	}
	for want, files := range cases {
		files["toft.toml"] = "[batch]\n"
		got := DetectFramework(batchRepo(t, files))
		if got.Framework != want {
			t.Errorf("detected %q, want %q", got.Framework, want)
		}
		if want == FrameworkBatchGo && got.Entrypoint == "" {
			t.Error("a batch Go module should name its main package for the build")
		}
	}
}

// Two languages and no declaration is a question the author answers; the
// refusal names both and the key that settles it.
func TestBatchWithTwoLanguagesAsksWhich(t *testing.T) {
	dir := batchRepo(t, map[string]string{
		"toft.toml": "[batch]\n", "package.json": "{}", "requirements.txt": "x\n",
	})
	if got := DetectFramework(dir).Framework; got != FrameworkUnsupported {
		t.Fatalf("detected %q, want a refusal", got)
	}
	msg := UnsupportedMessage(dir)
	if !strings.Contains(msg, "node and python") || !strings.Contains(msg, "language =") {
		t.Errorf("refusal = %q, want both languages and the key", msg)
	}
	dir2 := batchRepo(t, map[string]string{
		"toft.toml": "[batch]\nlanguage = \"python\"\n", "package.json": "{}", "requirements.txt": "x\n",
	})
	if got := DetectFramework(dir2).Framework; got != FrameworkBatchPython {
		t.Errorf("with language declared it detected %q", got)
	}
}

func TestBatchLanguageMustBeInTheRelease(t *testing.T) {
	dir := batchRepo(t, map[string]string{"toft.toml": "[batch]\nlanguage = \"go\"\n", "requirements.txt": "x\n"})
	if !strings.Contains(UnsupportedMessage(dir), "go.mod") {
		t.Errorf("refusal = %q, want the missing project file named", UnsupportedMessage(dir))
	}
}

// [batch] beside [static] is two answers to what the release is.
func TestBatchAndStaticAreRefusedTogether(t *testing.T) {
	_, err := ParseManifest(writeManifest(t, "[batch]\n[static]\nroot = \"dist\"\n"))
	if err == nil || !strings.Contains(err.Error(), "[batch] and [static]") {
		t.Errorf("err = %v, want both tables named", err)
	}
}

func TestBatchFrameworksHaveALanguage(t *testing.T) {
	for _, f := range BatchFrameworks {
		if LanguageOf(f) == "" {
			t.Errorf("%s has no language family", f)
		}
		if !f.IsBatch() {
			t.Errorf("%s does not report itself as batch", f)
		}
	}
	if FrameworkPython.IsBatch() {
		t.Error("a service framework reported itself as batch")
	}
}
