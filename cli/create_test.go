package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/cli/internal/config"
)

// createFake answers the two create routes and the by-slug read.
func createFake(t *testing.T, gotSlug *string, posted *bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		// `toft create` prints the new app's URL, which is the install's answer
		// rather than a derivation — so a create reads the config probe too.
		case p == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":"","apps_path_prefix":"/apps/"}`)
		case strings.HasSuffix(p, "/apps/upload") && r.Method == http.MethodPost:
			*posted = true
			if err := r.ParseMultipartForm(32 << 20); err == nil {
				*gotSlug = r.FormValue("slug")
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":%q,"status":"building"},"deployment":{"id":"dep-1"}}`, *gotSlug)
		case strings.HasSuffix(p, "/apps") && r.Method == http.MethodPost:
			*posted = true
			raw, _ := io.ReadAll(r.Body)
			*gotSlug = jsonField(string(raw), "slug")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":%q,"status":"building"},"deployment":{"id":"dep-1"}}`, *gotSlug)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
		}
	}
}

// jsonField is a crude reader for the one field these tests assert on.
func jsonField(body, key string) string {
	needle := `"` + key + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// **The slug is named before it is claimed**, and declining posts nothing.
func TestCreateNamesTheSlugBeforeClaimingIt(t *testing.T) {
	var slug string
	posted := false
	e, buf := fakeInstall(t, createFake(t, &slug, &posted))
	e.dir = t.TempDir()
	os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("print('hi')\n"), 0644)

	err := cmdCreate(e, []string{"--app", "reports", "--yes", "--no-watch"})
	if err != nil {
		t.Fatalf("cmdCreate: %v", err)
	}
	// The slug appears before the request — the estate's namespace is never
	// entered on a value nobody read.
	if !strings.Contains(buf.String(), "reports") {
		t.Errorf("the slug was not named: %q", buf.String())
	}
	if slug != "reports" {
		t.Errorf("claimed %q, want reports", slug)
	}
}

// **Unattended without --yes is exit 2 naming the flag, and posts nothing.**
func TestCreateUnattendedWithoutYes(t *testing.T) {
	var slug string
	posted := false
	e, _ := fakeInstall(t, createFake(t, &slug, &posted))
	e.dir = t.TempDir()
	t.Setenv("CI", "1")

	err := cmdCreate(e, []string{"--app", "reports", "--no-watch"})
	if err == nil {
		t.Fatal("an unattended create claimed a slug with nothing to confirm it")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the refusal does not name --yes: %v", err)
	}
	if posted {
		t.Error("a declined create still posted — nothing may be claimed before the confirmation")
	}
}

// --repo and a directory are two different sources, and saying both is exit 2
// rather than a silent preference for one.
func TestCreateRepoAndDirAreExclusive(t *testing.T) {
	var slug string
	posted := false
	e, _ := fakeInstall(t, createFake(t, &slug, &posted))
	err := cmdCreate(e, []string{"./service", "--repo", "https://github.com/corp/x", "--yes"})
	if err == nil {
		t.Fatal("both sources were accepted")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
	if posted {
		t.Error("a usage error still posted")
	}
}

// --repo bundles nothing: the fake fails if a multipart body arrives.
func TestCreateFromRepoUploadsNothing(t *testing.T) {
	var slug string
	posted := false
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
			t.Errorf("--repo sent a bundle: %q", ct)
		}
		createFake(t, &slug, &posted)(w, r)
	})
	e.dir = t.TempDir()

	err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--yes", "--no-watch"})
	if err != nil {
		t.Fatalf("cmdCreate --repo: %v", err)
	}
	// The slug came from the repository's own name, with .git stripped.
	if slug != "reports" {
		t.Errorf("claimed %q, want reports", slug)
	}
}

// --branch without --repo means nothing, and says so rather than being ignored.
func TestCreateBranchNeedsRepo(t *testing.T) {
	var slug string
	posted := false
	e, _ := fakeInstall(t, createFake(t, &slug, &posted))
	err := cmdCreate(e, []string{"--branch", "release", "--yes"})
	if err == nil {
		t.Fatal("--branch was accepted without --repo")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
}

// The server uniquifies a taken slug, so the app may not have the name that was
// confirmed — and every later command in the script is about the one it got.
func TestCreateSaysWhenTheSlugItGotIsNotTheOneConfirmed(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports-2","status":"building"},"deployment":{"id":"dep-1"}}`)
	})
	e.dir = t.TempDir()

	if err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--yes", "--no-watch"}); err != nil {
		t.Fatalf("cmdCreate: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "reports-2") {
		t.Errorf("the slug it actually got is not named: %q", out)
	}
	if !strings.Contains(out, "taken") {
		t.Errorf("output %q does not say the confirmed slug was taken", out)
	}
}

