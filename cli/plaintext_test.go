package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// What the CLI says in its own voice carries no escape an app wrote: an OSC
// in a failure message or a progress line would drive the terminal (security
// review 2026-11-10, G-1).
func TestTheCLIsOwnSentencesCarryNoEscapes(t *testing.T) {
	evil := "boom \x1b]52;c;ZXZpbA==\x07\x1b[2K\x1b[31mred\x1b[0m"
	for name, got := range map[string]string{
		"failure sentence": failureSentence("build", "", "", evil),
		"plain text":       plainText(evil),
	} {
		if strings.Contains(got, "\x1b") {
			t.Errorf("%s kept an escape: %q", name, got)
		}
		if !strings.Contains(got, "boom") || !strings.Contains(got, "red") {
			t.Errorf("%s lost the text: %q", name, got)
		}
	}
}

// The app-written fields a person reads on a terminal — a run's failure reason
// and params, an event's summary and detail, an object's name — carry no escape
// (security review 2026-11-14, CS-5).
func TestAppWrittenFieldsCarryNoEscapes(t *testing.T) {
	const evil = `\u001b]52;c;ZXZpbA==\u0007`
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			fmt.Fprint(w, `{"jobs":[{"id":"j1","name":"nightly"}]}`)
		case strings.HasSuffix(r.URL.Path, "/job-runs"):
			fmt.Fprint(w, `{"runs":[{"id":"9b21c0aa-bbbb","job":"nightly","status":"failed","started_by":"app",`+
				`"exit_code":1,"failure_reason":"exit 1: boom `+evil+` tail","queued_at":"2026-10-23T01:00:00Z"}]}`)
		case strings.Contains(r.URL.Path, "/job-runs/9b21c0aa"):
			fmt.Fprint(w, `{"id":"9b21c0aa-bbbb","job":"nightly","status":"failed","started_by":"app",`+
				`"params":{"region":"em`+evil+`ea"},"exit_code":1,"failure_reason":"exit 1: boom `+evil+` tail",`+
				`"queued_at":"2026-10-23T01:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			fmt.Fprint(w, `[{"id":"e1","kind":"build_failed","severity":"error",`+
				`"summary":"the build `+evil+` failed","detail":"log `+evil+` tail",`+
				`"occurred_at":"2026-10-23T01:00:00Z"}]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}
	for _, rich := range []bool{false, true} {
		for name, run := range map[string]func(e *env) error{
			"job runs":    func(e *env) error { return cmdJob(e, []string{"runs", "--app", "demo"}) },
			"job runs id": func(e *env) error { return cmdJob(e, []string{"runs", "9b21c0aa", "--app", "demo"}) },
			"events":      func(e *env) error { return cmdEvents(e, []string{"--app", "demo"}) },
		} {
			var e *env
			var out func() string
			if rich {
				e, out = richInstall(t, handler)
			} else {
				var buf *bytes.Buffer
				e, buf = fakeInstall(t, handler)
				out = buf.String
			}
			if err := run(e); err != nil {
				t.Fatalf("%s (rich %v): %v", name, rich, err)
			}
			got := out()
			if strings.Contains(got, "\x1b]") || strings.Contains(got, "\x07") {
				t.Errorf("%s (rich %v) kept the escape: %q", name, rich, got)
			}
			if !strings.Contains(got, "boom") && !strings.Contains(got, "the build") {
				t.Errorf("%s (rich %v) lost the words: %q", name, rich, got)
			}
		}
	}
	if got := listedName("toft://s/a\x1b]0;x\x07b"); got != "toft://s/a?]0;x?b" {
		t.Errorf("listedName = %q", got)
	}

	// Another party's text on four more surfaces: a dependency name from a
	// deployed tree's lockfile, another editor's variable, and an object name
	// in a recursive rm or cp whose items the install refuses (security review
	// 2026-11-15, CL-6).
	const hostile = `\u001b]0;x\u0007\r`
	more := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/dependencies"):
			fmt.Fprint(w, `{"available":true,"has_data":true,"refused":{"deployment_id":"dep-1",`+
				`"reason":"REFUSED `+hostile+`","findings":[{"advisory_id":"GHSA-1`+hostile+`","severity":"high",`+
				`"package":"left`+hostile+`pad","version":"1.0`+hostile+`","path":"a > b`+hostile+`",`+
				`"would_block":true}]},"waivers":[{"advisory_id":"GHSA-2","reason":"ok `+hostile+`"}]}`)
		case r.URL.Path == "/api/v1/apps/app-1":
			fmt.Fprint(w, `{"env_keys":["MODE"],"env_vars":[{"key":"MODE","value":"on`+hostile+`"}]}`)
		case r.URL.Path == "/api/v1/store":
			fmt.Fprint(w, `{"stores":[{"slug":"reports","access":"full"}]}`)
		case strings.HasSuffix(r.URL.Path, "/objects") && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"objects":[{"name":"q`+hostile+`.csv","size":1}]}`)
		case strings.Contains(r.URL.Path, "/objects/"):
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"that name holds a control character"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}
	for _, rich := range []bool{false, true} {
		cases := map[string]func(e *env) error{
			"deps":        func(e *env) error { return cmdDeps(e, []string{"--app", "demo"}) },
			"store rm -r": func(e *env) error { return cmdStore(e, []string{"rm", "-r", "--yes", "toft://reports/"}) },
			"store cp -r": func(e *env) error { return cmdStore(e, []string{"cp", "-r", "toft://reports/", t.TempDir()}) },
		}
		if rich {
			// The line form prints the stored value to a pipe, which may be
			// reading it back; only a terminal is cleaned.
			cases["env list"] = func(e *env) error { return cmdEnv(e, []string{"list", "--app", "demo"}) }
		}
		for name, run := range cases {
			var e *env
			var out func() string
			if rich {
				e, out = richInstall(t, more)
			} else {
				var buf *bytes.Buffer
				e, buf = fakeInstall(t, more)
				out = buf.String
			}
			_ = run(e) // the store items fail by design
			got := out()
			if strings.ContainsAny(got, "\x1b\x07\r") {
				t.Errorf("%s (rich %v) kept a control: %q", name, rich, got)
			}
			if !strings.Contains(got, "?]0;x?") {
				t.Errorf("%s (rich %v) did not show the name at all: %q", name, rich, got)
			}
		}
	}
}
