package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"toft/cli/internal/bundle"
	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

// `toft create` — a command, not a flag on `toft deploy`.
//
// ADR §42 refused to create an app "as a side effect of a typo in a directory
// name". That refusal is about implicitness, and the counter-argument is not
// that the risk was overstated: it is that a separate command is not a side
// effect of anything. A person who types `toft create` has asked for an app; a
// person who types `toft deploy` has not, and still cannot get one.
//
// `toft deploy --create` was the alternative and it is the shape §42 was
// refusing: `--creat` is a usage error, but a `--create` left in a script after
// the app exists is not.

const createUsage = `toft create [dir] — create an app and deploy this tree into it

Claims a slug in the estate's namespace, so it says which one before it takes
it and asks, unless --yes.

  toft create                     Bundle this directory into a new app
  toft create ./service           Bundle another one
  toft create --repo <url>        Create from a repository instead

Flags:
  --app <slug>          The slug to claim. Otherwise toft.toml, then the
                        directory's own name.
  --name <name>         The display name. Otherwise toft.toml's [app] name,
                        otherwise the slug. Read at create only.
  --repo <url>          Create from a repository; bundles nothing
  --branch <branch>     With --repo; otherwise the repository's default
  --subdir <folder>     With --repo: build this folder of the repository, for
                        one that holds more than one app
  --execution-mode <m>  shared or isolated, if this install lets you choose
  --owner-group <g>     The group that will own the app, by name or id.
                        Required with a deploy key, which belongs to nobody
  --link                Write the slug into toft.toml on success
  --logs                Interleave the first build's log
  --no-watch            Create it and return; print the deployment id
  --json                The new app as JSON
  --verify <path>       Once the first deploy succeeds, request this path as
                        you and fail unless it answers 2xx or 3xx
  --verify-timeout <d>  How long a 5xx may be retried with --verify (default 30s)
  --yes                 Do not ask
`

