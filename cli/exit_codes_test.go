package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/cli/internal/client"
)

// The exit-code contract: **a name that does not resolve, and an argument the
// caller got wrong, is 2** — never 1, which says the platform tried and failed
// and sends a reader to a log that does not exist. Each case is a path a test
// drive found answering 1, and each must also carry a `code` in the --json
// envelope, because the exit status alone does not say which mistake it was.
func TestArgumentMistakesExitTwoWithACode(t *testing.T) {
	const fullID = "0b6f7a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	notFound := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found","code":"not_found","retryable":false}`)
	}
	cases := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request) bool // true when it answered
		setup   func(t *testing.T, e *env)
		run     func(e *env) error
		code    string
	}{
		{
			name: "logs --deploy with an id that names nothing",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if strings.Contains(r.URL.Path, "/deployments/") {
					notFound(w, r)
					return true
				}
				return false
			},
			run:  func(e *env) error { return cmdLogs(e, []string{"--app", "demo", "--deploy", fullID, "--json"}) },
			code: "not_found",
		},
		{
			name: "logs --source that is not a voice",
			run:  func(e *env) error { return cmdLogs(e, []string{"--app", "demo", "--source", "both", "--json"}) },
		},
		{
			name: "store ls of a store nobody can reach",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasPrefix(r.URL.Path, "/api/v1/store/") {
					notFound(w, r)
					return true
				}
				return false
			},
			run:  func(e *env) error { return cmdStore(e, []string{"ls", "--json", "nope"}) },
			code: "not_found",
		},
		{
			// A named store on an install with no data plane: the install says
			// which absence it is, and the client keeps the code rather than
			// calling a store that may well exist unknown.
			name: "store ls of a named store on an install with no store uploads",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasPrefix(r.URL.Path, "/api/v1/store/") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"error":"this install does not offer store uploads","code":"store_uploads_off"}`)
					return true
				}
				return false
			},
			run:  func(e *env) error { return cmdStore(e, []string{"ls", "--json", "drop"}) },
			code: "store_uploads_off",
		},
		{
			name: "store get from a store nobody can reach",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasPrefix(r.URL.Path, "/api/v1/store/") {
					notFound(w, r)
					return true
				}
				return false
			},
			run: func(e *env) error {
				return cmdStore(e, []string{"get", "--json", "-o", filepath.Join(e.dir, "x"), "nope", "a.txt"})
			},
			code: "not_found",
		},
		{
			name: "a toft.toml that does not parse",
			setup: func(t *testing.T, e *env) {
				if err := os.WriteFile(filepath.Join(e.dir, "toft.toml"), []byte("[app\nslug = "), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			run:  func(e *env) error { return cmdStatus(e, []string{"--json"}) },
			code: "manifest_invalid",
		},
		{
			name: "create of a directory that is not there",
			run:  func(e *env) error { return cmdCreate(e, []string{"--yes", "--json", "toft-testdrive"}) },
			code: "no_such_directory",
		},
		{
			// Resolved before the confirmation: unattended and without --yes,
			// the refusal is about the store, not the missing flag.
			name: "store rm in a store nobody can reach, asked before --yes",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/api/v1/store" {
					fmt.Fprint(w, `{"stores":[{"slug":"drop"}]}`)
					return true
				}
				return false
			},
			run:  func(e *env) error { return cmdStore(e, []string{"rm", "--json", "nope/a.txt"}) },
			code: "not_found",
		},
		{
			name: "env set with a name no environment can carry",
			run:  func(e *env) error { return cmdEnv(e, []string{"set", "--app", "demo", "--json", "1BAD=x"}) },
			code: "invalid_env_name",
		},
		{
			name: "rollback with no earlier deployment at all",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/deployments") {
					fmt.Fprint(w, `[{"id":"d1","status":"success","serving":true,"current":true}]`)
					return true
				}
				return false
			},
			run:  func(e *env) error { return cmdRollback(e, []string{"--app", "demo", "--yes", "--json"}) },
			code: "nothing_older",
		},
		{
			name: "job run with a parameter that has no name",
			run: func(e *env) error {
				return cmdJob(e, []string{"run", "slow", "--app", "demo", "--param", "=x", "--json"})
			},
			code: "invalid_param",
		},
		{
			// drills rerun 2026-10-22 #9: exit 2 with no code.
			name: "env unset of a variable the app does not have",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/api/v1/apps/app-1/env" && r.Method == http.MethodPatch {
					fmt.Fprint(w, `{"unset":[],"not_present":["NOT_THERE"],"restart_needed":false}`)
					return true
				}
				return false
			},
			run:  func(e *env) error { return cmdEnv(e, []string{"unset", "--app", "demo", "--json", "NOT_THERE"}) },
			code: "env_not_set",
		},
		{
			name: "env set of a variable the platform sets",
			run:  func(e *env) error { return cmdEnv(e, []string{"set", "--app", "demo", "--json", "PORT=9000"}) },
			code: "reserved_env_name",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
				if c.handler != nil && c.handler(w, r) {
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
					fmt.Fprint(w, appRow("running"))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusTeapot)
				}
			})
			if c.setup != nil {
				c.setup(t, e)
			}
			e.json = true
			got := report(e, c.run(e))
			if got != exitUsage {
				t.Fatalf("exited %d, want %d:\n%s", got, exitUsage, buf)
			}
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			var doc map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil {
				t.Fatalf("the last line is not the refusal document: %q", lines[len(lines)-1])
			}
			if doc["kind"] != "usage" || doc["exit"] != float64(exitUsage) {
				t.Errorf("document %v", doc)
			}
			if c.code != "" && doc["code"] != c.code {
				t.Errorf("code = %v, want %q", doc["code"], c.code)
			}
		})
	}
}

