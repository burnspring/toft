package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

func init() {
	// A wait that moves through queued → running → an ending reads the run
	// once a step; two seconds a step is the client's cadence, not a test's.
	jobPollInterval = time.Millisecond
}

// **Every ending but success exits 1, and each word survives into the output.**
//
// Several statuses onto one code, because exit 1 is already "the platform did
// the thing and it failed". The distinctions live on the last line, where a
// person reads them, rather than in further exit codes a pipeline would have
// to learn. `skipped` is among them: a run the overlap rule stood down did no
// work.
func TestJobRunExitCodes(t *testing.T) {
	cases := []struct {
		status string
		want   int
	}{
		{"succeeded", exitOK},
		{"failed", exitFailed},
		{"cancelled", exitFailed},
		{"timed_out", exitFailed},
		{"out_of_memory", exitFailed},
		{"skipped", exitFailed},
	}

	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: tc.status}))
			err := cmdJob(e, []string{"run", "nightly", "--app", "demo"})
			if got := report(e, err); got != tc.want {
				t.Errorf("a %s run exited %d, want %d", tc.status, got, tc.want)
			}
			if !strings.Contains(buf.String(), tc.status) {
				t.Errorf("output %q does not contain %q", buf.String(), tc.status)
			}
		})
	}
}

// **The mode a pipeline uses must not be the mode that loses the failure.**
func TestJobRunJSONExitsOnTheRunsEnding(t *testing.T) {
	for _, status := range []string{"failed", "cancelled", "timed_out", "out_of_memory"} {
		t.Run(status, func(t *testing.T) {
			e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: status}))
			err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--json"})
			if got := report(e, err); got != exitFailed {
				t.Errorf("a %s run with --json exited %d, want %d", status, got, exitFailed)
			}
			// The run document comes first, a line of its own, and is the
			// install's whole run.
			first := strings.SplitN(buf.String(), "\n", 2)[0]
			var doc map[string]any
			if err := json.Unmarshal([]byte(first), &doc); err != nil || doc["status"] != status {
				t.Errorf("the run document was not printed first: %q", buf.String())
			}
			// And the refusal after it names the ending as its code, like
			// every other refusal envelope.
			var refusal map[string]any
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if strings.HasPrefix(line, `{"code"`) || strings.Contains(line, `"kind"`) {
					_ = json.Unmarshal([]byte(line), &refusal)
				}
			}
			if refusal["code"] != status || refusal["kind"] != "failed" {
				t.Errorf("the refusal = %v, want code %s", refusal, status)
			}
		})
	}
}

func TestJobRunJSONStillExitsZeroOnSuccess(t *testing.T) {
	e, _ := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "succeeded"}))
	err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--json"})
	if got := report(e, err); got != exitOK {
		t.Errorf("a succeeded run with --json exited %d, want %d", got, exitOK)
	}
}

// The two open states are not endings, and a wait must not stop on one and
// call it an answer — nor on a word this binary does not know.
func TestJobRunTerminalSet(t *testing.T) {
	for _, s := range []string{"queued", "running"} {
		if terminal, _ := jobRunTerminal(s); terminal {
			t.Errorf("%q was treated as an ending", s)
		}
	}
	for _, s := range []string{"succeeded", "failed", "cancelled", "timed_out", "out_of_memory", "skipped"} {
		if terminal, _ := jobRunTerminal(s); !terminal {
			t.Errorf("%q was not treated as an ending", s)
		}
	}
	if terminal, _ := jobRunTerminal("evicted"); terminal {
		t.Error("an unknown status was treated as an ending")
	}
}

// **Parameters are opaque.** Values are strings and are not parsed, coerced or
// quoted — a test that pins the *absence* of shell semantics.
func TestParamsAreOpaque(t *testing.T) {
	var p paramList
	if err := p.Set("a=b=c"); err != nil {
		t.Fatalf("--param a=b=c: %v", err)
	}
	if got := p.values()["a"]; got != "b=c" {
		t.Errorf("--param a=b=c sent %q, want %q — the split is on the first = only", got, "b=c")
	}

	p = nil
	for _, kv := range []string{"cmd=$(whoami)", "q=*", "s=a b; rm -rf /"} {
		if err := p.Set(kv); err != nil {
			t.Fatalf("Set(%q): %v", kv, err)
		}
	}
	vals := p.values()
	if vals["cmd"] != "$(whoami)" || vals["q"] != "*" || vals["s"] != "a b; rm -rf /" {
		t.Errorf("a value was interpreted rather than carried: %v", vals)
	}

	if err := p.Set("nokey"); err == nil {
		t.Error("--param nokey was accepted")
	}
	e, _ := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "succeeded"}))
	err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--param", "nokey"})
	if got := report(e, err); got != exitUsage {
		t.Errorf("a malformed --param exited %d, want %d", got, exitUsage)
	}
}

