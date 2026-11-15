package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// On a terminal the answers are drawn for a person — a header, badges, short
// ages — and off one they are the lines the plain tests pin. These pin the
// drawn half; colour is off, so what is asserted is the layout.

func richInstall(t *testing.T, handler http.HandlerFunc) (*env, func() string) {
	e, buf := fakeInstall(t, handler)
	e.out.Interactive = true
	e.out.Columns = 100
	return e, buf.String
}

func TestAppsIsATableOnATerminal(t *testing.T) {
	deployed := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":"a","slug":"demo","status":"running","framework":"flask",`+
			`"repo_url":"https://github.com/example/demo.git","last_deployed_at":%q},`+
			`{"id":"b","slug":"quiet","status":"sleeping","repo_url":"upload://x"}]`, deployed)
	})
	if err := cmdApps(e, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"APP", "STATUS", "DEPLOYED", "● running", "○ sleeping",
		"github.com/example/demo", "5m ago", "uploaded", "never"} {
		if !strings.Contains(out(), want) {
			t.Errorf("missing %q in:\n%s", want, out())
		}
	}
}

func TestStatusOnATerminal(t *testing.T) {
	at := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":"demo","status":"running",`+
				`"serving_since":%q,"status_changed_at":%q}}`, at, at)
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprintf(w, `[{"id":"c05c1973-8ff7-4df4-95d8-1afe8e3d9417","status":"success",`+
				`"serving":true,"branch":"main","finished_at":%q}]`, at)
		case r.URL.Path == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":""}`)
		}
	})
	if err := cmdStatus(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	for _, want := range []string{"● running for 5 minutes", "5 minutes ago · built then",
		"c05c1973-8ff7-4df4-95d8-1afe8e3d9417"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "built   ") {
		t.Errorf("two clocks saying the same minute should be one line:\n%s", got)
	}
}

// A failed job's detail usually restates its summary; the card says it once.
func TestEventsOnATerminalSayTheReasonOnce(t *testing.T) {
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		default:
			fmt.Fprintf(w, `[{"id":"1","kind":"job_run_failed","severity":"error",`+
				`"summary":"The job \"refresh\" failed: exit 1: no table apac",`+
				`"detail":"exit 1: no table apac\n\nrun ac346fc3 — run it again with: toft job rerun ac346fc3",`+
				`"occurred_at":%q}]`, time.Now().UTC().Format(time.RFC3339))
		}
	})
	if err := cmdEvents(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	if strings.Count(got, "no table apac") != 1 {
		t.Errorf("the reason should appear once:\n%s", got)
	}
	if !strings.Contains(got, "✕ error  job_run_failed") || !strings.Contains(got, "toft job rerun ac346fc3") {
		t.Errorf("want the badge, the kind and the rerun line:\n%s", got)
	}
	for _, l := range strings.Split(got, "\n") {
		if l != strings.TrimRight(l, " ") {
			t.Errorf("trailing whitespace: %q", l)
		}
	}
}

// A build log's tail as an event's detail is clipped under its card, and says
// where the rest is.
func TestEventsOnATerminalClipALongDetail(t *testing.T) {
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		default:
			fmt.Fprintf(w, `[{"id":"1","kind":"deploy_failed","severity":"error","summary":"A deploy did not finish",`+
				`"detail":"l1\nl2\nl3\nl4\nl5\nl6\nl7","deployment_id":"dep-9","occurred_at":%q}]`,
				time.Now().UTC().Format(time.RFC3339))
		}
	})
	if err := cmdEvents(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	if strings.Contains(got, "l5") || !strings.Contains(got, "… 3 more lines — toft logs --deploy dep-9") {
		t.Errorf("want four lines and the count of the rest:\n%s", got)
	}
}
