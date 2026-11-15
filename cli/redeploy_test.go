package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func appRowWithRepo(status, repo, branch string) string {
	return fmt.Sprintf(`{"app":{"id":"app-1","slug":"demo","status":%q,"repo_url":%q,"branch":%q}}`,
		status, repo, branch)
}

// **`toft redeploy` uploads nothing.** The fake fails the test if a multipart
// body ever arrives, which is the property that keeps this command from
// quietly becoming `toft deploy` with a different name.
func TestRedeployUploadsNothing(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
			t.Errorf("redeploy sent a bundle: Content-Type %q to %s", ct, r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/upload") {
			t.Errorf("redeploy posted to the upload route")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRowWithRepo("running", "https://github.com/corp/demo", "main"))
		case strings.HasSuffix(r.URL.Path, "/deploy"):
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"dep-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	if err := cmdRedeploy(e, []string{"--app", "demo", "--no-watch"}); err != nil {
		t.Fatalf("cmdRedeploy: %v", err)
	}
}

// It names the repository and branch before it starts, because on an app that
// has been pushed to that is the surprise the command exists to make visible.
func TestRedeploySaysWhatItWillBuild(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deploy") {
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"dep-9"}`)
			return
		}
		fmt.Fprint(w, appRowWithRepo("running", "https://github.com/corp/demo", "release"))
	})
	if err := cmdRedeploy(e, []string{"--app", "demo", "--no-watch"}); err != nil {
		t.Fatalf("cmdRedeploy: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "https://github.com/corp/demo") || !strings.Contains(out, "release") {
		t.Errorf("output %q does not name the repository and branch it is about to build", out)
	}
}

// An uploaded app rebuilds from the source last sent to it — and still sends
// nothing from this disk. Only one that has never gone live is refused, naming
// the command that would work.
func TestRedeployRebuildsAnUploadedAppFromItsCurrentSource(t *testing.T) {
	posted := false
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
			t.Errorf("redeploy sent a bundle: Content-Type %q", ct)
		}
		if strings.HasSuffix(r.URL.Path, "/deploy") {
			posted = true
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"dep-9"}`)
			return
		}
		fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"running","repo_url":"upload://demo.zip","current_release":"dep-1"}}`)
	})
	if err := cmdRedeploy(e, []string{"--app", "demo", "--no-watch"}); err != nil {
		t.Fatalf("cmdRedeploy: %v", err)
	}
	if !posted {
		t.Error("an uploaded app with a live release was not redeployed")
	}
	if !strings.Contains(buf.String(), "last uploaded") {
		t.Errorf("output %q does not say what it is building from", buf.String())
	}
}

func TestRedeployRefusesAnUploadedAppThatNeverWentLive(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deploy") {
			t.Error("redeploy posted for an app with nothing to rebuild")
		}
		fmt.Fprint(w, appRowWithRepo("running", "upload://demo.zip", ""))
	})
	err := cmdRedeploy(e, []string{"--app", "demo", "--no-watch"})
	if err == nil {
		t.Fatal("an uploaded app with no release was redeployed")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
	if !strings.Contains(err.Error(), "toft deploy") {
		t.Errorf("the refusal does not name the command that works: %v", err)
	}
}

// A queued build is the platform refusing in its own words, and exit 1.
func TestRedeployPassesTheRefusalThrough(t *testing.T) {
	const refusal = "a deployment is already queued for this app"
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deploy") {
			w.WriteHeader(http.StatusConflict)
			fmt.Fprintf(w, `{"error":%q,"code":"deploy_queued","retryable":true}`, refusal)
			return
		}
		fmt.Fprint(w, appRowWithRepo("running", "https://github.com/corp/demo", "main"))
	})
	err := cmdRedeploy(e, []string{"--app", "demo", "--no-watch"})
	if err == nil {
		t.Fatal("a queued deployment was accepted")
	}
	if !strings.Contains(err.Error(), refusal) {
		t.Errorf("the refusal was reworded: %v", err)
	}
	if got := report(e, err); got != exitFailed {
		t.Errorf("exit %d, want %d", got, exitFailed)
	}
}

// `--no-watch --json` answers one document, never a bare id -- the shape
// `toft create --no-watch --json` already answers. A bare id on stdout was not
// JSON, so the one mode built for scripts broke `jq`.
func TestNoWatchJSONIsADocument(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRowWithRepo("running", "https://github.com/corp/demo", "main"))
		case strings.HasSuffix(r.URL.Path, "/deploy"):
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"dep-9"}`)
		case strings.HasSuffix(r.URL.Path, "/config"):
			fmt.Fprint(w, `{"apps_base_url":"https://apps.example.com","apps_path_prefix":"/apps/"}`)
		}
	})
	if err := cmdRedeploy(e, []string{"--app", "demo", "--no-watch", "--json"}); err != nil {
		t.Fatalf("cmdRedeploy: %v", err)
	}
	var doc map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			if err := json.Unmarshal([]byte(line), &doc); err != nil {
				t.Fatalf("not JSON: %q (%v)", line, err)
			}
		}
	}
	if doc == nil || doc["deployment_id"] != "dep-9" || doc["app"] == nil {
		t.Errorf("--no-watch --json answered %q", buf.String())
	}
}
