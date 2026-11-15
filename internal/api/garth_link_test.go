package api

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
)

func TestGarthLink(t *testing.T) {
	app := &database.App{GarthLinkedAt: linked(), ID: "7d3c6a0e-1111-4222-8333-944445555666", Slug: "sales-dash", RepoURL: "https://x-access-token:ghp_secret@github.com/acme/mono.git", RepoSubdir: "apps/sales dash"}
	got := garthLink("https://garth.acme.com/", "https://toft.acme.com", app)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://garth.acme.com/open/toft" {
		t.Fatalf("link base = %q", got)
	}
	q := u.Query()
	want := map[string]string{"toft": "https://toft.acme.com", "app": "sales-dash", "app_id": "7d3c6a0e-1111-4222-8333-944445555666",
		"repo": "https://github.com/acme/mono.git", "subdir": "apps/sales dash"}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("unexpected parameters: %v", q)
	}
	for _, leak := range []string{"ghp_secret", "x-access-token"} {
		if strings.Contains(got, leak) {
			t.Fatalf("link carries the repository's userinfo: %q", got)
		}
	}
}

func TestGarthLinkWithoutRepoOmitsRepoAndSubdir(t *testing.T) {
	got := garthLink("https://garth.acme.com", "https://toft.acme.com", &database.App{GarthLinkedAt: linked(), Slug: "upload", RepoSubdir: "x"})
	u, _ := url.Parse(got)
	if q := u.Query(); q.Has("repo") || q.Has("subdir") || q.Get("app") != "upload" {
		t.Fatalf("link = %q", got)
	}
}

func TestGarthLinkOffWhenUnset(t *testing.T) {
	if got := garthLink("", "https://toft.acme.com", &database.App{GarthLinkedAt: linked(), Slug: "a"}); got != "" {
		t.Fatalf("link with no Garth address = %q", got)
	}
}

// An app no Garth session deployed, or one somebody unlinked, offers no link
// even with Garth's address set.
func TestGarthLinkOffForAnAppNotDeployedFromGarth(t *testing.T) {
	if got := garthLink("https://garth.acme.com", "https://toft.acme.com", &database.App{Slug: "a", RepoURL: "https://github.com/acme/a"}); got != "" {
		t.Fatalf("link for an app never deployed from Garth = %q", got)
	}
}

func linked() *time.Time { t := time.Now(); return &t }
