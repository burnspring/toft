package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"toft/internal/auth"
	"toft/internal/compliance"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/version"
)

// What this install tells an assessor.
//
// # Who may run these
//
// **Only the platform's own UI, and only an administrator using it.** All of
// these routes answer 404 to anyone who is not `system_admin`, and every one of
// them is in `apiKeyDeniedRoutes` — refused to a personal key and a deploy key
// alike. What is left after those two is a browser holding a
// `toft_*` session cookie, which is the platform's own page.
//
// That is stricter than `/admin/estate/reach`, which is already the strictest
// read on the install, and it is deliberate. The reach read is "the inventory
// of everything the estate was *given*, in one response — the single most
// useful thing on the install to a stolen token". This report is that inventory
// plus the settings document, the identity providers, the credential inventory,
// the connected git hosts and the source policy: it is the whole install
// described in one place, for somebody whose job is to look for weaknesses in
// it. A pipeline credential has no business holding it, and there is no
// automation case that needs one — an assessment is a thing a person does.
//
// `/admin/estate/targets` is the deliberate contrast and stays as it is: a
// scanner needs addresses, and addresses are not a posture.
//
// # What they do to the install
//
// Nothing. Every read below is a read, and the report is computed per request
// and stored nowhere — ADR §30's rule, kept, because what this feature argues
// with is §30's *reasoning* about point-in-time reports rather than its
// refusal to file one.

// complianceCollector reads evidence items for one request.
//
// The registry says WHAT the evidence is; this says HOW it is read. That is the
// split `checkDef` already makes, and it is why `internal/compliance` needs no
// database: the registry, the assembly and the rendering are testable without
// one.
type complianceCollector struct {
	s *Server
	r *http.Request

	// settings is read once and projected. Eight evidence items are slices of
	// the one settings document, and asking eight times for the same response
	// would be a worse artifact rather than a more thorough one.
	settings   database.PlatformSettings
	settingsOK bool

	// current is the evidence definition being collected. Held here so every
	// per-item function keeps one signature and the walk stays uniform, the way
	// checkReport carries the request for the one check that needs it.
	current compliance.Evidence
}

// complianceRegistry loads the embedded registry once.
//
// It is a fact about the build, so loading it per request would be re-parsing
// 300 KB to get the same answer. A failure here is a programming error the
// package's own tests catch, and it surfaces as a 500 rather than a panic
// because a malformed registry must not take the server down.
var complianceRegistry = sync.OnceValues(compliance.Load)

// collectors is the registry: one function per evidence id, and **the list is
// the specification**. A test asserts every item in the evidence registry has
// an entry here, so an item added to the data and forgotten in the code is a
// build failure rather than a row that quietly reports an error to an assessor.
var collectors = map[string]func(*complianceCollector) compliance.Result{
	"install_identity":      (*complianceCollector).installIdentity,
	"install_settings":      (*complianceCollector).installSettings,
	"install_checks":        (*complianceCollector).installChecks,
	"install_license":       (*complianceCollector).installLicense,
	"users_and_roles":       (*complianceCollector).users,
	"groups_and_members":    (*complianceCollector).groups,
	"apps_and_visibility":   (*complianceCollector).apps,
	"user_access_review":    (*complianceCollector).accessReview,
	"estate_reach":          (*complianceCollector).estateReach,
	"estate_targets":        (*complianceCollector).estateTargets,
	"estate_languages":      (*complianceCollector).estateLanguages,
	"processes":             (*complianceCollector).processes,
	"openapi":               (*complianceCollector).openapi,
	"audit_recent":          (*complianceCollector).auditRecent,
	"audit_by_category":     (*complianceCollector).auditByCategory,
	"audit_pruned":          (*complianceCollector).auditPruned,
	"auth_providers":        (*complianceCollector).authProviders,
	"auth_providers_public": (*complianceCollector).authProvidersPublic,
	"api_key_inventory":     (*complianceCollector).apiKeys,
	"deploy_keys":           (*complianceCollector).deployKeys,
	"source_policy":         (*complianceCollector).sourcePolicy,
	"git_connections":       (*complianceCollector).gitConnections,
	"telemetry_settings":    (*complianceCollector).telemetry,
	"integration_settings":  (*complianceCollector).integrations,
	"email_settings":        (*complianceCollector).email,
	"storage_resources":     (*complianceCollector).storage,
	"dependency_findings":   (*complianceCollector).dependencyFindings,
	"dependency_inventory":  (*complianceCollector).dependencyInventory,
	"shot_login":            (*complianceCollector).signInScreen,
	"shot_locked_app":       (*complianceCollector).lockedApps,

	// The eight settings projections. One read, eight slices.
	"setting_anonymous_access":  (*complianceCollector).fromSettings,
	"setting_listable":          (*complianceCollector).fromSettings,
	"setting_sessions":          (*complianceCollector).fromSettings,
	"setting_dormancy":          (*complianceCollector).fromSettings,
	"setting_refusal_copy":      (*complianceCollector).fromSettings,
	"setting_retention":         (*complianceCollector).fromSettings,
	"setting_app_output":        (*complianceCollector).fromSettings,
	"setting_api_keys":          (*complianceCollector).fromSettings,
	"setting_dependency_policy": (*complianceCollector).fromSettings,
}

