package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"toft/cli/internal/client"
)

// **stdout is the answer, and every command takes --json.**
//
// The guard against the next command forgetting the convention. It is what
// makes these composable — `toft job runs --json | jq '.[] | select(.status=="failed")'`
// is the thing an operator will actually write — and a command that quietly
// omitted it would only be discovered by somebody's broken pipeline.
func TestEveryCommandTakesJSON(t *testing.T) {
	cases := []struct {
		name string
		run  func(*env, []string) error
		args []string
	}{
		{"restart", cmdRestart, []string{"--app", "demo", "--json"}},
		{"start", cmdStart, []string{"--app", "demo", "--json"}},
		{"stop", cmdStop, []string{"--app", "demo", "--json"}},
		{"events", cmdEvents, []string{"--app", "demo", "--json"}},
		{"job list", cmdJob, []string{"list", "--app", "demo", "--json"}},
		{"job runs", cmdJob, []string{"runs", "--app", "demo", "--json"}},
		{"job run", cmdJob, []string{"run", "nightly", "--app", "demo", "--json"}},
		{"job logs", cmdJob, []string{"logs", "run-1", "--app", "demo", "--json"}},
		{"status", cmdStatus, []string{"--app", "demo", "--json"}},
		{"apps", cmdApps, []string{"--json"}},
		{"deps", cmdDeps, []string{"--app", "demo", "--json"}},
		{"link", cmdLink, []string{"--json", "demo"}},
		{"login --status", cmdLogin, []string{"--status", "--json"}},
		{"logs --deploy", cmdLogs, []string{"--app", "demo", "--deploy", "dep-1", "--json"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, buf := fakeInstall(t, everythingFake(t))
			if err := tc.run(e, tc.args); err != nil {
				t.Fatalf("%s --json: %v", tc.name, err)
			}
			out := strings.TrimSpace(buf.String())
			// A run's log with no lines, or a quiet app's events, is no
			// documents: NDJSON, one per line (BB-015, third 2026-11-03
			// list; BB-035, fourth).
			if out == "" && tc.name != "job logs" && tc.name != "events" {
				t.Fatalf("%s --json wrote nothing to stdout", tc.name)
			}
			for _, line := range strings.Split(out, "\n") {
				if line == "" {
					continue
				}
				var any any
				if err := json.Unmarshal([]byte(line), &any); err != nil {
					t.Errorf("%s --json put a line on stdout that is not JSON: %q", tc.name, line)
				}
			}
		})
	}
}

// The three deploy-shaped commands take --json too, but their answer is the
// deploy stream — NDJSON frames then one result object — so they are checked
// for accepting the flag rather than for one parseable document.
func TestDeployShapedCommandsTakeJSON(t *testing.T) {
	cases := []struct {
		name string
		run  func(*env, []string) error
		args []string
	}{
		// --no-watch prints the deployment id, the same bare-id answer `toft
		// deploy --no-watch` gives; what is asserted here is that --json is a
		// flag these commands have, not that it reshapes that answer.
		{"redeploy", cmdRedeploy, []string{"--app", "demo", "--json", "--no-watch"}},
		{"rollback", cmdRollback, []string{"--app", "demo", "--json", "--yes", "--no-watch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := fakeInstall(t, everythingFake(t))
			if err := tc.run(e, tc.args); err != nil {
				t.Fatalf("%s --json: %v", tc.name, err)
			}
		})
	}
}

