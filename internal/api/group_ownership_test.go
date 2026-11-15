package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The API surface of group ownership (ADR §51). The database-level guarantees
// are in internal/database; what is tested here is the half a caller can reach:
// who is refused, with which status, and whether the refusal says something
// somebody could act on.

// seedGroupOwnedApp builds the shape every test below needs: an app, a group
// with one eligible member, and the app transferred to that group.
func seedGroupOwnedApp(t *testing.T, srv *Server, slug, groupName string, members ...string) (*database.App, *database.Group) {
	t.Helper()
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: "founder",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	g, err := srv.db.CreateGroup(t.Context(), groupName, "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	for _, m := range members {
		if err := srv.db.AddGroupMember(t.Context(), g.ID, m, "root", database.RequestMeta{}); err != nil {
			t.Fatalf("add %s to group: %v", m, err)
		}
	}
	if err := srv.db.TransferOwnership(t.Context(), app.ID,
		database.PrincipalTypeGroup, g.ID, "root", database.RequestMeta{}); err != nil {
		t.Fatalf("transfer to group: %v", err)
	}
	return app, g
}

// Transferring to a group, over the wire, including the two ways it is refused.
func TestTransferToGroupOverTheAPI(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	seedUser(t, srv.db, "onlyviewer", database.SystemRoleViewer)

	app := &database.App{
		Slug: "prod-report", Name: "Prod Report", OwnerID: "founder",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	good, _ := srv.db.CreateGroup(t.Context(), "Platform", "root", database.RequestMeta{})
	empty, _ := srv.db.CreateGroup(t.Context(), "Nobody", "root", database.RequestMeta{})
	viewers, _ := srv.db.CreateGroup(t.Context(), "Viewers", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), good.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := srv.db.AddGroupMember(t.Context(), viewers.ID, "onlyviewer", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add viewer: %v", err)
	}

	path := "/api/v1/apps/" + app.ID + "/transfer"

	// §9.3 — an empty group cannot be given an app.
	rec := sendAs(t, srv, router, "founder", http.MethodPost, path,
		fmt.Sprintf(`{"new_owner_type":"group","new_owner_id":%q}`, empty.ID))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("transfer to an empty group: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}

	// A group whose only member is a system viewer is empty in the sense that
	// matters, and the message has to say which sense that is.
	rec = sendAs(t, srv, router, "founder", http.MethodPost, path,
		fmt.Sprintf(`{"new_owner_type":"group","new_owner_id":%q}`, viewers.ID))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("transfer to a group of viewers: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("system viewer")) {
		t.Errorf("refusal does not explain why: %s", rec.Body.String())
	}

	// The real thing.
	rec = sendAs(t, srv, router, "founder", http.MethodPost, path,
		fmt.Sprintf(`{"new_owner_type":"group","new_owner_id":%q}`, good.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("transfer to a group: %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	after, _ := srv.db.GetApp(app.ID)
	if after.OwnerGroupID != good.ID || after.OwnerID != "" {
		t.Fatalf("after transfer: owner_user=%q owner_group=%q", after.OwnerID, after.OwnerGroupID)
	}

	// And the member now holds owner over the wire — they can transfer it on,
	// which is the owner-only action nearest to hand.
	rec = sendAs(t, srv, router, "member", http.MethodPost, path,
		`{"new_owner_id":"founder"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("member of the owning group transferring: %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

// A bare new_owner_id still means a user. Every existing client sends this and
// none of them has heard of groups.
func TestTransferWithoutATypeStillMeansAUser(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "successor", database.SystemRolePublisher)
	app := &database.App{
		Slug: "legacy-client", Name: "Legacy", OwnerID: "founder",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	rec := sendAs(t, srv, router, "founder", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/transfer", `{"new_owner_id":"successor"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bare new_owner_id: %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	after, _ := srv.db.GetApp(app.ID)
	if after.OwnerID != "successor" || after.OwnerGroupID != "" {
		t.Fatalf("owner_user=%q owner_group=%q", after.OwnerID, after.OwnerGroupID)
	}
}

// BB-82: a bare group id is the group, not "target user not found" — the
// refusal's own hint said to send exactly that.
func TestTransferWithABareGroupIDMovesItToTheGroup(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	app := &database.App{
		Slug: "bare-group", Name: "Bare", OwnerID: "founder",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}
	g, _ := srv.db.CreateGroup(t.Context(), "Team", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	rec := sendAs(t, srv, router, "founder", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/transfer", fmt.Sprintf(`{"new_owner_id":%q}`, g.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bare group id: %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	after, _ := srv.db.GetApp(app.ID)
	if after.OwnerGroupID != g.ID || after.OwnerID != "" {
		t.Fatalf("owner_user=%q owner_group=%q", after.OwnerID, after.OwnerGroupID)
	}
}

// §9.4 — the payoff, over the wire: one action clears an estate, and then the
// account deletes.
func TestTransferAppsThenDeleteTheAccount(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "leaver", database.SystemRolePublisher)
	seedUser(t, srv.db, "stays", database.SystemRolePublisher)

	g, _ := srv.db.CreateGroup(t.Context(), "Finance Platform", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "stays", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	for i := 0; i < 3; i++ {
		app := &database.App{
			Slug: fmt.Sprintf("owned-%d", i), Name: "Owned", OwnerID: "leaver",
			Framework: "flask", Status: database.AppStatusRunning,
			Visibility: database.VisibilityPrivate,
		}
		if err := srv.db.CreateApp(app); err != nil {
			t.Fatalf("create app: %v", err)
		}
	}

	// Deleting first is refused, and the refusal now mentions a group because
	// this install has one.
	rec := sendAs(t, srv, router, "root", http.MethodDelete, "/api/v1/users/leaver", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete before transfer: %d, want 409", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("group")) {
		t.Errorf("the refusal does not offer a group: %s", rec.Body.String())
	}

	// One action moves all three.
	rec = sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/users/leaver/transfer-apps",
		fmt.Sprintf(`{"to_type":"group","to_id":%q}`, g.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("transfer-apps: %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Transferred int      `json:"transferred"`
		Apps        []string `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Transferred != 3 || len(out.Apps) != 3 {
		t.Fatalf("transferred %d apps %v, want 3", out.Transferred, out.Apps)
	}

	// And now the account goes.
	if rec := sendAs(t, srv, router, "root", http.MethodDelete, "/api/v1/users/leaver", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete after transfer: %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if u, _ := srv.db.GetUser("leaver"); u != nil {
		t.Fatal("the account survived")
	}
	// The apps are still there and still owned.
	for i := 0; i < 3; i++ {
		app, err := srv.db.GetAppBySlug(fmt.Sprintf("owned-%d", i))
		if err != nil || app == nil {
			t.Fatalf("app owned-%d is gone: %v", i, err)
		}
		if app.OwnerGroupID != g.ID {
			t.Errorf("owned-%d owner_group = %q, want %q", i, app.OwnerGroupID, g.ID)
		}
	}
}

// transfer-apps is admin-only and is not a way around the single-app transfer's
// authorization.
func TestTransferAppsIsAdminOnly(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "leaver", database.SystemRolePublisher)
	seedUser(t, srv.db, "nosy", database.SystemRolePublisher)

	rec := sendAs(t, srv, router, "nosy", http.MethodPost, "/api/v1/users/leaver/transfer-apps",
		`{"to_id":"nosy"}`)
	// 404 rather than 403 — the route must not confirm which user ids exist.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin calling transfer-apps: %d, want 404", rec.Code)
	}
	if rec := sendAs(t, srv, router, "", http.MethodPost, "/api/v1/users/leaver/transfer-apps",
		`{"to_id":"nosy"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous calling transfer-apps: %d, want 401", rec.Code)
	}
}

// The owning group's grant row is as immutable as an owning user's was.
func TestOwningGroupsGrantCannotBeEditedThroughTheGrantsAPI(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	app, g := seedGroupOwnedApp(t, srv, "grant-locked", "Owners", "member")

	// A member of the owning group cannot demote their own group.
	rec := putGrantAs(t, srv, router, "member", app.ID, "group", g.ID, database.ObjectRoleEditor)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("demoting the owning group: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	// Nor delete it, which would leave the app unreadable by its own owners.
	rec = sendAs(t, srv, router, "member", http.MethodDelete,
		fmt.Sprintf("/api/v1/apps/%s/grants/group/%s", app.ID, g.ID), "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("deleting the owning group's grant: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

// §9.1 and §9.2 as HTTP: 409 with the count, not a 500.
func TestGroupLifecycleRefusalsAreConflictsWithACount(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	_, g := seedGroupOwnedApp(t, srv, "held", "Holders", "member")

	rec := sendAs(t, srv, router, "root", http.MethodDelete, "/api/v1/groups/"+g.ID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting an owning group: %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("1 app")) {
		t.Errorf("the refusal does not name the count: %s", rec.Body.String())
	}

	rec = sendAs(t, srv, router, "root", http.MethodDelete,
		fmt.Sprintf("/api/v1/groups/%s/members/%s", g.ID, "member"), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("emptying an owning group: %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("last member")) {
		t.Errorf("the refusal does not say what is wrong: %s", rec.Body.String())
	}
}

// The address is admin-only, clears on empty, and is not a credential — it
// reads back in the clear.
func TestGroupEmailIsAdminOnlyAndClearable(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	g, _ := srv.db.CreateGroup(t.Context(), "Rota", "root", database.RequestMeta{})

	if rec := sendAs(t, srv, router, "member", http.MethodPatch, "/api/v1/groups/"+g.ID,
		`{"email":"x@example.test"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin setting the address: %d, want 403", rec.Code)
	}
	if rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/groups/"+g.ID,
		`{"email":"not-an-address"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("nonsense address: %d, want 422", rec.Code)
	}

	rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/groups/"+g.ID,
		`{"email":"rota@example.test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("setting the address: %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got, _ := srv.db.GetGroup(g.ID)
	if got.Email != "rota@example.test" {
		t.Fatalf("stored address = %q", got.Email)
	}
	// Read back in the clear — it is directory data, not a secret.
	rec = getAs(t, srv, router, "root", "/api/v1/groups/"+g.ID)
	if !bytes.Contains(rec.Body.Bytes(), []byte("rota@example.test")) {
		t.Errorf("the address does not come back: %s", rec.Body.String())
	}

	if rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/groups/"+g.ID,
		`{"email":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing the address: %d, want 200", rec.Code)
	}
	if got, _ := srv.db.GetGroup(g.ID); got.Email != "" {
		t.Fatalf("address after clearing = %q", got.Email)
	}
}

// The access review reports owner-via-group as its own source. Shipping without
// it makes the report silently under-report, which is a compliance defect
// rather than a cosmetic gap (AC-2(j)).
func TestAccessReviewReportsOwnerViaGroup(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	app, g := seedGroupOwnedApp(t, srv, "reviewed", "Reviewed Team", "member")

	// A second app shared with the same group by an ordinary grant, so the two
	// kinds have to be told apart rather than guessed from the role.
	shared := &database.App{
		Slug: "merely-shared", Name: "Merely Shared", OwnerID: "founder",
		Framework: "flask", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(shared); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if rec := putGrantAs(t, srv, router, "founder", shared.ID, "group", g.ID, database.ObjectRoleViewer); rec.Code != http.StatusOK {
		t.Fatalf("granting the group viewer: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := getAs(t, srv, router, "root", "/api/v1/users/member/access")
	if rec.Code != http.StatusOK {
		t.Fatalf("access review: %d (%s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Explicit []struct {
			App     struct{ ID, Slug string } `json:"app"`
			Sources []struct {
				Kind      string `json:"kind"`
				Role      string `json:"role"`
				GroupName string `json:"group_name"`
			} `json:"sources"`
		} `json:"explicit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the report: %v — body was %s", err, rec.Body.String())
	}

	kinds := map[string]string{}
	names := map[string]string{}
	for _, e := range body.Explicit {
		for _, s := range e.Sources {
			kinds[e.App.Slug] = s.Kind
			names[e.App.Slug] = s.GroupName
		}
	}
	if kinds[app.Slug] != "owner_via_group" {
		t.Errorf("source for the group-owned app = %q, want owner_via_group", kinds[app.Slug])
	}
	if names[app.Slug] != "Reviewed Team" {
		t.Errorf("the group is not named: %q", names[app.Slug])
	}
	if kinds[shared.Slug] != "group" {
		t.Errorf("source for the merely-shared app = %q, want group", kinds[shared.Slug])
	}
}

// A request against a group-owned app is routed to the group, any member may
// decide it, and the member who acts is the one recorded. "The group approved"
// is not an acceptable record of a decision (AC-5).
func TestAccessRequestAgainstAGroupOwnedAppRecordsTheMemberWhoDecided(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	seedUser(t, srv.db, "asker", database.SystemRolePublisher)
	app, g := seedGroupOwnedApp(t, srv, "requestable", "Deciders", "member")

	// The requester has to be able to see the app to ask about it.
	if rec := putGrantAs(t, srv, router, "member", app.ID, "user", "asker", database.ObjectRoleViewer); rec.Code != http.StatusOK {
		t.Fatalf("seeding read access: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := sendAs(t, srv, router, "asker", http.MethodPost,
		"/api/v1/apps/"+app.ID+"/access-requests", `{"role":"editor"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating the request: %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID              string `json:"id"`
		ApproverID      string `json:"approver_id"`
		ApproverGroupID string `json:"approver_group_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if created.ApproverGroupID != g.ID {
		t.Errorf("approver_group_id = %q, want %q — the request was not routed to the group",
			created.ApproverGroupID, g.ID)
	}
	if created.ApproverID != "" {
		t.Errorf("approver_id = %q on a pending request; it records who decided, not who was asked",
			created.ApproverID)
	}

	// Any member may decide it.
	rec = sendAs(t, srv, router, "member", http.MethodPost,
		fmt.Sprintf("/api/v1/apps/%s/access-requests/%s/approve", app.ID, created.ID), "")
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("member approving: %d (%s)", rec.Code, rec.Body.String())
	}

	// And the member who acted is on the row.
	decided, err := srv.db.GetAccessRequest(created.ID)
	if err != nil || decided == nil {
		t.Fatalf("re-reading the request: %v", err)
	}
	if decided.ApproverID != "member" {
		t.Fatalf("approver_id after the decision = %q, want the member who acted", decided.ApproverID)
	}
}

// --- The deploy credential -------------------------------------------------

// mintDeployKey returns a usable toftd_ secret, minted through the real route so
// the admin gate and the once-only disclosure are exercised too.
func mintDeployKey(t *testing.T, srv *Server, router http.Handler, name string) string {
	t.Helper()
	return mintScopedDeployKey(t, srv, router, name, database.DeployKeyScopeDeploy)
}

// mintScopedDeployKey is the same, naming the scope. Deploy is the default and
// what every key minted before scopes existed still has, so the plain helper
// above keeps meaning what it meant.
func mintScopedDeployKey(t *testing.T, srv *Server, router http.Handler, name, scope string) string {
	t.Helper()
	rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys",
		fmt.Sprintf(`{"name":%q,"scope":%q}`, name, scope))
	if rec.Code != http.StatusCreated {
		t.Fatalf("minting a deploy key: %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Secret string `json:"secret"`
		Key    struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the mint response: %v", err)
	}
	if !strings.HasPrefix(out.Secret, "toftd_") {
		t.Fatalf("secret %q does not carry the class's prefix", out.Secret)
	}
	return out.Secret
}

func asDeployKey(t *testing.T, router http.Handler, method, path, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// A deploy key cannot reach any credential or authorization route.
//
// TestADeployKeyCannotReachCredentialsOrAuthorization (keyroutes_test.go)
// proves it from the route map; this proves it over the wire, which is the half
// that would still be wrong if the guard were mounted in the wrong place in the
// middleware stack.
func TestADeployKeyIsRefusedCredentialRoutesOverTheWire(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	secret := mintDeployKey(t, srv, router, "pipeline")

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/users", ""},
		{http.MethodDelete, "/api/v1/users/root", ""},
		// GET /groups and GET /groups/{id} are *not* here: reading the
		// directory is neither managing a credential nor changing who may do
		// what, and the roster stays withheld. See
		// TestADeployKeyReadsTheGroupDirectoryAndNeverTheRoster.
		{http.MethodPost, "/api/v1/groups", `{"name":"x"}`},
		{http.MethodPatch, "/api/v1/groups/anything", `{"name":"x"}`},
		{http.MethodDelete, "/api/v1/groups/anything", ""},
		// Pointedly: it cannot mint its own replacement, or read the list of
		// keys, or turn one off.
		{http.MethodGet, "/api/v1/deploy-keys", ""},
		{http.MethodPost, "/api/v1/deploy-keys", `{"name":"second"}`},
		{http.MethodGet, "/api/v1/system/settings", ""},
		{http.MethodPut, "/api/v1/system/settings", `{}`},
	} {
		rec := asDeployKey(t, router, c.method, c.path, secret, c.body)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("%s %s reachable by a deploy key: %d (%s)", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// A deploy key cannot create an app without a valid, non-empty owning group.
//
// The joint between the two halves of the design: the credential creates the
// app, the group owns it, and neither is a person.
func TestADeployKeyMustNameAnOwningGroup(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	secret := mintDeployKey(t, srv, router, "promotion")

	good, _ := srv.db.CreateGroup(t.Context(), "Prod Owners", "root", database.RequestMeta{})
	empty, _ := srv.db.CreateGroup(t.Context(), "Nobody Here", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), good.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	base := `"repo_url":"https://github.com/example/app","name":"Promoted","slug":"promoted"`

	// No group named at all.
	rec := asDeployKey(t, router, http.MethodPost, "/api/v1/apps", secret, "{"+base+"}")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with no group: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("owner_group_id")) {
		t.Errorf("the refusal does not say what is missing: %s", rec.Body.String())
	}

	// A group that exists but can administer nothing.
	rec = asDeployKey(t, router, http.MethodPost, "/api/v1/apps", secret,
		fmt.Sprintf("{%s,\"owner_group_id\":%q}", base, empty.ID))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with an empty group: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}

	// A group that does not exist.
	rec = asDeployKey(t, router, http.MethodPost, "/api/v1/apps", secret,
		fmt.Sprintf("{%s,\"owner_group_id\":%q}", base, "no-such-group"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with an unknown group: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}

	// And the real thing.
	rec = asDeployKey(t, router, http.MethodPost, "/api/v1/apps", secret,
		fmt.Sprintf("{%s,\"owner_group_id\":%q}", base, good.ID))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create with a good group: %d (%s)", rec.Code, rec.Body.String())
	}
	app, err := srv.db.GetAppBySlug("promoted")
	if err != nil || app == nil {
		t.Fatalf("the app was not created: %v", err)
	}
	if app.OwnerGroupID != good.ID || app.OwnerID != "" {
		t.Fatalf("owner_user=%q owner_group=%q", app.OwnerID, app.OwnerGroupID)
	}

	// The audit row names the key, not a person and not nobody.
	var actorType, actorID string
	if err := srv.db.Conn().QueryRow(`
		SELECT actor_type, COALESCE(actor_id,'') FROM audit_log
		WHERE target_id = ? AND action = 'object_grant.create'`, app.ID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if actorType != "deploy_key" || actorID == "" {
		t.Fatalf("audit actor = (%s, %s), want the deploy key", actorType, actorID)
	}
}

// A session or an toftu_ key creates a personally-owned app with no group named,
// on an install that has no groups at all. Nothing new appears where the
// feature is not used.
func TestSelfServiceIsUntouchedByGroupOwnership(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "solo", database.SystemRolePublisher)

	groups, err := srv.db.ListGroups()
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("this install has %d groups; the test is not testing what it says", len(groups))
	}

	rec := sendAs(t, srv, router, "solo", http.MethodPost, "/api/v1/apps",
		`{"repo_url":"https://github.com/example/mine","name":"Mine","slug":"mine"}`)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("self-service create: %d (%s)", rec.Code, rec.Body.String())
	}
	app, err := srv.db.GetAppBySlug("mine")
	if err != nil || app == nil {
		t.Fatalf("app not created: %v", err)
	}
	if app.OwnerID != "solo" || app.OwnerGroupID != "" {
		t.Fatalf("owner_user=%q owner_group=%q — self-service must still produce single-user ownership",
			app.OwnerID, app.OwnerGroupID)
	}
}

// Off means absent, and off means now: a revoked or disabled key stops working
// on the next request, and revocation cannot be undone.
func TestDeployKeyRevocationTakesEffectImmediately(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	secret := mintDeployKey(t, srv, router, "revocable")

	// It works.
	if rec := asDeployKey(t, router, http.MethodGet, "/api/v1/apps", secret, ""); rec.Code != http.StatusOK {
		t.Fatalf("a fresh key cannot list apps: %d (%s)", rec.Code, rec.Body.String())
	}

	keys, _ := srv.db.ListDeployKeys()
	if len(keys) != 1 {
		t.Fatalf("expected one key, got %d", len(keys))
	}
	id := keys[0].ID

	// Disabled: refused, and reversible.
	if rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/deploy-keys/"+id,
		`{"disabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("disabling: %d (%s)", rec.Code, rec.Body.String())
	}
	rec := asDeployKey(t, router, http.MethodGet, "/api/v1/apps", secret, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a disabled key still works: %d", rec.Code)
	}
	if rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/deploy-keys/"+id,
		`{"disabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("re-enabling: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := asDeployKey(t, router, http.MethodGet, "/api/v1/apps", secret, ""); rec.Code != http.StatusOK {
		t.Fatalf("a re-enabled key does not work: %d", rec.Code)
	}

	// Revoked: refused, and final.
	if rec := sendAs(t, srv, router, "root", http.MethodDelete, "/api/v1/deploy-keys/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoking: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := asDeployKey(t, router, http.MethodGet, "/api/v1/apps", secret, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked key still works: %d", rec.Code)
	}
	if rec := sendAs(t, srv, router, "root", http.MethodPatch, "/api/v1/deploy-keys/"+id,
		`{"disabled":false}`); rec.Code != http.StatusConflict {
		t.Fatalf("un-revoking: %d, want 409 — revoked has to mean revoked", rec.Code)
	}

	// The row survives, so `deploy_key:{id}` in the audit log still resolves.
	if k, _ := srv.db.GetDeployKey(id); k == nil {
		t.Fatal("the row was deleted; the audit trail can no longer name the key")
	}
}

// Minting is an administrator's act, and no credential can perform it.
func TestOnlyAnAdministratorMintsADeployKey(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "publisher", database.SystemRolePublisher)

	if rec := sendAs(t, srv, router, "publisher", http.MethodPost, "/api/v1/deploy-keys",
		`{"name":"nope"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a publisher minting a deploy key: %d, want 403", rec.Code)
	}
	if rec := sendAs(t, srv, router, "", http.MethodPost, "/api/v1/deploy-keys",
		`{"name":"nope"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous minting a deploy key: %d, want 401", rec.Code)
	}
}

// --- The read scope, and what promotion needs ------------------------------

// The group directory is readable and the roster is not.
//
// Both halves matter. Without the first, `owner_group_id` is an id somebody has
// to be told out of band and a typo is a 422 at the worst possible moment.
// Without the second, a credential that belongs to nobody would learn who is in
// every team on the install, which is the one thing handleGetGroup withholds
// from a principal that is neither an administrator nor a member — and a deploy
// key is neither, having no user id, no groups and no system role.
func TestADeployKeyReadsTheGroupDirectoryAndNeverTheRoster(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "insider", database.SystemRolePublisher)

	g, err := srv.db.CreateGroup(t.Context(), "Prod Owners", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "insider", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	for _, scope := range []string{database.DeployKeyScopeRead, database.DeployKeyScopeDeploy} {
		secret := mintScopedDeployKey(t, srv, router, "directory-"+scope, scope)

		rec := asDeployKey(t, router, http.MethodGet, "/api/v1/groups", secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s scope listing groups: %d (%s)", scope, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Prod Owners") {
			t.Errorf("%s scope: the group is not in the list: %s", scope, rec.Body.String())
		}

		rec = asDeployKey(t, router, http.MethodGet, "/api/v1/groups/"+g.ID, secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s scope reading a group: %d (%s)", scope, rec.Code, rec.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if _, present := raw["members"]; present {
			t.Errorf("%s scope: the roster reached a credential that belongs to nobody: %v",
				scope, raw["members"])
		}
		if strings.Contains(rec.Body.String(), "insider") {
			t.Errorf("%s scope: a member's identity leaked: %s", scope, rec.Body.String())
		}
	}
}

// The scope, over the wire and in both directions.
//
// A read-scoped key is what an estate tool holds on the install it *runs on*:
// it can enumerate and pull, and a compromise of the thing holding it cannot
// become a push to every app on the box — the console's own app included. The
// refusal names the scope, because "mint the other one" and "stop asking" are
// different remedies.
func TestAReadScopedKeyPullsAndCannotPush(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)

	g, _ := srv.db.CreateGroup(t.Context(), "Owners", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	deployKey := mintDeployKey(t, srv, router, "pusher")
	readKey := mintScopedDeployKey(t, srv, router, "puller", database.DeployKeyScopeRead)

	// The deploy-scoped key creates the app the read-scoped one will look at.
	rec := asDeployKey(t, router, http.MethodPost, "/api/v1/apps", deployKey,
		fmt.Sprintf(`{"repo_url":"https://github.com/example/src","name":"Source","slug":"source","owner_group_id":%q}`, g.ID))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("seeding an app: %d (%s)", rec.Code, rec.Body.String())
	}
	app, err := srv.db.GetAppBySlug("source")
	if err != nil || app == nil {
		t.Fatalf("the app was not created: %v", err)
	}

	// Reads it may have.
	for _, path := range []string{
		"/api/v1/apps",
		"/api/v1/apps/" + app.ID,
		"/api/v1/apps/by-slug/source",
		"/api/v1/apps/" + app.ID + "/deployments",
		"/api/v1/apps/" + app.ID + "/dependencies",
		"/api/v1/groups",
	} {
		if rec := asDeployKey(t, router, http.MethodGet, path, readKey, ""); rec.Code != http.StatusOK {
			t.Errorf("read scope GET %s = %d (%s) — promotion pulls, and this is the pull",
				path, rec.Code, rec.Body.String())
		}
	}

	// Writes it may not, and the refusal says which fence stopped it.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/apps", fmt.Sprintf(`{"repo_url":"https://github.com/example/x","name":"X","slug":"x","owner_group_id":%q}`, g.ID)},
		{http.MethodPost, "/api/v1/apps/" + app.ID + "/deploy", ""},
		{http.MethodPost, "/api/v1/apps/" + app.ID + "/restart", ""},
		{http.MethodPost, "/api/v1/apps/" + app.ID + "/stop", ""},
		{http.MethodPost, "/api/v1/apps/" + app.ID + "/job-runs", `{}`},
	} {
		rec := asDeployKey(t, router, c.method, c.path, readKey, c.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read scope %s %s = %d, want 403 (%s)", c.method, c.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "deploy_key_read_scope") {
			t.Errorf("%s %s refused without naming the scope: %s", c.method, c.path, rec.Body.String())
		}
	}

	// And it is still bounded by the class, not only by the scope: a route no
	// deploy key may reach answers with the class's refusal, not the scope's.
	rec = asDeployKey(t, router, http.MethodGet, "/api/v1/deploy-keys", readKey, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read scope reading the key list: %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "deploy_key_read_scope") {
		t.Errorf("a route no deploy key may reach was refused as a scope problem: %s", rec.Body.String())
	}
}

// The create-from-archive path is permitted to a deploy key and used to be
// impossible for one: appOwnerID answers empty for a credential that belongs to
// nobody, no owner_group_id was read from the form, and apps_one_owner refused
// the insert. The mirror of TestADeployKeyMustNameAnOwningGroup, and its absence
// was the bug.
func TestADeployKeyCreatingFromAnArchiveMustNameAnOwningGroup(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	secret := mintDeployKey(t, srv, router, "promotion")

	g, _ := srv.db.CreateGroup(t.Context(), "Prod Owners", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// No group named: refused, and the refusal says what is missing.
	rec := uploadAsDeployKey(t, router, "/api/v1/apps/upload", secret,
		map[string]string{"name": "Promoted", "slug": "promoted"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("archive create with no group: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "owner_group_required") {
		t.Errorf("the refusal does not name the missing field: %s", rec.Body.String())
	}

	// A group that exists and can administer nothing.
	empty, _ := srv.db.CreateGroup(t.Context(), "Nobody Here", "root", database.RequestMeta{})
	rec = uploadAsDeployKey(t, router, "/api/v1/apps/upload", secret,
		map[string]string{"name": "Promoted", "slug": "promoted", "owner_group_id": empty.ID})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("archive create with an empty group: %d, want 422 (%s)", rec.Code, rec.Body.String())
	}

	// And the real thing: the app exists, owned by the group and by no person.
	rec = uploadAsDeployKey(t, router, "/api/v1/apps/upload", secret,
		map[string]string{"name": "Promoted", "slug": "promoted", "owner_group_id": g.ID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("archive create with a good group: %d (%s)", rec.Code, rec.Body.String())
	}
	app, err := srv.db.GetAppBySlug("promoted")
	if err != nil || app == nil {
		t.Fatalf("the app was not created: %v", err)
	}
	if app.OwnerGroupID != g.ID || app.OwnerID != "" {
		t.Fatalf("owner_user=%q owner_group=%q — exactly one owner, and it is the group",
			app.OwnerID, app.OwnerGroupID)
	}
}

// toft check before toft create answers for the owning group as the create
// will: a CI gate that passed a group the create then refused was a gate that
// lied (docs/bugs 2026-10-28 #2). And an empty group is told it is empty, not
// that its members are locked (#3).
func TestTheNewAppCheckAnswersForTheOwningGroup(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	seedUser(t, srv.db, "frozen", database.SystemRoleViewer)
	secret := mintDeployKey(t, srv, router, "gate")

	// No group named: the create's owner_group_required, at the check.
	rec := uploadAsDeployKey(t, router, "/api/v1/apps/check", secret, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "owner_group_required") {
		t.Fatalf("check with no group: %d %s, want 422 owner_group_required", rec.Code, rec.Body.String())
	}

	// A group with nobody in it: refused, saying so, with the create's code.
	empty, _ := srv.db.CreateGroup(t.Context(), "Nobody Here", "root", database.RequestMeta{})
	for _, path := range []string{"/api/v1/apps/check", "/api/v1/apps/upload"} {
		rec = uploadAsDeployKey(t, router, path, secret,
			map[string]string{"name": "Gated", "slug": "gated", "owner_group_id": empty.ID})
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		msg, _ := body["error"].(string)
		if rec.Code != http.StatusUnprocessableEntity || body["code"] != "owner_group_empty" ||
			body["hint"] == nil || !strings.Contains(msg, "has no members") {
			t.Errorf("%s with an empty group: %d %s, want 422 owner_group_empty naming no members", path, rec.Code, rec.Body.String())
		}
		// The hint's route is the one that adds a member (TOFT-BB-102: it
		// named POST /groups/{id}/members, which does not exist).
		if hint, _ := body["hint"].(string); !strings.Contains(hint, "PUT /api/v1/groups/{id}/members/{userId}") {
			t.Errorf("%s owner_group_empty hint %q does not name PUT /api/v1/groups/{id}/members/{userId}", path, hint)
		}
	}

	// Members, none of whom could administer: the other sentence.
	viewers, _ := srv.db.CreateGroup(t.Context(), "Just Viewers", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), viewers.ID, "frozen", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	rec = uploadAsDeployKey(t, router, "/api/v1/apps/check", secret, map[string]string{"owner_group_id": viewers.ID})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "locked or a system viewer") ||
		!strings.Contains(rec.Body.String(), "owner_group_no_eligible_member") {
		t.Errorf("check with a viewers-only group: %d %s", rec.Code, rec.Body.String())
	}

	// A group that can own: the check answers the bundle, and records nothing.
	g, _ := srv.db.CreateGroup(t.Context(), "Prod Owners", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	rec = uploadAsDeployKey(t, router, "/api/v1/apps/check", secret, map[string]string{"owner_group_id": g.ID})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("check with a good group: %d %s", rec.Code, rec.Body.String())
	}
	if a, _ := srv.db.GetAppBySlug("gated"); a != nil {
		t.Error("a check, or a refused create, left an app behind")
	}
}

// A promotion decides three things about its destination: the bytes, the name
// in the URL and the audience. The first two already worked at create; this is
// the third, and it is the one that used to be refused outright to this class.
func TestAnArchiveCreateCarriesVisibility(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	secret := mintDeployKey(t, srv, router, "promotion")

	g, _ := srv.db.CreateGroup(t.Context(), "Prod Owners", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	rec := uploadAsDeployKey(t, router, "/api/v1/apps/upload", secret, map[string]string{
		"name": "Reports", "slug": "reports", "owner_group_id": g.ID,
		"visibility": database.VisibilityAuthenticated,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("archive create: %d (%s)", rec.Code, rec.Body.String())
	}
	app, err := srv.db.GetAppBySlug("reports")
	if err != nil || app == nil {
		t.Fatalf("the app was not created: %v", err)
	}
	if app.Visibility != database.VisibilityAuthenticated {
		t.Fatalf("visibility = %q, want authenticated", app.Visibility)
	}

	// The transition is what the audit records, and the actor is the key.
	var actorType, actorID string
	if err := srv.db.Conn().QueryRow(`
		SELECT actor_type, COALESCE(actor_id,'') FROM audit_log
		WHERE target_id = ? AND action = 'app.visibility_change'`, app.ID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("reading the visibility audit row: %v", err)
	}
	if actorType != "deploy_key" || actorID == "" {
		t.Fatalf("audit actor = (%s, %s), want the deploy key — a robot's act must not read "+
			"as a person with no id", actorType, actorID)
	}

	// An invalid value is refused and creates nothing.
	rec = uploadAsDeployKey(t, router, "/api/v1/apps/upload", secret, map[string]string{
		"name": "Nope", "slug": "nope", "owner_group_id": g.ID, "visibility": "everyone",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid visibility: %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if a, _ := srv.db.GetAppBySlug("nope"); a != nil {
		t.Error("a refused visibility left an app behind")
	}
}

// uploadAsDeployKey posts a minimal ZIP as a multipart bundle. The archive
// itself is not the subject of these tests — the fields beside it are — so it
// holds one file and is built here rather than fixtured.
func uploadAsDeployKey(t *testing.T, router http.Handler, path, secret string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("writing field %s: %v", k, err)
		}
	}
	fw, err := mw.CreateFormFile("file", "bundle.zip")
	if err != nil {
		t.Fatalf("creating the file part: %v", err)
	}
	zw := zip.NewWriter(fw)
	w, err := zw.Create("index.html")
	if err != nil {
		t.Fatalf("creating a zip entry: %v", err)
	}
	if _, err := w.Write([]byte("<h1>promoted</h1>")); err != nil {
		t.Fatalf("writing a zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the zip: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing the form: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, path, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// GET /apps is on the class's allow-list, and it used to answer `[]` on an
// install full of apps.
//
// A deploy key has no user id and no groups, so the scoped listing query
// matched nothing — while auth.Authorize step 1a grants the same credential
// owner on every one of those apps through the by-id read beside it. That is
// the same "permitted and impossible" shape as the archive create, and it is
// worse than a refusal: an empty list reads as an answer.
func TestADeployKeyListsEveryApp(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)

	g, _ := srv.db.CreateGroup(t.Context(), "Owners", "root", database.RequestMeta{})
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// An app owned by somebody the credential has never heard of — which is the
	// case an install-scoped promotion credential exists for.
	if rec := sendAs(t, srv, router, "member", http.MethodPost, "/api/v1/apps",
		`{"repo_url":"https://github.com/example/theirs","name":"Theirs","slug":"theirs"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seeding an app: %d (%s)", rec.Code, rec.Body.String())
	}

	for _, scope := range []string{database.DeployKeyScopeRead, database.DeployKeyScopeDeploy} {
		secret := mintScopedDeployKey(t, srv, router, "lister-"+scope, scope)
		rec := asDeployKey(t, router, http.MethodGet, "/api/v1/apps", secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s scope listing apps: %d (%s)", scope, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "theirs") {
			t.Errorf("%s scope: the list does not contain an app the key did not create. "+
				"An empty list here is not a refusal, it is a wrong answer: %s", scope, rec.Body.String())
		}
	}
}

// A deploy key that stops an app is audited as the key. The lifecycle audit
// hardcoded the user principal type and captured p.ID, which for a deploy
// key is empty, so "who bounced this app" answered "a user with no id".
func TestADeployKeysLifecycleActionsAreAuditedAsTheKey(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	app := &database.App{
		Slug: "svc", Name: "Svc", OwnerID: "root",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateAppTx(app, "", "root", "", "", ""); err != nil {
		t.Fatal(err)
	}
	secret := mintDeployKey(t, srv, router, "ci")

	if rec := asDeployKey(t, router, http.MethodPost, "/api/v1/apps/"+app.ID+"/stop", secret, ""); rec.Code >= 300 {
		t.Fatalf("stop as a deploy key = %d: %s", rec.Code, rec.Body.String())
	}

	var actorType, actorID string
	if err := srv.db.Conn().QueryRow(`
		SELECT actor_type, COALESCE(actor_id,'') FROM audit_log
		WHERE target_id = ? AND action = 'app.stop'`, app.ID).Scan(&actorType, &actorID); err != nil {
		t.Fatalf("reading the audit row: %v", err)
	}
	if actorType != "deploy_key" || actorID == "" {
		t.Fatalf("audit actor = (%s, %s), want the deploy key", actorType, actorID)
	}
}

// The deploy-key mint validates expires_in_days with its siblings' sentence:
// -30 used to mint a key that never expired, and 100000 one centuries out.
func TestADeployKeysExpiryIsBounded(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	for _, days := range []int{-1, 3651} {
		rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/deploy-keys",
			fmt.Sprintf(`{"name":"ci","expires_in_days":%d}`, days))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "between 0 (never) and 3650") {
			t.Errorf("expires_in_days=%d = %d %s, want 400 naming the range", days, rec.Code, rec.Body.String())
		}
	}
}

// Membership is authorization: a publisher with no relationship to a group may
// neither join it nor change its roster, and a member may.
func TestAPublisherOutsideAGroupCannotChangItsRoster(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)
	seedUser(t, srv.db, "friend", database.SystemRolePublisher)
	seedUser(t, srv.db, "nosy", database.SystemRolePublisher)
	_, g := seedGroupOwnedApp(t, srv, "held", "Holders", "member")
	base := "/api/v1/groups/" + g.ID + "/members/"

	for _, c := range []struct{ method, who string }{
		{http.MethodPut, "nosy"}, {http.MethodDelete, "member"},
	} {
		if rec := sendAs(t, srv, router, "nosy", c.method, base+c.who, ""); rec.Code != http.StatusNotFound {
			t.Errorf("outsider %s %s = %d, want 404 (%s)", c.method, c.who, rec.Code, rec.Body.String())
		}
	}
	if ids, _ := srv.db.GetUserGroupIDs("nosy"); len(ids) != 0 {
		t.Errorf("the outsider joined: %v", ids)
	}
	if rec := sendAs(t, srv, router, "member", http.MethodPut, base+"friend", ""); rec.Code != http.StatusNoContent {
		t.Errorf("a member adding a colleague = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if rec := sendAs(t, srv, router, "root", http.MethodPut, base+"nosy", ""); rec.Code != http.StatusNoContent {
		t.Errorf("an administrator adding anyone = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

// A publisher who creates a group is its first member, so they can fill it.
func TestAGroupsCreatorIsItsFirstMember(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	seedUser(t, srv.db, "friend", database.SystemRolePublisher)

	rec := sendAs(t, srv, router, "founder", http.MethodPost, "/api/v1/groups", `{"name":"Mine"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	var g database.Group
	_ = json.Unmarshal(rec.Body.Bytes(), &g)
	if rec := sendAs(t, srv, router, "founder", http.MethodPut,
		"/api/v1/groups/"+g.ID+"/members/friend", ""); rec.Code != http.StatusNoContent {
		t.Errorf("creator adding a colleague = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}
