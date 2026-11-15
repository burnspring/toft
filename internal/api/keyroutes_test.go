package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/database"
)

// routerForWalk builds the platform router for route enumeration only. No
// database, no JWT service: nothing here serves a request, it only asks the
// router what it would serve.
//
// Through inventoryRouters, which is also what `toft routes` and the ZAP
// crawl are built from — so what these tests certify and what a scanner is
// handed are the same router, not two constructions of one.
func routerForWalk(t *testing.T) *chi.Mux {
	t.Helper()
	platform, _, err := inventoryRouters()
	if err != nil {
		t.Fatalf("building the router: %v", err)
	}
	return platform
}

// exemptRoutePrefixes are the two subtrees the allow-list does not govern,
// because apiKeyRouteGuard is not mounted over them.
//
//   - /api/v1/auth: only /auth/me sits behind apiKeyAuth. Everywhere else a
//     presented key is simply ignored and the caller is anonymous, which is the
//     right answer for a login form.
//   - /api/v1/app: an app's own outbound token (tofta_). The opposite direction
//     from a key — an toft_ key lets the world into an app, this lets the app out
//     — and appTokenAuth refuses every other credential class, so the
//     allow-list has nothing to say about it.
//   - /api/v1/runner: run-scoped bootstrap tokens, a different audience that
//     never sees a session or a personal key.
var exemptRoutePrefixes = []string{"/api/v1/auth/", "/api/v1/runner/", "/api/v1/app/"}

// exemptRoutes are the individually-mounted public routes. Each is registered
// outside the authenticated /api/v1 group and requires no credential from
// anyone, so apiKeyRouteGuard never runs over them and listing them in the
// allow-list would imply a gate that is not there.
var exemptRoutes = map[string]bool{
	"GET /api/v1/config": true,
	// The git webhook. The host presents an HMAC over the body and nothing
	// else: there is no session, key or role for either guard to reason about,
	// so classifying it would be certifying a gate that is not there. What
	// bounds it is the signature, the body cap and the rate limiter.
	"POST /api/v1/git/hooks/{connectionID}": true,
}

// TestEveryRouteIsClassified is what makes the allow-list true a year from
// now. Naming the dangerous routes in a comment is a promise; this is the
// mechanism.
//
// It walks the real router and fails on any /api/v1 route that appears in
// neither apiKeyAllowedRoutes nor apiKeyDeniedRoutes. A new endpoint is
// therefore unreachable by API key *and* a build failure until somebody
// decides which it is — the decision cannot be skipped by not thinking about
// it, which is the only way this kind of list ever goes wrong.
func TestEveryRouteIsClassified(t *testing.T) {
	router := routerForWalk(t)

	var unclassified, both []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		for _, p := range exemptRoutePrefixes {
			if strings.HasPrefix(route, p) {
				return nil
			}
		}
		// Normalized through the same function the guard uses, so the test can
		// never certify an entry the runtime would miss.
		key := method + " " + normalizeRoutePattern(route)
		if exemptRoutes[key] {
			return nil
		}
		allowed, denied := apiKeyAllowedRoutes[key], apiKeyDeniedRoutes[key]
		switch {
		case allowed && denied:
			both = append(both, key)
		case !allowed && !denied:
			unclassified = append(unclassified, key)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}

	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("%d route(s) are in neither apiKeyAllowedRoutes nor apiKeyDeniedRoutes.\n"+
			"They are refused to API keys by default, which is safe but probably not "+
			"deliberate. Add each to one map in keyroutes.go:\n\t%s",
			len(unclassified), strings.Join(unclassified, "\n\t"))
	}
	sort.Strings(both)
	if len(both) > 0 {
		t.Errorf("%d route(s) appear in BOTH maps, so the intent is unreadable:\n\t%s",
			len(both), strings.Join(both, "\n\t"))
	}
}

// TestNoStaleRouteEntries catches the other direction of drift: an entry left
// behind after its route was renamed or removed. A stale allow entry is dead
// permission that reads as live, which is worse than useless in a file whose
// job is to be read.
func TestNoStaleRouteEntries(t *testing.T) {
	router := routerForWalk(t)

	live := map[string]bool{}
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		live[method+" "+normalizeRoutePattern(route)] = true
		return nil
	}); err != nil {
		t.Fatalf("walking router: %v", err)
	}

	var stale []string
	for _, m := range []map[string]bool{apiKeyAllowedRoutes, apiKeyDeniedRoutes} {
		for key := range m {
			if !live[key] {
				stale = append(stale, key)
			}
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d entry/entries in keyroutes.go name routes the router does not serve:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}
}