// **The guard that catches the next command, rather than the ones somebody
// remembered to list.**
//
// `toft rollback --json` was accepted and ignored for as long as the flag
// existed: globalFlags registers --json on every command, so nothing refuses
// it, and a command that never reads it prints a table to a caller that asked
// for JSON. That is worse than not offering the flag, and it is invisible from
// the outside — which is why the two lists above have to be checked against the
// dispatch table rather than trusted.
func TestEveryCommandIsCheckedForJSON(t *testing.T) {
	checked := map[string]bool{
		// The single-document commands, from TestEveryCommandTakesJSON.
		"restart": true, "start": true, "stop": true, "events": true,
		"job": true, "status": true, "apps": true,
		// `top` off a terminal is `apps`, --json included: TestTopIsAppsOffATerminal.
		"top": true,
		// `task` only refuses, and a refusal is a JSON document like any other.
		"task": true,
		"deps": true, "link": true, "login": true, "logs": true,
		// The deploy-shaped ones.
		"redeploy": true, "rollback": true, "deploy": true,
		// `store` and `sdk` branch on g.json inside their own subcommand
		// tests; `create` is covered by create_test.go.
		"store": true, "sdk": true, "create": true,
		// `storage ls` passes the install's own answer through:
		// TestStorageListPassesTheInstallsAnswerThrough.
		"storage": true,
		// `api` answers with the install's own JSON, byte for byte, whether or
		// not --json was given — there is no second rendering for the flag to
		// choose between. TestAPIPassthroughReturnsTheInstallsOwnJSON holds
		// that, which is the property this list is really asking about.
		"api": true,
		// The agent-facing verbs, each driven with --json in
		// agent_commands_test.go.
		"env": true, "visibility": true, "delete": true, "deployments": true,
		"fetch": true, "check": true, "whoami": true, "skill": true, "hostname": true,
	}
	for name := range commandTable() {
		if !checked[name] {
			t.Errorf("`toft %s` is a command and no test checks that it honours --json — "+
				"either give it a --json answer and list it here, or make it refuse the flag", name)
		}
	}
	for name := range checked {
		if _, ok := commandTable()[name]; !ok {
			t.Errorf("this test claims to check `toft %s`, which is not a command", name)
		}
	}
}

// everythingFake answers every route the new commands touch, plausibly enough
// for one call each.
func everythingFake(t *testing.T) http.HandlerFunc {
	t.Helper()
	return everythingFakeOr(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})
}

