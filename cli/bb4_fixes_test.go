package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fourth 2026-11-03 black-box list, the CLI's half (TOFT-BB-033 …).

// BB-034: cp -r and mv -r of a source that is not there are not_found, exit
// 2, as the forms without -r are — not "nothing to copy" and exit 0. An
// object named with -r is that object.
func TestRecursiveCopyOfAMissingSourceIsNotFound(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/keep.txt", "k")
	f.put("reports/fold/x.txt", "x")
	dir := t.TempDir()

	for _, args := range [][]string{
		{"mv", "-r", "toft://reports/nope", "toft://reports/nope2", "--json"},
		{"cp", "-r", "toft://reports/nope/", "toft://reports/elsewhere/", "--json"},
		{"cp", "-r", "toft://reports/nope", dir + "/x", "--json"},
	} {
		code, out := storeRun(t, f, args...)
		if code != exitUsage || !strings.Contains(out, `"code":"not_found"`) {
			t.Errorf("%v = exit %d:\n%s\nwant 2 not_found", args, code, out)
		}
	}
	if code, out := storeRun(t, f, "cp", "-r", "toft://reports/keep.txt", "toft://reports/copy.txt"); code != exitOK {
		t.Errorf("cp -r of an object = %d:\n%s", code, out)
	}
	if got := strings.Join(f.keys(), " "); !strings.Contains(got, "reports/copy.txt") {
		t.Errorf("cp -r of an object copied nothing: %s", got)
	}
}

// unmarshalNDJSON reads a --json list — one document per line — into the
// slice v points at.
func unmarshalNDJSON(b []byte, v any) error {
	var docs []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			docs = append(docs, line)
		}
	}
	return json.Unmarshal([]byte("["+strings.Join(docs, ",")+"]"), v)
}

// BB-042: every refusal this client makes carries a hint under --json — its
// own, the line for its code, the step its sentence ends on, or its --help.
func TestUsageRefusalsCarryAHint(t *testing.T) {
	bb3Env(t)
	t.Setenv("TOFT_APP", "")
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		args []string
		code string
		hint string
	}{
		{[]string{"status", "--nope", "--json"}, "unknown_flag", "toft status --help"},
		{[]string{"status", "--json"}, "no_app", "--app <slug>"},
		{[]string{"create", "--app", "Bad_Slug", "--json"}, "invalid_slug", `"bad-slug"`},
		{[]string{"logs", "--since", "yesterday", "--json"}, "invalid_duration", "2h"},
		{[]string{"env", "list", "a", "b", "--json"}, "unexpected_argument", "typo"},
		{[]string{"logs", "--lines", "x", "--json"}, "", "toft logs --help"},
	} {
		stdout, _ := captureStd(t)
		run(tc.args)
		doc := lastDoc(t, stdout())
		if tc.code != "" && doc["code"] != tc.code {
			t.Errorf("%v: code %v, want %s (%v)", tc.args, doc["code"], tc.code, doc)
		}
		if h, _ := doc["hint"].(string); !strings.Contains(h, tc.hint) {
			t.Errorf("%v: hint %q, want it to name %s", tc.args, h, tc.hint)
		}
	}
}

