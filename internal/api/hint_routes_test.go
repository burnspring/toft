package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// hintRoute is a route as a refusal's hint or message names it: a method and
// an /api/v1 path, the path ending at the first character a route cannot hold.
var hintRoute = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE) (/api/v1/[A-Za-z0-9_{}/.:*-]*[A-Za-z0-9_}*])`)

// servedRoute is one route the router serves, as segments.
type servedRoute struct {
	method string
	segs   []string
}

// matches says whether a named path is this route: a {param} on either side
// stands for any one segment (a hint's {id} is the router's {appID}, and its
// literal "teamsbot" is the router's {kind}); a trailing * any rest.
func (r servedRoute) matches(method, path string) bool {
	if method != r.method {
		return false
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, want := range r.segs {
		if want == "*" {
			return true
		}
		if i >= len(segs) {
			return false
		}
		got := segs[i]
		if strings.HasPrefix(want, "{") || strings.HasPrefix(got, "{") || got == "*" {
			continue
		}
		if got != want {
			return false
		}
	}
	return len(segs) == len(r.segs)
}

// Every route a string in this package names — a hint, an error, a pointer in
// a response — is one the router serves. A hint that sends a caller to a route
// that is not there is a next step that 404s (TOFT-BB-102: owner_group_empty
// named POST /api/v1/groups/{id}/members, which never existed; adding a member
// is PUT /api/v1/groups/{id}/members/{userId}). Comments are not checked —
// only string literals, which are what a caller reads.
func TestEveryRouteAStringNamesIsServed(t *testing.T) {
	var served []servedRoute
	platform, apps, err := inventoryRouters()
	if err != nil {
		t.Fatalf("building the routers: %v", err)
	}
	for _, router := range []*chi.Mux{platform, apps} {
		if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			served = append(served, servedRoute{method, strings.Split(strings.Trim(normalizeRoutePattern(route), "/"), "/")})
			return nil
		}); err != nil {
			t.Fatalf("walking router: %v", err)
		}
	}
	isServed := func(method, path string) bool {
		for _, r := range served {
			if r.matches(method, path) {
				return true
			}
		}
		return false
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var missing []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range hintRoute.FindAllStringSubmatchIndex(s, -1) {
				// A path cut off at a "/" goes on in the next operand of a
				// concatenation ("/apps/" + app.ID + ...): only its start is
				// here, so there is nothing whole to check.
				if m[1] < len(s) && s[m[1]] == '/' {
					continue
				}
				method, path := s[m[2]:m[3]], s[m[4]:m[5]]
				if !isServed(method, path) {
					missing = append(missing, fset.Position(lit.Pos()).String()+": "+method+" "+path)
				}
			}
			return true
		})
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("%s is named in a string but the router does not serve it", m)
	}
}