// **Parameters never reach a command string.** The job is named by the route,
// and the request body is params as an object, scheduled_for, and nothing that
// concatenates a value into anything.
func TestJobRunSendsWhatItWasGiven(t *testing.T) {
	opts := &jobFakeOpts{runStatus: "succeeded"}
	e, _ := fakeInstall(t, jobFake(t, opts))
	err := cmdJob(e, []string{"run", "nightly", "--app", "demo",
		"--param", "rows=500", "--param", "mode=full",
		"--for", "2026-10-06T03:00:00+01:00", "--idempotency-key", "nightly-2026-10-06"})
	if err != nil {
		t.Fatalf("cmdJob run: %v", err)
	}
	if opts.startPath != "/api/v1/apps/app-1/jobs/nightly/runs" {
		t.Errorf("started at %q", opts.startPath)
	}
	params, ok := opts.startBody["params"].(map[string]any)
	if !ok {
		t.Fatalf("params is not an object: %#v", opts.startBody["params"])
	}
	if params["rows"] != "500" || params["mode"] != "full" {
		t.Errorf("params did not travel verbatim: %#v", params)
	}
	if opts.startBody["scheduled_for"] != "2026-10-06T03:00:00+01:00" {
		t.Errorf("scheduled_for = %#v", opts.startBody["scheduled_for"])
	}
	if opts.idemKey != "nightly-2026-10-06" {
		t.Errorf("Idempotency-Key = %q", opts.idemKey)
	}
	for k, v := range opts.startBody {
		if k == "params" {
			continue
		}
		if s, ok := v.(string); ok && (strings.Contains(s, "500") || strings.Contains(s, "full")) {
			t.Errorf("field %q contains a parameter value: %q", k, s)
		}
	}
}

// Each refusal of a start maps onto the exit contract and prints its hint —
// the command to paste — on stderr and in the --json document.
func TestJobStartRefusalsMapOntoTheExitContract(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       int
		kind       string
	}{
		{"param_required", `{"error":"this job requires the parameter \"region\"","code":"param_required","retryable":false,"hint":"toft job run nightly --param region=emea"}`,
			400, exitUsage, "usage"},
		{"param_choice", `{"error":"parameter \"region\" must be one of emea, amer","code":"param_choice","retryable":false,"hint":"toft job run nightly --param region=emea"}`,
			400, exitUsage, "usage"},
		{"no_such_job", `{"error":"this app's toft.toml declares no job called \"nightly\"","code":"no_such_job","retryable":false,"hint":"declare [jobs.nightly] in toft.toml and deploy; toft job list shows the jobs this app has"}`,
			404, exitUsage, "usage"},
		{"queue_full", `{"error":"this app's queue is full","code":"queue_full","retryable":true,"retry_after":30,"hint":"toft job runs --app <app> shows what is queued"}`,
			429, exitFailed, "failed"},
		{"jobs_unavailable", `{"error":"background jobs are switched off on this install","code":"jobs_unavailable","hint":"ask an admin to turn on background jobs in System settings"}`,
			422, exitAuth, "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
					fmt.Fprint(w, appRow("running"))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					fmt.Fprint(w, `{"jobs":[]}`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--json"})
			if got := report(e, err); got != tc.want {
				t.Errorf("exit %d, want %d", got, tc.want)
			}
			var hint map[string]any
			json.Unmarshal([]byte(tc.body), &hint)
			out := buf.String()
			if !strings.Contains(strings.ToLower(out), strings.ToLower(hint["hint"].(string))) {
				t.Errorf("the hint was not printed: %s", out)
			}
			for _, want := range []string{`"code":"` + tc.name + `"`, `"kind":"` + tc.kind + `"`} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal lost %s: %s", want, out)
				}
			}
			if tc.name == "queue_full" && !strings.Contains(out, `"retryable":true`) {
				t.Errorf("a full queue is the retryable one: %s", out)
			}
		})
	}
}

// `toft job list` leads with the venue and where runs go, and each job says
// when it runs, its params, and the limits that will stop a run — each with
// where the figure came from.
func TestJobListShowsScheduleParamsAndLimitsWithTheirSource(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, appRow("running"))
			return
		}
		fmt.Fprint(w, `{"placement":"cluster","jobs_available":true,"jobs":[
			{"id":"j1","name":"nightly-report","schedule":"CRON_TZ=Europe/London 0 3 * * *","overlap":"skip",
			 "notify":"failures","enabled":true,"next_run_at":"2026-10-23T02:00:00Z",
			 "params_spec":{"region":{"type":"string","required":true,"help":"Which region","choices":["emea","amer"]},
			                "rows":{"type":"int","default":500}},
			 "limits":{"timeout":{"value":900,"source":"clamped","declared":2700},"memory":{"value":1024,"source":"declared"}}},
			{"id":"j2","name":"reindex","enabled":true,
			 "limits":{"timeout":{"value":3600,"source":"install default"},"memory":{"value":512,"source":"app limit"}}},
			{"id":"j3","name":"digest","schedule":"0 6 * * *","enabled":false,"paused":true,
			 "limits":{"timeout":{"value":2700,"source":"declared"},"memory":{"value":512,"source":"install default"}}}]}`)
	})
	if err := cmdJob(e, []string{"list", "--app", "demo"}); err != nil {
		t.Fatalf("cmdJob list: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"process of its own", "Runs go to the cluster",
		"0 3 * * * (Europe/London)", "next ", "overlap skip", "notify failures",
		"timeout 15m (clamped by admin, declared 45m)", "memory 1024 MB (declared)",
		"timeout 1h (install default)", "memory 512 MB (app limit)", "timeout 45m (declared)",
		"--param region=<string>  required, one of emea|amer — Which region",
		"--param rows=<int>  default 500",
		"paused until next deploy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("toft job list does not say %q:\n%s", want, out)
		}
	}
}

