package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectFlaskWSGITarget(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"named object", "from flask import Flask\nserver = Flask(__name__)\n", "server"},
		{"annotated", "from flask import Flask\nweb: Flask = Flask(__name__)\n", "web"},
		{"conventional name from a factory call", "from pkg import build\napp = build()\n", "app"},
		{"factory", "from flask import Flask\n\ndef create_app(test_config=None):\n    app = Flask(__name__)\n    return app\n", "create_app()"},
		{"factory with a required argument", "def create_app(config):\n    pass\n", ""},
		{"object inside a function only", "def build():\n    app = Flask(__name__)\n    return app\n", ""},
		{"nothing", "print('hi')\n", ""},
		{"comparison is not assignment", "if app == 1:\n    pass\n", ""},
		{"named beats the factory", "def make_app():\n    pass\nsite = Flask(__name__)\n", "site"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "requirements.txt", "flask\n")
			write(t, dir, "app.py", tc.src)
			got := DetectFramework(dir)
			if got.Framework != FrameworkFlask {
				t.Fatalf("framework = %q", got.Framework)
			}
			if got.StartCmd != tc.want {
				t.Errorf("WSGI target = %q, want %q", got.StartCmd, tc.want)
			}
		})
	}
}

func TestFlaskServerLineNamesTheServer(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	step := func(s string) { lines = append(lines, s) }

	flaskServerLine(dir, "app", step)
	if !strings.Contains(strings.Join(lines, " "), "development server") ||
		!strings.Contains(strings.Join(lines, " "), "gunicorn") {
		t.Errorf("no-gunicorn line = %q", lines)
	}

	if err := os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "venv/bin/gunicorn", "#!/bin/sh\n")
	lines = nil
	flaskServerLine(dir, "app", step)
	if joined := strings.Join(lines, " "); !strings.Contains(joined, "Serving with gunicorn") {
		t.Errorf("gunicorn line = %q", lines)
	}
	lines = nil
	flaskServerLine(dir, "", step)
	if joined := strings.Join(lines, " "); !strings.Contains(joined, "development server") {
		t.Errorf("no-target line = %q", lines)
	}
}

// BB-78: plain `fastapi` does not pull in uvicorn, and the launcher has no
// other server — the build says so instead of the release crash-looping.
func TestAFastAPIReleaseWithoutUvicornFailsTheBuild(t *testing.T) {
	dir := t.TempDir()
	err := fastAPIServerCheck(dir, FrameworkFastAPI)
	if err == nil || !strings.Contains(err.Error(), "uvicorn") || !strings.Contains(err.Error(), "fastapi[standard]") {
		t.Fatalf("no uvicorn: err = %v", err)
	}
	if err := fastAPIServerCheck(dir, FrameworkFlask); err != nil {
		t.Errorf("a Flask release is not asked for uvicorn: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "venv/bin/uvicorn", "#!/bin/sh\n")
	if err := fastAPIServerCheck(dir, FrameworkFastAPI); err != nil {
		t.Errorf("with uvicorn installed: %v", err)
	}
}
