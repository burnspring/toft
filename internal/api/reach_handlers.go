package api

import (
	"net/http"

	"toft/internal/appint"
	"toft/internal/auth"
	"toft/internal/database"
)

// What an app can reach: the assembly, and the estate's transpose of it.
//
// Two routes and one rule they share. `GET /apps/{appID}/reach` is one app's
// whole picture for whoever can edit it; `GET /admin/estate/reach` is every
// app's, for an administrator. Neither reports a value, neither reports
// history, and both say on the page what the platform cannot see — see ADR §92.

// ------------------------------------------------------------- the per-app read

// appReachResponse is one app's assembly.
//
// The embedded AppReach carries the recorded facts; everything beside it is
// resolved here, because it needs either appint or the authorizer and
// internal/database can import neither.
type appReachResponse struct {
	database.AppReach
	// VisibilityWithheld says the word above is not the app's own: `public` in
	// the database and `authenticated` in effect, because this install's
	// anonymous-access switch is off or the reachability latch has closed.
	VisibilityWithheld bool `json:"visibility_withheld,omitempty"`
	// Integrations is resolved through appint, kind by kind, so the per-app
	// read and appint.Check cannot disagree about what an app may send.
	Integrations []database.ReachIntegration `json:"integrations"`
	// Owner is who the app belongs to — a user or a group, never both and
	// never neither. Not a grant: ownership is a column.
	Owner *reachOwner `json:"owner,omitempty"`
	// ShowStorage and ShowIntegrations are the optional-capabilities rule on
	// this page: an install that has provisioned no storage, or connected no
	// integration, gets no section rather than a section of zeros. Absent, not
	// off — an app was not *refused* something the install never offered.
	ShowStorage      bool `json:"show_storage"`
	ShowIntegrations bool `json:"show_integrations"`
	// CannotSee is the sentence this page is not allowed to omit. Served from
	// the API rather than written into the client so both surfaces say the
	// same thing and neither can quietly drop it.
	CannotSee string `json:"cannot_see"`
}

type reachOwner struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// cannotSee names the gap, in the words both surfaces render.
//
// **A page called Reach will be read as exhaustive**, and a hedge — "this may
// not be complete" — would be worse than nothing: it makes every entry suspect
// and names no gap. So the one thing the platform genuinely cannot report is
// said specifically, and nothing else is.
//
// Egress is unrestricted unless an install has set BlockedEgressCIDRs, naming
// an egress rule at all is what makes egress default-deny, and shared mode has
// no network policy of any kind. A control that is oversold produces confidence
// rather than safety.
const cannotSee = "Where an app connects out to is not shown or recorded: outbound " +
	"network access is open unless this install restricts it, and always open in shared mode."