// TestCredentialRoutesAreDenied states the invariant directly rather than
// leaving it implicit in a large map. If any of these ever became reachable by
// key, a leaked token could mint its replacement — and the incident would have
// no bottom.
func TestCredentialRoutesAreDenied(t *testing.T) {
	mustDeny := []string{
		"POST /api/v1/me/api-keys",
		"DELETE /api/v1/me/api-keys/{keyID}",
		"GET /api/v1/admin/api-keys",
		"PATCH /api/v1/users/{id}/role",
		"POST /api/v1/users/{id}/lock",
		"PUT /api/v1/apps/{appID}/grants/{principalType}/{principalID}",
		"POST /api/v1/apps/{appID}/transfer",
		"PUT /api/v1/admin/email-settings",
		"POST /api/v1/admin/auth-providers",
		"PUT /api/v1/system/settings",
		"POST /api/v1/admin/promotion-requests/{reqID}/approve",
	}
	for _, route := range mustDeny {
		if apiKeyAllowedRoutes[route] {
			t.Errorf("%s is reachable by API key; it manages a credential or an "+
				"authorization decision and must not be", route)
		}
		if !apiKeyDeniedRoutes[route] {
			t.Errorf("%s is not explicitly denied — it would be refused by default, "+
				"but the refusal should be deliberate and readable", route)
		}
	}
}

// The third map, and the property that keeps it honest: every route a deploy
// key may reach, a personal API key may reach too.
//
// A strict subset rather than a second independently-maintained list, because
// the two lists would otherwise drift and the drift would be invisible. It also
// makes the security argument short: the deploy class can only ever narrow the
// personal class's surface, so nothing needs to be re-argued from scratch when
// somebody adds a route to one of them.
//
// The reverse containment is deliberately *not* asserted — a personal key does
// far more than deploy, and that is the point of the subset.
func TestDeployKeyRoutesAreASubsetOfPersonalKeyRoutes(t *testing.T) {
	var notAllowed, alsoDenied []string
	for route := range deployKeyAllowedRoutes {
		if !apiKeyAllowedRoutes[route] {
			notAllowed = append(notAllowed, route)
		}
		if apiKeyDeniedRoutes[route] {
			alsoDenied = append(alsoDenied, route)
		}
	}

	sort.Strings(notAllowed)
	if len(notAllowed) > 0 {
		t.Errorf("%d route(s) are reachable by a deploy key but not by a personal API key, so the "+
			"deploy class is no longer a subset and its surface has to be argued on its own:\n\t%s",
			len(notAllowed), strings.Join(notAllowed, "\n\t"))
	}
	sort.Strings(alsoDenied)
	if len(alsoDenied) > 0 {
		t.Errorf("%d route(s) are in deployKeyAllowedRoutes AND apiKeyDeniedRoutes — a route "+
			"explicitly denied to a person's own key must never be reachable by a credential "+
			"nobody owns:\n\t%s", len(alsoDenied), strings.Join(alsoDenied, "\n\t"))
	}
}