// A slug the server only normalized was not taken, and the warning must not
// say it was (release check B3: "dot.slug.test" was taken). Only a derived slug
// is normalized; a stated one is refused invalid_slug (BB-006).
func TestCreateSaysANormalizedSlugWasNormalizedNotTaken(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":{"id":"app-1","slug":"dot-slug-test","status":"building"},"deployment":{"id":"dep-1"}}`)
	})
	e.dir = t.TempDir()
	if err := cmdCreate(e, []string{"--repo", "https://github.com/corp/dot.slug.test", "--yes", "--no-watch"}); err != nil {
		t.Fatalf("cmdCreate: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "taken") || !strings.Contains(out, "is not a slug") || !strings.Contains(out, "dot-slug-test") {
		t.Errorf("output %q should say the slug was normalized to dot-slug-test, not that it was taken", out)
	}
}

// --link writes the manifest; without it the command says what to add instead.
// Q2's lean: `toft create` does not write a file in a directory as a side effect.
func TestCreateLinkIsOptIn(t *testing.T) {
	var slug string
	posted := false

	e, buf := fakeInstall(t, createFake(t, &slug, &posted))
	e.dir = t.TempDir()
	if err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--yes", "--no-watch"}); err != nil {
		t.Fatalf("cmdCreate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "toft.toml")); !os.IsNotExist(err) {
		t.Error("a manifest was written without --link")
	}
	if !strings.Contains(buf.String(), "toft link") {
		t.Errorf("output %q does not say how to link it", buf.String())
	}

	e2, _ := fakeInstall(t, createFake(t, &slug, &posted))
	e2.dir = t.TempDir()
	if err := cmdCreate(e2, []string{"--repo", "https://github.com/corp/reports", "--yes", "--link", "--no-watch"}); err != nil {
		t.Fatalf("cmdCreate --link: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(e2.dir, "toft.toml"))
	if err != nil {
		t.Fatalf("--link wrote no manifest: %v", err)
	}
	if !strings.Contains(string(written), "reports") {
		t.Errorf("the manifest does not name the app: %q", written)
	}
}

// A refusal the server states is printed as it arrives. The client does not
// know the install's execution-mode policy and must not pre-empt it.
func TestCreateExecutionModeRefusalIsTheServersWords(t *testing.T) {
	const refusal = "Only an administrator may choose where an app runs on this install."
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":%q}`, refusal)
	})
	e.dir = t.TempDir()

	err := cmdCreate(e, []string{"--repo", "https://github.com/corp/x", "--app", "x",
		"--execution-mode", "isolated", "--yes", "--no-watch"})
	if err == nil {
		t.Fatal("the refusal was swallowed")
	}
	if !strings.Contains(err.Error(), refusal) {
		t.Errorf("the refusal was reworded: %v", err)
	}
	if got := report(e, err); got != exitAuth {
		t.Errorf("exit %d, want %d", got, exitAuth)
	}
}

// Two different slugs in one `toft link` is a typo somewhere, and linking either
// would be a guess. Refused before anything is sent or written.
func TestLinkRefusesTwoDifferentApps(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request was sent: %s %s", r.Method, r.URL.Path)
	})
	e.dir = t.TempDir()
	err := cmdLink(e, []string{"toft-testdrive", "--app", "other-slug"})
	if got := report(e, err); got != exitUsage {
		t.Fatalf("exit %d, want %d (%v)", got, exitUsage, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "toft.toml")); !os.IsNotExist(err) {
		t.Error("a manifest was written")
	}
}