func (c *complianceCollector) Collect(_ context.Context, item compliance.Evidence) compliance.Result {
	fn, ok := collectors[item.ID]
	if !ok {
		// Unreachable while the completeness test passes, and reported rather
		// than skipped if it ever is: a missing row would make the coverage
		// count a lie.
		return compliance.Errorf("no collector is registered for this item")
	}
	c.current = item
	return fn(c)
}

func (s *Server) newComplianceCollector(r *http.Request) *complianceCollector {
	c := &complianceCollector{s: s, r: r}
	if st, err := s.db.GetPlatformSettings(); err == nil {
		c.settings, c.settingsOK = st, true
	}
	return c
}

// projectSettings picks the named fields out of the settings document.
//
// Through the document's own JSON shape rather than by reflection over field
// names, because the registry names settings by their **wire** names — the
// spelling an administrator sees on the API and in the docs — and those are the
// json tags, not the Go identifiers. A projection keyed on Go names would drift
// the first time a field was renamed without its tag changing.
func projectSettings(st database.PlatformSettings, fields []string) map[string]any {
	raw, err := json.Marshal(st)
	if err != nil {
		return map[string]any{}
	}
	var all map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		// A field the document does not carry is recorded as absent rather than
		// omitted: an assessor reading a four-field item that renders three
		// rows cannot tell which one went missing.
		if v, ok := all[f]; ok {
			out[f] = v
		} else {
			out[f] = nil
		}
	}
	return out
}

// ---------------------------------------------------------------- the reads

func (c *complianceCollector) installIdentity() compliance.Result {
	host, _ := os.Hostname()
	return compliance.OK(map[string]any{
		"version":  version.Version,
		"revision": version.Revision(),
		"hostname": host,
		"os":       runtime.GOOS + "/" + runtime.GOARCH,
	})
}

func (c *complianceCollector) installSettings() compliance.Result {
	if !c.settingsOK {
		return compliance.Errorf("the settings document could not be read")
	}
	// The whole document, so an assessor asking "what else is configured" does
	// not have to trust the report's selection of it.
	return compliance.OK(c.settings)
}

// fromSettings projects the fields this item names out of the one settings
// read. The registry owns which fields belong to which item, so a new setting
// joins the document by being named in the data rather than in code here.
func (c *complianceCollector) fromSettings() compliance.Result {
	if !c.settingsOK {
		return compliance.Errorf("the settings document could not be read")
	}
	fields := c.current.Source.Fields
	if len(fields) == 0 {
		return compliance.Errorf("this item names no settings fields")
	}
	return compliance.OK(projectSettings(c.settings, fields))
}

func (c *complianceCollector) installChecks() compliance.Result {
	rows := c.s.runChecks(c.r)
	by := map[string]int{}
	for _, ck := range rows {
		by[ck.Status]++
	}
	return compliance.OK(map[string]any{"checks": rows, "by_status": by})
}

func (c *complianceCollector) installLicense() compliance.Result {
	ent := c.s.entitlements()
	return compliance.OK(map[string]any{
		"tier": string(ent.Tier()),
	})
}

func (c *complianceCollector) users() compliance.Result {
	rows, err := c.s.db.ListUsersWithActivity()
	if err != nil {
		return compliance.Errorf("listing accounts: " + err.Error())
	}
	by := map[string]int{}
	for _, u := range rows {
		by[u.SystemRole]++
	}
	return compliance.OK(map[string]any{"accounts": rows, "by_role": by, "total": len(rows)})
}

func (c *complianceCollector) groups() compliance.Result {
	rows, err := c.s.db.ListGroups()
	if err != nil {
		return compliance.Errorf("listing groups: " + err.Error())
	}
	return compliance.OK(map[string]any{"groups": rows, "total": len(rows)})
}