// An install with no jobs answers the list with an empty one and a sentence
// saying why — not "this app declares none", which is a different fact.
func TestJobListSaysWhenTheInstallOffersNoJobs(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, appRow("running"))
			return
		}
		fmt.Fprint(w, `{"jobs":[],"jobs_available":false,`+
			`"unavailable_bec":"This install has no isolated backend configured."}`)
	})
	if err := cmdJob(e, []string{"list", "--app", "demo"}); err != nil {
		t.Fatalf("cmdJob list: %v", err)
	}
	if !strings.Contains(buf.String(), "no isolated backend") {
		t.Errorf("the install's own reason is missing: %q", buf.String())
	}
}

// `toft job run` with no job is the menu: every job, what its params take, and
// the command that starts one.
func TestJobRunWithNoNameListsJobsAndTheirParams(t *testing.T) {
	e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "succeeded", jobs: regionJob}))
	if err := cmdJob(e, []string{"run", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"nightly", "region=<string>  required, one of emea|amer|apac — Which region",
		"Run one: toft job run nightly --param region=emea"} {
		if !strings.Contains(out, want) {
			t.Errorf("the menu does not say %q:\n%s", want, out)
		}
	}
}

const regionJob = `{"jobs":[{"id":"j1","name":"nightly","enabled":true,"params_spec":{
	"region":{"type":"string","required":true,"help":"Which region","choices":["emea","amer","apac"]},
	"rows":{"type":"int","default":500}}}],"jobs_available":true}`

// **A missing required parameter is asked for when a person is there** — on
// every stream, since it is an input program. The question uses the
// declaration's help and offers its choices; what was given by flag is not
// asked; and a terminal that closes before an answer is param_required.
func TestJobRunPromptsForMissingRequiredParams(t *testing.T) {
	asking := func(opts *jobFakeOpts, keys string) (*env, *lockedBuffer) {
		e, _, stderr, _ := liveInstall(t, jobFake(t, opts))
		ui.SetProgramHook(nil)
		e.stdinTerminal = func() bool { return true }
		e.stdin = strings.NewReader(keys)
		return e, stderr
	}

	opts := &jobFakeOpts{runStatus: "succeeded", jobs: regionJob}
	keys, typed := io.Pipe()
	e, screen := asking(opts, "")
	e.stdin = keys
	go func() {
		// Typed once the choices are on screen, as a person would.
		for !strings.Contains(screen.String(), "apac") {
			time.Sleep(5 * time.Millisecond)
		}
		typed.Write([]byte("\x1b[B\r")) // down, enter: the second choice
	}()
	err := cmdJob(e, []string{"run", "nightly", "--app", "demo"})
	typed.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := ansi.Strip(screen.String())
	for _, want := range []string{"region — Which region", "emea", "amer", "apac"} {
		if !strings.Contains(got, want) {
			t.Errorf("the question does not show %q:\n%s", want, got)
		}
	}
	params, _ := opts.startBody["params"].(map[string]any)
	if params["region"] != "amer" {
		t.Errorf("params = %#v", opts.startBody["params"])
	}
	if _, asked := params["rows"]; asked {
		t.Errorf("an optional parameter was asked for: %#v", params)
	}

	// Given by flag: nothing is asked, and stdin is not read.
	opts = &jobFakeOpts{runStatus: "succeeded", jobs: regionJob}
	e, screen = asking(opts, "")
	if err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--param", "region=emea"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screen.String(), "Which region") {
		t.Errorf("a parameter given by flag was asked for:\n%s", screen.String())
	}

	// The terminal closes before an answer: param_required, exit 2, with the
	// flag that answers it.
	e, _ = asking(&jobFakeOpts{runStatus: "succeeded", jobs: regionJob}, "")
	err = cmdJob(e, []string{"run", "nightly", "--app", "demo"})
	if got := report(e, err); got != exitUsage || refusalCode(err) != "param_required" ||
		!strings.Contains(err.Error(), "--param region=emea") {
		t.Errorf("exit %d, code %q, err %v", got, refusalCode(err), err)
	}
}

