package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/compliance"
	"toft/internal/database"
	"toft/internal/version"
)

// The app's audit file: one app, described to an assessor, for its editors
// and administrators (ADR §93's per-app counterpart).
//
// Each section is collected by its key and carries the gate of the route it
// mirrors (compliance.AppFileSections), for the caller and for the
// credential: a section whose route would refuse this caller is refused with
// the reason, and its value is never read, so a refused section cannot leak
// through a serialization bug. The two places an editor sees what no single
// route gives them are named: the grant attribution in `access`, and who
// started each build in `releases` (database.AppAttributions).
//
// **Deploy keys are refused**, by absence from both deploy-key lists: a
// deploy key is owner on every app, so the pack per app would rebuild the
// estate reach read one request at a time — the read every key is refused.

// appFileCollector reads one app's sections for one caller.
type appFileCollector struct {
	s   *Server
	app *database.App
	p   *auth.Principal
}

// gateRefusal is why this caller may not have a section, or "".
func (c *appFileCollector) gateRefusal(sec compliance.AppFileSection) string {
	obj := objectFromApp(c.app)
	switch sec.Gate {
	case compliance.GateNone:
		return ""
	case compliance.GateManageGrants:
		if !auth.Authorize(c.p, auth.ActionManageGrants, obj, c.s.db).Allow {
			return "needs the app's owner"
		}
	case compliance.GateAdmin:
		if c.p == nil || !c.p.IsAdmin() {
			return "needs an administrator"
		}
	case compliance.GateRead:
		if !auth.Authorize(c.p, auth.ActionRead, obj, c.s.db).Allow {
			return "needs read access to the app"
		}
	}
	// The credential half, from the key list itself: a section whose route
	// a personal key may not call is refused to a personal key, and a later
	// narrowing of the list narrows the pack with it.
	if c.p != nil && c.p.IsAPIKey() && !apiKeyAllowedRoutes[sec.Mirrors] {
		return "not available to an API key"
	}
	return ""
}

// appFileReads is every section's collector, keyed as the list is.
var appFileReads = map[string]func(*appFileCollector) compliance.Result{
	"identity":               (*appFileCollector).identity,
	"source":                 (*appFileCollector).source,
	"access":                 (*appFileCollector).access,
	"security_headers":       (*appFileCollector).securityHeaders,
	"embedding_and_identity": (*appFileCollector).embedding,
	"releases":               (*appFileCollector).releases,
	"dependencies":           (*appFileCollector).dependencies,
	"reach":                  (*appFileCollector).reach,
	"credential_age":         (*appFileCollector).credentialAge,
	"logs":                   (*appFileCollector).logs,
	"viewers":                (*appFileCollector).viewers,
	"activity":               (*appFileCollector).activity,
	"deletion":               (*appFileCollector).deletion,
	"cannot_see":             (*appFileCollector).cannotSee,
}

func (c *appFileCollector) CollectSection(_ context.Context, sec compliance.AppFileSection) compliance.Result {
	if why := c.gateRefusal(sec); why != "" {
		return compliance.Refused(why)
	}
	read, ok := appFileReads[sec.Key]
	if !ok {
		return compliance.Errorf("no collector for this section")
	}
	return read(c)
}

func (c *appFileCollector) identity() compliance.Result {
	a := c.app
	out := map[string]any{
		"id": a.ID, "slug": a.Slug, "name": a.Name, "created_at": a.CreatedAt,
		"repo_url": a.RepoURL, "repo_subdir": a.RepoSubdir, "branch": a.Branch,
		"gallery_item": a.GalleryItemID, "framework": a.Framework,
	}
	if owner := c.s.reachOwner(a); owner != nil {
		out["owner"] = owner
	}
	attrs, err := c.s.db.AppAttributions(a.ID)
	if err != nil {
		return compliance.Errorf("reading who created it: " + err.Error())
	}
	out["created_by"] = "not recorded"
	for _, at := range attrs {
		if at.Action == "app.create" {
			out["created_by"] = actorOf(at)
			break
		}
	}
	return compliance.OK(out)
}

