package database_test

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

// Test 3. Which predicate a query narrows through, asserted by walking the
// source rather than by trusting a comment.
//
// The two are indistinguishable in any test that has one app in it, and both
// ways of getting them backwards are silent (see TestListableContainsReadable
// for what each one does). So the caller set of each is written down here and
// the list *is* the specification: a new listing that reaches for `readable`,
// or an object check that reaches for `listable`, fails this test with a
// sentence rather than shipping.
//
// Adding a caller means adding it here, which is the point — the decision about
// which set a query means is one somebody should have to make out loud.
func TestAppsPredicateCallers(t *testing.T) {
	// A listing means listable. An object check means readable.
	//
	// readableAppsPredicate's only caller is listableAppsPredicate, which is
	// how the containment is a property of the code: listable is literally
	// readable with one more disjunct. That is not an accident of the current
	// factoring, so it is stated.
	allowed := map[string]map[string]bool{
		"readableAppsPredicate": {
			"listableAppsPredicate":       true,
			"(AppScope).readingPredicate": true,
		},
		"listableAppsPredicate": {
			"(*DB).ListApps":              true,
			"(AppScope).listingPredicate": true,
		},
		// AppScope carries the same split to the queries that reach the two
		// predicates through a struct, so its two methods get caller sets of
		// their own. A tag count is a listing; a cost figure, a language and a
		// version are facts about the app, which the rung does not disclose.
		"listingPredicate": {
			"(*DB).ListTags":             true,
			"(*DB).frameworkCounts":      true,
			"(*DB).ListTagValues":        true,
			"(*DB).ListAppsByTag":        true,
			"(*DB).ListAppsByFramework":  true,
			"(*DB).FrameworkValueCounts": true,
		},
		"readingPredicate": {
			"(*DB).EstateByLanguage": true,
			"(*DB).EstateTargets":    true,
			// What an app *holds* is the sharpest case for reading rather than
			// listing: a credential name, a mount path and a channel are facts
			// about the app, and the rung discloses the row that names it and
			// not one field more.
			"(*DB).EstateReach":       true,
			"(*DB).ListConsumption":   true,
			"(*DB).JobRunConsumption": true,
		},
	}

	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var offenders []string
	seen := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			caller := funcLabel(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					// scope.listingPredicate() and friends. Matched on the
					// method name alone: the receiver is always an AppScope, and
					// there is no other method in the package spelled either way.
					name = fun.Sel.Name
				default:
					return true
				}
				ident := struct{ Name string }{name}
				permitted, tracked := allowed[ident.Name]
				if !tracked {
					return true
				}
				seen[ident.Name]++
				if permitted[caller] {
					return true
				}
				offenders = append(offenders, fset.Position(call.Pos()).String()+": "+caller+" calls "+ident.Name)
				return true
			})
		}
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d call site(s) narrow through a predicate their caller set does not name.\n"+
			"A listing narrows through listableAppsPredicate; anything deciding whether a request may "+
			"proceed narrows through readableAppsPredicate. If this call site is right, add it to the "+
			"map in this test and say which it is:\n\t%s",
			len(offenders), strings.Join(offenders, "\n\t"))
	}

	// A predicate nothing calls means the walk matched nothing and the test is
	// passing vacuously — which is how a source-walking guard quietly stops
	// guarding after a rename.
	for predicate := range allowed {
		if seen[predicate] == 0 {
			t.Errorf("no call to %s was found anywhere in the package; this test is asserting nothing",
				predicate)
		}
	}

	// And the other direction: a caller named here that no longer exists is a
	// specification nobody is reading. Without this the map rots into a list of
	// functions that were once called something else.
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				found[funcLabel(fn)] = true
			}
		}
	}
	for predicate, callers := range allowed {
		for caller := range callers {
			if !found[caller] {
				t.Errorf("%s names %s as a permitted caller of %s, and no such function exists",
					"the caller map", caller, predicate)
			}
		}
	}
}

// funcLabel spells a declaration the way the map above does: bare for a
// function, receiver-qualified for a method.
func funcLabel(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	var recv string
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			recv = "*" + ident.Name
		}
	case *ast.Ident:
		recv = t.Name
	}
	return "(" + recv + ")." + fn.Name.Name
}
