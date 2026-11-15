package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The SCIM subtree's own guard, standing in for the one it does not get.
//
// `TestEveryRouteIsClassified` walks only `/api/v1/`, so a route under
// `/scim/v2` is neither classified nor flagged for being unclassified — it
// inherits no protection and no complaint. Whatever bounds this subtree is
// therefore the only thing bounding it, and this is that thing: the list below
// is the specification, and a route that appears without being added here fails
// the build.
//
// The list is deliberately narrow. Everything a directory may do to this
// install's people is on it, and everything on it is something ADR §48's
// revisit argued for.
var scimRoutes = map[string]string{
	// Discovery. Okta will not finish configuring a connector without these.
	"GET /scim/v2/ServiceProviderConfig": "what this endpoint supports",
	"GET /scim/v2/ResourceTypes":         "which resources it serves",
	"GET /scim/v2/Schemas":               "their attributes",

	// The lookup an IdP does before deciding whether to create.
	"GET /scim/v2/Users":      "userName eq — never an unfiltered enumeration",
	"GET /scim/v2/Users/{id}": "one account, scoped to the asking directory",

	// Deprovisioning, which is the whole feature.
	"PATCH /scim/v2/Users/{id}": "active true or false",

	// Group membership, over the rows the claim reconciler already writes.
	"PATCH /scim/v2/Groups/{id}": "member add and remove",

	// The refusals, which are routes on purpose: an IdP that receives a clear
	// 501 configures around it, and one that receives a 404 concludes the base
	// URL is wrong and stops.
	"POST /scim/v2/Users":         "501 — one provisioning path, and it is the sign-in",
	"PUT /scim/v2/Users/{id}":     "501 — a wholesale replace is a create in disguise",
	"DELETE /scim/v2/Users/{id}":  "501 — an account may own apps",
	"POST /scim/v2/Groups":        "501 — a group is a local row an administrator named",
	"DELETE /scim/v2/Groups/{id}": "501 — likewise",
	"GET /scim/v2/Groups":         "501 — a directory's pre-check gets a SCIM error body, not chi's 404",
	"GET /scim/v2/Groups/{id}":    "501 — likewise",
	"PUT /scim/v2/Groups/{id}":    "501 — a wholesale replace of a group is not a membership change",
}

func TestEverySCIMRouteIsOnTheList(t *testing.T) {
	router := routerForWalk(t)

	var unlisted []string
	seen := map[string]bool{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/scim/") {
			return nil
		}
		key := method + " " + normalizeRoutePattern(route)
		seen[key] = true
		if _, ok := scimRoutes[key]; !ok {
			unlisted = append(unlisted, key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}

	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		t.Errorf("these SCIM routes are on no list, and nothing else bounds this subtree:\n  %s",
			strings.Join(unlisted, "\n  "))
	}

	// The other direction: a list that names a route nobody serves is a list
	// somebody trusted and should not have.
	var missing []string
	for key := range scimRoutes {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the list names routes the router does not serve:\n  %s", strings.Join(missing, "\n  "))
	}
}

// Nothing under /scim reaches the platform API's authentication. A cookie
// session, a personal key or a deploy key presented here authenticates nobody:
// the only credential this subtree accepts is a directory's token, and the
// point of checking is that a future edit adding `auth.Middleware` over the
// whole router would silently give every SCIM route a second way in.
func TestSCIMRoutesAreNotOnThePlatformAPI(t *testing.T) {
	router := routerForWalk(t)

	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/scim/") {
			return nil
		}
		if strings.Contains(route, "/api/") {
			t.Errorf("%s %s is mounted under the platform API", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}
}
