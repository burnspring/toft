package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectNextJS(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"next":"15.0.0"},"scripts":{"build":"next build","start":"next start"}}`)

	got := DetectFramework(dir)

	if got.Framework != FrameworkNextJS {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkNextJS)
	}
	if !got.HasBuild {
		t.Error("HasBuild should be true")
	}
}

func TestDetectGenericNode(t *testing.T) {
	// A plain Express app: no next dependency, but a start script.
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"express":"4.19.0"},"scripts":{"start":"node server.js","build":"tsc"}}`)

	got := DetectFramework(dir)

	if got.Framework != FrameworkNode {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkNode)
	}
	if got.StartCmd != "start" {
		t.Errorf("StartCmd = %q, want start", got.StartCmd)
	}
	if !got.HasBuild {
		t.Error("HasBuild should be true when a build script exists")
	}
}

func TestNodeWithoutStartScriptIsUnsupported(t *testing.T) {
	// Nothing to run — a library, not an app.
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"lodash":"4.17.21"}}`)

	if got := DetectFramework(dir); got.Framework != FrameworkUnsupported {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkUnsupported)
	}
}

func TestDetectFastAPIFindsASGIAttribute(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "requirements.txt", "fastapi==0.115.0\nuvicorn\n")
	write(t, dir, "main.py", "from fastapi import FastAPI\n\napi = FastAPI(title=\"demo\")\n")

	got := DetectFramework(dir)

	if got.Framework != FrameworkFastAPI {
		t.Fatalf("framework = %q, want %q", got.Framework, FrameworkFastAPI)
	}
	if got.Entrypoint != "main.py" {
		t.Errorf("entrypoint = %q, want main.py", got.Entrypoint)
	}
	if got.StartCmd != "api" {
		t.Errorf("ASGI attribute = %q, want api", got.StartCmd)
	}
}

func TestDetectFastAPIDefaultsToApp(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "requirements.txt", "fastapi\n")
	write(t, dir, "app.py", "from fastapi import FastAPI\napp = FastAPI()\n")

	if got := DetectFramework(dir); got.StartCmd != "app" {
		t.Errorf("ASGI attribute = %q, want app", got.StartCmd)
	}
}

func TestDetectPythonFrameworks(t *testing.T) {
	tests := []struct {
		requirements string
		want         Framework
	}{
		{"flask>=3.0\n", FrameworkFlask},
		{"streamlit==1.38.0\n", FrameworkStreamlit},
		{"fastapi\nuvicorn\n", FrameworkFastAPI},
		{"# a comment\nrequests\n", FrameworkPython},
		// Lines made only of specifier characters name no package, and once
		// panicked the build worker; they are skipped, not the file.
		{"==\n[\n;\n \n", FrameworkPython},
		{"==\n[\n;\nflask\n", FrameworkFlask},
	}

	for _, tc := range tests {
		t.Run(string(tc.want), func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "requirements.txt", tc.requirements)
			write(t, dir, "app.py", "")

			if got := DetectFramework(dir); got.Framework != tc.want {
				t.Errorf("framework = %q, want %q", got.Framework, tc.want)
			}
		})
	}
}

func TestNextJSTakesPrecedenceOverPython(t *testing.T) {
	// A repo with both should be treated as the Node app it serves.
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"next":"15.0.0"},"scripts":{"build":"next build"}}`)
	write(t, dir, "requirements.txt", "flask\n")

	if got := DetectFramework(dir); got.Framework != FrameworkNextJS {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkNextJS)
	}
}

func TestDetectGoRootMain(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/demo\n\ngo 1.22\n")
	write(t, dir, "main.go", "// Demo server.\npackage main\n\nfunc main() {}\n")

	got := DetectFramework(dir)

	if got.Framework != FrameworkGo {
		t.Fatalf("framework = %q, want %q", got.Framework, FrameworkGo)
	}
	if got.Entrypoint != "." {
		t.Errorf("Entrypoint = %q, want .", got.Entrypoint)
	}
	if !got.HasBuild {
		t.Error("HasBuild should be true for Go apps")
	}
}

func TestDetectGoCmdDir(t *testing.T) {
	// Standard layout: no root main package, entrypoint under cmd/.
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/demo\n")
	write(t, dir, "lib.go", "package demo\n")
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "server"), 0755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "cmd", "server"), "main.go", "package main\n\nfunc main() {}\n")

	got := DetectFramework(dir)

	if got.Framework != FrameworkGo {
		t.Fatalf("framework = %q, want %q", got.Framework, FrameworkGo)
	}
	if got.Entrypoint != "./cmd/server" {
		t.Errorf("Entrypoint = %q, want ./cmd/server", got.Entrypoint)
	}
}

func TestGoLibraryIsUnsupported(t *testing.T) {
	// A module with no main package cannot be run.
	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/lib\n")
	write(t, dir, "lib.go", "package lib\n")

	if got := DetectFramework(dir); got.Framework != FrameworkUnsupported {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkUnsupported)
	}
}

func TestNodeTakesPrecedenceOverGo(t *testing.T) {
	// A repo with both is treated as the Node app it declares a start script for.
	dir := t.TempDir()
	write(t, dir, "package.json", `{"scripts":{"start":"node server.js"}}`)
	write(t, dir, "go.mod", "module example.com/demo\n")
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")

	if got := DetectFramework(dir); got.Framework != FrameworkNode {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkNode)
	}
}

func TestEmptyRepoIsUnsupported(t *testing.T) {
	if got := DetectFramework(t.TempDir()); got.Framework != FrameworkUnsupported {
		t.Errorf("framework = %q, want %q", got.Framework, FrameworkUnsupported)
	}
}

