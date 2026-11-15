package database_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// statusAgnostic is every file that reads or writes an app status and is
// right about `ready` without naming it (ADR §107), each with the reason. The
// list is the specification: a new reader of apps.status either handles
// `ready` or is added here with an argument.
var statusAgnostic = map[string]string{
	"internal/builder/reconcile.go":             "restores only running/crashed; startOn refuses a batch app with ErrNoService",
	"internal/builder/move.go":                  "a move settles back to stopped or sleeping only; a ready app is rebuilt and stays ready, as any deploy of one does",
	"internal/autodeploy/autodeploy.go":         "deploys every status but stopped, and a ready app should redeploy on a push",
	"internal/api/dependency_handlers.go":       "splits stopped from serving; a ready app's release is executed by its jobs, so it is serving",
	"internal/database/dependency_inventory.go": "same split as dependency_handlers.go",
	"internal/database/dependency_events.go":    "same split as dependency_handlers.go",
	"internal/api/app_handlers.go":              "restart and stop refuse a batch app first (refuseNoService); appHasWorkload lists the statuses with a workload, and ready has none",
	"internal/api/connect_handlers.go":          "writes an initial status for a new app; never reads one",
	"internal/api/gallery_handlers.go":          "writes an initial status for a new app; never reads one",
	"internal/api/apptoken_auth.go":             "refuses only stopped; a batch app's jobs hold no app token",
	"internal/api/applock_handlers.go":          "writes stopped only for an app with a service; a batch app is skipped by IsBatchFramework",
	"internal/depsweep/sweep.go":                "writes stopped only for an app with a service; a batch app is skipped by its framework",
	"internal/api/checks_handlers.go":           "counts crashed and failed; ready is neither",
	"internal/idle/reaper.go":                   "sleeps only running apps",
	"internal/health/watcher.go":                "watches only running and crashed apps",
	"internal/supervisor/supervisor.go":         "retries only crashed apps",
	"internal/supervisor/remount.go":            "remounts only running apps; a job gets its mounts at dispatch",
	"internal/appint/appint.go":                 "refuses only stopped; a batch app's jobs hold no app token",
	"internal/api/log_handlers.go":              "names the absence of a run log for sleeping and stopped; a batch app's output is its runs' logs",
	"internal/api/wake_handlers.go":             "wakes only a sleeping app; a ready app never sleeps",
	"internal/builder/renderschedule.go":        "fires for every status but stopped, as autodeploy does; a ready document re-renders on its schedule",
	"internal/builder/disk.go":                  "orders eviction running-last; a ready app has no process, so its releases go with the other idle ones",
}

var statusRead = regexp.MustCompile(`AppStatus(Running|Sleeping|Stopped|Crashed|Failed|Building)\b`)

// The test whose failure would otherwise be silent. A reader of apps.status
// that forgets `ready` does whatever its default does: a batch app probed for a
// page it never serves, or restarted into a crash loop.
func TestEveryStatusReaderNamesReady(t *testing.T) {
	root := filepath.Join("..", "..")
	var missing []string
	seen := map[string]bool{}
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(raw)
		if !statusRead.MatchString(src) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == "internal/database/models.go" {
			return nil // where the constants are defined
		}
		if strings.Contains(src, "AppStatusReady") || strings.Contains(src, "LiveStatus(") {
			return nil
		}
		if _, ok := statusAgnostic[rel]; ok {
			seen[rel] = true
			return nil
		}
		missing = append(missing, rel)
		return nil
	}
	if err := filepath.WalkDir(filepath.Join(root, "internal"), walk); err != nil {
		t.Fatal(err)
	}
	if err := walk(filepath.Join(root, "main.go"), fakeFile{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range missing {
		t.Errorf("%s reads an app status and says nothing about `ready`. Handle it, or add the "+
			"file to statusAgnostic with the reason it is already right (ADR §107)", f)
	}
	for f := range statusAgnostic {
		if !seen[f] {
			t.Errorf("statusAgnostic names %s, which no longer reads a status without handling "+
				"`ready` — remove it, so the list stays the specification", f)
		}
	}
}

type fakeFile struct{}

func (fakeFile) Name() string               { return "main.go" }
func (fakeFile) IsDir() bool                { return false }
func (fakeFile) Type() fs.FileMode          { return 0 }
func (fakeFile) Info() (fs.FileInfo, error) { return nil, nil }
