package sdk_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// **Every refusal an SDK makes itself carries a hint — in all four, checked
// at every place one is built — and the ones that do not yet are a list that
// only shrinks.**
//
// toft-sdk promises it ("every SDK-raised refusal carries a hint"), and black-box
// runs kept finding the next call that broke it, one language at a time,
// because nothing failed when a new refusal was written without one. This is
// that failure, read off the source of each SDK:
//
//   - Go: refusal(code) with no hint or an empty one, and badRequest(msg, "").
//   - Node: refused(...) with no hint, and a new ToftError(...) whose body is
//     written there with a code and no hint.
//   - Python: a ToftError(...) whose body is written there with a code and no
//     hint.
//   - R: toft_refusal(code, message) with no hint.
//
// A refusal relayed from the platform's own answer carries the platform's hint
// (internal/api's TestNoNewRefusalWithoutAHint is that side), so a body passed
// through from a response is not judged here.
//
// The exceptions are testdata/sdk-hintless-refusals.txt, keyed by file and the
// refusal's code or message — never a line. A new one fails; one that gained a
// hint fails until it is taken off. TOFT_UPDATE_HINTLESS=1 rewrites the list.
func TestEverySDKMadeRefusalCarriesAHint(t *testing.T) {
	found := map[string]int{}
	for _, key := range goHintless(t) {
		found[key]++
	}
	for _, key := range textHintless(t) {
		found[key]++
	}

	listPath := filepath.Join("testdata", "sdk-hintless-refusals.txt")
	if os.Getenv("TOFT_UPDATE_HINTLESS") == "1" {
		var lines []string
		for key, n := range found {
			for i := 0; i < n; i++ {
				lines = append(lines, key)
			}
		}
		sort.Strings(lines)
		header := "# SDK-made refusals that carry no hint yet — TestEverySDKMadeRefusalCarriesAHint.\n" +
			"# file\twhat. This list only shrinks: give one a hint and regenerate with\n" +
			"# TOFT_UPDATE_HINTLESS=1. Never add to it by hand.\n"
		if err := os.WriteFile(listPath, []byte(header+strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
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
		t.Errorf("an SDK-made refusal with no hint: %s\n\tgive it the next step, in that SDK's own call names", key)
	}
	for _, key := range gone {
		t.Errorf("listed as hintless but no longer is: %s\n\ttake it off %s (TOFT_UPDATE_HINTLESS=1)", key, listPath)
	}
}

// goHintless reads the Go SDK by its syntax tree.
func goHintless(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("go", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			// An &Error{Body: map[string]any{"code": …}} written out by hand
			// is a refusal made here as much as refusal(code, hint) is, and
			// was not read at all (BB-113's sweep).
			if lit, ok := n.(*ast.CompositeLit); ok {
				if code, hinted := goBodyLiteral(lit); code != nil && !hinted {
					out = append(out, fmt.Sprintf("%s\tError{Body: code %s}", filepath.ToSlash(name), goLabel(code)))
				}
				return true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || len(call.Args) == 0 {
				return true
			}
			var hint ast.Expr
			switch id.Name {
			case "refusal":
				if len(call.Args) > 1 {
					hint = call.Args[1]
				}
			case "badRequest":
				if len(call.Args) > 1 {
					hint = call.Args[1]
				}
			default:
				return true
			}
			if hint == nil || isEmptyString(hint) {
				out = append(out, fmt.Sprintf("%s\t%s(%s)", filepath.ToSlash(name), id.Name, goLabel(call.Args[0])))
			}
			return true
		})
	}
	return out
}

// goBodyLiteral reads an Error{...} literal's Body when it is a map literal:
// the "code" value, and whether a non-empty "hint" is beside it.
func goBodyLiteral(lit *ast.CompositeLit) (code ast.Expr, hinted bool) {
	if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "Error" {
		return nil, false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Body" {
			continue
		}
		body, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return nil, false
		}
		for _, field := range body.Elts {
			pair, ok := field.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			switch k, _ := pair.Key.(*ast.BasicLit); {
			case k == nil:
			case k.Value == `"code"`:
				code = pair.Value
			case k.Value == `"hint"`:
				hinted = !isEmptyString(pair.Value)
			}
		}
	}
	return code, hinted
}

func isEmptyString(e ast.Expr) bool {
	b, ok := e.(*ast.BasicLit)
	return ok && b.Kind == token.STRING && (b.Value == `""` || b.Value == "``")
}

func goLabel(e ast.Expr) string {
	if b, ok := e.(*ast.BasicLit); ok {
		return oneLine(b.Value)
	}
	return "<expr>"
}

// A constructor a scripting SDK builds refusals with, and how a call to it is
// judged.
type refusalSite struct {
	dir, ext string
	call     *regexp.Regexp
	// needsHint reports whether this call, given its argument text, is a
	// refusal made here that has to carry a hint.
	needsHint func(args string) bool
}

var (
	codeInBody = regexp.MustCompile(`(?:\bcode\s*:|["']code["']\s*:|\bcode\s*=)`)
	hintWord   = regexp.MustCompile(`\bhint\b|_HINT\b|Hint\b|\bhint_`)
)