// The governing sentence, as behaviour: a deploy key may never manage a
// credential or change who may do what.
//
// TestDeployKeyRoutesAreASubsetOfPersonalKeyRoutes proves the list is narrow;
// this proves the list is narrow in the direction that matters. It walks the
// real router rather than the map, so a route that exists and was never
// classified is caught here too — for this class an unclassified route is
// refused, and that default is what this asserts.
func TestADeployKeyCannotReachCredentialsOrAuthorization(t *testing.T) {
	router := routerForWalk(t)

	forbidden := []string{
		"users", "groups", "grants", "transfer", "api-keys", "deploy-keys",
		"access-requests", "promotion-requests", "auth/", "settings",
		"source-policy", "providers", "local-admin",
	}

	// The two stated exceptions, and they are exceptions to the *substring*
	// rather than to the rule. "groups" catches every route under /groups, and
	// two of those are reads of the directory: names, member counts and the
	// group's own address. Neither manages a credential and neither changes who
	// may do what — a deploy key that lists groups can pick the one that will
	// own an app it creates, which is the joint ADR §51 describes, and can name
	// the one to tell when something fails.
	//
	// What stays protected is the roster: handleGetGroup withholds membership
	// from a principal that is neither an administrator nor a member, and a
	// credential that belongs to nobody is neither.
	// TestGetGroupWithholdsRosterFromADeployKey states that as behaviour, which
	// is what makes this pair safe to list here rather than a hole.
	//
	// Spelled as whole routes, not as a substring subtracted from a substring:
	// POST /groups and PUT /groups/{id}/members/{userId} must keep failing this
	// test, and an exception written as "groups is fine sometimes" would not
	// make them.
	allowedReads := map[string]bool{
		"GET /api/v1/groups":      true,
		"GET /api/v1/groups/{id}": true,
	}

	var reachable []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		key := method + " " + normalizeRoutePattern(route)
		if !deployKeyAllowedRoutes[key] || allowedReads[key] {
			return nil
		}
		for _, bad := range forbidden {
			if strings.Contains(route, bad) {
				reachable = append(reachable, key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}

	sort.Strings(reachable)
	if len(reachable) > 0 {
		t.Errorf("a deploy key can reach %d route(s) that touch credentials or authorization. "+
			"A stolen CI token that can mint its replacement or grant itself a role turns a "+
			"bounded incident into an unbounded one:\n\t%s",
			len(reachable), strings.Join(reachable, "\n\t"))
	}
}

// A store key policy in one test: using a store is operation, and deciding who
// may use it is authorization.
//
// The upload routes are the intended use of the whole feature — an unattended
// nightly pushing a file — and they decide nothing: an administrator decided
// the grant, and the key reaches exactly what its owner reaches. The grant
// routes are the other side of this file's central line. A key that could
// grant itself `full` on a store would be a key that promotes its own holder.
func TestStoreUploadIsOperationAndGrantingIsNot(t *testing.T) {
	for _, route := range []string{
		"GET /api/v1/store",
		"GET /api/v1/store/{slug}/objects",
		"POST /api/v1/store/{slug}/objects",
		"GET /api/v1/store/{slug}/objects/*",
		"DELETE /api/v1/store/{slug}/objects/*",
		"POST /api/v1/store/{slug}/folders",
		"DELETE /api/v1/store/{slug}/folders/*",
	} {
		if !apiKeyAllowedRoutes[route] {
			t.Errorf("%s is not reachable by a personal key; an unattended upload is "+
				"the use this feature exists for", route)
		}
		if deployKeyAllowedRoutes[route] {
			t.Errorf("%s is reachable by a deploy key. A grant belongs to a person and a "+
				"deploy key belongs to nobody, so there is no answer to which grants it holds", route)
		}
	}

	for _, route := range []string{
		"GET /api/v1/admin/storage/{id}/grants",
		"PUT /api/v1/admin/storage/{id}/grants",
		"DELETE /api/v1/admin/storage/{id}/grants/{subject}",
	} {
		if apiKeyAllowedRoutes[route] {
			t.Errorf("%s is reachable by API key; it decides who may do what", route)
		}
		if !apiKeyDeniedRoutes[route] {
			t.Errorf("%s is not explicitly denied — it would be refused by default, "+
				"but the refusal should be deliberate and readable", route)
		}
	}
}

// Asking is not managing, and the two request ladders answer alike.
//
// A personal key could ask for access to an app and could not ask for its
// owner's promotion — a difference that read as a rule about escalation and was
// only an accident of when each half was written. Both are a request a human
// decides; neither grants anything by being made. What the denied map keeps is
// the deciding, on both ladders, which is where "a key never manages
// authorization" actually bites.
func TestBothRequestLaddersClassifyAskingAndDecidingAlike(t *testing.T) {
	asking := []string{
		"POST /api/v1/apps/{appID}/access-requests",
		"GET /api/v1/apps/{appID}/access-requests",
		"DELETE /api/v1/apps/{appID}/access-requests/{reqID}",
		"POST /api/v1/me/promotion-requests",
		"GET /api/v1/me/promotion-requests",
		"DELETE /api/v1/me/promotion-requests/{reqID}",
	}
	for _, route := range asking {
		if !apiKeyAllowedRoutes[route] {
			t.Errorf("%s is not reachable by a personal key. Asking is not managing, and "+
				"the two ladders are classified the same way or neither is defensible", route)
		}
		if apiKeyDeniedRoutes[route] {
			t.Errorf("%s is in both maps", route)
		}
	}

	deciding := []string{
		"POST /api/v1/apps/{appID}/access-requests/{reqID}/approve",
		"POST /api/v1/apps/{appID}/access-requests/{reqID}/deny",
		"GET /api/v1/admin/promotion-requests",
		"POST /api/v1/admin/promotion-requests/{reqID}/approve",
		"POST /api/v1/admin/promotion-requests/{reqID}/deny",
	}
	for _, route := range deciding {
		if apiKeyAllowedRoutes[route] {
			t.Errorf("%s is reachable by API key; it decides who may do what", route)
		}
		if !apiKeyDeniedRoutes[route] {
			t.Errorf("%s is not explicitly denied — it would be refused by default, "+
				"but the refusal should be deliberate and readable", route)
		}
	}

	// A credential that belongs to nobody has nobody to ask on behalf of.
	for _, route := range append(append([]string{}, asking...), deciding...) {
		if deployKeyAllowedRoutes[route] {
			t.Errorf("%s is reachable by a deploy key; a request is a person's and a "+
				"deploy key is not a person", route)
		}
	}
}

// D12's line, in two halves: a pipeline may start the work its deploy needs,
// and may not change what that work is.
//
// The argument for the first half is the list's own — "a pipeline must be able
// to undo its own deploy without being able to destroy what it deployed onto".
// Running a migration job is on the deploy side of that: the job is code this
// same pipeline just shipped, running under the app's own identity, and a
// pipeline that can push arbitrary code to the app can already cause it to run.
func TestDeployKeyMayStartAJob(t *testing.T) {
	for _, route := range []string{
		"POST /api/v1/apps/{appID}/job-runs",
		"GET /api/v1/apps/{appID}/job-runs/{runID}",
		"GET /api/v1/apps/{appID}/job-runs/{runID}/logs",
		// BB-108: and find the job and its runs without an id handed over.
		"GET /api/v1/apps/{appID}/jobs",
		"GET /api/v1/apps/{appID}/job-runs",
	} {
		if !deployKeyAllowedRoutes[route] {
			t.Errorf("a deploy key cannot %s — a pipeline that can ship the migration "+
				"cannot run it, which is the half-automated shape this class exists to close", route)
		}
	}
}

// The other half. Reshaping what a job *is* edits the app's configuration
// rather than shipping it, and configuration is not a step in a deploy.
func TestDeployKeyMayNotShapeAJob(t *testing.T) {
	for _, route := range []string{
		"PATCH /api/v1/apps/{appID}/jobs/{jobID}",
	} {
		if deployKeyAllowedRoutes[route] {
			t.Errorf("a deploy key can %s — that is an edit to the app's configuration, "+
				"not a step in shipping it", route)
		}
	}
}

// The chain, in one assertion: read ⊂ deploy ⊂ personal.
//
// The middle link is the one that needs stating. The outer two already have
// tests, but a scope inside a class is only a boundary if the narrow list can
// never reach past the wide one — otherwise "read" is a badge on a row rather
// than a fence, and the whole argument for a scope instead of a fourth class
// falls over. Chained here rather than in two tests so that widening the middle
// cannot pass by satisfying one of them.
func TestDeployKeyScopesAreAChainOfSubsets(t *testing.T) {
	var escapes, notPersonal []string
	for route := range deployKeyReadRoutes {
		if !deployKeyAllowedRoutes[route] {
			escapes = append(escapes, route)
		}
		if !apiKeyAllowedRoutes[route] {
			notPersonal = append(notPersonal, route)
		}
	}
	sort.Strings(escapes)
	if len(escapes) > 0 {
		t.Errorf("%d route(s) are reachable by a read-scoped deploy key and not by a "+
			"deploy-scoped one, so the scope is no longer a narrowing of the class:\n\t%s",
			len(escapes), strings.Join(escapes, "\n\t"))
	}
	sort.Strings(notPersonal)
	if len(notPersonal) > 0 {
		t.Errorf("%d route(s) are reachable by a read-scoped deploy key and not by a personal "+
			"key, which breaks the chain at its outer link:\n\t%s",
			len(notPersonal), strings.Join(notPersonal, "\n\t"))
	}
}

// The read scope is reads, and the word has to keep meaning that.
//
// A GET that changes something is still a mutation, but the cheap and durable
// property here is the method: nothing on this list may be anything but a GET.
// The day somebody adds a POST to it, this fails before the review does.
func TestTheReadScopeIsOnlyReads(t *testing.T) {
	for route := range deployKeyReadRoutes {
		if !strings.HasPrefix(route, "GET ") {
			t.Errorf("%s is on the read scope and is not a GET — the scope's whole claim is "+
				"that a compromise of whatever holds the key cannot push code", route)
		}
	}
}

// What promotion actually needs, named as behaviour rather than left to be
// inferred from a map.
//
// The archive is the load-bearing one: promotion does not build, so the bytes
// come off another install by a read, and a class that may write a release and
// not read one describes a pipeline that can only ship code it compiled itself.
func TestAReadScopedKeyMayPullAReleaseAndItsVerdict(t *testing.T) {
	for _, route := range []string{
		"GET /api/v1/apps",
		"GET /api/v1/apps/{appID}",
		"GET /api/v1/apps/by-slug/{slug}",
		"GET /api/v1/apps/{appID}/deployments",
		"GET /api/v1/apps/{appID}/deployments/{deployID}",
		"GET /api/v1/apps/{appID}/deployments/{deployID}/archive",
		"GET /api/v1/apps/{appID}/dependencies",
		"GET /api/v1/groups",
		"GET /api/v1/groups/{id}",
	} {
		if !deployKeyReadRoutes[route] {
			t.Errorf("a read-scoped deploy key cannot %s — promotion pulls a release and reads "+
				"the platform's own verdict on it, and neither is a build", route)
		}
	}
}

// The other half, and the reason the scope exists at all: the credential an
// estate tool holds on the install it *runs on* must not be able to push to the
// apps on that install, its own included.
func TestAReadScopedKeyMayNotPush(t *testing.T) {
	for _, route := range []string{
		"POST /api/v1/apps",
		"POST /api/v1/apps/upload",
		"POST /api/v1/apps/{appID}/deploy",
		"POST /api/v1/apps/{appID}/upload",
		"POST /api/v1/apps/{appID}/deployments/{deployID}/rollback",
		"POST /api/v1/apps/{appID}/restart",
		"POST /api/v1/apps/{appID}/stop",
		"POST /api/v1/apps/{appID}/job-runs",
	} {
		if deployKeyReadRoutes[route] {
			t.Errorf("a read-scoped deploy key can %s — the scope is the blast radius and this "+
				"is outside it", route)
		}
		if !deployKeyAllowedRoutes[route] {
			t.Errorf("a deploy-scoped key cannot %s, which is the scope's whole purpose", route)
		}
	}
}

// An unrecognised scope resolves to the narrow list, because this file is an
// allow-list that fails closed and a row carrying a word nobody has implemented
// must not be the widest thing on the install.
func TestAnUnknownScopeGetsTheNarrowList(t *testing.T) {
	for _, scope := range []string{"", "admin", "DEPLOY", "read-write"} {
		got := deployKeyRoutesForScope(scope)
		if len(got) != len(deployKeyReadRoutes) {
			t.Errorf("scope %q resolved to a list of %d routes; an unrecognised scope must "+
				"fall to the read list of %d", scope, len(got), len(deployKeyReadRoutes))
		}
	}
	if len(deployKeyRoutesForScope(database.DeployKeyScopeDeploy)) != len(deployKeyAllowedRoutes) {
		t.Error("the deploy scope did not resolve to the deploy list")
	}
	if len(deployKeyRoutesForScope(database.DeployKeyScopeRead)) != len(deployKeyReadRoutes) {
		t.Error("the read scope did not resolve to the read list")
	}
}

// --- Personal key scopes -----------------------------------------------------

// guardProbe runs the real apiKeyRouteGuard over a stub handler, as a key of
// the given scope, and reports what came back.
//
// It builds the router so the guard resolves patterns exactly as it does in
// production — routePattern goes through s.mux — and it never reaches a
// handler, so there is no database and no fixture. The stub is what "reached"
// means: the guard let the request past, which is the only thing under test.
func guardProbe(t *testing.T, s *Server, scope, method, path string) (reached bool, status int, code string) {
	t.Helper()
	var got bool
	h := s.apiKeyRouteGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = true
	}))
	r := httptest.NewRequest(method, path, nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), &auth.Principal{
		Type: database.PrincipalTypeUser, ID: "alice", SystemRole: "system_admin",
		APIKeyID: "key-1", APIKeyName: "probe", APIKeyScope: scope,
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got {
		return true, w.Code, ""
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return false, w.Code, body.Code
}

// guardServer builds a Server whose router is mounted, which is all the guard
// needs. Same construction as routerForWalk, kept separate because that one
// returns the mux and this one needs the Server the mux belongs to.
func guardServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{cfg: &config.Config{}}
	if _, ok := s.Router(http.NotFoundHandler()).(*chi.Mux); !ok {
		t.Fatal("the platform router is not a *chi.Mux")
	}
	return s
}

