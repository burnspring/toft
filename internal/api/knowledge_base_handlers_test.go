package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// The knowledge base is a settings row and a link, and the only part of it that
// can be silently wrong is who is told the link exists. These tests are mostly
// about that: an administrator picks an app, and `auth.Authorize` gives that
// administrator an owner-shaped answer on every app — so the person doing the
// picking is the one person who cannot tell by looking whether anybody else can
// open it.
//
// The rule under test is that **"no knowledge base", "the app is gone" and "you
// may not read it" are the same bytes.** A distinguishable answer turns the
// route into an oracle for "does this install have an app called X".

func kbEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()

	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "stranger", database.SystemRolePublisher)
	seedUser(t, srv.db, "boss", database.SystemRoleAdmin)
	return srv, srv.Router(http.NotFoundHandler())
}

func kbApp(t *testing.T, srv *Server, slug, framework, visibility string) *database.App {
	t.Helper()

	app := &database.App{
		Slug: slug, Name: strings.ToUpper(slug[:1]) + slug[1:], OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: visibility, Framework: framework,
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app %s: %v", slug, err)
	}
	return app
}

func kbWriteAs(t *testing.T, srv *Server, router http.Handler, who, method, body string) *httptest.ResponseRecorder {
	t.Helper()

	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/v1/admin/knowledge-base", nil)
	} else {
		r = httptest.NewRequest(method, "/api/v1/admin/knowledge-base", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		r.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// kbResolve returns the raw body of the resolving route, because the assertion
// this file cares most about is byte equality between three different negative
// answers.
func kbResolve(t *testing.T, srv *Server, router http.Handler, who string) (int, string) {
	t.Helper()

	rec := getAs(t, srv, router, who, "/api/v1/knowledge-base")
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func kbPick(t *testing.T, srv *Server, router http.Handler, appID, label string) {
	t.Helper()

	kbPickWithDescription(t, srv, router, appID, label, "")
}

func kbPickWithDescription(t *testing.T, srv *Server, router http.Handler, appID, label, description string) {
	t.Helper()

	body, _ := json.Marshal(map[string]string{"app_id": appID, "label": label, "description": description})
	rec := kbWriteAs(t, srv, router, "boss", http.MethodPut, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func kbAuditCount(t *testing.T, srv *Server, action string) int {
	t.Helper()

	var n int
	if err := srv.db.Conn().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatalf("counting %s rows: %v", action, err)
	}
	return n
}

// The empty answer, established once so every other test can compare against a
// constant rather than against itself.
const kbNothing = `{}`

func TestKnowledgeBase_NoPick_IsEmpty(t *testing.T) {
	srv, router := kbEnv(t)

	code, body := kbResolve(t, srv, router, "stranger")
	if code != http.StatusOK || body != kbNothing {
		t.Fatalf("resolve = %d %q, want 200 %q", code, body, kbNothing)
	}
}

// The disclosure rule, stated as byte equality. A private app picked by an
// administrator must look, to everybody else, exactly like an install that has
// picked nothing.
func TestKnowledgeBase_PrivateApp_LooksLikeNoPick(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityPrivate)
	kbPick(t, srv, router, app.ID, "Handbook")

	code, body := kbResolve(t, srv, router, "stranger")
	if code != http.StatusOK || body != kbNothing {
		t.Fatalf("resolve for a stranger = %d %q, want 200 %q", code, body, kbNothing)
	}
	if strings.Contains(body, "handbook") || strings.Contains(body, "Handbook") {
		t.Errorf("the slug or the label leaked to somebody who may not read the app: %s", body)
	}
}

// The same app, for the two people who may read it. The administrator's case is
// asserted rather than tolerated: the bypass is intended, and it is exactly why
// the admin page has to warn.
func TestKnowledgeBase_PrivateApp_ResolvesForOwnerAndAdmin(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityPrivate)
	kbPick(t, srv, router, app.ID, "Handbook")

	for _, who := range []string{"owner", "boss"} {
		code, body := kbResolve(t, srv, router, who)
		if code != http.StatusOK {
			t.Fatalf("resolve for %s = %d", who, code)
		}
		var got knowledgeBaseResponse
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("resolve for %s: %v", who, err)
		}
		if got.Slug != "handbook" || got.Label != "Handbook" || got.AppID != app.ID {
			t.Errorf("resolve for %s = %+v, want the picked app", who, got)
		}
	}
}

// The ask, in one test: "available to all logged in users" is the visibility
// that already means it.
func TestKnowledgeBase_AuthenticatedApp_ResolvesForEverySignedInUser(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)
	kbPick(t, srv, router, app.ID, "Handbook")

	_, body := kbResolve(t, srv, router, "stranger")
	var got knowledgeBaseResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Slug != "handbook" {
		t.Errorf("resolve = %+v, want the picked app for an unrelated signed-in user", got)
	}
}

