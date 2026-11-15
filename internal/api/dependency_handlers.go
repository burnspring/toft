package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/advisory"
	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/deps"
	"toft/internal/entitlements"
	"toft/internal/telemetry"
)

// The dependency policy's read and write surface.
//
// Two rules shape all of it. **The tab, the counts and the glyph all require
// `can_edit`**: a viewer can do nothing with a finding, and an exact
// package-and-version manifest of a running internal tool is a target map —
// the same reasoning that already keeps failure events off a viewer's Logs tab. And
// **every number is scoped to the corpus it was computed against**, so
// `feed_as_of` rides on every response rather than being a footnote on one.

// dependencyStateResponse is the app's Dependencies tab, in one request.
type dependencyStateResponse struct {
	// Mode is off/warn/block, and Enforcing says whether a finding can
	// actually stop a deploy right now — which is a different question, and
	// the one that matters when the corpus has never loaded.
	Mode      string `json:"mode"`
	Enforcing bool   `json:"enforcing"`
	// BlockSeverity is the deploy gate's threshold — critical, high,
	// moderate, low or off — with BlockMalware and BlockOnlyFixable beside
	// it: what "block · enforcing" blocks *on*. Without them 13 high
	// findings deployed under "block" with nothing to say the gate was set
	// at critical (BB-030, second 2026-11-03 bash).
	BlockSeverity    string `json:"block_severity"`
	BlockMalware     bool   `json:"block_malware"`
	BlockOnlyFixable bool   `json:"block_only_fixable"`
	// Available is false when the feature is off or unlicensed, and Reason
	// says which. A refusal that says only "not available" makes somebody open
	// a ticket to discover which of four things to do.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// FeedAsOf and FeedStale scope every figure below. HasData false means no
	// corpus has ever loaded, which is not the same as a clean bill of health
	// and never renders as one.
	FeedAsOf  *time.Time `json:"feed_as_of,omitempty"`
	FeedStale bool       `json:"feed_stale"`
	HasData   bool       `json:"has_data"`

	// Scanned false is the state most of an estate is in on the first day, and
	// ScanReason distinguishes its causes: this release predates the feature (a
	// redeploy fixes it), this install does not resolve dependencies at deploy
	// (a redeploy would change nothing), or the release's whole package list is
	// in an ecosystem nothing is checked against — `not_covered`, which is R,
	// and where the inventory is shown in full beside the words — or the
	// deploy read the manifest and could not list what the build installs —
	// `unresolved`, which a redeploy of the same tree repeats.
	Scanned    bool       `json:"scanned"`
	ScanReason string     `json:"scan_reason,omitempty"`
	ScannedAt  *time.Time `json:"scanned_at,omitempty"`
	// WatchEnabled false means these figures will not move on their own, and
	// the tab says so beside the Check again button.
	WatchEnabled bool `json:"watch_enabled"`

	// Serving is the release the findings describe.
	Serving *dependencyReleaseView `json:"serving,omitempty"`
	// Refused is the most recent deploy this policy turned away, when it is
	// newer than what is serving. **What is serving is not what was refused**
	// — after a blocked deploy the app is still on release 46 while 47 was
	// turned away, and a tab that conflated them would have the developer fix
	// the wrong package.
	Refused *dependencyRefusedView `json:"refused,omitempty"`

	// Waivers are this app's live ones. Visible to the owner although only an
	// administrator can grant one: "a waiver silences the refusal, never the
	// record" is not worth much if the record is admin-only.
	Waivers []database.DependencyWaiver `json:"waivers"`
	// CanWaive is whether *this viewer* may grant one, so the tab offers the
	// button to an administrator and explains the route to everybody else.
	CanWaive bool `json:"can_waive"`
	// WaiversAvailable is false when the install's cap is 0 — waivers off, and
	// the copy must not send a developer to an administrator with no button.
	WaiversAvailable bool `json:"waivers_available"`
	// WaiverMaxDays is the cap itself, so the grant dialog can refuse an
	// out-of-range expiry in the same sentence the server would — and name the
	// number. A bounds refusal a person only meets after pressing the button
	// is a bound the form never stated.
	WaiverMaxDays int `json:"waiver_max_days"`

	// LastScan is the most recent reading, and **one object, never a list.**
	// The owner's question is *what do I fix*, and it is answered completely by
	// the latest reading; the history is longitudinal, and a longitudinal
	// reading of a finding is a statement about **response** — how long this
	// sat, whether anyone acted, how many passes went by. That is the class
	// `audit_log`'s two-reader rule and `/admin/packages`'s admin-only rule
	// already govern, so the list lives on the administrator's route.
	//
	// Absent means never checked, which is not zero findings and never renders
	// as one.
	LastScan *database.DependencyScan `json:"last_scan,omitempty"`

	// LockedByPolicy is this app being held down by the policy itself rather
	// than by an administrator. It is on this response because the tab is where
	// the finding that caused it is, and because the unlock copy has to be able
	// to say the next pass will do it again.
	LockedByPolicy bool `json:"locked_by_policy"`
	// LockOn is the threshold the install locks at — off, malware, critical or
	// high — so the tab can say what *would* happen rather than only what did.
	LockOn string `json:"lock_on"`
}

// dependencyReleaseView is one release's findings and inventory.
type dependencyReleaseView struct {
	DeploymentID string     `json:"deployment_id"`
	CommitSHA    string     `json:"commit_sha,omitempty"`
	At           *time.Time `json:"at,omitempty"`

	Findings []advisory.Finding `json:"findings"`
	// Counts is what this install has said it cares about, which is what the
	// tab count and the list glyph render — never the raw total, or a tab
	// reading 31 on every app conveys nothing but that Node exists.
	Counts dependencyCounts `json:"counts"`

	// Inventory is the least interesting half on every day when nothing is
	// wrong, so it is collapsed in the UI and counted in its own summary.
	Inventory  []deps.Dependency `json:"inventory"`
	Ecosystems []string          `json:"ecosystems,omitempty"`
	Direct     int               `json:"direct"`
}

