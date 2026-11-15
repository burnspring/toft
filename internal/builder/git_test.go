package builder

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/gitconn"
)

func TestGitAuthEnvWithoutToken(t *testing.T) {
	env := GitAuthEnv("")
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatal("anonymous fetches must not hang on a credential prompt")
	}
	// The redirect ban is unconditional: an allowed host that answers 302
	// elsewhere would make the source allowlist check the URL that was typed
	// rather than the host that was fetched from.
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "http.followRedirects" ||
		env["GIT_CONFIG_VALUE_0"] != "false" {
		t.Fatalf("an anonymous fetch does not disable redirects: %v", env)
	}
}

func TestGitAuthEnvWithToken(t *testing.T) {
	const token = "github_pat_secret123"
	env := GitAuthEnv(token)

	// GIT_CONFIG_COUNT is the whole mechanism — a key past the count sets
	// nothing at all, silently — so the count and both keys are pinned.
	if env["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 2", env["GIT_CONFIG_COUNT"])
	}
	if env["GIT_CONFIG_KEY_0"] != "http.followRedirects" || env["GIT_CONFIG_VALUE_0"] != "false" {
		t.Fatalf("redirects are not disabled when a token is present: %v", env)
	}
	if env["GIT_CONFIG_KEY_1"] != "http.extraheader" {
		t.Fatalf("GIT_CONFIG_KEY_1 = %q", env["GIT_CONFIG_KEY_1"])
	}
	want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-token-auth:"+token))
	if env["GIT_CONFIG_VALUE_1"] != want {
		t.Fatalf("GIT_CONFIG_VALUE_1 = %q, want %q", env["GIT_CONFIG_VALUE_1"], want)
	}

	// The raw token must never appear in the clear anywhere in the env.
	for k, v := range env {
		if strings.Contains(v, token) {
			t.Fatalf("raw token leaked into %s", k)
		}
	}
}

// LsRemote must refuse hostile inputs before ever spawning git — same
// contract as the clone path.
func TestLsRemoteValidates(t *testing.T) {
	ctx := context.Background()
	if _, err := LsRemote(ctx, "ext::sh -c id", "main", ""); err == nil {
		t.Fatal("non-https URL accepted")
	}
	if _, err := LsRemote(ctx, "https://example.com/repo.git", "--upload-pack=id", ""); err == nil {
		t.Fatal("option-shaped branch accepted")
	}
}