// **A stated slug is sent exact, a derived one is not.** --app and toft.toml
// are statements, so a taken one comes back 409 name_taken and is exit 2 —
// a retried create must not claim `reports-2` beside the app the first try
// made. The repository's name is a derivation and may still be uniquified.
func TestAStatedSlugIsRefusedWhenTaken(t *testing.T) {
	var exact []string
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"slug_exact":true`) {
			exact = append(exact, jsonField(string(raw), "slug"))
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"the slug \"reports\" is taken","code":"name_taken","retryable":false,`+
				`"hint":"if that app is yours, link this directory to it and deploy: `+"`toft link reports`"+` then `+"`toft deploy`"+`"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports-2","status":"building"},"deployment":{"id":"dep-1"}}`)
	})
	e.dir = t.TempDir()

	err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--app", "reports", "--yes", "--no-watch"})
	if err == nil {
		t.Fatal("a taken --app slug was accepted")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d (name_taken)", got, exitUsage)
	}
	if len(exact) != 1 || exact[0] != "reports" {
		t.Errorf("slug_exact sent for %v, want [reports]", exact)
	}

	exact = nil
	if err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--yes", "--no-watch"}); err != nil {
		t.Fatalf("a derived slug: %v", err)
	}
	if len(exact) != 0 {
		t.Errorf("slug_exact sent for a slug derived from the repository: %v", exact)
	}
}

// **--json watches the first build**, and a failed one is the deploy's answer:
// frames, then one result object with `reason`, and exit 1 — not the new app
// and exit 0 while the build was still running.
func TestCreateJSONWatchesTheFirstBuild(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/api/v1/apps" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports","status":"building"},"deployment":{"id":"dep-1"}}`)
		case strings.HasSuffix(p, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"dep-1\",\"status\":\"failed\","+
				"\"phase\":\"build\",\"step\":\"clone\",\"reason\":\"source_unavailable\","+
				"\"message\":\"repository not found\"}\n\n")
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	e.dir = t.TempDir()
	e.json = true

	err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--yes", "--json"})
	if got := report(e, err); got != exitFailed {
		t.Errorf("exit %d, want %d for a failed first build (err %v)", got, exitFailed, err)
	}
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var doc map[string]any
		if json.Unmarshal([]byte(line), &doc) == nil && doc["app"] == "reports" {
			last = doc
		}
	}
	if last == nil || last["status"] != "failed" || last["reason"] != "source_unavailable" {
		t.Errorf("no result object with the failure in:\n%s", buf.String())
	}
}

// Under --json every stdout line is a JSON document: the app URL, which a
// person reads first, is narration there and goes to stderr.
func TestCreateJSONStdoutIsOnlyJSON(t *testing.T) {
	var slug string
	posted := false
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		createFake(t, &slug, &posted)(w, r)
	})
	var stdout bytes.Buffer
	e.out.Stdout = &stdout
	e.dir = t.TempDir()
	if err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--yes", "--no-watch", "--json"}); err != nil {
		t.Fatalf("cmdCreate --json: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("stdout line %d is not JSON: %q", i, line)
		}
	}
}

// **A deploy key's create names its owning group, or is refused before the
// upload** (drills rerun 2026-10-22 #4). The install refuses a deploy key with
// no group `owner_group_required` — after the bundle, at exit 1 — and no flag
// could supply the field. Now --owner-group takes a name or an id and sends the
// id, and its absence is exit 2 with the install's code, nothing posted.
func TestADeployKeyCreateNamesItsOwningGroup(t *testing.T) {
	groups := `[{"id":"g-1","name":"Prod Owners"},{"id":"g-2","name":"Data"}]`
	var posted bool
	var sentGroup string
	fake := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":"","apps_path_prefix":"/apps/"}`)
		case r.URL.Path == "/api/v1/groups":
			fmt.Fprint(w, groups)
		case r.URL.Path == "/api/v1/apps/upload":
			posted = true
			if err := r.ParseMultipartForm(32 << 20); err == nil {
				sentGroup = r.FormValue("owner_group_id")
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports","status":"building"},"deployment":{"id":"dep-1"}}`)
		case r.URL.Path == "/api/v1/apps" && r.Method == http.MethodPost:
			posted = true
			raw, _ := io.ReadAll(r.Body)
			sentGroup = jsonField(string(raw), "owner_group_id")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports","status":"building"},"deployment":{"id":"dep-1"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}

	t.Run("no group is exit 2 before anything is sent", func(t *testing.T) {
		posted = false
		e, buf := fakeInstall(t, fake)
		t.Setenv("TOFT_TOKEN", "toftd_ci")
		os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("print('hi')\n"), 0o644)
		e.json = true
		got := report(e, cmdCreate(e, []string{"--app", "reports", "--yes", "--no-watch", "--json"}))
		if got != exitUsage || posted {
			t.Fatalf("exit %d, posted %v:\n%s", got, posted, buf)
		}
		if !strings.Contains(buf.String(), `"code":"owner_group_required"`) || !strings.Contains(buf.String(), "--owner-group") {
			t.Errorf("the refusal names neither the code nor the flag:\n%s", buf)
		}
	})
	for _, c := range []struct{ name, given, want string }{
		{"a name", "Prod Owners", "g-1"},
		{"a name in another case", "data", "g-2"},
		{"an id", "g-1", "g-1"},
	} {
		t.Run("bundle, by "+c.name, func(t *testing.T) {
			posted, sentGroup = false, ""
			e, buf := fakeInstall(t, fake)
			t.Setenv("TOFT_TOKEN", "toftd_ci")
			os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("print('hi')\n"), 0o644)
			if err := cmdCreate(e, []string{"--app", "reports", "--yes", "--no-watch", "--owner-group", c.given}); err != nil {
				t.Fatalf("cmdCreate: %v\n%s", err, buf)
			}
			if sentGroup != c.want {
				t.Errorf("owner_group_id = %q, want %q", sentGroup, c.want)
			}
		})
	}
	t.Run("repo, by name", func(t *testing.T) {
		posted, sentGroup = false, ""
		e, buf := fakeInstall(t, fake)
		t.Setenv("TOFT_TOKEN", "toftd_ci")
		if err := cmdCreate(e, []string{"--repo", "https://github.com/x/reports", "--yes", "--no-watch",
			"--owner-group", "Prod Owners"}); err != nil {
			t.Fatalf("cmdCreate: %v\n%s", err, buf)
		}
		if sentGroup != "g-1" {
			t.Errorf("owner_group_id = %q, want g-1", sentGroup)
		}
	})
	t.Run("a group that does not exist is exit 2 not_found, nothing sent", func(t *testing.T) {
		posted = false
		e, buf := fakeInstall(t, fake)
		os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("print('hi')\n"), 0o644)
		e.json = true
		got := report(e, cmdCreate(e, []string{"--app", "reports", "--yes", "--no-watch", "--owner-group", "nope", "--json"}))
		if got != exitUsage || posted || !strings.Contains(buf.String(), `"code":"not_found"`) {
			t.Fatalf("exit %d, posted %v:\n%s", got, posted, buf)
		}
	})
	t.Run("a personal key names no group and sends none", func(t *testing.T) {
		posted, sentGroup = false, "unset"
		e, buf := fakeInstall(t, fake)
		os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("print('hi')\n"), 0o644)
		if err := cmdCreate(e, []string{"--app", "reports", "--yes", "--no-watch"}); err != nil {
			t.Fatalf("cmdCreate: %v\n%s", err, buf)
		}
		if !posted || sentGroup != "" {
			t.Errorf("posted %v, owner_group_id %q", posted, sentGroup)
		}
	})
}

