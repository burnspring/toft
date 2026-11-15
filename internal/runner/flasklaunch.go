package runner

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The Flask launcher, serviceCommand's flask case. The runner is the one
// launcher in both modes, so this is the only place a Flask app's start is
// written and the two modes cannot disagree. ADR §116.
//
// **gunicorn when the release carries it, and never installed by Toft.** A
// release whose requirements named gunicorn has venv/bin/gunicorn, and the app
// is served by it. One that did not runs on Flask's development server exactly as
// before — the platform does not add a package the app did not ask for
// (docs/invariants.md § Which third-party packages this install will run), and
// the build log says which server the app will get and how to change it.
//
// **Only with a target the detector found.** gunicorn needs `module:attr` or
// `module:factory()`, where `flask --app` searches for one itself. When the
// detector could not name the object, the Flask CLI's own search is the better
// guess, and the release keeps running on it.

// GunicornBin is where a release's venv puts gunicorn's console script. Its
// presence is the test; the launch itself goes through `python -m gunicorn`, so
// the script's shebang — an absolute path from the build directory — is never
// executed.
const GunicornBin = "venv/bin/gunicorn"

// HasGunicorn reports whether the release at dir installed gunicorn.
func HasGunicorn(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, GunicornBin))
	return err == nil && !info.IsDir()
}

// UvicornBin is where a release's venv puts uvicorn's console script — the
// test for "can this FastAPI release start", since the launcher runs
// `python -m uvicorn` and plain `fastapi` does not depend on it (BB-78).
const UvicornBin = "venv/bin/uvicorn"

// HasUvicorn reports whether the release at dir installed uvicorn.
func HasUvicorn(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, UvicornBin))
	return err == nil && !info.IsDir()
}

// FlaskServesWithGunicorn is the one spelling of "which server will this Flask
// release run on", asked by both launchers and by the builder's log line.
func FlaskServesWithGunicorn(dir, target string) bool {
	return target != "" && HasGunicorn(dir)
}

// FlaskArgs is the argv after venv/bin/python for a Flask app. module is the
// entrypoint without ".py"; target is the detector's WSGI target ("app",
// "create_app()") or "" when it found none.
//
// The base path is not here, and does not need to be: Flask is mounted at / and
// learns its prefix from X-Forwarded-Prefix through ProxyFix, a header the proxy
// sets whichever server is listening. Workers, timeouts and the rest are the
// app's to choose through GUNICORN_CMD_ARGS, which gunicorn reads itself; --bind
// on the command line wins over it, so the address stays the platform's.
func FlaskArgs(dir, module, target, bind string, port int) []string {
	if FlaskServesWithGunicorn(dir, target) {
		args := []string{"-m", "gunicorn", "--bind", net.JoinHostPort(bind, strconv.Itoa(port))}
		if gunicornHasControlSocket(dir) {
			args = append(args, "--no-control-socket")
		}
		return append(args, module+":"+target)
	}
	return []string{"-m", "flask", "--app", module, "run",
		"--host", bind, "--port", strconv.Itoa(port)}
}

// gunicornHasControlSocket reports whether the release's gunicorn is 25.1 or
// newer, which opens a control socket at $HOME/.gunicorn/gunicorn.ctl by
// default. An app's HOME is its release directory, deeper than a Unix socket
// path may be, so every start logged "[ERROR] Control server error: AF_UNIX
// path too long" (drills on trytoft, 2026-10-23). Nothing here uses gunicornc,
// so the socket is switched off — only for a version that knows the flag, since
// an older gunicorn refuses one it does not.
func gunicornHasControlSocket(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "venv", "lib", "python*", "site-packages", "gunicorn-*.dist-info"))
	for _, m := range matches {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "gunicorn-"), ".dist-info")
		parts := strings.SplitN(v, ".", 3)
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		minor := 0
		if len(parts) > 1 {
			minor, _ = strconv.Atoi(strings.TrimLeftFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
		}
		if major > 25 || (major == 25 && minor >= 1) {
			return true
		}
	}
	return false
}