// concreteRoute turns an allow-list entry into a request the router will match:
// every {param} and every trailing wildcard becomes a segment no static route
// claims. Splitting here rather than at each call site so the two scope walks
// below cannot disagree about what they probed.
func concreteRoute(route string) (method, path string, ok bool) {
	method, pattern, found := strings.Cut(route, " ")
	if !found {
		return "", "", false
	}
	segs := strings.Split(pattern, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") || seg == "*" {
			segs[i] = "zzprobe"
		}
	}
	return method, strings.Join(segs, "/"), true
}

// The read scope, walked rather than listed: every GET on the allow-list is
// reached and every other method on it is refused by name.
//
// Walking the map is the point — a fixture list of routes would drift from the
// allow-list the first time somebody added an entry, and the drift would be
// invisible because the test would keep passing on the routes it still knew
// about. This is TestTheReadScopeIsOnlyReads' equivalent for a scope that has
// no list of its own to check.
func TestAReadScopedKeyReachesEveryReadAndNoWrite(t *testing.T) {
	s := guardServer(t)
	for route := range apiKeyAllowedRoutes {
		method, path, ok := concreteRoute(route)
		if !ok {
			t.Errorf("%q is not a %q-separated method and pattern", route, " ")
			continue
		}
		// The probe is only meaningful if it lands where the entry says. A
		// mismatch means the allow-list holds a pattern the router does not
		// serve, which TestEveryRouteIsClassified would not catch.
		r := httptest.NewRequest(method, path, nil)
		if got := method + " " + s.routePattern(r); got != route {
			t.Errorf("probing %s resolved to %q, not %q — the entry and the router disagree",
				path, got, route)
			continue
		}
		reached, status, code := guardProbe(t, s, database.UserKeyScopeRead, method, path)
		if method == http.MethodGet {
			if !reached {
				t.Errorf("a read-scoped key is refused %s with %d/%s — the scope's whole "+
					"promise is that it reads everything its owner reads", route, status, code)
			}
			continue
		}
		if reached {
			t.Errorf("a read-scoped key reached %s — it writes, and the word says it does not",
				route)
			continue
		}
		if status != http.StatusForbidden || code != "key_read_scope" {
			t.Errorf("a read-scoped key was refused %s with %d/%q; want 403/key_read_scope, "+
				"which is the refusal that names the remedy", route, status, code)
		}
	}
}

