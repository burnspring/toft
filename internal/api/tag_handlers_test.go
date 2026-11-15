package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/tags"
)

// sendAs issues a request with a JSON body through the router as the named
// user, or anonymously when who is empty.
func sendAs(t *testing.T, srv *Server, router http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func mkTestApp(t *testing.T, srv *Server, slug, owner, visibility, framework string) *database.App {
	t.Helper()
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: owner,
		Framework: framework, Status: database.AppStatusRunning,
		Visibility: visibility,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("create app %s: %v", slug, err)
	}
	return app
}

// createTag makes a custom tag through the API as an admin and returns its slug.
func createTag(t *testing.T, srv *Server, router http.Handler, admin, name string) database.Tag {
	t.Helper()
	rec := sendAs(t, srv, router, admin, http.MethodPost, "/api/v1/admin/tags",
		fmt.Sprintf(`{"name":%q}`, name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create tag %q: status = %d, want 201 (body=%s)", name, rec.Code, rec.Body.String())
	}
	var tag database.Tag
	if err := json.Unmarshal(rec.Body.Bytes(), &tag); err != nil {
		t.Fatalf("decode tag: %v", err)
	}
	return tag
}

func TestTagVocabularyIsAdminOnly(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "publisher", database.SystemRolePublisher)

	cases := []struct {
		name, who string
		want      int
	}{
		{"anonymous", "", http.StatusUnauthorized},
		{"publisher", "publisher", http.StatusForbidden},
		{"admin", "admin", http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := sendAs(t, srv, router, tc.who, http.MethodPost, "/api/v1/admin/tags",
				fmt.Sprintf(`{"name":"Dept %s"}`, tc.name))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The vocabulary itself is readable by anyone — the picker and the filters need
// it — even though only admins can change it.
func TestListTagsIsReadableByAnyone(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	createTag(t, srv, router, "admin", "Department")

	rec := getAs(t, srv, router, "", "/api/v1/tags")
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous GET /tags: status = %d, want 200", rec.Code)
	}
	var list []database.Tag
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Framework (seeded) + Department.
	if len(list) != 2 {
		t.Fatalf("got %d tags, want 2: %+v", len(list), list)
	}
	if list[0].Kind != database.TagKindSystem {
		t.Errorf("first tag kind = %q, want the system tag listed first", list[0].Kind)
	}
}

func TestSystemTagCannotBeModified(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)

	path := "/api/v1/admin/tags/" + tags.FrameworkTagID

	rec := sendAs(t, srv, router, "admin", http.MethodPatch, path, `{"name":"Stack"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("PATCH system tag: status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	rec = sendAs(t, srv, router, "admin", http.MethodDelete, path, "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("DELETE system tag: status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestSetAppTagsAuthorization(t *testing.T) {
	srv := newTestServer(t)
	rungOff(t, srv)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "editor", database.SystemRolePublisher)
	seedUser(t, srv.db, "viewer", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	app := mkTestApp(t, srv, "shared", "owner", database.VisibilityPrivate, "flask")
	tag := createTag(t, srv, router, "admin", "Department")

	for _, g := range []struct{ who, role string }{
		{"editor", database.ObjectRoleEditor},
		{"viewer", database.ObjectRoleViewer},
	} {
		if err := srv.db.UpsertGrant(t.Context(), app.ID, database.PrincipalTypeUser, g.who,
			g.role, "owner", database.RequestMeta{}); err != nil {
			t.Fatalf("grant %s: %v", g.who, err)
		}
	}

	body := fmt.Sprintf(`{"tags":{%q:"Engineering"}}`, tag.Slug)
	path := "/api/v1/apps/" + app.ID + "/tags"

	cases := []struct {
		name, who string
		want      int
	}{
		{"anonymous", "", http.StatusUnauthorized},
		// 404 over 403: a stranger must not learn the private app exists.
		{"stranger", "stranger", http.StatusNotFound},
		{"viewer can read but not edit", "viewer", http.StatusForbidden},
		{"editor", "editor", http.StatusOK},
		{"owner", "owner", http.StatusOK},
		{"admin bypasses", "admin", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := sendAs(t, srv, router, tc.who, http.MethodPut, path, body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Framework is assigned by the platform. Accepting a value for it would let the
// stored tag drift away from the app's real framework.
func TestSettingASystemTagIsRejected(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := mkTestApp(t, srv, "demo", "owner", database.VisibilityPrivate, "nextjs")

	rec := sendAs(t, srv, router, "owner", http.MethodPut, "/api/v1/apps/"+app.ID+"/tags",
		`{"tags":{"framework":"Django"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	if n, _ := srv.db.CountAppsWithTag(tags.FrameworkTagID); n != 0 {
		t.Errorf("%d app_tags rows written for the Framework tag, want 0", n)
	}
}

func TestSetAppTagsValidation(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := mkTestApp(t, srv, "demo", "owner", database.VisibilityPrivate, "nextjs")
	tag := createTag(t, srv, router, "admin", "Department")
	path := "/api/v1/apps/" + app.ID + "/tags"

	long := ""
	for range 65 {
		long += "x"
	}
	tooMany := "{\"tags\":{"
	for i := range 21 {
		if i > 0 {
			tooMany += ","
		}
		tooMany += fmt.Sprintf(`"t%d":"v"`, i)
	}
	tooMany += "}}"

	cases := []struct {
		name, body string
		want       int
	}{
		{"unknown tag", `{"tags":{"nope":"x"}}`, http.StatusBadRequest},
		{"value too long", fmt.Sprintf(`{"tags":{%q:%q}}`, tag.Slug, long), http.StatusBadRequest},
		{"value with no slug characters", fmt.Sprintf(`{"tags":{%q:"***"}}`, tag.Slug), http.StatusBadRequest},
		{"too many tags", tooMany, http.StatusBadRequest},
		{"malformed body", `not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := sendAs(t, srv, router, "owner", http.MethodPut, path, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// D-106: an unknown tag is its own code with a hint, not the envelope's
	// generic `invalid`.
	rec := sendAs(t, srv, router, "owner", http.MethodPut, path, `{"tags":{"nope":"x"}}`)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "unknown_tag" || body["hint"] == nil || body["hint"] == "" {
		t.Errorf("unknown tag: %s, want code unknown_tag with a hint", rec.Body.String())
	}

	// A rejected request must not have written anything.
	stored, _ := srv.db.TagsForApp(app.ID)
	if len(stored) != 0 {
		t.Errorf("app carries %d tags after only-invalid requests, want 0", len(stored))
	}
}

// Setting a value replaces the previous one, and a new spelling snaps to the
// one already in use rather than forking the vocabulary.
func TestSetAppTagsReplacesAndSnaps(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	first := mkTestApp(t, srv, "first", "owner", database.VisibilityPrivate, "nextjs")
	second := mkTestApp(t, srv, "second", "owner", database.VisibilityPrivate, "flask")
	tag := createTag(t, srv, router, "admin", "Department")

	set := func(app *database.App, value string) []database.AppTag {
		t.Helper()
		rec := sendAs(t, srv, router, "owner", http.MethodPut, "/api/v1/apps/"+app.ID+"/tags",
			fmt.Sprintf(`{"tags":{%q:%q}}`, tag.Slug, value))
		if rec.Code != http.StatusOK {
			t.Fatalf("set %q: status = %d (body=%s)", value, rec.Code, rec.Body.String())
		}
		var out []database.AppTag
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	got := set(first, "Engineering")
	// The derived Framework pair rides along with the stored value.
	if len(got) != 2 {
		t.Fatalf("got %d tags, want Framework + Department: %+v", len(got), got)
	}
	if got[0].TagID != tags.FrameworkTagID || got[0].Value != "Next.js" {
		t.Errorf("first tag = %+v, want the derived Framework pair", got[0])
	}

	got = set(first, "Product")
	stored, _ := srv.db.TagsForApp(first.ID)
	if len(stored) != 1 || stored[0].Value != "Product" {
		t.Errorf("stored = %+v, want exactly one Product value", stored)
	}

	// A differently-cased spelling joins the existing one.
	set(second, "product")
	stored, _ = srv.db.TagsForApp(second.ID)
	if len(stored) != 1 || stored[0].Value != "Product" {
		t.Errorf("stored = %+v; the value should have snapped to \"Product\"", stored)
	}

	// An empty value clears the tag.
	set(first, "")
	stored, _ = srv.db.TagsForApp(first.ID)
	if len(stored) != 0 {
		t.Errorf("stored = %+v after clearing, want none", stored)
	}
}

// Browsing by tag must not become a way to enumerate apps the caller can't see.
func TestListAppsByTagRespectsVisibility(t *testing.T) {
	srv := newTestServer(t)
	rungOff(t, srv)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	pub := mkTestApp(t, srv, "public-app", "owner", database.VisibilityPublic, "nextjs")
	priv := mkTestApp(t, srv, "private-app", "owner", database.VisibilityPrivate, "nextjs")
	tag := createTag(t, srv, router, "admin", "Department")

	for _, app := range []*database.App{pub, priv} {
		rec := sendAs(t, srv, router, "owner", http.MethodPut, "/api/v1/apps/"+app.ID+"/tags",
			fmt.Sprintf(`{"tags":{%q:"Engineering"}}`, tag.Slug))
		if rec.Code != http.StatusOK {
			t.Fatalf("tag %s: status = %d (body=%s)", app.Slug, rec.Code, rec.Body.String())
		}
	}

	count := func(who, path string) int {
		t.Helper()
		rec := getAs(t, srv, router, who, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status = %d (body=%s)", who, path, rec.Code, rec.Body.String())
		}
		var out []map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(out)
	}

	custom := "/api/v1/apps?tag=" + tag.Slug + "&value=engineering"
	if n := count("stranger", custom); n != 1 {
		t.Errorf("stranger saw %d apps for the custom tag, want just the public one", n)
	}
	if n := count("owner", custom); n != 2 {
		t.Errorf("owner saw %d apps, want 2", n)
	}

	// The Framework tag takes the derived branch; it must be scoped too.
	system := "/api/v1/apps?tag=framework&value=nextjs"
	if n := count("stranger", system); n != 1 {
		t.Errorf("stranger saw %d apps for the Framework tag, want just the public one", n)
	}
	if n := count("owner", system); n != 2 {
		t.Errorf("owner saw %d nextjs apps, want 2", n)
	}

	// An unknown tag matches nothing rather than erroring the whole listing.
	if n := count("owner", "/api/v1/apps?tag=nonexistent"); n != 0 {
		t.Errorf("unknown tag returned %d apps, want 0", n)
	}
}

func TestTagValueSuggestionsAreViewerScoped(t *testing.T) {
	srv := newTestServer(t)
	rungOff(t, srv)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)

	pub := mkTestApp(t, srv, "public-app", "owner", database.VisibilityPublic, "nextjs")
	priv := mkTestApp(t, srv, "private-app", "owner", database.VisibilityPrivate, "nextjs")
	tag := createTag(t, srv, router, "admin", "Department")

	for app, value := range map[*database.App]string{pub: "Engineering", priv: "Project Titan"} {
		rec := sendAs(t, srv, router, "owner", http.MethodPut, "/api/v1/apps/"+app.ID+"/tags",
			fmt.Sprintf(`{"tags":{%q:%q}}`, tag.Slug, value))
		if rec.Code != http.StatusOK {
			t.Fatalf("tag %s: status = %d (body=%s)", app.Slug, rec.Code, rec.Body.String())
		}
	}

	values := func(who string) []database.TagValue {
		t.Helper()
		rec := getAs(t, srv, router, who, "/api/v1/tags/"+tag.Slug+"/values")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body=%s)", who, rec.Code, rec.Body.String())
		}
		var out []database.TagValue
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	got := values("stranger")
	if len(got) != 1 || got[0].Value != "Engineering" {
		t.Errorf("stranger saw %+v; the private app's value leaked into the autocomplete", got)
	}
	if len(values("owner")) != 2 {
		t.Errorf("owner saw %d values, want 2", len(values("owner")))
	}

	if rec := getAs(t, srv, router, "owner", "/api/v1/tags/nope/values"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown tag values: status = %d, want 404", rec.Code)
	}
}

func TestDeleteTagInUseNeedsForce(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := mkTestApp(t, srv, "demo", "owner", database.VisibilityPrivate, "nextjs")
	tag := createTag(t, srv, router, "admin", "Department")

	rec := sendAs(t, srv, router, "owner", http.MethodPut, "/api/v1/apps/"+app.ID+"/tags",
		fmt.Sprintf(`{"tags":{%q:"Engineering"}}`, tag.Slug))
	if rec.Code != http.StatusOK {
		t.Fatalf("set tag: status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	path := "/api/v1/admin/tags/" + tag.ID
	rec = sendAs(t, srv, router, "admin", http.MethodDelete, path, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in-use tag: status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}

	rec = sendAs(t, srv, router, "admin", http.MethodDelete, path+"?force=true", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("force delete: status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if stored, _ := srv.db.TagsForApp(app.ID); len(stored) != 0 {
		t.Errorf("app still carries %d tags after the tag was deleted", len(stored))
	}
}

func TestDuplicateTagNameIsRejected(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	createTag(t, srv, router, "admin", "Department")

	rec := sendAs(t, srv, router, "admin", http.MethodPost, "/api/v1/admin/tags", `{"name":"department"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"name_taken"`) {
		t.Errorf("status = %d, want 409 name_taken (body=%s)", rec.Code, rec.Body.String())
	}
}

// Renaming must not move the permalink out from under existing links.
func TestRenamingATagKeepsItsSlug(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	tag := createTag(t, srv, router, "admin", "Department")

	rec := sendAs(t, srv, router, "admin", http.MethodPatch, "/api/v1/admin/tags/"+tag.ID, `{"name":"Team"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var got database.Tag
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Team" {
		t.Errorf("name = %q, want Team", got.Name)
	}
	if got.Slug != tag.Slug {
		t.Errorf("slug changed from %q to %q; existing /tags links would break", tag.Slug, got.Slug)
	}
}

func TestTagMutationsAreAudited(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "admin", database.SystemRoleAdmin)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	app := mkTestApp(t, srv, "demo", "owner", database.VisibilityPrivate, "nextjs")

	before, err := srv.db.CountAuditEntries()
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}

	tag := createTag(t, srv, router, "admin", "Department")
	// An admin editing someone else's app still goes through the audit path.
	rec := sendAs(t, srv, router, "admin", http.MethodPut, "/api/v1/apps/"+app.ID+"/tags",
		fmt.Sprintf(`{"tags":{%q:"Engineering"}}`, tag.Slug))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin set tag: status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	after, err := srv.db.CountAuditEntries()
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	// tag.create + app.tag_set
	if after-before < 2 {
		t.Errorf("audit grew by %d, want at least 2 (tag.create and app.tag_set)", after-before)
	}
}
