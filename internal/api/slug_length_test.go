package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/builder"
	"toft/internal/database"
)

// The limit is what keeps a release's workload name a DNS label: at the
// longest slug the name is exactly 63, and the deployment suffix survives.
func TestTheLongestSlugsReleaseNameFitsAnObjectName(t *testing.T) {
	slug := strings.Repeat("a", maxSlugLen)
	name := builder.ReleaseProcessName(slug, "1234abcd-0000-0000-0000-000000000000")
	if len(name) != 63 {
		t.Fatalf("release name %q is %d characters, want 63", name, len(name))
	}
	if !strings.HasSuffix(name, "-1234abcd") {
		t.Errorf("the deployment suffix was lost: %q", name)
	}
}

// A stated slug over the limit is refused by name, with the figure.
func TestALongStatedSlugIsRefused(t *testing.T) {
	w := httptest.NewRecorder()
	long := strings.Repeat("x", maxSlugLen+1)
	if !rejectLongSlug(w, long) {
		t.Fatal("a slug over the limit was accepted")
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusBadRequest || body["code"] != "slug_too_long" ||
		!strings.Contains(body["error"], "at most 54 characters") {
		t.Errorf("refusal = %d %v", w.Code, body)
	}
	if rejectLongSlug(httptest.NewRecorder(), long[:maxSlugLen]) {
		t.Error("a slug at the limit was refused")
	}
}

// A slug derived from a long name is cut to fit, never with a trailing dash.
func TestADerivedSlugIsCutToFit(t *testing.T) {
	name := strings.Repeat("ab-", 30) // 90 characters, a dash at 54
	got := defaultSlug(name, "")
	if len(got) > maxSlugLen || strings.HasSuffix(got, "-") {
		t.Errorf("defaultSlug = %q (%d)", got, len(got))
	}
	if got := defaultSlug("", "https://github.com/o/"+strings.Repeat("r", 80)+".git"); len(got) != maxSlugLen {
		t.Errorf("from a source: %q (%d)", got, len(got))
	}
}

// The gallery's deploy route treats its slug as a proposal, the body's or the
// catalog's, and cuts it to fit rather than creating an app over the limit.
func TestAGallerySlugIsCutToFit(t *testing.T) {
	e := newGalleryEnv(t, database.GalleryAll)
	rec := e.do(t, "publisher", http.MethodPost, "/api/v1/gallery/ex-1/deploy",
		`{"slug":"`+strings.Repeat("g", 60)+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		App database.App `json:"app"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.App.Slug != strings.Repeat("g", maxSlugLen) {
		t.Errorf("slug = %q (%d), want %d characters", body.App.Slug, len(body.App.Slug), maxSlugLen)
	}
}
