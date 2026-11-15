package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// envInstall is a fake install whose env route answers a scripted result and
// records every request, so a test can see the body sent and that nothing was
// read first.
type envInstall struct {
	requests []string
	body     map[string]any
	answer   string
	out      interface {
		String() string
		Reset()
	}
}

func (fi *envInstall) env(t *testing.T) *env {
	t.Helper()
	e, out := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		fi.requests = append(fi.requests, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apps/app-1":
			fmt.Fprint(w, `{"env_keys":["API_KEY","DB_PASSWORD","MODE"],"env_vars":[`+
				`{"key":"MODE","value":"prod","sensitive":false},{"key":"API_KEY","sensitive":true},`+
				`{"key":"DB_PASSWORD","sensitive":true,"unset":true}]}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/apps/app-1/env":
			fi.body = nil
			json.NewDecoder(r.Body).Decode(&fi.body)
			fmt.Fprint(w, fi.answer)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	fi.out = out
	return e
}

func (fi *envInstall) set(t *testing.T, key string) map[string]any {
	t.Helper()
	set, _ := fi.body["set"].(map[string]any)
	one, _ := set[key].(map[string]any)
	return one
}

// One write, naming only what changes; no read of the environment before it.
func TestEnvSetSendsOnlyWhatChanges(t *testing.T) {
	fi := &envInstall{answer: `{"set":["DEBUG"],"kept":[],"cleared":[],"unset":[],"not_present":[],"restart_needed":true}`}
	e := fi.env(t)
	if err := cmdEnv(e, []string{"set", "DEBUG=a=b", "--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fi.out.String(), "restart_needed") {
		t.Errorf("--json speaks of a restart, which is toft restart's: %s", fi.out.String())
	}
	if one := fi.set(t, "DEBUG"); one["value"] != "a=b" || one["sensitive"] != nil {
		t.Errorf("body = %v, want DEBUG alone with no flag (the install makes a new row visible)", fi.body)
	}
	if set := fi.body["set"].(map[string]any); len(set) != 1 {
		t.Errorf("body names more than the change: %v", fi.body)
	}
	if slices.Contains(fi.requests, "GET /api/v1/apps/app-1") {
		t.Errorf("read the environment before writing: %v", fi.requests)
	}
}

func TestEnvSetFlags(t *testing.T) {
	fi := &envInstall{answer: `{"set":["A"],"restart_needed":true}`}
	e := fi.env(t)
	for _, tc := range []struct {
		flag string
		want any
	}{{"--sensitive", true}, {"--visible", false}} {
		if err := cmdEnv(e, []string{"set", "--app", "demo", tc.flag, "A=1"}); err != nil {
			t.Fatal(err)
		}
		if got := fi.set(t, "A")["sensitive"]; got != tc.want {
			t.Errorf("%s sent sensitive=%v", tc.flag, got)
		}
	}
	if err := cmdEnv(e, []string{"set", "--app", "demo", "--sensitive", "--visible", "A=1"}); !errors.As(err, new(*usageError)) {
		t.Errorf("--sensitive --visible = %v, want usage", err)
	}
	if err := cmdEnv(e, []string{"set", "--app", "demo", "--only-missing", "A=1"}); err != nil {
		t.Fatal(err)
	}
	if fi.body["only_missing"] != true {
		t.Errorf("--only-missing sent %v", fi.body)
	}
	if err := cmdEnv(e, []string{"unset", "--app", "demo", "--only-missing", "A"}); !errors.As(err, new(*usageError)) {
		t.Errorf("unset --only-missing = %v, want usage", err)
	}
}

// A file and stdin both work; operands win over the file; a malformed file
// sends nothing and names the line.
func TestEnvSetFromFile(t *testing.T) {
	fi := &envInstall{answer: `{"set":["A","B"],"kept":["C"],"restart_needed":true}`}
	e := fi.env(t)
	path := t.TempDir() + "/prod.env"
	if err := writeFile(path, "# comment\nA=from-file\nB=\"quoted value\"\nexport C=3\n"); err != nil {
		t.Fatal(err)
	}
	if err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", path, "A=operand"}); err != nil {
		t.Fatal(err)
	}
	if fi.set(t, "A")["value"] != "operand" || fi.set(t, "B")["value"] != "quoted value" || fi.set(t, "C")["value"] != "3" {
		t.Errorf("body = %v", fi.body)
	}

	e.stdin = strings.NewReader("TOKEN=from-stdin\n")
	if err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", "-", "--sensitive"}); err != nil {
		t.Fatal(err)
	}
	if one := fi.set(t, "TOKEN"); one["value"] != "from-stdin" || one["sensitive"] != true {
		t.Errorf("stdin body = %v", fi.body)
	}

	fi.body = nil
	sent := len(fi.requests)
	if err := writeFile(path, "A=1\nthis is not a line\n"); err != nil {
		t.Fatal(err)
	}
	err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", path})
	if err == nil || !strings.Contains(err.Error(), `line 2 has no KEY= in it`) || strings.Contains(err.Error(), "this is not") {
		t.Fatalf("malformed file = %v, want line 2 named and its text left out", err)
	}
	for _, r := range fi.requests[sent:] {
		if strings.HasPrefix(r, "PATCH") {
			t.Fatalf("a malformed file sent %s", r)
		}
	}
}

// The answer is the install's: kept and cleared are reported as such.
func TestEnvSetReportsKeptAndCleared(t *testing.T) {
	fi := &envInstall{answer: `{"set":["A"],"kept":["B"],"cleared":["SECRET"],"restart_needed":true}`}
	e := fi.env(t)
	out := fi.run(t, e, "set", "A=1", "B=2", "SECRET=", "--only-missing")
	if !strings.Contains(out, "A set; SECRET cleared on demo") || !strings.Contains(out, "B kept (already set)") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, "toft restart") {
		t.Errorf("a change should name toft restart: %q", out)
	}
}

func TestEnvUnsetAndList(t *testing.T) {
	fi := &envInstall{answer: `{"unset":["MODE"],"not_present":["NOPE"],"restart_needed":true}`}
	e := fi.env(t)
	out := fi.run(t, e, "unset", "MODE", "NOPE")
	if got := fi.body["unset"].([]any); len(got) != 2 {
		t.Errorf("unset body = %v", fi.body)
	}
	if !strings.Contains(out, "MODE removed") || strings.Contains(out, "NOPE removed") {
		t.Errorf("unset of one present, one absent said %q", out)
	}

	fi.answer = `{"unset":[],"not_present":["NOPE"],"restart_needed":false}`
	if err := cmdEnv(e, []string{"unset", "--app", "demo", "NOPE"}); !errors.As(err, new(*usageError)) {
		t.Errorf("unsetting nothing = %v, want usage", err)
	}

	out = fi.run(t, e, "list", "--json")
	var list []map[string]any
	if err := unmarshalNDJSON([]byte(out), &list); err != nil || len(list) != 3 {
		t.Fatalf("list = %q", out)
	}
	if list[0]["key"] != "API_KEY" || list[0]["sensitive"] != true || list[0]["unset"] != nil ||
		list[1]["key"] != "DB_PASSWORD" || list[1]["unset"] != true || list[2]["value"] != "prod" {
		t.Errorf("list = %v", list)
	}
	if out := fi.run(t, e, "list"); !strings.Contains(out, "DB_PASSWORD  not set") {
		t.Errorf("list = %q, want an empty secret said not set", out)
	}
}

// run runs one `toft env` command against the app and returns what it
// printed.
func (fi *envInstall) run(t *testing.T, e *env, args ...string) string {
	t.Helper()
	fi.out.Reset()
	if err := cmdEnv(e, append(args, "--app", "demo")); err != nil {
		t.Fatalf("toft env %v: %v", args, err)
	}
	return fi.out.String()
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }

// --restart was removed; it names toft restart rather than reading as an
// unknown flag (drills 2026-10-30).
func TestEnvSetRestartNamesTheCommand(t *testing.T) {
	fi := &envInstall{answer: `{"set":["X"]}`}
	err := cmdEnv(fi.env(t), []string{"set", "--app", "demo", "X=1", "--restart"})
	var u *usageError
	if !errors.As(err, &u) || !strings.Contains(err.Error(), "toft restart") {
		t.Fatalf("err = %v, want a usage refusal naming toft restart", err)
	}
	if len(fi.requests) != 0 {
		t.Errorf("sent %v, want nothing", fi.requests)
	}
}

// A dotenv file's $ is expansion, and a secret with one in it was stored
// altered with no error; the file form refuses such a value and names the
// literal forms (code review 2026-11-11, E-7).
func TestEnvSetFromFileRefusesAValueTheParserWouldRewrite(t *testing.T) {
	fi := &envInstall{answer: `{"set":["DB_PASSWORD"],"kept":[],"restart_needed":true}`}
	e := fi.env(t)
	e.stdin = strings.NewReader("DB_PASSWORD=aB3$Kq9\n")
	err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", "-", "--sensitive"})
	if err == nil || !strings.Contains(err.Error(), "single quotes") {
		t.Fatalf("err = %v, want a refusal naming single quotes", err)
	}
	if len(fi.requests) != 0 {
		t.Fatal("something was sent")
	}
	e.stdin = strings.NewReader("DB_PASSWORD='aB3$Kq9'\nOTHER=\"plain\"\n")
	if err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", "-", "--sensitive"}); err != nil {
		t.Fatal(err)
	}
	if fi.set(t, "DB_PASSWORD")["value"] != "aB3$Kq9" {
		t.Errorf("body = %v", fi.body)
	}

	// Every form godotenv reads, not only KEY=VALUE on one line, and no value
	// in the refusal (security review 2026-11-15, CL-1).
	for _, file := range []string{
		"DB_PASSWORD=aB3$Kq9\n",
		"K: a$Bc\n",
		"URL: x://u:p$X@h/db?m=1\n",
		"export\tK=a$Bc\n",
		"K=\"l1\nm$Xy\"\n",
		"A=\"x\" K=c$Dd\n",
	} {
		fi.requests = nil
		e.stdin = strings.NewReader(file)
		err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", "-", "--sensitive"})
		if err == nil || !strings.Contains(err.Error(), "single quotes") {
			t.Errorf("%q: err = %v, want a refusal naming single quotes", file, err)
			continue
		}
		for _, secret := range []string{"aB3", "Bc", "p$X", "Xy", "Dd", "u:p"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%q: the refusal quotes the value: %v", file, err)
			}
		}
		if len(fi.requests) != 0 {
			t.Errorf("%q: something was sent", file)
		}
	}
	for _, file := range []string{"K='a$Bc'\n", "K: 'a$Bc'\n"} {
		e.stdin = strings.NewReader(file)
		if err := cmdEnv(e, []string{"set", "--app", "demo", "--from-file", "-"}); err != nil {
			t.Errorf("%q: %v", file, err)
			continue
		}
		if got := fi.set(t, "K")["value"]; got != "a$Bc" {
			t.Errorf("%q: sent %v", file, got)
		}
	}

	e.stdin = strings.NewReader("API-KEY=sk_live_x\n")
	err = cmdEnv(e, []string{"set", "--app", "demo", "--from-file", "-"})
	if err == nil || strings.Contains(err.Error(), "sk_live_x") || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("bad line = %v, want line 1 named and no value", err)
	}
}
