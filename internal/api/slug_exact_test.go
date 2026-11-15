package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// uploadExact posts the ZIP path's form with slug_exact set.
func (e *createModeEnv) uploadExact(t *testing.T, who, slug string) *httptest.ResponseRecorder {
	t.Helper()
	var buf strings.Builder
	const boundary = "----tofttest"
	field := func(name, value string) {
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString(`Content-Disposition: form-data; name="` + name + `"` + "\r\n\r\n")
		buf.WriteString(value + "\r\n")
	}
	field("name", slug)
	field("slug", slug)
	field("slug_exact", "true")
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="file"; filename="app.zip"` + "\r\n")
	buf.WriteString("Content-Type: application/zip\r\n\r\n")
	buf.WriteString("PK\x05\x06" + strings.Repeat("\x00", 18) + "\r\n")
	buf.WriteString("--" + boundary + "--\r\n")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/apps/upload", strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Origin", e.srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, e.srv, who))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// **A slug somebody stated is refused when taken, never renamed.** A retried
// `toft create --app td-node` used to claim td-node-2 beside the app the first
// try made; with slug_exact it is a 409 name_taken naming toft link, and no row
// is written. Without the field the old renaming stays, for the SPA.
func TestAnExactSlugThatIsTakenIsRefused(t *testing.T) {
	e := newCreateModeEnv(t)
	if rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"tool","name":"Tool"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"json":   e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"tool","name":"Tool","slug_exact":true}`),
		"upload": e.uploadExact(t, "root", "tool"),
	} {
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: status = %d, want 409. body: %s", name, rec.Code, rec.Body.String())
		}
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		if body["code"] != "name_taken" || !strings.Contains(body["hint"].(string), "toft link tool") {
			t.Errorf("%s: body = %v, want name_taken with a hint naming toft link", name, body)
		}
	}
	if app := e.appBySlug(t, "tool-2"); app != nil {
		t.Error("a refused exact create still claimed tool-2")
	}

	// The old behaviour, unchanged for a caller that did not ask.
	if rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"tool","name":"Tool"}`); rec.Code != http.StatusCreated {
		t.Fatalf("non-exact create: %d %s", rec.Code, rec.Body.String())
	}
	if app := e.appBySlug(t, "tool-2"); app == nil {
		t.Error("a create without slug_exact no longer renames")
	}
}

// **A repository that cannot be cloned is refused before anything is
// written.** It used to become a `failed` app holding its slug, explained by
// git's own "could not read Username … terminal prompts disabled". A network
// blip is not refused — the build retries the clone — and a connection's
// credential is the builder's to resolve.
func TestAnUnreachableRepositoryCreatesNothing(t *testing.T) {
	cases := []struct {
		name, stderr, want string
		refused            bool
	}{
		{"missing or private", "git ls-remote: fatal: could not read Username for 'https://github.com': terminal prompts disabled",
			"was not found, or it is private", true},
		{"missing branch", `branch "nope" not found on remote`, `has no branch "nope"`, true},
		{"network", "git ls-remote: fatal: unable to access: Could not resolve host: github.com", "", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCreateModeEnv(t)
			e.srv.repoPreflight = func(context.Context, string, string, string) error { return errors.New(tc.stderr) }
			slug := fmt.Sprintf("gone%d", i)
			rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/gone","branch":"nope","slug":"`+slug+`","name":"Gone"}`)
			if !tc.refused {
				if rec.Code != http.StatusCreated {
					t.Fatalf("a network blip refused the create: %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422. body: %s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			json.Unmarshal(rec.Body.Bytes(), &body)
			if body["code"] != "source_unavailable" || !strings.Contains(body["error"].(string), tc.want) {
				t.Errorf("body = %v, want source_unavailable saying %q", body, tc.want)
			}
			if app := e.appBySlug(t, slug); app != nil {
				t.Error("a refused create left an app row holding its slug")
			}
		})
	}
}

