package api

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// accessSource explains why a user can reach an app: ownership, a direct
// grant, a grant to one of their groups, or membership of the group that
// **owns** it.
//
// owner_via_group is its own kind rather than a `group` source that happens to
// say owner, and the distinction is the reportable one. A group grant is
// somebody sharing an app with a team; owner_via_group is the team *being the
// owner*, which is the relationship this report exists to surface and the one
// an access review would otherwise miss entirely. Shipping without it would
// make the report silently under-report, which is a compliance defect
// (AC-2(j), AC-2(7)(b), CSF PR.AA-05) rather than a cosmetic gap.
type accessSource struct {
	Kind      string `json:"kind"` // owner | owner_via_group | direct | group
	Role      string `json:"role"`
	GroupID   string `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`
}

type accessAppRef struct {
	ID     string `json:"id"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type accessEntry struct {
	App           accessAppRef   `json:"app"`
	EffectiveRole string         `json:"effective_role"`
	Sources       []accessSource `json:"sources"`
}

// accessReportUser is the subject the report is about. Its fields are the four
// the table's header needs and no more — an access report is about apps, and
// widening this is how it would quietly become a second user record.
type accessReportUser struct {
	ID         string `json:"id"`
	Login      string `json:"login"`
	SystemRole string `json:"system_role"`
	Locked     bool   `json:"locked"`
}

// accessVisibility is the sentence under the table that makes the count
// honest: the apps everyone signed in can reach, and the apps anyone can.
// They are a count rather than rows because they are not *this* user's access
// — they are the floor everybody stands on.
type accessVisibility struct {
	PublicApps int `json:"public_apps"`
	// PublicAppsInEffect is how many of those anonymous callers can actually
	// reach: the rest are published apps whose reachability a service outside
	// this install would not confirm, and whose anonymous access the platform
	// has withdrawn while the column still reads `public`.
	//
	// Two numbers rather than one, because "who chose what" and "how exposed
	// are we" are different questions and this is the first thing in the
	// product that can make their answers differ.
	PublicAppsInEffect int `json:"public_apps_in_effect"`
	AuthenticatedApps  int `json:"authenticated_apps"`
}

type accessReport struct {
	User             accessReportUser `json:"user"`
	Explicit         []accessEntry    `json:"explicit"`
	VisibilityAccess accessVisibility `json:"visibility_access"`
}

// handleUserAccess — GET /users/{id}/access. system_admin only: this is
// cross-tenant introspection. Non-admins get 404, not 403 — the route must
// not confirm which user IDs exist. Read-only by design; revocation lives on
// each app's sharing panel.
func (s *Server) handleUserAccess(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	userID := chi.URLParam(r, "id")
	target, err := s.db.GetUser(userID)
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the users with their ids"})
		return
	}

	report, err := s.buildAccessReport(target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// buildAccessReport computes which apps a user can reach and why.
//
// It is a function rather than a handler body because there are now two ways
// to ask the question — an administrator reviewing somebody
// (GET /users/{id}/access) and a person reading their own standing
// (GET /me/access) — and the answers must not be able to differ. A page is a
// reader of this computation, never a second copy of it: nothing downstream
// re-derives access from grants, which is what keeps the effective-role rules
// (the viewer-exclusion downgrade, the group clamp) in the one place that
// already applies them.
//
// The caller does its own authorization and its own 404 before calling this;
// this function assumes the decision to answer has already been taken.
func (s *Server) buildAccessReport(target *database.User) (*accessReport, error) {
	userID := target.ID

	groups, err := s.db.ListUserGroups(userID)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]string, 0, len(groups))
	groupNames := map[string]string{}
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
		groupNames[g.ID] = g.Name
	}

	owned, err := s.db.ListAppsOwnedBy(userID)
	if err != nil {
		return nil, err
	}
	grants, err := s.db.ListUserAccessGrants(userID, groupIDs)
	if err != nil {
		return nil, err
	}

	// One entry per app, whatever the mix of ownership and grants; the object
	// row travels along so the effective role can be computed per app.
	type appRow struct {
		entry *accessEntry
		obj   *auth.Object
	}
	byApp := map[string]*appRow{}
	// The third construction of an auth.Object in the product, and the only one
	// not built from an App row — which is why it goes through appObject rather
	// than spelling the object out. A public app whose reachability has been
	// withdrawn — by a verdict or by this install's own switch — is
	// `authenticated` here for the same reason it is everywhere else.
	ensure := func(id, slug, name, ownerID, visibility, status string, anonymityWithheld bool) *appRow {
		if row, ok := byApp[id]; ok {
			return row
		}
		row := &appRow{
			entry: &accessEntry{App: accessAppRef{ID: id, Slug: slug, Name: name, Status: status}, Sources: []accessSource{}},
			obj:   appObject(id, ownerID, visibility, anonymityWithheld),
		}
		byApp[id] = row
		return row
	}

	for _, a := range owned {
		row := ensure(a.ID, a.Slug, a.Name, a.OwnerID, a.Visibility, a.Status, a.PublicWithheld)
		row.entry.Sources = append(row.entry.Sources, accessSource{Kind: "owner", Role: database.ObjectRoleOwner})
	}
	for _, g := range grants {
		// The owner's own grant row duplicates the ownership source.
		if g.PrincipalType == database.PrincipalTypeUser && g.Role == database.ObjectRoleOwner && g.AppOwnerID == userID {
			continue
		}
		row := ensure(g.AppID, g.AppSlug, g.AppName, g.AppOwnerID, g.AppVisibility, g.AppStatus, g.AppPublicWithheld)
		switch g.PrincipalType {
		case database.PrincipalTypeUser:
			row.entry.Sources = append(row.entry.Sources, accessSource{Kind: "direct", Role: g.Role})
		case database.PrincipalTypeGroup:
			// No clamp here any more. A group's stored role is what it confers,
			// and reporting viewer for a group that holds editor would be
			// the access review lying about the access it exists to review.
			// What still applies is the system-role downgrade, which is applied
			// below to the *effective* role for the whole app rather than per
			// source — a source says what the relationship grants, the
			// effective role says what the person ends up with.
			kind := "group"
			if g.PrincipalID == g.AppOwnerGroupID {
				kind = "owner_via_group"
			}
			row.entry.Sources = append(row.entry.Sources, accessSource{
				Kind: kind, Role: g.Role, GroupID: g.PrincipalID, GroupName: groupNames[g.PrincipalID],
			})
		}
	}

	// Same read-time rules as auth.Authorize (viewer-exclusion downgrade,
	// group clamp via the resolver) — not re-derived in SQL.
	targetPrincipal := &auth.Principal{
		Type: database.PrincipalTypeUser, ID: userID,
		SystemRole: target.SystemRole, Groups: groupIDs,
	}
	explicit := make([]accessEntry, 0, len(byApp))
	for _, row := range byApp {
		row.entry.EffectiveRole = auth.EffectiveObjectRole(targetPrincipal, row.obj, s.db)
		explicit = append(explicit, *row.entry)
	}
	sort.Slice(explicit, func(i, j int) bool { return explicit[i].App.Slug < explicit[j].App.Slug })

	publicCount, publicInEffect, authCount, err := s.db.CountAppsByVisibility()
	if err != nil {
		return nil, err
	}

	return &accessReport{
		User: accessReportUser{
			ID:         target.ID,
			Login:      target.Login,
			SystemRole: target.SystemRole,
			Locked:     target.Locked(),
		},
		Explicit: explicit,
		VisibilityAccess: accessVisibility{
			PublicApps:         publicCount,
			PublicAppsInEffect: publicInEffect,
			AuthenticatedApps:  authCount,
		},
	}, nil
}

// userAppUsageEntry is one app a person opened over the window.
type userAppUsageEntry struct {
	AppID    string `json:"app_id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	Days     int    `json:"days"`
	FirstDay string `json:"first_day"`
	LastDay  string `json:"last_day"`
}