// The other scope, and the reason it is worth a walk of its own: `full` is the
// behaviour every key on every install had before this feature, so a branch
// that fired for it would be a silent, install-wide regression.
func TestAFullScopedKeyReachesTheWholeAllowList(t *testing.T) {
	s := guardServer(t)
	for route := range apiKeyAllowedRoutes {
		method, path, ok := concreteRoute(route)
		if !ok {
			continue
		}
		if reached, status, code := guardProbe(t, s, database.UserKeyScopeFull, method, path); !reached {
			t.Errorf("a full-scope key is refused %s with %d/%s — that is what every key "+
				"minted before scopes existed can do", route, status, code)
		}
	}
}

// The order of the two fences, stated as behaviour.
//
// A read-scoped key asking for something no key may do must be told the route
// needs a session, not that its scope is too narrow: a wider key would fail
// identically, and the other answer sends the caller off to mint one and spend
// an afternoon on the wrong remedy.
func TestTheAllowListIsAskedBeforeTheScope(t *testing.T) {
	s := guardServer(t)
	for _, route := range []string{
		"POST /api/v1/me/api-keys",
		"POST /api/v1/me/sessions/revoke",
		"PUT /api/v1/admin/storage/{id}/grants",
	} {
		if apiKeyAllowedRoutes[route] {
			t.Fatalf("%s is on the allow-list — this test needs a route no key reaches", route)
		}
		method, path, _ := concreteRoute(route)
		reached, status, code := guardProbe(t, s, database.UserKeyScopeRead, method, path)
		if reached {
			t.Errorf("a read-scoped key reached %s, which no key may reach at all", route)
			continue
		}
		if status != http.StatusForbidden || code != "session_required" {
			t.Errorf("a read-scoped key was refused %s with %d/%q; want 403/session_required — "+
				"the scope is not what stopped it and saying so would misdirect the fix",
				route, status, code)
		}
	}
}

