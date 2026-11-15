package gitconn

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"toft/internal/database"
)

// Coverage: what a connection may be pointed at.
//
// **One rule, and it is an administrator's declaration rather than a per-person
// query.** Any developer who can create an app may point it at a connection —
// no role gates that, and gating it would take the repository choice away from
// the person who knows which repository they want. What makes it safe is that
// the repository has to be one the connection covers.
//
// The alternative considered was checking the repository against the *person*,
// through their own credential against the host: *you may attach an app to a
// repository you can see*, which is the better sentence. It works only where a
// picker exists, so it would hold on one of the three kind-and-method
// combinations and leave every token-backed connection with no boundary at all,
// and it would make the picker load-bearing. Coverage holds identically on all
// three.
//
// Within the covered set any developer may attach any repository, and that is
// the intent rather than a gap. The boundary is one an administrator drew — on
// GitHub by choosing what to install the App on, on a token connection by
// typing what it covers — and inside it, choosing which repository an app
// deploys from is a developer's decision and always was.

// Repo is an owner and a repository name, which is the only shape coverage
// speaks in. Both hosts spell a repository that way and nothing here needs more.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// ParseRepo pulls the owner and repository out of a clone URL.
//
// It is deliberately dumb about hosts: the *host* is the source policy's
// question and has seven enforcement points of its own. This answers only
// "which repository", and a URL it cannot read is one coverage refuses rather
// than guesses at.
func ParseRepo(rawURL string) (Repo, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return Repo{}, fmt.Errorf("not a URL")
	}
	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return Repo{}, fmt.Errorf("does not name an owner and a repository")
	}
	// GitLab nests subgroups — acme/team/payroll — and the owner is everything
	// before the last segment. GitHub never does, so the same rule reads both.
	return Repo{
		Owner: strings.Join(parts[:len(parts)-1], "/"),
		Name:  parts[len(parts)-1],
	}, nil
}

// Covers reports whether one pattern admits a repository.
//
// **The matcher is pure and the syntax is the smallest one that works**:
// `acme/*` for an owner, `acme/payroll` for a repository, and nothing else. An
// organization with a naming convention may want more; a pattern language is
// easier to widen later than to narrow, so this starts narrow.
//
// Comparison folds case, because both hosts do, and an owner compares **whole**
// — never by prefix, or `acme` would admit `acme-evil`.
func Covers(pattern string, repo Repo) bool {
	pattern = strings.TrimSpace(pattern)
	slash := strings.LastIndex(pattern, "/")
	if slash <= 0 || slash == len(pattern)-1 {
		return false
	}
	owner, name := pattern[:slash], pattern[slash+1:]
	if !strings.EqualFold(owner, repo.Owner) {
		return false
	}
	if name == "*" {
		return true
	}
	return strings.EqualFold(name, repo.Name)
}

// CoverageRefusal on the resolver is the one callers use: it knows the two ways
// coverage is declared and picks by auth method.
//
// **It fails closed.** A host that cannot be reached means the boundary cannot
// be checked, and a boundary that admits when it cannot check is not one — the
// same direction `builder.sourceRefusal` fails in, and for the same reason.
// The cost is that attaching an app is refused while GitHub is down, which is a
// minute of inconvenience against a rule that would otherwise be optional.
func (r *Resolver) CoverageRefusal(ctx context.Context, conn *database.GitConnection, repoURL string) string {
	return r.CoverageChecker(ctx, conn)(repoURL)
}

