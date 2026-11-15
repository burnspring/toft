package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The reserved namespace has to fail closed, and this file is the mechanism.
//
// Today the character is unreachable: sanitizeSlug replaces `_` with `-` and
// then trims, so a caller typing `_faq` gets `faq` and never reaches the guard.
// That is exactly why the guard cannot be tested by typing `_faq` at the API and
// hoping — such a test would pass on a build with the guard deleted.
//
// So these tests loosen `slugRe` for their duration, which is the one change
// that would open the namespace, and then assert every creation path still
// refuses. The enumeration is the specification, the same discipline as the
// source policy's seven enforcement points: one more path that turns a
// `_`-slug into a running app is a bug of the same class as a handler that
// forgets to authorize.

// allowUnderscoreInSlugs simulates the future in which somebody loosens slug
// normalization without noticing this file exists.
func allowUnderscoreInSlugs(t *testing.T) {
	t.Helper()
	original := slugRe
	slugRe = regexp.MustCompile(`[^a-z0-9_-]`)
	t.Cleanup(func() { slugRe = original })
}

func newSlugEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	db := dbtest.New(t)
	cfg := &config.Config{BaseURL: "http://localhost:8080", AppsDir: t.TempDir()}
	srv := newTestServerWithConfig(t, db, cfg)
	seedUser(t, db, "admin-user", database.SystemRoleAdmin)
	return srv, srv.Router(http.NotFoundHandler())
}

// The property the namespace rests on, asserted directly: with normalization as
// it actually ships, no caller input can produce a reserved slug. If this test
// starts failing, the guard below is what is holding the line.
func TestSanitizeSlug_CannotProduceReservedSlug(t *testing.T) {
	for _, in := range []string{"_faq", "__faq", "_", "_FAQ", " _faq ", "_faq_2"} {
		if got := sanitizeSlug(in); isReservedSlug(got) {
			t.Fatalf("sanitizeSlug(%q) = %q, which is in the reserved namespace", in, got)
		}
	}
}

// POST /apps — the front door.
func TestReservedSlug_RefusedOnCreate(t *testing.T) {
	allowUnderscoreInSlugs(t)
	srv, router := newSlugEnv(t)

	body, _ := json.Marshal(CreateAppRequest{
		RepoURL: "https://github.com/acme/thing", Name: "Thing", Slug: "_faq",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookieFor(t, srv, "admin-user"))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assertReservedRefusal(t, rr, "POST /apps")
	if app, _ := srv.db.GetAppBySlug("_faq"); app != nil {
		t.Fatal("an app was created on the reserved slug")
	}
}

// PATCH /apps/{id} — a rename is a creation path for a slug, and closing the
// front door while leaving this one open would be worse than not closing either,
// because it would read as covered.
func TestReservedSlug_RefusedOnRename(t *testing.T) {
	allowUnderscoreInSlugs(t)
	srv, router := newSlugEnv(t)

	app := &database.App{
		Slug: "ordinary", Name: "Ordinary", OwnerID: "admin-user",
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+app.ID,
		strings.NewReader(`{"slug":"_faq"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookieFor(t, srv, "admin-user"))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assertReservedRefusal(t, rr, "PATCH /apps/{id}")
	after, _ := srv.db.GetApp(app.ID)
	if after != nil && after.Slug != "ordinary" {
		t.Fatalf("slug became %q — the rename was allowed through", after.Slug)
	}
}

// POST /apps/upload — the ZIP path, which shares no code with the git one.
func TestReservedSlug_RefusedOnUpload(t *testing.T) {
	allowUnderscoreInSlugs(t)
	srv, router := newSlugEnv(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", "Thing")
	_ = mw.WriteField("slug", "_faq")
	part, err := mw.CreateFormFile("file", "thing.zip")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	part.Write(minimalZip(t))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(sessionCookieFor(t, srv, "admin-user"))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	assertReservedRefusal(t, rr, "POST /apps/upload")
	if app, _ := srv.db.GetAppBySlug("_faq"); app != nil {
		t.Fatal("an app was created on the reserved slug")
	}
}

// The gallery's deploy route. The catalog is data, never authority — including
// about what an app may be called.
func TestReservedSlug_RefusedOnGalleryDeploy(t *testing.T) {
	allowUnderscoreInSlugs(t)
	env := newGalleryEnv(t, database.GalleryAll)

	rr := env.do(t, "admin-user", http.MethodPost, "/api/v1/gallery/ex-1/deploy",
		`{"slug":"_faq"}`)

	assertReservedRefusal(t, rr, "POST /gallery/{item}/deploy")
	if app, _ := env.srv.db.GetAppBySlug("_faq"); app != nil {
		t.Fatal("an app was created on the reserved slug")
	}
}

// The availability check answers before somebody fills in the rest of the form.
// It needs no loosened normalization, because it reports on the sanitized slug
// and the platform's own app really does hold one.
func TestReservedSlug_ReportedUnavailable(t *testing.T) {
	allowUnderscoreInSlugs(t)
	srv, router := newSlugEnv(t)

	// Anonymous is refused: the answer says whether a private app holds the
	// name (security review 2026-11-10, B-3).
	anon := httptest.NewRequest(http.MethodGet, "/api/v1/slugs/_faq/available", nil)
	anon.RemoteAddr = "127.0.0.1:34567"
	if rr := httptest.NewRecorder(); func() int { router.ServeHTTP(rr, anon); return rr.Code }() != http.StatusUnauthorized {
		t.Fatalf("an anonymous slug check was answered")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/slugs/_faq/available", nil)
	req.RemoteAddr = "127.0.0.1:34567"
	req.AddCookie(sessionCookieFor(t, srv, "admin-user"))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	var got struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rr.Body.String())
	}
	if got.Available {
		t.Fatal("a reserved slug was reported available")
	}
	if got.Reason == "" {
		t.Fatal("unavailable with no reason — a name nothing appears to use is the " +
			"kind of thing people retry")
	}
}

// minimalZip is just enough archive for the upload path to get as far as the
// slug check, which is all these tests need it to do.
func minimalZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// A page at the root: something a create recognises, since it refuses a
	// tree it does not before it claims the slug.
	f, err := w.Create("index.html")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := io.WriteString(f, "<p>hello</p>\n"); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func assertReservedRefusal(t *testing.T, rr *httptest.ResponseRecorder, where string) {
	t.Helper()
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("%s on a reserved slug = %d, want 400 (body %s)", where, rr.Code, rr.Body.String())
	}
	var got struct{ Code string }
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Code != "reserved_slug" {
		t.Fatalf("%s refused with code %q, want reserved_slug — a caller has to be able "+
			"to tell this apart from a name that is merely taken", where, got.Code)
	}
}