// **The install's refusals map onto the contract in one place**
// (classifyRefusal), so a verb that never wrapped its own call cannot answer 1
// for something that never ran. Each case is one a test drive found exiting 1.
func TestInstallRefusalsMapOntoTheContract(t *testing.T) {
	const fullID = "00000000-0000-0000-0000-000000000000"
	refuse := func(w http.ResponseWriter, status int, body string) bool {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
		return true
	}
	cases := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request) bool
		run     func(e *env) error
		exit    int
		kind    string
		code    string
	}{
		{
			name: "deployments delete of an id that names nothing",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				return r.Method == http.MethodDelete && refuse(w, http.StatusNotFound,
					`{"error":"deployment not found","code":"not_found"}`)
			},
			run: func(e *env) error {
				return cmdDeployments(e, []string{"delete", fullID, "--app", "demo", "--yes", "--json"})
			},
			exit: exitUsage, kind: "usage", code: "not_found",
		},
		{
			name: "deployments delete of the serving deployment",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				return r.Method == http.MethodDelete && refuse(w, http.StatusConflict,
					`{"error":"that release is serving — deploy or promote another build first","code":"deployment_protected"}`)
			},
			run: func(e *env) error {
				return cmdDeployments(e, []string{"delete", fullID, "--app", "demo", "--yes", "--json"})
			},
			exit: exitAuth, kind: "refused", code: "deployment_protected",
		},
		{
			name: "deploy --watch of an id that names nothing",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				return strings.Contains(r.URL.Path, "/deployments/") && refuse(w, http.StatusNotFound,
					`{"error":"deployment not found","code":"not_found"}`)
			},
			run:  func(e *env) error { return cmdDeploy(e, []string{"--app", "demo", "--watch", fullID, "--json"}) },
			exit: exitUsage, kind: "usage", code: "not_found",
		},
		{
			// A prefix is resolved against the app's deployments first, as
			// `logs --deploy` resolves one, so one that names nothing is the
			// resolver's not_found and no watch is attempted.
			name: "deploy --watch of a prefix that names nothing",
			handler: func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/deployments") {
					fmt.Fprint(w, `[{"id":"fa1a7b41-ef32-47ad-8633-a6570a9a5e14","status":"success"}]`)
					return true
				}
				return false
			},
			run:  func(e *env) error { return cmdDeploy(e, []string{"--app", "demo", "--watch", "e6ddc6", "--json"}) },
			exit: exitUsage, kind: "usage", code: "not_found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
				if c.handler(w, r) {
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
					fmt.Fprint(w, appRow("running"))
				case strings.HasSuffix(r.URL.Path, "/steps"):
					fmt.Fprint(w, `[]`)
				case r.URL.Path == "/api/v1/config":
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusTeapot)
				}
			})
			e.json = true
			if got := report(e, c.run(e)); got != c.exit {
				t.Fatalf("exited %d, want %d:\n%s", got, c.exit, buf)
			}
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			var doc map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil {
				t.Fatalf("the last line is not the refusal document: %q", lines[len(lines)-1])
			}
			if doc["kind"] != c.kind || doc["exit"] != float64(c.exit) || doc["code"] != c.code {
				t.Errorf("document %v", doc)
			}
		})
	}
}

// **A credential the environment misdescribes is exit 2 with a code**
// (drills rerun 2026-10-22 #9): a token with no install beside it, and a token
// and an install with a conflicting --install, were both refusals with no
// `code`, which the runbook lists among those that carry one.
func TestEnvironmentCredentialRefusalsCarryACode(t *testing.T) {
	for _, c := range []struct {
		name    string
		envURL  string
		args    []string
		code    string
		mention string
	}{
		{"a token with no install named anywhere", "", []string{"--json"}, "missing_install", "TOFT_URL"},
		{"a token and an install, and another --install", "https://one.corp",
			[]string{"--install", "https://two.corp", "--json"}, "ambiguous_credential", "--install"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home+"/config")
			t.Setenv("TOFT_TOKEN", "toftd_ci")
			t.Setenv("TOFT_TOKEN_FILE", "")
			t.Setenv("TOFT_URL", c.envURL)
			e, buf := testEnv()
			e.json = true
			if got := report(e, cmdApps(e, c.args)); got != exitUsage {
				t.Fatalf("exit %d, want %d:\n%s", got, exitUsage, buf)
			}
			if !strings.Contains(buf.String(), `"code":"`+c.code+`"`) || !strings.Contains(buf.String(), c.mention) {
				t.Errorf("want code %s naming %s:\n%s", c.code, c.mention, buf)
			}
		})
	}
}

// A deploy refused at the upload carries the install's reason in the one
// document --json writes, as a deploy that failed later does (docs/bugs
// 2026-10-28 #3: the agent's switch on `reason` fell through).
func TestAnUploadRefusalCarriesItsReasonUnderJSON(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	e.json = true
	refusal := &client.Error{Status: 422, Code: "limit_exceeds_ceiling", Reason: "manifest_invalid",
		Message: "timeout is above the ceiling"}
	e.errorDocument(refusal, "failed", refusal.Code, exitFailed)
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("not one document: %q", buf.String())
	}
	if doc["reason"] != "manifest_invalid" || doc["code"] != "limit_exceeds_ceiling" {
		t.Errorf("doc = %v, want reason beside code", doc)
	}
}
