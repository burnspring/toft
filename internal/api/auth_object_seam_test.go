package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One spelling of "the app as the authorizer sees it".
//
// The effective visibility of an app is not the column: a public app the
// outside world will not confirm is reachable is handed to the authorizer as
// `authenticated`, and that substitution lives in exactly one function. A
// handler that assembled its own auth.Object from an app row would quietly
// serve a suspended app to anonymous callers — with nothing failing, nothing
// logged and no page looking wrong — which is the same class of drift
// readableAppsPredicate exists to prevent on the SQL side.
//
// This walks the source rather than the router because that is where the
// failure is: the object is built before any route is reached, and a walk of
// live routes cannot see a handler that has not been mounted yet.
//
// Loosening this test is a decision, not a fix. If a second construction is
// genuinely needed, it belongs beside AuthObjectOf in the file named here.
//
// The home is outside this package, so the walk covers both packages rather
// than only this one.
const authObjectHome = "../database/models.go"

var authObjectPackages = []string{".", "../database"}

func TestAuthObjectIsBuiltInOnePlace(t *testing.T) {
	fset := token.NewFileSet()
	found := map[string][]int{}
	for _, pkg := range authObjectPackages {
		entries, err := os.ReadDir(pkg)
		if err != nil {
			t.Fatalf("reading %s: %v", pkg, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(pkg, name)
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Object" {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "auth" {
					return true
				}
				found[path] = append(found[path], fset.Position(lit.Pos()).Line)
				return true
			})
		}
	}

	if len(found[authObjectHome]) == 0 {
		t.Errorf("no auth.Object is constructed in %s — has appObject moved?", authObjectHome)
	}
	for file, lines := range found {
		if file == authObjectHome {
			continue
		}
		t.Errorf("%s builds an auth.Object at lines %v — every one of them must go through "+
			"database.AuthObjectOf, or a public app whose reachability was withdrawn is "+
			"authorized from the stored column", file, lines)
	}
}