// CoverageChecker is CoverageRefusal for many repositories of one connection
// in one request: the installation's coverage is read from the host **once**,
// on the first App-method question, and every later answer compares against
// that set. The picker asks it for every listed repository, and reading the
// installation per row cost floor(N/100)+1 host calls each (security review
// 2026-11-14, DG-4).
//
// It lives as long as the function it returns, never across requests: a
// cache that outlived one would be a cache of an authorization answer.
func (r *Resolver) CoverageChecker(ctx context.Context, conn *database.GitConnection) func(repoURL string) string {
	var (
		read      bool
		granted   map[string]bool
		truncated bool
		readErr   error
	)
	return func(repoURL string) string {
		if why := HostRefusal(conn, repoURL); why != "" {
			return why
		}
		if conn.AuthMethod != database.GitAuthApp {
			return CoverageRefusal(conn, repoURL)
		}
		repo, err := ParseRepo(repoURL)
		if err != nil {
			return fmt.Sprintf("%s %s, so it cannot be checked against the %s connection.",
				repoURL, err, conn.DisplayName)
		}
		if !read {
			read = true
			var list []string
			list, truncated, readErr = r.installationCoverage(ctx, conn)
			granted = make(map[string]bool, len(list))
			for _, full := range list {
				granted[strings.ToLower(full)] = true
			}
		}
		if readErr != nil {
			return fmt.Sprintf("What the %s connection covers could not be read from the host just "+
				"now, so %s cannot be attached to it yet. Nothing has changed; try again.",
				conn.DisplayName, repo)
		}
		if granted[strings.ToLower(repo.String())] {
			return ""
		}
		if truncated {
			// The list this compared against was cut short, so absence from
			// it is not evidence: saying "not installed" sent people to add a
			// repository the App already had (DG-12). Still a refusal — the
			// boundary fails closed — with advice that is true.
			return fmt.Sprintf("Whether the %s connection covers %s could not be confirmed: the "+
				"installation has more repositories than this install reads. Ask an administrator "+
				"to narrow the GitHub App's installation, or try again later.",
				conn.DisplayName, repo)
		}
		// **The organization declares this one and GitHub enforces it too**, so the
		// remedy is on the host rather than on this install's admin page.
		return fmt.Sprintf("The %s connection is not installed on %s. Add the repository to the "+
			"GitHub App's installation, or point the app at one it already covers.",
			conn.DisplayName, repo)
	}
}

// CoverageRefusal reports why this connection may not be pointed at this
// repository, or "" to go ahead.
//
// It is a sentence rather than a boolean because a refusal a person can act on
// says what would work, and this one has to name **both** the repository and
// the connection: neither alone tells them whether to change the URL or to ask
// an administrator.
//
// **This is the `token` half only.** An `app`-backed connection's coverage is
// the installation's and is read from the host, which needs a resolver — see
// the method above. A pure function is kept for the half that is pure, because
// a pattern matcher that could make a network call is one nobody can test as a
// matcher.
func CoverageRefusal(conn *database.GitConnection, repoURL string) string {
	if why := HostRefusal(conn, repoURL); why != "" {
		return why
	}
	repo, err := ParseRepo(repoURL)
	if err != nil {
		return fmt.Sprintf("%s %s, so it cannot be checked against the %s connection.",
			repoURL, err, conn.DisplayName)
	}
	patterns := conn.CoverageList()
	for _, p := range patterns {
		if Covers(p, repo) {
			return ""
		}
	}
	if len(patterns) == 0 {
		return fmt.Sprintf("The %s connection does not yet cover any repository, so %s cannot be "+
			"attached to it. An administrator sets what it covers on Admin → Sources.",
			conn.DisplayName, repo)
	}
	return fmt.Sprintf("The %s connection does not cover %s. It covers %s. Point the app at a "+
		"repository it covers, or ask an administrator to widen it on Admin → Sources.",
		conn.DisplayName, repo, strings.Join(patterns, ", "))
}

// HostRefusal reports why a clone URL is not on the connection's own host, or
// "" when it is.
//
// **A connection's credential goes only to the host it was issued for.**
// Coverage speaks in owner/repo and ParseRepo drops the host, so without this
// `https://attacker.example/acme/payroll.git` was covered by `acme/*` and the
// clone sent the organization's token to attacker.example. The scheme must be
// https and the host (and port) must equal the base URL's.
func HostRefusal(conn *database.GitConnection, repoURL string) string {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	b, berr := url.Parse(strings.TrimSpace(conn.BaseURL))
	if err != nil || berr != nil || u.Scheme != "https" || u.Host == "" || !strings.EqualFold(u.Host, b.Host) {
		return fmt.Sprintf("The %s connection is for %s, and %s is not on that host.",
			conn.DisplayName, conn.BaseURL, repoURL)
	}
	return ""
}
