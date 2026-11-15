package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"toft/cli/internal/client"
)

// The 2026-11-04 black-box list (M1–L11).

// M1: an object path given to ls without its slash is is_an_object, exit 2 —
// never an empty folder, and never a retryable failure.
func TestListingAnObjectPathSaysItIsAnObject(t *testing.T) {
	f := newFakeStoreInstall(t)
	f.put("reports/x/a.txt", "x")
	f.put("reports/top.txt", "y")
	for _, path := range []string{"toft://reports/x/a.txt", "toft://reports/top.txt"} {
		code, out := storeRun(t, f, "ls", path, "--json")
		if code != exitUsage || !strings.Contains(out, `"code":"is_an_object"`) || !strings.Contains(out, `"retryable":false`) ||
			!strings.Contains(out, "toft store get") {
			t.Errorf("ls %s = %d:\n%s", path, code, out)
		}
	}
	// A path that names nothing still lists empty, and one written as a
	// folder is never taken for an object.
	if code, out := storeRun(t, f, "ls", "toft://reports/nothing", "--json"); code != exitOK || !strings.Contains(out, `"objects":[]`) {
		t.Errorf("ls of nothing = %d:\n%s", code, out)
	}
}

// M2: the store pacing the caller is retried with backoff, and what still
// fails carries its code and no bytes; the batch's document has a code too.
func TestABusyStoreIsRetriedAndCoded(t *testing.T) {
	defer func(n int, d time.Duration) { busyRetries, busyBackoff = n, d }(busyRetries, busyBackoff)
	busyBackoff = time.Millisecond

	var refusals atomic.Int32
	refusals.Store(3)
	f := newFakeStoreInstall(t)
	inner := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/bucket/") && r.Method == http.MethodPut && refusals.Add(-1) >= 0 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, "too many transfers at once — wait for one to finish")
			return
		}
		inner.ServeHTTP(w, r)
	})
	src := t.TempDir()
	for i := 0; i < 6; i++ {
		if err := writeFile(fmt.Sprintf("%s/f%d.txt", src, i), "ab"); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := storeRun(t, f, "cp", "-r", src+"/", "toft://reports/many/", "-j", "8", "--json"); code != exitOK {
		t.Fatalf("cp with three 429s = %d:\n%s", code, out)
	}
	if len(f.keys()) != 6 {
		t.Errorf("keys = %v", f.keys())
	}

	// Never admitted: each row is coded, reports no bytes, and the batch is
	// store_busy, retryable.
	busyRetries = 1
	refusals.Store(1000)
	code, out := storeRun(t, f, "cp", "-r", src+"/", "toft://reports/again/", "--json")
	if code != exitFailed || !strings.Contains(out, `"code":"store_busy"`) || strings.Contains(out, `"bytes":2`) ||
		!strings.Contains(out, `"retryable":true`) {
		t.Errorf("cp never admitted = %d:\n%s", code, out)
	}
}

// M2: a partly failed batch's final document carries a code.
func TestAPartialBatchIsCoded(t *testing.T) {
	e, buf := testEnv()
	e.json = true
	if got := report(e, &partialError{failed: 1, total: 12}); got != exitFailed ||
		!strings.Contains(buf.String(), `"code":"partial_failure"`) {
		t.Errorf("exit %d: %s", got, buf.String())
	}
}

// M3: toft api reads flags before its operands, as every command does —
// which is also where a global flag before `api` lands.
func TestAPIFlagsMayComeFirst(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "GET", "me"},
		{"GET", "--json", "me"},
		{"--session=false", "GET", "me"},
	} {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"ok":true}`)
		})
		if err := cmdAPI(e, args); err != nil || !strings.Contains(buf.String(), `{"ok":true}`) {
			t.Errorf("toft api %v = %v: %s", args, err, buf.String())
		}
	}
}

// M5: check of a name nothing has claimed refuses a non-slug as create does.
func TestCheckRefusesANonSlugAsCreateDoes(t *testing.T) {
	checked := false
	e, _, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/by-slug/") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"app not found","code":"not_found"}`)
			return
		}
		checked = true
		fmt.Fprint(w, `{"ok":true,"framework":"static"}`)
	})
	err := cmdCheck(e, []string{"--app", "Blackbox_Cli", "--json"})
	if refusalCode(err) != "invalid_slug" || checked {
		t.Errorf("check --app Blackbox_Cli = %v (checked: %v), want invalid_slug before any check", err, checked)
	}
}

