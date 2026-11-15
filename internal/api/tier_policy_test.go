package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/entitlements"
)

// The three enforcement points, each tested on its own and each **through the
// create path** rather than by calling the helper. A test that asserted
// `refusedTierCap` returns true would pass against a build where one of the
// three handlers never calls it, which is the only failure worth catching here.
//
// Each is also asserted to refuse *before* its expensive step: no slug
// allocated, no multipart parsed, no archive fetched. A cap that refuses after
// 100MB has travelled is the worst-tempered refusal in the product.

type tierEnv struct {
	db     *database.DB
	srv    *Server
	router http.Handler
}

func newTierEnv(t *testing.T, tier entitlements.Tier) *tierEnv {
	t.Helper()
	db := dbtest.New(t)
	cfg := &config.Config{BaseURL: "http://localhost:8080", AppsDir: t.TempDir()}
	srv := newTestServerWithConfig(t, db, cfg, WithEntitlements(entitlements.AtTier(tier)))
	seedUser(t, db, "admin-user", database.SystemRoleAdmin)
	seedUser(t, db, "publisher", database.SystemRolePublisher)
	return &tierEnv{db: db, srv: srv, router: srv.Router(http.NotFoundHandler())}
}

// fill creates n apps straight through the database, which is exactly the
// sibling D6 says a cap must not live behind: every fixture in this package
// builds apps this way, and a cap in `CreateApp` would be enforced on half the
// tree and bypassed by the other half.
func (e *tierEnv) fill(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("app-%02d", i)
		if err := e.db.CreateApp(&database.App{
			ID: slug, Slug: slug, Name: slug, OwnerID: "admin-user",
			RepoURL: "https://github.com/acme/" + slug, Branch: "main",
			Status: database.AppStatusRunning,
		}); err != nil {
			t.Fatalf("CreateApp: %v", err)
		}
	}
}

func (e *tierEnv) post(t *testing.T, who, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func assertTierRefusal(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	// 403 and not 402: 402 falls out of `client.Error.Unauthorized()` in `toft`
	// and would report a commercial refusal as a build failure.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s = %d, want 403 (%s)", what, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"tier_limit"`) {
		t.Errorf("%s: no tier_limit code, so `toft` cannot tell this from a permission problem: %s",
			what, body)
	}
	// The sentence is a commercial artefact, not an error string: it names the
	// tier, the number, what raises it, and that nothing already here stopped.
	for _, phrase := range []string{"demo tier", "free license key", "keeps serving"} {
		if !strings.Contains(body, phrase) {
			t.Errorf("%s: the refusal does not say %q: %s", what, phrase, body)
		}
	}
}

// fillRouteParams turns a chi pattern into a concrete path. The ids name
// nothing that exists, which is right: the license gate has to run before the
// lookup, or an unlicensed install would learn which ids are real.
func fillRouteParams(route string) string {
	out := route
	for {
		i := strings.IndexByte(out, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(out[i:], '}')
		if j < 0 {
			return out
		}
		out = out[:i] + "nope" + out[i+j+1:]
	}
}

func (e *tierEnv) request(t *testing.T, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}
