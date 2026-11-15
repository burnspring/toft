package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlaskArgs(t *testing.T) {
	dir := t.TempDir()
	devServer := "-m flask --app main run --host 0.0.0.0 --port 8080"

	// No gunicorn in the release: the Flask CLI, whatever the target.
	if got := strings.Join(FlaskArgs(dir, "main", "app", "0.0.0.0", 8080), " "); got != devServer {
		t.Errorf("without gunicorn = %q", got)
	}

	if err := os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, GunicornBin), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(FlaskArgs(dir, "main", "app", "0.0.0.0", 8080), " "); got != "-m gunicorn --bind 0.0.0.0:8080 main:app" {
		t.Errorf("with gunicorn = %q", got)
	}
	if got := strings.Join(FlaskArgs(dir, "wsgi", "create_app()", "127.0.0.1", 3100), " "); got != "-m gunicorn --bind 127.0.0.1:3100 wsgi:create_app()" {
		t.Errorf("factory = %q", got)
	}
	if got := strings.Join(FlaskArgs(dir, "main", "app", "::1", 3100), " "); !strings.Contains(got, "--bind [::1]:3100") {
		t.Errorf("IPv6 bind = %q", got)
	}
	// A target the detector could not find keeps the CLI, which searches itself.
	if got := strings.Join(FlaskArgs(dir, "main", "", "0.0.0.0", 8080), " "); got != devServer {
		t.Errorf("no target = %q", got)
	}
}

// gunicorn 25.1 and later open a control socket under $HOME, which is the
// release directory and too deep for a Unix socket path: every start logged an
// ERROR. The socket is switched off for a gunicorn that has one, and only then,
// because an older gunicorn refuses the flag.
func TestGunicornsControlSocketIsOffWhereItExists(t *testing.T) {
	for _, tc := range []struct {
		version string
		off     bool
	}{{"26.2.0", true}, {"25.1.0", true}, {"25.0.3", false}, {"23.0.0", false}} {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0o755)
		os.WriteFile(filepath.Join(dir, GunicornBin), []byte("#!/bin/sh\n"), 0o755)
		os.MkdirAll(filepath.Join(dir, "venv", "lib", "python3.15", "site-packages", "gunicorn-"+tc.version+".dist-info"), 0o755)
		got := strings.Join(FlaskArgs(dir, "main", "app", "127.0.0.1", 3100), " ")
		if strings.Contains(got, "--no-control-socket") != tc.off {
			t.Errorf("gunicorn %s: %q", tc.version, got)
		}
		if !strings.HasSuffix(got, " main:app") {
			t.Errorf("gunicorn %s: the target must stay last: %q", tc.version, got)
		}
	}
}

func TestIsolatedFlaskUsesTheSharedLauncher(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "venv", "bin"), 0o755)
	os.WriteFile(filepath.Join(dir, GunicornBin), []byte("x"), 0o755)
	boot := &BootstrapResponse{Framework: "flask", Entrypoint: "app.py", StartCmd: "app", Port: 8080, BindAddress: "0.0.0.0"}
	name, args, err := serviceCommand(boot, dir)
	if err != nil {
		t.Fatal(err)
	}
	if name != "venv/bin/python" || strings.Join(args, " ") != "-m gunicorn --bind 0.0.0.0:8080 app:app" {
		t.Errorf("isolated flask = %s %q", name, args)
	}
}
