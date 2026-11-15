package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"toft/cli/internal/config"
)

const linkUsage = `toft link <slug> — record which app this directory deploys to

Writes an [app] table into toft.toml, which is committed: an app slug is
not a secret and belongs in version control with the code that deploys to it.

  [app]
  slug = "phase-test"

A checkout that targets two installs says so, and neither entry is wrong:

  [app.installs."https://toft.corp"]
  slug = "phase-test"

Flags:
  --local     Write .toft.toml instead, uncommitted, and offer to
              .gitignore it — for a fork that deploys somewhere private
  --install   Write the per-install table rather than the plain [app] one
  --json      The file written, the slug and the install
`

func cmdLink(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("link", linkUsage)
	g.register(fs)
	local := fs.Bool("local", false, "write .toft.toml instead")
	if err := parseOperands(fs, args, 1); err != nil {
		return skipHelp(err)
	}
	if a := strings.TrimSpace(fs.Arg(0)); a != "" && g.app != "" && g.app != a {
		return usageCodef("unexpected_argument", "two apps named: %q and --app %q — `toft link <slug>` takes one", a, g.app)
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	slug := strings.TrimSpace(fs.Arg(0))
	if slug == "" {
		slug = g.app
	}
	if slug == "" {
		return usageCodef("missing_argument", "which app? `toft link <slug>` — `toft apps` lists what you can see")
	}

	// Resolved before it is written, so a typo fails here rather than on the
	// next deploy with a message about the deploy. This is also the check that
	// makes `toft link` refuse an app somebody cannot see, without ever saying
	// which of the two reasons applies.
	app, err := s.resolveApp(slug)
	if err != nil {
		return err
	}

	// The per-install form is written when the flag named an install
	// explicitly, which is the developer saying "this directory goes to more
	// than one place". Otherwise the plain [app] table, which is what the
	// overwhelming majority of repositories want.
	perInstall := g.install != ""
	file := config.ManifestFile
	if *local {
		file = config.LocalManifestFile
	}
	path := filepath.Join(e.dir, file)

	if err := writeAppTable(path, app.Slug, s.InstallURL, perInstall); err != nil {
		return err
	}
	if *local {
		if err := offerGitignore(e, filepath.Join(e.dir, ".gitignore")); err != nil {
			return err
		}
	}
	// The answer is which file now says what, because that is the thing a
	// caller might have to undo — and on a run that wrote the per-install table
	// it is not derivable from the arguments.
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"app":         app.Slug,
			"install":     s.InstallURL,
			"file":        path,
			"per_install": perInstall,
		})
	}
	e.out.Say("%s %s → %s", e.out.OK("✓"), file, e.out.Bold(app.Slug))
	return nil
}

// writeAppTable appends the table rather than rewriting the file.
//
// The file is shared — `[jobs]` and `[static]` live in it — and a rewrite
// through a TOML encoder would lose every comment somebody wrote in it. An
// append is crude and it is the only edit that cannot damage what it did not
// write.
func writeAppTable(path, slug, installURL string, perInstall bool) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(existing)

	var table string
	if perInstall {
		// A table for this install already, under any spelling of its
		// address: a second one with the same header is a TOML error that
		// refuses every command in the directory, and one spelled differently
		// is a second valid table that slugFor picks between at random
		// (security review 2026-11-15, CL-3).
		var m config.Manifest
		if _, err := toml.Decode(text, &m); err != nil {
			return err
		}
		want := config.NormalizeInstallURL(installURL)
		for url, entry := range m.App.Installs {
			if config.NormalizeInstallURL(url) != want {
				continue
			}
			if entry.Slug == slug {
				return nil
			}
			return usageCodef("app_already_named", "%s already links %s to %s — edit it by hand",
				filepath.Base(path), installURL, entry.Slug)
		}
		table = fmt.Sprintf("[app.installs.%q]\nslug = %q\n", installURL, slug)
		if !strings.Contains(text, "[app]") {
			// The plain table has to exist for the per-install one to be a
			// sub-table of it in a file somebody reads by eye, even though TOML
			// does not require it.
			table = fmt.Sprintf("[app]\nslug = %q\n\n", slug) + table
		}
	} else {
		table = fmt.Sprintf("[app]\nslug = %q\n", slug)
	}

	if strings.Contains(text, table) {
		return nil
	}
	if strings.Contains(text, "\n[app]") || strings.HasPrefix(text, "[app]") {
		if !perInstall {
			// Already this slug, whatever else the table carries and in
			// whatever order: `install = …` above `slug`, which the drills
			// runbook advises, defeated the text match and refused the file
			// for naming the very slug being linked.
			var m struct {
				App struct {
					Slug string `toml:"slug"`
				} `toml:"app"`
			}
			if _, err := toml.Decode(text, &m); err == nil && m.App.Slug == slug {
				return nil
			}
			// An [app] table with no slug -- `name` alone, which is what the
			// manifest's own examples carry -- gets the one line it lacks,
			// under its header. Refusing it made the next step `toft create`
			// prints fail in every such project (docs/bugs 2026-10-22-e2e-cli
			// #3). A table that already names a slug is somebody's decision,
			// and is still left alone.
			if edited, ok := addSlugToAppTable(text, slug); ok {
				return os.WriteFile(path, []byte(edited), 0644)
			}
			// Usage, not failure: nothing was sent to any install, the file on
			// disk is untouched, and the sentence already names the two ways
			// forward. Exit 1 here said "the platform did the thing and it
			// failed" about a refusal the platform never saw.
			return usageCodef("app_already_named", "%s already has an [app] table naming a slug — edit it by hand, "+
				"or use --install to add a per-install entry beside it", filepath.Base(path))
		}
	}

	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if text != "" {
		text += "\n"
	}
	text += "# Which app this directory deploys to. Read by `toft deploy`;\n" +
		"# the platform ignores tables it does not know.\n" + table
	return os.WriteFile(path, []byte(text), 0644)
}

// addSlugToAppTable inserts `slug = "…"` as the first line of the [app] table
// when the table has no slug of its own, and reports whether it did. A line
// edit rather than a TOML round trip, for the reason writeAppTable gives: the
// file's comments and its other tables are somebody else's.
func addSlugToAppTable(text, slug string) (string, bool) {
	lines := strings.SplitAfter(text, "\n")
	header := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if header < 0 {
			if t == "[app]" {
				header = i
			}
			continue
		}
		if strings.HasPrefix(t, "[") {
			break
		}
		if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == "slug" {
			return text, false
		}
	}
	if header < 0 {
		return text, false
	}
	if !strings.HasSuffix(lines[header], "\n") {
		lines[header] += "\n"
	}
	out := append([]string{}, lines[:header+1]...)
	out = append(out, fmt.Sprintf("slug = %q\n", slug))
	out = append(out, lines[header+1:]...)
	return strings.Join(out, ""), true
}

// offerGitignore adds the local override to .gitignore. It is the point of
// `--local` — a file whose whole purpose is not being committed, committed, is
// the worst of both.
func offerGitignore(e *env, path string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), config.LocalManifestFile) {
		return nil
	}
	text := string(existing)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += config.LocalManifestFile + "\n"
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		return err
	}
	e.out.Dim("added %s to .gitignore", config.LocalManifestFile)
	return nil
}