func (c *complianceCollector) apps() compliance.Result {
	rows, err := c.s.db.ListAllApps()
	if err != nil {
		return compliance.Errorf("listing apps: " + err.Error())
	}
	// Visibility is counted from the EFFECTIVE value, so an install that has
	// withdrawn anonymous access reports what it actually serves rather than
	// what the column says.
	by := map[string]int{}
	for _, a := range rows {
		obj := database.AuthObjectOf(a.ID, a.OwnerID, a.Visibility, a.PublicWithheld, false)
		by[obj.Visibility]++
	}
	return compliance.OK(map[string]any{"total": len(rows), "by_visibility": by})
}

func (c *complianceCollector) accessReview() compliance.Result {
	users, err := c.s.db.ListUsersWithActivity()
	if err != nil {
		return compliance.Errorf("listing accounts: " + err.Error())
	}
	out := make(map[string]any, len(users))
	for _, u := range users {
		grants, err := c.s.db.ListUserAccessGrants(u.ID, nil)
		if err != nil {
			out[u.ID] = map[string]any{"login": u.Login, "error": err.Error()}
			continue
		}
		// Keyed by id: a login is whatever the IdP asserted and is not unique,
		// so keyed by login two accounts collapsed into one entry.
		out[u.ID] = map[string]any{"login": u.Login, "role": u.SystemRole, "grants": len(grants)}
	}
	return compliance.OK(out)
}

func (c *complianceCollector) estateReach() compliance.Result {
	set, err := c.s.db.EstateReach(database.AppScope{All: true})
	if err != nil {
		return compliance.Errorf("reading estate reach: " + err.Error())
	}
	return compliance.OK(set)
}

func (c *complianceCollector) estateTargets() compliance.Result {
	set, err := c.s.db.EstateTargets(database.AppScope{All: true})
	if err != nil {
		return compliance.Errorf("reading estate targets: " + err.Error())
	}
	return compliance.OK(set)
}

func (c *complianceCollector) estateLanguages() compliance.Result {
	census, err := c.s.db.EstateByLanguage(database.AppScope{All: true})
	if err != nil {
		return compliance.Errorf("reading the language census: " + err.Error())
	}
	return compliance.OK(census)
}

func (c *complianceCollector) processes() compliance.Result {
	metrics, err := c.s.rts.List()
	if err != nil {
		// A backend that did not answer is a degraded read, not a failure: the
		// same choice the processes page makes, and for the same reason.
		return compliance.OK(map[string]any{
			"workloads": len(metrics),
			"degraded":  true,
			"note":      "one execution backend did not answer",
		})
	}
	return compliance.OK(map[string]any{"workloads": len(metrics)})
}

func (c *complianceCollector) openapi() compliance.Result {
	doc, err := OpenAPIDocument(c.s.cfg.BaseURL)
	if err != nil {
		return compliance.Errorf("describing the served routes: " + err.Error())
	}
	ops := 0
	for _, methods := range doc.Paths {
		ops += len(methods)
	}
	// Counts, not the document. The served surface is admin-only in its own
	// right, and reproducing it whole inside a second admin-only document adds
	// nothing an assessor cannot get from the route that owns it.
	return compliance.OK(map[string]any{"paths": len(doc.Paths), "operations": ops})
}

func (c *complianceCollector) auditRecent() compliance.Result {
	n, err := c.s.db.CountAuditEntries()
	if err != nil {
		return compliance.Errorf("counting audit entries: " + err.Error())
	}
	rows, err := c.s.db.ListActivity(database.ActivityFilter{Limit: 1})
	if err != nil {
		return compliance.Errorf("reading the activity feed: " + err.Error())
	}
	out := map[string]any{"audit_entries": n}
	if len(rows) > 0 {
		// The newest row's action and time, never the rows themselves. The feed
		// names people and objects, and this document is read by somebody
		// outside the organization.
		out["newest_action"] = rows[0].Action
		out["newest_at"] = rows[0].OccurredAt
	}
	return compliance.OK(out)
}

// auditPruned is the access record's newest retention pass: the cutoff it
// held the record to and the setting behind it. Absent rather than an error
// when there is none — an install that keeps everything has never pruned.
func (c *complianceCollector) auditPruned() compliance.Result {
	rows, err := c.s.db.ListActivity(database.ActivityFilter{Actions: []string{database.ActionAuditPruned}, Limit: 1})
	if err != nil {
		return compliance.Errorf("reading the activity feed: " + err.Error())
	}
	if len(rows) == 0 {
		return compliance.Absent("no retention pass has removed anything; access_record_retention_days keeps everything at 0")
	}
	return compliance.OK(map[string]any{"at": rows[0].OccurredAt, "pass": json.RawMessage(rows[0].Detail)})
}