// `toft check` of a new app asks the create's owner question too, so a check
// that passes is not followed by a create refused owner_group_required. The
// group goes with the bundle as the id create sends (drills 2026-10-28 #2), so
// the install can refuse a group the create would.
func TestADeployKeyCheckOfANewAppNeedsTheOwningGroup(t *testing.T) {
	var checked bool
	var sentGroup string
	fake := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/groups":
			fmt.Fprint(w, `[{"id":"g-1","name":"Prod Owners"}]`)
		case "/api/v1/apps/check":
			checked = true
			if err := r.ParseMultipartForm(32 << 20); err == nil {
				sentGroup = r.FormValue("owner_group_id")
			}
			fmt.Fprint(w, `{"ok":true,"framework":"static","not_checked":["dependencies"]}`)
		case "/api/v1/me/capabilities":
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}
	e, buf := fakeInstall(t, fake)
	t.Setenv("TOFT_TOKEN", "toftd_ci")
	os.WriteFile(filepath.Join(e.dir, "index.html"), []byte("x"), 0o644)
	e.json = true
	if got := report(e, cmdCheck(e, []string{"--json"})); got != exitUsage || checked {
		t.Fatalf("exit %d, checked %v:\n%s", got, checked, buf)
	}
	if !strings.Contains(buf.String(), `"code":"owner_group_required"`) || !strings.Contains(buf.String(), "--owner-group") {
		t.Errorf("the refusal names neither the code nor the flag:\n%s", buf)
	}

	if err := cmdCheck(e, []string{"--owner-group", "Prod Owners"}); err != nil || !checked {
		t.Fatalf("with the group: err %v, checked %v", err, checked)
	}
	if sentGroup != "g-1" {
		t.Errorf("owner_group_id = %q, want g-1", sentGroup)
	}

	// A personal key need not name one, and sends one when it does.
	t.Setenv("TOFT_TOKEN", "toft_personal")
	checked, sentGroup = false, "unset"
	if err := cmdCheck(e, nil); err != nil || !checked || sentGroup != "" {
		t.Fatalf("personal key, no group: err %v, checked %v, owner_group_id %q", err, checked, sentGroup)
	}
	if err := cmdCheck(e, []string{"--owner-group", "prod owners"}); err != nil || sentGroup != "g-1" {
		t.Fatalf("personal key, a group: err %v, owner_group_id %q", err, sentGroup)
	}
}

