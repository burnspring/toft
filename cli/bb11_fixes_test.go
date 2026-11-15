package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/cli/internal/client"
)

// The 2026-11-04 black-box list, third round (TOFT-BB-101 … TOFT-BB-106).

// BB-101: an app's own hostname that does not resolve is exit 4,
// hostname_unresolved, not retryable, with a hint naming the DNS record and
// `toft hostname --off`; a refused connection is exit 4 unreachable,
// retryable. Neither is an uncoded exit 1 any more.
func TestFetchThatReachesNothingIsCoded(t *testing.T) {
	nx := &net.DNSError{Err: "no such host", Name: "my-app.example.com", IsNotFound: true}
	err := appUnreachable("https://my-app.example.com/", true, "demo", nx)
	e, buf := testEnv()
	e.json = true
	if code := report(e, err); code != exitUnreachable {
		t.Fatalf("exit %d:\n%s", code, buf)
	}
	doc := lastDoc(t, buf.String())
	if doc["code"] != "hostname_unresolved" || doc["kind"] != "unreachable" || doc["retryable"] != false ||
		!strings.Contains(fmt.Sprint(doc["hint"]), "toft hostname --off --app demo") {
		t.Errorf("doc = %v", doc)
	}

	// The apps address not resolving is unreachable, not the hostname's code.
	if err := appUnreachable("https://apps.example/apps/demo/", false, "demo", nx); refusalCodeOfUnreach(err) != "unreachable" {
		t.Errorf("apps address = %v", err)
	}

	// A refused connection, end to end through cmdFetch.
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.URL
	closed.Close()
	e2, _, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":"demo","status":"running","public_url":%q}}`,
				strings.Replace(addr, "127.0.0.1", "localhost", 1))
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	err = cmdFetch(e2, []string{"/", "--app", "demo"})
	var u *appUnreachableError
	if !errors.As(err, &u) || u.code != "unreachable" || !u.retryable {
		t.Fatalf("refused connection = %#v", err)
	}
	if code := report(e2, err); code != exitUnreachable {
		t.Errorf("exit %d", code)
	}
}

func refusalCodeOfUnreach(err error) string {
	var u *appUnreachableError
	if errors.As(err, &u) {
		return u.code
	}
	return ""
}

// BB-103: the app's own Set-Cookie reaches --json's headers (and the text
// rendering); the platform's toft_-named session cookies never do.
func TestFetchPassesTheAppsSetCookie(t *testing.T) {
	e, stdout, stderr := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "a=1; Path=/apps/demo/")
		w.Header().Add("Set-Cookie", "b=2; Path=/apps/demo/; HttpOnly")
		w.Header().Add("Set-Cookie", "toft_app_session=secret-jwt; Path=/apps")
		w.Header().Add("Set-Cookie", "__Host-toft_app_session=secret-jwt; Path=/")
		fmt.Fprint(w, "ok")
	}))
	if err := cmdFetch(e, []string{"/cookie", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	doc := oneDocument(t, stdout)
	headers, _ := doc["headers"].(map[string]any)
	got := fmt.Sprint(headers["Set-Cookie"])
	if got != "a=1; Path=/apps/demo/\nb=2; Path=/apps/demo/; HttpOnly" {
		t.Errorf("Set-Cookie = %q", got)
	}
	stdout.Reset()
	if err := cmdFetch(e, []string{"/cookie", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if s := stderr.String(); !strings.Contains(s, "Set-Cookie: a=1; Path=/apps/demo/") ||
		!strings.Contains(s, "Set-Cookie: b=2;") || strings.Contains(s+stdout.String(), "secret-jwt") {
		t.Errorf("text rendering:\n%s", s)
	}
}

// BB-104: a status that fails the fetch — --expect's mismatch, or a non-2xx/3xx
// without it — is unexpected_status, and under --json a final coded error
// object follows the result. Exit 1 as before.
func TestFetchFailingStatusAddsACodedErrorObject(t *testing.T) {
	for _, args := range [][]string{{"/500", "--expect", "200"}, {"/500"}, {"/ok", "--expect", "201"}} {
		e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/500") {
				w.WriteHeader(http.StatusInternalServerError)
			}
			fmt.Fprint(w, "x")
		}))
		e.json = true
		err := cmdFetch(e, append(args, "--app", "demo", "--json"))
		if code := report(e, err); code != exitFailed {
			t.Fatalf("%v: exit %d", args, code)
		}
		docs := jsonLines(t, stdout.String())
		if len(docs) != 2 {
			t.Fatalf("%v: want the result then the error object:\n%s", args, stdout)
		}
		if docs[0]["status"] == nil || docs[1]["code"] != "unexpected_status" || docs[1]["kind"] != "failed" ||
			docs[1]["hint"] == nil || docs[1]["exit"] != float64(exitFailed) {
			t.Errorf("%v: docs = %v", args, docs)
		}
	}
}

