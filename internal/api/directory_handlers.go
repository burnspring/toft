package api

import (
	"net/http"
	"strings"

	"toft/internal/auth"
)

// directoryUserDTO is the minimal identity a picker needs. No email — the
// directory is readable by every authenticated user, and `GET /users` (the
// full record) stays system_admin-only.
type directoryUserDTO struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	// Name is what a picker shows a person as. It is not an email and not a
	// new disclosure: a login already names the same account.
	Name       string `json:"name"`
	AvatarURL  string `json:"avatar_url"`
	SystemRole string `json:"system_role"`
}

// handleDirectoryUsers answers user-picker searches (sharing, ownership
// transfer). Any authenticated principal may search; anonymous callers may
// not. Queries under two characters return nothing rather than the whole
// directory.
func (s *Server) handleDirectoryUsers(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	out := []directoryUserDTO{}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) >= 2 {
		users, err := s.db.SearchUsers(q, 10)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		for _, u := range users {
			out = append(out, directoryUserDTO{ID: u.ID, Login: u.Login, Name: u.Name, AvatarURL: u.AvatarURL, SystemRole: u.SystemRole})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type directoryGroupDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// EligibleMembers is how many of this group's members could actually act as
	// owner — neither locked nor a system viewer.
	//
	// It is on the picker DTO rather than looked up per row afterwards because
	// both things the picker has to say need it, and both have to be said
	// *before* the choice is made: an empty group is shown and unselectable with
	// the reason (a transfer to it would be refused anyway), and a write-bearing
	// role on a group states how many people that is. A dropdown that reads the
	// same for a group of 2 and a group of 200 is the thing that eventually
	// becomes a finding.
	EligibleMembers int `json:"eligible_members"`
}

// handleDirectoryGroups answers group-picker searches for the sharing panel.
// Same access rule and shape as the user directory: any authenticated
// principal, minimal DTO, nothing under two characters.
func (s *Server) handleDirectoryGroups(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}

	out := []directoryGroupDTO{}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) >= 2 {
		groups, err := s.db.SearchGroups(q, 10)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		for _, g := range groups {
			n, err := s.db.CountEligibleGroupMembers(g.ID, "")
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			out = append(out, directoryGroupDTO{ID: g.ID, Name: g.Name, EligibleMembers: n})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
