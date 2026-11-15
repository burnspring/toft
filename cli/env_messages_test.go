package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// changed is the install's answer to a set of A that wrote it.
const changed = `{"set":["A"],"kept":[],"cleared":[],"unset":[],"not_present":[],"restart_needed":true}`

func runEnv(t *testing.T, fw, answer string, args ...string) string {
	t.Helper()
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":"demo","status":"running","framework":%q}}`, fw)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/apps/app-1/env"):
			fmt.Fprint(w, answer)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	if err := cmdEnv(e, append(args, "--app", "demo")); err != nil {
		t.Fatalf("toft env %v: %v", args, err)
	}
	return buf.String()
}

// A document reads its environment when it is rendered, at deploy, so
// `toft env set` on one names the next deploy and not `toft restart`, which
// serves the same pages again (docs/bugs 2026-10-28 D11).
func TestEnvSetOnADocumentNamesTheNextDeploy(t *testing.T) {
	for _, fw := range []string{"notebook", "quarto", "rmarkdown"} {
		got := runEnv(t, fw, changed, "set", "A=1")
		if strings.Contains(got, "toft restart") || !strings.Contains(got, "next deploy") {
			t.Errorf("%s: %q, want the next deploy named and no restart", fw, got)
		}
	}
	if got := runEnv(t, "flask", changed, "set", "A=1"); !strings.Contains(got, "toft restart") {
		t.Errorf("a served app's env set should name toft restart: %q", got)
	}
}

// A write that changed nothing promises no restart: an only-missing set of
// a variable already there is reported kept.
func TestEnvSetThatChangedNothingNamesNoRestart(t *testing.T) {
	got := runEnv(t, "flask", `{"set":[],"kept":["SECRET"],"restart_needed":false}`, "set", "SECRET=x", "--only-missing")
	if strings.Contains(got, "SECRET set") || strings.Contains(got, "toft restart") {
		t.Errorf("an unchanged secret was claimed set: %q", got)
	}
	if !strings.Contains(got, "kept") {
		t.Errorf("the output should say it was kept: %q", got)
	}
}

// A static app has no process to read its environment: `toft env set` says
// the variable reaches nothing rather than naming `toft restart` (drills
// 2026-10-28 #8).
func TestEnvSetOnAStaticAppSaysItReachesNothing(t *testing.T) {
	got := runEnv(t, "static", changed, "set", "A=1")
	if strings.Contains(got, "toft restart") || !strings.Contains(got, "reaches nothing") {
		t.Errorf("env set on a static app: %q", got)
	}
	got = runEnv(t, "static", changed, "set", "A=1", "--json")
	if strings.Contains(got, "restart_needed") || !strings.Contains(got, "reaches nothing") {
		t.Errorf("env set --json on a static app: %q", got)
	}
}

// A value sent again is unchanged: said, and no restart named for it.
func TestEnvSetOfTheSameValueNamesNoRestart(t *testing.T) {
	got := runEnv(t, "flask", `{"set":[],"unchanged":["A"]}`, "set", "A=1")
	if strings.Contains(got, "toft restart") || !strings.Contains(got, "A already had that value") {
		t.Errorf("an unchanged write: %q", got)
	}
}
