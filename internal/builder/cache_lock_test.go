package builder

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/runtime"
)

// TestEveryBuildPathHoldsTheCacheLock is the test that turns D3's list into a
// specification instead of a sentence in a document.
//
// The rule: a function that calls installDependencies is a path that runs build
// commands against the shared cache, so it must hold cacheMu.RLock. Two such
// functions exist today — execute and Repair — and the second is the one that
// is easy to miss, because a repair is a *start* rather than a deploy and never
// goes near the build queue. The third caller is the foreseeable regression,
// and this is what catches it: the failure arrives when the caller is written,
// naming it, rather than as a corrupted release on somebody's install months
// later.
//
// It reads the source rather than exercising behaviour on purpose. A runtime
// test can only catch a path it was told to call, which is precisely the
// property a *new* caller does not have.
//
// The check is deliberately strict about *where* the lock is taken: a helper
// that calls installDependencies must take the lock itself, even if every one
// of its own callers already holds it. Go's RWMutex is not reentrant, so such a
// helper is either a deadlock or a lie about which span is protected, and
// either way it wants a human to look at it rather than a passing test.
func TestEveryBuildPathHoldsTheCacheLock(t *testing.T) {
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	var callers []string
	for _, p := range pkg {
		for _, file := range p.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || fn.Name.Name == "installDependencies" {
					continue
				}
				if !callsMethod(fn.Body, "installDependencies") {
					continue
				}
				callers = append(callers, fn.Name.Name)
				if !takesCacheReadLock(fn.Body) {
					t.Errorf("%s calls installDependencies without taking cacheMu.RLock.\n"+
						"Every path that runs a build command against the shared cache holds the "+
						"read side of that lock, so a reclaim cannot delete the cache out from "+
						"under it. Add b.cacheMu.RLock() with a deferred RUnlock, or say in the "+
						"PRD's terms why this path is not one of them.", fn.Name.Name)
				}
			}
		}
	}

	// Without this the test passes loudly and vacuously the day the walk breaks
	// — a rename of installDependencies, a move to another package — which is
	// the failure mode a source-reading test is actually prone to.
	if len(callers) < 2 {
		t.Fatalf("found %d caller(s) of installDependencies %v, want at least the two D3 names "+
			"(execute and Repair). The walk is broken, not the code.", len(callers), callers)
	}
}

// callsMethod reports whether body contains a call to a method of this name on
// any receiver — b.installDependencies(...) and nothing else in the package
// shares the name.
func callsMethod(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// takesCacheReadLock reports whether body contains a b.cacheMu.RLock() call —
// a selector two deep, so a plain b.mu.RLock() guarding the log listeners does
// not satisfy it.
func takesCacheReadLock(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		outer, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || outer.Sel.Name != "RLock" {
			return true
		}
		if inner, ok := outer.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "cacheMu" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestABuildStartedDuringAReclaimWaits asserts the writer-preference property
// D3 depends on rather than assuming it.
//
// The reclaim can only afford to *wait* for builds — rather than refuse while
// any are running, which on a busy install is an instruction nobody can ever
// satisfy — if a queue of arriving builds cannot starve it. Go's RWMutex gives
// that: once a writer is blocked, new readers block behind it instead of
// joining the readers already inside.
//
// This is a property of the standard library, and it is asserted here anyway,
// because the entire argument for waiting rather than refusing rests on it and
// nothing else in this package would notice if it changed.
func TestABuildStartedDuringAReclaimWaits(t *testing.T) {
	b := &Builder{}

	// A build is in flight, holding the read side.
	b.cacheMu.RLock()

	// The reclaim asks for the write side and blocks behind that build.
	reclaiming := make(chan struct{})
	go func() {
		b.cacheMu.Lock()
		close(reclaiming)
	}()

	// Give the reclaim time to actually be waiting rather than not yet
	// scheduled; without this the next goroutine could win the lock honestly
	// and the test would assert nothing.
	time.Sleep(50 * time.Millisecond)

	// A second build now arrives. It must NOT be admitted alongside the first,
	// because a writer is waiting.
	admitted := make(chan struct{})
	var once sync.Once
	go func() {
		b.cacheMu.RLock()
		once.Do(func() { close(admitted) })
		b.cacheMu.RUnlock()
	}()

	select {
	case <-admitted:
		t.Fatal("a build starting during a waiting reclaim was admitted alongside the build " +
			"already running. Readers are not queueing behind the writer, so a steady stream " +
			"of deploys would starve the reclaim forever — and D5's decision to wait rather " +
			"than refuse a reclaim while builds are in flight is no longer safe.")
	case <-time.After(100 * time.Millisecond):
		// Correct: the second build is waiting.
	}

	// The first build finishes; the reclaim gets in.
	b.cacheMu.RUnlock()
	select {
	case <-reclaiming:
	case <-time.After(2 * time.Second):
		t.Fatal("the reclaim never acquired the lock after the in-flight build finished")
	}
	b.cacheMu.Unlock()

	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting build never ran after the reclaim released the lock")
	}
}

// A rollback's start reaches Repair from inside execute, which already holds
// the read side; with a reclaim queued between the two, a second RLock on the
// same goroutine waited on the writer while the writer waited on the first
// (security review 2026-11-14, PB-3). A spec that says the caller holds the
// lock is repaired without taking it again.
func TestRepairUnderABuildDoesNotTakeTheCacheLockAgain(t *testing.T) {
	b := &Builder{}
	b.cacheMu.RLock()
	defer b.cacheMu.RUnlock()

	reclaiming := make(chan struct{})
	go func() {
		b.cacheMu.Lock()
		close(reclaiming)
		b.cacheMu.Unlock()
	}()
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		done <- b.Repair(runtime.WorkloadSpec{
			Slug: "repair", Framework: "python", BuildHoldsCache: true,
		}, t.TempDir())
	}()
	select {
	case <-done:
		// Whatever it answered, it did not block on the waiting reclaim.
	case <-time.After(2 * time.Second):
		t.Fatal("Repair blocked behind a waiting reclaim although its caller holds the cache lock")
	}
}
