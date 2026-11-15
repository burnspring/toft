package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

// The second 2026-11-03 black-box list, the CLI's half (TOFT-BB-006 …).

// BB-006: the usage refusals the report found uncoded each carry a code under
// --json, and a usage refusal that somehow has none still gets one.
func TestUsageRefusalsUnderJSONCarryACode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("TOFT_URL", "http://127.0.0.1:1")
	t.Setenv("TOFT_TOKEN", "toftu_test")
	t.Setenv("CI", "1")
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"link", "a", "--app", "b", "--json"}, "unexpected_argument"},
		{[]string{"delete", "--yes", "--json"}, "missing_argument"},
		{[]string{"job", "wait", "--json"}, "missing_argument"},
		// drills 2026-11-03: one operand short is missing, not unexpected.
		{[]string{"store", "mv", "toft://drop/a.csv", "--json"}, "missing_argument"},
		{[]string{"store", "cp", "a", "b", "c", "--json"}, "unexpected_argument"},
	} {
		stdout, _ := captureStd(t)
		if got := run(tc.args); got != exitUsage {
			t.Errorf("run(%v) = %d, want %d", tc.args, got, exitUsage)
			continue
		}
		if doc := lastDoc(t, stdout()); doc["code"] != tc.code || doc["kind"] != "usage" {
			t.Errorf("run(%v) document %v, want code %s", tc.args, doc, tc.code)
		}
	}

	e, buf := testEnv()
	e.json = true
	if exit := report(e, &usageError{msg: "something about the invocation"}); exit != exitUsage {
		t.Fatalf("exit %d", exit)
	}
	if !strings.Contains(buf.String(), `"code":"invalid_usage"`) {
		t.Errorf("an uncoded usage refusal went out with no code: %s", buf.String())
	}
}

// BB-006: refusals the report named by line each carry their code.
func TestNamedRefusalsCarryTheirCode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{refuseClobber(writeTemp(t), false), "file_exists"},
		{confirm(&env{stdinTerminal: func() bool { return false }}, &globalFlags{}, "delete demo"), "confirmation_required"},
	} {
		if got := refusalCode(tc.err); got != tc.code {
			t.Errorf("%v: code %q, want %q", tc.err, got, tc.code)
		}
	}
	// An install that does not answer is exit 4 and coded.
	e, buf := testEnv()
	e.json = true
	if exit := report(e, &client.UnreachableError{URL: "https://unreachable.invalid", Err: errors.New("no such host")}); exit != exitUnreachable {
		t.Fatalf("exit %d", exit)
	}
	if !strings.Contains(buf.String(), `"code":"unreachable"`) {
		t.Errorf("unreachable without a code: %s", buf.String())
	}
}

func writeTemp(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// BB-007: an unattended `toft login <url>` refuses, naming --token-stdin, and
// never asks the install for a device code.
func TestUnattendedLoginRefusesTheDeviceFlow(t *testing.T) {
	asked := false
	e, _, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		asked = true
		http.Error(w, "no", http.StatusTeapot)
	})
	e.stdinTerminal = func() bool { return false }
	err := cmdLogin(e, []string{os.Getenv("TOFT_URL")})
	if refusalCode(err) != "unattended_login" || !strings.Contains(err.Error(), "--token-stdin") {
		t.Fatalf("err = %v (code %q)", err, refusalCode(err))
	}
	if asked {
		t.Error("an unattended login started the device flow")
	}
}

// BB-008: the waited --json document carries the start answer's deduplicated.
func TestTheWaitedRunCarriesDeduplicated(t *testing.T) {
	var start jobRun
	if err := json.Unmarshal([]byte(`{"id":"r1","status":"queued","deduplicated":true}`), &start); err != nil {
		t.Fatal(err)
	}
	dedup := startDeduplicated(&start)
	if dedup == nil || !*dedup {
		t.Fatalf("start's deduplicated not read: %v", dedup)
	}
	var final jobRun
	json.Unmarshal([]byte(`{"id":"r1","status":"succeeded","exit_code":0}`), &final)
	withDeduplicated(&final, *dedup)
	b, _ := json.Marshal(final)
	if !strings.Contains(string(b), `"deduplicated":true`) || !strings.Contains(string(b), `"exit_code":0`) {
		t.Errorf("final document %s", b)
	}
	json.Unmarshal([]byte(`{"id":"r2","status":"queued","deduplicated":false}`), &start)
	if d := startDeduplicated(&start); d == nil || *d {
		t.Errorf("an explicit false was lost: %v", d)
	}
	withDeduplicated(&final, false)
	if b, _ := json.Marshal(final); !strings.Contains(string(b), `"deduplicated":false`) {
		t.Errorf("false not present: %s", b)
	}
}

// BB-010: job list --json names params `params` ({} when none) and says the
// timeout is enforced.
func TestJobListJSONHasParamsAndTimeoutEnforcement(t *testing.T) {
	e, buf := testEnv()
	defs := []jobDefinition{
		{Name: "hello", ParamsSpec: map[string]paramSpec{"n": {Type: "int", Required: true}},
			Limits: &jobLimits{Timeout: jobLimit{Value: 60, Source: "declared"},
				Memory: jobLimit{Value: 512, Source: "install default", Enforcement: "enforced"}}},
		{Name: "bare"},
	}
	if err := encodeJobs(e, defs); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := unmarshalNDJSON(buf.Bytes(), &rows); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	if p, ok := rows[0]["params"].(map[string]any); !ok || p["n"] == nil {
		t.Errorf("params: %v", rows[0]["params"])
	}
	if p, ok := rows[1]["params"].(map[string]any); !ok || len(p) != 0 {
		t.Errorf("a job with none: params %v", rows[1]["params"])
	}
	timeout := rows[0]["limits"].(map[string]any)["timeout"].(map[string]any)
	if timeout["enforcement"] != "enforced" {
		t.Errorf("timeout %v", timeout)
	}
}

