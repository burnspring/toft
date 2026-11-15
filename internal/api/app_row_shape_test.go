package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"toft/internal/database"
	"toft/internal/runtime"
)

// What an app's readers are told about it, field by field (BB-22, BB-28).
//
// Every assertion here is an exact key set rather than "this field is absent",
// because the failure these tests exist for is a field nobody decided to send:
// a column added to `database.App` or `database.User` reaching a list holder or
// a stranger because the row embedded the whole struct.

func sortedKeys(m map[string]json.RawMessage) []string {
	out := keysOf(m)
	sort.Strings(out)
	return out
}

func wantKeys(t *testing.T, what string, m map[string]json.RawMessage, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := sortedKeys(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s keys = %v\nwant exactly %v", what, got, want)
	}
}

// The public owner shape: who to ask, and nothing about the account.
var publicOwnerKeys = []string{"id", "login", "name", "avatar_url"}

func listRows(t *testing.T, srv *Server, router http.Handler, who string) map[string]map[string]json.RawMessage {
	t.Helper()
	rec := getAs(t, srv, router, who, "/api/v1/apps")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps as %s = %d: %s", who, rec.Code, rec.Body.String())
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bySlug := map[string]map[string]json.RawMessage{}
	for _, r := range rows {
		var slug string
		_ = json.Unmarshal(r["slug"], &slug)
		bySlug[slug] = r
	}
	return bySlug
}

func object(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode object %s: %v", raw, err)
	}
	return m
}

// A row held at the `list` rung is name, slug, owner and framework, and the
// flags that say what the caller may do with it — the documented four and not
// one field more (ADR §90). The owner is its id, its group when it has one,
// and the public shape.
func TestAListRungRowCarriesTheDocumentedFourAndNothingElse(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)
	if err := srv.db.UpdateAppStatus(app.ID, database.AppStatusRunning); err != nil {
		t.Fatal(err)
	}

	row, ok := listRows(t, srv, router, "lister")["payroll"]
	if !ok {
		t.Fatal("the list holder's listing does not carry the app; the test asserts nothing")
	}
	wantKeys(t, "a list-rung row", row,
		"id", "name", "slug", "framework", "owner_id", "owner",
		"can_edit", "can_delete", "can_open", "mine", "listable")
	if string(row["can_open"]) != "false" {
		t.Errorf("can_open = %s on a list-rung row", row["can_open"])
	}
	wantKeys(t, "a list-rung row's owner", object(t, row["owner"]), publicOwnerKeys...)
}

// The owner is narrowed on every row and every detail an administrator is not
// reading — a viewer of somebody else's app gets who to ask, not their address,
// role and last sign-in. An administrator still gets the whole account, which
// the People page is.
func TestAnAppsOwnerIsThePublicShapeForEveryoneButAnAdministrator(t *testing.T) {
	srv, router := newHealthTestEnv(t)
	app, err := srv.db.GetAppBySlug("bob-internal")
	if err != nil || app == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}

	row, ok := listRows(t, srv, router, "alice")["bob-internal"]
	if !ok {
		t.Fatal("alice's listing does not carry bob-internal")
	}
	if string(row["can_open"]) != "true" {
		t.Fatalf("can_open = %s, want a readable row", row["can_open"])
	}
	wantKeys(t, "a reader's list-row owner", object(t, row["owner"]), publicOwnerKeys...)

	for _, path := range []string{"/api/v1/apps/" + app.ID, "/api/v1/apps/by-slug/" + app.Slug} {
		rec := getAs(t, srv, router, "alice", path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		page := object(t, rec.Body.Bytes())
		wantKeys(t, "a reader's detail owner ("+path+")", object(t, page["owner"]), publicOwnerKeys...)
		if strings.Contains(rec.Body.String(), "bob@example.com") {
			t.Errorf("GET %s carries the owner's address to a non-administrator", path)
		}
	}

	adminRow := listRows(t, srv, router, "root")["bob-internal"]
	if owner := object(t, adminRow["owner"]); owner["email"] == nil || owner["system_role"] == nil {
		t.Errorf("an administrator's row lost the owner's account: %v", sortedKeys(owner))
	}
	rec := getAs(t, srv, router, "root", "/api/v1/apps/"+app.ID)
	if owner := object(t, object(t, rec.Body.Bytes())["owner"]); owner["email"] == nil {
		t.Errorf("an administrator's detail lost the owner's account: %v", sortedKeys(owner))
	}
}

