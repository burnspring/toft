package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// The refusal the rung exists to produce, and the boundary that makes it safe.
//
// *404 over 403* protects the existence of something the caller may not see.
// A `list` holder has been told the app exists by an owner who wrote them a
// grant saying so, and answering 404 on an id the same API just listed teaches
// them the list is unreliable — which is the reasoning the invariant already
// carries, word for word, for an environment-configured sign-in provider.
//
// What bounds the exception is the grant. A stranger still gets 404, and that
// is asserted here beside it: without that line this test would pass on a build
// that had turned the whole rule off.
func seedListRungApp(t *testing.T, srv *Server) *database.App {
	t.Helper()
	// **The install-wide switch off, because these tests are about the grant.**
	//
	// The rung arrives two ways and they bound differently. A grant is a row an
	// owner wrote, so the 403-instead-of-404 exception is bounded by that row
	// and a stranger still gets 404 — which is the sentence half of this file
	// exists to assert, and it needs somebody who is genuinely a stranger. With
	// the install-wide switch on, which is the default since 2026-08-31, nobody
	// signed in is a stranger any more.
	//
	// The on-case is asserted below in its own test rather than folded in here,
	// because "everybody holds it" and "this person was given it" are different
	// claims and a fixture that quietly supplied both would let either one rot.
	rungOff(t, srv)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "lister", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	app := &database.App{
		Slug: "payroll", Name: "Payroll", OwnerID: "owner",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser,
		"lister", database.ObjectRoleList, "owner", database.RequestMeta{}); err != nil {
		t.Fatalf("grant list: %v", err)
	}
	return app
}

func TestListRungIsRefusedWithACodeAndAStrangerIsNot(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	rec := getAs(t, srv, router, "lister", "/api/v1/apps/"+app.ID)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a list holder opening the app got %d, want %d", rec.Code, http.StatusForbidden)
	}
	var body refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "deny_list_only" {
		t.Errorf("refusal code = %q, want %q — the client cannot render the gate without it",
			body["code"], "deny_list_only")
	}

	if rec := getAs(t, srv, router, "stranger", "/api/v1/apps/"+app.ID); rec.Code != http.StatusNotFound {
		t.Errorf("somebody with no relationship to the app got %d, want %d; the exception is "+
			"bounded by a grant an owner wrote, and nothing else may reach it", rec.Code, http.StatusNotFound)
	}
}

// Every app route, not only the detail read. A gate on one route and a raw 404
// on the rest is how a caller learns what exists by trying — and it is also how
// the SPA ends up showing an error page on the tab beside the gate.
func TestTheRungIsRefusedTheSameWayOnEveryAppRoute(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	for _, path := range []string{
		"/api/v1/apps/" + app.ID,
		"/api/v1/apps/" + app.ID + "/events",
		"/api/v1/apps/" + app.ID + "/deployments",
		"/api/v1/apps/" + app.ID + "/grants",
		"/api/v1/apps/" + app.ID + "/consumption",
	} {
		rec := getAs(t, srv, router, "lister", path)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s answered 404 to a list holder; the rung's refusal is the same on every "+
				"app route or the gate is only on one of them", path)
			continue
		}
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusOK {
			t.Errorf("%s answered %d to a list holder, want 403", path, rec.Code)
		}
	}

	// The one route that answers instead of refusing, because it is the route
	// that says *why* the others refused. Without it a client cannot tell the
	// gate from an error page.
	rec := getAs(t, srv, router, "lister", "/api/v1/apps/"+app.ID+"/permissions")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /permissions for a list holder = %d, want 200", rec.Code)
	}
	var perms map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &perms); err != nil {
		t.Fatalf("decode permissions: %v", err)
	}
	if perms["effective_role"] != database.ObjectRoleList {
		t.Errorf("effective_role = %v, want %q", perms["effective_role"], database.ObjectRoleList)
	}
	if perms["can_read"] != false {
		t.Error("can_read is true for a caller who may not open the app")
	}
	// And a stranger is still told nothing at all by it.
	if rec := getAs(t, srv, router, "stranger", "/api/v1/apps/"+app.ID+"/permissions"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /permissions for a stranger = %d, want 404", rec.Code)
	}
}

// The listing is the other half: the app is *in* it, which is the whole point
// of the grant and the only reason the refusal above is not a lie.
func TestTheRungPutsTheAppInTheListing(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	rec := getAs(t, srv, router, "lister", "/api/v1/apps")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps = %d", rec.Code)
	}
	if !containsAppID(t, rec.Body.Bytes(), app.ID) {
		t.Error("a list holder's listing does not contain the app they were granted the rung on")
	}

	rec = getAs(t, srv, router, "stranger", "/api/v1/apps")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps = %d", rec.Code)
	}
	if containsAppID(t, rec.Body.Bytes(), app.ID) {
		t.Error("a private app reached a listing of somebody holding nothing on it")
	}
}

