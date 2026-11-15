package gitconn_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Test 2. The resolver is the only thing that opens a clone credential, and a
// walk of the source is what says so.
//
// This has the shape of `keyroutes`'s router walk, and for the same reason: a
// property that is true today because two functions happen to be the only ones
// written, and that a third would break without any test failing. The failure
// mode is specific — a new code path decrypts `apps.git_token` for itself,
// works perfectly, and never learns that a connection now answers that question
// or that a connection can be disabled. It would be a *correct-looking* fourth
// opinion about what git authenticates with.
//
// What it looks for is a decrypt whose argument is a credential field. That is
// narrower than "nothing outside this package touches the column" on purpose:
// `internal/api` writes `apps.git_token`, reports whether one is set, and
// clears it, and none of those is the thing being kept to one place. Opening
// it is.
func TestOnlyTheResolverOpensACloneCredential(t *testing.T) {
	// Fields whose plaintext is a clone credential. Decrypting one of these
	// anywhere but internal/gitconn is the bug.
	credentialFields := map[string]bool{
		"GitToken":    true,
		"CloneSecret": true,
	}
	decrypts := map[string]bool{
		"Decrypt":        true,
		"DecryptContext": true,
	}

	root := repoRoot(t)
	var offenders []string

	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "testdata" || info.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// The resolver is allowed to, and is the point.
			if strings.HasPrefix(path, filepath.Join(base, "gitconn")+string(filepath.Separator)) {
				return nil
			}

			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Errorf("parsing %s: %v", path, perr)
				return nil
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !decrypts[fn.Sel.Name] {
					return true
				}
				for _, arg := range call.Args {
					sel, ok := arg.(*ast.SelectorExpr)
					if ok && credentialFields[sel.Sel.Name] {
						rel, _ := filepath.Rel(root, path)
						offenders = append(offenders,
							rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line)+" decrypts ."+sel.Sel.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("a clone credential is opened outside internal/gitconn:\n\t%s\n"+
			"There is one reader, and it is gitconn.Resolver.CloneCredential. A second "+
			"one would not learn that a connection can answer this question, or that a "+
			"connection can be disabled — and it would look right while doing it.",
			strings.Join(offenders, "\n\t"))
	}
}

// repoRoot walks up from this package until it finds go.mod, so the test does
// not depend on where `go test` was invoked from.
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
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
