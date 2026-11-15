package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"toft/cli/internal/bundle"
	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

// The 2026-11-04 black-box list, second round (BB-004 … BB-028, D-05, D-18).

// jsonLines are the lines of out that are JSON objects — the documents, with
// stderr's sentences (the same buffer in these tests) left out.
func jsonLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		docs = append(docs, doc)
	}
	return docs
}

// BB-004, BB-016: rm -r backs off a busy store as cp does, and under --json
// prints one document per item; what is never admitted is coded store_busy.
func TestRemoveTreeIsPacedAndItemised(t *testing.T) {
	defer func(n int, d time.Duration) { busyRetries, busyBackoff = n, d }(busyRetries, busyBackoff)
	busyBackoff = time.Millisecond

	var refusals atomic.Int32
	f := newFakeStoreInstall(t)
	inner := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/bucket/") && r.Method == http.MethodDelete && refusals.Add(-1) >= 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "too many transfers at once")
			return
		}
		inner.ServeHTTP(w, r)
	})
	for i := 0; i < 6; i++ {
		f.put(fmt.Sprintf("reports/td/f%d.txt", i), "ab")
	}
	refusals.Store(4)
	code, out := storeRun(t, f, "rm", "-r", "--yes", "-j", "8", "reports/td/", "--json")
	if code != exitOK {
		t.Fatalf("rm -r with four 429s = %d:\n%s", code, out)
	}
	if len(f.keys()) != 0 {
		t.Errorf("left %v", f.keys())
	}
	docs := jsonLines(t, out)
	if len(docs) != 6 {
		t.Fatalf("want one document per object, got %d:\n%s", len(docs), out)
	}
	for _, d := range docs {
		if d["deleted"] != true || !strings.HasPrefix(fmt.Sprint(d["object"]), "toft://reports/td/") || d["bytes"] != float64(2) {
			t.Errorf("item = %v", d)
		}
	}

	// Never admitted: every item carries store_busy, and the batch is that.
	busyRetries = 1
	for i := 0; i < 3; i++ {
		f.put(fmt.Sprintf("reports/again/f%d.txt", i), "ab")
	}
	refusals.Store(1000)
	code, out = storeRun(t, f, "rm", "-r", "--yes", "reports/again/", "--json")
	if code != exitFailed || !strings.Contains(out, `"retryable":true`) {
		t.Errorf("rm -r never admitted = %d:\n%s", code, out)
	}
	busy := 0
	for _, d := range jsonLines(t, out) {
		if d["code"] == "store_busy" && d["deleted"] == false {
			busy++
		}
	}
	if busy < 3 {
		t.Errorf("want each item coded store_busy:\n%s", out)
	}
}

// BB-015: a level too low for the verb is insufficient_access with a hint, as
// mv's own up-front refusal is — never a bare `forbidden`.
func TestALevelTooLowIsInsufficientAccess(t *testing.T) {
	for _, body := range []string{
		`{"error":"your access here is \"read\", and this needs \"write\"","code":"forbidden"}`,
		`{"error":"your access here is \"write\", which does not permit this","code":"forbidden","hint":"ask an administrator for a higher level on this store"}`,
	} {
		var apiErr client.Error
		json.Unmarshal([]byte(body), &apiErr)
		apiErr.Status = http.StatusForbidden
		var raw struct {
			Error, Code, Hint string
		}
		json.Unmarshal([]byte(body), &raw)
		apiErr.Message, apiErr.Code, apiErr.Hint = raw.Error, raw.Code, raw.Hint
		err := storeError(&apiErr, "reports")
		e, buf := testEnv()
		e.json = true
		if got := report(e, err); got != exitAuth {
			t.Errorf("exit %d", got)
		}
		doc := lastDoc(t, buf.String())
		if doc["code"] != "insufficient_access" || doc["kind"] != "refused" || fmt.Sprint(doc["hint"]) == "" || doc["hint"] == nil {
			t.Errorf("doc = %v", doc)
		}
	}
	// An outside-the-folders refusal keeps its own code.
	if err := storeError(&client.Error{Status: 403, Code: "outside_mapped_folders", Message: "x"}, "reports"); refusalCode(err) != "outside_mapped_folders" {
		t.Errorf("outside_mapped_folders became %v", refusalCode(err))
	}
}