// BB-014: the gateway's 401 and its run pages are the platform's answers.
func TestFetchNamesTheGatewaysOwnAnswers(t *testing.T) {
	for _, tc := range []struct {
		r    fetchResult
		want string
	}{
		{fetchResult{Status: 401, Body: "authentication required\n", URL: "https://a/apps/x/"}, "sign_in_required"},
		{fetchResult{Status: 404, Body: "no such run\n", URL: "https://a/apps/x/__toft/runs/0000"}, "run_page"},
		{fetchResult{Status: 401, Body: `{"error":"who are you"}`, URL: "https://a/apps/x/api"}, ""},
		{fetchResult{Status: 404, Body: "nope", URL: "https://a/apps/x/missing"}, ""},
	} {
		if got := platformPage(&tc.r); got != tc.want {
			t.Errorf("%d %s: %q, want %q", tc.r.Status, tc.r.URL, got, tc.want)
		}
	}
	err := platformAnswered(&fetchResult{Status: 503, Platform: "stopped"}, "demo", "")
	if !strings.HasPrefix(err.Error(), "the platform answered 503 in the app's place (stopped)") ||
		refusalCodeOf(err) != "stopped" {
		t.Errorf("stopped: %v", err)
	}
	err = platformAnswered(&fetchResult{Status: 401, Platform: "sign_in_required"}, "demo", "")
	if !strings.Contains(err.Error(), "the platform answered 401 in the app's place") {
		t.Errorf("sign in: %v", err)
	}
}

// BB-015: a toft.toml that does not parse is a check problem, exit 1.
func TestCheckOfAnUnparseableManifestIsAProblem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("TOFT_URL", "http://127.0.0.1:1")
	t.Setenv("TOFT_TOKEN", "toftu_test")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "toft.toml"), []byte("this is = = not toml\n"), 0o644)
	stdout, _ := captureStd(t)
	if got := run([]string{"check", dir, "--json"}); got != exitFailed {
		t.Fatalf("exit %d, want %d", got, exitFailed)
	}
	out := stdout()
	var doc struct {
		OK       bool `json:"ok"`
		Problems []struct{ Reason, Code, Hint, Message string }
	}
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(out), "\n")[0]), &doc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if doc.OK || len(doc.Problems) != 1 || doc.Problems[0].Code != "manifest_syntax" ||
		doc.Problems[0].Reason != "manifest_invalid" || doc.Problems[0].Hint == "" ||
		!strings.Contains(doc.Problems[0].Message, "line 1") {
		t.Errorf("document %s", out)
	}
}

// BB-016: check --json carries the .env and secret-file notices.
func TestCheckJSONCarriesTheBundleNotices(t *testing.T) {
	raw := json.RawMessage(`{"ok":true,"warnings":["x"],"warning_details":[{"code":"manifest_unread","message":"x"}]}`)
	got := withBundleNotices(raw, []string{".env"}, []string{"a.key"})
	var doc struct {
		Warnings []string
		Details  []map[string]any `json:"warning_details"`
	}
	json.Unmarshal(got, &doc)
	if len(doc.Warnings) != 3 || len(doc.Details) != 3 ||
		doc.Details[1]["code"] != "env_file_skipped" || doc.Details[2]["code"] != "secret_in_bundle" ||
		doc.Details[2]["path"] != "a.key" {
		t.Errorf("%s", got)
	}
	if string(withBundleNotices(raw, nil, nil)) != string(raw) {
		t.Error("nothing to add changed the answer")
	}
}

// BB-026: status reports the address the install says the app answers at.
func TestStatusURLFollowsTheHostname(t *testing.T) {
	s := &session{cfgRead: true, cfg: installConfig{AppsBaseURL: "https://apps.example.com"}}
	if got := s.publicURL(&appSummary{Slug: "demo", PublicURL: "https://demo.example.com"}); got != "https://demo.example.com" {
		t.Errorf("publicURL = %s", got)
	}
	if got := s.publicURL(&appSummary{Slug: "demo"}); got != "https://apps.example.com/apps/demo" {
		t.Errorf("fallback = %s", got)
	}
}

// BB-027: apps --json rows always carry owner_group_id, null when unset.
func TestAppsRowsCarryOwnerGroupID(t *testing.T) {
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"1","slug":"a","mine":true},{"id":"2","slug":"b","mine":true,"owner_group_id":"g1"},{"id":"3","slug":"c","mine":true,"owner_group_id":""}]`)
	})
	e.out = ui.New(false, false, false)
	e.out.Stdout = stdout
	e.out.Stderr = &bytes.Buffer{}
	if err := cmdApps(e, []string{"--mine", "--json"}); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := unmarshalNDJSON(stdout.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		v, ok := r["owner_group_id"]
		if !ok {
			t.Errorf("%v: no owner_group_id", r["slug"])
		}
		if r["slug"] != "b" && v != nil {
			t.Errorf("%v: owner_group_id %v, want null", r["slug"], v)
		}
	}
}

// drills 2026-11-03: params over the cap are the invocation to fix — exit 2
// usage like every other param refusal, never a retryable-looking failure.
func TestParamsTooLargeIsAUsageRefusal(t *testing.T) {
	kind, exit := classifyRefusal(&client.Error{Status: http.StatusRequestEntityTooLarge, Code: "params_too_large"})
	if kind != "usage" || exit != exitUsage {
		t.Fatalf("params_too_large = %s/%d, want usage/%d", kind, exit, exitUsage)
	}
}
