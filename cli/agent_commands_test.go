package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The commands that replaced `toft api` and a hand-written body, and the ones
// that ask before doing. Each test splits stdout from stderr, because the
// property most of them hold is that the answer is on stdout and nothing else
// is.

func splitInstall(t *testing.T, handler http.HandlerFunc) (*env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	e, _ := fakeInstall(t, handler)
	var stdout, stderr bytes.Buffer
	e.out.Stdout, e.out.Stderr = &stdout, &stderr
	return e, &stdout, &stderr
}

func oneDocument(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %q", out.String())
	}
	return doc
}

// appWithEnv is the app's detail route as an editor reads it: one visible
// variable with its value, one sensitive one without.
const appWithEnv = `{"app":{"id":"app-1","slug":"demo","visibility":"private"},` +
	`"env_keys":["API_KEY","MODE"],` +
	`"env_vars":[{"key":"MODE","value":"prod","sensitive":false},{"key":"API_KEY","sensitive":true}]}`

func TestVisibilityChangesOnlyWhenAsked(t *testing.T) {
	var patches []string
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case r.Method == http.MethodGet:
			fmt.Fprint(w, appWithEnv)
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			patches = append(patches, string(b))
			fmt.Fprint(w, `{}`)
		}
	})
	if err := cmdVisibility(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["visibility"] != "private" || doc["changed"] != false || len(patches) != 0 {
		t.Errorf("reading wrote: %v %v", doc, patches)
	}
	stdout.Reset()
	if err := cmdVisibility(e, []string{"authenticated", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["changed"] != true || len(patches) != 1 ||
		!strings.Contains(patches[0], `"authenticated"`) {
		t.Errorf("doc = %v patches = %v", doc, patches)
	}
	if err := cmdVisibility(e, []string{"everyone", "--app", "demo"}); err == nil {
		t.Error("a word that is not a visibility was accepted")
	}
}

// **The slug is typed out, never inherited.** A directory being linked to an
// app is not a statement that it should go.
func TestDeleteNeedsTheSlugAndAnAnswer(t *testing.T) {
	deleted := false
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case r.Method == http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		}
	})
	t.Setenv("TOFT_APP", "demo")
	var usage *usageError
	if err := cmdDelete(e, []string{"--yes"}); !errors.As(err, &usage) || deleted {
		t.Fatalf("no slug: err = %v, deleted = %v", err, deleted)
	}
	t.Setenv("CI", "1")
	if err := cmdDelete(e, []string{"demo"}); !errors.As(err, &usage) || deleted {
		t.Fatalf("unattended without --yes: err = %v, deleted = %v", err, deleted)
	}
	if err := cmdDelete(e, []string{"demo", "--yes", "--json"}); err != nil || !deleted {
		t.Fatalf("err = %v, deleted = %v", err, deleted)
	}
	if doc := oneDocument(t, stdout); doc["deleted"] != "demo" {
		t.Errorf("doc = %v", doc)
	}
	// --app is typed out as well: refusing it said "typed out" about a slug
	// that had been.
	deleted = false
	if err := cmdDelete(e, []string{"--app", "demo", "--yes", "--json"}); err != nil || !deleted {
		t.Fatalf("--app demo: err = %v, deleted = %v", err, deleted)
	}
}

