package gitconn

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"toft/internal/database"
)

// Testing a connection: one authenticated call to the host, on demand, so an
// administrator finds out that a credential is wrong *now* rather than on
// somebody else's deploy at 3am.
//
// **It tests what is stored, never what is typed.** That is the rule the email
// test button already follows, and it is the same argument: what an
// administrator needs to know is whether the configuration this install will
// actually use works, and testing an unsaved form proves something about a
// form. A connection is created first and tested second.
//
// **A test is a diagnostic, not a configuration change.** It writes no
// connection field, mints nothing that outlives the call, and is deliberately
// not license-gated — a license gates configuration and never operation, and an
// install whose license has lapsed keeps deploying every app it already
// connected, so it must keep being able to find out why one stopped.
//
// What it deliberately does not do is check *coverage*. Coverage is this
// install's own boundary on what may be attached, enforced at the write; a host
// has no opinion about it and asking one would invent an authority that is not
// there.

// CheckResult is what one test learned. Every field beyond OK and Summary is
// what the host happened to say — absent rather than zero when it said nothing,
// because a metric a host does not report is not a count of none.
type CheckResult struct {
	// OK is whether the host accepted this install's credential.
	OK bool `json:"ok"`
	// Summary is the one sentence the page shows. On failure it is also what
	// is written to the connection's health, so the row says the same thing
	// the toast did.
	Summary string `json:"summary"`
	// Identity is who the host says the credential belongs to — a login, an
	// App's installation. Empty when the host was not asked or did not say.
	Identity string `json:"identity,omitempty"`
	// Repositories is how many the credential reaches, when the host counts
	// them for us. Only the App method has such a count; a token's reach is
	// whatever it was scoped to and no endpoint enumerates it cheaply.
	Repositories *int `json:"repositories,omitempty"`
	// Disabled repeats what the row already says, because a test that passes
	// against a connection nothing may use is a true answer to a question the
	// administrator did not ask.
	Disabled bool `json:"disabled"`
}

// Check makes one authenticated call to the connection's host and reports what
// happened, in a sentence an administrator can act on.
//
// **A refused credential is a result, not an error.** The returned error is
// reserved for the test being unable to run at all; everything the host said —
// including "no" — comes back as a CheckResult with OK false, because the
// caller records it as the connection's health either way and a transport
// failure and a 401 are the same kind of news to the person reading the page.
func (r *Resolver) Check(ctx context.Context, conn *database.GitConnection) CheckResult {
	if conn == nil {
		return CheckResult{Summary: "There is no such connection."}
	}
	res := CheckResult{Disabled: !conn.Enabled}

	switch {
	case conn.AuthMethod == database.GitAuthApp:
		r.checkApp(ctx, conn, &res)
	case conn.AuthMethod == database.GitAuthToken:
		r.checkToken(ctx, conn, &res)
	default:
		// The same refusal the clone path gives, for the same reason: a method
		// this build does not implement is named rather than guessed at.
		res.Summary = fmt.Sprintf("This connection authenticates in a way this install cannot use (%s).",
			conn.AuthMethod)
	}

	if res.OK && res.Disabled {
		// The credential works and nothing may use it. Both halves, because
		// either on its own misleads: "it works" hides that every deploy
		// through it fails, and "it is disabled" hides that re-enabling it is
		// all that is needed.
		res.Summary += " This connection is disabled, so nothing deploys through it until it is turned back on."
	}
	return res
}

// checkApp proves the whole App chain: the private key opens, it signs an
// assertion the host accepts, and the installation still exists. Minting is the
// test — it is exactly what a clone does, one step before git is spawned.
//
// Unnarrowed on purpose. A mint narrowed to a repository would also be
// answering "may this App see that repository", which is a question about one
// app rather than about the connection, and a test that failed because
// somebody's repository was archived would send an administrator to the wrong
// screen.
func (r *Resolver) checkApp(ctx context.Context, conn *database.GitConnection, res *CheckResult) {
	token, err := r.installationToken(ctx, conn, "")
	if err != nil {
		res.Summary = explainHostFailure(conn, err,
			"The App's private key, App id or installation id is wrong, or the App was uninstalled")
		return
	}
	res.OK = true
	res.Identity = "installation " + conn.InstallationID
	res.Summary = "Reached " + conn.BaseURL + " and minted an installation token."

	// The count is a second call and a nicety, so a host that answers the mint
	// and not this is still a pass — the credential works, and that is what was
	// asked.
	q := url.Values{}
	q.Set("per_page", "1")
	var body struct {
		TotalCount int `json:"total_count"`
	}
	if err := r.hostJSON(ctx, conn, apiBase(conn)+"/installation/repositories?"+q.Encode(), token, &body); err != nil {
		return
	}
	n := body.TotalCount
	res.Repositories = &n
	res.Summary = "Reached " + conn.BaseURL + " — the App is installed on " + countedRepos(n) + "."
}