// dependencyRefusedView is a deploy this policy turned away.
type dependencyRefusedView struct {
	DeploymentID string             `json:"deployment_id"`
	CommitSHA    string             `json:"commit_sha,omitempty"`
	At           *time.Time         `json:"at,omitempty"`
	Reason       string             `json:"reason"`
	Findings     []advisory.Finding `json:"findings"`
}

// dependencyCounts is the tone-bearing summary. Malware is its own number
// everywhere it appears, because there is no mode in which somebody should
// have to go looking for it.
type dependencyCounts struct {
	Total    int `json:"total"`
	Malware  int `json:"malware"`
	Blocking int `json:"blocking"`
	Waived   int `json:"waived"`
	// Counted is malware plus anything at or above the threshold, minus
	// waived: the one number the tab badge shows.
	Counted int `json:"counted"`
	// Unknown is advisories nobody scored, counted separately so a feed
	// degrading into unscored records is visible rather than read as an
	// improvement.
	Unknown int `json:"unknown"`
	// BySeverity drives the filter chips.
	BySeverity map[string]int `json:"by_severity"`
}

// noDependenciesReason is the third answer, for an app that has no dependencies
// and never will.
//
// The two existing reasons both describe a scan that *did not happen*:
// `never_scanned` is fixed by a redeploy and `not_scanned_by_policy` would not
// be. A static app is neither. It has no manifest in any supported ecosystem,
// so rendering it as "0 findings" would claim a scan happened and rendering it
// as "not scanned" would send somebody to turn scanning on for an app that
// would still have nothing to scan. It also means the gate is *skipped* rather
// than passed: a static app can never be blocked, because there is nothing to
// block on.
//
// `not_enumerable` is the fourth, and it is the opposite claim: this app *has*
// dependencies and the platform cannot list them.
//
// An R app with no renv.lock has a library(dplyr) on line one and no manifest
// naming a version. Rendering that as `no_dependencies` would be a status that
// is a lie — the thing this product refuses by name in another subsystem — and
// rendering it as `never_scanned` would send the owner to redeploy, which
// changes nothing. So it is its own answer, with copy that says what to add.
//
// Like `no_dependencies` it **skips** the gate rather than passing it. The hole
// that leaves is stated rather than closed: an unlocked R app can never be
// blocked by the dependency policy. No enforcement point is removed — the
// invariant's *three enforcement points, and the list is the specification*
// stays literally true — but an install that believed the gate covered
// everything it hosts would be wrong about R, and the settings copy says so.
//
// Refusing the deploy instead was the alternative. It would be the first case
// of the policy refusing a deploy for having *no manifest* rather than for a
// finding, which is a different control wearing this one's name.
//
// Returns "" for every other framework, so it composes as a precedence rather
// than as a branch at each call site.
func noDependenciesReason(app *database.App) string {
	if app.Framework == string(builder.FrameworkStatic) {
		return "no_dependencies"
	}
	return ""
}

// emptyInventoryReason is the answer when a release was scanned and nothing was
// recorded — which is not one fact but two, and R is what separates them.
//
// For every other family an empty inventory means the scan has not happened
// yet: a redeploy fixes it, and `never_scanned` says so. For R it usually means
// something else. A repository with no renv.lock deploys on purpose, installs
// nothing, and *has* dependencies all the same — a library(dplyr) on line one
// with no manifest naming a version — so there is nothing for the platform to
// enumerate and a redeploy will not change that.
//
// **An R app that does have a renv.lock never reaches here**, because its
// inventory is recorded: it is `not_covered` instead, which is the opposite
// message — the packages are listed, and no corpus checks them. The two were
// one bug apart, and the bug read as `not_enumerable` on an app whose lockfile
// the builder had just parsed.
//
// Answered here rather than in noDependenciesReason because it is a question
// about the recorded inventory, and both call sites compute the reason before
// they have read one.
func emptyInventoryReason(app *database.App) string {
	// All four R frameworks, which is the builder's own set: an R Markdown or
	// Quarto app with no renv.lock is not enumerable either, and telling its
	// owner a redeploy would fix it sends them to redeploy for nothing.
	switch app.Framework {
	case string(builder.FrameworkShiny), string(builder.FrameworkPlumber),
		string(builder.FrameworkRMarkdown), string(builder.FrameworkQuarto),
		string(builder.FrameworkBatchR):
		return "not_enumerable"
	}
	return "never_scanned"
}

// releaseScanReason is "unresolved" when the deploy that recorded deployID's
// empty inventory had a resolver give up on an ecosystem — `unresolved`, the
// scan_reason that is not `never_scanned`. The deploy did scan: it read the
// manifest and could not list what the build installs (a Python package with
// no wheel for this interpreter, a source distribution the scan will not
// build), the deploy's warnings say why, and a redeploy of the same tree
// repeats it — so telling the owner to redeploy sends them nowhere (docs/bugs
// 2026-10-28 #4). Read from the recorded verdict's gaps, which every deploy
// with the policy on writes, including one that resolved nothing.
//
// And the other half of the same record: a deploy whose verdict was written
// with nothing unread and nothing resolved had no dependencies to scan — a
// Node app with no packages, a Go module that imports only the standard
// library. That is `no_dependencies`, as for a static app, not
// `never_scanned` beside a scanned_at, whose remedy (a redeploy) changes
// nothing (docs/bugs 2026-10-28 #6). It only replaces `never_scanned`: R's
// `not_enumerable` is the reason an R app with no lockfile keeps.
func (s *Server) releaseScanReason(deployID, reason string) string {
	v, ok, err := s.db.GetDeploymentVerdict(deployID)
	if err != nil || !ok {
		return reason
	}
	for _, g := range v.Unread {
		if g.Kind == deps.GapUnresolved {
			return "unresolved"
		}
	}
	if reason == "never_scanned" && len(v.Unread) == 0 && v.Packages == 0 {
		return "no_dependencies"
	}
	return reason
}