// BB-015: a local file the upload cannot read is unreadable_file, on the item.
func TestAnUnreadableLocalFileIsCoded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	f := newFakeStoreInstall(t)
	src := t.TempDir()
	writeFile(filepath.Join(src, "ok.txt"), "x")
	locked := filepath.Join(src, "locked.txt")
	writeFile(locked, "x")
	os.Chmod(locked, 0)
	defer os.Chmod(locked, 0o600)
	code, out := storeRun(t, f, "cp", "-r", src+"/", "toft://reports/up/", "--json")
	if code != exitFailed {
		t.Errorf("exit %d:\n%s", code, out)
	}
	found := false
	for _, d := range jsonLines(t, out) {
		if strings.HasSuffix(fmt.Sprint(d["from"]), "locked.txt") {
			found = d["code"] == "unreadable_file"
		}
	}
	if !found {
		t.Errorf("the unreadable item has no code:\n%s", out)
	}
}

// BB-005: a rollback the install rebuilt for a changed base path is a new row
// at the top of the list. Rolling back from it goes older than the release it
// restored, never forward to the one it left.
func TestRollbackFromARebuiltRollbackGoesOlder(t *testing.T) {
	// A < B < C; C was rolled back to B, then B to A — and A was rebuilt as R.
	rows := []deployment{
		{ID: "R", Status: "success", CommitSHA: "aaa", Branch: "main", TriggeredBy: "rollback", Serving: true},
		{ID: "C", Status: "success", CommitSHA: "ccc", Branch: "main"},
		{ID: "B", Status: "success", CommitSHA: "bbb", Branch: "main"},
		{ID: "A", Status: "success", CommitSHA: "aaa", Branch: "main"},
	}
	if got, err := rollbackTarget(rows, ""); refusalCode(err) != "nothing_older" || got != nil {
		t.Errorf("third rollback = %v, %v; want nothing_older", got, err)
	}
	// One rebuilt step: R restores B, and the previous is A.
	rows = []deployment{
		{ID: "R", Status: "success", CommitSHA: "bbb", Branch: "main", TriggeredBy: "rollback", Serving: true},
		{ID: "C", Status: "success", CommitSHA: "ccc", Branch: "main"},
		{ID: "B", Status: "success", CommitSHA: "bbb", Branch: "main"},
		{ID: "A", Status: "success", CommitSHA: "aaa", Branch: "main"},
	}
	got, err := rollbackTarget(rows, "")
	if err != nil || got.ID != "A" {
		t.Errorf("rollback from a rebuilt B = %v, %v; want A", got, err)
	}
	if !newerThanServing(rows, &rows[1]) || newerThanServing(rows, &rows[3]) {
		t.Error("C is newer than the release R restored, and A is older")
	}
	// An install that records restored_from: the commit is not needed — R
	// restored B (no commit, a pushed tree), so the previous is A.
	rows = []deployment{
		{ID: "R", Status: "success", TriggeredBy: "rollback", RestoredFrom: "B", Serving: true},
		{ID: "C", Status: "success"},
		{ID: "B", Status: "success"},
		{ID: "A", Status: "success"},
	}
	if got, err := rollbackTarget(rows, ""); err != nil || got.ID != "A" {
		t.Errorf("rollback from R restoring B = %v, %v; want A", got, err)
	}
	rows[0].RestoredFrom = "A"
	if got, err := rollbackTarget(rows, ""); refusalCode(err) != "nothing_older" || got != nil {
		t.Errorf("rollback from R restoring A = %v, %v; want nothing_older", got, err)
	}
	// The ordinary case is unchanged.
	rows = []deployment{
		{ID: "C", Status: "success"},
		{ID: "B", Status: "success", Serving: true},
		{ID: "A", Status: "success"},
	}
	if got, err := rollbackTarget(rows, ""); err != nil || got.ID != "A" {
		t.Errorf("ordinary = %v, %v", got, err)
	}
}