// Anonymous is refused by construction, not by reading the app. A `public` app
// is the case that proves it: the read gate would allow it.
func TestKnowledgeBase_PublicApp_IsStillNothingAnonymously(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityPublic)
	kbPick(t, srv, router, app.ID, "Handbook")

	code, body := kbResolve(t, srv, router, "")
	if code != http.StatusOK || body != kbNothing {
		t.Fatalf("anonymous resolve = %d %q, want 200 %q", code, body, kbNothing)
	}

	if _, signedIn := kbResolve(t, srv, router, "stranger"); signedIn == kbNothing {
		t.Error("a public app resolved for nobody — the anonymous refusal is not supposed to reach signed-in callers")
	}
}

// The settings table has no foreign key, so a deleted app leaves the row
// behind. Resolution answers as if nothing were picked, and writes nothing.
func TestKnowledgeBase_DanglingPick_IsEmptyAndWritesNothing(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)
	kbPick(t, srv, router, app.ID, "Handbook")

	if err := srv.db.DeleteApp(app.ID); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	code, body := kbResolve(t, srv, router, "stranger")
	if code != http.StatusOK || body != kbNothing {
		t.Fatalf("resolve after delete = %d %q, want 200 %q", code, body, kbNothing)
	}

	// Nothing repaired on a read: the row is still there for the admin page to
	// find, name and clear.
	setting, err := srv.db.GetSetting(knowledgeBaseKey)
	if err != nil {
		t.Fatal(err)
	}
	if setting == nil {
		t.Error("the read path deleted the settings row — nothing is written on a read")
	}

	// And the administrator can still see what it names, which is the whole
	// reason that route is separate from the resolving one.
	rec := kbWriteAs(t, srv, router, "boss", http.MethodGet, "")
	var admin adminKnowledgeBaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	if admin.AppID != app.ID {
		t.Errorf("admin read = %+v, want the dangling app id so the page can say so", admin)
	}
}

// D2, made checkable. One behaviour across every family — nothing in this
// feature reads apps.framework, and a test that walks the frameworks is what
// keeps that true.
func TestKnowledgeBase_AppTypeIsNotAnInput(t *testing.T) {
	for _, framework := range []string{"static", "quarto", "notebook", "flask", "nextjs", "go", "shiny"} {
		t.Run(framework, func(t *testing.T) {
			srv, router := kbEnv(t)
			app := kbApp(t, srv, "handbook", framework, database.VisibilityAuthenticated)
			kbPick(t, srv, router, app.ID, "Handbook")

			_, body := kbResolve(t, srv, router, "stranger")
			var got knowledgeBaseResponse
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got.Slug != "handbook" {
				t.Errorf("a %s app did not resolve: %+v", framework, got)
			}
		})
	}
}

