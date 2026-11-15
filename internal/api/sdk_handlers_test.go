package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/version"
	"toft/sdk"
)

func getSDK(t *testing.T, srv *Server, router http.Handler, lang, who string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sdk/"+lang, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// The route is authenticated, and that is a decision rather than a default:
// auth.Middleware lets an anonymous caller through with a nil principal, so a
// handler that did not look would be an open route.
func TestTheSDKRouteIsNotAnonymous(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	if rec := getSDK(t, srv, router, "go", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /sdk/go = %d, want 401", rec.Code)
	}
}

// What the route serves is what the build would have installed. That is the
// whole argument for serving it at all rather than publishing a package: a
// developer compiles against the copy belonging to the install they deploy to.
func TestTheSDKRouteServesWhatTheBuildInstalls(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "dev", database.SystemRolePublisher)
	router := srv.Router(http.NotFoundHandler())

	for _, lang := range sdk.Languages() {
		rec := getSDK(t, srv, router, lang, "dev")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /sdk/%s = %d, body=%s", lang, rec.Code, rec.Body.String())
		}
		var got struct {
			Language      string            `json:"language"`
			Module        string            `json:"module"`
			Package       string            `json:"package"`
			LanguageFloor string            `json:"language_floor"`
			Version       string            `json:"version"`
			Files         map[string]string `json:"files"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want, err := sdk.Files(lang)
		if err != nil {
			t.Fatal(err)
		}
		if got.Language != lang || len(got.Files) != len(want.Files) {
			t.Errorf("%s: language=%q, %d files, want %q and %d", lang, got.Language, len(got.Files), lang, len(want.Files))
		}
		for path, body := range want.Files {
			if got.Files[path] != body {
				t.Errorf("%s/%s on the wire differs from what the build writes", lang, path)
			}
		}
		// The install's own release, not the SDK's declared version: it is what
		// answers "is my vendored copy still the one this install would build".
		if got.Version != version.Version {
			t.Errorf("%s: version = %q, want the install's %q", lang, got.Version, version.Version)
		}
		if (got.Module == "") == (got.Package == "") {
			t.Errorf("%s: module=%q package=%q — exactly one names the SDK", lang, got.Module, got.Package)
		}
	}
}

// Go carries the one extra fact a client cannot work out for itself, because
// the workspace it writes may not declare less than the module it uses.
func TestTheGoAnswerCarriesTheLanguageFloor(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "dev", database.SystemRolePublisher)
	router := srv.Router(http.NotFoundHandler())

	rec := getSDK(t, srv, router, "go", "dev")
	var got struct {
		Module        string            `json:"module"`
		LanguageFloor string            `json:"language_floor"`
		Files         map[string]string `json:"files"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)

	if got.Module != sdk.GoModule || got.LanguageFloor != sdk.GoLanguageFloor {
		t.Errorf("module = %q, floor = %q", got.Module, got.LanguageFloor)
	}
	if manifest := got.Files["go.mod"]; !strings.Contains(manifest, "module "+sdk.GoModule) {
		t.Errorf("go.mod = %q, want the generated manifest to travel with the source", manifest)
	}
}

// A client that guessed gets the short list rather than a bare 404 — the set is
// three names and the caller is a program somebody is holding wrong.
func TestAnUnknownLanguageNamesTheOnesThisInstallCarries(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "dev", database.SystemRolePublisher)
	router := srv.Router(http.NotFoundHandler())

	rec := getSDK(t, srv, router, "ruby", "dev")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /sdk/ruby = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	for _, lang := range sdk.Languages() {
		if !strings.Contains(body, lang) {
			t.Errorf("the refusal %q does not name %s", body, lang)
		}
	}
}
