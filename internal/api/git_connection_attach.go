package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"toft/internal/auth"
	"toft/internal/database"
)

// Attaching an app to a connected git host, on the two routes that can do it.
//
// Both checks live here rather than beside each write, because they are one
// rule each and the two writes are two hundred lines apart. What they are:
//
//  1. **No key may set the field** (D10). `POST /apps` is on both the
//     personal-key and the deploy-key maps and `PATCH /apps/{appID}` is off the
//     deploy map already, explicitly because it carries `git_token`. A deploy
//     key belongs to nobody and is owner on every app, so a deploy key that
//     could set `git_connection_id` could grant itself the organization's git
//     access. The field is refused rather than the route, because taking
//     `POST /apps` off the deploy-key map would break every pipeline that
//     creates its own app to fix a field those pipelines do not send.
//
//  2. **The connection has to cover the repository** (D17). This is what makes
//     it safe that *any* session principal may attach — no role gates it, and a
//     role would take the repository choice away from the person who knows
//     which repository they want. The boundary is one an administrator drew.
//
// Both refuse **before anything is written**, so a refused create leaves no app
// row and a refused re-point leaves the app where it was.

// refusedConnectionByKey answers rule 1. It returns true when it has written
// the response.
func refusedConnectionByKey(w http.ResponseWriter, r *http.Request, field string) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !(p.IsAPIKey() || p.IsDeployKey()) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "`" + field + "` requires an interactive session — pointing an app at a connected " +
			"git host is a credential decision, and API keys cannot manage credentials",
		"code": "session_required",
	})
	return true
}

// refusedUncoveredRepo answers rule 2 for one repository against one
// connection. An empty connectionID is no connection at all and is allowed:
// an app with no connection is every app that exists today.
//
// The refusal is 403 and names **both** the repository and the connection,
// because neither alone tells the developer whether to change the URL or to ask
// an administrator. A repository typed into the URL field by hand meets exactly
// the check a picked one does — which is why the picker can filter without
// being an enforcement point.
func (s *Server) refusedUncoveredRepo(ctx context.Context, w http.ResponseWriter, connectionID, repoURL string) bool {
	if connectionID == "" {
		return false
	}
	conn, err := s.db.GetGitConnection(connectionID)
	if errors.Is(err, database.ErrGitConnectionNotFound) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "no such connected git host",
			"code":  "no_such_connection", "hint": "GET /api/v1/git/connections lists the connected git hosts with their ids",
		})
		return true
	}
	if err != nil {
		log.Printf("reading git connection %s: %v", connectionID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return true
	}
	if why := s.gitConn.CoverageRefusal(ctx, conn, repoURL); why != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": why,
			"code":  "repository_not_covered", "hint": "point the app at a repository the connection covers, or ask an administrator to add this one to it",
		})
		return true
	}
	return false
}

// developerGitConnectionDTO is what a *developer* is told about a connection:
// that it exists, what it is called, which host it is, and whether they will
// get a repository list or a URL field.
//
// **Never the auth method.** Whether the organization's connection is backed by
// an App or by a token is an administrator's fact about a credential. What a
// developer needs to know is that there is a connected host and that they need
// no credential for it, which is the entire promise, and `picker` is the only
// part of the difference that changes what they see.
type developerGitConnectionDTO struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Picker      bool   `json:"picker"`
	// Coverage is what they may point an app at, so the form can say so before
	// the write refuses. It is the administrator's declaration, not a
	// credential, and a developer who can see it opens fewer tickets.
	Coverage []string `json:"coverage"`
}

// handleListGitConnections for developers: the connections an app may be
// pointed at, on the New App form and in an app's settings.
//
// Every signed-in user may read it, on the same reasoning as the effective
// source policy beside it: a form that refuses without naming what would work
// sends every developer to the same Slack channel. A disabled connection is
// absent — offering a developer a connection whose next deploy would fail is
// worse than not offering one.
func (s *Server) handleListMyGitConnections(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rows, err := s.db.ListGitConnections()
	if err != nil {
		log.Printf("listing git connections: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]developerGitConnectionDTO, 0, len(rows))
	for i := range rows {
		if !rows[i].Enabled {
			continue
		}
		out = append(out, developerGitConnectionDTO{
			ID:          rows[i].ID,
			Kind:        rows[i].Kind,
			DisplayName: rows[i].DisplayName,
			Picker:      rows[i].HasPicker(),
			Coverage:    rows[i].CoverageList(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

// appConnectionSummary is what an app's own page says about the credential in
// force: the connection's name, or nothing.
//
// Nil rather than an empty object when there is none, so a client can tell "no
// connection" from "a connection whose name did not load" — and so Settings
// renders the token field it always has, byte for byte, on the install that
// never adopted this feature.
func (s *Server) appConnectionSummary(app *database.App) *developerGitConnectionDTO {
	if app.GitConnectionID == "" {
		return nil
	}
	conn, err := s.db.GetGitConnection(app.GitConnectionID)
	if err != nil {
		// A connection that has gone missing is a fact worth logging and not
		// worth failing the app's page over. The resolver logs it too, and
		// falls back to whatever the app has of its own.
		log.Printf("reading git connection %s for app %s: %v", app.GitConnectionID, app.Slug, err)
		return nil
	}
	return &developerGitConnectionDTO{
		ID:          conn.ID,
		Kind:        conn.Kind,
		DisplayName: conn.DisplayName,
		Picker:      conn.HasPicker(),
		Coverage:    conn.CoverageList(),
	}
}
