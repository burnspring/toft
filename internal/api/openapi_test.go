package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"toft/internal/database"
)

// The document is a *coverage* artefact, so its failure mode is the one this
// whole area is written against: not a wrong answer anybody sees, but a route
// quietly missing from a scanner's list and a clean report nobody has a reason
// to distrust. The first test is therefore the drift guard, and the rest are
// the claims the document makes about itself.

func TestOpenAPINamesExactlyTheScannableRoutes(t *testing.T) {
	routes, err := RouteInventory()
	if err != nil {
		t.Fatalf("RouteInventory: %v", err)
	}
	doc, err := OpenAPIDocument("")
	if err != nil {
		t.Fatalf("OpenAPIDocument: %v", err)
	}

	want := map[string]bool{}
	for _, r := range ScannableRoutes(routes) {
		want[r.Method+" "+r.Pattern] = true
	}
	got := map[string]bool{}
	for pattern, ops := range doc.Paths {
		for method := range ops {
			got[strings.ToUpper(method)+" "+pattern] = true
		}
	}

	for key := range want {
		if !got[key] {
			t.Errorf("%s is served and is not in the document — a scanner told to use this would not test it", key)
		}
	}
	for key := range got {
		if !want[key] {
			t.Errorf("%s is in the document and is not served — a scanner told to use this would report a 404 as a finding", key)
		}
	}
	if len(want) == 0 {
		t.Fatal("no scannable routes at all, which means the filter is wrong rather than the router empty")
	}
}

// A wildcard is not a path and the four generated methods are not operations.
// Both are dropped in one place so the document and the crawl cannot disagree
// about what "every route" means.
func TestScannableRoutesDropsWildcardsAndGeneratedMethods(t *testing.T) {
	in := []Route{
		{Method: "GET", Pattern: "/api/v1/apps", Router: RouterPlatform},
		{Method: "OPTIONS", Pattern: "/api/v1/apps", Router: RouterPlatform},
		{Method: "HEAD", Pattern: "/api/v1/apps", Router: RouterPlatform},
		{Method: "GET", Pattern: "/apps/{slug}/*", Router: RouterPlatform},
		{Method: "GET", Pattern: "/handshake", Router: RouterApps},
	}
	got := ScannableRoutes(in)
	if len(got) != 1 || got[0].Method != "GET" || got[0].Pattern != "/api/v1/apps" {
		t.Fatalf("kept %+v, want only GET /api/v1/apps", got)
	}
}

// The extension exists so a keyed scan can say which routes it did not reach.
// If it inverted, the run would report the credential-management surface as
// scanned and clean, which is the most expensive lie this document could tell.
func TestOpenAPIMarksWhatAKeyCannotReach(t *testing.T) {
	doc, err := OpenAPIDocument("")
	if err != nil {
		t.Fatalf("OpenAPIDocument: %v", err)
	}

	for _, tc := range []struct {
		path, method string
		denied       bool
		why          string
	}{
		{"/api/v1/apps", "get", false, "listing apps is what a key is for"},
		{"/api/v1/me/api-keys", "post", true, "a token must not mint tokens"},
		{"/api/v1/me/api-keys", "get", true, "or inspect them"},
		{"/api/v1/admin/openapi.json", "get", false, "the scanner's own read"},
		// Outside the guarded group (D-24, fourth 2026-11-03 black-box list).
		{"/api/v1/auth/me", "get", false, "mounted behind apiKeyAuth, answers the key as its owner"},
		{"/api/v1/auth/providers", "get", false, "answers anyone; a key there is ignored"},
		{"/api/v1/config", "get", false, "answers anyone"},
		{"/api/v1/auth/cli/device/approve", "post", true, "a browser session and nothing else"},
		{"/api/v1/app/notify/{kind}", "post", true, "an app's own token"},
	} {
		op, ok := doc.Paths[tc.path][tc.method]
		if !ok {
			t.Fatalf("%s %s is not in the document", tc.method, tc.path)
		}
		if op.KeyDenied != tc.denied {
			t.Errorf("%s %s: x-toft-key-denied = %v, want %v (%s)",
				tc.method, tc.path, op.KeyDenied, tc.denied, tc.why)
		}
	}
}

// Every {param} in the pattern is declared, because a scanner substitutes what
// it is told about and leaves the rest as the literal string "{appID}" — which
// is a 404 in the site tree, and reads exactly like a route that was tested.
func TestOpenAPIDeclaresEveryPathParameter(t *testing.T) {
	doc, err := OpenAPIDocument("")
	if err != nil {
		t.Fatalf("OpenAPIDocument: %v", err)
	}
	for pattern, ops := range doc.Paths {
		var want []string
		for _, segment := range strings.Split(pattern, "/") {
			if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
				want = append(want, segment[1:len(segment)-1])
			}
		}
		for method, op := range ops {
			got := map[string]bool{}
			for _, p := range op.Parameters {
				if p.In != "path" || !p.Required {
					t.Errorf("%s %s: %q is %q and required=%v", method, pattern, p.Name, p.In, p.Required)
				}
				got[p.Name] = true
			}
			for _, name := range want {
				if !got[name] {
					t.Errorf("%s %s does not declare {%s}", method, pattern, name)
				}
			}
			if len(got) != len(want) {
				t.Errorf("%s %s declares %d parameters for %d in the path", method, pattern, len(got), len(want))
			}
		}
	}
}