// checkToken proves the stored token opens and the host accepts it.
//
// The endpoint is the host's own "who am I", which every kind of token this
// method accepts can call: a GitHub personal token classic or fine-grained, and
// a GitLab group or project access token, whose bot user is what answers. A
// GitLab host that refuses it is asked for its version instead — an
// authenticated route with no notion of a user, which is the one shape this
// would otherwise report as a bad credential when the credential is fine.
func (r *Resolver) checkToken(ctx context.Context, conn *database.GitConnection, res *CheckResult) {
	token, err := r.enc.DecryptContext(ctx, conn.EncryptedCloneSecret())
	if err != nil {
		// Not a host failure and must not read as one. This is the rotated
		// ENCRYPTION_KEY case, indistinguishable from a revoked token to
		// anybody reading a generic message — the distinction this whole
		// package exists to keep.
		res.Summary = "This install's keys cannot open the stored credential — re-enter the access token."
		return
	}

	var who struct {
		Login    string `json:"login"`    // GitHub
		Username string `json:"username"` // GitLab
	}
	err = r.hostJSON(ctx, conn, apiBase(conn)+"/user", token, &who)
	if err == nil {
		res.OK = true
		res.Identity = firstNonEmpty(who.Login, who.Username)
		res.Summary = "Reached " + conn.BaseURL + " and the access token was accepted"
		if res.Identity != "" {
			res.Summary += ", as " + res.Identity
		}
		res.Summary += "."
		return
	}

	if conn.Kind == database.GitConnGitLab {
		var version struct {
			Version string `json:"version"`
		}
		if verr := r.hostJSON(ctx, conn, apiBase(conn)+"/version", token, &version); verr == nil {
			res.OK = true
			res.Summary = "Reached " + conn.BaseURL + " and the access token was accepted."
			if version.Version != "" {
				res.Summary = "Reached GitLab " + version.Version + " at " + conn.BaseURL +
					" and the access token was accepted."
			}
			return
		}
	}

	res.Summary = explainHostFailure(conn, err,
		"The access token is wrong, expired, or has been revoked")
}

// explainHostFailure turns what the host did into a sentence naming the thing
// to go and fix.
//
// `refused` is the caller's guess at the cause of a 401 or 403, because those
// two are the only statuses where the credential is genuinely the likely
// answer and the two auth methods have different things to blame. Everything
// else is reported as what it is — the host's own status, never its body.
func explainHostFailure(conn *database.GitConnection, err error, refused string) string {
	var status *hostStatusError
	if errors.As(err, &status) {
		switch {
		case status.Code == 401 || status.Code == 403:
			return conn.DisplayName + " refused this install's credential (" + status.Status + "). " +
				refused + "."
		case status.Code == 404:
			return conn.DisplayName + " answered " + status.Status +
				". Check the host address — a Server install puts its API under the host itself, " +
				"and this install asked " + apiBase(conn) + "."
		case status.Code == 429:
			return conn.DisplayName + " is rate-limiting this install (" + status.Status +
				"). The credential may well be fine; try again shortly."
		default:
			return conn.DisplayName + " answered " + status.Status + "."
		}
	}
	// A transport failure, a refused address, a TLS problem, a signing key that
	// is not a key. The wrapped text is this install's own words in every one
	// of those cases — never a response body — so it is safe to show.
	return "Couldn't reach " + conn.BaseURL + ": " + trimError(err.Error()) + "."
}

// trimError takes the trailing full stop off a wrapped error so the sentence
// around it does not end in two.
func trimError(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ".")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func countedRepos(n int) string {
	if n == 1 {
		return "1 repository"
	}
	return strconv.Itoa(n) + " repositories"
}