// everythingFakeOr is everythingFake with the answer for a route it does not
// know left to the caller.
func everythingFakeOr(unknown http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/by-slug/demo"):
			fmt.Fprint(w, appRowWithRepo("running", "https://github.com/corp/demo", "main"))
		case strings.HasSuffix(p, "/restart"):
			fmt.Fprint(w, `{"status":"restarted"}`)
		case strings.HasSuffix(p, "/stop"):
			fmt.Fprint(w, `{"status":"stopped"}`)
		case strings.HasSuffix(p, "/deploy"):
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"dep-1"}`)
		case strings.HasSuffix(p, "/events"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(p, "/jobs"):
			fmt.Fprint(w, `{"jobs":[{"id":"j1","name":"nightly","memory_mb":512,"enabled":true}],"jobs_available":true}`)
		case strings.Contains(p, "/job-runs/") && strings.HasSuffix(p, "/logs"):
			fmt.Fprint(w, `{"lines":[],"status":"succeeded"}`)
		case strings.Contains(p, "/jobs/") && strings.HasSuffix(p, "/runs") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"run-1","job":"nightly","status":"succeeded"}`)
		// The envelope the route actually answers with. This stub used to serve
		// a bare array, which is the shape the *client* wanted rather than the
		// one the platform sends — so the test passed against a reply no
		// install has ever produced, and `toft job runs` was broken in every form
		// against every real one. A stub that agrees with the bug cannot see it.
		case strings.HasSuffix(p, "/job-runs"):
			fmt.Fprint(w, `{"runs":[{"id":"run-1","job":"nightly","status":"succeeded"}]}`)
		case strings.Contains(p, "/job-runs/"):
			fmt.Fprint(w, `{"id":"run-1","job":"nightly","status":"succeeded"}`)
		case strings.Contains(p, "/runs/") && strings.HasSuffix(p, "/logs"):
			fmt.Fprint(w, `{"lines":[{"at":"2026-08-21T03:00:01Z","line":"done"}]}`)
		case strings.HasSuffix(p, "/run") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"status":"started","run":{"id":"cr-1","status":"success"}}`)
		case strings.HasSuffix(p, "/runs"):
			fmt.Fprint(w, `[{"id":"cr-1","status":"success","duration_ms":12,`+
				`"started_at":"2026-08-21T03:00:00Z"}]`)
		case p == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":""}`)
		case strings.HasSuffix(p, "/dependencies"):
			fmt.Fprint(w, `{"mode":"warn","enforcing":false,"available":true,`+
				`"feed_stale":false,"has_data":true,"scanned":true,"waivers":[]}`)
		case strings.HasSuffix(p, "/deployments/dep-1"):
			fmt.Fprint(w, `{"id":"dep-1","status":"failed","build_log":"ERROR: no"}`)
		case strings.HasSuffix(p, "/deployments/dep-1/logs"):
			fmt.Fprint(w, `{"log":"ERROR: no"}`)
		case strings.HasSuffix(p, "/deployments"):
			fmt.Fprint(w, `[{"id":"dep-1","status":"success","serving":true,`+
				`"current":true,"branch":"main"},`+
				`{"id":"dep-0","status":"success","branch":"main"}]`)
		case p == "/api/v1/apps":
			fmt.Fprint(w, `[{"id":"app-1","slug":"demo","status":"running"}]`)
		case strings.HasSuffix(p, "/rollback"):
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"dep-2"}`)
		default:
			unknown(w, r)
		}
	}
}

// **`--json` has to be known before any command parses anything.**
//
// The refusals a program most needs to read — a bad flag, no credential — never
// reach a command's FlagSet, so `run` scans the raw arguments. It is handed the
// whole argv including the command name, and reading that name as a positional
// (which it is) made every one of those refusals print nothing on stdout. Caught
// by running the binary, not by a test, which is why there is one now.
func TestWantsJSONReadsTheFlagsAfterTheCommand(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--json"}, true},
		{[]string{"-json"}, true},
		{[]string{"--app", "demo", "--json"}, true},
		{[]string{}, false},
		{[]string{"--quiet"}, false},
		// **A flag's value is not a positional, and this does not try to tell
		// them apart.** `--app demo --json` is the most likely way anybody
		// writes it, and stopping at `demo` was the bug.
		{[]string{"--app", "demo", "--json"}, true},
		// After a positional it is still read. `parse` will refuse the flag
		// there and say so — and a caller that typed `--json` should get that
		// refusal in the shape it asked for.
		{[]string{"demo", "--json"}, true},
		// `--` ends the flags.
		{[]string{"--", "--json"}, false},
		// A value that merely contains the word is not the flag.
		{[]string{"--app", "my--json-app"}, false},
		{[]string{"--app", "json"}, false},
	}
	for _, tc := range cases {
		if got := wantsJSON(tc.args); got != tc.want {
			t.Errorf("wantsJSON(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// A refusal is a document on stdout, and it is the exit code's own account of
// itself — a caller that captured the output and lost the status should not have
// to guess.
func TestRefusalIsJSONOnStdout(t *testing.T) {
	e, buf := fakeInstall(t, everythingFake(t))
	e.json = true

	exit := e.refuse(&usageError{msg: "no app is linked in this directory"}, "usage", "", exitUsage)
	if exit != exitUsage {
		t.Errorf("refuse returned %d, want %d", exit, exitUsage)
	}

	var doc map[string]any
	line := strings.TrimSpace(buf.String())
	// The sentence goes to stderr, which this fixture points at the same
	// buffer, so take the last line — which is the document.
	parts := strings.Split(line, "\n")
	if err := json.Unmarshal([]byte(parts[len(parts)-1]), &doc); err != nil {
		t.Fatalf("refusal was not JSON: %q", line)
	}
	if doc["kind"] != "usage" || doc["exit"] != float64(exitUsage) {
		t.Errorf("refusal document is %v", doc)
	}
	if _, named := doc["code"]; named {
		t.Error("an install that named no code must not get an empty one — a consumer " +
			"testing for the key would read \"\" as a category")
	}
	// The human still gets the sentence.
	if !strings.Contains(line, "no app is linked") {
		t.Error("the sentence did not reach stderr")
	}
}

// Without --json a refusal writes no document at all: the sentence on stderr is
// the whole answer, and a stray object on stdout would land in somebody's
// redirected output.
func TestRefusalIsSilentOnStdoutWithoutJSON(t *testing.T) {
	e, buf := fakeInstall(t, everythingFake(t))
	e.json = false
	e.refuse(&usageError{msg: "no app is linked in this directory"}, "usage", "", exitUsage)
	if strings.Contains(buf.String(), `"kind"`) {
		t.Errorf("wrote a document without --json: %q", buf.String())
	}
}

// The install's envelope reaches the refusal document: its hint, and its own
// answer to whether a retry could work — which may disagree with the status,
// and is the install's to decide.
func TestRefusalCarriesTheInstallsHintAndRetryable(t *testing.T) {
	e, buf := testEnv()
	e.json = true
	no := false
	err := &client.Error{Status: http.StatusServiceUnavailable, Code: "jobs_unavailable",
		Message: "jobs are off", Hint: "configuration, not the job", Retryable: &no}
	report(e, err)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var doc map[string]any
	if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); jerr != nil {
		t.Fatalf("not JSON: %q", buf.String())
	}
	if doc["hint"] != "configuration, not the job" || doc["retryable"] != false {
		t.Errorf("document = %v", doc)
	}
	if !strings.Contains(buf.String(), "    Configuration, not the job") {
		t.Errorf("the hint did not reach stderr: %q", buf.String())
	}
}

// An install older than the envelope says nothing about retrying, and the
// status answers; a refusal this client made itself is retryable only when the
// install could not be reached.
func TestRefusalRetryableWithoutTheEnvelope(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&client.Error{Status: http.StatusBadGateway, Message: "proxy"}, true},
		{&client.Error{Status: http.StatusConflict, Message: "busy"}, false},
		{&client.UnreachableError{URL: "https://x", Err: errors.New("refused")}, true},
		{&usageError{msg: "no app"}, false},
	}
	for _, c := range cases {
		e, buf := testEnv()
		e.json = true
		report(e, c.err)
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		var doc map[string]any
		json.Unmarshal([]byte(lines[len(lines)-1]), &doc)
		if doc["retryable"] != c.want {
			t.Errorf("%v: retryable = %v, want %v", c.err, doc["retryable"], c.want)
		}
	}
}

// The JSON nits a test drive found, one case each.
func TestJSONShapesAreTheirOwn(t *testing.T) {
	t.Run("env list: an empty value is present, a sensitive one is marked", func(t *testing.T) {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
				fmt.Fprint(w, appRow("running"))
				return
			}
			fmt.Fprint(w, `{"env_keys":["EMPTY","SECRET"],"env_vars":[{"key":"EMPTY","value":""},`+
				`{"key":"SECRET","sensitive":true}]}`)
		})
		if err := cmdEnv(e, []string{"list", "--app", "demo", "--json"}); err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		unmarshalNDJSON(buf.Bytes(), &rows)
		if len(rows) != 2 {
			t.Fatalf("rows = %s", buf)
		}
		if v, ok := rows[0]["value"]; !ok || v != "" {
			t.Errorf("an empty value is absent: %v", rows[0])
		}
		if _, ok := rows[1]["value"]; ok || rows[1]["sensitive"] != true {
			t.Errorf("a sensitive row: %v", rows[1])
		}
	})

	t.Run("deployments list: step_timings is an object, not a string of one", func(t *testing.T) {
		got := undoubleStepTimings(json.RawMessage(`[{"id":"d1","step_timings":"{\"build\":1200}","x":1},{"id":"d2"}]`))
		var rows []map[string]any
		if err := json.Unmarshal(got, &rows); err != nil {
			t.Fatal(err)
		}
		if st, ok := rows[0]["step_timings"].(map[string]any); !ok || st["build"] != float64(1200) || rows[0]["x"] != float64(1) {
			t.Errorf("rows = %s", got)
		}
	})

	t.Run("a host that does not resolve is not retryable", func(t *testing.T) {
		e, buf := testEnv()
		e.json = true
		report(e, &client.UnreachableError{URL: "https://nope.invalid",
			Err: &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}})
		var doc map[string]any
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		json.Unmarshal([]byte(lines[len(lines)-1]), &doc)
		if doc["retryable"] != false || doc["exit"] != float64(exitUnreachable) {
			t.Errorf("doc = %v", doc)
		}
		// Not retryable, so the hint names the address, never a retry.
		if h, _ := doc["hint"].(string); strings.Contains(h, "retry") || !strings.Contains(h, "does not resolve") {
			t.Errorf("hint = %q", h)
		}
	})
}
