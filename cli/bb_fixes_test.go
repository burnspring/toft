package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"toft/cli/internal/config"
)

// lastDoc is the last JSON line written, decoded.
func lastDoc(t *testing.T, out string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var doc map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil {
		t.Fatalf("the last line is not a JSON document: %q", out)
	}
	return doc
}

// BB-039: an unknown command under --json is a document, wherever --json is.
func TestAnUnknownCommandUnderJSONIsADocument(t *testing.T) {
	for _, args := range [][]string{{"bogus", "--json"}, {"--json", "bogus"}} {
		stdout, _ := captureStd(t)
		if got := run(args); got != exitUsage {
			t.Fatalf("run(%v) = %d, want %d", args, got, exitUsage)
		}
		doc := lastDoc(t, stdout())
		if doc["code"] != "unknown_command" || doc["kind"] != "usage" {
			t.Errorf("run(%v) document %v", args, doc)
		}
	}
}

// BB-039: global flags before the command are the command's flags.
func TestGlobalFlagsMayComeBeforeTheCommand(t *testing.T) {
	stdout, _ := captureStd(t)
	if got := run([]string{"--json", "--app", "demo", "status", "--nope"}); got != exitUsage {
		t.Fatalf("exit %d, want %d", got, exitUsage)
	}
	doc := lastDoc(t, stdout())
	if doc["code"] != "unknown_flag" {
		t.Errorf("want the status command's own flag refusal, got %v", doc)
	}

	leading, rest := leadingGlobalFlags([]string{"--json", "--install=x", "-q", "--app", "a", "deploy", "--yes"})
	if strings.Join(leading, " ") != "--json --install=x -q --app a" || strings.Join(rest, " ") != "deploy --yes" {
		t.Errorf("leading %q rest %q", leading, rest)
	}
	if _, rest := leadingGlobalFlags([]string{"--bogus", "apps"}); rest[0] != "--bogus" {
		t.Errorf("an unknown leading flag was swallowed: %q", rest)
	}
}

// BB-037: every flag refusal carries a code.
func TestFlagRefusalsCarryACode(t *testing.T) {
	fs := newFlags("x", "")
	fs.Bool("enabled", false, "")
	fs.Int("n", 0, "")
	for args, code := range map[string]string{
		"--nope":          "unknown_flag",
		"--n=abc":         "bad_flag",
		"--enabled false": "bad_flag",
		"extra":           "unexpected_argument",
	} {
		err := parse(fs, strings.Fields(args))
		if got := refusalCode(err); got != code {
			t.Errorf("%s: code %q, want %q (%v)", args, got, code, err)
		}
	}
}

// BB-017: a folder named where an object is wanted is is_a_folder in every
// verb, slash or no slash; a missing source is not_found exit 2, not "1 of 1".
func TestStoreFolderAndMissingSourceRefusalsAreCoded(t *testing.T) {
	t.Setenv("CI", "1")
	f := newFakeStoreInstall(t)
	f.put("reports/dir/x.txt", "x")
	dst := t.TempDir()
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"get", "reports/dir", "-o", dst + "/", "--json"}, "is_a_folder"},
		{[]string{"get", "reports/dir/", "--json"}, "is_a_folder"},
		{[]string{"cp", "toft://reports/dir", dst, "--json"}, "is_a_folder"},
		{[]string{"cp", "toft://reports/dir/", dst, "--json"}, "is_a_folder"},
		{[]string{"cp", dst, "toft://reports/", "--json"}, "is_a_folder"},
		{[]string{"cp", "toft://reports/nothere.txt", dst, "--json"}, "not_found"},
		{[]string{"mv", "toft://reports/nothere.txt", "toft://reports/b.txt", "--json"}, "not_found"},
		{[]string{"get", "reports/nothere.txt", "-o", dst + "/", "--json"}, "not_found"},
	} {
		code, out := storeRun(t, f, tc.args...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d:\n%s", tc.args, code, exitUsage, out)
			continue
		}
		if doc := lastDoc(t, out); doc["code"] != tc.code {
			t.Errorf("%v: code %v, want %s", tc.args, doc["code"], tc.code)
		}
	}
}