func actorOf(a database.Attribution) map[string]string {
	m := map[string]string{"type": a.ActorType}
	if a.ActorID != "" {
		m["id"] = a.ActorID
	}
	if a.ActorLogin != "" {
		m["login"] = a.ActorLogin
	}
	if a.APIKeyID != "" {
		m["api_key_id"] = a.APIKeyID
	}
	return m
}

func (c *appFileCollector) source() compliance.Result {
	allowed, reason := c.s.appSourcePolicy(c.app)
	out := map[string]any{"admitted_by_source_policy": allowed}
	if reason != "" {
		out["refusal"] = reason
	}
	switch {
	case builder.IsUploadedSource(c.app.RepoURL):
		out["kind"] = "upload"
	case c.app.GalleryItemID != "":
		out["kind"] = "gallery"
	default:
		out["kind"] = "git"
	}
	return compliance.OK(out)
}

// The two labels a NULL granted_by gets. Never blank, and only what the row
// supports. A group's owner grant is NULL from two writers that leave the same
// row -- the ownership backfill and a deploy key creating the app -- so its
// label names both rather than guessing one: it once said "backfilled" of
// every such grant, a deploy key's included.
const (
	grantedByGroupOwner = "no person recorded — written when this install backfilled ownership, or by a deploy key that created the app"
	grantedByUnknown    = "no person recorded — created by a deploy key, granted by an account since deleted, or written by the platform"
)

func (c *appFileCollector) access() compliance.Result {
	// The effective visibility, with the anonymity suppressors applied: the
	// one fact the gateway enforces.
	obj := c.app.AuthObject()
	grants, err := c.s.db.GrantsOf(c.app.ID)
	if err != nil {
		return compliance.Errorf("reading the grants: " + err.Error())
	}
	rows := make([]map[string]any, 0, len(grants))
	for _, g := range grants {
		row := map[string]any{
			"principal_type": g.PrincipalType, "principal_id": g.PrincipalID, "name": g.DisplayName,
			"role": g.Role, "current_role_since": g.RoleSince,
		}
		switch {
		case g.GrantedBy != "":
			by := map[string]any{"id": g.GrantedBy, "login": g.GrantedByLogin}
			if g.GrantedByLocked {
				by["locked"] = true
			}
			row["granted_by"] = by
		case g.PrincipalType == database.PrincipalTypeGroup && g.Role == database.ObjectRoleOwner:
			row["granted_by"] = grantedByGroupOwner
		default:
			row["granted_by"] = grantedByUnknown
		}
		if g.PrincipalType == database.PrincipalTypeGroup {
			row["members"] = map[string]int{"manual": g.ManualMembers, "directory": g.DirectoryMembers}
		}
		rows = append(rows, row)
	}
	out := map[string]any{
		"visibility": obj.Visibility, "listable": c.app.Listable,
		"locked": c.app.LockedAt != nil, "grants": rows,
		"note": "This lists the grants; reviewing them is the organization's to do.",
	}
	if obj.Visibility != c.app.Visibility {
		out["visibility_withheld"] = c.app.Visibility
	}
	if c.app.LockedAt != nil {
		out["lock_source"] = c.app.LockSource()
	}
	return compliance.OK(out)
}

func (c *appFileCollector) securityHeaders() compliance.Result {
	out := map[string]any{
		"baseline": c.s.db.AppSecurityHeaders(),
		"note":     "nosniff and Referrer-Policy are added where the app sends neither; the app's own header wins",
	}
	appsURL := c.s.cfg.AppsBaseURL
	if appsURL == "" {
		appsURL = c.s.cfg.BaseURL
	}
	if !hstsWithheld(c.s.cfg) && isHTTPSURL(appsURL) {
		out["hsts"] = hstsValue
	} else {
		out["hsts"] = "not sent"
	}
	return compliance.OK(out)
}

