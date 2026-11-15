package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

// **A warning reaches the reader that cannot see the build log.**
//
// `toft.toml` ignores tables its readers do not name, which is what lets
// an older platform run a newer manifest — so a `[tasks]` table where the file
// wanted `[jobs]` is silently nothing. The platform warns about it in the build
// log, and a `--json` deploy never starts the log follower: the caller most
// likely to have written that table was the one that could not see the warning,
// and reading it cost a second command after every deploy on the chance that
// something had been ignored.
//
// A successful deploy is the case that matters. Nothing failed, so nothing
// sends anybody to the log.
func TestDeployCarriesTheWarningsTheBuildLogHad(t *testing.T) {
	const warning = "tasks in toft.toml is not read by Toft and was ignored"

	progress := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"success\","+
			"\"phase\":\"deploy\",\"step\":\"flip\",\"warnings\":[%q]}\n\n", warning)
	}

	t.Run("a plain deploy prints them", func(t *testing.T) {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/progress"):
				progress(w, r)
			case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
				fmt.Fprint(w, appRow("running"))
			default:
				fmt.Fprint(w, `{}`)
			}
		})
		s, app, err := jobSession(e, &globalFlags{app: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := watchDeploy(e, s, app, "d1", &globalFlags{}, false); err != nil {
			t.Fatalf("watchDeploy: %v", err)
		}
		if !strings.Contains(buf.String(), warning) {
			t.Errorf("a successful deploy said nothing about the ignored table:\n%s", buf.String())
		}
	})

	t.Run("--json carries them in the result object", func(t *testing.T) {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/progress"):
				progress(w, r)
			case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
				fmt.Fprint(w, appRow("running"))
			default:
				fmt.Fprint(w, `{}`)
			}
		})
		s, app, err := jobSession(e, &globalFlags{app: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := watchAsJSON(e, s, app, "d1"); err != nil {
			t.Fatalf("watchAsJSON: %v", err)
		}

		// The last line is the result object; the ones before it are the frames.
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		var result map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
			t.Fatalf("the result object did not parse (%v): %q", err, lines[len(lines)-1])
		}
		got, ok := result["warnings"].([]any)
		if !ok || len(got) != 1 || got[0] != warning {
			t.Errorf("result[warnings] = %v, want the ignored table — this is the shape the "+
				"documentation tells an agent to parse", result["warnings"])
		}
	})

	// Absent rather than empty when there are none: a consumer testing for the
	// key learns "this install does not report warnings" instead of reading []
	// as "there were none".
	t.Run("absent when there are none", func(t *testing.T) {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/progress"):
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\","+
					"\"status\":\"success\",\"phase\":\"deploy\",\"step\":\"flip\"}\n\n")
			case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
				fmt.Fprint(w, appRow("running"))
			default:
				fmt.Fprint(w, `{}`)
			}
		})
		s, app, err := jobSession(e, &globalFlags{app: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := watchAsJSON(e, s, app, "d1"); err != nil {
			t.Fatalf("watchAsJSON: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		var result map[string]any
		json.Unmarshal([]byte(lines[len(lines)-1]), &result)
		if _, present := result["warnings"]; present {
			t.Errorf("warnings was sent as %v when there were none", result["warnings"])
		}
	})
}

// A rollback is watched through a transient row the platform deletes on
// success; the id printed afterwards is the release that is now serving.
func TestRollbackNamesTheReleaseNowServing(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			switch {
			case strings.HasSuffix(p, "/by-slug/demo"):
				fmt.Fprint(w, appRow("running"))
			case strings.HasSuffix(p, "/rollback"):
				w.WriteHeader(http.StatusAccepted)
				fmt.Fprint(w, `{"id":"transient-9","source_deploy_id":"dep-0"}`)
			case strings.HasSuffix(p, "/progress"):
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"transient-9\",\"status\":\"success\","+
					"\"phase\":\"deploy\",\"step\":\"flip\"}\n\n")
			case strings.HasSuffix(p, "/deployments"):
				fmt.Fprint(w, `[{"id":"dep-1","status":"success","serving":true,"current":true,"branch":"main"},`+
					`{"id":"dep-0","status":"success","branch":"main"}]`)
			default:
				fmt.Fprint(w, `{}`)
			}
		})
		args := []string{"--app", "demo", "--yes"}
		if asJSON {
			args = append(args, "--json")
		}
		if err := cmdRollback(e, args); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		out := buf.String()
		if asJSON {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			var result map[string]any
			json.Unmarshal([]byte(lines[len(lines)-1]), &result)
			if result["deployment_id"] != "dep-0" {
				t.Errorf("--json names %v, want dep-0:\n%s", result["deployment_id"], out)
			}
			continue
		}
		if !strings.Contains(out, "deployment dep-0") || strings.Contains(out, "deployment transient-9") {
			t.Errorf("rollback did not name the serving release:\n%s", out)
		}
	}
}

