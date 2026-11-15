package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The drawn half of the app commands — deployments, env, deps, restart — on a
// terminal. The plain lines are pinned by each command's own tests.

func TestDeploymentsListIsATableOnATerminal(t *testing.T) {
	at := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprintf(w, `[{"id":"aaaaaaaa-1111-2222-3333-444444444444","status":"running","branch":"upload","started_at":%q},`+
				`{"id":"bbbbbbbb-1111-2222-3333-444444444444","status":"success","serving":true,"branch":"main",`+
				`"commit_sha":"0123456789","started_at":%q}]`, at, at)
		}
	})
	if err := cmdDeployments(e, []string{"list", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	for _, want := range []string{"DEPLOYMENT", "STATUS", "◐ running", "✓ success", "▶  bbbbbbbb ",
		"main · 0123456", "3h ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "aaaaaaaa-1111") {
		t.Errorf("ids should be the short form a prefix accepts:\n%s", got)
	}
}

func TestEnvListIsATableOnATerminal(t *testing.T) {
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case r.URL.Path == "/api/v1/apps/app-1":
			fmt.Fprint(w, `{"env_vars":[{"key":"REGION","value":"emea"},{"key":"TOKEN","sensitive":true}]}`)
		}
	})
	if err := cmdEnv(e, []string{"list", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	for _, want := range []string{"KEY", "VALUE", "REGION  emea", "TOKEN   •••••••• sensitive"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestDepsFindingsAreATableOnATerminal(t *testing.T) {
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/dependencies"):
			fmt.Fprint(w, `{"mode":"block","enforcing":true,"available":true,"has_data":true,"scanned":true,`+
				`"serving":{"deployment_id":"dep-1","counts":{"total":2,"counted":1},"findings":[`+
				`{"advisory_id":"GHSA-low","severity":"moderate","package":"lodash","version":"4.17.0","path":"app > a > lodash"},`+
				`{"advisory_id":"GHSA-bad","severity":"critical","package":"flask","version":"0.1","fixed_in":"2.3.2","direct":true}]}}`)
		}
	})
	if err := cmdDeps(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	got := out()
	for _, want := range []string{"SEVERITY", "● Serving", "✕ critical", "▲ moderate", "→ 2.3.2",
		"no fix published", "app > a > lodash"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "critical") > strings.Index(got, "moderate") {
		t.Errorf("worst first:\n%s", got)
	}
}

func TestStopReadsBackABadgeOnATerminal(t *testing.T) {
	e, out := richInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		default:
			fmt.Fprint(w, `{"status":"stopped"}`)
		}
	})
	if err := cmdStop(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out(), "demo  ○ stopped") {
		t.Errorf("got:\n%s", out())
	}
}