func (c *complianceCollector) auditByCategory() compliance.Result {
	out := map[string]any{}
	for _, cat := range []string{"deploys", "access", "signins", "settings", "system"} {
		// The same translation the audit handler does: `system` means the whole
		// events half, every other category is a family of action strings
		// defined server-side. Read from activityCategories rather than
		// restated, so a category added there appears here.
		f := database.ActivityFilter{Limit: 1}
		if cat == "system" {
			f.Source = "event"
		} else {
			f.Actions = activityCategories[cat]
		}
		rows, err := c.s.db.ListActivity(f)
		if err != nil {
			out[cat] = map[string]any{"error": err.Error()}
			continue
		}
		// Whether the category is populated, not what is in it. That is the
		// question this item exists to answer: each family of auditable event
		// is being recorded on THIS install rather than merely supported by the
		// product.
		entry := map[string]any{"populated": len(rows) > 0}
		if len(rows) > 0 {
			entry["newest_at"] = rows[0].OccurredAt
		}
		out[cat] = entry
	}
	return compliance.OK(out)
}

// The two identity-provider items read what their evidence registry says they
// read: the same merged list the admin page and the login page render —
// environment-configured providers and rows, with Ready() deciding what the
// login page offers. Reading the rows alone reported no providers on an
// install whose only SSO was the operator's environment, and counted a row
// enabled with no secret that the login page hides.
func (c *complianceCollector) authProviders() compliance.Result {
	providers, err := c.s.authProviders()
	if err != nil {
		return compliance.Errorf("listing identity providers: " + err.Error())
	}
	// Names and kinds. Secrets are write-only in the store and never reach
	// here, and this document must not be the one place they appear.
	out := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		out = append(out, map[string]any{
			"id": p.ID, "kind": p.Kind, "name": p.DisplayName, "enabled": p.Enabled,
			"ready": p.Ready(),
		})
	}
	return compliance.OK(map[string]any{"providers": out, "total": len(out)})
}

func (c *complianceCollector) authProvidersPublic() compliance.Result {
	providers, err := c.s.authProviders()
	if err != nil {
		return compliance.Errorf("listing identity providers: " + err.Error())
	}
	ready := 0
	for _, p := range providers {
		if p.Ready() {
			ready++
		}
	}
	return compliance.OK(map[string]any{
		"ways_in_offered": ready,
		"note": "what the sign-in page renders; a provider configured but not ready " +
			"does not appear there",
	})
}

func (c *complianceCollector) apiKeys() compliance.Result {
	rows, err := c.s.db.ListAllUserAPIKeys(0)
	if err != nil {
		return compliance.Errorf("reading the key inventory: " + err.Error())
	}
	active := 0
	now := time.Now().UTC()
	for i := range rows {
		if rows[i].State(now) == "active" {
			active++
		}
	}
	return compliance.OK(map[string]any{"personal_keys": len(rows), "active": active})
}

func (c *complianceCollector) deployKeys() compliance.Result {
	rows, err := c.s.db.ListDeployKeys()
	if err != nil {
		return compliance.Errorf("listing deploy keys: " + err.Error())
	}
	return compliance.OK(map[string]any{"deploy_keys": len(rows)})
}

func (c *complianceCollector) sourcePolicy() compliance.Result {
	pol, err := c.s.db.ResolveSourcePolicy()
	if err != nil {
		return compliance.Errorf("reading the source policy: " + err.Error())
	}
	return compliance.OK(pol)
}

func (c *complianceCollector) gitConnections() compliance.Result {
	rows, err := c.s.db.ListGitConnections()
	if err != nil {
		return compliance.Errorf("listing git connections: " + err.Error())
	}
	out := make([]map[string]any, 0, len(rows))
	for _, g := range rows {
		// The clone credential lives behind these and never appears here.
		out = append(out, map[string]any{
			"id": g.ID, "kind": g.Kind, "name": g.DisplayName, "enabled": g.Enabled,
		})
	}
	return compliance.OK(map[string]any{"connections": out, "total": len(out)})
}

