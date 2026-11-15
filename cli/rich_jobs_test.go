package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The jobs commands drawn for a person: the plain halves are pinned in
// job_test.go, and these pin the layout a terminal gets.

func TestJobListOnATerminal(t *testing.T) {
	next := time.Now().Add(3*time.Hour + time.Minute).UTC().Format(time.RFC3339)
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, appRow("running"))
			return
		}
		fmt.Fprintf(w, `{"placement":"cluster","jobs_available":true,"jobs":[
			{"id":"j1","name":"nightly-report","schedule":"CRON_TZ=Europe/London 0 3 * * *","overlap":"skip",
			 "notify":"failures","enabled":true,"next_run_at":%q,
			 "params_spec":{"region":{"type":"string","required":true,"help":"Which region","choices":["emea","amer"]}},
			 "limits":{"timeout":{"value":900,"source":"clamped","declared":2700},"memory":{"value":1024,"source":"declared"}}},
			{"id":"j2","name":"reindex","enabled":true},
			{"id":"j3","name":"digest","schedule":"0 6 * * *","paused":true}]}`, next)
	})
	if err := cmdJob(e, []string{"list", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	for _, want := range []string{
		"nightly-report  ⏱ 0 3 * * * (Europe/London)", "in 3h",
		"rules    overlap skip · notify failures",
		"limits   timeout 15m (clamped by admin, declared 45m) · memory 1024 MB (declared)",
		"param    --param region=<string>  required, one of emea|amer — Which region",
		"reindex  on demand",
		"digest  ⏱ 0 6 * * *  ■ paused until next deploy",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestJobRunsIsATableOnATerminal(t *testing.T) {
	e, out := richInstall(t, runsFake(t))
	if err := cmdJob(e, []string{"runs", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "RUN") || !strings.Contains(lines[0], "RESULT") {
		t.Errorf("want a header first:\n%s", got)
	}
	for _, want := range []string{
		"✓ succeeded", "12m",
		"✕ failed", "exit 3, connection refused",
		"◐ running · 4m", "40% loading rows", "◌ queued · 2nd",
		"→ toft job runs 9b21c0aa", "then toft job rerun 9b21c0aa",
		"○ skipped", "the previous run was still going",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 100 {
			t.Errorf("%d columns, over the terminal's 100: %q", n, l)
		}
	}
}

func TestJobRunDetailOnATerminal(t *testing.T) {
	e, out := richInstall(t, runsFake(t))
	if err := cmdJob(e, []string{"runs", "9b21c0aa", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	for _, want := range []string{
		"9b21c0aa  nightly-report  ✕ failed",
		"params       region=emea rows=500",
		"peak memory  612 MB",
		"queued       Oct 23", " ago",
		"next         toft job logs 9b21c0aa, then toft job rerun 9b21c0aa",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