// BB-017: rm -r deletes every object before any marker, and markers deepest
// first — a host-volume marker is a directory that stays if it is not empty.
func TestRemoveTreeDeletesMarkersLastAndDeepestFirst(t *testing.T) {
	f := newFakeStoreInstall(t)
	for _, k := range []string{"reports/td/a/", "reports/td/a/x", "reports/td/a/b/",
		"reports/td/a/b/y", "reports/td/z", "reports/td/e/"} {
		f.put(k, "")
	}
	if code, out := storeRun(t, f, "rm", "-r", "--yes", "-j", "4", "reports/td/"); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var deletes []string
	for _, m := range f.mints {
		if strings.HasPrefix(m, "DELETE ") {
			deletes = append(deletes, strings.TrimPrefix(m, "DELETE reports/td/"))
		}
	}
	seenMarker := false
	pos := map[string]int{}
	for i, d := range deletes {
		pos[d] = i
		if d == "" || strings.HasSuffix(d, "/") {
			seenMarker = true
		} else if seenMarker {
			t.Errorf("object %s deleted after a marker: %v", d, deletes)
		}
	}
	if pos["a/b/"] > pos["a/"] {
		t.Errorf("a/ before a/b/: %v", deletes)
	}
	if left := f.keys(); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// BB-017: cp -r downloads no entry for a folder marker that has something in
// it (the host volume lists every directory as one); an empty one is still a
// local directory.
func TestCopyDownSkipsMarkersOfFoldersThatHoldSomething(t *testing.T) {
	f := newFakeStoreInstall(t)
	for _, k := range []string{"reports/t/a/", "reports/t/a/x.txt", "reports/t/empty/"} {
		f.put(k, "")
	}
	ops, _, err := planCopy(f.session(), "toft://reports/t/", t.TempDir(), true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, o := range ops {
		kinds = append(kinds, o.src)
	}
	if len(ops) != 2 || strings.Join(kinds, ",") != "t/a/x.txt,t/empty/" {
		t.Errorf("ops %v", kinds)
	}
	if err := copyTree(t, f, "toft://reports/t/", "toft://reports/u/", true, false, true); err != nil {
		t.Fatal(err)
	}
	for _, k := range f.keys() {
		if strings.HasPrefix(k, "reports/t/") {
			t.Errorf("a move left %s behind", k)
		}
	}
}

// BB-037: refusals that had no code under --json.
func TestStoreAndVisibilityRefusalsCarryACode(t *testing.T) {
	f := newFakeStoreInstall(t)
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"ls", "toft://", "--json"}, "no_store"},
		{[]string{"get", "reports/a.txt", "-o", "-", "--json"}, "conflicting_flags"},
	} {
		code, out := storeRun(t, f, tc.args...)
		if doc := lastDoc(t, out); code != exitUsage || doc["code"] != tc.code {
			t.Errorf("%v: exit %d doc %v, want code %s", tc.args, code, doc, tc.code)
		}
	}
	if got := refusalCode(cmdVisibility(treeEnv(), []string{"bogus", "--app", "demo"})); got != "invalid_visibility" {
		t.Errorf("visibility bogus: code %q", got)
	}
}

// BB-040: redirects and final_url are always in the document.
func TestFetchJSONAlwaysCarriesRedirectsAndFinalURL(t *testing.T) {
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	if err := cmdFetch(e, []string{"/healthz", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	doc := oneDocument(t, stdout)
	hops, ok := doc["redirects"].([]any)
	if !ok || len(hops) != 0 || doc["final_url"] != doc["url"] {
		t.Errorf("doc = %v", doc)
	}
}

// BB-049: the gateway's 503 for a batch app is the platform's answer, coded,
// not "the app answered 503".
func TestFetchOfABatchAppIsThePlatformsAnswer(t *testing.T) {
	e, _, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Toft-Interstitial", "batch")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "<html></html>")
	}))
	err := cmdFetch(e, []string{"/", "--app", "demo", "--json"})
	if strings.Contains(fmt.Sprint(err), "the app answered") || refusalCodeOf(err) != "batch" {
		t.Fatalf("err = %v", err)
	}
	var buf bytes.Buffer
	e.out.Stdout = &buf
	e.json = true
	if got := report(e, err); got != exitFailed {
		t.Errorf("exit %d", got)
	}
	if doc := lastDoc(t, buf.String()); doc["code"] != "batch" || doc["retryable"] != false {
		t.Errorf("doc = %v", doc)
	}
}

