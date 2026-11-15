package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/docs"
	"toft/internal/version"
)

func documentationFor(t *testing.T, e *galleryEnv, who string) documentationResponse {
	t.Helper()
	rec := e.do(t, who, http.MethodGet, "/api/v1/documentation", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET documentation as %s = %d: %s", who, rec.Code, rec.Body.String())
	}
	var body documentationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the documentation response: %v", err)
	}
	return body
}

// Every reader gets the same three links, to the folder for the version this
// binary is — the admin guide included, because it is public.
func TestDocumentationLinksEveryReaderToThisVersion(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryOff)
	e.srv.cfg.DocsURL = "https://docs.example/toft-docs"
	want := "https://docs.example/toft-docs/" + docs.Minor(version.Version) + "/"

	for _, who := range []string{"viewer", "publisher", "admin-user"} {
		got := documentationFor(t, e, who)
		if got.URL == nil || *got.URL != want {
			t.Errorf("%s: url = %v, want %s", who, got.URL, want)
		}
		if got.Version != docs.Minor(version.Version) {
			t.Errorf("%s: version = %q", who, got.Version)
		}
		ids := []string{}
		for _, s := range got.Sections {
			ids = append(ids, s.ID)
			if !strings.HasPrefix(s.URL, want) {
				t.Errorf("%s: section %s links %q, outside %s", who, s.ID, s.URL, want)
			}
		}
		if got.AgentSkills == nil || *got.AgentSkills != want+"cli/cli/agents/" {
			t.Errorf("%s: agent_skills_url = %v", who, got.AgentSkills)
		}
		if strings.Join(ids, ",") != "user,admin,cli" {
			t.Errorf("%s: sections = %v, want the three guides for everybody", who, ids)
		}
	}
}

// DOCS_URL=off is absent, not empty links: no url, no sections, and the
// version still said.
func TestDocumentationUnderDocsURLOffLinksNothing(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryOff)
	e.srv.cfg.DocsURL = ""
	got := documentationFor(t, e, "viewer")
	if got.URL != nil || len(got.Sections) != 0 || got.AgentSkills != nil {
		t.Errorf("under off: url = %v, sections = %+v", got.URL, got.Sections)
	}
	if got.Version == "" {
		t.Error("under off the version is still owed to the page")
	}
	rec := e.do(t, "viewer", http.MethodGet, "/api/v1/documentation", "")
	if !strings.Contains(rec.Body.String(), `"sections":[]`) {
		t.Errorf("sections should be an empty list, not null: %s", rec.Body.String())
	}
}