// The classifications are what decide whether an app's owner is told to rotate
// a token, fix a URL, or do nothing at all — so they are pinned against the
// strings git actually emits rather than paraphrases of them.
func TestClassifyGitError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   GitFailure
	}{
		{"github expired token", "remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/acme/reports.git/'", GitFailureAuth},
		{"github password removed", "remote: Support for password authentication was removed on August 13, 2021.", GitFailureAuth},
		{"no credential at all", "fatal: could not read Username for 'https://github.com': terminal prompts disabled", GitFailureAuth},
		{"gitlab basic auth", "remote: HTTP Basic: Access denied\nfatal: Authentication failed", GitFailureAuth},
		{"http 401", "fatal: unable to access 'https://git.internal/x.git/': The requested URL returned error: 401", GitFailureAuth},
		{"http 403", "fatal: unable to access 'https://git.internal/x.git/': The requested URL returned error: 403", GitFailureAuth},

		{"repo gone", "remote: Repository not found.\nfatal: repository 'https://github.com/acme/gone.git/' not found", GitFailureNotFound},
		{"http 404", "fatal: unable to access 'https://git.internal/x.git/': The requested URL returned error: 404", GitFailureNotFound},

		{"branch gone", "fatal: couldn't find remote ref refs/heads/main", GitFailureBranch},
		{"our own message", `branch "main" not found on remote`, GitFailureBranch},

		{"dns", "fatal: unable to access 'https://github.com/acme/x.git/': Could not resolve host: github.com", GitFailureNetwork},
		{"timeout", "fatal: unable to access 'https://github.com/x.git/': Failed to connect to github.com port 443: Connection timed out", GitFailureNetwork},
		{"tls", "fatal: unable to access 'https://git.internal/x.git/': SSL certificate problem: unable to get local issuer certificate", GitFailureNetwork},
		{"context deadline", "git ls-remote: context deadline exceeded", GitFailureNetwork},

		{"anything else", "fatal: the remote end hung up unexpectedly", GitFailureUnknown},
		{"empty", "", GitFailureUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyGitError(tc.stderr); got != tc.want {
				t.Fatalf("ClassifyGitError(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}

// Only the failures a person has to act on are errors; a network blip is a
// warning, because the next poll may well fix it.
func TestGitFailureNeedsAttention(t *testing.T) {
	for _, f := range []GitFailure{GitFailureAuth, GitFailureNotFound, GitFailureBranch} {
		if !f.NeedsAttention() {
			t.Errorf("%q should need attention", f)
		}
	}
	for _, f := range []GitFailure{GitFailureNetwork, GitFailureUnknown} {
		if f.NeedsAttention() {
			t.Errorf("%q should not need attention", f)
		}
	}
}

// The sentence an owner reads names the repository and the fix, not the
// command that failed.
func TestGitFailureExplain(t *testing.T) {
	repo := "https://github.com/acme/reports"
	pasted := gitconn.Credential{Source: gitconn.SourceAppToken}
	if got := GitFailureAuth.Explain(repo, "main", pasted); !strings.Contains(got, repo) || !strings.Contains(got, "settings") {
		t.Errorf("auth explanation does not name the repo and the fix: %q", got)
	}
	if got := GitFailureBranch.Explain(repo, "release", pasted); !strings.Contains(got, "release") {
		t.Errorf("branch explanation does not name the branch: %q", got)
	}
	if got := GitFailureUnknown.Explain(repo, "main", pasted); got == "" {
		t.Error("an unclassified failure still has to say something")
	}
}

// Test 9. Nobody is sent to a field they cannot fill in.
//
// The connection case is the new one; the no-credential case is a defect that
// predates connections entirely — an app that never had a token was told to
// update the token it does not have, by the same sentence.
func TestExplainNeverSendsSomeoneToAFieldTheyDoNotHave(t *testing.T) {
	repo := "https://github.com/acme/reports"

	for _, class := range []GitFailure{GitFailureAuth, GitFailureNotFound} {
		got := class.Explain(repo, "main", gitconn.Credential{
			Source: gitconn.SourceConnection, Connection: "Acme GitHub",
		})
		if strings.Contains(got, "app's settings") {
			t.Errorf("%s sends a connection-backed app to the app's settings: %q", class, got)
		}
		if !strings.Contains(got, "Acme GitHub") {
			t.Errorf("%s does not name the connection: %q", class, got)
		}

		got = class.Explain(repo, "main", gitconn.Credential{Source: gitconn.SourceNone})
		if strings.Contains(got, "update it in the app's settings") {
			t.Errorf("%s tells an app with no credential to update one: %q", class, got)
		}
		if !strings.Contains(got, repo) {
			t.Errorf("%s does not name the repository: %q", class, got)
		}
	}
}

// The operator's proxy and CA configuration has to reach git, which is the one
// consumer of GIT_SSL_CAINFO on the passthrough list and reached nothing until
// gitEnv existed.
func TestGitEnvCarriesToolchainPassthrough(t *testing.T) {
	t.Setenv("GIT_SSL_CAINFO", "/etc/ssl/corp.pem")
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")

	env := gitEnv("secret-token")

	if env["GIT_SSL_CAINFO"] != "/etc/ssl/corp.pem" {
		t.Errorf("GIT_SSL_CAINFO = %q, want the operator's value", env["GIT_SSL_CAINFO"])
	}
	if env["HTTPS_PROXY"] != "http://proxy.internal:3128" {
		t.Errorf("HTTPS_PROXY = %q, want the operator's value", env["HTTPS_PROXY"])
	}
	// The credential still arrives, and still only through GIT_CONFIG_*.
	if env["GIT_CONFIG_COUNT"] != "2" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 2", env["GIT_CONFIG_COUNT"])
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-token-auth:secret-token"))
	seen := 0
	for k, v := range env {
		if strings.Contains(v, basic) {
			seen++
			if !strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
				t.Errorf("credential reached %s, which is not git's config channel", k)
			}
		}
	}
	if seen != 1 {
		t.Errorf("credential appears in %d variables, want exactly 1", seen)
	}
}

// A folder's tree id moves with a commit inside the folder and with nothing
// else: it is what lets the poller skip a push to another app's folder.
func TestCloneSubdirTreeMovesOnlyWithTheFolder(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(subdir string) (string, string) {
		t.Helper()
		sha, tree, err := cloneSubdirTree(context.Background(), dir, subdir)
		if err != nil {
			t.Fatal(err)
		}
		return sha, tree
	}

	git("init", "-q", "-b", "main")
	write("apps/one/app.py", "one")
	write("apps/two/app.py", "two")
	git("add", ".")
	git("commit", "-qm", "first")
	sha1, tree1 := read("apps/one")
	if tree1 == "" {
		t.Fatal("apps/one has no tree at the first commit")
	}

	write("apps/two/app.py", "two, changed")
	git("commit", "-qam", "elsewhere")
	sha2, tree2 := read("apps/one/")
	if sha2 == sha1 {
		t.Fatal("the commit did not move")
	}
	if tree2 != tree1 {
		t.Errorf("a commit outside apps/one moved its tree: %s -> %s", tree1, tree2)
	}

	write("apps/one/app.py", "one, changed")
	git("commit", "-qam", "inside")
	if _, tree3 := read("apps/one"); tree3 == tree1 {
		t.Error("a commit inside apps/one left its tree where it was")
	}
	if _, absent := read("apps/three"); absent != "" {
		t.Errorf("an absent folder has tree %q, want none", absent)
	}
}