func (c *complianceCollector) telemetry() compliance.Result {
	t, err := c.s.db.GetTelemetrySettings()
	if err != nil {
		return compliance.Errorf("reading telemetry settings: " + err.Error())
	}
	if !t.Enabled {
		return compliance.Absent("telemetry export is not configured on this install")
	}
	// The endpoint, never the credential. The collector credential never
	// reaches a workload and it does not reach this document either.
	return compliance.OK(map[string]any{"enabled": true, "endpoint": t.Endpoint})
}

func (c *complianceCollector) integrations() compliance.Result {
	set, err := c.s.db.GetIntegrationSettings()
	if err != nil {
		return compliance.Errorf("reading integration settings: " + err.Error())
	}
	return compliance.OK(set)
}

func (c *complianceCollector) email() compliance.Result {
	e, err := c.s.db.GetEmailSettings()
	if err != nil {
		return compliance.Errorf("reading mail settings: " + err.Error())
	}
	if !e.Enabled {
		return compliance.Absent("no mail relay is configured on this install")
	}
	return compliance.OK(map[string]any{
		"enabled": true, "transport": e.Transport, "host": e.Host, "port": e.Port,
	})
}

func (c *complianceCollector) storage() compliance.Result {
	if !c.s.storageOffered() {
		// The capability this install does not have. Absent, never an error —
		// an install with no isolated backend is a correct install.
		return compliance.Absent("this install offers no durable storage backend")
	}
	rows, err := c.s.db.ListStorageResources()
	if err != nil {
		return compliance.Errorf("listing storage resources: " + err.Error())
	}
	return compliance.OK(map[string]any{"resources": len(rows)})
}

func (c *complianceCollector) dependencyFindings() compliance.Result {
	lic := c.s.entitlements().Entitled(entitlements.FeatureDependencyPolicy)
	pol, err := c.s.db.ResolveDependencyPolicy(lic.Entitled, lic.Reason)
	if err != nil {
		return compliance.Errorf("reading the dependency policy: " + err.Error())
	}
	affected, err := c.s.db.EstateFindings(pol)
	if err != nil {
		return compliance.Errorf("reading estate findings: " + err.Error())
	}
	return compliance.OK(map[string]any{"apps_affected": len(affected)})
}

func (c *complianceCollector) dependencyInventory() compliance.Result {
	rows, err := c.s.db.PackageCatalog(database.PackageFilter{})
	if err != nil {
		return compliance.Errorf("reading the package inventory: " + err.Error())
	}
	return compliance.OK(map[string]any{"packages": len(rows)})
}

// signInScreen is a LINK, not a photograph. The platform serves this report, so
// the live page is better evidence than a picture of it and cannot go stale
// between the reading and the assessment.
func (c *complianceCollector) signInScreen() compliance.Result {
	return compliance.OK(map[string]any{
		"url":  strings.TrimRight(c.s.cfg.BaseURL, "/") + "/login",
		"note": "served to a visitor who has proved nothing; open it signed out",
	})
}

func (c *complianceCollector) lockedApps() compliance.Result {
	apps, err := c.s.db.ListAllApps()
	if err != nil {
		return compliance.Errorf("listing apps: " + err.Error())
	}
	var locked []string
	for _, a := range apps {
		if a.LockedAt != nil {
			locked = append(locked, a.Slug)
		}
	}
	if len(locked) == 0 {
		return compliance.Absent("no app on this install is locked")
	}
	sort.Strings(locked)
	return compliance.OK(map[string]any{"locked": locked})
}

// ------------------------------------------------------------- the handlers

// complianceGate is the one place these routes decide who may read them, so a
// fourth route added later cannot answer differently.
//
// 404 rather than 403 on the house rule: what an install is capable of is
// infrastructure detail, and a refusal that confirms the surface exists tells a
// caller something about the install. API keys never reach here at all — the
// guard in keyroutes.go refuses them before the handler — so what survives this
// is an administrator holding a session cookie.
func (s *Server) complianceGate(w http.ResponseWriter, r *http.Request) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return false
	}
	return true
}

// GET /api/v1/admin/compliance — the framework and the coverage.
func (s *Server) handleCompliance(w http.ResponseWriter, r *http.Request) {
	if !s.complianceGate(w, r) {
		return
	}
	reg, err := complianceRegistry()
	if err != nil {
		log.Printf("compliance: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"catalog":       reg.Catalog.Name,
		"oscal_version": reg.Catalog.OSCALVersion,
		"families":      reg.Catalog.Families,
		"coverage":      reg.Coverage(),
	})
}