func textHintless(t *testing.T) []string {
	t.Helper()
	always := func(string) bool { return true }
	madeHere := func(args string) bool { return codeInBody.MatchString(args) }
	rTwoArgs := func(args string) bool { return len(topLevelArgs(args)) < 3 }
	sites := []refusalSite{
		// refused gives invalid_name the hint every name refusal shares.
		{"node", ".js", regexp.MustCompile(`\brefused\(`), func(args string) bool { return !strings.Contains(args, `"invalid_name"`) }},
		{"node", ".js", regexp.MustCompile(`\brefusal\(`), always},
		{"node", ".js", regexp.MustCompile(`\bbadRequest\(`), always},
		{filepath.Join("python", "toft"), ".py", regexp.MustCompile(`\brefused\(`), func(args string) bool { return len(topLevelArgs(args)) < 3 }},
		{filepath.Join("python", "toft"), ".py", regexp.MustCompile(`\bbad_request\(`), func(args string) bool { return len(topLevelArgs(args)) < 2 }},
		{"node", ".js", regexp.MustCompile(`\bnew ToftError\(`), madeHere},
		{filepath.Join("python", "toft"), ".py", regexp.MustCompile(`\bToftError\(`), madeHere},
		{filepath.Join("r", "R"), ".R", regexp.MustCompile(`\btoft_refusal\(`), rTwoArgs},
	}
	var out []string
	for _, site := range sites {
		files, err := filepath.Glob(filepath.Join(site.dir, "*"+site.ext))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range files {
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			text := string(src)
			for _, loc := range site.call.FindAllStringIndex(text, -1) {
				if isDefinition(text, loc[0]) {
					continue
				}
				args, ok := callArgs(text, loc[1])
				if !ok {
					t.Errorf("%s: could not read the call at byte %d", name, loc[0])
					continue
				}
				if !site.needsHint(args) || hintWord.MatchString(args) {
					continue
				}
				label := "<expr>"
				if parts := topLevelArgs(args); len(parts) > 0 {
					label = oneLine(parts[0])
				}
				if len(label) > 90 {
					label = label[:90] + "…"
				}
				out = append(out, fmt.Sprintf("%s\t%s%s)", filepath.ToSlash(name),
					strings.TrimSpace(text[loc[0]:loc[1]]), label))
			}
		}
	}
	return out
}

// isDefinition skips the constructor's own definition: `function refused(`,
// `class ToftError(`, `toft_refusal <- function(`.
func isDefinition(text string, at int) bool {
	start := strings.LastIndex(text[:at], "\n") + 1
	line := text[start:at]
	return strings.Contains(line, "function") || strings.Contains(line, "class ") ||
		strings.Contains(line, "def ") || strings.HasPrefix(strings.TrimSpace(text[at:]), "ToftError(Exception)")
}

// callArgs returns the text between the call's opening parenthesis (just
// before from) and its matching close, skipping strings and comments.
func callArgs(text string, from int) (string, bool) {
	depth := 1
	for i := from; i < len(text); i++ {
		switch c := text[i]; c {
		case '"', '\'', '`':
			if j := skipString(text, i); j > i {
				i = j
			}
		case '#':
			// A comment in Python and R; in JavaScript only inside a string,
			// which skipString has already passed over — except a private
			// field (#name), which is never followed by a space.
			if i+1 < len(text) && (text[i+1] == ' ' || text[i+1] == '\'') {
				if nl := strings.IndexByte(text[i:], '\n'); nl > 0 {
					i += nl
				}
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return text[from:i], true
			}
		}
	}
	return "", false
}

// skipString returns the index of the closing quote of the string opening at
// i, handling a Python triple quote and backslash escapes.
func skipString(text string, i int) int {
	q := text[i]
	if q != '`' && strings.HasPrefix(text[i:], strings.Repeat(string(q), 3)) {
		if end := strings.Index(text[i+3:], strings.Repeat(string(q), 3)); end >= 0 {
			return i + 3 + end + 2
		}
	}
	for j := i + 1; j < len(text); j++ {
		switch text[j] {
		case '\\':
			j++
		case q:
			return j
		case '\n':
			if q != '`' {
				return j
			}
		}
	}
	return i
}

// topLevelArgs splits a call's argument text on its own commas.
func topLevelArgs(args string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(args); i++ {
		switch c := args[i]; c {
		case '"', '\'', '`':
			if j := skipString(args, i); j > i {
				i = j
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, args[start:i])
				start = i + 1
			}
		}
	}
	if rest := strings.TrimSpace(args[start:]); rest != "" {
		parts = append(parts, args[start:])
	}
	return parts
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// The literal reader itself: a hand-written Body with a code and no hint is
// found, and one with a hint is not.
func TestTheGuardReadsAHandWrittenErrorBody(t *testing.T) {
	src := `package toft
var a = &Error{Body: map[string]any{"code": "unexpected_redirect", "retryable": false}}
var b = &Error{Body: map[string]any{"code": "no_viewer", "hint": "use toft.Jobs"}}
var c = &Error{Message: "cannot reach Toft"}`
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flagged []string
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok {
			if code, hinted := goBodyLiteral(lit); code != nil && !hinted {
				flagged = append(flagged, goLabel(code))
			}
		}
		return true
	})
	if len(flagged) != 1 || flagged[0] != `"unexpected_redirect"` {
		t.Fatalf("flagged %v, want only the hintless unexpected_redirect", flagged)
	}
}
