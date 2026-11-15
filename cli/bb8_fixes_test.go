package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// BB-088: --as names the object inside the folder the destination names, not
// at the store's root.
func TestPutAsStaysInTheFolderNamed(t *testing.T) {
	f := newFakeStoreInstall(t)
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	for as, want := range map[string]string{"b.txt": "reports/mine/sub/b.txt", "deeper/c.txt": "reports/mine/sub/deeper/c.txt", "": "reports/mine/sub/a.txt"} {
		args := []string{"put", "toft://reports/mine/sub/", file}
		if as != "" {
			args = append(args, "--as", as)
		}
		code, out := storeRun(t, f, args...)
		if code != exitOK || !strings.Contains(out, "toft://"+want) {
			t.Errorf("--as %q = %d:\n%s", as, code, out)
		}
		if _, ok := f.objects[want]; !ok {
			t.Errorf("--as %q: keys = %v, want %s", as, f.keys(), want)
		}
	}
}

// BB-094: top --app names an app that must exist, as every other --app does.
func TestTopWithAnUnknownAppIsNotFound(t *testing.T) {
	e, _ := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/by-slug/") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"app not found","code":"not_found"}`)
			return
		}
		fmt.Fprint(w, `[]`)
	})
	err := cmdTop(e, []string{"--app", "nope", "--json"})
	var u *usageError
	if !errors.As(err, &u) || u.code != "not_found" || report(e, err) != exitUsage {
		t.Errorf("top --app nope = %v, want not_found exit 2", err)
	}
}

// BB-093: the missing-subcommand hint keeps every word that followed.
func TestMissingSubcommandHintKeepsTheSlug(t *testing.T) {
	err := unknownSubcommand("deployments", []string{"--app", "blackbox-cli-main", "--json"}, "list, prune or delete")
	var u *usageError
	if !errors.As(err, &u) || u.hint != "toft deployments list --app blackbox-cli-main --json" {
		t.Errorf("hint = %#v", err)
	}
}

// BB-093: a plain run prints a local refusal's hint under its sentence.
func TestPlainLocalRefusalPrintsItsHint(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {})
	e.command = "top"
	report(e, usageCodef("unknown_flag", "flag provided but not defined: --bogus"))
	if lines := strings.Split(strings.TrimSpace(buf.String()), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[1], "`toft top --help` lists its flags") {
		t.Errorf("stderr = %q", buf.String())
	}
}

// D-43: a zoned schedule's next firing is printed in that zone.
func TestNextRunIsInTheSchedulesZone(t *testing.T) {
	at := time.Date(2026, 11, 4, 0, 20, 0, 0, time.UTC)
	if got := nextRunTime("CRON_TZ=America/Bogota 20 19 * * *", at); got != "Nov 3 19:20 -05" {
		t.Errorf("nextRunTime = %q, want Nov 3 19:20 -05", got)
	}
	if got := nextRunTime("20 19 * * *", at); got != runTime(at) {
		t.Errorf("no zone = %q, want the local %q", got, runTime(at))
	}
}
