package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// **Every refusal this install makes carries a hint, and the ones that do not
// yet are a list that only shrinks.**
//
// The rule (toft-sdk and toft-cli: "every refusal toft or an SDK makes itself
// carries an actionable hint") was enforced one black-box finding at a time, and
// every list found more — because nothing failed when a new handler wrote a
// 4xx without one. This is that failure.
//
// A refusal is a writeJSON call with a 4xx status and a map literal. It has a
// hint when the literal has a "hint" key, or when its code — the literal's
// "code", else the status's — has one in errorHints, which completeErrorBody
// fills in. Everything else is listed in testdata/hintless-refusals.txt, keyed
// by file, function, code and message (never a line number, which every edit
// above it would move).
//
//   - A new hintless refusal fails: give it a hint. (Adding it to the list is
//     possible, and is a decision a reviewer will see in the diff.)
//   - A listed one that gained a hint, or went away, fails too until it is
//     taken off the list, so the list never describes code that is gone.
//
// TOFT_UPDATE_HINTLESS=1 rewrites the list from the code, for that second case.
//
// Not covered: refusals written through helpers that take a code and a
// sentence (writeAuthzError, a refusal value's write) and bodies built in a
// variable. Those helpers carry their hints by construction.
func TestNoNewRefusalWithoutAHint(t *testing.T) {
	found := hintlessRefusals(t)
	listPath := filepath.Join("testdata", "hintless-refusals.txt")

	if os.Getenv("TOFT_UPDATE_HINTLESS") == "1" {
		writeHintlessList(t, listPath, found)
		return
	}

	listed := map[string]int{}
	raw, err := os.ReadFile(listPath)
	if err != nil {
		t.Fatalf("reading %s: %v (TOFT_UPDATE_HINTLESS=1 writes it)", listPath, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			listed[line]++
		}
	}

	var added, gone []string
	for key, n := range found {
		for i := listed[key]; i < n; i++ {
			added = append(added, key)
		}
	}
	for key, n := range listed {
		for i := found[key]; i < n; i++ {
			gone = append(gone, key)
		}
	}
	sort.Strings(added)
	sort.Strings(gone)
	for _, key := range added {
		t.Errorf("a refusal with no hint: %s\n\tadd \"hint\": the next step, in the caller's words", key)
	}
	for _, key := range gone {
		t.Errorf("listed as hintless but no longer is: %s\n\ttake it off %s (TOFT_UPDATE_HINTLESS=1 go test -run TestNoNewRefusalWithoutAHint)", key, listPath)
	}
}

func writeHintlessList(t *testing.T, path string, found map[string]int) {
	t.Helper()
	var lines []string
	for key, n := range found {
		for i := 0; i < n; i++ {
			lines = append(lines, key)
		}
	}
	sort.Strings(lines)
	header := "# Refusals that carry no hint yet — TestNoNewRefusalWithoutAHint.\n" +
		"# file\tfunction\tcode\tmessage. This list only shrinks: give one a hint and\n" +
		"# regenerate with TOFT_UPDATE_HINTLESS=1. Never add to it by hand.\n"
	if err := os.WriteFile(path, []byte(header+strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hintlessRefusals scans this package's non-test sources.
func hintlessRefusals(t *testing.T) map[string]int {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if key, ok := hintlessKey(name, fn.Name.Name, n); ok {
					found[key]++
				}
				return true
			})
		}
	}
	return found
}

func hintlessKey(file, fn string, n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 3 {
		return "", false
	}
	if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "writeJSON" {
		return "", false
	}
	sel, ok := call.Args[1].(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	status, ok := statusByName[sel.Sel.Name]
	if !ok || status < 400 || status >= 500 {
		return "", false
	}
	lit, ok := call.Args[2].(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	code, message := "", "<expr>"
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok {
			continue
		}
		switch key.Value {
		case `"hint"`:
			return "", false
		case `"code"`:
			code = stringLit(kv.Value)
		case `"error"`:
			if s := stringLit(kv.Value); s != "" {
				message = s
			}
		}
	}
	if code == "" {
		code = errorCodeForStatus[status]
	}
	if errorHints[code] != "" {
		return "", false
	}
	message = strings.Join(strings.Fields(message), " ")
	return fmt.Sprintf("%s\t%s\t%s\t%s", file, fn, code, message), true
}

func stringLit(e ast.Expr) string {
	if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
		if s, err := strconv.Unquote(b.Value); err == nil {
			return s
		}
	}
	return ""
}

// statusByName is the 4xx constants of net/http by name, which is how the
// source spells them.
var statusByName = map[string]int{
	"StatusBadRequest": http.StatusBadRequest, "StatusUnauthorized": http.StatusUnauthorized,
	"StatusPaymentRequired": http.StatusPaymentRequired, "StatusForbidden": http.StatusForbidden,
	"StatusNotFound": http.StatusNotFound, "StatusMethodNotAllowed": http.StatusMethodNotAllowed,
	"StatusNotAcceptable": http.StatusNotAcceptable, "StatusRequestTimeout": http.StatusRequestTimeout,
	"StatusConflict": http.StatusConflict, "StatusGone": http.StatusGone,
	"StatusLengthRequired": http.StatusLengthRequired, "StatusPreconditionFailed": http.StatusPreconditionFailed,
	"StatusRequestEntityTooLarge": http.StatusRequestEntityTooLarge, "StatusUnsupportedMediaType": http.StatusUnsupportedMediaType,
	"StatusRequestedRangeNotSatisfiable": http.StatusRequestedRangeNotSatisfiable, "StatusExpectationFailed": http.StatusExpectationFailed,
	"StatusMisdirectedRequest": http.StatusMisdirectedRequest, "StatusUnprocessableEntity": http.StatusUnprocessableEntity,
	"StatusLocked": http.StatusLocked, "StatusFailedDependency": http.StatusFailedDependency,
	"StatusTooEarly": http.StatusTooEarly, "StatusUpgradeRequired": http.StatusUpgradeRequired,
	"StatusPreconditionRequired": http.StatusPreconditionRequired, "StatusTooManyRequests": http.StatusTooManyRequests,
	"StatusRequestHeaderFieldsTooLarge": http.StatusRequestHeaderFieldsTooLarge,
	"StatusUnavailableForLegalReasons":  http.StatusUnavailableForLegalReasons,
}