func cmdCreate(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("create", createUsage)
	g.register(fs)
	name := fs.String("name", "", "the display name")
	repo := fs.String("repo", "", "create from a repository")
	branch := fs.String("branch", "", "with --repo")
	subdir := fs.String("subdir", "", "with --repo: the folder of the repository to build")
	mode := fs.String("execution-mode", "", "shared or isolated")
	ownerGroup := fs.String("owner-group", "", "the group that will own the app")
	link := fs.Bool("link", false, "write the slug into toft.toml")
	showLogs := fs.Bool("logs", false, "interleave the first build's log")
	e.verifyFlags(fs)
	noWatch := fs.Bool("no-watch", false, "create it and return")
	taken, rest := takeOperand(args)
	if err := parseAfter(fs, rest, taken); err != nil {
		return skipHelp(err)
	}
	if e.verify != "" {
		if err := refuseAbsoluteURL(e.verify); err != nil {
			return err
		}
		if err := refuseDotSegments("/" + strings.TrimPrefix(e.verify, "/")); err != nil {
			return err
		}
	}
	if *noWatch && e.verify != "" {
		return usageCodef("conflicting_flags", "--verify needs the deploy watched; drop --no-watch")
	}
	dir := operandOr(taken, fs)

	// Two doors, and saying both is a usage error rather than a silent
	// preference: what is being created is the same thing either way, and where
	// the code comes from is the one thing the flags disagree about.
	if *repo != "" && dir != "" {
		return usageCodef("conflicting_flags", "--repo and a directory are two different sources — pass one")
	}
	if *branch != "" && *repo == "" {
		return usageCodef("conflicting_flags", "--branch only means something with --repo")
	}
	if *subdir != "" && *repo == "" {
		return usageCodef("conflicting_flags", "--subdir only means something with --repo — a bundle is already the folder you name")
	}
	// **The directory is checked before anything is said about it.** `toft create
	// toft-testdrive` reads as "create an app called toft-testdrive" and is a
	// directory argument; it used to print "Creating …" and then fail on the
	// bundle. A missing directory that looks like a slug gets the spelling that
	// was probably meant.
	if dir != "" {
		if err := checkCreateDir(dir); err != nil {
			return err
		}
	}

	// The directory before the session, as deploy and check do: its
	// toft.toml may pin the install, and the resolver reads e.dir (security
	// review 2026-11-15, CL-4).
	if dir != "" {
		e.dir = dir
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	slug, origin, exact, err := createSlug(e, &g, s.InstallURL, *repo, *subdir)
	if err != nil {
		return err
	}
	if err := refuseStatedNonSlug(slug, origin, exact); err != nil {
		return err
	}
	// The display name: --name, else toft.toml's [app] name, else the slug
	// (orDefault, at the request). Read here and never on a deploy.
	if *name == "" {
		if resolver, rerr := config.NewResolver(e.dir, nil); rerr == nil {
			*name = resolver.Name().Value
		}
	}
	// The owner is settled before the slug is claimed and before a byte is
	// uploaded: a deploy key with no group is refused by the install only
	// after the bundle has gone, and a name that resolves to nothing would
	// be the same wasted upload.
	groupID, err := ownerGroupFor(s, "toft create", *ownerGroup)
	if err != nil {
		return err
	}

	// **The slug is named before it is claimed.** It becomes a URL in the
	// estate's namespace, and the namespace is never entered on a value the
	// person has not read.
	e.out.Say("Creating %s on %s", e.out.Bold(slug), s.InstallURL)
	e.out.Dim("  the slug comes from %s", origin)
	if err := confirm(e, &g, fmt.Sprintf("claim the slug %q", slug)); err != nil {
		return err
	}

	var created struct {
		App        appSummary `json:"app"`
		Deployment struct {
			ID string `json:"id"`
		} `json:"deployment"`
	}
	if *repo != "" {
		err = createFromRepo(s, slug, exact, *name, *repo, *branch, *subdir, *mode, groupID, &created)
	} else {
		err = createFromBundle(e, s, &g, slug, exact, *name, *mode, groupID, &created)
	}
	if err != nil {
		return err
	}

	app := &created.App
	// A derived slug that is taken is uniquified by the server, so the app may
	// not have the name that was confirmed. (A stated one is refused instead.) Said prominently, because every later command in
	// the script is about the slug that was actually claimed.
	// A slug the server normalized (dots, capitals, spaces) is not one that was
	// taken, and saying it was sent people looking for an app that never existed.
	if app.Slug != slug {
		if norm := normalizeSlug(slug); app.Slug == norm {
			e.out.Warn("%q is not a slug — lowercase letters, digits and dashes — so this app is %s", slug, e.out.Bold(app.Slug))
		} else if len(norm) > maxSlugLen {
			// A proposed slug over the limit is cut to fit by the server, and
			// "was taken" would send somebody looking for an app that never
			// existed.
			e.out.Warn("%q is longer than a slug may be (%d characters), so this app is %s", norm, maxSlugLen, e.out.Bold(app.Slug))
		} else {
			e.out.Warn("the slug %q was taken; this app is %s", norm, e.out.Bold(app.Slug))
		}
	}
	// Under --json stdout is NDJSON and the URL is in the result object, so a
	// bare line of text here was line one of a stream every parser chokes on.
	if g.json {
		e.out.Say("%s", s.appURL(app))
	} else {
		url := s.appURL(app)
		e.out.Answer("%s", e.out.Link(url, url))
	}

	if *link {
		path := filepath.Join(e.dir, config.ManifestFile)
		if err := writeAppTable(path, app.Slug, s.InstallURL, g.install != ""); err != nil {
			// The app exists. Failing the command over the convenience file
			// would report a create that worked as a create that did not.
			e.out.Warn("could not write %s: %v", config.ManifestFile, err)
		} else {
			e.out.Say("%s %s → %s", e.out.OK("✓"), config.ManifestFile, e.out.Bold(app.Slug))
		}
	} else {
		suggest(e, "Link this directory to it with", "toft link "+app.Slug, "links this directory")
	}

	// **--json watches too.** It used to print the new app and exit 0 while the
	// first build was still running, so a script that switched to JSON to parse
	// the answer stopped noticing a create whose deploy failed. Under --json a
	// watched create is a deploy's answer — NDJSON frames, then one result
	// object with `reason` and the matching exit code; only --no-watch keeps
	// the bare app document.
	if g.json && (created.Deployment.ID == "" || *noWatch) {
		doc := map[string]any{
			"app":           app.Slug,
			"app_id":        app.ID,
			"url":           s.appURL(app),
			"deployment_id": created.Deployment.ID,
		}
		// The bundle's warnings are this client's, known already: an
		// unwatched create says them as a watched one does (BB-017).
		if warnings, details := clientWarnings(e); len(warnings) > 0 {
			doc["warnings"], doc["warning_details"] = warnings, details
		}
		return json.NewEncoder(e.out.Stdout).Encode(doc)
	}
	if created.Deployment.ID == "" || *noWatch {
		if created.Deployment.ID != "" {
			suggest(e, "Watch the first build with",
				fmt.Sprintf("toft deploy --watch %s --app %s", created.Deployment.ID, app.Slug),
				"watches the first build")
		}
		return nil
	}
	// The first deployment of a brand-new app: a failed --verify has nothing
	// to roll back to, and must not say it has (BB-045).
	firstDeploys[app.Slug] = true
	defer delete(firstDeploys, app.Slug)
	return watchDeploy(e, s, app, created.Deployment.ID, &g, *showLogs)
}

// checkCreateDir refuses a directory argument that is not one, exit 2, and names
// --app when the word looks like a slug rather than a path.
func checkCreateDir(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	if slugLike.MatchString(dir) {
		return usageCodef("no_such_directory", "%s is not a directory here — `toft create` takes the "+
			"directory to deploy. To name the app, run `toft create --app %s` in the app's directory", dir, dir)
	}
	return usageCodef("no_such_directory", "%s is not a directory", dir)
}

// slugLike is a word with no path in it: lowercase letters, digits and dashes.
var slugLike = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// createSlug walks the one resolution order and no other, and says which layer
// answered — because the answer becomes a URL and "where did that name come
// from" is the question somebody asks a minute later.
//
// `exact` is whether the slug was stated (--app, toft.toml) rather than
// derived from a name. A stated slug that is taken is refused, exit 2
// name_taken: renaming it would make a retried create — a CI rerun, an agent
// that lost its place — a second app beside the first. A derived one is still
// uniquified, because nobody asked for that exact name.
func createSlug(e *env, g *globalFlags, installURL, repo, subdir string) (slug, origin string, exact bool, err error) {
	if g.app != "" {
		return g.app, "--app", true, nil
	}
	resolver, err := config.NewResolver(e.dir, nil)
	if err == nil {
		// The per-install slug for the install being created on, as deploy
		// reads it — not the plain table's alone.
		if v := resolver.App(installURL); v.Found() && v.Value != "" {
			return v.Value, v.Origin, true, nil
		}
	}
	// The folder before the repository: every app of a shared repository has
	// the same repository name, and the folder is what tells them apart.
	if sub := strings.Trim(subdir, "/ "); repo != "" && sub != "" {
		return path.Base(sub), "the folder's name", false, nil
	}
	if repo != "" {
		base := strings.TrimSuffix(filepath.Base(strings.TrimRight(repo, "/")), ".git")
		if base != "" && base != "." && base != "/" {
			return base, "the repository's name", false, nil
		}
	}
	abs, err := filepath.Abs(e.dir)
	if err != nil {
		return "", "", false, err
	}
	base := filepath.Base(abs)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", "", false, usageCodef("missing_argument", "no slug to use — pass --app <slug>")
	}
	return base, "this directory's name", false, nil
}