func TestDeploymentsListAndPrune(t *testing.T) {
	e, stdout, _ := splitInstall(t, everythingFakePlus(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/deployments") {
			fmt.Fprint(w, `{"deleted":1,"freed_disk_bytes":2048,"freed_artifact_bytes":0}`)
			return true
		}
		return false
	}))
	if err := cmdDeployments(e, []string{"list", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if unmarshalNDJSON(stdout.Bytes(), &rows); len(rows) != 2 {
		t.Fatalf("list = %q", stdout.String())
	}
	stdout.Reset()
	if err := cmdDeployments(e, []string{"prune", "--app", "demo", "--yes", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["deleted"] != float64(1) {
		t.Errorf("doc = %v", doc)
	}
}

// A prune names each deployment it kept, with the reason and the remedy the
// platform sent, in both renderings — "1 deleted" alone read as "only the
// serving release is left" (drills D8, 2026-10-28).
func TestDeploymentsPruneSaysWhatItKept(t *testing.T) {
	const kept = `{"id":"0123456789abcdef-kept","reason":"a job run that has not finished will start from that build",` +
		`"remedy":"wait for the run to finish or cancel it"}`
	e, stdout, _ := splitInstall(t, everythingFakePlus(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/deployments") {
			fmt.Fprint(w, `{"deleted":1,"freed_disk_bytes":2048,"freed_artifact_bytes":0,"kept":[`+kept+`]}`)
			return true
		}
		return false
	}))
	if err := cmdDeployments(e, []string{"prune", "--app", "demo", "--yes"}); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	for _, want := range []string{"1 deleted", "kept 012345", "job run that has not finished", "— wait for the run to finish or cancel it"} {
		if !strings.Contains(got, want) {
			t.Errorf("prune = %q, want %q", got, want)
		}
	}
	stdout.Reset()
	if err := cmdDeployments(e, []string{"prune", "--app", "demo", "--yes", "--json"}); err != nil {
		t.Fatal(err)
	}
	doc := oneDocument(t, stdout)
	rows, _ := doc["kept"].([]any)
	if len(rows) != 1 {
		t.Fatalf("prune --json = %v, want kept", doc)
	}
	if k := rows[0].(map[string]any); k["id"] != "0123456789abcdef-kept" || k["reason"] == "" || k["remedy"] != "wait for the run to finish or cancel it" {
		t.Errorf("kept = %v", k)
	}
}

// everythingFakePlus answers the routes `extra` claims, and everythingFake's
// for the rest.
func everythingFakePlus(t *testing.T, extra func(http.ResponseWriter, *http.Request) bool) http.HandlerFunc {
	base := everythingFake(t)
	return func(w http.ResponseWriter, r *http.Request) {
		if !extra(w, r) {
			base(w, r)
		}
	}
}

// appOrigin serves the install and, under /apps/demo, the deployed app — a
// one-origin install, where the app's address is the install's with a path.
func appOrigin(t *testing.T, app http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":"","apps_path_prefix":"/apps"}`)
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasPrefix(r.URL.Path, "/apps/demo"):
			app(w, r)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}
}

// The credential goes to the app's address, the body is bounded, and a failed
// answer is exit 1 with the result document as the whole of stdout.
func TestFetchSendsTheKeyAndBoundsTheBody(t *testing.T) {
	var auth string
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path == "/apps/demo/broken" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		fmt.Fprint(w, strings.Repeat("x", 100))
	}))
	if err := cmdFetch(e, []string{"/healthz", "--app", "demo", "--max-body", "10", "--json"}); err != nil {
		t.Fatal(err)
	}
	doc := oneDocument(t, stdout)
	if auth != "Bearer toft_test" && auth != "Bearer toftu_test" {
		t.Errorf("Authorization = %q", auth)
	}
	if doc["status"] != float64(200) || doc["body"] != strings.Repeat("x", 10) || doc["truncated"] != true ||
		doc["bytes"] != float64(100) {
		t.Errorf("doc = %v", doc)
	}

	stdout.Reset()
	err := cmdFetch(e, []string{"/broken", "--app", "demo", "--json"})
	// The result, then the coded refusal (BB-104, 2026-11-04 black-box list).
	var failed *failedError
	if !errors.As(err, &failed) || failed.code != "unexpected_status" || report(e, err) != exitFailed {
		t.Fatalf("a 500 = %v", err)
	}
	if docs := jsonLines(t, stdout.String()); len(docs) != 2 || docs[0]["status"] != float64(500) ||
		docs[1]["code"] != "unexpected_status" {
		t.Errorf("a failed fetch = %q", stdout.String())
	}

	stdout.Reset()
	if err := cmdFetch(e, []string{"/", "--app", "demo", "--anonymous", "--json"}); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Errorf("--anonymous sent %q", auth)
	}
}

