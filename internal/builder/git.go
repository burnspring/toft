package builder

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"toft/internal/gitconn"
	"toft/internal/runtime"
	"toft/internal/toolchain"
)

// GitAuthEnv returns environment variables that make git authenticate an
// HTTPS remote with `token`, keeping the credential out of argv (visible in
// /proc), out of the URL (persisted in .git/config of the clone), and off
// disk. GIT_CONFIG_{COUNT,KEY_n,VALUE_n} is git's env-only config channel.
//
// The basic-auth username is "x-token-auth": GitHub and GitLab ignore the
// username when the password is a token, and Bitbucket requires exactly this
// one — so a single form covers all three.
//
// An empty token yields GIT_TERMINAL_PROMPT=0 and the redirect ban below, so an
// anonymous fetch of a private repo fails fast instead of waiting on a prompt
// nobody will answer.
//
// **Redirects are disabled here, in every source-policy mode.** An allowed host
// can answer 302 to anywhere and git follows it, so the allowlist would be
// checking the URL that was typed rather than the host that was fetched from —
// a bypass that leaves no trace in any record the platform keeps. It is applied
// even under `any` because a redirect-following clone is a hazard regardless of
// policy; what is being given up is an internal mirror that answers on a
// redirecting front door, which must have its real host configured instead.
//
// Note that GIT_CONFIG_COUNT is the whole mechanism: adding a key without
// renumbering the count sets nothing at all, silently. Both keys are written
// from one slice for that reason.
func GitAuthEnv(token string) map[string]string {
	cfg := [][2]string{{"http.followRedirects", "false"}}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-token-auth:" + token))
		cfg = append(cfg, [2]string{"http.extraheader", "Authorization: Basic " + basic})
	}

	env := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_COUNT":    strconv.Itoa(len(cfg)),
	}
	for i, kv := range cfg {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = kv[0]
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = kv[1]
	}
	return env
}

// gitEnv is the whole environment a git subprocess gets beyond runtime.BaseEnv:
// the operator's toolchain and proxy variables, then the credential.
//
// The passthrough list carries GIT_SSL_CAINFO for exactly this call and, until
// this existed, reached nothing — so behind a TLS-intercepting proxy the build
// would fetch npm packages happily and fail at the clone, which is the first
// step. HTTP_PROXY and NO_PROXY are the same story. The credential is merged
// last and nothing on the operator's list collides with it, so the order is a
// statement rather than a tie-break: an operator configures the route, and
// only the platform supplies the secret.
func gitEnv(token string) map[string]string {
	env := toolchain.Passthrough()
	for k, v := range GitAuthEnv(token) {
		env[k] = v
	}
	return env
}

// LsRemote returns the commit SHA at the tip of branch on the remote without
// cloning. An empty token queries anonymously.
func LsRemote(ctx context.Context, repoURL, branch, token string) (string, error) {
	if err := ValidateRepoURL(repoURL); err != nil {
		return "", err
	}
	if err := ValidateBranch(branch); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--", repoURL, "refs/heads/"+branch)
	cmd.Env = append(runtime.BaseEnv(), envSlice(gitEnv(token))...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git ls-remote: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git ls-remote: %w", err)
	}

	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("branch %q not found on remote", branch)
	}
	return fields[0], nil
}

// GitFailure classifies why a git command against a remote did not work, from
// git's own words.
//
// It exists because the three interesting cases are indistinguishable to the
// app's owner and completely different to fix: a credential that no longer
// works needs a new token, a repository that moved needs a new URL, and a
// network blip needs nothing at all. Reporting all three as "git failed" is
// what the platform did before, and it is why an expired token could go
// unnoticed for months.
type GitFailure string