// dependencyPolicy resolves the install's policy with this server's license
// source. One call site, so the entitlement cannot be forgotten at a new one.
func (s *Server) dependencyPolicy() (deps.Policy, error) {
	if s.deps == nil {
		// A server wired without the feature reports it absent rather than
		// panicking — the same shape as every other optional capability.
		status := s.entitlements().Entitled(entitlements.FeatureDependencyPolicy)
		return s.db.ResolveDependencyPolicy(status.Entitled, status.Reason)
	}
	r := *s.deps
	r.Ents = s.entitlements()
	return r.Policy()
}

// handleAppDependencies is the app's Dependencies tab.
func (s *Server) handleAppDependencies(w http.ResponseWriter, r *http.Request) {
	app, p, ok := s.appForEdit(w, r)
	if !ok {
		return
	}

	policy, err := s.dependencyPolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, s.dependencyState(app, policy, p))
}

// dependencyState assembles the tab. Errors degrade rather than propagate:
// this is explanatory material, and an app page must not 500 because the
// corpus was briefly unreadable.
func (s *Server) dependencyState(app *database.App, policy deps.Policy,
	p *auth.Principal) dependencyStateResponse {

	resp := dependencyStateResponse{
		Mode:             policy.Mode,
		Enforcing:        policy.Enforcing(),
		BlockSeverity:    policy.BlockSeverity,
		BlockMalware:     policy.BlockMalware,
		BlockOnlyFixable: policy.BlockOnlyFixable,
		Available:        !policy.Off(),
		FeedAsOf:         policy.FeedAsOf,
		FeedStale:        policy.Stale(),
		HasData:          policy.HasData(),
		WatchEnabled:     policy.WatchEnabled,
		Waivers:          []database.DependencyWaiver{},
		CanWaive:         p != nil && p.IsAdmin(),
		WaiversAvailable: policy.WaiverMaxDays > 0,
		WaiverMaxDays:    policy.WaiverMaxDays,
		LockedByPolicy:   app.LockedByPolicy(),
		LockOn:           policy.LockOn,
	}
	// Read even while the feature reads as unavailable: a lapsed license or a
	// policy switched off yesterday does not unsay when this app was last
	// checked, and "not checked since 12 August" is the honest answer where
	// silence would read as never.
	if scan, err := s.db.LatestDependencyScan(app.ID); err == nil {
		resp.LastScan = scan
	} else {
		log.Printf("deps: reading %s's last scan: %v", app.Slug, err)
	}
	switch {
	case !policy.Licensed:
		// **A lapsed entitlement reads as unevaluated, never as clean.**
		// Stored inventory and recorded findings are not deleted; the surface
		// says the license lapsed and the gate stops gating.
		resp.Reason = policy.LicenseReason
	case policy.Mode == database.DependencyPolicyOff:
		resp.Reason = "Dependency policy is off on this install."
	}
	if resp.Available {
		resp.ScanReason = noDependenciesReason(app)
	}
	if resp.Available && resp.ScanReason == "" && !policy.ScanOnDeploy {
		resp.ScanReason = "not_scanned_by_policy"
	}

	if waivers, err := s.db.ListDependencyWaivers(app.ID, false); err == nil {
		resp.Waivers = waivers
	} else {
		log.Printf("deps: reading %s's waivers: %v", app.Slug, err)
	}

	if !resp.Available {
		return resp
	}

	deployID, err := s.db.ServingDeploymentID(app.ID)
	if err != nil {
		log.Printf("deps: resolving %s's serving release: %v", app.Slug, err)
		return resp
	}
	if deployID != "" {
		if view := s.dependencyRelease(app, deployID, policy); view != nil {
			resp.Serving = view
			resp.Scanned = deps.ScannedPackages(policy, view.Inventory) > 0
			resp.ScannedAt = view.At
			if !resp.Scanned && len(view.Inventory) > 0 && resp.ScanReason == "" {
				resp.ScanReason = "not_covered"
				// In a covered ecosystem, and checked against nothing: its
				// corpus has never loaded here (DG-5).
				if deps.CoveredPackages(view.Inventory) > 0 {
					resp.ScanReason = "no_corpus"
				}
			}
		}
	}
	if !resp.Scanned && resp.ScanReason == "" {
		// The other cause, and the one a redeploy actually fixes — except for
		// R, where an empty inventory is usually a repository with no lockfile
		// and a redeploy changes nothing, and except for a release whose
		// resolver gave up, which a redeploy of the same tree repeats.
		resp.ScanReason = emptyInventoryReason(app)
		if deployID != "" && (resp.Serving == nil || len(resp.Serving.Inventory) == 0) {
			resp.ScanReason = s.releaseScanReason(deployID, resp.ScanReason)
		}
	}

	if refused, err := s.db.LastRefusedDeployment(app.ID); err == nil && refused != nil {
		v, ok, _ := s.db.GetDeploymentVerdict(refused.ID)
		if ok {
			view := &dependencyRefusedView{
				DeploymentID: refused.ID, CommitSHA: refused.CommitSHA,
				At: refused.FinishedAt, Reason: v.Reason, Findings: []advisory.Finding{},
			}
			if f, err := s.db.FindingsFor(refused.ID, app.ID, policy); err == nil {
				view.Findings = f
			}
			resp.Refused = view
		}
		// Nothing serves, and the one deploy there was refused at the scan:
		// that deploy's own record says why there is no inventory. A resolver
		// that gave up is `unresolved`, not `never_scanned` — a redeploy of
		// the same tree repeats it (drills on trytoft 2026-11-02, marimo).
		if deployID == "" && !resp.Scanned {
			resp.ScanReason = s.releaseScanReason(refused.ID, resp.ScanReason)
		}
	}
	return resp
}

func (s *Server) dependencyRelease(app *database.App, deployID string,
	policy deps.Policy) *dependencyReleaseView {

	inventory, err := s.db.ListDeploymentDependencies(deployID)
	if err != nil {
		log.Printf("deps: reading %s's inventory: %v", app.Slug, err)
		return nil
	}
	findings, err := s.db.FindingsFor(deployID, app.ID, policy)
	if err != nil {
		log.Printf("deps: deriving %s's findings: %v", app.Slug, err)
		findings = []advisory.Finding{}
	}

	view := &dependencyReleaseView{
		DeploymentID: deployID,
		Findings:     findings,
		Inventory:    inventory,
		Counts:       countFindings(policy, findings),
	}
	seen := map[string]bool{}
	for _, d := range inventory {
		if d.Direct {
			view.Direct++
		}
		if !seen[d.Ecosystem] {
			seen[d.Ecosystem] = true
			view.Ecosystems = append(view.Ecosystems, d.Ecosystem)
		}
	}
	if v, ok, _ := s.db.GetDeploymentVerdict(deployID); ok {
		at := v.CheckedAt
		view.At = &at
	}
	if d, err := s.db.GetDeployment(deployID); err == nil && d != nil {
		view.CommitSHA = d.CommitSHA
	}
	return view
}

