package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// The passthrough hands back the install's own JSON, unchanged.
//
// The point of the command is reaching a route this binary has no verb for, so
// re-encoding the answer through a struct would defeat it: a field `toft` has
// never heard of has to survive, and that is exactly the field somebody is
// using this for.
func TestAPIPassthroughReturnsTheInstallsOwnJSON(t *testing.T) {
	const answer = `{"id":"c1","name":"digest","a_field_lp_has_never_heard_of":[1,2,3]}`
	fake := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/a1/jobs" {
			t.Errorf("called %s, want the path as written", r.URL.Path)
		}
		fmt.Fprint(w, answer)
	}
	for _, args := range [][]string{
		{"GET", "/api/v1/apps/a1/jobs"},
		{"GET", "/api/v1/apps/a1/jobs", "--json"},
		// Relative to /api/v1/, which is the same call.
		{"GET", "apps/a1/jobs"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			e, buf := fakeInstall(t, fake)
			if err := cmdAPI(e, args); err != nil {
				t.Fatalf("toft api %s: %v", strings.Join(args, " "), err)
			}
			if got := strings.TrimSpace(buf.String()); got != answer {
				t.Errorf("got %q, want the install's own bytes %q", got, answer)
			}
		})
	}
}

// The refusals that are the caller's mistake, not the platform's.
func TestAPIRefusesWhatWouldReachTheWrongRoute(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no method or path", []string{}},
		{"no path", []string{"GET"}},
		// A typo'd method silently becoming a GET is a read where a write was
		// meant; the reverse is worse.
		{"unknown method", []string{"FETCH", "/api/v1/apps"}},
		// The install would refuse this, and its refusal would be about the
		// wrong thing — the caller wants to know their body is malformed.
		{"malformed body", []string{"POST", "/api/v1/apps", "--data", "{not json"}},
		{"body on a GET", []string{"GET", "/api/v1/apps", "--data", "{}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("a refusal reached the install: %s %s", r.Method, r.URL.Path)
			})
			if got := report(e, cmdAPI(e, tc.args)); got != exitUsage {
				t.Errorf("exited %d, want %d (usage)", got, exitUsage)
			}
		})
	}
}

// **A path the shell rewrote is named as such, before the request.**
//
// MSYS2 — Git Bash, which is the default shell for an agent on Windows —
// converts a POSIX-looking argument into a Windows path before exec, so
// `toft api GET /api/v1/apps` reaches this binary as
// `C:/Program Files/Git/api/v1/apps`. That has no leading slash, so the
// relative-path rule prefixes it, and the install resolves no route.
//
// Every part of that is invisible from where the caller sits: they typed the
// example out of `toft api --help`, and what came back was a sentence about
// credentials. This is the only place the evidence still exists.
func TestAPIRefusesAPathTheShellRewrote(t *testing.T) {
	for _, path := range []string{
		`C:/Program Files/Git/api/v1/apps`,
		`C:\Program Files\Git\healthz`,
		`c:/msys64/apps`,
	} {
		e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("a rewritten path was sent to the install: %s", r.URL.Path)
		})
		err := cmdAPI(e, []string{"GET", path})
		if err == nil {
			t.Fatalf("toft api accepted %q", path)
		}
		if got := report(e, err); got != exitUsage {
			t.Errorf("toft api %q exited %d, want %d — this is an argument problem, and "+
				"exit 3 sends an unattended run to fetch a human", path, got, exitUsage)
		}
		// The message has to carry what *arrived*: the caller's own command line
		// does not contain it, and comparing the two is the whole realisation.
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", path)) {
			t.Errorf("the refusal does not quote what arrived: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "MSYS_NO_PATHCONV") {
			t.Errorf("the refusal does not name a fix: %q", err.Error())
		}
	}
}

// The forms the help documents all reach the same route, and the absolute one
// is not double-prefixed. This is the table the audit measured and found only
// the bare relative form working — on a shell that was rewriting the other
// three before `toft` saw them.
func TestAPIPathFormsResolveWhereTheHelpSaysTheyDo(t *testing.T) {
	for _, tc := range []struct{ given, want string }{
		{"apps", "/api/v1/apps"},
		{"/api/v1/apps", "/api/v1/apps"},
		{"apps/123/jobs", "/api/v1/apps/123/jobs"},
		// Taken as written, which is what "a path is taken as written" means:
		// this one is a real route on the platform's own root.
		{"/healthz", "/healthz"},
	} {
		var got string
		e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			fmt.Fprint(w, `{}`)
		})
		if err := cmdAPI(e, []string{"GET", tc.given}); err != nil {
			t.Fatalf("toft api GET %s: %v", tc.given, err)
		}
		if got != tc.want {
			t.Errorf("toft api GET %s called %q, want %q", tc.given, got, tc.want)
		}
	}
}

// **An unknown route is exit 2, not exit 3.**
//
// The install answers 404 with code route_not_found; the credential guard used
// to answer 403 "this route requires an interactive session" first, which is
// the most expensive wrong answer this client can give — the documented
// recovery for exit 3 is to mint a key or ask an administrator, and neither is
// retryable, so a careful agent stops and escalates over a typo.
func TestAnUnknownRouteIsAUsageErrorAndNotAnAuthOne(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no route GET /api/v1/nosuchroute on this install",`+
			`"code":"route_not_found"}`)
	})
	err := cmdAPI(e, []string{"GET", "nosuchroute"})
	if err == nil {
		t.Fatal("an unknown route exited 0")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
	out := buf.String() + err.Error()
	if !strings.Contains(out, "nosuchroute") {
		t.Errorf("the refusal does not name the path it did not recognise: %q", out)
	}
}

