package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The bet the read scope is built on: **no GET handler on the platform API
// writes anything.**
//
// `read` is derived from the request method rather than curated into a second
// map (see userKeyScopeWrites), which is only sound while that sentence is
// true. Nothing else in this codebase would notice it becoming false: no
// middleware treats the method as a safety classification — originCheck gates
// on the presence of an Origin header and exempts nothing — so a mutating GET
// would be an ordinary route with an ordinary review, and its consequence, a
// read-scoped key that writes, is invisible until somebody goes looking.
//
// **It is a static check with known blind spots.** A write reached through a
// helper, or in a package a handler calls, passes this. That is accepted rather
// than papered over: what it catches is the realistic version of the mistake —
// somebody adding a GET that mutates because GET was convenient — and it puts
// the decision somewhere a reviewer sees it fail rather than in a comment
// nobody reads.
//
// Failing this is not a test to loosen. Either the handler should not be a GET,
// or the read scope has stopped meaning what the mint dialog and both guides
// say it means.

// writeVerbs are the prefixes of a database method that changes something. They
// are matched only against calls on `s.db`, so `w.Header().Set` and a mutex's
// `Add` are not swept up by a vocabulary that is deliberately broad.
var writeVerbs = []string{
	"Create", "Insert", "Update", "Delete", "Set", "Save", "Add", "Remove",
	"Upsert", "Record", "Purge", "Prune", "Touch", "Claim", "Mark", "Clear",
}

// osWriters are the filesystem calls a handler could reach for directly.
var osWriters = map[string]bool{
	"Remove": true, "RemoveAll": true, "WriteFile": true, "MkdirAll": true,
	"Mkdir": true, "Rename": true, "Truncate": true, "Create": true,
}

func TestNoGetHandlerWrites(t *testing.T) {
	fset := token.NewFileSet()

	// Every GET the router registers, by handler name. Registration is what is
	// walked rather than the live router, because a handler is a method value
	// there and a method value carries no body to inspect.
	routerFile, err := parser.ParseFile(fset, "router.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing router.go: %v", err)
	}
	getHandlers := map[string]bool{}
	ast.Inspect(routerFile, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Get" || len(call.Args) != 2 {
			return true
		}
		if _, ok := call.Args[0].(*ast.BasicLit); !ok {
			return true
		}
		// s.handleX — the shape every route in this file uses.
		if h, ok := call.Args[1].(*ast.SelectorExpr); ok {
			getHandlers[h.Sel.Name] = true
		}
		// An inline handler has no name to look up, so it is checked here and
		// reported against the route's own position.
		if lit, ok := call.Args[1].(*ast.FuncLit); ok {
			if hits := writeShapedCalls(lit.Body); len(hits) > 0 {
				t.Errorf("the inline GET handler at router.go:%d makes %s — a GET that "+
					"writes makes a read-scoped key a writing credential",
					fset.Position(lit.Pos()).Line, strings.Join(hits, ", "))
			}
		}
		return true
	})
	if len(getHandlers) < 100 {
		t.Fatalf("only %d GET handlers found in router.go — the walk has stopped seeing "+
			"the registrations, and a passing result would mean nothing", len(getHandlers))
	}

	// Their bodies, from the whole package: handlers are spread across files
	// and the router names them without saying where they live.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var problems []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !getHandlers[fn.Name.Name] {
				continue
			}
			seen[fn.Name.Name] = true
			for _, hit := range writeShapedCalls(fn.Body) {
				problems = append(problems, name+":"+
					strconv.Itoa(fset.Position(fn.Pos()).Line)+" "+fn.Name.Name+" calls "+hit)
			}
		}
	}

	sort.Strings(problems)
	for _, p := range problems {
		t.Errorf("%s — a GET handler that writes turns the read scope into a promise this "+
			"install does not keep", p)
	}

	// A handler the walk could not find is a hole in the sweep, not a pass.
	var missing []string
	for name := range getHandlers {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d GET handler(s) registered in router.go were not found in this package, "+
			"so their bodies were never checked:\n\t%s",
			len(missing), strings.Join(missing, "\n\t"))
	}
}

// writeShapedCalls reports the calls in a body that change something, by the
// name a reader would recognise.
func writeShapedCalls(body *ast.BlockStmt) []string {
	var hits []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		switch {
		case name == "Exec" || name == "ExecContext":
			hits = append(hits, "."+name+"()")
		case name == "Enqueue":
			hits = append(hits, ".Enqueue()")
		case strings.HasPrefix(name, "WriteAudit"):
			hits = append(hits, name+"()")
		case isOSCall(sel) && osWriters[name]:
			hits = append(hits, "os."+name+"()")
		case isDBCall(sel) && hasWriteVerb(name):
			hits = append(hits, "s.db."+name+"()")
		}
		return true
	})
	return hits
}

// isDBCall reports whether the selector's receiver is `s.db`, the one handle
// every handler reaches the database through.
func isDBCall(sel *ast.SelectorExpr) bool {
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "db" {
		return false
	}
	ident, ok := inner.X.(*ast.Ident)
	return ok && ident.Name == "s"
}

func isOSCall(sel *ast.SelectorExpr) bool {
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "os"
}

func hasWriteVerb(name string) bool {
	for _, v := range writeVerbs {
		if strings.HasPrefix(name, v) {
			return true
		}
	}
	return false
}
