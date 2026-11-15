package gitconn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"toft/internal/database"
	"toft/internal/safefetch"
)

// The GitHub host client.
//
// **A connection builds its own `safefetch.Client`, and that is the whole
// exception.** The package answered this before the feature asked: `Client`
// holds a strict and a permissive transport, and `New(configuredURL, timeout)`
// makes exactly the host in `configuredURL` reachable and nothing else. A
// connection *is* a configured URL, so there is no flag on the row, no
// checkbox, and no per-case judgement — a GitHub Enterprise Server on
// 10.0.0.0/8 is reachable because an administrator typed it into an admin-only
// field, and a URL that arrives *inside* a response from that host gets the
// strict client like every other URL somebody else chose.

const (
	hostTimeout = 20 * time.Second
	// maxHostResponse caps what a host may hand back. Generous for a page of
	// repositories and far short of anything that would matter.
	maxHostResponse = 4 << 20
	// reposPerPage is what the picker and the coverage read ask for.
	reposPerPage = 100
	// maxCoveragePages bounds the installation walk. An installation granted
	// more repositories than this is one whose coverage nobody reads as a list
	// anyway, and an unbounded loop against a host is not a thing to write.
	maxCoveragePages = 10
)

// clientFor returns the connection's own HTTP client, building it on first use
// and dropping it whenever the row is written (see Resolver.Forget).
func (r *Resolver) clientFor(conn *database.GitConnection) *safefetch.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[conn.ID]; ok {
		return c
	}
	c := safefetch.New(conn.BaseURL, hostTimeout)
	r.clients[conn.ID] = c
	return c
}

// SetClientForTesting installs a client for one connection, so a test can point
// a connection at an `httptest` server — whose certificate nothing trusts and
// whose address is on loopback — while keeping every other rule in force. The
// same seam `safefetch.NewForTesting` exists for, one layer up. Nothing in
// production calls it.
func (r *Resolver) SetClientForTesting(connectionID string, c *safefetch.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[connectionID] = c
}

// apiBase is where this connection's REST API lives. github.com puts it on a
// separate host; every Enterprise Server puts it under /api/v3 on the host
// itself, which is the case the base URL exists for.
func apiBase(conn *database.GitConnection) string {
	base := strings.TrimRight(conn.BaseURL, "/")
	switch conn.Kind {
	case database.GitConnGitHub:
		if base == "https://github.com" {
			return "https://api.github.com"
		}
		return base + "/api/v3"
	default:
		return base + "/api/v4"
	}
}

// acceptFor is the media type this host is asked for. GitHub's REST API wants
// its own vendor type; GitLab has none, and sending it another host's is how a
// request comes back 406 for a reason nobody can read off the error.
func acceptFor(conn *database.GitConnection) string {
	if conn.Kind == database.GitConnGitLab {
		return "application/json"
	}
	return "application/vnd.github+json"
}

// hostStatusError is a non-2xx from the host, carrying the code so a caller
// can tell "your credential was refused" from "that host has no such route".
//
// **The status and never the body**, which is the rule this package already
// held as a string format: a body echoed into an error is how a token ends up
// in a log. Making it a type changes nothing about what is said — Error()
// renders exactly the sentence the format did — and gives the connection test
// the one fact it needs to phrase a 401 differently from a 404.
type hostStatusError struct {
	Connection string
	Code       int
	Status     string
}

func (e *hostStatusError) Error() string {
	return fmt.Sprintf("%s answered %s", e.Connection, e.Status)
}

// hostJSON performs one authenticated GET against the connection's host and
// decodes the body into out.
//
// Every request goes through the connection's own client, so the private-host
// exemption is the connection's and nothing else inherits it. The response is
// size-capped, and a non-2xx carries the host's *status* and never its body —
// a body echoed into an error is how a token ends up in a log.
func (r *Resolver) hostJSON(ctx context.Context, conn *database.GitConnection, rawURL, token string, out any) error {
	client := r.clientFor(conn)
	if err := client.CheckConfigured(rawURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", acceptFor(conn))
	req.Header.Set("User-Agent", "Toft")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.For(rawURL).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &hostStatusError{Connection: conn.DisplayName, Code: resp.StatusCode, Status: resp.Status}
	}
	body, err := safefetch.ReadLimited(resp.Body, maxHostResponse, "the host's answer")
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// hostRepo is the shape both listings decode into. Deliberately three fields:
// what the picker renders and what coverage compares.
type hostRepo struct {
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	Private  bool   `json:"private"`
	// Archived repositories are still listed — a developer who cannot see why
	// their repository is missing opens a ticket — and marked.
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
}

// Repository is one entry of the picker's list.
type Repository struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
	// Allowed and Refusal are the source policy's answer, marked rather than
	// enforced — see D9 and `handleListConnectionRepos`.
	Allowed bool   `json:"allowed"`
	Refusal string `json:"refusal,omitempty"`
	// Covered says whether attaching an app to it would be refused by the
	// coverage rule. Shown for the same reason the policy's answer is: a
	// developer who can see why does not open a ticket.
	Covered bool `json:"covered"`
}