// countFindings is the one place a finding set becomes numbers, so the tab
// badge, the estate view and the app list cannot disagree about what "3" means.
func countFindings(policy deps.Policy, findings []advisory.Finding) dependencyCounts {
	c := dependencyCounts{BySeverity: map[string]int{}}
	for _, f := range findings {
		c.Total++
		key := f.Severity
		if f.Malware() {
			key = "malware"
			c.Malware++
		} else if f.Severity == advisory.SeverityUnknown {
			key = "unknown"
			c.Unknown++
		}
		c.BySeverity[key]++
		if f.Waived {
			c.Waived++
			continue
		}
		if f.WouldBlock {
			c.Blocking++
		}
		if policy.Counts(f) {
			c.Counted++
		}
	}
	return c
}

// handleAppDependencyRecheck is the Check again button.
//
// **A re-check is not a re-scan of the code**, and the copy beside the button
// says so: this is inventory × advisories × waivers, which touches no registry
// and runs no resolver. Re-reading what the repository *would* install today
// requires a build, because a floating range resolves differently on a
// different day.
//
// Anyone with can_edit may press it. It accepts no risk, changes no setting
// and reveals nothing the caller could not already read, so it sits on the
// permitted side of "a key never manages authorization" — a personal key may
// call it.
func (s *Server) handleAppDependencyRecheck(w http.ResponseWriter, r *http.Request) {
	app, p, ok := s.appForEdit(w, r)
	if !ok {
		return
	}
	policy, err := s.dependencyPolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if policy.Off() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Dependency policy is off on this install, so there is nothing to re-check.", "hint": "an administrator can switch dependency policy on with PUT /api/v1/system/settings, then re-check"})
		return
	}

	deployID, err := s.db.ServingDeploymentID(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if deployID != "" && s.depSweeper != nil {
		baselinedAt, _ := s.db.DependencyBaselinedAt(app.ID)
		if _, err := s.depSweeper.SweepApp(deps.ServingRelease{
			AppID: app.ID, Slug: app.Slug, Status: app.Status,
			DeploymentID: deployID, Stopped: app.Status == database.AppStatusStopped,
			BaselinedAt: baselinedAt,
			// What the sweep needs to read a shared R app's library off disk,
			// which Check again does as well as the timer.
			Framework: app.Framework, RunningMode: app.RunningMode, ReleaseRef: app.ReleaseRef,
		}); err != nil {
			log.Printf("deps: re-checking %s: %v", app.Slug, err)
		}
	}
	// No audit row: discovering a CVE is not an administrative action.
	writeJSON(w, http.StatusOK, s.dependencyState(app, policy, p))
}

// grantWaiverRequest is what an administrator submits.
type grantWaiverRequest struct {
	AdvisoryID string `json:"advisory_id"`
	Package    string `json:"package,omitempty"`
	Reason     string `json:"reason"`
	// Days is how long, capped by the install's setting. A duration rather
	// than a date because that is what the person is actually deciding, and it
	// makes the cap expressible in the same unit as the refusal.
	Days int `json:"days"`
}

// handleGrantDependencyWaiver accepts a risk on the organization's behalf.
//
// **Administrator only**, which is the same argument that makes re-pointing an
// app's repository admin-only: this is not content editing. And denied to API
// keys entirely — granting a waiver is authorization-shaped, and a key does
// not do that.
func (s *Server) handleGrantDependencyWaiver(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return
	}

	var req grantWaiverRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	days := req.Days
	if days <= 0 {
		days = settings.DependencyWaiverMaxDays
	}

	waiver := &database.DependencyWaiver{
		AppID: app.ID, AdvisoryID: req.AdvisoryID, Package: req.Package,
		Reason: req.Reason, ExpiresAt: time.Now().UTC().AddDate(0, 0, days),
	}
	if err := s.db.GrantDependencyWaiver(waiver, settings.DependencyWaiverMaxDays,
		p.ID, requestMeta(r.Context())); err != nil {
		// Every refusal from the database layer is a sentence a person can act
		// on — a missing reason, an expiry past the cap, waivers switched off.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "correct what the sentence names — give a reason, or an expiry inside the limit — and send it again"})
		return
	}
	telemetry.RecordDependencyWaiver(r.Context(), "granted")
	writeJSON(w, http.StatusCreated, waiver)
}