// **A path that matches no route is not a credential refusal.**
//
// The guard used to ask the allow-list for the pattern the router resolved, and
// an unresolvable path resolves to "" — which is in no map, so a typo came back
// as 403 "this route requires an interactive session". A confident, specific,
// wrong diagnosis, and the worst shape one can have for an unattended caller:
// the documented recovery for that answer is to mint a new key or ask an
// administrator, neither of which is retryable, so a careful agent stops and
// escalates to a human over a mistyped path.
//
// It now falls through to the router, which 404s — the answer the request would
// have got with any credential, including none.
func TestAnUnknownRouteIsNotReportedAsACredentialProblem(t *testing.T) {
	s := guardServer(t)
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nosuchroute"},
		{http.MethodGet, "/api/v1/apps/zzprobe/nosuchsubroute"},
		{http.MethodPost, "/api/v1/nosuchroute"},
		{http.MethodGet, "/api/v1/api/v1/apps"},
	} {
		for _, scope := range []string{database.UserKeyScopeRead, database.UserKeyScopeFull} {
			reached, status, code := guardProbe(t, s, scope, probe.method, probe.path)
			if !reached {
				t.Errorf("%s %s as a %s key was refused %d/%q by the credential guard; "+
					"no route matches it, so nothing here has anything to decide",
					probe.method, probe.path, scope, status, code)
			}
		}
	}
}