// **A piped stdout is not asked, even with a person on stdin** (D11): the
// install refuses with param_required and the command that would have worked,
// which is the answer a pipe should get.
func TestJobRunDoesNotPromptIntoAPipe(t *testing.T) {
	opts := &jobFakeOpts{runStatus: "succeeded", jobs: regionJob}
	e, _ := fakeInstall(t, jobFake(t, opts))
	e.stdinTerminal = func() bool { return true }
	e.stdin = strings.NewReader("emea\n")
	t.Setenv("CI", "")
	if err := cmdJob(e, []string{"run", "nightly", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if opts.startBody["params"] != nil {
		t.Errorf("something was asked for with stdout piped: %#v", opts.startBody)
	}
}

// With nobody at the terminal, nothing is asked: the install refuses with
// param_required, which is exit 2.
func TestJobRunDoesNotPromptWithoutATerminal(t *testing.T) {
	opts := &jobFakeOpts{runStatus: "succeeded", jobs: regionJob}
	e, _ := fakeInstall(t, jobFake(t, opts))
	e.stdinTerminal = func() bool { return false }
	e.stdin = strings.NewReader("emea\n")
	if err := cmdJob(e, []string{"run", "nightly", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if opts.startBody["params"] != nil {
		t.Errorf("something was asked for without a terminal: %#v", opts.startBody)
	}
}

// While a run waits its turn, its place in the queue is said; while it runs,
// its progress — each once, as it changes.
func TestJobRunShowsQueuePositionAndProgress(t *testing.T) {
	opts := &jobFakeOpts{states: []string{
		`{"id":"9b21c0aa-1111","job":"nightly","status":"queued","queue_position":2}`,
		`{"id":"9b21c0aa-1111","job":"nightly","status":"queued","queue_position":2}`,
		`{"id":"9b21c0aa-1111","job":"nightly","status":"queued","queue_position":1}`,
		`{"id":"9b21c0aa-1111","job":"nightly","status":"running","progress":40,"progress_message":"loading rows"}`,
		`{"id":"9b21c0aa-1111","job":"nightly","status":"succeeded"}`,
	}}
	e, buf := fakeInstall(t, jobFake(t, opts))
	if err := cmdJob(e, []string{"run", "nightly", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"9b21c0aa  queued · 2nd", "9b21c0aa  queued · 1st", "running · 40% loading rows", "9b21c0aa  succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("the wait did not say %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "queued · 2nd") != 1 {
		t.Errorf("an unchanged position was said twice:\n%s", out)
	}
}

// --follow streams the run's log while it waits.
func TestJobRunFollowStreamsTheLog(t *testing.T) {
	e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "succeeded", logLines: []string{"loading 500 rows"}}))
	if err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--follow"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "loading 500 rows") {
		t.Errorf("--follow did not stream the log: %s", buf.String())
	}
}

// --timeout gives up on the wait and not on the run, and names the command
// that picks the wait back up.
func TestJobRunTimeoutNamesTheWait(t *testing.T) {
	e, _ := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "running"}))
	err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--timeout", "20ms"})
	if err == nil || !strings.Contains(err.Error(), "toft job wait run-1") {
		t.Errorf("err = %v", err)
	}
}

// --no-wait is exit 0 with the run id: the run was accepted, which is all that
// is being asserted.
func TestJobRunNoWaitPrintsTheRunID(t *testing.T) {
	e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "running"}))
	if err := cmdJob(e, []string{"run", "nightly", "--app", "demo", "--no-wait"}); err != nil {
		t.Fatalf("--no-wait: %v", err)
	}
	if !strings.Contains(buf.String(), "run-1") {
		t.Errorf("--no-wait did not print the run id: %q", buf.String())
	}
}

// A failed run prints its tail unasked, its reason in words, and the next
// command.
func TestAFailedJobRunShowsItsLogAndReason(t *testing.T) {
	e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{
		states: []string{`{"id":"9b21c0aa-2222","job":"nightly","status":"failed","exit_code":3,` +
			`"failure_reason":"exit 3: connection refused"}`},
		logLines: []string{"Traceback (most recent call last):", "KeyError: 'rows'"},
	}))
	err := cmdJob(e, []string{"run", "nightly", "--app", "demo"})
	if got := report(e, err); got != exitFailed {
		t.Fatalf("a failed run exited %d", got)
	}
	out := buf.String()
	for _, want := range []string{"KeyError", "failed — exit 3: connection refused", "toft job rerun 9b21c0aa"} {
		if !strings.Contains(out, want) {
			t.Errorf("a failed run did not say %q:\n%s", want, out)
		}
	}
}

// `toft job rerun` starts a new run of the one named, says which it reruns,
// and waits for it like `toft job run`.
func TestJobRerunStartsAndWaits(t *testing.T) {
	opts := &jobFakeOpts{states: []string{`{"id":"77d4e810-3333","job":"nightly","status":"failed","rerun_of":"9b21c0aa-2222"}`}}
	e, buf := fakeInstall(t, jobFake(t, opts))
	err := cmdJob(e, []string{"rerun", "9b21c0aa", "--app", "demo"})
	if got := report(e, err); got != exitFailed {
		t.Errorf("a failed rerun exited %d", got)
	}
	if opts.rerunPath != "/api/v1/apps/app-1/job-runs/9b21c0aa/rerun" {
		t.Errorf("rerun at %q", opts.rerunPath)
	}
	if !strings.Contains(buf.String(), "a rerun of 9b21c0aa") {
		t.Errorf("the rerun did not name what it reruns:\n%s", buf.String())
	}

	// --no-wait under --json is the run, rerun_of and all.
	e, buf = fakeInstall(t, jobFake(t, &jobFakeOpts{states: opts.states}))
	if err := cmdJob(e, []string{"rerun", "9b21c0aa", "--app", "demo", "--no-wait", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"rerun_of":"9b21c0aa-2222"`) {
		t.Errorf("json = %s", buf.String())
	}
	// deduplicated, as toft job run always carries it (L7).
	if !strings.Contains(buf.String(), `"deduplicated":false`) {
		t.Errorf("json = %s, want deduplicated", buf.String())
	}
}

// A rerun of a run still going is refused: exit 3, with the wait as the hint.
func TestJobRerunOfAnOpenRunIsRefused(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, appRow("running"))
			return
		}
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"run 9b21c0aa has not finished","code":"run_still_open","retryable":false,"hint":"toft job wait 9b21c0aa"}`)
	})
	err := cmdJob(e, []string{"rerun", "9b21c0aa", "--app", "demo"})
	if got := report(e, err); got != exitAuth {
		t.Errorf("exit %d, want %d", got, exitAuth)
	}
	if !strings.Contains(buf.String(), "    toft job wait 9b21c0aa") {
		t.Errorf("the hint was not printed: %s", buf.String())
	}
}