// handleUserAppUsage — GET /users/{id}/app-usage?days=N. What one person has
// been opening, and how much, over the last N UTC days (1–365, default 90):
// the transpose of each app's own Usage → viewers, which an administrator can
// already read app by app. Only apps that still exist (GetUserAppUsage).
//
// system_admin only, 404 otherwise, exactly as /users/{id}/access: it is
// cross-tenant introspection about a named person, and the route must not
// confirm which user ids exist. Not on the API-key allow-list.
func (s *Server) handleUserAppUsage(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	target, err := s.db.GetUser(chi.URLParam(r, "id"))
	if err != nil || target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "hint": "GET /api/v1/users lists the users with their ids"})
		return
	}

	days := 90
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 365 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "`days` must be a whole number from 1 to 365", "code": "invalid_parameter",
				"hint": "send days as a whole number, like ?days=30",
			})
			return
		}
		days = n
	}
	until := time.Now().UTC().Truncate(24 * time.Hour)
	since := until.AddDate(0, 0, -(days - 1))

	rows, err := s.db.GetUserAppUsage(target.ID, since, until)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]userAppUsageEntry, 0, len(rows))
	for _, u := range rows {
		out = append(out, userAppUsageEntry(u))
	}
	// As the per-app read says it: a window reaching past what the install
	// keeps is answered, and the page says it was clipped.
	retained := ""
	if from, err := s.db.AccessRecordRetainedFrom(); err == nil && !from.IsZero() {
		retained = from.Format(dayLayout)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days": days, "since": since.Format(dayLayout), "until": until.Format(dayLayout),
		"apps": out, "retained_from": retained,
	})
}
