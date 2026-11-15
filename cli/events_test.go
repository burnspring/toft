package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --since takes both spellings people actually have: a pipeline that recorded
// when it started has a timestamp, a person at a terminal has "two hours".
func TestParseSince(t *testing.T) {
	if got, err := parseSince(""); err != nil || !got.IsZero() {
		t.Errorf("empty --since = (%v, %v), want the zero time and no error", got, err)
	}

	stamp := "2026-08-21T00:00:00Z"
	got, err := parseSince(stamp)
	if err != nil {
		t.Fatalf("parseSince(%q): %v", stamp, err)
	}
	if got.UTC().Format(time.RFC3339) != stamp {
		t.Errorf("parseSince(%q) = %v", stamp, got)
	}

	got, err = parseSince("2h")
	if err != nil {
		t.Fatalf("parseSince(2h): %v", err)
	}
	if delta := time.Since(got); delta < 119*time.Minute || delta > 121*time.Minute {
		t.Errorf("parseSince(2h) is %s ago, want about 2h", delta)
	}

	// **Never a filter silently ignored.** The endpoint drops a `since` it
	// cannot parse, which would answer a different question from the one asked,
	// so the client refuses first and names the flag.
	err = mustFail(t, func() error { _, err := parseSince("last tuesday"); return err })
	if !strings.Contains(err.Error(), "--since") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
	e, _ := testEnv()
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
}

func mustFail(t *testing.T, f func() error) error {
	t.Helper()
	err := f()
	if err == nil {
		t.Fatal("accepted, want a refusal")
	}
	return err
}

// The filters reach the server as query parameters, and `since` reaches it as
// RFC3339 whichever spelling was typed.
func TestEventsSendsItsFilters(t *testing.T) {
	var got url.Values
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			got = r.URL.Query()
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, appRow("running"))
	})

	err := cmdEvents(e, []string{"--app", "demo", "--since", "2h",
		"--severity", "error", "--kind", "app_crashed", "--limit", "5"})
	if err != nil {
		t.Fatalf("cmdEvents: %v", err)
	}
	if got.Get("severity") != "error" || got.Get("kind") != "app_crashed" || got.Get("limit") != "5" {
		t.Errorf("filters did not travel: %v", got)
	}
	if _, err := time.Parse(time.RFC3339, got.Get("since")); err != nil {
		t.Errorf("since=%q is not RFC3339, which is the only form the endpoint reads", got.Get("since"))
	}
}

// An app nothing has gone wrong with is not an error, and --json still answers
// with a list rather than nothing at all.
func TestEventsOnAQuietApp(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, appRow("running"))
	})
	if err := cmdEvents(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatalf("cmdEvents: %v", err)
	}
	var list []appEvent
	if err := unmarshalNDJSON(buf.Bytes(), &list); err != nil {
		t.Fatalf("--json is not NDJSON: %q", buf.String())
	}
	if len(list) != 0 {
		t.Errorf("got %d events, want none", len(list))
	}
}

func TestEventsRendersARow(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			fmt.Fprint(w, `[{"id":"ev-1","kind":"crash","severity":"error",
				"summary":"the app exited 1 during startup",
				"detail":"ModuleNotFoundError: no module named 'flask'",
				"occurred_at":"2026-08-21T00:00:00Z"}]`)
			return
		}
		fmt.Fprint(w, appRow("crashed"))
	})
	if err := cmdEvents(e, []string{"--app", "demo"}); err != nil {
		t.Fatalf("cmdEvents: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"crash", "the app exited 1 during startup", "ModuleNotFoundError"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
}

// Piped, an event's detail goes to stdout with its headline: the detail is
// part of the answer — a rerun remedy is in it — and on stderr a pipe kept
// the headline and lost the rest (docs/bugs 2026-10-28 D19).
func TestPipedEventsKeepTheDetailWithTheHeadline(t *testing.T) {
	e, stdout, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			fmt.Fprint(w, `[{"id":"ev-1","kind":"job_failed","severity":"error",
				"summary":"nightly failed","detail":"first line\nRerun it: toft job run nightly",
				"occurred_at":"2026-08-21T00:00:00Z"}]`)
			return
		}
		fmt.Fprint(w, appRow("running"))
	})
	if err := cmdEvents(e, []string{"--app", "demo"}); err != nil {
		t.Fatalf("cmdEvents: %v", err)
	}
	for _, want := range []string{"nightly failed", "first line", "Rerun it: toft job run nightly"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q is missing %q", stdout, want)
		}
	}
	if strings.Contains(stderr.String(), "Rerun it") {
		t.Errorf("the detail went to stderr: %q", stderr)
	}
}