// refuseStatedNonSlug refuses a slug somebody named (--app, toft.toml) that is
// not one. Only a name nobody offered as a slug — a folder's, a repository's —
// is turned into one: `--app Bad_Slug` used to create bad-slug with a warning,
// an app under a slug nobody typed, which every later command in the script
// then missed (BB-006, third 2026-11-03 list).
func refuseStatedNonSlug(slug, origin string, exact bool) error {
	if !exact {
		return nil
	}
	if norm := normalizeSlug(slug); norm != slug {
		return usageHinted("invalid_slug", fmt.Sprintf("name it %q instead", norm),
			"%q (from %s) is not a slug — lowercase letters, digits and "+
				"dashes, not starting or ending with one; %q would be", slug, origin, norm)
	}
	return nil
}

// maxSlugLen is the platform's longest slug: a stated one longer is refused,
// a proposed one (a folder's name) is cut to it.
const maxSlugLen = 54

func createFromRepo(s *session, slug string, exact bool, name, repo, branch, subdir, mode, groupID string, out any) error {
	body := map[string]any{"repo_url": repo, "slug": slug, "name": orDefault(name, slug)}
	if exact {
		// An older install ignores the field and renames as before; the
		// warning below still says so.
		body["slug_exact"] = true
	}
	if branch != "" {
		body["branch"] = branch
	}
	if subdir != "" {
		// Sent as typed; the server normalizes and validates it, so the two
		// cannot disagree about what a folder may be.
		body["repo_subdir"] = subdir
	}
	if mode != "" {
		// Passed through. The server refuses a caller who may not choose, and
		// that refusal is printed as it arrives rather than pre-empted here —
		// the client does not know the install's policy and should not guess.
		body["execution_mode"] = mode
	}
	if groupID != "" {
		body["owner_group_id"] = groupID
	}
	err := s.client.Do(http.MethodPost, "/api/v1/apps", body, out)
	// The install's hint names its API's fields (repo_url, git_token); this
	// command's are flags, and it has no way to send a token (L8,
	// 2026-11-04 black-box list).
	var apiErr *client.Error
	if errors.As(err, &apiErr) && apiErr.Code == "source_unavailable" {
		apiErr.Hint = "check --repo and --branch; a private repository needs a Git connection made in Toft's UI " +
			"— or run `toft create` in a checkout of it, which uploads the tree instead"
	}
	return err
}

