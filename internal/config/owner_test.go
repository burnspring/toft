package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryCopyOfTheOwnerAgrees holds GitHubOwner equal to the copies that
// cannot import it: the client's (a separate module), the publish scripts'
// scripts/lib/owner.sh, and the two installers, which are fetched alone by a
// shell pipeline and so must spell it out. A mismatch is invisible until a
// customer's install or upgrade command fetches from the wrong organization.
func TestEveryCopyOfTheOwnerAgrees(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if os.IsNotExist(err) && rel != "cli/install_version.go" && rel != "deploy/cli/install.sh" {
			return "" // a private script, absent from the public source snapshot
		}
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return string(b)
	}
	for rel, re := range map[string]*regexp.Regexp{
		"cli/install_version.go": regexp.MustCompile(`(?m)^const githubOwner = "([^"]+)"`),
		"scripts/lib/owner.sh":   regexp.MustCompile(`(?m)^GITHUB_OWNER=(\S+)$`),
		"deploy/cli/install.sh":  regexp.MustCompile(`(?m)^REPO_DEFAULT=([^/\s]+)/`),
		"deploy/compose/install.sh": regexp.MustCompile(
			`(?m)^RAW_BASE_DEFAULT=https://raw\.githubusercontent\.com/([^/\s]+)/`),
	} {
		body := read(rel)
		if body == "" {
			continue
		}
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s no longer names the owner where this test looks", rel)
			continue
		}
		if m[1] != GitHubOwner {
			t.Errorf("%s names %q; config.GitHubOwner is %q", rel, m[1], GitHubOwner)
		}
	}
	if !strings.Contains(DefaultGalleryCatalogURL, "/"+GitHubOwner+"/") {
		t.Errorf("DefaultGalleryCatalogURL %q is not under %s", DefaultGalleryCatalogURL, GitHubOwner)
	}
	if DefaultDocsURL != "https://docs."+ProductDomain {
		t.Errorf("DefaultDocsURL %q is not the docs subdomain of %s", DefaultDocsURL, ProductDomain)
	}
}
