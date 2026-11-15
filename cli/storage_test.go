package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `toft storage ls` is the install's own answer under --json, and a table of
// what you may attach — or, with --app, what that app has and where each came
// from — on a terminal.
func TestStorageListPassesTheInstallsAnswerThrough(t *testing.T) {
	mine := `{"resources":[{"slug":"sales-data","kind":"store","max_access":"full","partitioned":true,` +
		`"future_field":1}],"shared_mode":true}`
	apps := `{"mappings":[{"resource_slug":"sales-data","resource_kind":"store","folder":"2026","access":"read",` +
		`"path":"/storage/sales-data/2026","source":"manifest"},{"resource_slug":"cache","resource_kind":"volume",` +
		`"access":"full","path":"/data/cache","source":"console"}],"pending":false}`
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/me/storage":
			fmt.Fprint(w, mine)
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/apps/app-1/storage"):
			fmt.Fprint(w, apps)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}

	e, stdout, _ := splitInstall(t, handler)
	if err := cmdStorage(e, []string{"ls", "--json"}); err != nil {
		t.Fatalf("storage ls --json: %v", err)
	}
	if !strings.Contains(stdout.String(), "future_field") {
		t.Errorf("a field this binary does not name was dropped: %s", stdout.String())
	}

	e, stdout, _ = splitInstall(t, handler)
	if err := cmdStorage(e, []string{"ls"}); err != nil {
		t.Fatalf("storage ls: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "sales-data") || !strings.Contains(got, "by folder") {
		t.Errorf("table = %q, want the resource and that it is attached by folder", got)
	}

	e, stdout, _ = splitInstall(t, handler)
	if err := cmdStorage(e, []string{"ls", "--app", "demo"}); err != nil {
		t.Fatalf("storage ls --app: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "sales-data/2026") || !strings.Contains(got, "toft.toml") ||
		!strings.Contains(got, "console") {
		t.Errorf("table = %q, want each mapping with where it came from", got)
	}
}

// `toft check` prints the storage plan in the deploy log's marks.
func TestCheckPrintsTheStoragePlan(t *testing.T) {
	answer := `{"ok":true,"framework":"flask","not_checked":["build"],"storage":{"keep":[],` +
		`"add":[{"name":"sales-data","access":"read","path":"/storage/sales-data"}],"change":[],` +
		`"remove":[{"name":"old","access":"read","path":"/storage/old"}],` +
		`"console":[{"name":"cache","access":"full","path":"/data/cache"}]}}`
	e, stdout, _ := splitInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/check"):
			fmt.Fprint(w, answer)
		}
	})
	os.WriteFile(filepath.Join(e.dir, "app.py"), []byte("x"), 0o644)
	if err := cmdCheck(e, []string{"--app", "demo"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	got := stdout.String() + e.out.Stderr.(interface{ String() string }).String()
	for _, want := range []string{"+ sales-data, read at /storage/sales-data", "- old, detached",
		"= cache, kept: attached in the console"} {
		if !strings.Contains(got, want) {
			t.Errorf("check output lacks %q:\n%s", want, got)
		}
	}
}