// A Python repository with no file to start is refused at detection, not
// deployed and left to crash-loop. Before this, `detectPythonEntrypoint`
// returned "app.py" whether or not it existed: a repository of notebooks and a
// requirements.txt installed cleanly, published a release, reported a green
// deploy, and then died forever on a file that was never there.
func TestPythonWithNoEntrypointIsRefused(t *testing.T) {
	for _, decl := range []string{"requirements.txt", "pyproject.toml", "Pipfile"} {
		t.Run(decl, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, decl, "pandas\n")
			write(t, dir, "lib/helpers.py", "def go():\n    pass\n")

			got := DetectFramework(dir)
			if got.Framework != FrameworkUnsupported {
				t.Fatalf("framework = %q, want %q", got.Framework, FrameworkUnsupported)
			}

			msg := UnsupportedMessage(dir)
			if !strings.Contains(msg, decl) {
				t.Errorf("message does not name %s:\n%s", decl, msg)
			}
			for _, name := range pythonEntrypoints {
				if !strings.Contains(msg, name) {
					t.Errorf("message does not name the candidate %s:\n%s", name, msg)
				}
			}
			// The generic "declare a [static] root" advice is the wrong answer
			// for somebody whose repository is a Python app.
			if strings.Contains(msg, "[static]") {
				t.Errorf("message offers static hosting to a Python app:\n%s", msg)
			}
		})
	}
}

// Every candidate is actually honoured, so the list in the message is the list
// the detector uses.
func TestPythonEntrypointCandidates(t *testing.T) {
	for _, name := range pythonEntrypoints {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "requirements.txt", "requests\n")
			write(t, dir, name, "")

			got := DetectFramework(dir)
			if got.Framework != FrameworkPython {
				t.Fatalf("framework = %q, want %q", got.Framework, FrameworkPython)
			}
			if got.Entrypoint != name {
				t.Errorf("entrypoint = %q, want %q", got.Entrypoint, name)
			}
		})
	}
}

// BB-4: a lone stdlib app.py — no manifest, no HTML — is a plain Python app
// with no dependencies, not an unsupported tree told to add the app.py it has.
func TestDetectPythonEntryFileWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "app.py", "from http.server import HTTPServer\n")

	got := DetectFramework(dir)
	if got.Framework != FrameworkPython || got.Entrypoint != "app.py" {
		t.Fatalf("got %q / %q, want python / app.py", got.Framework, got.Entrypoint)
	}
}

// The narrow half of the same rule: a folder of HTML that ships a preview
// server.py stays a static site.
func TestDetectStaticSiteWithPreviewServerStaysStatic(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "index.html", "<!doctype html><title>x</title>\n")
	write(t, dir, "server.py", "import http.server\n")

	if got := DetectFramework(dir); got.Framework != FrameworkStatic {
		t.Fatalf("framework = %q, want static", got.Framework)
	}
}

// The generic hint never names a starting file the tree already holds.
func TestUnsupportedHintOmitsFilesAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.txt", "hello\n")
	hint := unsupportedHint(dir)
	for _, want := range []string{"app.py", "app.R", "package.json", "go.mod"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint = %q, want %s named", hint, want)
		}
	}
	for _, not := range []string{"server.js", "main.go"} {
		if strings.Contains(hint, not) {
			t.Errorf("hint = %q offers %s as a file an app starts from alone", hint, not)
		}
	}
}

// BB-19: a lone server.js is told to add a package.json, not a main.go.
func TestLoneNodeEntryNamesPackageJSON(t *testing.T) {
	for _, entry := range []string{"server.js", "index.js", "app.mjs", "server.ts"} {
		dir := t.TempDir()
		write(t, dir, entry, "require('http')\n")
		if got := DetectFramework(dir).Framework; got != FrameworkUnsupported {
			t.Fatalf("%s: framework = %q, want unsupported", entry, got)
		}
		hint := unsupportedHint(dir)
		if !strings.Contains(hint, "package.json") || !strings.Contains(hint, `"node `+entry+`"`) || strings.Contains(hint, "main.go") {
			t.Errorf("%s: hint = %q, want package.json with a start script and no main.go", entry, hint)
		}
		msg := UnsupportedMessage(dir)
		if !strings.Contains(msg, "package.json") || strings.Contains(msg, "HTML document") {
			t.Errorf("%s: message = %q, want package.json named and no static-site sentence", entry, msg)
		}
	}
}

// BB-19: a lone main.go is told to add a go.mod, not a server.js.
func TestLoneGoMainNamesGoMod(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	if got := DetectFramework(dir).Framework; got != FrameworkUnsupported {
		t.Fatalf("framework = %q, want unsupported", got)
	}
	hint := unsupportedHint(dir)
	if !strings.Contains(hint, "go.mod") || !strings.Contains(hint, "go mod init") || strings.Contains(hint, "server.js") {
		t.Errorf("hint = %q, want go.mod named and no server.js", hint)
	}
	msg := UnsupportedMessage(dir)
	if !strings.Contains(msg, "go mod init") || strings.Contains(msg, "HTML document") {
		t.Errorf("message = %q, want go.mod named and no static-site sentence", msg)
	}
}

// BB-5: storage_unknown's hint gives a publisher told the name by an
// administrator a way forward, without confirming the name exists.
func TestStorageUnknownHintNamesTheDelegation(t *testing.T) {
	hint := codeHint(StorageUnknown)
	if !strings.Contains(hint, "administrator") || !strings.Contains(hint, "delegate") {
		t.Errorf("hint = %q, want the administrator's delegation named", hint)
	}
	if strings.Contains(hint, "this install has") {
		t.Errorf("hint = %q still loops on a name the install has", hint)
	}
}