// handleRevokeDependencyWaiver withdraws one. Scoped by app id in the query,
// because this route takes an id from the URL.
func (s *Server) handleRevokeDependencyWaiver(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	ok, err := s.db.RevokeDependencyWaiver(chi.URLParam(r, "appID"),
		chi.URLParam(r, "waiverID"), p.ID, requestMeta(r.Context()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/apps/{id}/dependencies lists this app's waivers with their ids"})
		return
	}
	telemetry.RecordDependencyWaiver(r.Context(), "revoked")
	w.WriteHeader(http.StatusNoContent)
}

// ── the estate ───────────────────────────────────────────────────────────

// adminDependencyResponse is Admin → Dependencies.
type adminDependencyResponse struct {
	Mode      string `json:"mode"`
	Enforcing bool   `json:"enforcing"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// Feeds is per ecosystem, because they are fetched and swapped
	// independently: npm succeeding while PyPI 404s must show as one current
	// corpus and one stale one, not as an average.
	Feeds     []advisory.FeedState `json:"feeds"`
	FeedAsOf  *time.Time           `json:"feed_as_of,omitempty"`
	FeedStale bool                 `json:"feed_stale"`
	// CanSync is false on an air-gapped install, where the answer is
	// `toft advisory-import` rather than a button that cannot work.
	CanSync bool `json:"can_sync"`
	// CorpusSize is how many advisories the install holds, and MalwareRecords
	// how many of those are about malicious packages. Both describe the
	// *corpus*, not this estate — the estate's own numbers are on the rows.
	CorpusSize     int `json:"corpus_size"`
	MalwareRecords int `json:"malware_records"`

	// Apps is one row per app, including the unscanned ones — most of an
	// estate on the first day.
	Apps []adminDependencyApp `json:"apps"`
	// Advisories is the same findings grouped the other way — the default axis
	// on the page, because the thing being triaged is usually one advisory
	// rather than one app.
	Advisories []advisoryGroup `json:"advisories"`
	// Waivers is every live waiver on the install, sorted by expiry, because
	// the aggregate is what a security review asks for.
	Waivers []database.DependencyWaiver `json:"waivers"`

	CredentialSet bool `json:"credential_set"`
	// CredentialUnreadable is a stored feed credential this install's keys
	// cannot decrypt — a lost or changed ENCRYPTION_KEY. See credentials.go.
	CredentialUnreadable bool `json:"credential_unreadable"`

	// LockOn is the threshold this install takes a serving app off the air at,
	// and LockedApps is what that has actually done. **A new row state gets a
	// new list, and this is the only place the estate-wide consequence of a
	// switch somebody flipped once is visible.**
	LockOn     string               `json:"lock_on"`
	LockedApps []adminDependencyApp `json:"locked_apps"`
}

// adminDependencyApp is one app on the estate view.
type adminDependencyApp struct {
	database.AppInventoryState
	Counts dependencyCounts `json:"counts"`
	// Serving is false for a stopped app. **A stopped app is separated, never
	// merged into the count**: it is not exposure, and folding it into the
	// headline number inflates the thing a security review reads. A *sleeping*
	// app is exposure — it wakes on the next request — and counts normally.
	Serving bool `json:"serving"`
}

func (s *Server) handleAdminDependencies(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	policy, err := s.dependencyPolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := adminDependencyResponse{
		Mode: policy.Mode, Enforcing: policy.Enforcing(), Available: !policy.Off(),
		FeedAsOf: policy.FeedAsOf, FeedStale: policy.Stale(),
		CanSync: s.advisories != nil,
		Apps:    []adminDependencyApp{}, Feeds: []advisory.FeedState{},
		Waivers:    []database.DependencyWaiver{},
		LockedApps: []adminDependencyApp{},
	}
	if settings, err := s.db.GetPlatformSettings(); err == nil {
		resp.LockOn = settings.DependencyLockOn
	}
	if !policy.Licensed {
		resp.Reason = policy.LicenseReason
	}
	if ciphertext, err := s.db.GetDependencyFeedCredential(); err == nil {
		resp.CredentialSet = ciphertext != ""
		resp.CredentialUnreadable = s.credentialUnreadable(ciphertext)
	}
	if feeds, err := s.db.FeedStates(); err == nil {
		resp.Feeds = feeds
		for _, f := range feeds {
			if f.Origin == "import" {
				// Nothing will refresh this on its own, so the page names the
				// command rather than offering a button that cannot work.
				resp.CanSync = false
			}
		}
	}
	if total, malware, err := s.db.CountAdvisories(); err == nil {
		resp.CorpusSize, resp.MalwareRecords = total, malware
	}
	if waivers, err := s.db.ListAllDependencyWaivers(); err == nil {
		resp.Waivers = waivers
	}

	states, err := s.db.ListAppInventoryStates()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// One query for every app's findings, not one per app. It is also what
	// makes the two axes below the same rows grouped differently, so "how bad
	// is this app" and "how bad is this news" cannot disagree.
	estate, err := s.db.EstateFindings(policy)
	if err != nil {
		log.Printf("deps: reading estate findings: %v", err)
	}
	findingsByApp := map[string][]advisory.Finding{}
	for _, a := range estate {
		findingsByApp[a.AppID] = a.Findings
	}

	for _, st := range states {
		row := adminDependencyApp{
			AppInventoryState: st,
			Serving:           st.Status != database.AppStatusStopped,
		}
		// A finding that is recorded is counted whether or not its ecosystem
		// is a covered one: Scanned is about what *absence* means, and the
		// deploy gate enforces a CRAN finding all the same. Skipping on it
		// showed 0 here for an app the gate refuses (docs/bugs
		// 2026-10-22-e2e-identity-automation #1).
		if st.Scanned || len(findingsByApp[st.AppID]) > 0 {
			row.Counts = countFindings(policy, findingsByApp[st.AppID])
		}
		resp.Apps = append(resp.Apps, row)
		if st.LockedAt != nil && st.LockedSource == database.AppLockSourceDependency {
			resp.LockedApps = append(resp.LockedApps, row)
		}
	}
	resp.Advisories = groupByAdvisory(policy, estate)
	writeJSON(w, http.StatusOK, resp)
}

// advisoryGroup is the estate's other axis: one advisory, the apps under it.
//
// **The default axis**, because "how bad is this news" is the question an
// administrator actually arrives with — the by-app table is already forty rows
// on day one, and the thing being triaged is usually one advisory.
type advisoryGroup struct {
	AdvisoryID string `json:"advisory_id"`
	Kind       string `json:"kind"`
	Severity   string `json:"severity"`
	Summary    string `json:"summary,omitempty"`
	URL        string `json:"url,omitempty"`
	// Apps under it, each with the package and version that matched.
	Apps []advisoryGroupApp `json:"apps"`
	// Serving is how many of them are exposure. A stopped app is not, and is
	// counted apart rather than folded into the headline.
	Serving int `json:"serving"`
	Stopped int `json:"stopped"`
	Waived  int `json:"waived"`
}

type advisoryGroupApp struct {
	AppID   string `json:"app_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Package string `json:"package"`
	Version string `json:"version"`
	FixedIn string `json:"fixed_in,omitempty"`
	Waived  bool   `json:"waived"`
	Serving bool   `json:"serving"`
	// Counted is over this install's line (policy.Counts) — what puts the pair
	// on the page's *Needs a decision* list — and WouldBlock is whether it
	// refuses the next deploy under block. Carried per pair so the page never
	// re-derives the policy.
	Counted    bool `json:"counted"`
	WouldBlock bool `json:"would_block"`
}

// groupByAdvisory is the same rows, grouped the other way. Sorted by the same
// rule every finding table uses — malware, then blocking, then severity — so a
// reader who learned the order on an app's tab does not relearn it here.
func groupByAdvisory(policy deps.Policy, estate []database.AffectedApp) []advisoryGroup {
	byID := map[string]*advisoryGroup{}
	var order []string
	for _, app := range estate {
		serving := app.Serving()
		for _, f := range app.Findings {
			g, ok := byID[f.AdvisoryID]
			if !ok {
				g = &advisoryGroup{
					AdvisoryID: f.AdvisoryID, Kind: f.Kind, Severity: f.Severity,
					Summary: f.Summary, URL: f.URL,
				}
				byID[f.AdvisoryID] = g
				order = append(order, f.AdvisoryID)
			}
			g.Apps = append(g.Apps, advisoryGroupApp{
				AppID: app.AppID, Slug: app.Slug, Name: app.Name,
				Package: f.Package, Version: f.Version, FixedIn: f.FixedIn,
				Waived: f.Waived, Serving: serving,
				Counted: policy.Counts(f), WouldBlock: f.WouldBlock,
			})
			switch {
			case f.Waived:
				g.Waived++
			case serving:
				g.Serving++
			default:
				g.Stopped++
			}
		}
	}

	out := make([]advisoryGroup, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Kind == advisory.KindMalware) != (b.Kind == advisory.KindMalware) {
			return a.Kind == advisory.KindMalware
		}
		if a.Serving != b.Serving {
			return a.Serving > b.Serving
		}
		if a.Severity != b.Severity {
			return advisory.AtOrAbove(a.Severity, b.Severity)
		}
		return a.AdvisoryID < b.AdvisoryID
	})
	return out
}