func refusalCodeOf(err error) string {
	var f *failedError
	if errors.As(err, &f) {
		return f.code
	}
	return ""
}

// BB-037: an app that does not answer within --timeout is code timeout,
// retryable.
func TestFetchTimeoutIsCodedAndRetryable(t *testing.T) {
	e, _, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	err := cmdFetch(e, []string{"/", "--app", "demo", "--timeout", "50ms"})
	var f *failedError
	if !errors.As(err, &f) || f.code != "timeout" || !f.retryable {
		t.Fatalf("err = %v", err)
	}
}

// BB-037: --session with no session stored, and the other sign-in refusals,
// carry a code.
func TestSignInRefusalsCarryACode(t *testing.T) {
	for err, code := range map[error]string{
		&config.NoAdminSessionError{Install: "https://t"}:                      "no_session",
		&config.NoAdminSessionError{Install: "https://t", Expired: time.Now()}: "session_expired",
		&config.NotSignedInError{Install: "https://t"}:                         "not_signed_in",
		config.ErrNoInstall: "no_install",
	} {
		e, buf := testEnv()
		e.json = true
		if got := report(e, err); got != exitAuth {
			t.Errorf("%v: exit %d", err, got)
		}
		if doc := lastDoc(t, buf.String()); doc["code"] != code {
			t.Errorf("%v: doc %v, want code %s", err, doc, code)
		}
	}
}

// BB-037: a job wait that runs out is wait_timeout, retryable, as the SDKs
// name it.
func TestJobWaitTimeoutIsCodedAndRetryable(t *testing.T) {
	e, _ := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "running"}))
	err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--timeout", "20ms"})
	var f *failedError
	if !errors.As(err, &f) || f.code != "wait_timeout" || !f.retryable {
		t.Fatalf("err = %v", err)
	}
}

// BB-043: job list --json splits the zone off the schedule and always
// carries the overdue window.
func TestJobListJSONSplitsTheScheduleAndCarriesExpectWithin(t *testing.T) {
	week := 7 * 86400
	var buf bytes.Buffer
	e, _ := testEnv()
	e.out.Stdout = &buf
	if err := encodeJobs(e, []jobDefinition{
		{Name: "nightly", Schedule: "CRON_TZ=Europe/London 0 3 * * *", ExpectWithinSec: &week},
		{Name: "plain", Schedule: "0 4 * * *"},
		{Name: "manual"},
	}); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := unmarshalNDJSON(buf.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0]["schedule"] != "0 3 * * *" || rows[0]["timezone"] != "Europe/London" ||
		rows[0]["expect_within_sec"] != float64(week) || rows[0]["expect_within"] != "7d" {
		t.Errorf("nightly = %v", rows[0])
	}
	if rows[1]["timezone"] != "UTC" || rows[1]["schedule"] != "0 4 * * *" {
		t.Errorf("plain = %v", rows[1])
	}
	if v, ok := rows[2]["expect_within_sec"]; !ok || v != nil {
		t.Errorf("manual = %v", rows[2])
	}
	if _, ok := rows[2]["timezone"]; ok {
		t.Errorf("an unscheduled job has a zone: %v", rows[2])
	}
}

// BB-046: events --kind and --severity are refused when they name nothing,
// rather than answering "nothing has gone wrong".
func TestEventsRefusesAnUnknownKindOrSeverity(t *testing.T) {
	for args, code := range map[string]string{
		"--kind bogus":     "invalid_kind",
		"--severity bogus": "invalid_severity",
	} {
		err := cmdEvents(treeEnv(), strings.Fields(args))
		if refusalCode(err) != code {
			t.Errorf("%s: %v", args, err)
		}
		if !strings.Contains(fmt.Sprint(err), "deploy_failed") && code == "invalid_kind" {
			t.Errorf("%s does not name the kinds: %v", args, err)
		}
	}
}