// BB-036: fetch -o never replaces a file without --force, refuses a directory
// before asking the app (and leaves no temp file), and -o - is stdout.
func TestFetchOutputRules(t *testing.T) {
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "BODY")
	}))
	dir := t.TempDir()
	file := filepath.Join(dir, "out.bin")
	os.WriteFile(file, []byte("old"), 0o644)

	if err := cmdFetch(e, []string{"/", "--app", "demo", "-o", file}); refusalCode(err) != "file_exists" {
		t.Errorf("onto an existing file: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != "old" {
		t.Errorf("the file was replaced without --force: %q", b)
	}
	if err := cmdFetch(e, []string{"/", "--app", "demo", "-o", file, "--force"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "BODY" {
		t.Errorf("--force did not replace it: %q", b)
	}

	sub := filepath.Join(dir, "d")
	os.Mkdir(sub, 0o755)
	err := cmdFetch(e, []string{"/", "--app", "demo", "-o", sub})
	if refusalCode(err) != "is_a_directory" || strings.Contains(err.Error(), ".d.") {
		t.Errorf("onto a directory: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("something was left behind: %v", entries)
	}

	stdout.Reset()
	if err := cmdFetch(e, []string{"/", "--app", "demo", "-o", "-"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "BODY" {
		t.Errorf("-o - stdout = %q", got)
	}
	if _, err := os.Stat("-"); err == nil {
		os.Remove("-")
		t.Error("-o - wrote a file named -")
	}
	if err := cmdFetch(e, []string{"/", "--app", "demo", "-o", "-", "--json"}); refusalCode(err) != "conflicting_flags" {
		t.Errorf("-o - --json: %v", err)
	}
}

// BB-040: a run page that is not there is said once, by the refusal — not
// first by the "answered in the app's place" line.
func TestFetchOfAMissingRunPageIsSaidOnce(t *testing.T) {
	e, stdout, stderr := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Toft-Interstitial", "run_page")
		http.Error(w, "no such run", http.StatusNotFound)
	}))
	err := cmdFetch(e, []string{"/__toft/runs/00000000-0000-0000-0000-000000000000", "--app", "demo"})
	if refusalCodeOf(err) != "run_page" {
		t.Errorf("err = %v, want run_page", err)
	}
	if out := stdout.String() + stderr.String(); strings.Contains(out, "in the app's place") {
		t.Errorf("the old line was printed before the refusal:\n%s", out)
	}
}

// BB-041: start of a running app restarts nothing and says already_running;
// restart still bounces it.
func TestStartOfARunningAppIsANoOp(t *testing.T) {
	posted := 0
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			posted++
			fmt.Fprint(w, `{"status":"restarted","action":"restart"}`)
			return
		}
		fmt.Fprint(w, appRow("running"))
	})
	if err := cmdStart(e, []string{"--app", "demo", "--wait", "--json"}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil || doc["outcome"] != "already_running" || doc["status"] != "already_running" || doc["settled"] != "running" {
		t.Errorf("start --json = %s (%v)", buf, err)
	}
	if posted != 0 {
		t.Errorf("start of a running app posted /restart %d time(s)", posted)
	}
	buf.Reset()
	if err := cmdRestart(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if posted != 1 {
		t.Errorf("restart did not restart")
	}
}

// BB-045: --for with an offset is sent as typed — the zone, and any fraction,
// are the install's to keep.
func TestJobRunForIsSentAsTyped(t *testing.T) {
	for typed, want := range map[string]string{
		"2026-11-03T09:00:00-05:00":     "2026-11-03T09:00:00-05:00",
		"2026-11-03T09:00:00.250-05:00": "2026-11-03T09:00:00.250-05:00",
		"2026-11-03T09:00":              "2026-11-03T09:00:00Z",
	} {
		opts := &jobFakeOpts{runStatus: "succeeded"}
		e, _ := fakeInstall(t, jobFake(t, opts))
		if err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--for", typed}); err != nil {
			t.Fatal(err)
		}
		if opts.startBody["scheduled_for"] != want {
			t.Errorf("--for %s sent %#v, want %s", typed, opts.startBody["scheduled_for"], want)
		}
	}
}

// BB-033: a credential no install issues is refused here, exit 3 invalid_key,
// before anything is sent; and an install's 401 for one is exit 3 as well.
func TestAGarbageTokenIsExit3(t *testing.T) {
	bb3Env(t)
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid API key","code":"invalid_key","hint":"mint a new key"}`)
	}))
	defer srv.Close()
	t.Setenv("TOFT_URL", srv.URL)

	for _, token := range []string{"garbage", "abc.def"} {
		t.Setenv("TOFT_TOKEN", token)
		stdout, _ := captureStd(t)
		if got := run([]string{"apps", "--json"}); got != exitAuth {
			t.Errorf("TOFT_TOKEN=%s: exit %d, want 3", token, got)
		}
		if doc := lastDoc(t, stdout()); doc["code"] != "invalid_key" || doc["hint"] == nil {
			t.Errorf("TOFT_TOKEN=%s: %v", token, doc)
		}
	}
	if sent != 0 {
		t.Errorf("a garbage token was sent %d time(s)", sent)
	}
	t.Setenv("TOFT_TOKEN", "toftu_x")
	stdout, _ := captureStd(t)
	if got := run([]string{"apps", "--json"}); got != exitAuth {
		t.Errorf("the install's 401: exit %d, want 3", got)
	}
	if doc := lastDoc(t, stdout()); doc["code"] != "invalid_key" {
		t.Errorf("the install's 401: %v", doc)
	}
}

// BB-038: a failed deploy's result message is text — the app's colour codes,
// quoted from its crash, are stripped.
func TestDeployResultMessageHasNoEscapes(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"failed\","+
				"\"phase\":\"deploy\",\"step\":\"start\",\"reason\":\"start_failed\","+
				"\"message\":\"exited: \\u001b[31mValueError\\u001b[0m boom\"}\n\n")
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
	if result["message"] != "exited: ValueError boom" {
		t.Errorf("message = %q", result["message"])
	}
}

// BB-037: a deploy's --json result passes the install's warning_details
// through, each with its code.
func TestDeployResultCarriesWarningDetails(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"success\","+
				"\"warnings\":[\"go 1.99 was fetched\"],"+
				"\"warning_details\":[{\"code\":\"go_toolchain_fetched\",\"message\":\"go 1.99 was fetched\"}]}\n\n")
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
	var result struct {
		WarningDetails []struct{ Code string } `json:"warning_details"`
	}
	json.Unmarshal([]byte(lines[len(lines)-1]), &result)
	if len(result.WarningDetails) != 1 || result.WarningDetails[0].Code != "go_toolchain_fetched" {
		t.Errorf("result = %s", lines[len(lines)-1])
	}
}