// An encoded slash must not move a denied route out of the guard's sight. chi
// routes on RawPath, so `a%2Fb` is one `{id}` to the handler; matched on the
// decoded path it was a route nobody registered, and passed through (API-AUTH-3).
func TestAnEncodedSlashDoesNotHideADeniedRoute(t *testing.T) {
	s := guardServer(t)
	reached, status, _ := guardProbe(t, s, database.UserKeyScopeFull, http.MethodGet, "/api/v1/users/a%2Fb/api-keys")
	if reached {
		t.Errorf("GET /api/v1/users/a%%2Fb/api-keys reached the handler (%d); it is a denied route", status)
	}
}

// The deploy class had the identical bug for the identical reason, and it is
// worth its own probe because its guard is a different function with its own
// two-list structure.
func TestAnUnknownRouteIsNotADeployKeyRefusal(t *testing.T) {
	s := guardServer(t)
	var reached bool
	h := s.deployKeyRouteGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/nosuchroute", nil)
	r = r.WithContext(auth.WithPrincipal(r.Context(), &auth.Principal{
		Type: database.PrincipalTypeUser, ID: "dk-1",
		DeployKeyID: "dk-1", DeployKeyScope: database.DeployKeyScopeDeploy,
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !reached {
		t.Errorf("an unmatched path was refused %d as a deploy-key problem: %s",
			w.Code, w.Body.String())
	}
}

// And what the caller gets instead: a 404 that says so in JSON, with a code
// that tells it apart from the platform answering "no such app".
//
// Without the split an unmatched /api/ path fell through to the SPA — 200 and
// an HTML document, handed to a caller holding a JSON decoder — so "no such
// route" was not something the platform could express at all.
func TestAnUnknownAPIPathIs404JSONAndNotTheSPA(t *testing.T) {
	s := guardServer(t)
	mux := s.Router(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html>the SPA"))
	}))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/nosuchroute", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /api/v1/nosuchroute = %d, want 404", w.Code)
	}
	var body struct{ Error, Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer was not JSON (%v): %s", err, w.Body.String())
	}
	if body.Code != "route_not_found" {
		t.Errorf("code = %q, want route_not_found — the distinguisher between a path "+
			"nobody registered and the platform answering that an id names nothing", body.Code)
	}
	if !strings.Contains(body.Error, "/api/v1/nosuchroute") {
		t.Errorf("the refusal does not quote the path it did not recognise: %q", body.Error)
	}

	// Everything else is still the SPA, which is what makes a deep link into a
	// client-side route work.
	r = httptest.NewRequest(http.MethodGet, "/settings/some-spa-route", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "the SPA") {
		t.Errorf("an unknown non-API path = %d %q, want the SPA", w.Code, w.Body.String())
	}
}

// An unrecognised scope cannot write, for the reason its deploy-class twin
// resolves to the narrow list: this is an allow-list that fails closed, and a
// row carrying a word nobody has implemented must not be the widest credential
// on the install. Get this backwards and the failure is invisible.
func TestAnUnknownUserKeyScopeCannotWrite(t *testing.T) {
	for _, scope := range []string{"", "read", "readonly", "FULL", "admin", "deploy"} {
		if userKeyScopeWrites(scope) {
			t.Errorf("scope %q may write; only %q may, and everything else narrows",
				scope, database.UserKeyScopeFull)
		}
	}
	if !userKeyScopeWrites(database.UserKeyScopeFull) {
		t.Error("the full scope may not write, which is every key that existed before this column")
	}
}

// The second containment path, beside the existing chain.
//
// TestDeployKeyScopesAreAChainOfSubsets asserts read ⊂ deploy ⊂ personal, which
// goes on holding against personal `full`. This is the other route through the
// lattice — a deploy key's read list is inside a *read-scoped* personal key —
// and it holds by construction rather than by curation, because every entry on
// that list is a GET and every GET on the allow-list is what `read` means.
//
// The two scopes are incomparable in both other directions, which is correct
// and deliberately left unasserted: it is a fact about the lists, not a rule
// either of them has to keep.
func TestDeployKeyReadsAreInsideAReadScopedPersonalKey(t *testing.T) {
	for route := range deployKeyReadRoutes {
		method, _, _ := strings.Cut(route, " ")
		if method != http.MethodGet {
			t.Errorf("%s is on the deploy read list and is not a GET, so a read-scoped "+
				"personal key cannot reach it", route)
		}
		if !apiKeyAllowedRoutes[route] {
			t.Errorf("%s is reachable by a read-scoped deploy key and not by a personal key "+
				"at all", route)
		}
	}
}

