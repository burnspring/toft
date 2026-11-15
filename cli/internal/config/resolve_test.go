package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The resolution order, stated once here so it cannot drift from the sentence
// in dev-reference.md.
//
// Every layer is populated at once and then removed from the top down, which is
// the only shape of test that catches the failure that matters: not "does the
// flag work" but "does each layer actually yield to the one above it".
func TestResolutionOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ManifestFile), `
[app]
slug = "from-project"

[app.installs."https://prod.example.com"]
slug = "from-per-install"
`)
	write(t, filepath.Join(dir, LocalManifestFile), `
[app]
slug = "from-local"
`)

	const install = "https://prod.example.com"
	cases := []struct {
		name  string
		flags map[Key]string
		env   map[string]string
		files []string // files to remove before resolving
		want  string
	}{
		{
			name:  "the flag beats everything",
			flags: map[Key]string{KeyApp: "from-flag"},
			env:   map[string]string{"TOFT_APP": "from-env"},
			want:  "from-flag",
		},
		{
			name: "the environment beats every file",
			env:  map[string]string{"TOFT_APP": "from-env"},
			want: "from-env",
		},
		{
			name: "the uncommitted override beats the committed manifest",
			want: "from-local",
		},
		{
			// The case the per-install table exists for: a checkout that
			// deploys to two installs, where neither entry is wrong.
			name:  "the per-install table beats the plain one",
			files: []string{LocalManifestFile},
			want:  "from-per-install",
		},
		{
			name:  "and the plain table is the last thing in the directory",
			files: []string{LocalManifestFile},
			want:  "from-project",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The last case asks what happens with no per-install entry, so it
			// rewrites the manifest without one.
			if tc.want == "from-project" {
				write(t, filepath.Join(dir, ManifestFile), "[app]\nslug = \"from-project\"\n")
				defer write(t, filepath.Join(dir, ManifestFile), `
[app]
slug = "from-project"

[app.installs."https://prod.example.com"]
slug = "from-per-install"
`)
			}
			for _, f := range tc.files {
				os.Remove(filepath.Join(dir, f))
				defer write(t, filepath.Join(dir, LocalManifestFile), "[app]\nslug = \"from-local\"\n")
			}

			r, err := NewResolver(dir, tc.flags)
			if err != nil {
				t.Fatalf("NewResolver: %v", err)
			}
			r.lookupEnv = func(k string) string { return tc.env[k] }
			// The user config file is the layer below all of these and is not
			// under test here; a real one in the developer's home directory
			// must not decide the answer.
			r.user = nil

			if got := r.App(install); got.Value != tc.want {
				t.Errorf("App() = %q (from %s), want %q", got.Value, got.Origin, tc.want)
			}
		})
	}
}

// The install resolves before the app, because the per-install table is keyed by
// the install's URL — a repository that deploys to two Tofts cannot say
// which app it means until it knows which one it is talking to.
func TestPerInstallSlugFollowsTheInstall(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ManifestFile), `
[app]
slug = "phase-test"

[app.installs."https://staging.example.com"]
slug = "phase-test-stg"
`)
	r, err := NewResolver(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.lookupEnv = func(string) string { return "" }
	r.user = nil

	if got := r.App("https://staging.example.com"); got.Value != "phase-test-stg" {
		t.Errorf("staging resolved to %q, want phase-test-stg", got.Value)
	}
	// An install with no entry of its own falls back to the plain table rather
	// than to nothing: the common case is one app deployed to one place.
	if got := r.App("https://prod.example.com"); got.Value != "phase-test" {
		t.Errorf("prod resolved to %q, want phase-test", got.Value)
	}
}

// The same install is written four ways by four people, and every one of those
// spellings is used as a map key — in the credentials file, in
// [app.installs], and in the comparison between the two.
func TestInstallURLNormalization(t *testing.T) {
	want := "https://toft.corp"
	for _, in := range []string{
		"https://toft.corp",
		"https://toft.corp/",
		"https://TOFT.CORP",
		"  https://toft.corp//  ",
		// The scheme is supplied here and nowhere else, which is what makes a
		// bare host and a written-out one one key rather than two entries for
		// one install.
		"toft.corp",
		"TOFT.CORP/",
		"  toft.corp  ",
	} {
		if got := NormalizeInstallURL(in); got != want {
			t.Errorf("NormalizeInstallURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The host is case-insensitive by RFC and the path is not.
//
// Lowercasing the whole string — which this used to do — normalized an install
// under a base path to a key that did not match itself, and that key is what
// the credentials file and [app.installs."<url>"] are both looked up by.
func TestNormalizeInstallURLSuppliesSchemeAndKeepsPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://TOFT.CORP/Team/Reports", "https://toft.corp/Team/Reports"},
		{"TOFT.CORP/Team", "https://toft.corp/Team"},
		{"HTTP://localhost:8080", "http://localhost:8080"},
		// http and https stay distinct keys, so a token minted over TLS is
		// never handed to the plaintext spelling of the same host.
		{"http://toft.corp", "http://toft.corp"},
		{"localhost:8080", "https://localhost:8080"},
		{"localhost", "https://localhost"},
		// Not an address. Turning an alias into a host would convert "that
		// alias names nothing" into a request to a machine called prod.
		{"prod", "prod"},
		{"Staging", "staging"},
		// Empty stays empty: callers compare against "" to mean unset.
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := NormalizeInstallURL(tc.in); got != tc.want {
			t.Errorf("NormalizeInstallURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The property the map keys actually depend on: two spellings of one install
// find one entry.
func TestABareHostFindsTheStoredCredential(t *testing.T) {
	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://toft.corp", Token: "toftu_secret"})

	for _, spelling := range []string{"toft.corp", "https://toft.corp", "TOFT.CORP/"} {
		if _, ok, _ := f.Lookup(spelling); !ok {
			t.Errorf("Lookup(%q) found nothing — it is the same install", spelling)
		}
	}
}

// A directory with none of these files is the normal case, not an error: `toft`
// runs in plenty of them.
func TestNoFilesIsNotAnError(t *testing.T) {
	r, err := NewResolver(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("an empty directory should resolve fine: %v", err)
	}
	r.lookupEnv = func(string) string { return "" }
	r.user = nil
	if got := r.App(""); got.Found() {
		t.Errorf("App() = %q from nothing", got.Value)
	}
}

// A file somebody has written and the CLI silently ignores is worse than a
// refusal, because the refusal names the line.
func TestAMalformedManifestIsReported(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ManifestFile), "[app\nslug = ")
	if _, err := NewResolver(dir, nil); err == nil {
		t.Error("a manifest that cannot be parsed was ignored")
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
