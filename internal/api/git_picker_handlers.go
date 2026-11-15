package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/gitconn"
)

// The repository picker: sign a developer in to the host as themselves, and
// show them what they can already see.
//
// **The state cookie sits at its own path**, `/api/v1/git`, against the sign-in
// flow's `/api/v1/auth`. The browser is what keeps a repository-scoped callback
// out of the sign-in handler — a stronger separation than two handlers that
// agree not to share, and it costs one constant. Reusing the sign-in callback
// would make it reachable with a repository scope, which is worth one constant
// to avoid.
const (
	gitCookiePath      = "/api/v1/git"
	gitStateCookie     = "toft_git_state"
	gitConnCookie      = "toft_git_connection"
	gitStateCookieTTL  = 600
	maxPickerPageIndex = 20
)

func (s *Server) setGitCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     gitCookiePath,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   gitStateCookieTTL,
	})
}

// clearGitCookies expires the in-flight picker state. Called on every exit from
// the callback, success or failure: a leftover state is a credential for an
// attempt that is over.
func (s *Server) clearGitCookies(w http.ResponseWriter) {
	for _, name := range []string{gitStateCookie, gitConnCookie} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: gitCookiePath, MaxAge: -1, HttpOnly: true,
			Secure: s.secureCookies(),
		})
	}
}

// pickerConnection resolves the connection named in the URL, for a signed-in
// person, and refuses one with no picker.
func (s *Server) pickerConnection(w http.ResponseWriter, r *http.Request) (*database.GitConnection, *auth.Principal, bool) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, nil, false
	}
	conn, err := s.db.GetGitConnection(chi.URLParam(r, "connectionID"))
	if errors.Is(err, database.ErrGitConnectionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/git/connections lists the connected git hosts you can pick from"})
		return nil, nil, false
	}
	if err != nil {
		log.Printf("reading git connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, nil, false
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/git/connections lists the connected git hosts you can pick from"})
		return nil, nil, false
	}
	if !conn.HasPicker() {
		// A developer is never told *why* — that this connection is backed by a
		// pasted token rather than a GitHub App is an administrator's fact
		// about a credential. What they get is the URL field, which the form
		// already knows from `picker: false`.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this connection has no repository picker — enter the repository URL instead", "hint": "enter the repository URL in repo_url instead"})
		return nil, nil, false
	}
	return conn, p, true
}

func (s *Server) gitRedirectURI(connectionID string) string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/api/v1/git/connections/" + connectionID + "/callback"
}

// handleGitAuthorize starts the picker's sign-in.
func (s *Server) handleGitAuthorize(w http.ResponseWriter, r *http.Request) {
	conn, _, ok := s.pickerConnection(w, r)
	if !ok {
		return
	}
	url, state, err := s.gitConn.AuthorizeURL(conn, s.gitRedirectURI(conn.ID))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "hint": "enter the repository URL instead, or ask an administrator to check the connection's sign-in settings"})
		return
	}
	s.setGitCookie(w, gitStateCookie, state)
	s.setGitCookie(w, gitConnCookie, conn.ID)
	http.Redirect(w, r, url, http.StatusFound)
}

// handleGitCallback finishes it, and stores this person's token.
func (s *Server) handleGitCallback(w http.ResponseWriter, r *http.Request) {
	conn, p, ok := s.pickerConnection(w, r)
	if !ok {
		return
	}
	defer s.clearGitCookies(w)

	state, err := r.Cookie(gitStateCookie)
	if err != nil || state.Value == "" || state.Value != r.URL.Query().Get("state") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this sign-in could not be matched to one that started here — try again", "hint": "start the sign-in again from the same browser"})
		return
	}
	// The connection in the cookie has to be the connection in the URL, or a
	// state minted for one host would finish against another.
	if c, err := r.Cookie(gitConnCookie); err != nil || c.Value != conn.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this sign-in was started for a different connection", "hint": "start the sign-in again from the connection you want to use"})
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the host returned no code", "hint": "start the sign-in again, and approve access on the git host"})
		return
	}

	if err := s.gitConn.ExchangeCode(r.Context(), conn, p.ID, code, s.gitRedirectURI(conn.ID)); err != nil {
		log.Printf("git picker: exchanging the code for %s: %v", conn.DisplayName, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "signing in to " + conn.DisplayName + " did not work — try again"})
		return
	}
	// Back to where a repository is chosen. The SPA reads the connection from
	// the query and re-opens the picker.
	http.Redirect(w, r, strings.TrimRight(s.cfg.BaseURL, "/")+
		"/apps/new?connection="+conn.ID, http.StatusFound)
}

// handleListConnectionRepos is the picker's list.
//
// **It filters and it is not an enforcement point** (D9). Each entry is marked
// with whether the source policy would accept its clone URL and whether the
// connection covers it, and the refused ones render disabled with the refusal
// text rather than being hidden — a developer who can see why does not open a
// ticket. It writes nothing, and every one of the source policy's seven
// enforcement points runs afterwards unchanged.
//
// **An eighth entry on that list would be wrong**, and saying so is the point:
// the picker cannot refuse anything, because a repository it never lists can
// still be typed into the URL field beside it.
func (s *Server) handleListConnectionRepos(w http.ResponseWriter, r *http.Request) {
	conn, p, ok := s.pickerConnection(w, r)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page > maxPickerPageIndex {
		page = maxPickerPageIndex
	}

	repos, hasMore, err := s.gitConn.ListRepositoriesFor(r.Context(), conn, p.ID,
		strings.TrimSpace(r.URL.Query().Get("q")), page)
	if errors.Is(err, gitconn.ErrNoPickerIdentity) {
		// Not an error: not having signed in to the host yet is the ordinary
		// first state. The client sends them to /authorize.
		writeJSON(w, http.StatusOK, map[string]any{
			"repositories":    []gitconn.Repository{},
			"needs_authorize": true,
			"authorize_url":   "/api/v1/git/connections/" + conn.ID + "/authorize",
			"connection_name": conn.DisplayName,
		})
		return
	}
	if err != nil {
		log.Printf("git picker: listing repositories on %s: %v", conn.DisplayName, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": conn.DisplayName + " could not be reached. Enter the repository URL instead, " +
				"or try again."})
		return
	}

	policy, policyErr := s.sourcePolicy()
	// One coverage read for the whole page, not one per row (DG-4).
	covers := s.gitConn.CoverageChecker(r.Context(), conn)
	for i := range repos {
		repos[i].Covered = covers(repos[i].CloneURL) == ""
		repos[i].Allowed = true
		if policyErr != nil {
			continue
		}
		if refusal := policy.Allows(repos[i].CloneURL); refusal != nil {
			repos[i].Allowed = false
			repos[i].Refusal = refusal.Error()
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repositories":    repos,
		"has_more":        hasMore,
		"connection_name": conn.DisplayName,
	})
}