// A method a route does not take is a 405 in the envelope, carrying
// method_not_allowed and the methods it does take. chi's default was an empty
// body, so `code` was absent (CLI.28: never absent).
func TestAMethodARouteDoesNotTakeIs405WithACode(t *testing.T) {
	s := guardServer(t)
	mux := s.Router(http.NotFoundHandler())
	// Every depth of the router, not one route: the empty 405 was chi's
	// default for any wrong verb on a route that exists.
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/tags"},
		{http.MethodPatch, "/api/v1/config"},
		{http.MethodPut, "/api/v1/apps/00000000-0000-4000-8000-000000000000/lock"},
		{http.MethodDelete, "/api/v1/auth/local/login"},
		// chi resolved these two to the GET's pattern, so Allow listed every
		// verb, the wrong one included (D-23, fourth 2026-11-03 black-box list).
		{http.MethodPatch, "/api/v1/apps"},
		{http.MethodPut, "/api/v1/apps/00000000-0000-4000-8000-000000000000"},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405: %s", c.method, c.path, w.Code, w.Body.String())
			continue
		}
		var body struct {
			Error, Code, Hint string
			Retryable         *bool
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s: the 405 was not JSON (%v): %q", c.method, c.path, err, w.Body.String())
			continue
		}
		if body.Code != "method_not_allowed" || body.Retryable == nil || *body.Retryable {
			t.Errorf("%s %s: 405 body = %s, want code method_not_allowed and retryable false",
				c.method, c.path, w.Body.String())
		}
		// The answer names what would have worked, three ways: the header a
		// client library reads, the sentence a person reads, and the hint.
		allow := w.Header().Get("Allow")
		if allow == "" || strings.Contains(allow, c.method) {
			t.Errorf("%s %s: Allow = %q, want the route's methods without %s", c.method, c.path, allow, c.method)
		}
		if want := c.path + " does not take " + c.method + " — it takes " + allow; body.Error != want {
			t.Errorf("%s %s: error = %q, want %q", c.method, c.path, body.Error, want)
		}
		if first := strings.Split(allow, ", ")[0]; !strings.HasPrefix(body.Hint, "send "+first) {
			t.Errorf("%s %s: hint = %q, want it to name %s", c.method, c.path, body.Hint, first)
		}
	}
}

// A wrong verb on a real route, sent with a key, is the router's 405 — never
// session_required, which told the CLI (exit 3) that the key was the problem
// for a method nobody may send (D-23, fourth 2026-11-03 black-box list).
func TestAWrongMethodWithAKeyIsNotACredentialRefusal(t *testing.T) {
	s := guardServer(t)
	for _, probe := range []struct{ method, path string }{
		{http.MethodPatch, "/api/v1/apps"},
		{http.MethodPut, "/api/v1/apps"},
		{http.MethodDelete, "/api/v1/apps"},
		{http.MethodPut, "/api/v1/apps/zzprobe"},
	} {
		for _, scope := range []string{database.UserKeyScopeRead, database.UserKeyScopeFull} {
			reached, status, code := guardProbe(t, s, scope, probe.method, probe.path)
			if !reached {
				t.Errorf("%s %s as a %s key was refused %d/%q by the credential guard; "+
					"no handler takes that method, so the router answers 405",
					probe.method, probe.path, scope, status, code)
			}
		}
	}
	// And a method the route does take is still decided by the guard.
	if reached, _, _ := guardProbe(t, s, database.UserKeyScopeRead, http.MethodPost, "/api/v1/apps"); reached {
		t.Error("POST /api/v1/apps as a read-scoped key reached the handler")
	}
}

// TestBySlugIsNotShadowed is what keeps a name from resolving to the wrong
// route. Every two-segment GET /api/v1/apps/{appID}/<word> on the outer router
// was once tried before the /apps subrouter's catch-all, so an app slugged
// "sends" was asked for its sends list with appID "by-slug" (RO-2). The next
// such route must not reopen it.
func TestBySlugIsNotShadowed(t *testing.T) {
	router := routerForWalk(t)
	const want = "/api/v1/apps/by-slug/{slug}"
	var words []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		rest, ok := strings.CutPrefix(route, "/api/v1/apps/{appID}/")
		if method != http.MethodGet || !ok || rest == "" || strings.ContainsAny(rest, "/{*") {
			return nil
		}
		words = append(words, rest)
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}
	if len(words) == 0 {
		t.Fatal("no GET /api/v1/apps/{appID}/<word> route found; the walk is not seeing the router")
	}
	for _, w := range words {
		rctx := chi.NewRouteContext()
		if !router.Match(rctx, http.MethodGet, "/api/v1/apps/by-slug/"+w) {
			t.Errorf("GET /api/v1/apps/by-slug/%s resolves to nothing", w)
			continue
		}
		if got := rctx.RoutePattern(); got != want {
			t.Errorf("GET /api/v1/apps/by-slug/%s resolves to %s, want %s", w, got, want)
		}
	}
}