// BB-006: an app on its own hostname is fetched there, not at the apps
// address that only redirects to it — credential and all.
func TestFetchGoesToTheAppsOwnHostname(t *testing.T) {
	var auth, cookies, path string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, cookies, path = r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.URL.Path
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(host.Close)
	hostURL := strings.Replace(host.URL, "127.0.0.1", "localhost", 1)
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":"demo","status":"running","public_url":%q}}`, hostURL)
		case strings.HasPrefix(r.URL.Path, "/apps/"):
			http.Redirect(w, r, hostURL+"/", http.StatusPermanentRedirect)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	if err := cmdFetch(e, []string{"/healthz", "--app", "demo"}); err != nil {
		t.Fatalf("fetch = %v:\n%s", err, buf)
	}
	if path != "/healthz" || auth != "Bearer toftu_test" || !strings.Contains(buf.String(), "ok") {
		t.Errorf("hostname got path %q auth %q:\n%s", path, auth, buf)
	}

	// Under --session both of the gateway's cookie names go, since which one
	// the host reads is the install's setting.
	s, app, err := jobSession(e, &globalFlags{app: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	s.client.Cookie, s.client.Token = true, "jwt"
	if _, err := s.fetchApp(e.ctx, app, fetchRequest{Path: "/"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cookies, "toft_app_session=jwt") || !strings.Contains(cookies, "__Host-toft_app_session=jwt") {
		t.Errorf("cookies = %q", cookies)
	}

	// An app with no hostname (public_url on the apps address) is unchanged.
	gw, own := (&session{client: s.client, Session: &config.Session{InstallURL: "https://t.example"}, cfgRead: true}).fetchBase(&appSummary{Slug: "demo",
		PublicURL: "https://t.example/apps/demo"})
	if own || !strings.HasSuffix(gw, "/apps/demo") {
		t.Errorf("fetchBase = %q, %v", gw, own)
	}
}

// BB-021: nothing on stdin to --data @- is empty_input, nothing sent.
func TestFetchEmptyStdinIsEmptyInput(t *testing.T) {
	sent := false
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) { sent = true })
	stdinHolding(t, "")
	err := cmdFetch(e, []string{"/echo", "--app", "demo", "--method", "POST", "--data", "@-"})
	if refusalCode(err) != "empty_input" || sent {
		t.Errorf("err = %v (sent %v)", err, sent)
	}
}

// D-18: a flag's refusal is in this client's words — --param, not -param, and
// what a number flag wanted.
func TestFlagRefusalsAreInTheClientsWords(t *testing.T) {
	for _, c := range []struct {
		cmd  func(*env, []string) error
		args []string
		want string
	}{
		{cmdJob, []string{"run", "nightly", "--app", "demo", "--param", "noequals"}, `--param "noequals" needs a k=v pair`},
		{cmdLogs, []string{"--app", "demo", "--lines", "abc"}, `--lines takes a whole number, not "abc"`},
		{cmdFetch, []string{"/", "--header", "nocolon"}, `--header takes 'Name: value', not "nocolon"`},
	} {
		e, _ := testEnv()
		err := c.cmd(e, c.args)
		if err == nil || err.Error() != c.want || strings.Contains(err.Error(), "for flag -") {
			t.Errorf("%v = %v, want %q", c.args, err, c.want)
		}
	}
}

// BB-020: no install is exit 3 no_install whichever command says it.
func TestNoInstallIsOneExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("TOFT_URL", "")
	t.Setenv("TOFT_TOKEN", "")
	stdinHolding(t, "toftu_x\n")
	e, buf := testEnv()
	e.json = true
	if got := report(e, cmdLogin(e, []string{"--token-stdin", "--json"})); got != exitAuth ||
		lastDoc(t, buf.String())["code"] != "no_install" {
		t.Errorf("login with no install = %d:\n%s", got, buf)
	}
}

// BB-015: --mine with a deploy key is coded and hinted, exit 2.
func TestMineWithADeployKeyIsCoded(t *testing.T) {
	e, buf := testEnv()
	e.json = true
	if got := report(e, errMineNeedsAPerson); got != exitUsage {
		t.Errorf("exit %d", got)
	}
	doc := lastDoc(t, buf.String())
	if doc["code"] != "needs_personal_credential" || !strings.Contains(fmt.Sprint(doc["hint"]), "--mine") {
		t.Errorf("doc = %v", doc)
	}
}