// --verify runs after the platform's success and lands in the one result
// object; a path that fails makes the deploy exit 1.
func TestDeployVerifyIsInTheResult(t *testing.T) {
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	e.verify, e.verifyWithin = "/healthz", 0
	app := &appSummary{ID: "app-1", Slug: "demo"}
	s, err := e.setup(&globalFlags{app: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	got, verr := verifyDeploy(e.ctx, s, app, e.verify, e.verifyWithin)
	if verr != nil || got.Status != http.StatusNotFound || got.ok() {
		t.Fatalf("verify = %+v %v", got, verr)
	}
	// The release is live by now, so the sentence names that and the undo.
	if err := verifyFailed("/healthz", "my-app", got, nil); !strings.Contains(err.Error(), "404") ||
		!strings.Contains(err.Error(), "toft rollback --yes --app my-app") {
		t.Errorf("verifyFailed = %v, want the status and the rollback that undoes it", err)
	}
	_ = stdout
}

// check renders the install's answer and passes it through under --json; a
// problem is exit 1 with that document as the whole of stdout.
func TestCheckPassesTheInstallsAnswerThrough(t *testing.T) {
	answer := `{"ok":false,"framework":"unsupported","problems":[{"reason":"unsupported_framework","message":"nothing to serve"}],"not_checked":["dependencies"],"future_field":1}`
	e, stdout, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/check"):
			if _, _, err := r.FormFile("file"); err != nil {
				t.Errorf("no bundle: %v", err)
			}
			fmt.Fprint(w, answer)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	os.WriteFile(filepath.Join(e.dir, "notes.txt"), []byte("x"), 0o644)
	err := cmdCheck(e, []string{"--app", "demo", "--json"})
	var failed *resultFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v", err)
	}
	if doc := oneDocument(t, stdout); doc["future_field"] != float64(1) {
		t.Errorf("a field this binary does not name was dropped: %v", doc)
	}
	if !strings.Contains(stderr.String(), "checking") {
		t.Errorf("no narration on stderr: %q", stderr.String())
	}
}

func TestWhoamiPassesCapabilitiesThrough(t *testing.T) {
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/me/capabilities":
			fmt.Fprint(w, `{"system_role":"publisher","can_create_objects":true,`+
				`"credential":{"kind":"api_key","name":"laptop","scope":"full"},`+
				`"install":{"version":"0.9.6","uploads_allowed":true,"runtimes":{"node":["22.1.0"]}}}`)
		case "/api/v1/auth/me":
			fmt.Fprint(w, `{"login":"alice","email":"alice@example.com"}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	if err := cmdWhoami(e, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	doc := oneDocument(t, stdout)
	caps := doc["capabilities"].(map[string]any)
	if caps["credential"].(map[string]any)["scope"] != "full" || doc["user"].(map[string]any)["login"] != "alice" {
		t.Errorf("doc = %v", doc)
	}

	stdout.Reset()
	if err := cmdWhoami(e, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice@example.com", "publisher", "laptop", "does not expire", "22.1.0"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("whoami does not say %q: %s", want, stdout.String())
		}
	}
}

// A deploy key that may create says how: the app is a group's, named with
// --owner-group (drills rerun 2026-10-22 #4).
func TestWhoamiTellsADeployKeyHowItCreates(t *testing.T) {
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/capabilities" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"system_role":"","can_create_objects":true,`+
			`"credential":{"kind":"deploy_key","name":"ci","scope":"deploy"},"install":{"version":"0.9.6"}}`)
	})
	t.Setenv("TOFT_TOKEN", "toftd_ci")
	if err := cmdWhoami(e, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "--owner-group") {
		t.Errorf("whoami does not say how a deploy key creates: %s", stdout.String())
	}
}

// The skills are the bytes this binary was built with, and printing one needs
// no install.
func TestSkillPrintsTheEmbeddedSkills(t *testing.T) {
	var stdout bytes.Buffer
	e, _ := testEnv()
	e.out.Stdout = &stdout
	if err := cmdSkill(e, []string{"toft-cli"}); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile("../deploy/cli/skills/toft-cli/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != string(disk) {
		t.Error("toft skill toft-cli is not the SKILL.md in this tree")
	}
	stdout.Reset()
	if err := cmdSkill(e, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var names []string
	if unmarshalNDJSON(stdout.Bytes(), &names); len(names) != 2 {
		t.Errorf("names = %q", stdout.String())
	}
	var usage *usageError
	if err := cmdSkill(e, []string{"nope"}); !errors.As(err, &usage) {
		t.Errorf("an unknown skill = %v", err)
	}
}

// No linked app is a new app: the check goes to the create route's twin.
func TestCheckWithoutAnAppChecksANewOne(t *testing.T) {
	var route string
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		route = r.Method + " " + r.URL.Path
		fmt.Fprint(w, `{"ok":true,"framework":"static","not_checked":["dependencies"]}`)
	})
	os.WriteFile(filepath.Join(e.dir, "index.html"), []byte("x"), 0o644)
	if err := cmdCheck(e, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if route != "POST /api/v1/apps/check" {
		t.Errorf("checked against %s", route)
	}
	if doc := oneDocument(t, stdout); doc["ok"] != true {
		t.Errorf("doc = %v", doc)
	}
}