// toft.toml's [app] name is the new app's display name when --name is not
// passed, and --name overrides it. Read at create only.
func TestCreateTakesTheDisplayNameFromTomlAndTheFlagWins(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "Reports Desk"},
		{[]string{"--name", "Ops Reports"}, "Ops Reports"},
	} {
		var gotName string
		e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/apps") && r.Method == http.MethodPost {
				raw, _ := io.ReadAll(r.Body)
				gotName = jsonField(string(raw), "name")
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports","status":"building"},"deployment":{"id":"dep-1"}}`)
				return
			}
			if r.URL.Path == "/api/v1/config" {
				fmt.Fprint(w, `{"apps_base_url":"","apps_path_prefix":"/apps/"}`)
				return
			}
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		})
		e.dir = t.TempDir()
		if err := os.WriteFile(filepath.Join(e.dir, "toft.toml"),
			[]byte("[app]\nslug = \"reports\"\nname = \"Reports Desk\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"--repo", "https://github.com/corp/reports", "--yes", "--no-watch"}, c.args...)
		if err := cmdCreate(e, args); err != nil {
			t.Fatalf("cmdCreate %v: %v", c.args, err)
		}
		if gotName != c.want {
			t.Errorf("create %v posted name %q, want %q", c.args, gotName, c.want)
		}
	}
}

// `toft create` reads the per-install slug for the install it creates on, and
// the directory it is given before it picks that install — as `toft deploy
// <dir>` then does (security review 2026-11-15, CL-4).
func TestCreateReadsTheDirectoryAndThePerInstallSlugAsDeployDoes(t *testing.T) {
	var gotSlug string
	var posted bool
	e, _ := fakeInstall(t, createFake(t, &gotSlug, &posted))
	url := os.Getenv("TOFT_URL")
	e.dir = t.TempDir()
	toml := fmt.Sprintf("[app]\nslug = \"svc\"\n\n[app.installs.%q]\nslug = \"svc-stg\"\n", url)
	if err := os.WriteFile(filepath.Join(e.dir, "toft.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdCreate(e, []string{"--repo", "https://github.com/corp/svc", "--yes", "--no-watch"}); err != nil {
		t.Fatalf("cmdCreate: %v", err)
	}
	if gotSlug != "svc-stg" {
		t.Errorf("posted slug %q, want the per-install svc-stg", gotSlug)
	}

	// A tree that pins another install, named from an empty directory: refused
	// as not signed in there, not created on the stored default.
	gotSlug, posted = "", false
	isolateConfigDir(t)
	t.Setenv("TOFT_URL", "")
	t.Setenv("TOFT_TOKEN", "")
	creds := &config.CredentialsFile{Installs: map[string]config.Credential{}}
	creds.Set(config.Credential{URL: url, Token: "toftu_default", User: "ada@corp.com"})
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "toft.toml"),
		[]byte("[app]\nslug = \"svc\"\ninstall = \"https://elsewhere.example\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "index.html"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.dir = t.TempDir()
	err := cmdCreate(e, []string{tree, "--yes", "--no-watch"})
	if err == nil || posted {
		t.Fatalf("create in a tree pinned elsewhere = %v (posted %v), want refused before anything is sent", err, posted)
	}
}