// The list is the install's: every Kind constant of internal/events, read
// from the source beside this module when it is there.
func TestEventKindsAreTheInstalls(t *testing.T) {
	src, err := os.ReadFile("../internal/events/events.go")
	if err != nil {
		t.Skip("the server's source is not beside this module")
	}
	known := map[string]bool{}
	for _, k := range eventKinds {
		known[k] = true
	}
	for _, m := range regexp.MustCompile(`Kind = "([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		if !known[m[1]] {
			t.Errorf("the install records kind %q and toft events would refuse it", m[1])
		}
	}
}

// BB-047: NODE_ENV=production is set, and warned about — it is a build trap.
func TestEnvSetWarnsAboutNodeEnvProduction(t *testing.T) {
	got := runEnv(t, "node", `{"set":["NODE_ENV"]}`, "set", "NODE_ENV=production")
	if !strings.Contains(got, "devDependencies") || !strings.Contains(got, "NODE_ENV set") {
		t.Errorf("no warning, or no set: %q", got)
	}
	if got := runEnv(t, "node", `{"set":["NODE_ENV"]}`, "set", "NODE_ENV=production", "--json"); !strings.Contains(got, `"warnings"`) {
		t.Errorf("--json carries no warning: %q", got)
	}
	if got := runEnv(t, "node", `{"set":["A"]}`, "set", "A=production"); strings.Contains(got, "devDependencies") {
		t.Errorf("warned about another variable: %q", got)
	}
}

// BB-050: sdk vendor --lang node says what is true of package.json.
func TestNodeShipsLineSaysWhatPackageJSONSays(t *testing.T) {
	dir := t.TempDir()
	if got := nodeShipsLine(dir, ".toft/sdk-node"); !strings.Contains(got, "no package.json") {
		t.Errorf("no package.json: %q", got)
	}
	os.WriteFile(dir+"/package.json", []byte(`{"dependencies":{}}`), 0o644)
	if got := nodeShipsLine(dir, ".toft/sdk-node"); !strings.Contains(got, "does not name") {
		t.Errorf("unnamed: %q", got)
	}
	os.WriteFile(dir+"/package.json", []byte(`{"dependencies":{"toft-sdk":"file:.toft/sdk-node"}}`), 0o644)
	if got := nodeShipsLine(dir, ".toft/sdk-node"); !strings.Contains(got, "because package.json names it") {
		t.Errorf("named: %q", got)
	}
}

// BB-045: a failed --verify on a brand-new app's first deploy names no
// rollback — there is nothing earlier to roll back to.
func TestCreateVerifyFailureSuggestsNoRollback(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":"","apps_path_prefix":"/apps"}`)
		case p == "/api/v1/apps" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"reports","status":"building"},"deployment":{"id":"dep-1"}}`)
		case strings.HasSuffix(p, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"dep-1\",\"status\":\"success\",\"phase\":\"done\"}\n\n")
		case strings.HasPrefix(p, "/apps/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	e.dir = t.TempDir()
	e.json = true
	err := cmdCreate(e, []string{"--repo", "https://github.com/corp/reports", "--yes", "--json", "--verify", "/x"})
	if err == nil || strings.Contains(err.Error(), "rollback") || !strings.Contains(err.Error(), "first deployment") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(buf.String(), "toft rollback") || !strings.Contains(buf.String(), `"next":"toft logs --app reports"`) {
		t.Errorf("the result names a rollback:\n%s", buf.String())
	}
	if firstDeploys["reports"] {
		t.Error("create left its mark behind")
	}
	if got := verifyNext("reports"); !strings.Contains(got, "rollback") {
		t.Errorf("a later deploy lost the rollback: %q", got)
	}
}

// BB-6 (2026-11-04): an install that answers a batch app's address with its
// jobs card as a 200 is a fetch that worked — exit 0, the batch line, no refusal.
func TestFetchOfABatchAppsCardAt200Succeeds(t *testing.T) {
	e, _, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Toft-Interstitial", "batch")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html></html>")
	}))
	if err := cmdFetch(e, []string{"/", "--app", "demo"}); err != nil {
		t.Fatalf("err = %v", err)
	}
}