// handleDependencySearch answers "which of our apps has this".
//
// By advisory id **or by package name**, and the second is the one that
// actually gets used: the news arrives as "there's something going round in
// event-stream" hours before anybody has an identifier. It reads stored
// inventory and reaches no registry and no feed, so it works with the watch
// off, with the corpus stale, and while the policy is warn and enforcing
// nothing.
func (s *Server) handleDependencySearch(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"query": "", "apps": []database.AffectedApp{}, "serving": 0, "stopped": 0})
		return
	}
	policy, err := s.dependencyPolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	apps, err := s.db.AppsAffectedBy(query, policy)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var serving, stopped int
	for _, a := range apps {
		if a.Serving() {
			serving++
		} else {
			stopped++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query": query, "apps": apps,
		// Counted apart, never summed: a stopped app is not exposure.
		"serving": serving, "stopped": stopped,
	})
}

// handleDependencyPreview answers "what would this do", before saving.
//
// ADR §29 built the same thing for the source policy and for the same reason:
// an administrator tightening a control is entitled to know what it breaks
// *before* saving, not through forty people's deploys failing over the
// following week. It is the whole content of the warn → block step, and it
// answers the reverse question too — critical to high is the change that
// quietly triples the blocked set.
func (s *Server) handleDependencyPreview(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	var req struct {
		BlockSeverity    *string `json:"dependency_block_severity"`
		BlockMalware     *bool   `json:"dependency_block_malware"`
		BlockOnlyFixable *bool   `json:"dependency_block_only_fixable"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)

	policy, err := s.dependencyPolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// The policy being asked about is not the one in force, which is the whole
	// point: the question is what a change *would* do.
	if req.BlockSeverity != nil {
		if !database.ValidDependencySeverityThreshold(*req.BlockSeverity) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "dependency_block_severity is not a threshold this build knows", "hint": `send "dependency_block_severity" as one of off, low, moderate, high or critical`})
			return
		}
		policy.BlockSeverity = *req.BlockSeverity
	}
	if req.BlockMalware != nil {
		policy.BlockMalware = *req.BlockMalware
	}
	if req.BlockOnlyFixable != nil {
		policy.BlockOnlyFixable = *req.BlockOnlyFixable
	}

	states, err := s.db.ListAppInventoryStates()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	type blockedApp struct {
		Slug      string             `json:"slug"`
		Name      string             `json:"name"`
		OwnerName string             `json:"owner_name,omitempty"`
		Findings  []advisory.Finding `json:"findings"`
	}
	blocked := []blockedApp{}
	var unscanned int
	for _, st := range states {
		var findings []advisory.Finding
		if st.DeploymentID != "" {
			f, err := s.db.FindingsFor(st.DeploymentID, st.AppID, policy)
			if err != nil {
				continue
			}
			findings = f
		}
		if !st.Scanned && len(findings) == 0 {
			// Named rather than counted as passing: an app with no inventory
			// is *unknown* under this policy, not clean, and an administrator
			// deciding whether to switch to block needs that number. An app
			// with a recorded finding is not unknown, covered ecosystem or
			// not -- the gate refuses on it, so the preview must name it.
			unscanned++
			continue
		}
		var blocking []advisory.Finding
		for _, f := range findings {
			if f.WouldBlock {
				blocking = append(blocking, f)
			}
		}
		if len(blocking) > 0 {
			blocked = append(blocked, blockedApp{
				Slug: st.Slug, Name: st.Name, OwnerName: st.OwnerName, Findings: blocking,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"blocked":   blocked,
		"unscanned": unscanned,
		"total":     len(states),
	})
}

// handleDependencyRecheckAll is the administrator's "answer this now".
//
// The sweep runs every six hours and the feed refreshes on its own timer, and
// neither is any use on the morning a security team reads about GHSA-xxxx.
// Rate-limited on the gallery syncer's pattern by the syncer itself.
func (s *Server) handleDependencyRecheckAll(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	if s.depSweeper == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Dependency policy is not configured on this install.", "hint": "the operator who runs this install has to configure dependency policy before anything can be re-checked"})
		return
	}
	if err := s.depSweeper.SweepNow(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDependencyFeedSync refreshes the corpus.
func (s *Server) handleDependencyFeedSync(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	if s.advisories == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Dependency policy is not configured on this install.", "hint": "the operator who runs this install has to configure dependency policy before the feed can sync"})
		return
	}
	if err := s.advisories.SyncNow(r.Context()); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, advisory.ErrSyncTooSoon) {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	// The sweep straight after, so the page an administrator is looking at is
	// current with the corpus they just fetched. The admin surface offers them
	// as one button because "refresh the data" and "re-answer the question"
	// are one intent.
	if s.depSweeper != nil {
		if err := s.depSweeper.SweepNow(r.Context()); err != nil {
			log.Printf("deps: sweeping after a manual sync: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetDependencyFeedCredential stores the mirror credential.
//
// Write-only through the API: there is no read route, and the estate response
// carries a boolean rather than the value. Same treatment as the SMTP password
// and the collector headers.
func (s *Server) handleSetDependencyFeedCredential(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}
	var req struct {
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	ciphertext := ""
	if strings.TrimSpace(req.Credential) != "" {
		var err error
		if ciphertext, err = s.encryptor.Encrypt(req.Credential); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	if err := s.db.SetDependencyFeedCredential(ciphertext, p.ID, requestMeta(r.Context())); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dependencyPolicyFor is the app's half of the read-only policy block on
// GET /apps/{id} (ADR §20).
//
// It costs the same queries the tab would, and it is on the app request
// deliberately: the header and the tab strip render before anything on a tab
// has been fetched, so the count and the notice have to arrive with the app or
// they arrive late. Nil while the feature is off — a page that went on
// mentioning something the viewer cannot act on is worse than one that says
// nothing.
//
// Failures degrade rather than propagate, like everything else in that block:
// this is explanatory material on a page with plenty else to show.
func (s *Server) dependencyPolicyFor(app *database.App) *appDependencyPolicy {
	policy, err := s.dependencyPolicy()
	if err != nil {
		log.Printf("deps: resolving the policy for %s: %v", app.Slug, err)
		return nil
	}
	if policy.Off() {
		return nil
	}

	out := &appDependencyPolicy{
		Mode: policy.Mode, Enforcing: policy.Enforcing(),
		FeedAsOf: policy.FeedAsOf, FeedStale: policy.Stale(),
		Waivers: []database.DependencyWaiver{},
	}
	if out.ScanReason = noDependenciesReason(app); out.ScanReason == "" && !policy.ScanOnDeploy {
		out.ScanReason = "not_scanned_by_policy"
	}
	if waivers, err := s.db.ListDependencyWaivers(app.ID, false); err == nil {
		out.Waivers = waivers
	}

	deployID, err := s.db.ServingDeploymentID(app.ID)
	if err != nil || deployID == "" {
		if out.ScanReason == "" {
			out.ScanReason = "never_scanned"
		}
		return out
	}
	out.Release = deployID

	inventory, err := s.db.ListDeploymentDependencies(deployID)
	if err != nil || len(inventory) == 0 {
		// **Unscanned never reads as zero findings.** The counts stay at their
		// zero value and Scanned stays false, and the UI renders the state
		// rather than the number.
		if out.ScanReason == "" {
			out.ScanReason = emptyInventoryReason(app)
			if err == nil {
				out.ScanReason = s.releaseScanReason(deployID, out.ScanReason)
			}
		}
		return out
	}
	if deps.ScannedPackages(policy, inventory) == 0 {
		// Recorded, and still not scanned — the same distinction the app tab
		// makes, made here so the app list and the tab cannot disagree.
		if out.ScanReason == "" {
			out.ScanReason = "not_covered"
			if deps.CoveredPackages(inventory) > 0 {
				out.ScanReason = "no_corpus"
			}
		}
		return out
	}
	out.Scanned = true

	findings, err := s.db.FindingsFor(deployID, app.ID, policy)
	if err != nil {
		log.Printf("deps: deriving %s's findings for its policy block: %v", app.Slug, err)
		return out
	}
	out.Counts = countFindings(policy, findings)

	if v, ok, _ := s.db.GetDeploymentVerdict(deployID); ok {
		at := v.CheckedAt
		out.ScannedAt = &at
	}

	// The two notice conditions, decided here so the app page has one source
	// for them rather than re-deriving the rule in TypeScript.
	for _, f := range findings {
		if f.Malware() && !f.Waived {
			// Earns a notice in **every** mode, including warn and including
			// an install whose threshold would not block it. There is no
			// low-severity credential stealer, so there is no mode in which
			// somebody should have to go looking.
			out.MalwareServing = true
		}
	}
	if out.Counts.Blocking > 0 {
		// The next deploy would be refused. The sentence is the deploy's own,
		// so the notice above the tabs and the build log say the same thing —
		// and it is read *before* somebody presses Redeploy rather than after.
		var blocking []advisory.Finding
		for _, f := range findings {
			if f.WouldBlock && policy.Refuses(f) {
				blocking = append(blocking, f)
			}
		}
		if len(blocking) == 0 {
			return out
		}
		refusal := &deps.Refusal{
			Blocking: blocking, Threshold: policy.BlockSeverity,
			FromGit:          !builder.IsUploadedSource(app.RepoURL),
			WaiversAvailable: policy.WaiverMaxDays > 0,
		}
		out.DeploysBlocked, out.BlockedReason = true, refusal.Message()
	}
	return out
}

// The governance surface: what this estate carries, whether or not it is
// vulnerable.
//
// A sibling of handleDependencySearch rather than a flag on it. Under an
// `include_unaffected=true` parameter an app would come back with an empty
// `Findings` array meaning "has the package, no advisory", where today an empty
// array can only mean a bug — one field with two meanings, on the route a
// security review reads, to save a URL. They share the policy resolution, the
// entitlement gate and the administrator check, and nothing else.

// packageSearchResponse is one governance answer.
//
// **The coverage counts ride on every response and the interface leads with
// them.** Without that, the feature's most likely first output — "2 apps use
// requests" on an install where 61 of 84 apps have no inventory at all — is
// read as an estate-wide all-clear, and read that way in a meeting. Unscanned
// is a state with a name and never reads as zero, which is already the estate
// view's rule; a search result is where it is easiest to forget and most
// expensive to get wrong, because a result *feels* exhaustive in a way a list
// does not.
type packageSearchResponse struct {
	// Mode is "search" or "catalog", so a reader of the payload can tell which
	// question was answered without inferring it from which array is empty.
	Mode  string `json:"mode"`
	Query string `json:"query"`
	// Available is false when no dependency policy is configured. Absent, not
	// dormant: the same gate as the rest of this feature.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	Occurrences  []database.PackageOccurrence   `json:"occurrences"`
	Incomparable []database.PackageOccurrence   `json:"incomparable"`
	Packages     []database.PackageCatalogEntry `json:"packages"`

	AppsTotal   int `json:"apps_total"`
	AppsScanned int `json:"apps_scanned"`
}

// handlePackageSearch answers "which of our apps has this" for real, and with
// no `q` answers the question a search box cannot be used for.
//
// Administrator only, 404 otherwise. The existing rule is that an exact
// package-and-version manifest of a running internal tool is a target map, and
// an estate-wide manifest is that argument multiplied. Nothing is lost by it:
// an editor who wants their own app's list already has it on the app's
// Dependencies tab; what they cannot have is everybody else's.
func (s *Server) handlePackageSearch(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	policy, err := s.dependencyPolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	resp := packageSearchResponse{
		Mode: "catalog", Query: q, Available: !policy.Off(),
		Occurrences:  []database.PackageOccurrence{},
		Incomparable: []database.PackageOccurrence{},
		Packages:     []database.PackageCatalogEntry{},
	}
	if q != "" {
		resp.Mode = "search"
	}
	if !policy.Licensed {
		resp.Reason = policy.LicenseReason
	}
	if !resp.Available {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	filter, err := packageFilterFrom(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass direct and dev as true or false, and ecosystem as one this install scans, or leave them out"})
		return
	}

	// Coverage first, because it is what the rest of the answer has to be read
	// against, and a failure to compute it must not be silent.
	states, err := s.db.ListAppInventoryStates()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	for _, st := range states {
		resp.AppsTotal++
		if st.Scanned {
			resp.AppsScanned++
		}
	}

	if q == "" {
		packages, err := s.db.PackageCatalog(filter)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		resp.Packages = packages
		writeJSON(w, http.StatusOK, resp)
		return
	}

	found, err := s.db.SearchPackageOccurrences(filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	resp.Occurrences, resp.Incomparable = found.Occurrences, found.Incomparable

	// A CSV because the destination for this data is usually a spreadsheet
	// somebody else asked for, or a CMDB. **No audit_log writer**: reading the
	// inventory is not an administrative action, and a governance report that
	// fills that table with reads makes the mutations harder to find.
	if strings.Contains(r.Header.Get("Accept"), "text/csv") {
		writePackageCSV(w, found)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// packageFilterFrom reads the predicate off the query string.
//
// The version parameters are advisory.Range's own fields, named the same way,
// because they are compared by the same function a refusal is issued from.
// `version=` is rendered as the closed interval [v, v] rather than as an exact
// string, so that a version this binary cannot parse comes back as
// *incomparable* rather than quietly failing to match.
func packageFilterFrom(r *http.Request) (database.PackageFilter, error) {
	qs := r.URL.Query()
	f := database.PackageFilter{Query: strings.TrimSpace(qs.Get("q"))}

	if raw := strings.TrimSpace(qs.Get("ecosystem")); raw != "" {
		eco := advisory.NormalizeEcosystem(raw)
		if eco == "" {
			// Silently returning nothing would read as "no app has this".
			return f, errors.New("unknown ecosystem")
		}
		f.Ecosystem = eco
	}

	var err error
	if f.Direct, err = optionalBool(qs.Get("direct")); err != nil {
		return f, errors.New("direct must be true or false")
	}
	if f.Dev, err = optionalBool(qs.Get("dev")); err != nil {
		return f, errors.New("dev must be true or false")
	}

	if v := strings.TrimSpace(qs.Get("version")); v != "" {
		f.Version.Introduced, f.Version.LastAffected = v, v
		return f, nil
	}
	f.Version.Introduced = strings.TrimSpace(qs.Get("introduced"))
	f.Version.Fixed = strings.TrimSpace(qs.Get("fixed"))
	f.Version.LastAffected = strings.TrimSpace(qs.Get("last_affected"))
	return f, nil
}

// optionalBool distinguishes absent from false, which is the whole point of the
// direct and dev filters defaulting to the entire tree.
func optionalBool(raw string) (*bool, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "":
		return nil, nil
	case "true":
		yes := true
		return &yes, nil
	case "false":
		no := false
		return &no, nil
	}
	return nil, errors.New("not a boolean")
}

// writePackageCSV writes the occurrence rows, incomparable ones included and
// marked.
//
// They are in the file rather than left out of it for the reason they are in
// their own bucket on the page: a spreadsheet that silently omits the apps
// pinned to a fork is the one somebody will act on.
func writePackageCSV(w http.ResponseWriter, found database.PackageOccurrences) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="packages.csv"`)

	c := csv.NewWriter(w)
	defer c.Flush()
	_ = c.Write([]string{"app", "app_name", "status", "ecosystem", "package",
		"version", "direct", "dev", "path", "deployment", "version_comparable"})

	row := func(o database.PackageOccurrence, comparable bool) {
		// The name, package, version and path are strings an app's lockfile
		// controls — a git URL or local tag is kept verbatim as a version — so
		// they get the viewer CSV's formula neutralising.
		_ = c.Write([]string{o.Slug, csvCell(o.Name), o.Status, o.Ecosystem, csvCell(o.Package),
			csvCell(o.Version), strconv.FormatBool(o.Direct), strconv.FormatBool(o.Dev),
			csvCell(o.Path), o.DeploymentID, strconv.FormatBool(comparable)})
	}
	for _, o := range found.Occurrences {
		row(o, true)
	}
	for _, o := range found.Incomparable {
		row(o, false)
	}
}