// `toft job wait` waits for a run it did not start, with the same exit code.
func TestJobWaitExitsOnTheOutcome(t *testing.T) {
	for status, want := range map[string]int{"succeeded": exitOK, "timed_out": exitFailed, "out_of_memory": exitFailed} {
		t.Run(status, func(t *testing.T) {
			opts := &jobFakeOpts{runStatus: status}
			e, buf := fakeInstall(t, jobFake(t, opts))
			err := cmdJob(e, []string{"wait", "run-1", "--app", "demo"})
			if got := report(e, err); got != want {
				t.Errorf("exit %d, want %d", got, want)
			}
			if opts.started {
				t.Error("wait started a run")
			}
			if !strings.Contains(buf.String(), status) {
				t.Errorf("output %q", buf.String())
			}
		})
	}
}

// **A name that does not resolve is exit 2, in every verb that takes one.**
func TestAnUnresolvableNameIsUsage(t *testing.T) {
	notFound := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, appRow("running"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such run","code":"not_found","hint":"toft job runs lists this app's runs"}`)
	}
	for _, args := range [][]string{
		{"wait", "nope", "--app", "demo"},
		{"rerun", "nope", "--app", "demo"},
		{"logs", "nope", "--app", "demo"},
		{"cancel", "nope", "--app", "demo"},
	} {
		t.Run(args[0], func(t *testing.T) {
			e, buf := fakeInstall(t, notFound)
			err := cmdJob(e, args)
			if got := report(e, err); got != exitUsage {
				t.Errorf("toft job %s exited %d, want %d (usage)", args[0], got, exitUsage)
			}
			if !strings.Contains(buf.String(), "    toft job runs lists") {
				t.Errorf("the hint was dropped: %s", buf.String())
			}
		})
	}
}

// runsFake serves a list of runs in every state and the jobs that own them.
func runsFake(t *testing.T) http.HandlerFunc {
	started := time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339)
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `{"jobs":[{"id":"j1","name":"nightly-report"},{"id":"j2","name":"weekly"}]}`)
		case strings.HasSuffix(r.URL.Path, "/job-runs"):
			if j := r.URL.Query().Get("job"); j == "weekly" {
				fmt.Fprint(w, `{"runs":[]}`)
				return
			}
			fmt.Fprintf(w, `{"runs":[
				{"id":"6ffbcf44-aaaa","job":"nightly-report","status":"succeeded","started_by":"schedule",
				 "queued_at":"2026-10-23T02:00:00Z","started_at":"2026-10-23T02:00:00Z","finished_at":"2026-10-23T02:12:00Z"},
				{"id":"9b21c0aa-bbbb","job":"nightly-report","status":"failed","started_by":"app","exit_code":3,
				 "failure_reason":"exit 3, connection refused","queued_at":"2026-10-23T01:00:00Z","peak_memory_mb":612,"future_field":"kept"},
				{"id":"0c3e9a12-cccc","job":"nightly-report","status":"running","started_by":"viewer","progress":40,
				 "progress_message":"loading rows","queued_at":"2026-10-23T08:14:00Z","started_at":%q},
				{"id":"77d4e810-dddd","job":"nightly-report","status":"queued","started_by":"cli","queue_position":2,
				 "queued_at":"2026-10-23T08:15:00Z"},
				{"id":"5e5e5e5e-eeee","job":"nightly-report","status":"skipped","started_by":"schedule",
				 "failure_reason":"the previous run was still going","queued_at":"2026-10-22T02:00:00Z"}]}`, started)
		case strings.Contains(r.URL.Path, "/job-runs/9b21c0aa"):
			fmt.Fprint(w, `{"id":"9b21c0aa-bbbb","job":"nightly-report","status":"failed","started_by":"app",
				"params":{"region":"emea","rows":500},"exit_code":3,"failure_reason":"exit 3, connection refused",
				"peak_memory_mb":612,"queued_at":"2026-10-23T01:00:00Z"}`)
		case strings.Contains(r.URL.Path, "/job-runs/"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"no such run","code":"not_found"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}
}

// `toft job runs` is one row per run with its reason in words — and a running
// run's progress, a queued one's place, a skipped one too.
func TestJobRunsRowsSayWhy(t *testing.T) {
	e, buf := fakeInstall(t, runsFake(t))
	if err := cmdJob(e, []string{"runs", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"exit 3, connection refused — toft job rerun 9b21c0aa",
		"running · 4m · 40% loading rows",
		"queued · 2nd",
		"5e5e5e5e  nightly-report  skipped",
		"the previous run was still going",
		"  12m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the runs do not say %q:\n%s", want, out)
		}
	}
	// A row names who started it, in the platform's word.
	for _, by := range []string{" app ", " viewer ", " cli ", " schedule "} {
		if !strings.Contains(out, by) {
			t.Errorf("no row says %q:\n%s", by, out)
		}
	}
	for _, gone := range []string{"attempt", "retr"} {
		if strings.Contains(out, gone) {
			t.Errorf("the list still speaks of %q:\n%s", gone, out)
		}
	}
}

