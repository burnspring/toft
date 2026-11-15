package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

// The third 2026-11-03 black-box list, the CLI's half (TOFT-BB-001 …).

func bb3Env(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("TOFT_URL", "http://127.0.0.1:1")
	t.Setenv("TOFT_TOKEN", "toftu_test")
	t.Setenv("CI", "1")
}

// BB-001: global flags before a command with subcommands land after the
// subcommand, and a leading switch given true/false is bad_flag.
func TestGlobalFlagsBeforeAGroupedCommand(t *testing.T) {
	bb3Env(t)
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"--json", "job", "wait"}, "missing_argument"},
		{[]string{"--json", "--app", "demo", "store", "mv", "toft://drop/a.csv"}, "missing_argument"},
		{[]string{"--json", "-q", "env", "nope"}, "unknown_command"},
		{[]string{"--json", "--plain", "false", "apps"}, "bad_flag"},
	} {
		stdout, _ := captureStd(t)
		if got := run(tc.args); got != exitUsage {
			t.Errorf("run(%v) = %d, want %d", tc.args, got, exitUsage)
			continue
		}
		if doc := lastDoc(t, stdout()); doc["code"] != tc.code {
			t.Errorf("run(%v) document %v, want code %s", tc.args, doc, tc.code)
		}
	}
}

// Every grouped command refuses an unknown subcommand as one — so the list
// main.go moves leading flags past is the list of commands that have them.
func TestGroupedCommandsAreListed(t *testing.T) {
	bb3Env(t)
	for name := range groupedCommands {
		if _, ok := commandTable()[name]; !ok {
			t.Errorf("%s is listed as grouped but is no command", name)
		}
		stdout, _ := captureStd(t)
		run([]string{name, "zz-not-a-subcommand", "--json"})
		if doc := lastDoc(t, stdout()); doc["code"] != "unknown_command" {
			t.Errorf("%s zz: %v, want unknown_command", name, doc)
		}
	}
}

// BB-006: a slug somebody named that is not one is refused, not rewritten;
// a folder's name is still made into one.
func TestAStatedNonSlugIsRefused(t *testing.T) {
	if refusalCode(refuseStatedNonSlug("Bad_Slug", "--app", true)) != "invalid_slug" {
		t.Error("--app Bad_Slug was not refused invalid_slug")
	}
	if err := refuseStatedNonSlug("good-slug", "--app", true); err != nil {
		t.Errorf("a valid slug refused: %v", err)
	}
	if err := refuseStatedNonSlug("My_Folder", "this directory's name", false); err != nil {
		t.Errorf("a derived slug refused: %v", err)
	}
}

// BB-014: a run page that answered is printed like any page, and the app's own
// 404 at /__toft/runs/ is the app's, not the platform's.
func TestFetchOfARunPageAndOfTheBareRunsPath(t *testing.T) {
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/__toft/runs/") {
			http.Error(w, "app 404", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Toft-Interstitial", "run_page")
		fmt.Fprint(w, `{"id":"r1","status":"succeeded"}`)
	}))
	if err := cmdFetch(e, []string{"/__toft/runs/r1", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if out := stdout.String(); !strings.Contains(out, `"succeeded"`) || strings.Contains(out, "in the app's place") {
		t.Errorf("run page output %q", out)
	}
	err := cmdFetch(e, []string{"/__toft/runs/", "--app", "demo"})
	if refusalCodeOf(err) != "unexpected_status" {
		t.Errorf("bare runs path: %v", err)
	}
}

// BB-015: a run's log under --json is one document per line.
func TestJobLogsJSONIsNDJSON(t *testing.T) {
	e, buf := testEnv()
	for _, l := range []jobLogLine{{Seq: 1, Line: "a"}, {Seq: 2, Line: "b"}} {
		if err := writeJobLogLine(e, true, l); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "{") || !strings.HasPrefix(lines[1], "{") {
		t.Errorf("not NDJSON: %q", buf.String())
	}
}

// BB-016: --forget-all under --json is a document on stdout.
func TestForgetAllUnderJSONIsADocument(t *testing.T) {
	bb3Env(t)
	stdout, _ := captureStd(t)
	if got := run([]string{"login", "--forget-all", "--json"}); got != exitOK {
		t.Fatalf("exit %d", got)
	}
	if doc := lastDoc(t, stdout()); doc["forgotten"] == nil {
		t.Errorf("no forgotten list: %v", doc)
	}
}

// BB-016: a signal during a read of stdin is a refusal coded interrupted, with
// a document under --json — not exit 1 with nothing said.
func TestAStdinReadInterruptedIsCoded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, w := io.Pipe()
	defer w.Close()
	_, err := readAllCtx(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v is not context.Canceled to callers", err)
	}
	e, buf := testEnv()
	e.json = true
	if got := report(e, err); got != exitFailed {
		t.Errorf("exit %d", got)
	}
	if doc := lastDoc(t, buf.String()); doc["code"] != "interrupted" || doc["hint"] == nil {
		t.Errorf("doc %v", doc)
	}
}

// BB-021: the platform's answer and a job's ending carry their next step as
// `hint`.
func TestPlatformAndJobEndingRefusalsCarryAHint(t *testing.T) {
	for _, err := range []error{
		platformAnswered(&fetchResult{Status: 503, Platform: "stopped"}, "demo", ""),
		platformAnswered(&fetchResult{Status: 401, Platform: "sign_in_required"}, "demo", ""),
		platformAnswered(&fetchResult{Status: 503, Platform: "batch"}, "demo", ""),
		jobRunOutcome(&jobRun{ID: "abcdef0123", Job: "nightly", Status: jobRunFailed}),
		jobRunOutcome(&jobRun{ID: "abcdef0123", Job: "nightly", Status: jobRunOutOfMemory}),
		jobRunOutcome(&jobRun{ID: "abcdef0123", Job: "nightly", Status: jobRunTimedOut}),
		jobRunOutcome(&jobRun{ID: "abcdef0123", Job: "nightly", Status: jobRunCancelled}),
	} {
		e, buf := testEnv()
		e.json = true
		report(e, err)
		doc := lastDoc(t, buf.String())
		if h, _ := doc["hint"].(string); !strings.Contains(h, "toft ") {
			t.Errorf("%v: hint %v", err, doc["hint"])
		}
	}
}