// BB-017: an unwatched deploy carries the client's own bundle warnings.
func TestNoWatchCarriesTheBundleWarnings(t *testing.T) {
	e, buf := testEnv()
	e.bundleSecrets, e.bundleEnvFiles = []string{"server.key"}, []string{".env"}
	s := &session{client: client.New("https://t.example", "tok", "toft-test"), Session: &config.Session{InstallURL: "https://t.example"}}
	s.cfgRead = true
	if err := startedDocument(e, s, &appSummary{ID: "a", Slug: "demo"}, "d1"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`"code":"secret_in_bundle"`, `"code":"env_file_skipped"`, `"deployment_id":"d1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s: %s", want, out)
		}
	}
}

// BB-028: a go.work naming a directory the bundle leaves out is said, by
// check and deploy alike, before the build fails on it.
func TestAGoWorkspaceDirectoryLeftOutIsWarned(t *testing.T) {
	dir := t.TempDir()
	writeFile(filepath.Join(dir, "go.mod"), "module x\n\ngo 1.22\n")
	writeFile(filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
	writeFile(filepath.Join(dir, "go.work"), "go 1.22\n\nuse (\n\t.\n\t./.toft/sdk-go // vendored\n)\n")
	writeFile(filepath.Join(dir, ".gitignore"), "/.toft/\n")
	os.MkdirAll(filepath.Join(dir, ".toft", "sdk-go"), 0o755)
	writeFile(filepath.Join(dir, ".toft", "sdk-go", "go.mod"), "module toft\n")
	built, err := bundle.Build(dir, bundle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(built.Path)
	if len(built.WorkspaceLeftOut) != 1 || built.WorkspaceLeftOut[0] != ".toft/sdk-go" {
		t.Fatalf("left out = %v", built.WorkspaceLeftOut)
	}
	raw := withBundleNotices(json.RawMessage(`{"ok":true}`), nil, nil, built.WorkspaceLeftOut...)
	if !strings.Contains(string(raw), `"code":"go_work_dir_excluded"`) {
		t.Errorf("check = %s", raw)
	}

	// Shipped, it is not warned about.
	os.Remove(filepath.Join(dir, ".gitignore"))
	built, err = bundle.Build(dir, bundle.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(built.Path)
	if len(built.WorkspaceLeftOut) != 0 {
		t.Errorf("shipped, yet left out = %v", built.WorkspaceLeftOut)
	}
}

// sdk vendor: a .gitignore rule that already covers .toft/ is not given a
// second line under it.
func TestVendorDoesNotReIgnoreACoveredFolder(t *testing.T) {
	for _, rule := range []string{"/.toft/", ".toft/", ".toft", "/.toft"} {
		dir := t.TempDir()
		writeFile(filepath.Join(dir, ".gitignore"), rule+"\n")
		line, err := ignoreVendored(dir, filepath.Join(".toft", "sdk-python"))
		if err != nil || line != "" {
			t.Errorf("rule %q: added %q (%v)", rule, line, err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
		if string(got) != rule+"\n" {
			t.Errorf("rule %q: .gitignore now %q", rule, got)
		}
	}
	dir := t.TempDir()
	if line, _ := ignoreVendored(dir, filepath.Join(".toft", "sdk-python")); line != "/.toft/sdk-python/" {
		t.Errorf("nothing covering it: added %q", line)
	}
}

// D-05: toft sdk --help says what R's vendor writes and how R finds it.
func TestSDKHelpNamesR(t *testing.T) {
	if !strings.Contains(sdkUsage, ".toft/sdk-r/") || !strings.Contains(sdkUsage, "R CMD INSTALL .toft/sdk-r") {
		t.Error("toft sdk --help has no r entry")
	}
}

// BB-028: an install that checks go.work itself says so once — the client's
// own warning is not added beside the install's for the same directory.
func TestTheGoWorkWarningIsNotSaidTwice(t *testing.T) {
	raw := json.RawMessage(`{"ok":true,"warnings":["go.work uses .toft/sdk-go, which this tree does not contain"],` +
		`"warning_details":[{"code":"go_work_dir_excluded","table":"go.work","message":"go.work uses .toft/sdk-go, which this tree does not contain"}]}`)
	var doc struct {
		Details []map[string]any `json:"warning_details"`
	}
	if err := json.Unmarshal(withBundleNotices(raw, nil, nil, ".toft/sdk-go", "other"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Details) != 2 {
		t.Fatalf("details = %v; want the install's and the client's for other", doc.Details)
	}
}