// ok:false with no problems is still a refusal, and never an index out of range.
func TestCheckRefusalWithNoProblemsListed(t *testing.T) {
	e, _, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":false}`)
	})
	os.WriteFile(filepath.Join(e.dir, "index.html"), []byte("x"), 0o644)
	var failed *resultFailedError
	if err := cmdCheck(e, nil); !errors.As(err, &failed) {
		t.Fatalf("err = %v", err)
	}
}

// check says what deploy says about the bundle — the secret-looking files it
// carries — and warns when toft.toml declares jobs this install cannot run.
func TestCheckNamesSecretsAndJobsTheInstallCannotRun(t *testing.T) {
	e, _, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/me/capabilities":
			fmt.Fprint(w, `{"install":{"jobs_available":false}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"framework":"flask","jobs":["refresh"],"not_checked":["dependencies"]}`)
		}
	})
	os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(e.dir, ".env"), []byte("SECRET=1"), 0o644)
	os.WriteFile(filepath.Join(e.dir, "server.key"), []byte("-----BEGIN"), 0o644)
	if err := cmdCheck(e, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"including server.key", "not uploading .env — toft env set --from-file .env", "runs no jobs"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("check does not say %q:\n%s", want, stderr)
		}
	}
}

// A stopped app is answered by the platform's interstitial; fetch says so and
// names the command, rather than printing five kilobytes of its HTML.
func TestFetchRecognisesAStoppedApp(t *testing.T) {
	page := `<!DOCTYPE html><html><head><title>demo — Stopped</title></head><body>` +
		strings.Repeat("<div></div>", 400) + `</body></html>`
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, page)
	}))
	err := cmdFetch(e, []string{"/", "--app", "demo"})
	var failed *failedError
	if !errors.As(err, &failed) || failed.code != "stopped" || !strings.Contains(err.Error(), "toft start --app demo") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(stdout.String(), "<div>") {
		t.Errorf("the interstitial's HTML was printed:\n%.200s", stdout)
	}
}

// The header names the state whatever the page says, and is what --json carries.
func TestFetchReadsTheInterstitialHeader(t *testing.T) {
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Toft-Interstitial", "deploy_failed")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "<html><title>demo</title></html>")
	}))
	_ = cmdFetch(e, []string{"/", "--app", "demo", "--json"})
	if !strings.Contains(stdout.String(), `"platform_page":"deploy_failed"`) {
		t.Errorf("stdout = %s", stdout)
	}
}

// A redirect is followed and said, and --no-follow answers with it: a 404 at
// the end of respond's 303 read as the path's own (jobs-regression #12).
func TestFetchSaysWhichRedirectsItFollowed(t *testing.T) {
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps/demo/x/respond" {
			w.Header().Set("Location", "/apps/demo/__toft/runs/run-1")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	_ = cmdFetch(e, []string{"/x/respond", "--app", "demo", "--json"})
	doc := oneDocument(t, stdout)
	hops, _ := doc["redirects"].([]any)
	if doc["status"] != float64(404) || len(hops) != 1 ||
		!strings.HasSuffix(fmt.Sprint(doc["final_url"]), "/apps/demo/__toft/runs/run-1") {
		t.Fatalf("doc = %v", doc)
	}
	if hop := hops[0].(map[string]any); hop["status"] != float64(303) ||
		!strings.HasSuffix(fmt.Sprint(hop["location"]), "/__toft/runs/run-1") {
		t.Errorf("hop = %v", hop)
	}

	stdout.Reset()
	if err := cmdFetch(e, []string{"/x/respond", "--app", "demo", "--no-follow", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["status"] != float64(303) || len(doc["redirects"].([]any)) != 0 ||
		!strings.HasSuffix(fmt.Sprint(doc["final_url"]), "/apps/demo/x/respond") ||
		doc["headers"].(map[string]any)["Location"] != "/apps/demo/__toft/runs/run-1" {
		t.Errorf("--no-follow = %v", doc)
	}
}

// A redirect off the apps address is answered, not followed: the app chose
// where to send us, and the host it named never sees the request (CLI-1).
func TestFetchDoesNotFollowARedirectOffTheAppsAddress(t *testing.T) {
	var reached bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer elsewhere.Close()
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", elsewhere.URL+"/steal")
		w.WriteHeader(http.StatusFound)
	}))
	_ = cmdFetch(e, []string{"/", "--app", "demo", "--json"})
	doc := oneDocument(t, stdout)
	if reached {
		t.Error("the redirect off the apps address was followed")
	}
	if doc["status"] != float64(302) || doc["headers"].(map[string]any)["Location"] != elsewhere.URL+"/steal" {
		t.Errorf("doc = %v", doc)
	}
}