func (c *appFileCollector) embedding() compliance.Result {
	return compliance.OK(map[string]any{
		"frame_ancestors": c.s.frameAncestorsPolicy(c.app),
		"viewer_identity": c.app.ViewerIdentity,
	})
}

// notRecorded is a build with no attribution row.
const notRecorded = "this install holds no record of who started this build — it was built before builds were attributed, or the record has passed this install's retention"

func (c *appFileCollector) releases() compliance.Result {
	ds, err := c.s.db.ListDeployments(c.app.ID)
	if err != nil {
		return compliance.Errorf("reading the deployments: " + err.Error())
	}
	attrs, err := c.s.db.AppAttributions(c.app.ID)
	if err != nil {
		return compliance.Errorf("reading who started them: " + err.Error())
	}
	started := map[string]database.Attribution{}
	promoted := map[string][]database.Attribution{}
	for _, a := range attrs {
		switch a.Action {
		case database.ActionAppDeploy:
			started[a.DeploymentID] = a
		case "app.release_promoted":
			promoted[a.DeploymentID] = append(promoted[a.DeploymentID], a)
		}
	}
	live, _ := c.s.db.ServingDeploymentID(c.app.ID)
	rows := make([]map[string]any, 0, len(ds))
	for _, d := range ds {
		row := map[string]any{
			"id": d.ID, "commit": d.CommitSHA, "branch": d.Branch, "triggered_by": d.TriggeredBy,
			"status": d.Status, "started_at": d.StartedAt, "finished_at": d.FinishedAt,
			"runtime": d.RuntimeLanguage + " " + d.RuntimeVersion,
			"live":    live != "" && d.ID == live,
		}
		if a, ok := started[d.ID]; ok {
			row["started_by"] = actorOf(a)
		} else {
			row["started_by"] = notRecorded
		}
		if ps := promoted[d.ID]; len(ps) > 0 {
			var made []map[string]any
			for _, p := range ps {
				made = append(made, map[string]any{"by": actorOf(p), "at": p.At})
			}
			row["made_current_again"] = made
		}
		rows = append(rows, row)
	}
	return compliance.OK(rows)
}

func (c *appFileCollector) dependencies() compliance.Result {
	policy, err := c.s.dependencyPolicy()
	if err != nil {
		return compliance.Errorf("reading the dependency policy: " + err.Error())
	}
	state := c.s.dependencyState(c.app, policy, c.p)
	out := map[string]any{"state": state}
	if live, err := c.s.db.ServingDeploymentID(c.app.ID); err == nil && live != "" {
		if sum, ok := c.s.sbomDigest(c.app, live); ok {
			out["live_release_sbom_sha256"] = sum
		}
		if v, ok, err := c.s.db.GetDeploymentVerdict(live); err == nil && ok {
			if v.Unread == nil {
				out["unread"] = "built before this install recorded what resolution missed"
			} else {
				out["unread"] = v.Unread
			}
		}
	}
	return compliance.OK(out)
}

// sbomDigest is the SHA-256 of a release's SBOM as the route serves it.
func (s *Server) sbomDigest(app *database.App, deployID string) (string, bool) {
	d, err := s.db.GetDeployment(deployID)
	if err != nil || d == nil || d.AppID != app.ID {
		return "", false
	}
	inv, err := s.db.ListDeploymentDependencies(d.ID)
	if err != nil {
		return "", false
	}
	v, has, err := s.db.GetDeploymentVerdict(d.ID)
	if err != nil {
		return "", false
	}
	body, err := json.MarshalIndent(renderSBOM(sbomRecord{
		AppID: app.ID, Deployment: d, Inventory: inv, Verdict: v, HasVerdict: has,
		Static:        noDependenciesReason(app) == "no_dependencies",
		NotEnumerable: emptyInventoryReason(app) == "not_enumerable",
	}), "", "  ")
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), true
}

