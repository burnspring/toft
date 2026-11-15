package api

import (
	"net/http"
	"strings"

	"toft/internal/auth"
	"toft/internal/database"
)

// refuseAnonymousAppRead answers 401 to a caller with no credential. The
// handlers that once served a public app's record to anybody — the list, the
// detail by id and by slug, and its usage (BB-28) — still call it themselves;
// refuseAnonymousAppRoutes applies the same rule to every route under
// /api/v1/apps, which is what keeps it true of a route added later (BB-40:
// /consumption authorized with the app's read rule alone, which on a public app
// admits anybody).
//
// A public app's anonymous surface is the app, at the gateway. Nothing that
// runs without a session reads these routes — not the landing or sign-in page,
// not the proxy's own pages, not `toft fetch --anonymous`, which resolves the
// app with the caller's credential and drops it only for the app request — and
// what they returned (the owner, the release, the port, live readings,
// consumption) is an operator's.
func refuseAnonymousAppRead(w http.ResponseWriter, p *auth.Principal) bool {
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return true
	}
	return false
}

// refuseAnonymousAppRoutes is refuseAnonymousAppRead as middleware on the
// /api/v1 subrouter, for every route whose pattern starts with /api/v1/apps.
// It asks routeMatch, the resolver the credential guards use, so it refuses
// only a request some handler would take: a wrong method stays the router's
// 405 and a path nothing serves its 404, which say nothing about any app.
func (s *Server) refuseAnonymousAppRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pattern, ok := s.routeMatch(r); ok &&
			(pattern == "/api/v1/apps" || strings.HasPrefix(pattern, "/api/v1/apps/")) {
			if refuseAnonymousAppRead(w, auth.GetPrincipal(r.Context())) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// publicOwner is an app's owner as anyone but an administrator reads it: who
// to ask, and nothing about the account (BB-22, BB-28).
//
// `database.User` serializes the address, the system role, when the account
// was made and when it last signed in — the People page's row, which an
// administrator is reading on purpose. Attached whole to every app, it handed
// every signed-in account the email and last sign-in of every owner on the
// install. This is a type of its own rather than that struct with fields
// blanked, so a field added to `User` later reaches an app's readers only by
// somebody deciding it should.
type publicOwner struct {
	ID        string `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// ownerFor is the one place an app's owner is shaped for a caller: the whole
// record for an administrator, the public shape for everybody else. Untyped nil
// for no person — a group-owned app — so `omitempty` drops it rather than
// writing a typed nil.
func ownerFor(p *auth.Principal, u *database.User) any {
	if u == nil {
		return nil
	}
	if p.IsAdmin() {
		return u
	}
	return publicOwnerOf(u)
}

func publicOwnerOf(u *database.User) *publicOwner {
	if u == nil {
		return nil
	}
	return &publicOwner{ID: u.ID, Login: u.Login, Name: u.Name, AvatarURL: u.AvatarURL}
}

// listOnlyAppRow is a row of `GET /apps` held at the `list` rung — the app is
// in the caller's list and opening it meets the gate.
//
// The rung discloses the listing's row and not one field more (ADR §90): name,
// slug, owner and framework, plus what this caller may do with the row. The
// owner travels as its id, its group and its public shape, because the access
// gate and the Owner column draw either kind of owner from those three. A
// projection of its own, never the full row with fields blanked: a field added
// to `database.App` later must not reach a list holder by default.
type listOnlyAppRow struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Slug         string       `json:"slug"`
	Framework    string       `json:"framework"`
	OwnerID      string       `json:"owner_id"`
	OwnerGroupID string       `json:"owner_group_id,omitempty"`
	Owner        *publicOwner `json:"owner,omitempty"`
	CanEdit      bool         `json:"can_edit"`
	CanDelete    bool         `json:"can_delete"`
	CanOpen      bool         `json:"can_open"`
	Mine         bool         `json:"mine"`
	Listable     bool         `json:"listable"`
}

func listOnlyRow(app *database.App, owner *database.User) listOnlyAppRow {
	return listOnlyAppRow{
		ID: app.ID, Name: app.Name, Slug: app.Slug, Framework: app.Framework,
		OwnerID: app.OwnerID, OwnerGroupID: app.OwnerGroupID, Owner: publicOwnerOf(owner),
		Listable: app.Listable,
	}
}

// readerAppSummary is the summary block of GET /apps/{id} for a reader below
// editor: the grant counts, the 30-day usage and the quiet verdict the
// viewer's Overview prints. A separate type rather than zeroed fields, because
// GetAppSummary's own rule is that a zeroed field reads as a fact ("0
// schedules") and QuietSince would marshal as year 1 — the withheld keys are
// absent.
type readerAppSummary struct {
	GrantUsers  int    `json:"grant_users"`
	GrantGroups int    `json:"grant_groups"`
	Requests30d int    `json:"requests_30d"`
	Viewers30d  int    `json:"viewers_30d"`
	QuietDays   int    `json:"quiet_days"`
	QuietBasis  string `json:"quiet_basis"`
}

func readerSummaryOf(s *database.AppSummary) *readerAppSummary {
	if s == nil {
		return nil
	}
	return &readerAppSummary{
		GrantUsers: s.GrantUsers, GrantGroups: s.GrantGroups,
		Requests30d: s.Requests30d, Viewers30d: s.Viewers30d,
		QuietDays: s.QuietDays, QuietBasis: s.QuietBasis,
	}
}

// summaryFor is the summary block as this caller may read it; nil stays a
// JSON null either way.
func summaryFor(s *database.AppSummary, canEdit bool) any {
	if canEdit {
		if s == nil {
			return nil
		}
		return s
	}
	if r := readerSummaryOf(s); r != nil {
		return r
	}
	return nil
}