func TestRoughAgeIsSingularForOne(t *testing.T) {
	for d, want := range map[time.Duration]string{
		90 * time.Second: "1 minute", 5 * time.Minute: "5 minutes",
		61 * time.Minute: "1 hour", 3 * time.Hour: "3 hours", 50 * time.Hour: "2 days",
	} {
		if got := roughAge(d); got != want {
			t.Errorf("roughAge(%s) = %q, want %q", d, got, want)
		}
	}
}

// An installed skill is a pointer: the embedded front matter, so the agent
// still knows when to load it, over a body that sends it to `toft skill`. With
// no agent folder in the home directory nothing is created.
func TestSkillInstallWritesPointers(t *testing.T) {
	e, _ := testEnv()
	dir := t.TempDir()
	if err := cmdSkill(e, []string{"install", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"toft-cli", "toft-sdk"} {
		got, err := os.ReadFile(filepath.Join(dir, n, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		embedded, _ := os.ReadFile("../deploy/cli/skills/" + n + "/SKILL.md")
		fm := frontMatter(string(embedded))
		if fm == "" || !strings.HasPrefix(string(got), fm) {
			t.Errorf("%s does not start with the embedded front matter", n)
		}
		if !strings.Contains(string(got), "toft skill "+n+"\n") {
			t.Errorf("%s does not send the agent to toft skill %s", n, n)
		}
		if len(got) > 2048 {
			t.Errorf("%s is %d bytes; a pointer, not a copy", n, len(got))
		}
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Nothing to write into is a coded refusal, exit 2 (BB-022).
	if err := cmdSkill(e, []string{"install"}); refusalCode(err) != "no_agent_folder" {
		t.Fatalf("install with no agent folder = %v", err)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("created %v in a home with no agent folder", entries)
	}
	os.Mkdir(filepath.Join(home, ".agents"), 0o755)
	if err := cmdSkill(e, []string{"install"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents/skills/toft-cli/SKILL.md")); err != nil {
		t.Error("did not install into the existing ~/.agents")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Error("created ~/.claude")
	}
}

// `toft hostname` reads without writing, and sends only the fields asked for;
// `changed` is whether the state moved, so a repeated --off is false (BB-026).
func TestHostnameSendsOnlyWhatWasAsked(t *testing.T) {
	var patches []string
	enabled := true
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"app":{"hostname":"demo.example.com","hostname_enabled":%t,"public_url":"https://demo.example.com"}}`, enabled)
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			patches = append(patches, string(b))
			var body struct {
				Enabled *bool `json:"hostname_enabled"`
			}
			json.Unmarshal(b, &body)
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			fmt.Fprint(w, `{}`)
		}
	})
	if err := cmdHostname(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["hostname"] != "demo.example.com" || doc["public_url"] != "https://demo.example.com" ||
		doc["changed"] != false || len(patches) != 0 {
		t.Errorf("reading wrote: %v %v", doc, patches)
	}
	stdout.Reset()
	if err := cmdHostname(e, []string{"--off", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["changed"] != true || doc["hostname_enabled"] != false {
		t.Errorf("--off: %v", doc)
	}
	stdout.Reset()
	if err := cmdHostname(e, []string{"--off", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["changed"] != false {
		t.Errorf("a second --off reported a change: %v", doc)
	}
	stdout.Reset()
	patches = nil
	if err := cmdHostname(e, []string{"demo.example.com", "--on", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if doc := oneDocument(t, stdout); doc["changed"] != true || len(patches) != 1 ||
		!strings.Contains(patches[0], `"hostname":"demo.example.com"`) || !strings.Contains(patches[0], `"hostname_enabled":true`) {
		t.Errorf("doc = %v patches = %v", doc, patches)
	}
	if err := cmdHostname(e, []string{"--on", "--off", "--app", "demo"}); err == nil {
		t.Error("--on with --off was accepted")
	}
}