func createFromBundle(e *env, s *session, g *globalFlags, slug string, exact bool, name, mode, groupID string, out any) error {
	built, err := bundle.Build(e.dir, bundle.Options{KeepPaths: staticRoots(e.dir)})
	if err != nil {
		return err
	}
	defer os.Remove(built.Path)

	archive, err := os.Open(built.Path)
	if err != nil {
		return err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return err
	}
	if info.Size() > deployMaxBytes {
		return tooBig(e, built, info.Size())
	}
	for _, skipped := range built.EnvFiles {
		e.out.Dim("not uploading %s — toft env set --from-file %s sends its values", skipped, skipped)
	}
	for _, secret := range built.Secrets {
		e.out.Dim("including %s", secret)
	}
	// The watched result's warnings name them, as a deploy's do (BB-016,
	// third 2026-11-03 list).
	e.bundleSecrets, e.bundleEnvFiles = built.Secrets, built.EnvFiles
	e.bundleWorkspaceLeftOut = built.WorkspaceLeftOut
	for _, dir := range built.WorkspaceLeftOut {
		e.out.Warn("%s", workspaceLeftOutWarning(dir))
	}

	fields := map[string]string{"slug": slug, "name": orDefault(name, slug)}
	if exact {
		fields["slug_exact"] = "true"
	}
	if mode != "" {
		fields["execution_mode"] = mode
	}
	if groupID != "" {
		fields["owner_group_id"] = groupID
	}
	err = s.client.UploadWithFields("/api/v1/apps/upload", slug+".zip", archive, info.Size(), fields,
		func(sent, total int64) { e.out.Meter("upload", sent, total) }, out)
	e.out.ClearLine()
	return err
}

// ownerGroupFor is the group a new app will be owned by, as an id, or "" for
// the caller themselves.
//
// **A deploy key belongs to nobody**, so for one the group is not optional:
// the install refuses the create `owner_group_required` — after the upload,
// and at exit 1 when it arrived as a 422. Refused here instead, exit 2 with the
// install's own code and the flag that supplies it. `toft check` asks the same
// question of a new app, so a check that passes is a create that will not be
// refused for this.
func ownerGroupFor(s *session, command, given string) (string, error) {
	given = strings.TrimSpace(given)
	if given == "" {
		if s.IsDeployKey() {
			return "", usageCodef("owner_group_required", "a deploy key belongs to nobody, so the app "+
				"it creates must be owned by a group — pass --owner-group <name|id> to %s "+
				"(`toft api GET groups` lists them)", command)
		}
		return "", nil
	}
	return resolveGroupID(s, given)
}

// resolveGroupID turns a group's id or name into its id, against GET /groups —
// the directory a deploy key and a personal key may both read. An exact id
// wins, then an exact name, then a name that differs only in case; none, or a
// case-folded name two groups share, is exit 2 naming the candidates.
func resolveGroupID(s *session, given string) (string, error) {
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := s.client.Do(http.MethodGet, "/api/v1/groups", nil, &groups); err != nil {
		return "", err
	}
	return matchGroupID(groups, given)
}

func matchGroupID(groups []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}, given string) (string, error) {
	var folded []string
	for _, g := range groups {
		if g.ID == given || g.Name == given {
			return g.ID, nil
		}
		if strings.EqualFold(g.Name, given) {
			folded = append(folded, g.ID)
		}
	}
	switch len(folded) {
	case 1:
		return folded[0], nil
	case 0:
		return "", usageCodef("not_found", "no group called %q — `toft api GET groups` lists them", given)
	}
	return "", usageCodef("ambiguous_name", "%q names %d groups that differ only in case — pass the id:\n    %s",
		given, len(folded), strings.Join(folded, "\n    "))
}

// normalizeSlug is the server's sanitizeSlug (internal/api): lowercase, every
// character but a-z, 0-9 and dash a dash, no dash at either end.
func normalizeSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "app"
	}
	return out
}

// suggest names the command to run next. Off a terminal it is the sentence it
// always was, "<plain>: <command>"; on one the command leads, bold behind an
// arrow, with what it does dimmed after it.
func suggest(e *env, plain, command, does string) {
	if !e.out.Rich() {
		e.out.Dim("%s: %s", plain, command)
		return
	}
	if e.out.Quiet {
		return
	}
	e.out.Say("  %s%s  %s", e.out.Dimmed("→ "), e.out.Bold(command), e.out.Dimmed(does))
}
