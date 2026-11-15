package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/appint"
	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The app-facing surface: what a deployed app may ask about itself.
//
// Every route here is self-referential — /api/v1/app/…, never
// /api/v1/apps/{id}/… — and that is not cosmetic. The standing invariant
// "anything taking an ID from the URL scopes its query by that ID" exists
// because forgetting it is an IDOR, and a surface with no ID in it cannot have
// the bug. The token is the app.
//
// What is deliberately absent: deploy, rollback, restart, stop, this app's
// logs, its environment, its API keys, anything about another app, anything
// admin. An app that could redeploy itself is a loop; an app that could read
// its own secrets gains nothing (it already has them) and hands a template to
// whoever later wants to widen this surface.

// handleAppSelf is how app code asks *may I* before trying.
//
// Load-bearing rather than a convenience: an app that wants to email a report
// should be able to render "email is not enabled for this app; ask an
// administrator" on a screen at startup, instead of discovering it at 2am from
// a refusal in a log nobody is reading.
func (s *Server) handleAppSelf(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	app, err := s.db.GetApp(token.Token.AppID)
	if err != nil || app == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	caps, err := appint.Capabilities(s.db, app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Viewer identity joins the same list, and off-by-default is what makes
	// advertising it load-bearing rather than a nicety: the ordinary state of a
	// new app is off, so an app renders "viewer identity is not enabled for this
	// app — ask an administrator" at startup instead of meeting a 403 at 2am.
	// The email half is derived from visibility rather than granted, so an app
	// on a public install is *told* the address will not come rather than
	// finding a null in the response.
	caps = append(caps, appint.ViewerCapabilities(app.ViewerIdentity, app.Visibility, app.Framework)...)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":   app.ID,
		"slug": app.Slug,
		"name": app.Name,
		"mode": string(s.callerMode(app, token.Token)),
		// The mount prefix, from the one function that computes it — the same
		// value the deploy writes into BASE_PATH, rather than a second
		// construction of "/apps/" + slug that could disagree with it.
		//
		// **Absent is not the same as empty here, and that is why it is sent
		// at all.** Every SDK exposes this as `base_path` and reads it out of
		// this payload; when the key was missing they each fell back to "",
		// reported it as fact, and an app that built its links from the
		// accessor emitted root-absolute URLs — which the proxy answers with a
		// 404 on every link, with nothing in any log saying why. "" is also
		// the legitimate answer on an install that gives each app its own
		// origin, so no app could tell the two apart.
		//
		// The serving release's path, not one computed from the slug: it is
		// what this code was built and started under, which is what its links
		// have to agree with — and "" for real on an app at the root of its
		// own hostname.
		"base_path": builder.ServingBasePath(app),
		// The same read-only block GET /apps/{id} carries, and for the same
		// reason it exists there: a platform decision about one app is visible
		// on that app.
		//
		// The dependency half is deliberately omitted: false, not canEdit,
		// because the caller here is the app's own token rather than a person.
		// An exact package-and-version manifest is a target map, and app code
		// is the last thing that should be able to read one back out of the
		// platform.
		"policy": s.appPolicy(app, false),
		// Attached or not, never *why* not. The reason is an administrator's
		// business — the same line /system/settings draws by 404ing to everyone
		// who is not one.
		"capabilities": caps,
	})
}

// appURLPath is the route handleAppURL answers, named once because the run
// token's list (jobSendPaths) has to name it too.
const appURLPath = "/api/v1/app/url"

// handleAppURL is where this app is reached, now: the whole address a visitor
// is sent to, and the base path inside it. Every SDK's `url(path)` joins a path
// onto it to build a link that leaves the app — an email, a Slack post, a PDF
// — which a relative link cannot be, and which a request's own Host cannot
// build either: the gateway hands the app an internal one, and a job has no
// request at all.
//
// **A route of its own, not a field on /app/self**, for two reasons. A job
// reaches it with its run token (jobSendPaths) and must not reach /app/self,
// whose capabilities and policy are the app's business rather than a run's
// (ADR §108). And the SDKs cache /app/self for the life of the process, while
// this is read fresh: the address moves without a deploy — an administrator
// changes APPS_BASE_URL, or gives the app a hostname of its own — and a link
// built from a value cached at start is the stale link this exists to stop.
//
// From builder.PublicURL over the apps origin, the one construction every
// other caller uses: the same value GET /apps/{id} reports as `public_url`, so
// an app's links and the console's agree. The base path is the serving
// release's, which is the rule PublicURL itself keys on — the two halves
// cannot describe different releases mid-move.
//
// No address is a refusal, not an empty string: "" joined to a path is the
// relative link this route exists to replace, and the caller would send it.
func (s *Server) handleAppURL(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	app, err := s.db.GetApp(token.Token.AppID)
	if err != nil || app == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	public := builder.PublicURL(s.cfg.AppsBaseURL, app)
	if public == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":     "this install has no address for its apps, so no absolute link can be built",
			"code":      "no_public_address",
			"retryable": false,
			"hint":      "an administrator sets APPS_BASE_URL (or BASE_URL) in the install's configuration",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"public_url": public,
		"base_path":  builder.ServingBasePath(app),
	})
}