func containsAppID(t *testing.T, body []byte, id string) bool {
	t.Helper()
	var apps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &apps); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	for _, a := range apps {
		if a.ID == id {
			return true
		}
	}
	return false
}

// The ask, which is the point of the rung and was impossible before it: the
// create route refused anybody who could not already read the app, so the only
// people who could ask for access were people who had it.
func TestAListHolderCanAskAndAStrangerStillCannot(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	rec := sendJSONAs(t, srv, router, "lister", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests",
		`{"role":"viewer","note":"I own the quarterly close and this is the source"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a list holder asking for access got %d, want %d: %s",
			rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created["reason"] != "I own the quarterly close and this is the source" {
		t.Errorf("the note came back as %q; an ask you cannot explain is one people route "+
			"around into chat", created["reason"])
	}

	if rec := sendJSONAs(t, srv, router, "stranger", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests", `{"role":"viewer"}`); rec.Code != http.StatusNotFound {
		t.Errorf("somebody with no relationship to the app asked and got %d, want 404 — the "+
			"enumeration argument survives only because a list grant is a row an owner wrote", rec.Code)
	}

	// The requester can read their own back, which they could not before: the
	// object list route is the decider's permission.
	rec = getAs(t, srv, router, "lister", "/api/v1/me/access-requests")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/access-requests = %d", rec.Code)
	}
	var mine []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(mine) != 1 || mine[0]["object_id"] != app.ID {
		t.Fatalf("the requester's own list holds %d request(s), want 1 against %s", len(mine), app.ID)
	}
	if mine[0]["app_slug"] != app.Slug {
		t.Errorf("app_slug = %v, want %q — a list of ids is not a list somebody can read",
			mine[0]["app_slug"], app.Slug)
	}
	// And it is one person's own list, not everybody's.
	rec = getAs(t, srv, router, "stranger", "/api/v1/me/access-requests")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me/access-requests as somebody else = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(mine) != 0 {
		t.Errorf("somebody else's request appeared in this person's own list")
	}
}

// The permissions payload carries the request itself rather than a boolean, so
// the page resolves in one round trip and the gate does not flash between "you
// may ask" and "you already did".
func TestPermissionsCarriesTheOpenRequest(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	perms := func(who string) map[string]any {
		t.Helper()
		rec := getAs(t, srv, router, who, "/api/v1/apps/"+app.ID+"/permissions")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /permissions as %s = %d", who, rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	before := perms("lister")
	if before["can_request_access"] != true {
		t.Error("a list holder is told they may not ask; asking is the only thing the rung is for")
	}
	if before["pending_access_request"] != nil {
		t.Error("an open request was reported before one was made")
	}

	// The owner holds everything this route can grant, so there is nothing for
	// them to ask for — which is what the flag used to get wrong.
	if perms("owner")["can_request_access"] != false {
		t.Error("the owner is offered an ask-for-access affordance for their own app")
	}

	if rec := sendJSONAs(t, srv, router, "lister", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests", `{"role":"viewer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create request: %d %s", rec.Code, rec.Body.String())
	}

	after := perms("lister")
	open, ok := after["pending_access_request"].(map[string]any)
	if !ok {
		t.Fatalf("pending_access_request = %v, want the request", after["pending_access_request"])
	}
	if open["status"] != "pending" || open["requested_role"] != "viewer" {
		t.Errorf("the open request reads %v", open)
	}
}

// Revoking the rung withdraws nothing. The owner saw the ask, and silently
// dropping it would be a refusal nobody sent.
func TestRevokingTheRungLeavesAPendingRequestDecidable(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	if rec := sendJSONAs(t, srv, router, "lister", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests", `{"role":"viewer"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create request: %d", rec.Code)
	}
	if _, err := srv.db.DeleteGrant(context.Background(), app.ID, database.PrincipalTypeUser,
		"lister", "owner", database.RequestMeta{}); err != nil {
		t.Fatalf("revoke the rung: %v", err)
	}

	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID+"/access-requests")
	if rec.Code != http.StatusOK {
		t.Fatalf("the owner's queue = %d", rec.Code)
	}
	var queue []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(queue) != 1 || queue[0]["status"] != "pending" {
		t.Fatalf("the queue holds %v; revoking the rung must withdraw nothing", queue)
	}
	if rec := sendJSONAs(t, srv, router, "owner", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests/"+queue[0]["id"].(string)+"/approve", ""); rec.Code != http.StatusNoContent &&
		rec.Code != http.StatusOK {
		t.Errorf("approving a request whose rung was revoked = %d: %s", rec.Code, rec.Body.String())
	}
}