// A folder that is not there is refused like a branch that is not there, and
// a folder the check could not answer about is left to the build.
func TestAMissingSubdirCreatesNothing(t *testing.T) {
	for i, tc := range []struct {
		name    string
		exists  bool
		err     error
		refused bool
	}{
		{"missing", false, nil, true},
		{"present", true, nil, false},
		{"unknown", false, errors.New("git clone: timeout"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCreateModeEnv(t)
			e.srv.repoPreflight = func(context.Context, string, string, string) error { return nil }
			e.srv.subdirPreflight = func(context.Context, string, string, string, string) (bool, error) { return tc.exists, tc.err }
			slug := fmt.Sprintf("sub%d", i)
			rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/mono","branch":"main","repo_subdir":"apps/nope","slug":"`+slug+`","name":"Sub"}`)
			if !tc.refused {
				if rec.Code != http.StatusCreated {
					t.Fatalf("refused: %d %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `no folder \"apps/nope\"`) {
				t.Fatalf("status = %d body %s, want 422 naming the folder", rec.Code, rec.Body.String())
			}
			if app := e.appBySlug(t, slug); app != nil {
				t.Error("a refused create left an app row")
			}
		})
	}
}

// **A slug proposed from a folder's name is cut to fit; one somebody stated is
// refused.** `toft create <dir>` sends the directory's name as the slug without
// slug_exact, and a 71-character folder name was refused slug_too_long — as if
// the person had chosen it (drills, 2026-10-23). The same slug stated with
// slug_exact is still a refusal, before any row is written.
func TestAProposedSlugIsCutAndAStatedOneIsRefused(t *testing.T) {
	e := newCreateModeEnv(t)
	long := strings.Repeat("folder-name-", 6)[:71]

	stated := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"`+long+`","name":"Tool","slug_exact":true}`)
	if stated.Code != http.StatusBadRequest || !strings.Contains(stated.Body.String(), "slug_too_long") {
		t.Fatalf("a stated 71-character slug: %d %s, want 400 slug_too_long", stated.Code, stated.Body.String())
	}

	proposed := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"`+long+`","name":"Tool"}`)
	if proposed.Code != http.StatusCreated {
		t.Fatalf("a proposed 71-character slug: %d %s, want 201", proposed.Code, proposed.Body.String())
	}
	var got struct {
		App struct {
			Slug string `json:"slug"`
		} `json:"app"`
		Slug string `json:"slug"`
	}
	json.Unmarshal(proposed.Body.Bytes(), &got)
	slug := got.App.Slug
	if slug == "" {
		slug = got.Slug
	}
	if len(slug) > maxSlugLen || !strings.HasPrefix(long, strings.TrimSuffix(slug, "-")) || strings.HasSuffix(slug, "-") {
		t.Errorf("slug = %q (%d), want the folder's name cut to %d with no trailing dash", slug, len(slug), maxSlugLen)
	}
}

// **A stated slug that is not a slug is refused, not rewritten.** With
// slug_exact, "Bad_Slug" created bad-slug — an app under a name nobody typed —
// for any caller but the CLI, which refuses first (BB-006, third 2026-11-03
// black-box list). A proposed one is still turned into a slug.
func TestAStatedNonSlugIsRefusedInvalidSlug(t *testing.T) {
	e := newCreateModeEnv(t)
	stated := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"Bad_Slug","name":"Tool","slug_exact":true}`)
	if stated.Code != http.StatusBadRequest || !strings.Contains(stated.Body.String(), "invalid_slug") ||
		!strings.Contains(stated.Body.String(), "bad-slug") {
		t.Fatalf("a stated Bad_Slug: %d %s, want 400 invalid_slug naming bad-slug", stated.Code, stated.Body.String())
	}
	proposed := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"Bad_Slug","name":"Tool"}`)
	if proposed.Code != http.StatusCreated || !strings.Contains(proposed.Body.String(), "bad-slug") {
		t.Fatalf("a proposed Bad_Slug: %d %s, want 201 as bad-slug", proposed.Code, proposed.Body.String())
	}
}

// BB-070: an exact slug that is not one is said before a missing repo_url.
func TestAStatedNonSlugIsJudgedBeforeRepoURL(t *testing.T) {
	e := newCreateModeEnv(t)
	got := e.create(t, "root", `{"name":"x","slug":"Bad_X","slug_exact":true}`)
	if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "invalid_slug") {
		t.Fatalf("got %d %s, want invalid_slug", got.Code, got.Body.String())
	}
	got = e.create(t, "root", `{"name":"x"}`)
	if !strings.Contains(got.Body.String(), "repo_url is required") || !strings.Contains(got.Body.String(), `"hint"`) {
		t.Fatalf("no repo_url: %d %s, want a hinted refusal", got.Code, got.Body.String())
	}
}
