package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// originFake answers a status: the config probe, the app, and its (empty)
// deployment history. probes counts the config reads, which is the other half
// of what is asserted here.
func originFake(t *testing.T, cfg string, probes *int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/api/v1/config":
			*probes++
			if cfg == "" {
				http.Error(w, "not here", http.StatusNotFound)
				return
			}
			fmt.Fprint(w, cfg)
		case strings.HasSuffix(p, "/apps/by-slug/demo"):
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"running"}}`)
		case strings.HasSuffix(p, "/deployments"):
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
		}
	}
}

// statusURL runs `toft status --json` against a fake install and returns the URL
// it reported — the field a script, or an agent about to fetch the app, reads.
func statusURL(t *testing.T, cfg string, probes *int) string {
	t.Helper()
	e, buf := fakeInstall(t, originFake(t, cfg, probes))
	if err := cmdStatus(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}
	var got struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", buf.String(), err)
	}
	return got.URL
}

// **An app's address comes from the install, not from the install's address.**
// A two-origin install serves apps somewhere else entirely, and the platform
// origin answers `/apps/...` with the SPA rather than a 404 — so a derived URL
// fetches successfully and shows the wrong server's page.
func TestAppURLUsesTheOriginTheInstallPublishes(t *testing.T) {
	probes := 0
	got := statusURL(t, `{"apps_base_url":"https://apps.example.com","apps_path_prefix":"/apps/","version":"0.1.0"}`, &probes)
	if want := "https://apps.example.com/apps/demo"; got != want {
		t.Errorf("url %q, want %q", got, want)
	}
}

// A trailing slash on the base and a prefix spelled either way are the same
// address. One separator, wherever the two halves put theirs.
func TestAppURLJoinsTheHalvesOnce(t *testing.T) {
	for _, cfg := range []string{
		`{"apps_base_url":"https://apps.example.com/","apps_path_prefix":"/apps/"}`,
		`{"apps_base_url":"https://apps.example.com","apps_path_prefix":"apps"}`,
	} {
		probes := 0
		if got, want := statusURL(t, cfg, &probes), "https://apps.example.com/apps/demo"; got != want {
			t.Errorf("%s\n url %q, want %q", cfg, got, want)
		}
	}
}

// Single origin publishes an empty base deliberately — relative links are
// correct there — and the install's own address is then the right one. What
// must never happen is the empty value reaching the output: a relative path is
// not an address anybody can fetch.
func TestAppURLFallsBackToTheInstallOnASingleOrigin(t *testing.T) {
	probes := 0
	got := statusURL(t, `{"apps_base_url":"","apps_path_prefix":"/apps/","separate_apps_origin":false}`, &probes)
	if !strings.HasPrefix(got, os.Getenv("TOFT_URL")) || !strings.HasSuffix(got, "/apps/demo") {
		t.Errorf("url %q is not the install's own address", got)
	}
}

// An install too old to publish either field, or a probe that failed: the zero
// value is the derivation this replaced, which is right wherever the two
// origins are one and is what the client did before. A courtesy read may not
// fail the command it decorates.
func TestAppURLSurvivesAnInstallThatSaysNothing(t *testing.T) {
	probes := 0
	e, buf := fakeInstall(t, originFake(t, "", &probes))
	if err := cmdStatus(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatalf("a failed config probe failed the command: %v", err)
	}
	if !strings.Contains(buf.String(), "/apps/demo") {
		t.Errorf("no fallback url: %q", buf.String())
	}
}

// The probe is a courtesy read, and a command that prints the URL more than
// once must not ask more than once — including when it failed, which is a fact
// worth remembering rather than a reason to retry.
func TestTheInstallIsProbedOnceAnInvocation(t *testing.T) {
	for _, cfg := range []string{`{"apps_base_url":"https://apps.example.com"}`, ""} {
		probes := 0
		e, _ := fakeInstall(t, originFake(t, cfg, &probes))
		s, err := e.setup(&globalFlags{app: "demo"})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		app := &appSummary{Slug: "demo"}
		s.appURL(app)
		s.appURL(app)
		e.noteInstallVersion(s)
		if probes != 1 {
			t.Errorf("cfg %q: the config probe ran %d times, want 1", cfg, probes)
		}
	}
}
