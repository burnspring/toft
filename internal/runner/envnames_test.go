package runner

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// appNamesStillMatter are names appEnv sets that an app's own value still
// reaches or shapes — the reason each is off PlatformEnvNames is on that list.
var appNamesStillMatter = map[string]bool{"PATH": true, "HOME": true, "LANG": true, "TZ": true,
	"PYTHONPATH": true, "NODE_ENV": true, "FLASK_APP": true}

// TestPlatformEnvNamesCoverAppEnv: every name the isolated launcher sets, in a
// service and in a job with every field given, is on the list or is one the
// app's own value still matters for.
func TestPlatformEnvNamesCoverAppEnv(t *testing.T) {
	c := &Client{baseURL: "http://platform", runToken: "rt"}
	t.Setenv(EnvStorage, `[]`)
	boots := []*BootstrapResponse{
		{AppSlug: "demo", Framework: "shiny", Port: 8080, BasePath: "/apps/demo",
			AppToken: "t", AppID: "a", RoleKey: "k"},
		{AppSlug: "demo", Framework: "python", RunID: "r", JobName: "j", Trigger: "ui",
			TriggerRole: "owner", OutputURL: "out", ScheduledFor: "now", RerunOf: "r0",
			Deadline: "2026-10-22T03:45:00Z", Params: json.RawMessage(`{}`)},
	}
	for _, mode := range []string{"serve", "job"} {
		for _, boot := range boots {
			for _, kv := range appEnv(boot, c, mode, t.TempDir()) {
				k, _, _ := strings.Cut(kv, "=")
				if strings.HasPrefix(k, "TOFT_PARAM_") || appNamesStillMatter[k] || IsPlatformEnvName(k) {
					continue
				}
				t.Errorf("appEnv sets %s, which is not on PlatformEnvNames: an app could store a value for it that never arrives", k)
			}
		}
	}
}

func TestEnvNameProblem(t *testing.T) {
	for name, want := range map[string]string{
		"DATABASE_URL": "",
		"_private":     "",
		"PATH":         "",
		"1ST":          EnvNameInvalidCode,
		"MY-VAR":       EnvNameInvalidCode,
		"":             EnvNameInvalidCode,
		"A B":          EnvNameInvalidCode,
		"PORT":         EnvNameReservedCode,
		"BASE_PATH":    EnvNameReservedCode,
		EnvAppToken:    EnvNameReservedCode,
	} {
		code, msg := EnvNameProblem(name)
		if code != want {
			t.Errorf("EnvNameProblem(%q) = %q, want %q", name, code, want)
		}
		if code == EnvNameReservedCode && !strings.Contains(msg, "set by the platform") {
			t.Errorf("reserved refusal does not say the platform sets it: %q", msg)
		}
	}
}

// TestPlatformEnvNamesMatchTheClientsCopy: cli/ is its own module and keeps a
// copy of this list to refuse a name before the round trip. Read as text, both
// ways — a name the client lacks costs a round trip, and a name only the
// client has refuses a variable the install would take (security review
// 2026-11-15, CL-10).
func TestPlatformEnvNamesMatchTheClientsCopy(t *testing.T) {
	src, err := os.ReadFile("../../cli/env.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`(?s)var platformSetEnv = map\[string\]bool\{(.*?)\n\}`).FindSubmatch(src)
	if literal == nil {
		t.Fatal("cli/env.go has no `var platformSetEnv = map[string]bool{...}` literal")
	}
	var client []string
	for _, m := range regexp.MustCompile(`"([A-Z0-9_]+)"`).FindAllSubmatch(literal[1], -1) {
		client = append(client, string(m[1]))
	}
	for _, name := range PlatformEnvNames {
		if !slices.Contains(client, name) {
			t.Errorf("%s is set by the platform and missing from cli/env.go's platformSetEnv", name)
		}
	}
	for _, name := range client {
		if !IsPlatformEnvName(name) {
			t.Errorf("cli/env.go's platformSetEnv refuses %s, which the platform does not set", name)
		}
	}
}
