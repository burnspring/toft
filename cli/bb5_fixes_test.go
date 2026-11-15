package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

// The fifth 2026-11-03 black-box list, the CLI's half.

// BB-054: a --token-stdin value no install could have issued is refused here,
// exit 3 invalid_key, before anything is sent — as TOFT_TOKEN=garbage is.
func TestATokenStdinThatIsNotAToftKeyIsRefusedBeforeSending(t *testing.T) {
	bb3Env(t)
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid API key","code":"invalid_key"}`)
	}))
	defer srv.Close()

	stdinHolding(t, "garbage\n")
	stdout, _ := captureStd(t)
	if got := run([]string{"login", "--token-stdin", srv.URL, "--json"}); got != exitAuth {
		t.Errorf("exit %d, want 3", got)
	}
	doc := lastDoc(t, stdout())
	if msg, _ := doc["error"].(string); doc["code"] != "invalid_key" || !strings.Contains(msg, "nothing was sent") {
		t.Errorf("doc = %v", doc)
	}
	if n := sent.Load(); n != 0 {
		t.Errorf("a garbage key was sent %d time(s)", n)
	}
	// BB-068: the hint is about stdin, never TOFT_TOKEN — the key came from
	// the pipe.
	if hint, _ := doc["hint"].(string); strings.Contains(hint, "TOFT_TOKEN") || !strings.Contains(hint, "--token-stdin") {
		t.Errorf("hint = %q", hint)
	}
}

// BB-053: the refusals whose hint was missing or a generic --help name the
// step that gets past them.
func TestRefusalHintsNameTheNextStep(t *testing.T) {
	bb3Env(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"task", "--json"}, "toft job list"},
		{[]string{"login", "--forget", "https://nowhere.example", "--json"}, "TOFT_TOKEN"},
	} {
		stdout, _ := captureStd(t)
		run(tc.args)
		doc := lastDoc(t, stdout())
		if h, _ := doc["hint"].(string); !strings.Contains(h, tc.want) || strings.Contains(h, "--help shows how") {
			t.Errorf("%v: hint %q, want one naming %q (doc %v)", tc.args, h, tc.want, doc)
		}
	}

	e := &env{json: true, command: "delete"}
	var buf strings.Builder
	e.out = ui.New(false, false, false)
	e.out.Stdout = &buf
	e.errorDocument(usageCodef("confirmation_required", "refusing to delete demo without --yes (CI is set)"),
		"usage", "confirmation_required", exitUsage)
	if !strings.Contains(buf.String(), `pass --yes`) {
		t.Errorf("confirmation_required: %s", buf.String())
	}

	// An install's refusal that came without a hint gets this client's.
	for code, want := range map[string]string{"no_service": "toft job", "sdk_language_unknown": "--lang"} {
		buf.Reset()
		err := &client.Error{Status: http.StatusConflict, Code: code, Message: "refused"}
		e.errorDocument(err, "refused", code, exitAuth)
		var doc map[string]any
		json.Unmarshal([]byte(buf.String()), &doc)
		if h, _ := doc["hint"].(string); !strings.Contains(h, want) {
			t.Errorf("%s: hint %q, want %q", code, h, want)
		}
	}
	// The install's own hint still wins.
	buf.Reset()
	e.errorDocument(&client.Error{Status: 409, Code: "no_service", Message: "refused", Hint: "theirs"},
		"refused", "no_service", exitAuth)
	if !strings.Contains(buf.String(), `"hint":"theirs"`) {
		t.Errorf("the install's hint was replaced: %s", buf.String())
	}
}

// BB-062: start of a stopped app says "started" in status as in outcome — not
// the install's "restarted" beside it.
func TestStartOfAStoppedAppSaysStartedInStatus(t *testing.T) {
	var reads atomic.Int32
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			fmt.Fprint(w, `{"status":"restarted","action":"start"}`)
			return
		}
		if reads.Add(1) == 1 {
			fmt.Fprint(w, appRow("stopped"))
			return
		}
		fmt.Fprint(w, appRow("running"))
	})
	if err := cmdStart(e, []string{"--app", "demo", "--wait", "--json"}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &doc); err != nil {
		t.Fatalf("not one document: %q", buf.String())
	}
	if doc["outcome"] != "started" || doc["status"] != "started" {
		t.Errorf("doc = %v, want status and outcome started", doc)
	}
}

// BB-063: a leading switch given a value is a bad_flag document on stdout when
// --json is anywhere on the line, not only among the leading flags.
func TestALeadingSwitchGivenAValueIsADocumentWithJSONAnywhere(t *testing.T) {
	bb3Env(t)
	stdout, _ := captureStd(t)
	if got := run([]string{"--plain", "false", "apps", "--json"}); got != exitUsage {
		t.Errorf("exit %d, want 2", got)
	}
	if doc := lastDoc(t, stdout()); doc["code"] != "bad_flag" {
		t.Errorf("doc = %v", doc)
	}
}

// BB-050: scheduled_for is the install's string, offset and all — never
// re-rendered as UTC by this client — in runs <id>, runs and rerun.
func TestScheduledForKeepsTheInstallsOffset(t *testing.T) {
	const at = "2026-11-03T12:34:00-05:00"
	runDoc := `{"id":"9b21c0aa-2222","job":"nightly","status":"succeeded","started_by":"schedule",` +
		`"scheduled_for":"` + at + `","queued_at":"2026-11-03T17:34:00Z"}`
	for _, args := range [][]string{
		{"runs", "9b21c0aa", "--app", "demo", "--json"},
		{"runs", "--app", "demo", "--json"},
		{"rerun", "9b21c0aa", "--app", "demo", "--json"},
		{"rerun", "9b21c0aa", "--app", "demo", "--no-wait", "--json"},
	} {
		e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{states: []string{runDoc}}))
		if err := cmdJob(e, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(buf.String(), `"scheduled_for":"`+at+`"`) {
			t.Errorf("%v: scheduled_for was not passed through:\n%s", args, buf.String())
		}
	}
}