// handleAppReach answers "what does this app touch".
//
// Gated `appForEdit`, which is Dependencies' gate for a stronger version of
// Dependencies' reason. A viewer keeps reading `GET /apps/{appID}/storage`
// exactly as today — that route is deliberately open to anyone who can read the
// app, and this does not narrow it. What a viewer does not get is the
// *assembly*, because the assembly is worth more than the sum: "public,
// writable storage, three credentials, and it may act as the viewer" is a
// sentence about how to attack the install, and no part of it alone is.
func (s *Server) handleAppReach(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.appForEdit(w, r)
	if !ok {
		return
	}

	resp, found, err := s.appReach(app)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !found {
		// appForEdit loaded the app a moment ago, so this is a delete that
		// crossed the read. The same 404 the gate would have given.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found", "hint": "toft apps lists the apps you can open"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// appReach is the reach read, for the route and for the app's audit file,
// which renders the same assembly. found is false for an app deleted under
// the read.
func (s *Server) appReach(app *database.App) (appReachResponse, bool, error) {
	reach, err := s.db.ReachOf(app.ID)
	if err != nil {
		return appReachResponse{}, false, err
	}
	if reach == nil {
		return appReachResponse{}, false, nil
	}

	// The three booleans, kind by kind, through the resolver appint.Check
	// itself uses. Ten reads for one app, which is the trade D8 makes
	// deliberately: correctness here, and one query on the estate.
	ints := make([]database.ReachIntegration, 0, len(appint.Known))
	for i := range appint.Known {
		kind := &appint.Known[i]
		a, err := s.db.ResolveAttachment(app.ID, kind.Name)
		if err != nil {
			return appReachResponse{}, false, err
		}
		// A kind the app is not attached to is not reach — it is a thing the
		// app cannot do. Only an attachment is a row.
		if !a.Attached {
			continue
		}
		ints = append(ints, database.ReachIntegration{
			Kind:        kind.Name,
			Destination: a.Destination.Label,
			Connected:   a.Connected,
			Attached:    true,
			Enabled:     a.Enabled,
		})
	}

	resources, err := s.db.ListStorageResources()
	if err != nil {
		return appReachResponse{}, false, err
	}
	offered, err := s.anyIntegrationOfferedToApps()
	if err != nil {
		return appReachResponse{}, false, err
	}

	owner := s.reachOwner(app)
	resp := foldAppReach(*reach, ints, len(resources) > 0, offered)
	resp.Owner = owner
	resp.Grants = grantsBesidesTheOwner(resp.Grants, owner)
	return resp, true, nil
}

// foldAppReach turns the recorded rows into the answer.
//
// Pure and separate from the handler, exactly as foldEstateTargets is, so the
// visibility substitution and the never-omitted sentence can both be exercised
// with no database and no HTTP request.
func foldAppReach(reach database.AppReach, ints []database.ReachIntegration, anyStorage, anyIntegration bool) appReachResponse {
	stored := reach.Visibility
	// One substitution, through the one function that owns it. A report that
	// disagreed with the authorizer would tell somebody their app is open to
	// the internet when the install refuses exactly that.
	reach.Visibility = database.AuthObjectOf(reach.AppID, "", reach.Visibility, reach.AnonymityWithheld, false).Visibility

	if ints == nil {
		ints = []database.ReachIntegration{}
	}
	return appReachResponse{
		AppReach:           reach,
		VisibilityWithheld: reach.Visibility != stored,
		Integrations:       ints,
		ShowStorage:        anyStorage,
		// Offered by the install, or held by this app: an attachment whose
		// connection was switched off is present and not effective, and that
		// is never reported as absent (database/reach.go). Gated on the offer
		// alone, the section vanished the moment the last connection stopped
		// offering itself (docs/bugs 2026-10-22-e2e-install, ARCH.10).
		ShowIntegrations: anyIntegration || len(ints) > 0,
		CannotSee:        cannotSee,
	}
}

// grantsBesidesTheOwner drops the grant row that restates ownership.
//
// An app is owned by a user or a group and that is a *column*, not a grant —
// but a row in object_grants naming the same principal at `owner` is written
// too, and reporting both makes the owner appear twice under a heading that
// reads "everybody who has been given a rung on this app by name". It is one
// fact and it belongs to the column, which is the one that cannot be absent.
//
// Only the owning principal is dropped, and only at the `owner` rung: somebody
// else holding an owner grant is a real second fact and stays.
func grantsBesidesTheOwner(grants []database.ReachGrant, owner *reachOwner) []database.ReachGrant {
	if owner == nil {
		return grants
	}
	out := make([]database.ReachGrant, 0, len(grants))
	for _, g := range grants {
		if g.PrincipalType == owner.Kind && g.PrincipalID == owner.ID &&
			g.Role == auth.ObjectRoleOwner {
			continue
		}
		out = append(out, g)
	}
	return out
}

// reachOwner resolves the app's owner to something renderable. An app is owned
// by a user or a group, never both and never neither.
func (s *Server) reachOwner(app *database.App) *reachOwner {
	switch {
	case app.OwnerGroupID != "":
		o := &reachOwner{Kind: "group", ID: app.OwnerGroupID}
		if g, err := s.db.GetGroup(app.OwnerGroupID); err == nil && g != nil {
			o.Name = g.Name
		}
		return o
	case app.OwnerID != "":
		o := &reachOwner{Kind: "user", ID: app.OwnerID}
		if u, err := s.db.GetUser(app.OwnerID); err == nil && u != nil {
			// Login when the provider asserted no name — callers fall back
			// rather than inventing one.
			o.Name = u.Name
			if o.Name == "" {
				o.Name = u.Login
			}
		}
		return o
	}
	return nil
}

// ------------------------------------------------------------ the estate read

// estateReachRow is one app on the estate page.
type estateReachRow struct {
	database.EstateReachRow
	VisibilityWithheld bool `json:"visibility_withheld,omitempty"`
}

// estateReachResponse is the rows and their coverage, together for the reason
// the census and the targets carry theirs: a list feels exhaustive, and a
// governance reader that answers less than it knows without saying so is the
// defect § Optional capabilities exists to prevent.
type estateReachResponse struct {
	Rows      []estateReachRow `json:"rows"`
	AppsTotal int              `json:"apps_total"`
	// AppsWithReach is how many hold anything at all beyond who may open them.
	AppsWithReach int `json:"apps_with_reach"`
	// AppsPublic is how many are effectively open to anyone — resolved from
	// the same rows the table renders, never counted separately, so the
	// sentence above the table and the table cannot disagree.
	AppsPublic          int `json:"apps_public"`
	AppsWritableStorage int `json:"apps_writable_storage"`
	// ShowStorage and ShowIntegrations are the optional-capabilities rule at
	// estate scale: a column of zeros on an install that has provisioned
	// neither is a report of refusals that never happened.
	ShowStorage      bool   `json:"show_storage"`
	ShowIntegrations bool   `json:"show_integrations"`
	CannotSee        string `json:"cannot_see"`
}

// handleEstateReach answers "what does each app hold, and which apps hold each
// thing".
//
// **Administrator only, 404, and not a deploy key.** The census's gate for the
// census's reason. `/admin/estate/targets` is the one route under /admin either
// deploy-key scope reaches, and that was argued for a scanner that needs
// addresses; a pipeline credential has no business enumerating what every app
// on the install holds. The asymmetry is deliberate and ADR §92 D12 says so,
// because it will otherwise read as an oversight.
func (s *Server) handleEstateReach(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	set, err := s.db.EstateReach(scopeFor(p))
	if err != nil {
		// Rows and coverage come from one query, so a failure takes both or
		// neither — never a table with no sentence over it.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	resources, err := s.db.ListStorageResources()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	offered, err := s.anyIntegrationOfferedToApps()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, foldEstateReach(set, len(resources) > 0, offered))
}

// anyIntegrationOfferedToApps is this install's integrations switch.
//
// **Not "a connection exists".** Every install carries an `email` connection
// from the schema baseline, because mail is configured on its own page and its
// connection is the install's relay; the row ships with `for_apps` false. A
// connection that exists *and offers itself to apps* is the switch, which is
// the invariant's own wording, and counting rows instead would offer an
// integrations section on every install that has never turned one on.
func (s *Server) anyIntegrationOfferedToApps() (bool, error) {
	connections, err := s.db.ListConnections()
	if err != nil {
		return false, err
	}
	for _, c := range connections {
		if c.ForApps {
			return true, nil
		}
	}
	return false, nil
}

// foldEstateReach resolves the effective visibility on every row and counts the
// coverage from the rows it returns.
//
// Pure, so the substitution rule can be tested without a database — the
// property foldEstateTargets was factored out for, and the same one.
func foldEstateReach(set database.EstateReachSet, anyStorage, anyIntegration bool) estateReachResponse {
	resp := estateReachResponse{
		Rows:                make([]estateReachRow, 0, len(set.Rows)),
		AppsTotal:           set.AppsTotal,
		AppsWithReach:       set.AppsWithReach,
		AppsWritableStorage: set.AppsWritableStorage,
		ShowStorage:         anyStorage,
		ShowIntegrations:    anyIntegration,
		CannotSee:           cannotSee,
	}
	for _, row := range set.Rows {
		// The same rule as foldAppReach: a held attachment keeps the column.
		if len(row.Integrations) > 0 {
			resp.ShowIntegrations = true
		}
		stored := row.Visibility
		row.Visibility = database.AuthObjectOf(row.AppID, "", row.Visibility, row.AnonymityWithheld, false).Visibility
		r := estateReachRow{EstateReachRow: row, VisibilityWithheld: row.Visibility != stored}
		if row.Visibility == database.VisibilityPublic {
			resp.AppsPublic++
		}
		resp.Rows = append(resp.Rows, r)
	}
	return resp
}