// GET /api/v1/admin/compliance/controls — the control document as data.
//
// It reads no install state at all, which is the property D2 protects: what
// Toft does about a requirement is a fact about the build, identical on
// every install of a version.
func (s *Server) handleComplianceControls(w http.ResponseWriter, r *http.Request) {
	if !s.complianceGate(w, r) {
		return
	}
	reg, err := complianceRegistry()
	if err != nil {
		log.Printf("compliance: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"catalog":       reg.Catalog.Name,
		"oscal_version": reg.Catalog.OSCALVersion,
		"families":      reg.Catalog.Families,
		"requirements":  reg.Requirements,
		"evidence":      reg.Evidence,
		"coverage":      reg.Coverage(),
	})
}

// GET /api/v1/admin/compliance/report — the live reading.
//
// Computed per request and filed nowhere.
func (s *Server) handleComplianceReport(w http.ResponseWriter, r *http.Request) {
	if !s.complianceGate(w, r) {
		return
	}
	reg, err := complianceRegistry()
	if err != nil {
		log.Printf("compliance: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	rep := compliance.Assemble(r.Context(), reg, s.newComplianceCollector(r))
	host, _ := os.Hostname()
	rep.Version, rep.Hostname = version.Version, host
	// The install, not only the host: the hostname is the pod or container
	// and changes with it, so two exports of one install named two subjects.
	rep.InstallID, _ = s.db.InstallID()
	if p := auth.GetPrincipal(r.Context()); p != nil {
		// The principal carries an id rather than a login, and the id is what
		// the audit log records too — so an exported report names its author in
		// the same spelling the install's own record does.
		rep.CollectedBy = p.ID
	}
	writeJSON(w, http.StatusOK, rep)
}

// GET /api/v1/admin/compliance/report/export — the same reading, frozen.
//
// **The platform generates it and hands it over; it keeps no copy.** No table,
// no retention dial, no sweep — ADR §30's refusal to file a report stands
// unchanged. What this feature argues with is §30's *reasoning* that a
// point-in-time report has no value, and that argument needs an artifact rather
// than a row: an assessor asks what was true on the assessment date, and a page
// that is always "now" cannot answer it. The customer archives what they are
// given, which is where an assessment artifact belongs anyway.
//
// The digest is tamper evidence over the reading as delivered: a reader with the
// file and nothing else recomputes SHA-256 over its `report` member and
// compares. It claims nothing more than that, and in particular it is not a way
// to tell two assessments apart — it covers the collection timestamp, so two
// exports of an unchanged install differ.
func (s *Server) handleComplianceExport(w http.ResponseWriter, r *http.Request) {
	if !s.complianceGate(w, r) {
		return
	}
	reg, err := complianceRegistry()
	if err != nil {
		log.Printf("compliance: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	rep := compliance.Assemble(r.Context(), reg, s.newComplianceCollector(r))
	host, _ := os.Hostname()
	rep.Version, rep.Hostname = version.Version, host
	// The install, not only the host: the hostname is the pod or container
	// and changes with it, so two exports of one install named two subjects.
	rep.InstallID, _ = s.db.InstallID()
	if p := auth.GetPrincipal(r.Context()); p != nil {
		rep.CollectedBy = p.ID
	}

	export, err := compliance.Freeze(rep)
	if err != nil {
		// Marshalled before anything is written, so a document that cannot be
		// produced is a 500 and not a 200 with half a file in it. The report
		// route has the same hazard and a test for it: writeJSON discards
		// Encode's error, and a truncated evidence bundle is worse than none.
		log.Printf("compliance export: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+compliance.ExportFilename(rep)+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(export)
}

// GET /api/v1/admin/compliance/controls.pdf — the control document as a file.
//
// Buffered whole before the first byte goes out, for the export's reason: a
// document that fails halfway through rendering must be a 500 rather than a
// truncated PDF, because a truncated PDF opens.
func (s *Server) handleComplianceControlsPDF(w http.ResponseWriter, r *http.Request) {
	if !s.complianceGate(w, r) {
		return
	}
	reg, err := complianceRegistry()
	if err != nil {
		log.Printf("compliance: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	body, err := compliance.ControlDocumentPDF(reg, compliance.PDFOptions{
		Version:   version.Version,
		Revision:  version.Revision(),
		Generated: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("compliance pdf: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Named for the build rather than for the day, because that is what it
	// describes: two installs on the same version hold the same document, and
	// dating it would invite somebody to file it as an assessment.
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition",
		`attachment; filename="toft-800-171r3-controls-`+
			compliance.SafeFilenamePart(version.Version)+`.pdf"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