// A plain rollback's row is deleted once the restore succeeds, and the progress
// route answers 404 after that. A watcher that arrives late reads the
// deployments list: the restored release serving is a success, anything else
// is the 404 (security review 2026-11-14, CS-1).
func TestARollbackWhoseRowIsAlreadyGone(t *testing.T) {
	for _, c := range []struct {
		name     string
		restored bool
		asJSON   bool
	}{
		{"restored, text", true, false},
		{"restored, json", true, true},
		{"not restored", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var posted atomic.Bool
			e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
				p := r.URL.Path
				switch {
				case strings.HasSuffix(p, "/by-slug/demo"):
					fmt.Fprint(w, appRow("running"))
				case strings.HasSuffix(p, "/rollback"):
					posted.Store(true)
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprint(w, `{"id":"transient-9","source_deploy_id":"dep-0"}`)
				case strings.HasSuffix(p, "/progress"):
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"error":"deployment not found","code":"not_found"}`)
				case strings.HasSuffix(p, "/deployments"):
					if posted.Load() && c.restored {
						fmt.Fprint(w, `[{"id":"dep-1","status":"success","branch":"main"},`+
							`{"id":"dep-0","status":"success","serving":true,"current":true,"branch":"main"}]`)
						return
					}
					fmt.Fprint(w, `[{"id":"dep-1","status":"success","serving":true,"current":true,"branch":"main"},`+
						`{"id":"dep-0","status":"success","branch":"main"}]`)
				default:
					fmt.Fprint(w, `{}`)
				}
			})
			args := []string{"--app", "demo", "--yes"}
			if c.asJSON {
				args = append(args, "--json")
			}
			err := cmdRollback(e, args)
			if !c.restored {
				var apiErr *client.Error
				if !errors.As(err, &apiErr) || !apiErr.NotFound() {
					t.Fatalf("want the 404 to stand, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			out := buf.String()
			if c.asJSON {
				lines := strings.Split(strings.TrimSpace(out), "\n")
				var result map[string]any
				json.Unmarshal([]byte(lines[len(lines)-1]), &result)
				if result["status"] != "success" || result["deployment_id"] != "dep-0" {
					t.Errorf("--json result %v, want success naming dep-0:\n%s", result, out)
				}
				return
			}
			if !strings.Contains(out, "deployment dep-0") {
				t.Errorf("rollback did not name the serving release:\n%s", out)
			}
		})
	}
}

// A deployment is named by a unique prefix of six or more characters; a short,
// absent or ambiguous one is exit 2 with a code, the candidates named.
func TestDeploymentIDPrefixes(t *testing.T) {
	ds := []deployment{{ID: "4f1a2b3c-aaaa"}, {ID: "4f1a2b9d-bbbb"}, {ID: "77e0c1d2-cccc"}}
	cases := []struct {
		given, want, code string
	}{
		{"77e0c1", "77e0c1d2-cccc", ""},
		{"4f1a2b3", "4f1a2b3c-aaaa", ""},
		{"4f1a2b9d-bbbb", "4f1a2b9d-bbbb", ""},
		{"4f1a2b", "", "ambiguous_id"},
		{"77e0", "", "invalid_id"},
		{"000000", "", "not_found"},
	}
	for _, c := range cases {
		got, err := matchDeploymentID(ds, c.given)
		if c.code == "" {
			if err != nil || got != c.want {
				t.Errorf("%q = %q, %v; want %q", c.given, got, err, c.want)
			}
			continue
		}
		var u *usageError
		if !errors.As(err, &u) || u.code != c.code {
			t.Errorf("%q = %v; want a usage error with code %s", c.given, err, c.code)
		}
		if c.code == "ambiguous_id" && !strings.Contains(err.Error(), "4f1a2b9d-bbbb") {
			t.Errorf("the ambiguity does not name the candidates: %v", err)
		}
	}
}

// --logs: the build's own last lines are printed before the line that says the
// build finished, not after it.
func TestDeployLogsFlushBeforeTheStepMarker(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			for _, f := range []string{
				`{"deployment_id":"d1","status":"building","phase":"build","step":"install"}`,
				`{"deployment_id":"d1","status":"building","phase":"deploy","step":"flip"}`,
				`{"deployment_id":"d1","status":"success","phase":"deploy","step":"flip"}`,
			} {
				fmt.Fprintf(w, "event: progress\ndata: %s\n\n", f)
			}
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			fmt.Fprint(w, `{"build_log":"Collecting flask\nSuccessfully installed flask\n"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	s, app, err := jobSession(e, &globalFlags{app: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := watchDeploy(e, s, app, "d1", &globalFlags{}, true); err != nil {
		t.Fatalf("watchDeploy: %v", err)
	}
	out := buf.String()
	logAt := strings.Index(out, "Successfully installed flask")
	markAt := strings.Index(out, "✓")
	if logAt < 0 || markAt < 0 || logAt > markAt {
		t.Errorf("the build log landed after its step marker:\n%s", out)
	}
}

// check and deploy render a warning one way. A sentence is printed as it is —
// check used to wrap every one in "toft.toml carries %s, which nothing reads"
// — and the bare key an older install sends in a check is made one.
func TestWarningsHaveOneRendering(t *testing.T) {
	const sentence = `this build uses Python 3.15.0, and pyproject.toml's requires-python "==3.13.*" excludes it`
	if got := warningSentence(sentence); got != sentence {
		t.Errorf("a sentence was rewritten: %q", got)
	}
	if got := warningSentence("tasks"); got != "tasks in toft.toml is not read by Toft and was ignored" {
		t.Errorf("a bare key = %q", got)
	}
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	renderCheck(e, &checkResult{OK: true, Framework: "flask", Warnings: []string{sentence}})
	if strings.Contains(buf.String(), "nothing reads") || !strings.Contains(buf.String(), "Warning: "+sentence) {
		t.Errorf("check rendered:\n%s", buf)
	}
}

// A secret-looking file in the bundle is a warning in the --json result, as it
// is a line (`including .env`) in the text one.
func TestDeployJSONNamesTheSecretFile(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"success\","+
				"\"phase\":\"deploy\",\"step\":\"flip\"}\n\n")
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	s, app, err := jobSession(e, &globalFlags{app: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	e.bundleSecrets, e.bundleEnvFiles = []string{"server.key"}, []string{".env"}
	if err := watchAsJSON(e, s, app, "d1"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var result struct {
		Warnings []string `json:"warnings"`
		Details  []struct {
			Code, Path string
		} `json:"warning_details"`
	}
	json.Unmarshal([]byte(lines[len(lines)-1]), &result)
	if len(result.Warnings) != 2 || !strings.HasPrefix(result.Warnings[1], "server.key ") {
		t.Errorf("warnings = %v", result.Warnings)
	}
	// Each with its code and path, as toft check gives them (L2).
	if len(result.Details) != 2 || result.Details[0].Code != "env_file_skipped" || result.Details[0].Path != ".env" ||
		result.Details[1].Code != "secret_in_bundle" || result.Details[1].Path != "server.key" {
		t.Errorf("warning_details = %+v", result.Details)
	}
}

// Rejoining a deploy that already finished reports the deploy's own duration,
// not the instant this client spent watching it ("0.0s").
func TestRejoiningAFinishedDeployReportsItsRealDuration(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"success\","+
				"\"phase\":\"deploy\",\"step\":\"flip\"}\n\n")
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprint(w, `[{"id":"d1","status":"success","started_at":"2026-10-21T10:00:00Z",`+
				`"finished_at":"2026-10-21T10:00:14Z"}]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	if err := cmdDeploy(e, []string{"--app", "demo", "--watch", "d1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "14.0s") {
		t.Errorf("no 14.0s in:\n%s", buf)
	}
}

// `toft deploy --watch <prefix>` resolves a prefix of six or more characters
// the way `logs --deploy` does, and watches the whole id it names (drills
// rerun 2026-10-22 #7: it was passed through and answered "not found").
func TestDeployWatchResolvesAPrefix(t *testing.T) {
	const full = "fa1a7b41-ef32-47ad-8633-a6570a9a5e14"
	var watched string
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			watched = r.URL.Path
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\""+full+"\",\"status\":\"success\","+
				"\"phase\":\"deploy\",\"step\":\"flip\"}\n\n")
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprint(w, `[{"id":"`+full+`","status":"success"},{"id":"0b6f7a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b","status":"success"}]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	if err := cmdDeploy(e, []string{"--app", "demo", "--watch", "fa1a7b41"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(watched, full) {
		t.Errorf("watched %q, want the whole id %s", watched, full)
	}
}

// -q drops narration, never the reason a deploy failed: it exited 1 having
// printed nothing at all (release check B4).
func TestAQuietFailedDeployStillSaysWhy(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"failed\","+
				"\"phase\":\"deploy\",\"step\":\"start\",\"reason\":\"not_ready\",\"message\":\"the app exited with status 1\"}\n\n")
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprint(w, `[{"id":"d1","status":"failed"}]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	e.out.Quiet = true
	err := cmdDeploy(e, []string{"--app", "demo", "--watch", "d1"})
	var failed *deployFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v, want a failed deploy", err)
	}
	if !strings.Contains(buf.String(), "the app exited with status 1") {
		t.Errorf("-q printed no reason:\n%q", buf.String())
	}
}

// `toft deploy --dry-run` is `toft check`: an unlinked directory is checked as
// a new app rather than refused for having nothing to deploy to.
func TestDryRunInAnUnlinkedDirectoryChecksANewApp(t *testing.T) {
	var route string
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		route = r.Method + " " + r.URL.Path
		fmt.Fprint(w, `{"ok":true,"framework":"static","not_checked":["dependencies"]}`)
	})
	os.WriteFile(filepath.Join(e.dir, "index.html"), []byte("x"), 0o644)
	if err := cmdDeploy(e, []string{"--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if route != "POST /api/v1/apps/check" {
		t.Errorf("checked against %s", route)
	}
}

// An argument-less rollback goes back from the serving release, never forward:
// after rolling B back to A, the next one is A's predecessor, not B again.
func TestRollbackTargetGoesBackFromTheServingRelease(t *testing.T) {
	rows := func(serving string) []deployment {
		out := []deployment{{ID: "c", Status: "success"}, {ID: "x", Status: "failed"},
			{ID: "b", Status: "success"}, {ID: "a", Status: "success"}}
		for i := range out {
			out[i].Serving = out[i].ID == serving
		}
		return out
	}
	if got, err := rollbackTarget(rows("c"), ""); err != nil || got.ID != "b" {
		t.Errorf("serving c: %v %v", got, err)
	}
	if got, err := rollbackTarget(rows("b"), ""); err != nil || got.ID != "a" {
		t.Errorf("serving b, after a rollback from c: %v %v", got, err)
	}
	_, err := rollbackTarget(rows("a"), "")
	var usage *usageError
	if !errors.As(err, &usage) || usage.code != "nothing_older" || !strings.Contains(err.Error(), "toft rollback c") {
		t.Errorf("serving the oldest: %v", err)
	}
	d := rows("a")
	if !newerThanServing(d, &d[0]) || newerThanServing(rows("c"), &d[3]) {
		t.Error("newerThanServing")
	}
}

// With the policy off there is nothing below the reason, so the corpus
// sentence ("nothing below has been checked") is not printed above a blank.
func TestDepsWithThePolicyOffSaysOnlyThat(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	printDependencyState(e, &appSummary{Slug: "demo"}, &dependencyState{Mode: "off",
		Reason: "Dependency policy is off on this install."})
	if strings.Contains(buf.String(), "nothing below") || !strings.Contains(buf.String(), "policy is off") {
		t.Errorf("printed:\n%s", buf)
	}
}

// A manifest may name the install and the deploy does not ask (ADR §42); the
// line naming the install is what stands in for the question, so the origin
// strings the resolver writes must keep reading as a manifest's.
func TestADeployNamesAnInstallItsManifestChose(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TOFT_INSTALL", "")
	for _, file := range []string{config.ManifestFile, config.LocalManifestFile} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, file), []byte("[app]\ninstall = \"prod\"\nslug = \"payroll\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := config.NewResolver(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Install(); got.Value != "prod" || !manifestOrigin(got.Origin) {
			t.Errorf("%s: install %+v is not read as the manifest's", file, got)
		}
		flagged, _ := config.NewResolver(dir, map[config.Key]string{config.KeyInstall: "dev"})
		if got := flagged.Install(); manifestOrigin(got.Origin) {
			t.Errorf("%s: --install is read as the manifest's: %+v", file, got)
		}
	}
}

// A slug nobody holds is not_found under --json, as every other exit-2 lookup
// is (drills 2026-10-28 evening #6).
func TestNoSuchAppIsNotFound(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found","code":"not_found"}`)
	})
	_, _, err := jobSession(e, &globalFlags{app: "nobody"})
	var usage *usageError
	if !errors.As(err, &usage) || usage.code != "not_found" {
		t.Errorf("err = %#v, want a usage error coded not_found", err)
	}
}

// toft check says what [restart] and [render] would record, beside the jobs.
func TestCheckSaysTheSchedules(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	renderCheck(e, &checkResult{OK: true, Framework: "flask",
		Restart: &checkSchedule{Schedule: "0 4 * * *", Timezone: "Europe/London", NextRunAt: "2026-10-31T04:00:00Z"},
		Render:  &checkSchedule{Schedule: "0 6 * * 1"}})
	out := buf.String()
	if !strings.Contains(out, "restart") || !strings.Contains(out, "0 4 * * * (Europe/London)") ||
		!strings.Contains(out, "next 2026-10-31T04:00:00Z") || !strings.Contains(out, "0 6 * * 1 (UTC)") {
		t.Errorf("check rendered:\n%s", out)
	}
}

// A deploy that failed with a finer code than its reason carries both in the
// one result object — storage_unknown beside storage_refused, as toft check
// names it — and one that sent no code carries its reason as the code, with
// retryable false: the result is the failed deploy's final, coded answer
// (BB-018).
func TestDeployJSONCarriesTheFailuresCode(t *testing.T) {
	for _, tc := range []struct{ frame, want string }{
		{`"reason":"storage_refused","code":"storage_revoked_during_deploy"`, "storage_revoked_during_deploy"},
		{`"reason":"build_failed"`, "build_failed"},
		{`"x":1`, "deploy_failed"},
	} {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/progress"):
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"failed\","+
					"\"phase\":\"deploy\",\"step\":\"flip\",\"message\":\"no\","+tc.frame+"}\n\n")
			case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
				fmt.Fprint(w, appRow("running"))
			default:
				fmt.Fprint(w, `{}`)
			}
		})
		s, app, err := jobSession(e, &globalFlags{app: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		_ = watchAsJSON(e, s, app, "d1")
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		var result map[string]any
		json.Unmarshal([]byte(lines[len(lines)-1]), &result)
		got := result["code"]
		if result["retryable"] != false {
			t.Errorf("retryable = %v, want false (result %v)", result["retryable"], result)
		}
		if got != tc.want {
			t.Errorf("code = %v, want %s (result %v)", got, tc.want, result)
		}
	}
}