// A rename is a supported operation, which is why the id is what is stored.
// The slug follows the app; the label does not follow the name.
func TestKnowledgeBase_Rename_MovesTheSlugAndNotTheLabel(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)
	kbPick(t, srv, router, app.ID, "Handbook")

	rec := patchAppAs(t, srv, router, "owner", app.ID, `{"slug":"staff-guide","name":"Staff guide"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body.String())
	}

	_, body := kbResolve(t, srv, router, "stranger")
	var got knowledgeBaseResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Slug != "staff-guide" {
		t.Errorf("slug = %q, want the renamed slug — a stored slug would 404 on the gateway", got.Slug)
	}
	if got.Label != "Handbook" {
		t.Errorf("label = %q, want the stored label — renaming an app must not rename a word in the chrome", got.Label)
	}
}

func TestKnowledgeBase_WriteRoutesAreAdminOnly(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)

	body, _ := json.Marshal(map[string]string{"app_id": app.ID, "label": "Handbook"})
	for _, who := range []string{"owner", "stranger"} {
		if rec := kbWriteAs(t, srv, router, who, http.MethodPut, string(body)); rec.Code != http.StatusForbidden {
			t.Errorf("PUT as %s = %d, want 403", who, rec.Code)
		}
		if rec := kbWriteAs(t, srv, router, who, http.MethodDelete, ""); rec.Code != http.StatusForbidden {
			t.Errorf("DELETE as %s = %d, want 403", who, rec.Code)
		}
		if rec := kbWriteAs(t, srv, router, who, http.MethodGet, ""); rec.Code != http.StatusForbidden {
			t.Errorf("admin GET as %s = %d, want 403", who, rec.Code)
		}
	}

	if s, err := srv.db.GetSetting(knowledgeBaseKey); err != nil || s != nil {
		t.Errorf("a refused write left a settings row: %v %v", s, err)
	}
	if n := kbAuditCount(t, srv, "knowledge_base.set"); n != 0 {
		t.Errorf("a refused write left %d audit rows", n)
	}
}

func TestKnowledgeBase_PutRefusals(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)

	long := strings.Repeat("x", knowledgeBaseLabelMaxLen+1)
	longDesc := strings.Repeat("x", knowledgeBaseDescriptionMaxLen+1)
	for name, body := range map[string]string{
		"no app":               `{"app_id":"","label":"Handbook"}`,
		"unknown app":          `{"app_id":"no-such-app","label":"Handbook"}`,
		"no label":             `{"app_id":"` + app.ID + `","label":"   "}`,
		"label too long":       `{"app_id":"` + app.ID + `","label":"` + long + `"}`,
		"description too long": `{"app_id":"` + app.ID + `","label":"Handbook","description":"` + longDesc + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := kbWriteAs(t, srv, router, "boss", http.MethodPut, body); rec.Code != http.StatusBadRequest {
				t.Errorf("PUT = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}

	if s, err := srv.db.GetSetting(knowledgeBaseKey); err != nil || s != nil {
		t.Errorf("a refused write left a settings row: %v %v", s, err)
	}
}

// The label is capped in runes, not bytes: a cap that counts bytes would refuse
// a shorter word in a language that does not fit in one byte per character.
func TestKnowledgeBase_LabelCapCountsRunes(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)

	label := strings.Repeat("社", knowledgeBaseLabelMaxLen)
	body, _ := json.Marshal(map[string]string{"app_id": app.ID, "label": label})
	if rec := kbWriteAs(t, srv, router, "boss", http.MethodPut, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 — the cap counted bytes: %s", rec.Code, rec.Body.String())
	}
}

// An install-wide change to what everybody sees in the chrome, with no row
// saying who made it, is what the audit exists to prevent.
func TestKnowledgeBase_SetAndClearAreAudited(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)
	kbPick(t, srv, router, app.ID, "Handbook")

	if n := kbAuditCount(t, srv, "knowledge_base.set"); n != 1 {
		t.Fatalf("knowledge_base.set rows = %d, want 1", n)
	}

	var actor, target, after string
	if err := srv.db.Conn().QueryRow(
		`SELECT actor_id, target_id, COALESCE(after::text,'') FROM audit_log
		 WHERE action = 'knowledge_base.set'`).Scan(&actor, &target, &after); err != nil {
		t.Fatal(err)
	}
	if actor != "boss" {
		t.Errorf("actor = %q, want the administrator who set it", actor)
	}
	if target != app.ID {
		t.Errorf("target = %q, want the picked app", target)
	}
	if !strings.Contains(after, "Handbook") {
		t.Errorf("after = %q, want the label that was set", after)
	}

	rec := kbWriteAs(t, srv, router, "boss", http.MethodDelete, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if n := kbAuditCount(t, srv, "knowledge_base.clear"); n != 1 {
		t.Errorf("knowledge_base.clear rows = %d, want 1", n)
	}

	if code, body := kbResolve(t, srv, router, "stranger"); body != kbNothing {
		t.Errorf("resolve after clear = %d %q, want %q", code, body, kbNothing)
	}
}

// Replacing one pick with another records what it replaced. A `before` that is
// absent on a change is how a settings audit stops being able to answer "what
// did it used to point at".
func TestKnowledgeBase_ReplacingAPickRecordsTheOldOne(t *testing.T) {
	srv, router := kbEnv(t)
	first := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)
	second := kbApp(t, srv, "wiki", "flask", database.VisibilityAuthenticated)

	kbPick(t, srv, router, first.ID, "Handbook")
	kbPick(t, srv, router, second.ID, "Wiki")

	var before string
	if err := srv.db.Conn().QueryRow(
		`SELECT COALESCE(before::text,'') FROM audit_log
		 WHERE action = 'knowledge_base.set' AND target_id = $1`, second.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, first.ID) {
		t.Errorf("before = %q, want the pick it replaced", before)
	}
}

