// Package config is where `toft` works out which install it is talking to and
// which app it is talking about.
//
// **One resolution order, for everything.** A CLI that resolves `--app`
// differently from `--install` is one whose behaviour has to be learned rather
// than known, so there is a single ordered walk and every setting goes through
// it:
//
//	flag
//	  → TOFT_* environment variable
//	  → .toft.toml            (uncommitted, this directory)
//	  → toft.toml [app.installs."<url>"]
//	  → toft.toml [app]
//	  → ~/.config/toft/config.toml
//	  → the built-in default
//
// The one thing that reads oddly and is deliberate: the flag beats the
// environment. The first draft of this design put the environment first so that
// "a pipeline never picks up a developer's file" — and the file is still below
// both, which is what that sentence was actually protecting. A flag typed on
// this one invocation is the most specific statement of intent there is, and an
// order with an exception in it is not one order.
//
// The credential does **not** come through here. It has its own file, its own
// permissions and its own rule (never a flag) — see credentials.go.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Key names one resolvable setting. There are two today; the mechanism is
// general because the second one existing is what makes the order a rule rather
// than a coincidence.
type Key string

const (
	// KeyInstall is the Toft this command talks to: a URL, or an alias
	// naming an entry in the credentials file.
	KeyInstall Key = "install"
	// KeyApp is the app slug this directory deploys to.
	KeyApp Key = "app"
)

// envVar is the environment variable for a key. Spelled out rather than
// derived, so a rename here cannot silently change what a CI job is reading.
var envVar = map[Key]string{
	KeyInstall: "TOFT_URL",
	KeyApp:     "TOFT_APP",
}

// Value is a resolved setting and where it came from. The origin is not
// decoration: "no app linked" and "the app named in toft.toml does not
// exist here" send somebody to two different places, and a refusal that names
// the layer it read is the difference.
type Value struct {
	Value  string
	Origin string
}

// Found reports whether anything set this.
func (v Value) Found() bool { return v.Value != "" }

// Resolver holds the layers, in order, for one invocation.
//
// It is built once at startup — including reading the two manifests off disk —
// so that a command which asks for the same setting twice cannot get two
// answers, and so that a file changing under a long `toft deploy` cannot change
// its mind halfway.
type Resolver struct {
	flags     map[Key]string
	local     *Manifest // .toft.toml, uncommitted
	project   *Manifest // toft.toml, committed
	user      map[string]string
	lookupEnv func(string) string
}

// NewResolver reads what is on disk and returns the layers for this directory.
//
// A missing or unreadable file is not an error: `toft` runs in directories that
// have none of these, and refusing to start because a config file has a typo in
// it would make the tool unusable in exactly the moment somebody is trying to
// fix that typo. What *is* reported is a file that exists and cannot be parsed,
// because silently ignoring it would leave somebody staring at a setting they
// have written down and the CLI insisting it is unset.
func NewResolver(dir string, flags map[Key]string) (*Resolver, error) {
	r := &Resolver{flags: flags, lookupEnv: os.Getenv}

	var err error
	if r.local, err = ReadManifest(filepath.Join(dir, LocalManifestFile)); err != nil {
		return nil, err
	}
	if r.project, err = ReadManifest(filepath.Join(dir, ManifestFile)); err != nil {
		return nil, err
	}
	if r.user, err = readUserConfig(); err != nil {
		return nil, err
	}
	return r, nil
}

// Install is the install this command talks to.
//
// Resolved before App, and that ordering is a real dependency rather than an
// accident of the code: the `[app.installs."<url>"]` layer is keyed by the
// install's URL, so a repository that deploys to both staging and production
// cannot say which app it means until it knows which Toft it is talking
// to.
func (r *Resolver) Install() Value {
	if v := r.flags[KeyInstall]; v != "" {
		return Value{v, "--install"}
	}
	if v := strings.TrimSpace(r.lookupEnv(envVar[KeyInstall])); v != "" {
		return Value{v, envVar[KeyInstall]}
	}
	if r.local != nil && r.local.App.Install != "" {
		return Value{r.local.App.Install, LocalManifestFile + " [app] install"}
	}
	if r.project != nil && r.project.App.Install != "" {
		return Value{r.project.App.Install, ManifestFile + " [app] install"}
	}
	if v := r.user["default_install"]; v != "" {
		return Value{v, userConfigPath()}
	}
	// The default comes from the credentials file's `default =` line, which is
	// written by `toft login` and is the answer for the overwhelmingly common
	// case of one install. Read there rather than here because it lives beside
	// the token it belongs to.
	return Value{}
}

// App is the app slug this directory deploys to, given the install it is
// talking to.
func (r *Resolver) App(installURL string) Value {
	if v := r.flags[KeyApp]; v != "" {
		return Value{v, "--app"}
	}
	if v := strings.TrimSpace(r.lookupEnv(envVar[KeyApp])); v != "" {
		return Value{v, envVar[KeyApp]}
	}
	for _, m := range []struct {
		manifest *Manifest
		file     string
	}{{r.local, LocalManifestFile}, {r.project, ManifestFile}} {
		if m.manifest == nil {
			continue
		}
		// The per-install table first: a checkout that targets two installs
		// says so, and neither entry is wrong.
		if slug := m.manifest.App.slugFor(installURL); slug != "" {
			return Value{slug, fmt.Sprintf("%s [app.installs.%q]", m.file, installURL)}
		}
		if m.manifest.App.Slug != "" {
			return Value{m.manifest.App.Slug, m.file + " [app]"}
		}
	}
	if v := r.user["default_app"]; v != "" {
		return Value{v, userConfigPath()}
	}
	return Value{}
}

// Name is the display name toft.toml proposes for a new app: `[app] name`,
// .toft.toml before toft.toml. Only `toft create` asks, and its --name wins.
func (r *Resolver) Name() Value {
	for _, m := range []struct {
		manifest *Manifest
		file     string
	}{{r.local, LocalManifestFile}, {r.project, ManifestFile}} {
		if m.manifest != nil && strings.TrimSpace(m.manifest.App.Name) != "" {
			return Value{strings.TrimSpace(m.manifest.App.Name), m.file + " [app] name"}
		}
	}
	return Value{}
}

// userConfigDir is where the two files live: the credentials, and the settings
// somebody may edit. XDG on Unix, %AppData% on Windows.
func userConfigDir() string {
	if runtime.GOOS == "windows" {
		if app := os.Getenv("AppData"); app != "" {
			return filepath.Join(app, "toft")
		}
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "toft")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "toft")
}

func userConfigPath() string {
	dir := userConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.toml")
}

// readUserConfig reads the settings file — which holds no credentials, ever.
// The separation from credentials.toml is the point: a file with a secret in it
// gets different permissions, different backup advice and different blast
// radius from a file people are meant to edit and paste into tickets.
func readUserConfig() (map[string]string, error) {
	path := userConfigPath()
	if path == "" {
		return nil, nil
	}
	var doc struct {
		DefaultInstall string `toml:"default_install"`
		DefaultApp     string `toml:"default_app"`
	}
	if err := decodeTOMLFile(path, &doc); err != nil {
		return nil, err
	}
	return map[string]string{
		"default_install": doc.DefaultInstall,
		"default_app":     doc.DefaultApp,
	}, nil
}