const (
	// GitFailureAuth is the headline case: the credential is missing, expired,
	// revoked, or was never accepted. GitHub's "password authentication was
	// removed" and a terminal prompt that could not be answered land here too —
	// both mean the same thing to the person who has to fix it.
	GitFailureAuth GitFailure = "auth"
	// GitFailureNotFound is a repository the remote will not admit exists. It
	// is deliberately separate from auth even though a private repo returns it
	// for the wrong credential: the fix is usually the URL.
	GitFailureNotFound GitFailure = "notfound"
	// GitFailureBranch is a remote that answered fine and has no such ref.
	GitFailureBranch GitFailure = "branch"
	// GitFailureNetwork is DNS, a timeout, a refused connection, a proxy. The
	// next poll may well succeed, which is why it is a warning and not an error.
	GitFailureNetwork GitFailure = "network"
	// GitFailureUnknown is everything else — reported as-is rather than guessed
	// at, because a wrong diagnosis sends someone to rotate a working token.
	GitFailureUnknown GitFailure = "unknown"
)

// gitFailurePatterns maps substrings of git's stderr onto a classification.
// Ordered: the first match wins, so the more specific patterns come first.
var gitFailurePatterns = []struct {
	needle string
	class  GitFailure
}{
	// Auth. "could not read Username" is what GIT_TERMINAL_PROMPT=0 produces
	// when a private repo is fetched with no credential at all.
	{"authentication failed", GitFailureAuth},
	{"invalid username or password", GitFailureAuth},
	{"could not read username", GitFailureAuth},
	{"could not read password", GitFailureAuth},
	{"terminal prompts disabled", GitFailureAuth},
	{"support for password authentication was removed", GitFailureAuth},
	{"http basic: access denied", GitFailureAuth},
	{"permission denied", GitFailureAuth},
	{"the requested url returned error: 401", GitFailureAuth},
	{"the requested url returned error: 403", GitFailureAuth},
	{"error: 401", GitFailureAuth},
	{"error: 403", GitFailureAuth},

	// Branch. Ahead of the missing-repository group on purpose: "not found on
	// remote" is our own wording for a missing ref and would otherwise be
	// swallowed by the bare "not found" below.
	{"couldn't find remote ref", GitFailureBranch},
	{"not found on remote", GitFailureBranch},
	{"unknown revision", GitFailureBranch},

	// Missing. A private repo answers 404 to an unauthorized caller, so this
	// reads as "not found" and the copy says so without claiming to know which.
	{"repository not found", GitFailureNotFound},
	{"the requested url returned error: 404", GitFailureNotFound},
	{"does not appear to be a git repository", GitFailureNotFound},
	{"not found", GitFailureNotFound},

	// Network.
	{"could not resolve host", GitFailureNetwork},
	{"connection timed out", GitFailureNetwork},
	{"connection refused", GitFailureNetwork},
	{"connection reset", GitFailureNetwork},
	{"failed to connect", GitFailureNetwork},
	{"operation timed out", GitFailureNetwork},
	{"network is unreachable", GitFailureNetwork},
	{"ssl certificate problem", GitFailureNetwork},
	{"gnutls_handshake() failed", GitFailureNetwork},
	{"empty reply from server", GitFailureNetwork},
	{"context deadline exceeded", GitFailureNetwork},
	{"signal: killed", GitFailureNetwork},
}

// ClassifyGitError reads git's stderr and says what kind of problem it is.
func ClassifyGitError(stderr string) GitFailure {
	s := strings.ToLower(stderr)
	for _, p := range gitFailurePatterns {
		if strings.Contains(s, p.needle) {
			return p.class
		}
	}
	return GitFailureUnknown
}

// NeedsAttention reports whether a failure will keep happening until a person
// changes something. Network trouble does not qualify — the next poll may fix
// it — and that distinction is what decides error versus warning.
func (f GitFailure) NeedsAttention() bool {
	return f == GitFailureAuth || f == GitFailureNotFound || f == GitFailureBranch
}