// The owner's withdrawal over the wire: who may write it, that a re-send of the
// stored value records nothing, and that the audit row lands.
//
// The no-op case is the one worth stating. "Who withdrew this app from
// everybody's list, and when" is a question an identical before and after
// cannot answer, so a PATCH re-sending the value an app already has has to be
// accepted and recorded as nothing — the same rule visibility earned when its
// audit row was written in a transaction that committed before the update ran.
func TestUnListingAnAppIsTheOwnersAndIsAudited(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	countRows := func() int {
		t.Helper()
		var n int
		if err := srv.db.Conn().QueryRow(
			`SELECT COUNT(*) FROM audit_log WHERE target_id = ? AND action = 'app.listable_change'`,
			app.ID).Scan(&n); err != nil {
			t.Fatalf("counting audit rows: %v", err)
		}
		return n
	}

	// A list holder may not write it. They can see the app exists; deciding who
	// else may is the owner's, and it is the same permission as visibility.
	if rec := doJSONAs(t, srv, router, "lister", http.MethodPatch,
		"/api/v1/apps/"+app.ID, map[string]any{"listable": false}); rec.Code != http.StatusForbidden {
		t.Errorf("a list holder withdrawing the app got %d, want %d", rec.Code, http.StatusForbidden)
	}
	if n := countRows(); n != 0 {
		t.Fatalf("a refused write left %d audit rows behind", n)
	}

	// The owner may.
	if rec := doJSONAs(t, srv, router, "owner", http.MethodPatch,
		"/api/v1/apps/"+app.ID, map[string]any{"listable": false}); rec.Code != http.StatusOK {
		t.Fatalf("the owner withdrawing the app got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if n := countRows(); n != 1 {
		t.Errorf("one withdrawal wrote %d audit rows, want 1", n)
	}

	fresh, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if fresh.Listable {
		t.Error("the app is still listable after a 200 on the write that withdrew it")
	}

	// Re-sending what is already stored is accepted and records nothing.
	if rec := doJSONAs(t, srv, router, "owner", http.MethodPatch,
		"/api/v1/apps/"+app.ID, map[string]any{"listable": false}); rec.Code != http.StatusOK {
		t.Fatalf("re-sending the stored value got %d, want 200", rec.Code)
	}
	if n := countRows(); n != 1 {
		t.Errorf("re-sending the stored value wrote a second audit row (%d total) — an identical "+
			"before and after cannot answer the question this row exists to answer", n)
	}
}

// The other way in, and the one that is now the default: nobody wrote anybody a
// row, and the install said everyone may see that every app exists.
//
// The bound is different and worth stating beside the grant's. A grant's
// exception is bounded by the row an owner wrote; this one is bounded by the
// install's own decision, which is why it reaches every signed-in caller and
// every app — and why the owner needs `apps.listable` to take one back.
func TestTheInstallWideRungRefusesWithTheSameDoor(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	app := seedListRungApp(t, srv)

	// seedListRungApp turned it off; this test is the one that wants it on, and
	// asks for the default by removing the row rather than writing "true" — so
	// it is the upgrade path being asserted, not a setting somebody chose.
	if _, err := srv.db.Conn().Exec(
		`DELETE FROM settings WHERE key = ?`, database.SettingAppsListableToAuthenticated); err != nil {
		t.Fatalf("clearing the setting: %v", err)
	}

	rec := getAs(t, srv, router, "stranger", "/api/v1/apps/"+app.ID)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("with the install-wide rung on, somebody holding nothing got %d, want 403 — the "+
			"install has told them this app exists, so a 404 would make their own list a liar",
			rec.Code)
	}
	var body refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "deny_list_only" {
		t.Errorf("refusal code = %q, want %q — the gate is the same one either way",
			body["code"], "deny_list_only")
	}

	// And the owner's withdrawal puts 404 back, which is the whole reason the
	// column exists: *404 over 403* is not withdrawn by the default, it is moved
	// behind a control belonging to the person whose app is being talked about.
	if err := srv.db.SetAppListable(app.ID, true, false,
		database.PrincipalTypeUser, "owner", "", "", "", ""); err != nil {
		t.Fatalf("SetAppListable: %v", err)
	}
	if rec := getAs(t, srv, router, "stranger", "/api/v1/apps/"+app.ID); rec.Code != http.StatusNotFound {
		t.Errorf("after the owner withdrew the app, somebody holding nothing got %d, want 404",
			rec.Code)
	}
}