// Under --json the rows are the install's runs, every field — including one
// this binary does not know.
func TestJobRunsJSONCarriesTheFullRun(t *testing.T) {
	e, buf := fakeInstall(t, runsFake(t))
	if err := cmdJob(e, []string{"runs", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	// NDJSON: one run per line (BB-015, third 2026-11-03 list).
	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a line is not one JSON document: %v\n%s", err, buf.String())
		}
		got = append(got, r)
	}
	if len(got) != 5 {
		t.Fatalf("rows = %d", len(got))
	}
	var failed map[string]any
	for _, r := range got {
		if r["status"] == "failed" {
			failed = r
		}
	}
	if failed["peak_memory_mb"] != float64(612) || failed["future_field"] != "kept" || failed["started_by"] != "app" {
		t.Errorf("a field was dropped: %v", failed)
	}
}

// Given a run id rather than a job, `toft job runs` is that run in full.
func TestJobRunsGivenARunIDShowsTheRun(t *testing.T) {
	e, buf := fakeInstall(t, runsFake(t))
	if err := cmdJob(e, []string{"runs", "9b21c0aa", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"region=emea rows=500", "exit code    3", "exit 3, connection refused",
		"peak memory  612 MB", "toft job logs 9b21c0aa, then toft job rerun 9b21c0aa"} {
		if !strings.Contains(out, want) {
			t.Errorf("the run does not say %q:\n%s", want, out)
		}
	}
}

// A job name narrows the list; a defined job with no runs is not an error; a
// word that is neither a job nor a run is exit 2.
func TestJobRunsNarrowsToOneJob(t *testing.T) {
	e, buf := fakeInstall(t, runsFake(t))
	if err := cmdJob(e, []string{"runs", "nightly-report", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "6ffbcf44") {
		t.Errorf("output %s", buf.String())
	}

	e, _ = fakeInstall(t, runsFake(t))
	if err := cmdJob(e, []string{"runs", "--app", "demo", "weekly", "--json"}); err != nil {
		t.Errorf("a defined job with no runs is not an error: %v", err)
	}

	e, _ = fakeInstall(t, runsFake(t))
	err := cmdJob(e, []string{"runs", "nope", "--app", "demo"})
	if got := report(e, err); got != exitUsage || refusalCode(err) != "no_such_job" {
		t.Errorf("an unknown job exited %d code %q (%v)", got, refusalCode(err), err)
	}
}

func TestParseRunFor(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-06T03:00:00+01:00": "2026-10-06T02:00:00Z",
		"2026-10-06T03:00":          "2026-10-06T03:00:00Z",
		"2026-10-06T03:00-05:00":    "2026-10-06T08:00:00Z",
		"2026-10-06T03:00Z":         "2026-10-06T03:00:00Z",
		"2026-10-06":                "2026-10-06T00:00:00Z",
	} {
		got, err := parseRunFor(in)
		if err != nil {
			t.Errorf("parseRunFor(%q): %v", in, err)
			continue
		}
		if got.UTC().Format(time.RFC3339) != want {
			t.Errorf("parseRunFor(%q) = %s, want %s", in, got.UTC().Format(time.RFC3339), want)
		}
	}
	if _, err := parseRunFor("tuesday"); err == nil {
		t.Error("a word was accepted as a time")
	}
}

func TestOrdinal(t *testing.T) {
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 21: "21st", 102: "102nd"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// Cancelling a run that already finished says so — it is not reported as a
// cancel, and the JSON carries changed:false.
func TestCancellingAFinishedRunSaysItFinished(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, appRow("running"))
			return
		}
		fmt.Fprint(w, `{"id":"run-1","job":"nightly","status":"succeeded","changed":false}`)
	}
	e, buf := fakeInstall(t, handler)
	if err := cmdJob(e, []string{"cancel", "run-1", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "already finished (succeeded)") {
		t.Errorf("output %q", buf.String())
	}

	e, buf = fakeInstall(t, handler)
	if err := cmdJob(e, []string{"cancel", "run-1", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"changed":false`) || !strings.Contains(buf.String(), `"succeeded"`) {
		t.Errorf("json %s", buf.String())
	}
}

// `toft check` prints each job's effective limits with their source, a
// problem's code and hint, and a clamp as a warning of its own.
func TestCheckShowsJobLimitsProblemsAndClamps(t *testing.T) {
	e, stdout, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/me/capabilities":
			fmt.Fprint(w, `{"install":{"jobs_available":true}}`)
		default:
			fmt.Fprint(w, `{"ok":false,"framework":"flask","jobs":["nightly","weekly"],
				"job_limits":[
				  {"name":"nightly","limits":{"timeout":{"value":900,"source":"clamped","declared":2700},"memory":{"value":1024,"source":"declared"}}},
				  {"name":"weekly","limits":{"timeout":{"value":3600,"source":"install default"},"memory":{"value":512,"source":"install default"}}}],
				"warnings":["nightly's timeout of 45m is above the install's ceiling and runs at 15m"],
				"warning_details":[{"code":"limit_clamped","message":"nightly's timeout of 45m is above the install's ceiling and runs at 15m"}],
				"problems":[{"reason":"manifest_invalid","code":"removed_key","hint":"replace [tasks.digest] with [jobs.digest] and a schedule",
				  "message":"toft.toml: [tasks.digest] is no longer read"}],
				"not_checked":["dependencies"]}`)
		}
	})
	os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("x"), 0o644)
	err := cmdCheck(e, nil)
	if got := report(e, err); got != exitFailed {
		t.Errorf("a refused check exited %d", got)
	}
	for _, want := range []string{"nightly  timeout 15m (clamped by admin, declared 45m) · memory 1024 MB (declared)",
		"weekly   timeout 1h (install default) · memory 512 MB (install default)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("check does not say %q:\n%s", want, stdout)
		}
	}
	for _, want := range []string{"(limit_clamped)", "removed_key", "→ replace [tasks.digest] with [jobs.digest]"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("check does not say %q:\n%s", want, stderr)
		}
	}
	if strings.Count(stderr.String(), "runs at 15m") != 1 {
		t.Errorf("the clamp was said twice:\n%s", stderr)
	}
}

type jobFakeOpts struct {
	// runStatus is the run's status on every read, when states is empty.
	runStatus string
	// states are successive answers to GET /job-runs/{id}; the last repeats.
	states   []string
	logLines []string
	// jobs is the answer to GET /jobs.
	jobs string

	mu        sync.Mutex
	read      int
	started   bool
	startPath string
	startBody map[string]any
	idemKey   string
	rerunPath string
}

func (o *jobFakeOpts) runDoc() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.states) == 0 {
		return fmt.Sprintf(`{"id":"run-1","job":"nightly","status":%q}`, o.runStatus)
	}
	i := min(o.read, len(o.states)-1)
	o.read++
	return o.states[i]
}

// jobFake serves the routes `toft job` uses.
func jobFake(t *testing.T, opts *jobFakeOpts) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(p, "/jobs") && r.Method == http.MethodGet:
			fmt.Fprint(w, orDefault(opts.jobs, `{"jobs":[{"id":"j1","name":"nightly"}],"jobs_available":true}`))
		case strings.HasSuffix(p, "/logs"):
			// A page of at most 1000 lines after ?after, as the route answers.
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			lines := make([]string, 0, len(opts.logLines))
			for i, l := range opts.logLines {
				if i+1 <= after {
					continue
				}
				lines = append(lines, fmt.Sprintf(`{"seq":%d,"stream":"stdout","line":%q}`, i+1, l))
				if len(lines) == 1000 {
					break
				}
			}
			fmt.Fprintf(w, `{"lines":[%s],"status":%q}`, strings.Join(lines, ","), opts.runStatus)
		case strings.Contains(p, "/jobs/") && strings.HasSuffix(p, "/runs") && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			opts.mu.Lock()
			opts.started, opts.startPath, opts.idemKey = true, p, r.Header.Get("Idempotency-Key")
			json.Unmarshal(raw, &opts.startBody)
			opts.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, opts.runDoc())
		case strings.HasSuffix(p, "/rerun") && r.Method == http.MethodPost:
			opts.mu.Lock()
			opts.rerunPath = p
			opts.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, opts.runDoc())
		case strings.Contains(p, "/job-runs/"):
			fmt.Fprint(w, opts.runDoc())
		case strings.HasSuffix(p, "/job-runs"):
			fmt.Fprintf(w, `{"runs":[%s]}`, opts.runDoc())
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
		}
	}
}

// `toft job output` is the SDKs' run.output(): the bytes the job wrote, text
// as readily as JSON, read from the presigned URL without the credential —
// `toft api` on the route could only show an output that parsed as JSON
// (jobs-regression #17).
func TestJobOutputIsTheBytesFromTheStore(t *testing.T) {
	var storeAuth string
	var srvURL string
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case strings.HasSuffix(p, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(p, "/job-runs/9b21c0aa/output"):
			http.Redirect(w, r, srvURL+"/store/out.csv?sig=x", http.StatusFound)
		case strings.HasSuffix(p, "/job-runs/77d4e810/output"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"this run produced no output","code":"no_output"}`)
		case p == "/store/out.csv":
			storeAuth = r.Header.Get("Authorization")
			fmt.Fprint(w, "region,rows\nemea,500\n")
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
		}
	})
	srvURL = os.Getenv("TOFT_URL")

	if err := cmdJob(e, []string{"output", "9b21c0aa", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "region,rows\nemea,500\n") {
		t.Errorf("stdout = %q, want the bytes", buf.String())
	}
	if storeAuth != "" {
		t.Errorf("the credential was sent to the store: %q", storeAuth)
	}

	dest := filepath.Join(t.TempDir(), "out.csv")
	if err := cmdJob(e, []string{"output", "9b21c0aa", "--app", "demo", "-o", dest}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "region,rows\nemea,500\n" {
		t.Errorf("file = %q", got)
	}

	// A file already there is kept unless --force says otherwise, as
	// `toft store get` keeps one (CLI-6).
	os.WriteFile(dest, []byte("mine"), 0o644)
	var usage *usageError
	if err := cmdJob(e, []string{"output", "9b21c0aa", "--app", "demo", "-o", dest}); !errors.As(err, &usage) {
		t.Errorf("-o over an existing file = %v, want a usage refusal", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "mine" {
		t.Errorf("the existing file was replaced: %q", got)
	}
	if err := cmdJob(e, []string{"output", "9b21c0aa", "--app", "demo", "-o", dest, "--force"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "region,rows\nemea,500\n" {
		t.Errorf("--force file = %q", got)
	}

	err := cmdJob(e, []string{"output", "77d4e810", "--app", "demo"})
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "no_output" {
		t.Errorf("a run with no output = %v, want the install's no_output", err)
	}
}

// A static app refuses a [jobs] table, so its empty job list and job menu say
// so instead of advising one (docs/bugs 2026-10-28 D14) — and without first
// describing how a run goes, which the next line contradicts (drills
// 2026-10-28 #7).
func TestNoJobsOnAStaticAppDoesNotAdviseAJobsTable(t *testing.T) {
	for _, verb := range []string{"list", "run"} {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
				fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"running","framework":"static"}}`)
				return
			}
			fmt.Fprint(w, `{"jobs":[],"jobs_available":true,"placement":"this machine"}`)
		})
		if err := cmdJob(e, []string{verb, "--app", "demo"}); err != nil {
			t.Fatalf("toft job %s: %v", verb, err)
		}
		if got := buf.String(); strings.Contains(got, "add a [jobs") || !strings.Contains(got, "static app") ||
			strings.Contains(got, "A job runs as a process") {
			t.Errorf("toft job %s on a static app: %q", verb, got)
		}
	}
}

// The scheduler skips every firing of a locked app, so the list says the
// schedule is held by the lock rather than promising its next firing (D15).
func TestJobListOfALockedAppSaysTheScheduleIsHeld(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
			fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"stopped","locked_at":"2026-10-28T12:00:00Z"}}`)
			return
		}
		fmt.Fprint(w, `{"jobs":[{"id":"j1","name":"nightly","enabled":true,"schedule":"0 2 * * *",`+
			`"next_run_at":"2026-10-29T02:00:00Z"}],"jobs_available":true}`)
	})
	if err := cmdJob(e, []string{"list", "--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); strings.Contains(got, "next ") || !strings.Contains(got, "held by the app's lock") {
		t.Errorf("a locked app's job list: %q", got)
	}
}

// --json carries the hold too: held_by_lock and no next_run_at, whether the
// platform said so itself or predates the field and only the lock says it (D15).
func TestJobListJSONOfALockedAppSaysTheScheduleIsHeld(t *testing.T) {
	for name, job := range map[string]string{
		"older platform": `{"id":"j1","name":"nightly","enabled":true,"schedule":"0 2 * * *","next_run_at":"2026-10-29T02:00:00Z"}`,
		"this platform":  `{"id":"j1","name":"nightly","enabled":true,"schedule":"0 2 * * *","held_by_lock":true}`,
	} {
		e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/by-slug/demo") {
				fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"stopped","locked_at":"2026-10-28T12:00:00Z"}}`)
				return
			}
			fmt.Fprint(w, `{"jobs":[`+job+`],"jobs_available":true}`)
		})
		if err := cmdJob(e, []string{"list", "--app", "demo", "--json"}); err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		if err := unmarshalNDJSON(buf.Bytes(), &got); err != nil || len(got) != 1 {
			t.Fatalf("%s: %q: %v", name, buf.String(), err)
		}
		if _, has := got[0]["next_run_at"]; has || got[0]["held_by_lock"] != true {
			t.Errorf("%s: a locked app's job list --json: %v", name, got[0])
		}
	}
}

// A static tree's [jobs] is a refusal, so check does not tick a jobs line
// above it (docs/bugs 2026-10-28 D14).
func TestCheckOfAStaticTreeWithJobsPrintsNoJobsLine(t *testing.T) {
	e, stdout, stderr := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/me/capabilities":
			fmt.Fprint(w, `{"install":{"jobs_available":false}}`)
		default:
			fmt.Fprint(w, `{"ok":false,"framework":"static","jobs":["x"],
				"job_limits":[{"name":"x","limits":{"timeout":{"value":600,"source":"install default"},"memory":{"value":512,"source":"install default"}}}],
				"problems":[{"reason":"manifest_invalid","message":"toft.toml: declares a job (x), and a static app runs no commands"}],
				"not_checked":["dependencies"]}`)
		}
	})
	os.WriteFile(filepath.Join(e.dir, "index.html"), []byte("x"), 0o644)
	err := cmdCheck(e, nil)
	if got := report(e, err); got != exitFailed {
		t.Errorf("a refused check exited %d", got)
	}
	all := stdout.String() + stderr.String()
	if strings.Contains(all, "timeout 10m") || strings.Contains(all, "this install runs no jobs") {
		t.Errorf("a static tree's check listed its jobs above the refusal:\n%s", all)
	}
	if !strings.Contains(all, "a static app runs no commands") {
		t.Errorf("the refusal is missing:\n%s", all)
	}
}

// The logs route answers a thousand lines a page and says nothing about the
// rest, so both forms read until an empty page: a run that printed more used
// to show its first thousand and exit 0, cutting the end where the failure was
// (code review 2026-11-11, H-1).
func TestJobLogsReadsEveryPage(t *testing.T) {
	lines := make([]string, 2500)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	for _, args := range [][]string{
		{"logs", "run-1", "--app", "demo"},
		{"logs", "run-1", "--app", "demo", "--follow"},
	} {
		e, buf := fakeInstall(t, jobFake(t, &jobFakeOpts{runStatus: "failed", logLines: lines}))
		if err := cmdJob(e, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(buf.String(), "line 2500") || !strings.Contains(buf.String(), "line 1001") {
			t.Errorf("%v stopped before the end of the log (%d bytes)", args, buf.Len())
		}
	}
}
