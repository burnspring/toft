package builder

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRepoURL(t *testing.T) {
	valid := []string{
		"https://github.com/acme/tool",
		"https://github.example.corp/team/app.git",
	}
	for _, u := range valid {
		if err := ValidateRepoURL(u); err != nil {
			t.Errorf("ValidateRepoURL(%q) = %v, want nil", u, err)
		}
	}

	invalid := []struct {
		name string
		url  string
	}{
		// git parses a leading dash as an option; --upload-pack executes a command.
		{"option injection", "--upload-pack=touch /tmp/pwned"},
		{"ext transport runs commands", "ext::sh -c 'touch /tmp/pwned'"},
		{"file transport", "file:///etc/passwd"},
		{"local path", "/etc/passwd"},
		{"ssh", "git@github.com:acme/tool.git"},
		{"plain http", "http://github.com/acme/tool"},
		{"embedded credentials", "https://user:token@github.com/acme/tool"},
		{"newline injection", "https://github.com/acme/tool\n--upload-pack=x"},
		{"empty", ""},
		{"whitespace", "   "},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateRepoURL(tc.url); err == nil {
				t.Errorf("ValidateRepoURL(%q) = nil, want error", tc.url)
			}
		})
	}
}

func TestValidateBranch(t *testing.T) {
	for _, b := range []string{"main", "release/2.0", "feature-x", "v1.2.3"} {
		if err := ValidateBranch(b); err != nil {
			t.Errorf("ValidateBranch(%q) = %v, want nil", b, err)
		}
	}

	invalid := []struct{ name, branch string }{
		{"option injection", "--upload-pack=x"},
		{"command substitution", "main;touch /tmp/x"},
		{"path traversal", "../../etc"},
		{"space", "my branch"},
		{"empty", ""},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBranch(tc.branch); err == nil {
				t.Errorf("ValidateBranch(%q) = nil, want error", tc.branch)
			}
		})
	}
}

func TestReleaseProcessNameIsStableAndUnique(t *testing.T) {
	a := ReleaseProcessName("my-app", "3f2b1c4d-aaaa-bbbb-cccc-ddddeeeeffff")
	b := ReleaseProcessName("my-app", "9a8b7c6d-aaaa-bbbb-cccc-ddddeeeeffff")

	if a == b {
		t.Error("different deployments must get different process names")
	}
	if a != ReleaseProcessName("my-app", "3f2b1c4d-aaaa-bbbb-cccc-ddddeeeeffff") {
		t.Error("process name must be deterministic")
	}
	if got, want := a, "my-app-3f2b1c4d"; got != want {
		t.Errorf("process name = %q, want %q", got, want)
	}
}

func TestKeepsPrefix(t *testing.T) {
	// Frameworks told their mount point at build/start time receive the full
	// path; the rest are mounted at / and the proxy strips the prefix.
	keeps := map[string]bool{
		"nextjs":    true,
		"streamlit": true,
		"flask":     false,
		"fastapi":   false,
		"python":    false,
		"node":      false,
		"":          false,
	}
	for framework, want := range keeps {
		if got := KeepsPrefix(framework); got != want {
			t.Errorf("KeepsPrefix(%q) = %v, want %v", framework, got, want)
		}
	}
}

func TestBasePath(t *testing.T) {
	if got, want := BasePath("my-app"), "/apps/my-app"; got != want {
		t.Errorf("BasePath = %q, want %q", got, want)
	}
}

// APPS_DIR itself is healed: on a fresh volume it was found 0700 root, every
// app account was refused its own tree, and EnsureAppDirs fixed the two levels
// below and never this one (trytoft after an instance refresh, 2026-10-23).
func TestEnsureAppDirsHealsAppsDirItself(t *testing.T) {
	appsDir := filepath.Join(t.TempDir(), "apps")
	if err := os.Mkdir(appsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAppDirs(appsDir, "app1"); err != nil {
		t.Fatalf("EnsureAppDirs: %v", err)
	}
	info, err := os.Stat(appsDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o001 == 0 || perm&0o004 != 0 {
		t.Errorf("APPS_DIR mode = %o, want searchable by every account and not listable", perm)
	}
}

func TestEnsureAppDirs(t *testing.T) {
	appsDir := t.TempDir()

	// Fresh tree: both levels created traversable.
	if err := EnsureAppDirs(appsDir, "app1"); err != nil {
		t.Fatalf("EnsureAppDirs: %v", err)
	}
	for _, dir := range []string{
		AppDir(appsDir, "app1"),
		filepath.Join(appsDir, "app1", "releases"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0711 {
			t.Errorf("%s mode = %o, want 0711 (app accounts must traverse to their release)", dir, perm)
		}
	}

	// A tree created before the invariant existed (0700 parents, which make
	// every spawn under an app account fail with EACCES) is healed.
	locked := filepath.Join(appsDir, "app2", "releases")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAppDirs(appsDir, "app2"); err != nil {
		t.Fatalf("EnsureAppDirs on existing tree: %v", err)
	}
	info, err := os.Stat(locked)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0711 {
		t.Errorf("existing releases dir mode = %o, want 0711", perm)
	}
}

// A release name is matched to its deployment by the id it carries, so a
// rename that rebuilt nothing still finds the serving row.
func TestReleaseNameIsDeploymentSurvivesARename(t *testing.T) {
	id := "3f2b1c4d-aaaa-bbbb-cccc-ddddeeeeffff"
	name := ReleaseProcessName("old-slug", id)
	if !ReleaseNameIsDeployment(name, id) {
		t.Errorf("%q not matched to %s", name, id)
	}
	if ReleaseNameIsDeployment(name, "9a8b7c6d-aaaa-bbbb-cccc-ddddeeeeffff") {
		t.Error("matched another deployment")
	}
	if ReleaseNameIsDeployment("", id) || ReleaseNameIsDeployment("-3f2b1c4d", id) {
		t.Error("an empty or slugless name matched")
	}
}