// Explain turns a classification into the sentence an app's owner is told,
// naming the repository and the fix rather than the command that failed.
//
// **It takes the credential that was used, because two of these five sentences
// cannot be written without it.** "Update it in the app's settings" is the
// right instruction for a token the owner pasted, a wrong one for an app that
// has no credential at all — a case that existed before connections did and was
// answered with the same sentence — and a harmful one for a connection, whose
// field is empty on the app's settings page and whose fix belongs to an
// administrator on a page the owner may not be able to see.
//
// **No error value reaches here.** It classifies and it explains; it never
// carries a host's response body, which is what would put a token in a
// sentence.
func (f GitFailure) Explain(repoURL, branch string, cred gitconn.Credential) string {
	switch f {
	case GitFailureAuth:
		switch cred.Source {
		case gitconn.SourceConnection:
			return fmt.Sprintf("%s could not be cloned with the %s connection — an administrator "+
				"needs to check it in Admin → Sources.", repoURL, cred.Connection)
		case gitconn.SourceNone:
			// A host answers a missing public repository and a private one
			// asked anonymously alike — GitHub with a credential prompt — so
			// the sentence names both rather than asserting the one it cannot
			// tell from the other.
			return fmt.Sprintf("%s was not found, or it is private and no credential is set for it.", repoURL)
		default:
			return fmt.Sprintf("The access token for %s is no longer accepted — update it in the app's settings.", repoURL)
		}
	case GitFailureNotFound:
		switch cred.Source {
		case gitconn.SourceConnection:
			return fmt.Sprintf("%s could not be found — check the repository URL, and that the %s "+
				"connection covers it.", repoURL, cred.Connection)
		case gitconn.SourceNone:
			return fmt.Sprintf("%s could not be found — check the repository URL. This app has no "+
				"credential, so a private repository would look the same.", repoURL)
		default:
			return fmt.Sprintf("%s could not be found — check the repository URL, and that the access token can still see it.", repoURL)
		}
	case GitFailureBranch:
		return fmt.Sprintf("The branch %q no longer exists on %s.", branch, repoURL)
	case GitFailureNetwork:
		return fmt.Sprintf("%s could not be reached. Toft will keep trying.", repoURL)
	default:
		return fmt.Sprintf("Checking %s for new commits failed.", repoURL)
	}
}

// SubdirExists asks whether a folder exists at the tip of branch, without
// fetching a single file. It is what lets POST /apps refuse a missing
// `--subdir` before claiming a slug, as it refuses a missing branch — the
// alternative was an app row in `failed` holding its name.
func SubdirExists(ctx context.Context, repoURL, branch, token, subdir string) (bool, error) {
	_, tree, err := SubdirTree(ctx, repoURL, branch, token, subdir)
	return tree != "", err
}

// SubdirTree is the tip of branch and the git tree id of subdir there, "" when
// the folder is absent, read with a one-commit, blob-less clone with nothing
// checked out — only the trees are fetched. The auto-deploy poller compares the
// tree with the one the app's last build cloned (ADR §112): equal means a push
// changed nothing the app is made of.
func SubdirTree(ctx context.Context, repoURL, branch, token, subdir string) (sha, tree string, err error) {
	if err := ValidateRepoURL(repoURL); err != nil {
		return "", "", err
	}
	if err := ValidateBranch(branch); err != nil {
		return "", "", err
	}
	dir, err := os.MkdirTemp("", "toft-subdir-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)

	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--filter=blob:none",
		"--no-checkout", "--single-branch", "--branch", branch, "--", repoURL, dir)
	clone.Env = append(runtime.BaseEnv(), envSlice(gitEnv(token))...)
	if out, err := clone.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git clone: %s", strings.TrimSpace(string(out)))
	}
	return cloneSubdirTree(ctx, dir, subdir)
}

// cloneSubdirTree is a clone's HEAD and subdir's tree id at it, "" when the
// folder is absent there.
func cloneSubdirTree(ctx context.Context, dir, subdir string) (sha, tree string, err error) {
	rev := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	rev.Env = runtime.BaseEnv()
	out, err := rev.Output()
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse: %w", err)
	}
	sha = strings.TrimSpace(string(out))
	ls := exec.CommandContext(ctx, "git", "-C", dir, "ls-tree", "-d", "HEAD", "--", strings.Trim(subdir, "/"))
	ls.Env = runtime.BaseEnv()
	if out, err = ls.Output(); err != nil {
		return "", "", fmt.Errorf("git ls-tree: %w", err)
	}
	// "040000 tree <id>\t<path>"
	if f := strings.Fields(string(out)); len(f) >= 3 && f[1] == "tree" {
		tree = f[2]
	}
	return sha, tree, nil
}