// BB-016: a watched create's result names a secret-looking file in the bundle,
// as a deploy's does.
func TestCreateJSONWarnsOfASecretInTheBundle(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/apps/upload") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports","status":"building"},"deployment":{"id":"dep-1"}}`)
		case strings.HasSuffix(p, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"dep-1\",\"status\":\"success\"}\n\n")
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("print(1)\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "test.pem"), []byte("-----BEGIN-----\n"), 0o644)
	e.dir = dir
	e.json = true
	_ = cmdCreate(e, []string{"--app", "reports", "--yes", "--json"})
	if !strings.Contains(buf.String(), "test.pem") || !strings.Contains(buf.String(), `"warnings"`) {
		t.Errorf("no warning naming test.pem:\n%s", buf.String())
	}
}

// BB-016: under --json a log line's `line` is the text alone, and `raw` the
// line as recorded when it held escapes.
func TestJSONLogLinesCarryTheTextWithoutEscapes(t *testing.T) {
	text, raw := jsonLogText("\x1b[32mready\x1b[0m")
	if text != "ready" || raw != "\x1b[32mready\x1b[0m" {
		t.Errorf("text %q raw %q", text, raw)
	}
	if text, raw := jsonLogText("plain"); text != "plain" || raw != "" {
		t.Errorf("a plain line changed: %q %q", text, raw)
	}
	e, buf := testEnv()
	writeJobLogLine(e, true, jobLogLine{Seq: 1, Line: "\x1b[1mbold\x1b[0m"})
	if !strings.Contains(buf.String(), `"line":"bold"`) {
		t.Errorf("job log line %s", buf.String())
	}
}

// BB-016: a refusal at the upload that names the deployment it recorded keeps
// that id in the refusal document.
func TestAnUploadRefusalKeepsItsDeploymentID(t *testing.T) {
	e, buf := testEnv()
	e.json = true
	report(e, &client.Error{Status: 422, Code: "manifest_invalid", Message: "bad", Reason: "manifest_invalid", DeploymentID: "dep-9"})
	if doc := lastDoc(t, buf.String()); doc["deployment_id"] != "dep-9" {
		t.Errorf("doc %v", doc)
	}
}

// BB-017: a duration is shown as declared, not rounded down to the minute.
func TestShortDurationKeepsEveryUnit(t *testing.T) {
	for sec, want := range map[int]string{90: "1m30s", 45: "45s", 2700: "45m", 3600: "1h", 5400: "1h30m", 3725: "1h2m5s"} {
		if got := durationText(sec); got != want {
			t.Errorf("%ds: %q, want %q", sec, got, want)
		}
	}
}

// BB-018: a remote folder is is_a_folder even when the local destination
// exists.
func TestStoreGetOfAFolderOverALocalFile(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/dir/x.txt", "x")
	local := filepath.Join(t.TempDir(), "dir")
	os.WriteFile(local, []byte("mine"), 0o644)
	code, out := storeRun(t, f, "get", "reports/dir", "-o", local, "--json")
	if doc := lastDoc(t, out); code != exitUsage || doc["code"] != "is_a_folder" {
		t.Errorf("exit %d doc %v", code, doc)
	}
}

// BB-019: cp and mv take <store>/<path> like every other subcommand, when no
// local file has the store's name.
func TestStoreMoveTakesStoreSlashPath(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/a.txt", "a")
	if code, out := storeRun(t, f, "mv", "reports/a.txt", "reports/b.txt", "--json"); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	keys := strings.Join(f.keys(), ",")
	if !strings.Contains(keys, "reports/b.txt") || strings.Contains(keys, "reports/a.txt") {
		t.Errorf("objects after mv: %s", keys)
	}
	if code, out := storeRun(t, f, "cp", "nostore/a", "elsewhere/b", "--json"); code != exitUsage || lastDoc(t, out)["code"] != "no_store" {
		t.Errorf("two non-stores: exit %d %s", code, out)
	}
	if storeSide("./reports/a.txt", map[string]bool{"reports": true}) {
		t.Error("./reports was read as a store")
	}
}

// BB-020: a typed slug's refusal names no file it was read from, and a URL
// with another scheme is not offered back with https:// in front of it.
func TestWordingOfTypedSlugAndForeignScheme(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "toft.toml"), []byte("[app]\nslug = \"demo\"\n"), 0o644)
	r, err := config.NewResolver(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &session{Session: &config.Session{}, resolver: r, slugTyped: true}
	if msg := (&session{Session: &config.Session{}, resolver: r}).noSuchApp("demo").Error(); !strings.Contains(msg, "read from") {
		t.Fatalf("the control case names no origin: %s", msg)
	}
	if msg := s.noSuchApp("demo").Error(); strings.Contains(msg, "read from") {
		t.Errorf("typed slug: %s", msg)
	}

	bb3Env(t)
	stdout, _ := captureStd(t)
	run([]string{"login", "--token-stdin", "ftp://x", "--json"})
	doc := lastDoc(t, stdout())
	if msg, _ := doc["error"].(string); strings.Contains(msg, "https://ftp://") || doc["code"] != "invalid_url" {
		t.Errorf("doc %v", doc)
	}
}
