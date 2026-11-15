package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The regression that would be silent, isolated half.
//
// See internal/runtime's twin: what a notebook's file server is rooted at
// decides what becomes public, and a root of the release directory publishes
// requirements.txt, the notebook source and the whole of venv/. Both launchers
// are asserted because splitting them ships an app that serves in shared mode
// and leaks in isolated — the worst version of every bug in this seam.
func TestNotebookServesTheRenderDirectoryAndNeverTheRelease(t *testing.T) {
	const release = "/workspace/release"
	name, args, err := serviceCommand(&BootstrapResponse{
		Framework: "notebook", Entrypoint: "analysis.ipynb", BasePath: "/apps/demo",
		Port: 8080, BindAddress: "0.0.0.0",
	}, release)
	if err != nil {
		t.Fatalf("serviceCommand: %v", err)
	}
	if name != "/proc/self/exe" {
		t.Errorf("command = %q, want the runner binary itself", name)
	}
	joined := strings.Join(args, " ")
	want := filepath.Join(release, "toft-render")
	if !strings.Contains(joined, "--dir "+want) {
		t.Fatalf("args = %q, want --dir %s", joined, want)
	}
	if strings.Contains(joined, "--dir "+release+" ") {
		t.Fatalf("the file server is rooted at the release directory: %q", joined)
	}
}

func TestMarimoStartCommand(t *testing.T) {
	name, args, err := serviceCommand(&BootstrapResponse{
		Framework: "marimo", Entrypoint: "notebook.py", BasePath: "/apps/demo",
		Port: 8080, BindAddress: "0.0.0.0",
	}, "/workspace/release")
	if err != nil {
		t.Fatalf("serviceCommand: %v", err)
	}
	if name != "venv/bin/python" {
		t.Errorf("command = %q, want the release's own interpreter", name)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-m marimo run notebook.py", "--base-url /apps/demo", "--headless", "--no-token",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args = %q, want it to contain %q", joined, want)
		}
	}
	if strings.Contains(joined, "--include-code") {
		t.Errorf("args ship the notebook's source to the browser: %q", joined)
	}
}

// A notebook release carries a venv even though its workload never opens one,
// because the build executed the cells in it — so it is asked the Python
// question, and that is why its image family is python rather than static.
func TestNotebookIsRepairedLikeAPythonRelease(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "toft-render"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, framework := range []string{"notebook", "marimo"} {
		t.Run(framework, func(t *testing.T) {
			broken, what := Unrunnable(framework, dir)
			if !broken {
				t.Fatal("a release with no venv/bin/python was called runnable")
			}
			if !strings.Contains(what, "virtual environment") {
				t.Errorf("reason = %q, want it to name the venv", what)
			}
		})
	}
}

// A rebuild inside a pod is a repair, and a repair restores what dangles. A
// venv is bound to an absolute interpreter path; the rendered document is a
// file and is already in this release. Re-executing every cell here would
// produce output the next start throws away, because a start replaces the tree.
func TestRepairingANotebookDoesNotReRender(t *testing.T) {
	dir := t.TempDir()
	steps, cleanup, err := buildSteps(&BootstrapResponse{
		Framework: "notebook", Entrypoint: "analysis.ipynb",
	}, dir)
	defer cleanup()
	if err != nil {
		t.Fatalf("buildSteps: %v", err)
	}

	sawRenderer := false
	for _, s := range steps {
		joined := s.name + " " + strings.Join(s.args, " ")
		if strings.Contains(joined, "nbconvert") && strings.Contains(joined, "pip install") {
			sawRenderer = true
		}
		if strings.Contains(joined, "--execute") || strings.Contains(joined, "--output-dir") {
			t.Errorf("a repair re-renders the notebook: %q", joined)
		}
	}
	if !sawRenderer {
		t.Error("a repaired notebook venv does not carry the renderer the deploy put in it")
	}
}

// marimo *is* the workload, so it has to be in the venv a repair rebuilds.
func TestRepairingMarimoInstallsMarimo(t *testing.T) {
	dir := t.TempDir()
	steps, cleanup, err := buildSteps(&BootstrapResponse{
		Framework: "marimo", Entrypoint: "notebook.py",
	}, dir)
	defer cleanup()
	if err != nil {
		t.Fatalf("buildSteps: %v", err)
	}
	for _, s := range steps {
		if strings.Contains(strings.Join(s.args, " "), "install marimo") {
			return
		}
	}
	t.Error("a repaired marimo venv has no marimo in it")
}