// callerMode is the execution mode of the workload *asking*, which is not
// always what the app row says is live.
//
// The row is written at the flip, and the flip is the last step of a deploy —
// after the readiness probe. The probe is usually the new workload's first
// request, so an app that reads its own mode at startup reads it inside the one
// window where the row still describes the release being replaced, and on a
// first deploy that row's mode column is empty, which RunningModeOrShared
// reads as `shared`. An isolated app was therefore told `shared` until somebody
// restarted it — the conservative-looking wrong answer, which is what kept it
// quiet: an app branching on this to decide whether durable storage is there
// takes the wrong branch for the life of the process and nothing downstream
// looks broken.
//
// So the answer comes from the deployment the caller's own workload belongs to,
// where the mode was recorded at step 0 and is true for that release's lifetime
// (database.Deployment.ExecutionMode). It cannot be stale, and it stays right
// for an old release still serving beside a new one in the other mode.
//
// Falling back to the app row covers a token whose workload names no deployment
// we can find — nothing else could answer, and it is the reading every caller
// of RunningModeOrShared already takes.
//
// This is the same trap releaseForWorkload documents one file over: reading a
// column the flip owns, from a request that arrives before the flip.
func (s *Server) callerMode(app *database.App, token database.AppToken) runtime.Mode {
	if d := s.deploymentForWorkload(app, token.Workload); d != nil {
		if m, err := runtime.ParseMode(d.ExecutionMode); err == nil {
			return m
		}
	}
	return app.RunningModeOrShared()
}

// handleAppViewer answers who is looking at this app and what they may do here.
//
// The reversal of a deliberate refusal, and the shape it takes is the argument.
// The app *asks*, relaying the act-for blob it already receives, rather than the
// proxy injecting signed identity headers on every proxied request. Four reasons
// in the PRD, and the first is the one that decides it: a header is a snapshot,
// and a Streamlit app holds its handshake headers for hours. Injection would
// hand an app a role that a demotion stopped being true eight hours ago — and
// unlike the act-for path there is no second check, because on this route the
// app is the enforcer. Asking is also what makes the capability absent unless
// wanted: an app that never calls this learns nothing, and a person's address
// never enters an app's request log by default.
//
// Three answers, and the difference between two of them is load-bearing:
//
//	204  there is no viewer — anonymous, or somebody who may no longer read this
//	403  there is a viewer and this app may not be told who
//	200  here they are
//
// Collapsing 403 into 204 would make an app render its signed-out state to a
// signed-in person and give its author no reason to suspect a setting.
func (s *Server) handleAppViewer(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	app, err := s.db.GetApp(token.Token.AppID)
	if err != nil || app == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The permissive resolver, because on this one route *nobody in particular*
	// is an answer rather than a failure. Everywhere else an anonymous blob is
	// refused by name; here it is a 204, which is what this route has always
	// said about a visitor it cannot describe.
	viewer, present, ok := s.appViewerAny(w, r, app.ID)
	if !ok {
		return
	}
	// Legitimately absent or legitimately nobody: a viewer holding a personal
	// toftu_ key — a person, but not a session, so no blob is minted for them —
	// and an anonymous visitor to a public app, who now relays a blob that says
	// so. None of these is an error, and none of them is somebody to describe.
	//
	// Deliberately the same 204 for both. For this route's purposes they mean
	// the same thing — *show this person nothing personal* — and a distinct code
	// would only invite an app to treat one of them as a viewer.
	if !present || viewer.Anonymous {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Off is the ordinary state of a new app, so this refusal names the setting
	// and who can change it. The standing rule on this surface: a refusal that
	// names the remedy is a fixable situation, and a bare 403 is not.
	//
	// After the anonymous answer, never before it: the 403 means *there is a
	// viewer and this app may not be told who*, and an anonymous visitor to a
	// public app is nobody whatever the setting. Checked first, it turned every
	// public page that asks who is looking into a 500 for the whole public.
	if len(appint.ViewerCapabilities(app.ViewerIdentity, app.Visibility, app.Framework)) == 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "viewer identity is not enabled for this app — an administrator can enable it on the app's settings",
			"code":  "viewer_identity_disabled",
			"hint":  "ask an administrator to turn on viewer identity in this app's settings"})
		return
	}

	p, err := s.principalForViewer(viewer)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// CanRead is the gate, and EffectiveObjectRole is only the description. The
	// difference is the whole administrator case: the ladder has no system_admin
	// bypass by design, so an administrator with no grant on a private app
	// resolves to "none" there while Authorize said yes here. Testing the ladder
	// instead of the gate would answer *no viewer* to the one person with full
	// authority over the app.
	obj := objectFromApp(app)
	if !auth.CanRead(p, obj, s.db) {
		// A grant revoked while a Streamlit socket stayed open lands here. The
		// app asks who is there and is told nobody, and renders its signed-out
		// state — which is the case that justifies reading the role live at all.
		// Conflated with anonymous on purpose: for the app's purposes both mean
		// *show this person nothing personal*, and a distinct code would only
		// invite an app to treat one of them as a viewer.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	user, err := s.db.GetUser(p.ID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	groups, err := s.db.ListUserGroups(p.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Counted, not audited. Which apps are reading viewer identity is a fair
	// governance question; a row per call is a database write on every page load
	// of every app, which the standing rule forbids.
	s.usage.RecordCapability(app.ID, appint.CapViewerIdentity)

	// A list holder cannot reach a workload, so cannot reach this route; the
	// error is a 500 rather than a softened role, for the reason GrantedRole
	// states.
	grantedRole, err := auth.GrantedRole(auth.EffectiveObjectRole(p, obj, s.db))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := map[string]interface{}{
		"id":   user.ID,
		"name": user.Name,
		// The three surviving values, mirroring the words the Sharing tab
		// granted from. is_admin is separate rather than a fourth role value:
		// an administrator who is *also* the owner is an ordinary case, and one
		// enum would force a precedence rule that loses one of the two facts.
		// It is also the load-bearing half — Authorize hands back "owner" on any
		// object for an administrator, so without this an app gating a
		// destructive action on role == "owner" would grant it to every
		// Toft administrator with no way to refuse.
		"role":     grantedRole,
		"is_admin": p.IsAdmin(),
		"groups":   viewerGroups(groups),
	}
	// Every group the person is in, not only the ones holding a grant here: a
	// domain rule belongs on `finance` or `approvers`, and those are usually not
	// the groups that explain this app's access. A filtered list would answer
	// "not in finance" about somebody who is, which is the worst shape an
	// authorization input can have.
	if user.AvatarURL != "" {
		out["avatar_url"] = user.AvatarURL
	}
	// Withheld on a public app, whose visitors did not consent to a mailing list
	// by visiting. Everything else still answers in full — the identity question
	// an app legitimately asked is *who is this and what may they do*, which the
	// address is no part of. The absence is advertised as its own capability on
	// /app/self rather than met as a null at runtime.
	if app.Visibility != database.VisibilityPublic {
		out["email"] = user.Email
	}
	writeJSON(w, http.StatusOK, out)
}