// L3: a hostname another app holds is the caller's to change, exit 2.
func TestHostnameTakenIsUsage(t *testing.T) {
	kind, exit := classifyRefusal(&client.Error{Status: http.StatusConflict, Code: "hostname_taken"})
	if kind != "usage" || exit != exitUsage {
		t.Errorf("hostname_taken = %s %d", kind, exit)
	}
}

// L4: a TOFT_URL that is not a URL is exit 2 invalid_url; a server that
// answers with a web page is not_toft, never its HTML as the error.
func TestAnAddressThatIsNotAnInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("TOFT_URL", "not-a-url")
	t.Setenv("TOFT_TOKEN", "toftu_x")
	e, buf := testEnv()
	e.json = true
	if got := report(e, cmdApps(e, []string{"--json"})); got != exitUsage || !strings.Contains(buf.String(), `"code":"invalid_url"`) {
		t.Errorf("TOFT_URL=not-a-url: exit %d\n%s", got, buf.String())
	}

	e, buf = fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<!DOCTYPE html><html><body>404</body></html>")
	})
	e.json = true
	if got := report(e, cmdApps(e, []string{"--json"})); got != exitUsage ||
		!strings.Contains(buf.String(), `"code":"not_toft"`) || strings.Contains(buf.String(), "DOCTYPE") {
		t.Errorf("a web page: exit %d\n%s", got, buf.String())
	}
}

// L5: `--plain false` before the command names the spelling that works, as
// its hint, not the generic toft --help.
func TestASwitchGivenAWordIsHinted(t *testing.T) {
	err := switchGivenAValue([]string{"--plain"}, []string{"false", "apps"})
	var u *usageError
	if !errors.As(err, &u) || u.code != "bad_flag" || !strings.Contains(u.hint, "--plain=false") {
		t.Errorf("err = %#v", err)
	}
}

// L11: job runs --limit below 1 is refused, not read as the default 50.
func TestJobRunsLimitBelowOneIsRefused(t *testing.T) {
	for _, n := range []string{"0", "-1"} {
		e, _ := fakeInstall(t, jobFake(t, &jobFakeOpts{}))
		if err := cmdJob(e, []string{"runs", "--app", "demo", "--limit", n}); refusalCode(err) != "invalid_limit" {
			t.Errorf("--limit %s = %v", n, err)
		}
	}
}

// L8: source_unavailable's hint names this command's flags, not the API's.
func TestSourceUnavailableHintNamesTheFlags(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"error":"no such repository","code":"source_unavailable","retryable":false,`+
			`"hint":"check repo_url and branch, and pass git_token when the repository is private"}`)
	})
	s, err := e.setup(&globalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	err = createFromRepo(s, "x", true, "", "https://github.com/o/r", "", "", "", "", nil)
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Hint, "--repo") || strings.Contains(apiErr.Hint, "git_token") {
		t.Errorf("err = %#v", err)
	}
}

// L9: put's --json answer names the object as a toft:// path beside the
// store-relative name.
func TestPutJSONNamesTheObjectAsAToftPath(t *testing.T) {
	f := newFakeStoreInstall(t)
	file := t.TempDir() + "/a.txt"
	if err := writeFile(file, "x"); err != nil {
		t.Fatal(err)
	}
	code, out := storeRun(t, f, "put", "toft://reports/mine/", file, "--json")
	if code != exitOK || !strings.Contains(out, `"object":"toft://reports/mine/a.txt"`) || !strings.Contains(out, `"name":"mine/a.txt"`) {
		t.Errorf("put = %d:\n%s", code, out)
	}
}

// L10: nothing on stdin to --from-file - is empty_input.
func TestEmptyStdinToFromFileIsEmptyInput(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	stdinHolding(t, "")
	if err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", "-"}); refusalCode(err) != "empty_input" {
		t.Errorf("err = %v", err)
	}
}

// A full queue's refusal document carries the install's figures.
func TestQueueFullCarriesItsFigures(t *testing.T) {
	e, buf := testEnv()
	e.json = true
	report(e, &client.Error{Status: 429, Code: "queue_full", Message: "full",
		Body: []byte(`{"error":"full","code":"queue_full","in_flight":50,"limit":50,"retry_after":30}`)})
	for _, want := range []string{`"in_flight":50`, `"limit":50`, `"retry_after":30`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %s: %s", want, buf.String())
		}
	}
}