// BB-105: text mode prints every warning the install returned — a coded one
// carried only in warning_details included — each with its code.
func TestDeployTextPrintsEveryWarning(t *testing.T) {
	const removed = "The scheduled restart was removed: this release's toft.toml declares no [restart]."
	for name, frame := range map[string]string{
		"in both":          `"warnings":["` + removed + `"],"warning_details":[{"code":"schedule_removed","table":"restart","message":"` + removed + `"}]`,
		"in details alone": `"warning_details":[{"code":"schedule_removed","table":"restart","message":"` + removed + `"}]`,
	} {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/progress"):
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d1\",\"status\":\"success\","+frame+"}\n\n")
			case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
				fmt.Fprint(w, appRow("running"))
			default:
				fmt.Fprint(w, `{}`)
			}
		})
		s, app, err := jobSession(e, &globalFlags{app: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := watchDeploy(e, s, app, "d1", &globalFlags{app: "demo"}, false); err != nil {
			t.Fatal(err)
		}
		if out := buf.String(); strings.Count(out, "Warning: "+removed+" (schedule_removed)") != 1 {
			t.Errorf("%s: text output:\n%s", name, out)
		}
		// And --json's warnings carries the sentence either way.
		buf.Reset()
		_ = watchAsJSON(e, s, app, "d1")
		if doc := lastDoc(t, buf.String()); !strings.Contains(fmt.Sprint(doc["warnings"]), removed) {
			t.Errorf("%s: --json result %v", name, doc)
		}
	}
}

// BB-106: insufficient_access is kind refused (exit 3) however it arrives —
// the install's own coded 403 from put/rm, as mv's up-front check says it.
func TestInsufficientAccessIsRefusedEverywhere(t *testing.T) {
	hint := "ask an administrator for write access to this store; `toft store ls` shows your level on each"
	for _, apiErr := range []*client.Error{
		{Status: http.StatusForbidden, Code: "insufficient_access", Message: "this store does not take uploads at your access level", Hint: hint},
		{Status: http.StatusForbidden, Code: "forbidden", Message: "forbidden"},
	} {
		e, buf := testEnv()
		e.json = true
		if code := report(e, storeError(apiErr, "reports")); code != exitAuth {
			t.Fatalf("%s: exit %d", apiErr.Code, code)
		}
		doc := lastDoc(t, buf.String())
		if doc["kind"] != "refused" || doc["code"] != "insufficient_access" || doc["hint"] == nil {
			t.Errorf("%s: doc = %v", apiErr.Code, doc)
		}
	}
	// mv's own refusal, for comparison: the same kind and code.
	e, buf := testEnv()
	e.json = true
	report(e, refusedCodef("insufficient_access", "nothing was moved"))
	if doc := lastDoc(t, buf.String()); doc["kind"] != "refused" {
		t.Errorf("mv doc = %v", doc)
	}
}

// BB-106: store mkdir --json says which folders were new and which already
// existed, as text's created/exists does.
func TestStoreMkdirJSONSaysWhetherItExisted(t *testing.T) {
	f := newFakeStoreInstall(t)
	code, out := storeRun(t, f, "mkdir", "reports/new", "--json")
	if code != exitOK {
		t.Fatalf("mkdir = %d:\n%s", code, out)
	}
	doc := lastDoc(t, out)
	if fmt.Sprint(doc["created"]) != "[toft://reports/new/]" || fmt.Sprint(doc["existed"]) != "[]" {
		t.Errorf("first mkdir = %v", doc)
	}
	code, out = storeRun(t, f, "mkdir", "reports/new", "--json")
	if code != exitOK {
		t.Fatalf("second mkdir = %d:\n%s", code, out)
	}
	doc = lastDoc(t, out)
	if fmt.Sprint(doc["existed"]) != "[toft://reports/new/]" || fmt.Sprint(doc["created"]) != "[]" ||
		fmt.Sprint(doc["folders"]) != "[toft://reports/new/]" {
		t.Errorf("second mkdir = %v", doc)
	}
}

// TOFT-BB-102: the install's owner refusals arrive as 422 and exited 1, while
// owner_group_required and an unknown group, caught here first, exit 2. One
// family, one exit.
func TestOwnerRefusalsAreUsage(t *testing.T) {
	for _, code := range []string{"owner_group_empty", "owner_group_no_eligible_member", "owner_group_mapped",
		"owner_not_found", "owner_is_viewer", "owner_user_locked"} {
		kind, exit := classifyRefusal(&client.Error{Status: http.StatusUnprocessableEntity, Code: code})
		if kind != "usage" || exit != exitUsage {
			t.Errorf("%s: kind %q exit %d, want usage %d", code, kind, exit, exitUsage)
		}
	}
}