// viewerGroups is the wire shape of a viewer's groups: id and name, never the
// row. A name because an app gating on membership of *finance* cannot use an
// opaque id it has no way to map, and there is no route that would let it. The
// invariant that groups are local rows and never directory claims is untouched —
// these are Toft groups.
func viewerGroups(groups []database.Group) []map[string]string {
	out := make([]map[string]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, map[string]string{"id": g.ID, "name": g.Name})
	}
	return out
}

// handleAppStorage lists the durable resources mapped to this app.
//
// The same shape as the TOFT_STORAGE environment variable, so an SDK can
// resolve by name from either and get the same answer. Resolution is by name
// and never by path: that is what lets an administrator move a resource without
// breaking every app that uses it.
func (s *Server) handleAppSelfStorage(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	mounts, err := s.db.AppMounts(token.Token.AppID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Rendered by the function DescribeMounts renders, from the paths the
	// caller's own backend resolves, so this answer and TOFT_STORAGE cannot
	// disagree. They did: a hand-built copy here dropped the folder, and in
	// shared mode reported the administrator's path where the workload was
	// handed a link under apps/<id>/storage.
	if s.rts != nil && len(mounts) > 0 {
		if app, err := s.db.GetApp(token.Token.AppID); err == nil && app != nil {
			mounts = s.rts.ResolvedMounts(s.callerMode(app, token.Token), app.ID, mounts)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"storage": runtime.MountDescriptors(mounts)})
}

// handleAppSend reads back one send this app made.
//
// Scoped to the calling app, and there is deliberately no listing. Reading back
// an id the app minted, about content the app wrote, tells it nothing it did
// not already know; browsing the ledger would teach an app which addresses its
// administrator finds interesting.
func (s *Server) handleAppSend(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	send, err := s.db.GetAppSend(token.Token.AppID, chi.URLParam(r, "sendID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if send == nil {
		// 404 rather than 403 for a send belonging to another app: the standing
		// rule for an object this caller may not read.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no such send", "code": "not_found", "hint": "use the id the notify or deliver call returned for this app's own send"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":   send.ID,
		"kind": send.Kind,
		// The published name for the same fact. Both SDKs read `integration`
		// and the SDK contract is that an app written against one release keeps
		// running on the next, so it is emitted forever beside the name the
		// catalog uses. This is the one place the old word survives.
		"integration":  send.Kind,
		"action":       send.Action,
		"status":       send.Status,
		"error":        send.Error,
		"created_at":   send.CreatedAt,
		"completed_at": send.CompletedAt,
	})
}