func (c *appFileCollector) reach() compliance.Result {
	resp, found, err := c.s.appReach(c.app)
	if err != nil {
		return compliance.Errorf("reading reach: " + err.Error())
	}
	if !found {
		return compliance.Errorf("the app was deleted while it was read")
	}
	return compliance.OK(resp)
}

func (c *appFileCollector) credentialAge() compliance.Result {
	creds, err := c.s.db.AppCredentialAttachments(c.app.ID)
	if err != nil {
		return compliance.Errorf("reading the credentials: " + err.Error())
	}
	vars, err := c.s.db.GetEnvVars(c.app.ID)
	if err != nil {
		return compliance.Errorf("reading the environment: " + err.Error())
	}
	env := make([]map[string]any, 0, len(vars))
	for _, v := range vars {
		env = append(env, map[string]any{"key": v.Key, "sensitive": v.Sensitive})
	}
	return compliance.OK(map[string]any{
		"credentials": creds,
		"credentials_note": "last_changed moves on any edit, rotation or rename; this install " +
			"does not record when one value was last rotated",
		"env":      env,
		"env_note": "this install does not record when an environment value was set",
	})
}

func (c *appFileCollector) logs() compliance.Result {
	out := map[string]any{}
	if lc := c.s.logCopyPolicy(c.app); lc != nil {
		out["copy"] = lc
	} else {
		out["copy"] = map[string]string{"mode": "not copied to a collector"}
	}
	out["on_disk"] = "each release's output files are rotated at 4 MiB and removed with the release and with the app"
	return compliance.OK(out)
}

func (c *appFileCollector) viewers() compliance.Result {
	until := time.Now().UTC().Truncate(24 * time.Hour)
	since := until.AddDate(0, 0, -(usageMaxDays - 1))
	rows, err := c.s.db.GetAppUsageViewers(c.app.ID, since, until)
	if err != nil {
		return compliance.Errorf("reading the viewers: " + err.Error())
	}
	out := make([]usageViewer, 0, len(rows))
	for _, v := range rows {
		out = append(out, viewerOf(v))
	}
	result := map[string]any{"since": since.Format(dayLayout), "until": until.Format(dayLayout), "viewers": out}
	// The two counts with nobody in them: page loads, and — when the window
	// holds any sketch — the anonymous browsers, one union across it.
	if totals, err := c.s.db.GetAppUsageTotals(c.app.ID, usageMaxDays); err == nil {
		result["page_loads"] = totals.PageLoads
	}
	if n, err := c.s.db.AppAudienceTotal(c.app.ID, usageMaxDays); err == nil && n != nil {
		result["anonymous_browsers"] = *n
	}
	return compliance.OK(result)
}

func (c *appFileCollector) activity() compliance.Result {
	rows, err := c.s.db.ListActivity(database.ActivityFilter{AppID: c.app.ID, Limit: database.MaxActivityLimit})
	if err != nil {
		return compliance.Errorf("reading the activity: " + err.Error())
	}
	out := map[string]any{"entries": rows}
	if started, err := c.s.db.AppScopeStartedAt(); err == nil && !started.IsZero() {
		out["complete_from"] = started
	}
	// One page, newest first. A full page means older rows exist that are not
	// here, and saying nothing let complete_from name a date long before the
	// oldest row shown — so the cut is stated, and where the rest is.
	if len(rows) == database.MaxActivityLimit {
		out["truncated"] = true
		out["shown"] = len(rows)
		out["oldest_shown"] = rows[len(rows)-1].OccurredAt
		out["note"] = "Only the newest rows are shown; the full history is GET /api/v1/admin/audit?app=" + c.app.ID + ", paged."
	}
	return compliance.OK(out)
}

func (c *appFileCollector) deletion() compliance.Result {
	return compliance.OK(deletionFor(c.s.db, c.app.ID))
}

