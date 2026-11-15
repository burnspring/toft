package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

// `toft create` prints `toft link <slug>` as the next step, and a project
// whose toft.toml already declares [app] with a name but no slug -- every
// manifest example does -- used to have that step refused. The slug now goes
// under the existing header, and nothing else in the file moves.
func TestLinkAddsASlugToAnAppTableThatHasNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toft.toml")
	before := "# the team's manifest\n[app]\nname = \"Payroll\"\n\n[jobs.nightly]\ncommand = \"python run.py\"\n"
	if err := os.WriteFile(path, []byte(before), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeAppTable(path, "payroll", "https://toft.example.com", false); err != nil {
		t.Fatalf("linking a table with no slug was refused: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := "# the team's manifest\n[app]\nslug = \"payroll\"\nname = \"Payroll\"\n\n[jobs.nightly]\ncommand = \"python run.py\"\n"
	if string(got) != want {
		t.Errorf("file after link:\n%s\nwant:\n%s", got, want)
	}
}

// A table that already names a slug is somebody's decision: still refused, and
// the file is untouched.
func TestLinkStillRefusesAnAppTableThatNamesASlug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toft.toml")
	before := "[app]\nslug = \"other\"\n"
	os.WriteFile(path, []byte(before), 0644)
	err := writeAppTable(path, "payroll", "https://toft.example.com", false)
	var u *usageError
	if !errors.As(err, &u) {
		t.Fatalf("want a usage refusal, got %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != before {
		t.Errorf("the file changed: %q", got)
	}
}

// A per-install link for an install the file already links, under any
// spelling of its address, is refused when the slug differs and a no-op when
// it is the same — never a second table, which TOML refuses (security review
// 2026-11-15, CL-3).
func TestLinkPerInstallRefusesASecondTableForOneInstall(t *testing.T) {
	const url = "https://toft.example.com"
	for _, key := range []string{url, url + "/"} {
		path := filepath.Join(t.TempDir(), "toft.toml")
		before := "[app]\nslug = \"app-a\"\n\n[app.installs.\"" + key + "\"]\nslug = \"app-a\"\n"
		os.WriteFile(path, []byte(before), 0644)

		err := writeAppTable(path, "app-b", url, true)
		var u *usageError
		if !errors.As(err, &u) || u.code != "app_already_named" || !strings.Contains(err.Error(), "app-a") {
			t.Errorf("%s: err = %v, want app_already_named naming app-a", key, err)
		}
		if got, _ := os.ReadFile(path); string(got) != before {
			t.Errorf("%s: the file changed: %q", key, got)
		}
		if err := writeAppTable(path, "app-a", url, true); err != nil {
			t.Errorf("%s: the same slug again: %v", key, err)
		}
		if got, _ := os.ReadFile(path); string(got) != before {
			t.Errorf("%s: the same slug changed the file: %q", key, got)
		}
	}
}

// A table that already names this slug is linked already, whatever key comes
// first: `install` above `slug` used to be refused as "naming a slug".
func TestLinkLeavesAnAppTableAloneThatNamesThisSlug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toft.toml")
	before := "[app]\ninstall = \"https://toft.example.com\"\nslug = \"payroll\"\n\n[jobs.nightly]\ncommand = \"python run.py\"\n"
	os.WriteFile(path, []byte(before), 0644)
	if err := writeAppTable(path, "payroll", "https://toft.example.com", false); err != nil {
		t.Fatalf("a table already naming this slug was refused: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != before {
		t.Errorf("the file changed: %q", got)
	}
}

// A slug the install does not show is refused naming the file it was read
// from, when it was not typed on the command line: a stale .toft.toml
// overriding toft.toml read as "no such app" with nothing pointing at it.
func TestAnUnresolvedSlugNamesTheLayerItWasReadFrom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found","code":"not_found"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, config.LocalManifestFile), []byte("[app]\nslug = \"stale\"\n"), 0644)
	resolver, err := config.NewResolver(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &session{Session: &config.Session{InstallURL: srv.URL}, resolver: resolver,
		client: client.New(srv.URL, "tok", "toft-test")}
	_, err = s.resolveApp("stale")
	if err == nil || !strings.Contains(err.Error(), config.LocalManifestFile) {
		t.Errorf("the refusal does not name %s: %v", config.LocalManifestFile, err)
	}
	// Typed on the command line, it is the caller's own word: nothing to add.
	resolver, _ = config.NewResolver(dir, map[config.Key]string{config.KeyApp: "typed"})
	s.resolver = resolver
	if _, err = s.resolveApp("typed"); err == nil || strings.Contains(err.Error(), "read from") {
		t.Errorf("a typed slug names a layer: %v", err)
	}
}

// An app with no variables lists as [] under --json, never null.
func TestEnvViewOfNoVariablesIsAnEmptyList(t *testing.T) {
	b, err := json.Marshal(envView(&appEnv{}))
	if err != nil || string(b) != "[]" {
		t.Errorf("envView of nothing encodes as %s (%v), want []", b, err)
	}
}