// The announcement's audit family, at last.
//
// `event_handlers.go` has offered an `announcement.*` filter to the settings
// view since it was written, and nothing in the repository has ever written an
// action beginning `announcement.` — so an administrator asking who put a line
// across every page on this install was shown a family that is always empty.
// This lives here rather than in a file of its own because the mechanism it
// leans on is the audited settings writer that the knowledge base needed first.
func TestAnnouncement_SetAndClearAreAudited(t *testing.T) {
	srv, router := kbEnv(t)

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/announcement", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", srv.cfg.BaseURL)
		r.AddCookie(sessionCookieFor(t, srv, "boss"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		return rec
	}

	if rec := put(`{"message":"Maintenance on Sunday","level":"warning"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if n := kbAuditCount(t, srv, "announcement.set"); n != 1 {
		t.Fatalf("announcement.set rows = %d, want 1", n)
	}

	// A replacement records what it replaced.
	if rec := put(`{"message":"Maintenance finished","level":"info"}`); rec.Code != http.StatusOK {
		t.Fatalf("second PUT = %d", rec.Code)
	}
	var before string
	if err := srv.db.Conn().QueryRow(
		`SELECT COALESCE(before::text,'') FROM audit_log
		 WHERE action = 'announcement.set' AND after::text LIKE '%finished%'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "Sunday") {
		t.Errorf("before = %q, want the banner it replaced", before)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/announcement", nil)
	r.Header.Set("Origin", srv.cfg.BaseURL)
	r.AddCookie(sessionCookieFor(t, srv, "boss"))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if n := kbAuditCount(t, srv, "announcement.clear"); n != 1 {
		t.Errorf("announcement.clear rows = %d, want 1", n)
	}
}

// The description an administrator writes reaches the people the pick resolves
// for, and reaches the administrator's own page as what is *stored* — which are
// two different shapes answering two different questions, and both of them have
// to carry it or the field is write-only.
func TestKnowledgeBase_DescriptionReachesBothReads(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)
	const written = "Engineering handbook, owned by the platform team."
	kbPickWithDescription(t, srv, router, app.ID, "Handbook", written)

	rec := getAs(t, srv, router, "stranger", "/api/v1/knowledge-base")
	var resolved knowledgeBaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decoding the resolved answer: %v", err)
	}
	if resolved.Description != written {
		t.Errorf("resolved description = %q, want %q", resolved.Description, written)
	}

	rec = kbWriteAs(t, srv, router, "boss", http.MethodGet, "")
	var stored adminKnowledgeBaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decoding the stored answer: %v", err)
	}
	if stored.Description != written {
		t.Errorf("stored description = %q, want %q", stored.Description, written)
	}
}

// Blank is the ordinary state and has to stay tellable from a written one: the
// client renders the product's own sentence when the field is absent, so an
// empty string on the wire would be a description that says nothing.
func TestKnowledgeBase_NoDescriptionIsAbsentRatherThanEmpty(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)
	kbPick(t, srv, router, app.ID, "Handbook")

	rec := getAs(t, srv, router, "stranger", "/api/v1/knowledge-base")
	if strings.Contains(rec.Body.String(), "description") {
		t.Errorf("an unwritten description reached the wire: %s", rec.Body.String())
	}

	// The negative answers are still one set of bytes. The new field must not
	// have made "nothing is picked" distinguishable from "not for you".
	if _, body := kbResolve(t, srv, router, ""); body != kbNothing {
		t.Errorf("anonymous answer = %s, want %s", body, kbNothing)
	}
}

// Capped in runes like the label, and for the same reason: a byte cap refuses a
// shorter sentence in a language that does not fit in one byte per character.
func TestKnowledgeBase_DescriptionCapCountsRunes(t *testing.T) {
	srv, router := kbEnv(t)
	app := kbApp(t, srv, "handbook", "static", database.VisibilityAuthenticated)

	description := strings.Repeat("社", knowledgeBaseDescriptionMaxLen)
	body, _ := json.Marshal(map[string]string{
		"app_id": app.ID, "label": "Handbook", "description": description,
	})
	if rec := kbWriteAs(t, srv, router, "boss", http.MethodPut, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 — the cap counted bytes: %s", rec.Code, rec.Body.String())
	}
}
