package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
)

// A slug out of a URL names an app no more than an id does.
//
// The route exists so a client holding a name does not have to fetch the estate
// to resolve it — which makes it exactly the shape of endpoint that turns into
// an enumeration oracle if it answers "not yours" differently from "no such
// app". It answers 404 to both.
func TestGetAppBySlugIsScopedLikeGetApp(t *testing.T) {
	e := newRollbackEnv(t)
	rungOff(t, e.srv)

	bySlug := func(who, slug string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/apps/by-slug/"+slug, nil)
		if who != "" {
			req.AddCookie(sessionCookieFor(t, e.srv, who))
		}
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("the owner gets the same view the by-id route serves", func(t *testing.T) {
		rec := bySlug("owner", e.app.Slug)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		var got struct {
			App struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
			} `json:"app"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.App.ID != e.app.ID {
			t.Errorf("resolved to %q, want %q", got.App.ID, e.app.ID)
		}

		// Byte-identical to the by-id answer, because it is the same handler
		// body: a second rendering of an app's page is one that will eventually
		// show a different set of fields.
		byID := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/apps/%s", e.app.ID), nil)
		byID.AddCookie(sessionCookieFor(t, e.srv, "owner"))
		idRec := httptest.NewRecorder()
		e.router.ServeHTTP(idRec, byID)
		if idRec.Body.String() != rec.Body.String() {
			t.Errorf("by-slug and by-id answer differently:\n by-slug: %s\n by-id:   %s",
				rec.Body.String(), idRec.Body.String())
		}
	})

	t.Run("a private app the caller may not read is 404, not 403", func(t *testing.T) {
		rec := bySlug("owner", e.otherApp.Slug)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 — 403 would confirm the app exists", rec.Code)
		}
	})

	t.Run("a slug nothing holds is the same 404", func(t *testing.T) {
		rec := bySlug("owner", "no-such-app")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("anonymous is refused", func(t *testing.T) {
		rec := bySlug("", e.app.Slug)
		if rec.Code == http.StatusOK {
			t.Errorf("status = %d, want a refusal", rec.Code)
		}
	})

	// The brief view is what `toft` resolves a slug with on every command: the
	// row the full page carries, the in-flight start, and nothing joined — the
	// full page was some twenty queries per CLI command. Same scoping.
	t.Run("view=brief is the full view's app and nothing joined", func(t *testing.T) {
		rec := bySlug("owner", e.app.Slug+"?view=brief")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		var brief map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &brief); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(brief) != 2 || brief["app"] == nil || brief["starting"] == nil {
			t.Errorf("brief keys = %v, want exactly app and starting", keysOf(brief))
		}

		full := bySlug("owner", e.app.Slug)
		var page map[string]json.RawMessage
		if err := json.Unmarshal(full.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if string(page["app"]) != string(brief["app"]) {
			t.Errorf("the brief app differs from the full view's:\n brief: %s\n full:  %s", brief["app"], page["app"])
		}
	})

	// A slug that is also the last word of a GET /apps/{appID}/<word> route
	// resolves to its own app, not to that route with appID "by-slug" (RO-2).
	t.Run("a slug named like an app subroute resolves to the app", func(t *testing.T) {
		sends := &database.App{
			Slug: "sends", Name: "Sends", OwnerID: "owner",
			RepoURL: "upload://sends.zip", Status: database.AppStatusRunning,
			Visibility: database.VisibilityPrivate,
		}
		if err := e.srv.db.CreateApp(sends); err != nil {
			t.Fatalf("create app: %v", err)
		}
		rec := bySlug("owner", "sends")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		var got struct {
			App struct {
				ID string `json:"id"`
			} `json:"app"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.App.ID != sends.ID {
			t.Errorf("resolved to %q, want %q", got.App.ID, sends.ID)
		}
	})

	t.Run("view=brief of an app the caller may not read is the same 404", func(t *testing.T) {
		rec := bySlug("owner", e.otherApp.Slug+"?view=brief")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