// SearchRepositories lists what **this person** can see on the host.
//
// The token is theirs (`git_connection_identities.access_token`), never the
// install's. Listing with the clone credential is the shortcut that turns the
// picker into an estate directory — an install-wide credential sees everything
// the organization exposed, so any signed-in developer would get a catalogue of
// the company's private repositories. There is no conversion between the two
// types and `TestThePickerNeverListsThroughTheCloneCredential` says so.
//
// No cache: a cache of what a person may see is a cache of an authorization
// answer. The cost is the host's rate limits in the request path, which is why
// the search debounces and has a real error state.
func (r *Resolver) SearchRepositories(ctx context.Context, conn *database.GitConnection, pickerToken pickerToken, query string, page int) ([]Repository, bool, error) {
	if page < 1 {
		page = 1
	}
	// **A search walks the pages; a plain listing reads one.** Neither listing
	// route filters by name on the host, so a query applied to one page of 100
	// could never find the 101st repository (security review 2026-11-14,
	// DG-11). The walk is bounded like the coverage read, and stops at a
	// short page or a full page of matches. hasMore says the answer was cut
	// short, so the picker can say "search to find others".
	needle := strings.ToLower(query)
	last := page
	if needle != "" {
		last = page + maxCoveragePages - 1
	}
	out := []Repository{}
	for p := page; p <= last; p++ {
		got, err := r.reposPage(ctx, conn, pickerToken, p)
		if err != nil {
			return nil, false, err
		}
		for _, repo := range got {
			if needle != "" && !strings.Contains(strings.ToLower(repo.FullName), needle) {
				continue
			}
			if len(out) == reposPerPage {
				return out, true, nil
			}
			out = append(out, Repository{
				FullName:      repo.FullName,
				CloneURL:      repo.CloneURL,
				Private:       repo.Private,
				Archived:      repo.Archived,
				DefaultBranch: repo.DefaultBranch,
				Description:   repo.Description,
			})
		}
		if len(got) < reposPerPage {
			return out, false, nil
		}
	}
	// The last page read was full: there may be more.
	return out, true, nil
}

// reposPage is one page of the installation's repositories this person can
// see.
func (r *Resolver) reposPage(ctx context.Context, conn *database.GitConnection, pickerToken pickerToken, page int) ([]hostRepo, error) {
	q := url.Values{}
	q.Set("per_page", fmt.Sprint(reposPerPage))
	q.Set("page", fmt.Sprint(page))
	q.Set("sort", "pushed")
	// The installation's repositories that *this person* can also see. That
	// intersection is the picker's whole definition: the connection bounds it
	// to what the organization installed the App on, and the person's token
	// bounds it to what they could already reach.
	endpoint := apiBase(conn) + "/installation/repositories?" + q.Encode()

	var body struct {
		Repositories []hostRepo `json:"repositories"`
	}
	if err := r.hostJSON(ctx, conn, endpoint, string(pickerToken), &body); err != nil {
		return nil, err
	}
	return body.Repositories, nil
}

// InstallationCoverage reads what the installation was granted, which is an
// `app`-backed connection's coverage.
//
// **The organization declares it and GitHub enforces it too**, which is the
// difference from a token connection, where the patterns are an administrator's
// typing and this install is the only thing that checks them.
//
// Read with the *installation* token, not a person's: this is a fact about the
// connection rather than about a viewer.
func (r *Resolver) InstallationCoverage(ctx context.Context, conn *database.GitConnection) ([]string, error) {
	out, _, err := r.installationCoverage(ctx, conn)
	return out, err
}

// installationCoverage is InstallationCoverage and whether the list was cut
// short — the walk stopped at maxCoveragePages on a full page, or the host
// counted more than it returned. A repository missing from a cut list is
// unconfirmed, not uninstalled (security review 2026-11-14, DG-12).
func (r *Resolver) installationCoverage(ctx context.Context, conn *database.GitConnection) ([]string, bool, error) {
	token, err := r.installationToken(ctx, conn, "")
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	var out []string
	for page := 1; page <= maxCoveragePages; page++ {
		q := url.Values{}
		q.Set("per_page", fmt.Sprint(reposPerPage))
		q.Set("page", fmt.Sprint(page))
		var body struct {
			TotalCount   int        `json:"total_count"`
			Repositories []hostRepo `json:"repositories"`
		}
		endpoint := apiBase(conn) + "/installation/repositories?" + q.Encode()
		if err := r.hostJSON(ctx, conn, endpoint, token, &body); err != nil {
			return nil, false, err
		}
		for _, repo := range body.Repositories {
			if repo.FullName != "" && !seen[repo.FullName] {
				seen[repo.FullName] = true
				out = append(out, repo.FullName)
			}
		}
		if len(body.Repositories) < reposPerPage {
			return out, body.TotalCount > len(out), nil
		}
	}
	// The bound was reached on a full page: there may be more.
	return out, true, nil
}
