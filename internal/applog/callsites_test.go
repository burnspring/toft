package applog_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writers is every file that may put a line in front of an app's owner.
//
// **The list is the specification.** A seventh writer is a build failure rather
// than a discovery, which is the whole of what this stage buys: a log line is
// free text and no filter over it could be a promise the implementation keeps,
// so what is testable is not *what* is said but *who* may say it.
//
// Adding a file here is a deliberate act with a paragraph attached. Before you
// do, read the two rules in the package comment — no error value, stage marks
// rather than a stream — because they are the reason the operator's log.Printf
// beside your new line stays exactly where it is.
var writers = map[string]bool{
	// The shared launcher, through the runtime.Narrator seam.
	"internal/runtime/agent.go": true,
	// The shared repair, whose subprocess output deliberately does not come
	// through here.
	"internal/builder/dependencies.go": true,
	// The background actors.
	"internal/health/watcher.go":        true,
	"internal/supervisor/supervisor.go": true,
	"internal/supervisor/remount.go":    true,
	"internal/idle/reaper.go":           true,
	// The isolated ingest, which relays what a service's runner already said
	// in the platform's voice, and drops what its app printed.
	"internal/api/runner_handlers.go": true,
	// The builder's two lines to an owner: why a start failed, said once per
	// transition, and how long a wake from idle sleep took. The container never started, so this narration is the only
	// log the Logs tab has for the failure; the operator's log.Printf beside
	// it stays. Everything else the builder says goes to the build log, which
	// is the deployment's and not the platform's voice.
	"internal/builder/reconcile.go": true,
	// The wiring, which adapts the sink to the seam.
	"main.go": true,
}

// sayVerbs are the writing verbs, and the reason they are named distinctively:
// a textual scan for them is exact, so this test needs no type information and
// therefore no build of the packages it audits.
var sayVerbs = map[string]bool{"Say": true, "Sayf": true}

func TestOnlyTheListedFilesSay(t *testing.T) {
	root := repoRoot(t)

	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// A dot-directory is never the product: .git, and .claude, which
			// holds whole checkouts (agent worktrees) of other trees.
			if path != root && strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			switch info.Name() {
			case "node_modules", "web", "data", "tests", "deploy", "docs", "cli":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		// The package that defines the verbs is not a caller of them.
		if strings.HasPrefix(rel, "internal/applog/") {
			return nil
		}

		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", rel, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !sayVerbs[sel.Sel.Name] {
				return true
			}
			if !writers[rel] {
				found = append(found, rel+" calls ."+sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(found) > 0 {
		sort.Strings(found)
		t.Errorf("a file outside the writer list speaks in the platform's voice:\n  %s\n\n"+
			"Every line this reaches is read by an app's *owner*, not by an operator. "+
			"If that is what you meant, add the file to `writers` in this test with a "+
			"paragraph saying what it says and why the owner is owed it — and keep the "+
			"operator's log.Printf where it is. If it is not, it belongs in log.Printf alone.",
			strings.Join(found, "\n  "))
	}
}

// TestEveryListedWriterActuallySays is the other half, and it is the half that
// rots: a writer removed from the code and left in the list turns this test
// into decoration that passes for the wrong reason.
func TestEveryListedWriterActuallySays(t *testing.T) {
	root := repoRoot(t)
	for rel := range writers {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Errorf("%s is on the writer list and does not exist", rel)
			continue
		}
		if !strings.Contains(string(b), ".Say") {
			t.Errorf("%s is on the writer list and says nothing — remove it, "+
				"or the list has stopped being the specification", rel)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