// A request with no credential is told nothing about a public app by the
// platform API: the four reads that answered it — with the owner's address,
// role and last sign-in, the release, the port and live readings — refuse with
// the 401 every other app route gives. A public app's anonymous surface is the
// app, at the gateway.
func TestAnAnonymousCallerReadsNoAppFromThePlatformAPI(t *testing.T) {
	srv, router := newHealthTestEnv(t)
	app, err := srv.db.GetAppBySlug("alice-public")
	if err != nil || app == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	if app.Visibility != database.VisibilityPublic {
		t.Fatalf("visibility = %q, want public", app.Visibility)
	}
	if _, err := srv.rts.Shared().Deploy(runtime.WorkloadSpec{
		AppID: app.ID, Slug: app.Slug, Name: app.ProcessName(), Framework: app.Framework,
	}); err != nil {
		t.Fatalf("register mock process: %v", err)
	}

	for _, path := range []string{
		"/api/v1/apps",
		"/api/v1/apps/" + app.ID,
		"/api/v1/apps/" + app.ID + "?view=brief",
		"/api/v1/apps/by-slug/" + app.Slug,
		"/api/v1/apps/" + app.ID + "/usage",
		// Answered the same as a taken one, so the refusal is no oracle.
		"/api/v1/apps/by-slug/no-such-app",
		"/api/v1/apps/00000000-0000-0000-0000-000000000000",
	} {
		rec := getAs(t, srv, router, "", path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401: %s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		// The refusal and its envelope, and nothing else: no app field, no
		// owner field, whatever the route would have answered signed in.
		refusal := object(t, rec.Body.Bytes())
		wantKeys(t, "anonymous GET "+path, refusal, "error", "code", "hint", "retryable")
		if string(refusal["code"]) != `"unauthorized"` {
			t.Errorf("anonymous GET %s code = %s, want unauthorized", path, refusal["code"])
		}
		for _, leak := range []string{"email", "system_role", "last_login_at", "alice@example.com", app.Slug} {
			if strings.Contains(body, leak) {
				t.Errorf("anonymous GET %s carries %q: %s", path, leak, body)
			}
		}
	}

	// Signed in, the same public app is still readable — the refusal is about
	// the credential, not the app.
	if rec := getAs(t, srv, router, "bob", "/api/v1/apps/"+app.ID); rec.Code != http.StatusOK {
		t.Errorf("a signed-in reader of a public app got %d", rec.Code)
	}
}

// /system/health is a listing too, and the list rung sees an app without being
// able to open it: no status for it, as there are no readings, and no place in
// the counts, which would be its status told by subtraction.
func TestSystemHealthGivesNoStatusForAListRungApp(t *testing.T) {
	srv, router := newHealthTestEnv(t)
	if _, err := srv.db.PutSetting(database.SettingAppsListableToAuthenticated, "true", ""); err != nil {
		t.Fatal(err)
	}
	secret, err := srv.db.GetAppBySlug("bob-secret")
	if err != nil || secret == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	if err := srv.db.UpdateAppStatus(secret.ID, database.AppStatusCrashed); err != nil {
		t.Fatal(err)
	}

	health := func(who string) (map[string]map[string]json.RawMessage, map[string]int) {
		rec := getAs(t, srv, router, who, "/api/v1/system/health")
		if rec.Code != http.StatusOK {
			t.Fatalf("health as %s = %d", who, rec.Code)
		}
		var h struct {
			Summary map[string]int               `json:"summary"`
			Apps    []map[string]json.RawMessage `json:"apps"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]map[string]json.RawMessage{}
		for _, a := range h.Apps {
			var slug string
			_ = json.Unmarshal(a["slug"], &slug)
			out[slug] = a
		}
		return out, h.Summary
	}

	apps, summary := health("alice")
	row, ok := apps["bob-secret"]
	if !ok {
		t.Fatal("the install-wide rung did not list bob-secret; the test asserts nothing")
	}
	wantKeys(t, "a list-rung health row", row, "id", "name", "slug")
	if own := apps["alice-public"]; own["status"] == nil {
		t.Error("alice's own app lost its status")
	}
	if summary["crashed"] != 0 {
		t.Errorf("crashed = %d for a caller who can open no crashed app", summary["crashed"])
	}
	if summary["total"] != len(apps) {
		t.Errorf("total = %d, want the %d apps listed", summary["total"], len(apps))
	}

	adminApps, adminSummary := health("root")
	if adminApps["bob-secret"]["status"] == nil || adminSummary["crashed"] != 1 {
		t.Errorf("an administrator lost bob-secret's status: row %v, crashed %d",
			sortedKeys(adminApps["bob-secret"]), adminSummary["crashed"])
	}
}

// Every route under /api/v1/apps, not just the four above: walked from the
// router, so a route added later is covered without anyone remembering to add
// it here. /consumption answered a public app's figures to anybody because it
// authorized with the app's read rule alone (BB-40).
func TestEveryAppRouteRefusesAnAnonymousCaller(t *testing.T) {
	srv, router := newHealthTestEnv(t)
	app, err := srv.db.GetAppBySlug("alice-public")
	if err != nil || app == nil {
		t.Fatalf("GetAppBySlug: %v", err)
	}
	param := regexp.MustCompile(`\{[^}]+\}`)
	seen := 0
	err = chi.Walk(srv.Router(http.NotFoundHandler()).(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/apps") {
			return nil
		}
		path := strings.ReplaceAll(route, "{appID}", app.ID)
		path = param.ReplaceAllString(path, "x")
		path = strings.ReplaceAll(path, "/*", "/")
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		seen++
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401: %.200s", method, route, rec.Code, rec.Body.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("walked %d app routes", seen)
	if seen < 50 {
		t.Fatalf("walked %d app routes; the walk is not reaching the router", seen)
	}
}