// operationId is required to be unique or the document is invalid, and two
// routes sharing a handler is normal here — which is why it is derived from
// the method and the path and not from the handler name.
func TestOpenAPIOperationIDsAreUnique(t *testing.T) {
	doc, err := OpenAPIDocument("")
	if err != nil {
		t.Fatalf("OpenAPIDocument: %v", err)
	}
	seen := map[string]string{}
	for pattern, ops := range doc.Paths {
		for method, op := range ops {
			if first, dup := seen[op.OperationID]; dup {
				t.Errorf("operationId %q is on both %s and %s %s", op.OperationID, first, method, pattern)
			}
			seen[op.OperationID] = method + " " + pattern
		}
	}
}

// A wrong server URL is worse than none: a scanner would use it, and every
// request would go somewhere nobody meant.
func TestOpenAPIServerIsAbsentUnlessGiven(t *testing.T) {
	doc, err := OpenAPIDocument("")
	if err != nil {
		t.Fatalf("OpenAPIDocument: %v", err)
	}
	if len(doc.Servers) != 0 {
		t.Errorf("servers = %+v with no address given", doc.Servers)
	}
	doc, err = OpenAPIDocument("https://toft.example.com/")
	if err != nil {
		t.Fatalf("OpenAPIDocument: %v", err)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "https://toft.example.com" {
		t.Errorf("servers = %+v, want the address without its trailing slash", doc.Servers)
	}
}

// The gate. This response is the install's attack surface enumerated in one
// place, so it answers 404 to everybody who is not an administrator — the same
// status as the estate readers, and for a sharper version of the same reason.
func TestOpenAPIRouteIsAdminOnly(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "admin1", database.SystemRoleAdmin)
	router := srv.Router(http.NotFoundHandler())

	get := func(who string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/openapi.json", nil)
		if who != "" {
			req.AddCookie(sessionCookieFor(t, srv, who))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(""); rec.Code != http.StatusNotFound {
		t.Errorf("anonymous: status = %d, want 404", rec.Code)
	}
	if rec := get("owner"); rec.Code != http.StatusNotFound {
		t.Errorf("a publisher: status = %d, want 404", rec.Code)
	}

	rec := get("admin1")
	if rec.Code != http.StatusOK {
		t.Fatalf("an administrator: status = %d: %s", rec.Code, rec.Body.String())
	}
	var doc OpenAPIDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if doc.OpenAPI != openAPIVersion || len(doc.Paths) == 0 {
		t.Errorf("openapi = %q with %d paths", doc.OpenAPI, len(doc.Paths))
	}
	if _, ok := doc.Paths["/api/v1/admin/openapi.json"]["get"]; !ok {
		t.Error("the document does not name the route that served it")
	}
}

// keyRefusedOutsideGuard is the other half of keyAnsweredOutsideGuard: the
// routes outside the guarded group that refuse a key. Together they cover every
// such route, so a new one is a failure until somebody says which it is.
var keyRefusedOutsideGuard = map[string]bool{
	"GET /api/v1/auth/cli/device":           true, // a browser session
	"POST /api/v1/auth/cli/device/approve":  true, // a browser session
	"GET /api/v1/auth/app-grant":            true, // a browser session
	"POST /api/v1/git/hooks/{connectionID}": true, // the host's HMAC
}

func TestEveryRouteOutsideTheGuardIsClassified(t *testing.T) {
	router := routerForWalk(t)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + normalizeRoutePattern(route)
		if !strings.HasPrefix(route, "/api/v1/") || apiKeyAllowedRoutes[key] || apiKeyDeniedRoutes[key] {
			return nil
		}
		// An app's own token and a workload's: no key is ever answered there.
		if strings.HasPrefix(route, "/api/v1/app/") || strings.HasPrefix(route, "/api/v1/runner/") {
			if keyAnsweredOutsideGuard[key] {
				t.Errorf("%s is in keyAnsweredOutsideGuard, but its subtree takes no key", key)
			}
			return nil
		}
		answered, refused := keyAnsweredOutsideGuard[key], keyRefusedOutsideGuard[key]
		if answered == refused {
			t.Errorf("%s is outside the key guard and in %s of keyAnsweredOutsideGuard "+
				"and keyRefusedOutsideGuard — name it in exactly one", key,
				map[bool]string{true: "both", false: "neither"}[answered])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}
}