// **A method the route does not take is exit 2 as well**, and the install's
// hint naming the method that would have worked reaches the caller.
func TestAWrongMethodIsAUsageErrorThatNamesTheRightOne(t *testing.T) {
	e, stdout, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "PUT")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprint(w, `{"error":"/api/v1/apps/x/tags does not take GET — it takes PUT",`+
			`"code":"method_not_allowed","retryable":false,"hint":"send PUT instead — nothing was attempted"}`)
	})
	if got := report(e, cmdAPI(e, []string{"GET", "apps/x/tags"})); got != exitUsage {
		t.Errorf("exit %d, want %d (usage)", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "Send PUT instead") {
		t.Errorf("the hint naming the method was not shown:\n%s", stderr)
	}
	if doc := oneDocument(t, stdout); doc["code"] != "method_not_allowed" {
		t.Errorf("stdout is not the install's body: %v", doc)
	}
}

// A slug where an apps/ route takes the id is a 404 that names the way to the
// id; the path itself is still taken as written (jobs-regression #11).
func TestASlugInAnAppsPathPointsAtTheID(t *testing.T) {
	e, stdout, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/apps/sdk-py" {
			t.Errorf("the path was rewritten: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		// The install's own 404 hint is the generic one; the slug's wins.
		fmt.Fprint(w, `{"error":"app not found","code":"not_found","hint":"GET /api/v1/apps lists the apps you can see","retryable":false}`)
	})
	if got := report(e, cmdAPI(e, []string{"GET", "apps/sdk-py"})); got != exitUsage {
		t.Errorf("exit %d, want %d (usage)", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "`toft api GET apps/by-slug/sdk-py` answers with the app's id") {
		t.Errorf("no way to the id:\n%s", stderr)
	}
	if doc := oneDocument(t, stdout); doc["code"] != "not_found" {
		t.Errorf("stdout is not the install's body: %v", doc)
	}
	for path, want := range map[string]bool{
		"/api/v1/apps/0b6f3c1e-2a4d-4e8b-9c1a-5d2e7f8a9b0c/env": false,
		"/api/v1/apps/by-slug/x":                                false, "/api/v1/apps/my-app/env": true, "/api/v1/apps": false,
	} {
		if _, got := slugInAppsPath(path); got != want {
			t.Errorf("slugInAppsPath(%s) = %v, want %v", path, got, want)
		}
	}
}

// **A path that is not the API is the caller's mistake, not a version skew.**
//
// Everywhere else in this client the path is the client's own choice, so an
// answer it cannot decode really does mean the install is a version it does not
// understand — and `DecodeError` says exactly that, and says to upgrade `toft`.
// Here the path came from the caller: `toft api GET /apps` is taken as written,
// reaches the SPA, and gets a 200 and an HTML document. Reporting that as
// "upgrade toft" sends somebody to replace a binary that is fine.
func TestAPIOnANonAPIPathBlamesThePathAndNotTheVersion(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><title>the SPA</title>")
	})
	err := cmdAPI(e, []string{"GET", "/apps"})
	if err == nil {
		t.Fatal("an HTML answer was accepted as JSON")
	}
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
	if strings.Contains(err.Error(), "upgrade toft") {
		t.Errorf("the refusal blames the client's version for the caller's path: %q",
			err.Error())
	}
	for _, want := range []string{"/apps", "not an API route", "/api/v1/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %q", want, err.Error())
		}
	}
}

// A refusal's body is the install's JSON on stdout, exactly once — no client
// envelope beside it under --json — and the exit code still says it was refused.
// The install's hint, already inside its sentence, is not printed a second time.
func TestAPIRefusalWritesTheInstallsBodyToStdout(t *testing.T) {
	const body = `{"error":"no route GET /api/v1/nope on this install — paths are relative to /api/v1/",` +
		`"code":"route_not_found","retryable":false,"hint":"paths are relative to /api/v1/"}`
	for _, asJSON := range []bool{false, true} {
		e, stdout, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, body)
		})
		args := []string{"GET", "nope"}
		if asJSON {
			args = append(args, "--json")
		}
		if got := report(e, cmdAPI(e, args)); got != exitUsage {
			t.Errorf("exit %d, want %d", got, exitUsage)
		}
		if doc := oneDocument(t, stdout); doc["code"] != "route_not_found" || doc["hint"] == nil {
			t.Errorf("stdout is not the install's body: %v", doc)
		}
		if n := strings.Count(stderr.String(), "relative to /api/v1/"); n != 1 {
			t.Errorf("the hint was said %d times:\n%s", n, stderr)
		}
	}
}

// BB-003: a signal ends a read of stdin that never sees EOF.
func TestAStdinReadGivesWayToACancel(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() { _, err := readAllCtx(ctx, r); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read outlived its cancel")
	}
	if _, err := readLineCtx(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("line read ended with %v", err)
	}
}