func (c *appFileCollector) cannotSee() compliance.Result {
	return compliance.OK(cannotSee)
}

// newAppFile reads one app's pack for this request's caller.
func (s *Server) newAppFile(r *http.Request, app *database.App, p *auth.Principal) *compliance.AppFile {
	f := compliance.AssembleAppFile(r.Context(), &appFileCollector{s: s, app: app, p: p})
	f.App = compliance.AppFileSubject{ID: app.ID, Slug: app.Slug, Name: app.Name}
	host, _ := os.Hostname()
	installID, _ := s.db.InstallID()
	f.Install = compliance.AppFileInstall{ID: installID, Version: version.Version, Hostname: host}
	if p != nil {
		f.Producer = compliance.AppFileProducer{PrincipalID: p.ID, Credential: "session"}
		if p.IsAPIKey() {
			f.Producer.Credential, f.Producer.KeyID = "personal_key", p.APIKeyID
		}
	}
	return f
}

// handleAppAuditFile — GET /apps/{appID}/audit-file. The pack, read now.
// Computed per request, filed nowhere, and reading it writes nothing.
func (s *Server) handleAppAuditFile(w http.ResponseWriter, r *http.Request) {
	app, p, ok := s.appForEdit(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.newAppFile(r, app, p))
}

// handleAppAuditFileExport — GET /apps/{appID}/audit-file/export. The same
// reading, frozen and digested, marshalled whole before the first byte so a
// failure is a 500 and never half a file.
func (s *Server) handleAppAuditFileExport(w http.ResponseWriter, r *http.Request) {
	app, p, ok := s.appForEdit(w, r)
	if !ok {
		return
	}
	f := s.newAppFile(r, app, p)
	body, err := compliance.FreezeAppFile(f)
	if err != nil {
		log.Printf("audit file: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+compliance.AppFileFilename(f)+`"`)
	_, _ = w.Write(body)
}

// AppFileCoverage is the reach lists' guard, one step removed: every reach
// source and every table reach excludes maps to a section of the pack, or to
// an exclusion with its reason. A new app-keyed table already has to land in
// one of the reach lists (TestReachSourcesAreTheSpecification), so it has to
// land here too.
var AppFileCoverage = map[string]string{
	// The reach sources.
	"grants":            "access",
	"embedding":         "embedding_and_identity",
	"viewer_identity":   "embedding_and_identity",
	"credentials":       "credential_age",
	"env":               "credential_age",
	"storage":           "reach",
	"integrations":      "reach",
	"data":              "reach",
	"report_recipients": "reach",
	// The tables reach excludes.
	"audit_log":              "activity",
	"app_usage_daily":        "viewers",
	"app_audience_daily":     "viewers",
	"deployments":            "releases",
	"dependency_scans":       "dependencies",
	"dependency_waivers":     "dependencies",
	"app_reachability":       "access",
	"integration_placements": "deletion",
	"app_events":             "activity",

	"api_key_usage_daily":        "excluded: what a key did belongs to the key's own page",
	"app_capability_usage_daily": "excluded: a counter of sends, with nobody's name in it",
	"app_consumption_daily":      "excluded: what the app cost is the install's accounting",
	"app_consumption_intervals":  "excluded: the intervals behind the cost",
	"app_integration_sends":      "excluded: the send ledger is the integration log's",
	"app_platform_log":           "excluded: the platform's narration is the Logs tab's",
	"job_runs":                   "excluded: what a job did is the Automation tab's",
	"report_deliveries":          "excluded: a report's deliveries are its schedule's",
	"job_definitions":            "excluded: what the app's manifest declares, read on the Automation tab",
	"app_restart_schedules":      "excluded: the platform restarting the app on a timetable, read on the Automation tab",
	"app_tags":                   "excluded: an administrator's labels, not a fact about access or code",
	"app_tokens":                 "excluded: the workload's own outbound credential, held by no person",
}
