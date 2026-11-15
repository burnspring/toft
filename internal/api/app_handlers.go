package api

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/deploysteps"
	"toft/internal/entitlements"
	"toft/internal/gallery"
	"toft/internal/gitconn"
	"toft/internal/runner"
	"toft/internal/runtime"
)

var slugRe = regexp.MustCompile(`[^a-z0-9-]`)

type CreateAppRequest struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	// RepoSubdir is the folder of the repository the app is built from, for a
	// repository that holds more than one app. Optional: absent is the root.
	RepoSubdir string `json:"repo_subdir"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	// SlugExact makes a taken slug a refusal (409 name_taken) rather than a
	// renamed app. A caller sets it when the slug is a statement — typed with
	// --app or read from toft.toml — so a retried create cannot claim
	// `<slug>-2` beside the app it already made. Absent keeps the old
	// behaviour, which the SPA and older clients rely on.
	SlugExact bool              `json:"slug_exact"`
	EnvVars   map[string]string `json:"env_vars"`
	// EnvSensitive marks which env_vars keys are write-only secrets. Keys not
	// listed are visible configuration.
	EnvSensitive map[string]bool `json:"env_sensitive"`
	// GitToken lets a private repo clone on the very first deploy. Stored
	// encrypted, never returned.
	GitToken string `json:"git_token"`
	// GitConnectionID points the clone at a connected git host instead, so the
	// app carries no credential at all.
	//
	// **Refused when the principal is a key**, on this route and on PATCH.
	// Attaching an app to the install's clone credential is a credential
	// decision, and a deploy key belongs to nobody while being owner on every
	// app — it would be the one principal that could grant itself the
	// organization's git access. Taking POST /apps off the deploy-key map
	// instead would break every pipeline that creates its own app, to fix a
	// field those pipelines do not send.
	GitConnectionID string `json:"git_connection_id"`
	// ExecutionMode is where this app runs, decided at birth rather than by a
	// promotion afterwards. Optional: absent means the install's default.
	//
	// Deliberately the same wire name and the same values as the PATCH field,
	// because a client that can promote an app and a client that can create one
	// should not have to know two spellings of one idea. Administrator-only —
	// see resolveCreateMode.
	ExecutionMode string `json:"execution_mode"`

	// OwnerGroupID makes the new app a group's rather than a person's.
	//
	// Optional for a person and **required for a deploy key**, which is the
	// joint between the two halves of ADR §51: the credential creates the app,
	// the group owns it, and neither is a person. A session or an toftu_ key that
	// names no group still creates a personally-owned app, on an install that
	// has no groups at all — nobody is made to create a group to deploy.
	OwnerGroupID string `json:"owner_group_id"`

	// Visibility is the audience the app is born with — public, authenticated
	// or private — through the same gates as the upload create's form value
	// (resolveVisibilityTo). It used to be dropped without a word, so a
	// pipeline's "visibility":"public" got 201 and a private app (BB-96).
	Visibility string `json:"visibility"`
}

// envVarResponse is one env var as the API returns it: the value only when the
// var is visible — a sensitive value never leaves the server once stored.
// Unset says a sensitive row holds the empty string, so "not set" and
// "stored" read differently; it tells an editor nothing else about the value.
//
// A pointer, so a visible variable set to "" reads `value: ""`: absent is the
// sensitive row's shape, and an empty visible value read absent was a
// withheld secret to anybody reading the response.
type envVarResponse struct {
	Key       string  `json:"key"`
	Value     *string `json:"value,omitempty"`
	Sensitive bool    `json:"sensitive"`
	Unset     bool    `json:"unset,omitempty"`
}

type UpdateAppRequest struct {
	Name       *string `json:"name,omitempty"`
	Slug       *string `json:"slug,omitempty"`
	Visibility *string `json:"visibility,omitempty"`
	// Listable is the owner's half of the install-wide `list` rung: whether this
	// app appears in the listing of somebody who holds nothing on it. Gated by
	// the same permission as Visibility — the question is who may see this app,
	// and "may know it exists" is the weaker half of it, not a third rule.
	//
	// Inert above `private`, and accepted there anyway: an owner who un-lists a
	// private app, publishes it and comes back gets their own answer back, and
	// refusing the write would be the platform deciding it knows better than a
	// value nobody can currently observe.
	Listable *bool `json:"listable,omitempty"`
	// FrameAncestors replaces the whole list of origins allowed to embed this
	// app. A pointer to a slice because `[]` and *absent* are different
	// requests — one says "nobody but this install", the other says nothing —
	// and a bare slice cannot tell them apart.
	FrameAncestors *[]string `json:"frame_ancestors,omitempty"`
	// EnvVars and EnvSensitive are read only to be refused (use_env_route):
	// an existing app's environment changes through PATCH /apps/{id}/env, and
	// two write routes with different rules for a blank value would be two
	// meanings of one field. Creation still takes them.
	EnvVars      json.RawMessage `json:"env_vars,omitempty"`
	EnvSensitive json.RawMessage `json:"env_sensitive,omitempty"`
	Branch       *string         `json:"branch,omitempty"`
	// RepoSubdir moves the app to another folder of its repository; "" is the
	// root. The same tier as Branch: it changes what is built, not where from.
	RepoSubdir *string `json:"repo_subdir,omitempty"`
	AutoDeploy *bool   `json:"auto_deploy,omitempty"`
	// RepoURL re-points the app at a different repository. Administrator-only,
	// and it exists for one situation: an install that restricts where apps may
	// come from has apps that no longer comply, and without this the only way to
	// move one onto an approved mirror would be to delete it and recreate it —
	// losing its environment variables, storage, schedules, grants and history.
	// On an install that already has apps, which is every install that would
	// want a source policy, that makes the switch unadoptable.
	RepoURL *string `json:"repo_url,omitempty"`
	// GitToken is owner-only; "" clears the stored credential.
	GitToken *string `json:"git_token,omitempty"`
	// GitConnectionID re-points the app at a connection, or at none with "".
	// Refused to key principals for the reason stated on the create request.
	GitConnectionID *string `json:"git_connection_id,omitempty"`
	// ExecutionMode promotes an app to isolated mode, or returns it to shared.
	// A setting rather than a redeploy, so a density decision can be corrected
	// cheaply — and admin-only, because it spends the operator's elastic
	// capacity rather than the app owner's.
	ExecutionMode *string `json:"execution_mode,omitempty"`
	// AutoDeleteReleases and ReleasesKept are the owner's release retention.
	// A number above the platform maximum is stored rather than refused; the
	// settings page reports the gap. See database.ResolveRetention.
	AutoDeleteReleases *bool `json:"auto_delete_releases,omitempty"`
	ReleasesKept       *int  `json:"releases_kept,omitempty"`
	// NeverSleep exempts the app from idle sleep. Admin-only, like execution
	// mode: idle sleep is how an operator reclaims a shared footprint, so an
	// app cannot excuse itself from it.
	NeverSleep *bool `json:"never_sleep,omitempty"`
	// Hostname is a DNS name of the app's own, and HostnameEnabled whether it
	// is served. Admin-only, through the never_sleep gate: which origin an app
	// lives on is a decision about the install's browser boundary, not about
	// the app. "" clears the name (and switches it off). Either change that
	// moves the app — on, off, or a new name while on — queues a rebuild of
	// the running code at its new address.
	Hostname        *string `json:"hostname,omitempty"`
	HostnameEnabled *bool   `json:"hostname_enabled,omitempty"`
	// This app's own memory ceiling, in whichever unit the administrator is
	// thinking in — exactly one may be sent. Admin-only, and for a sharper
	// reason than NeverSleep: a ceiling an app can raise for itself is not a
	// ceiling. 0 hands the number back to the install's dial, never "uncapped".
	MemoryLimitMB *int     `json:"memory_limit_mb,omitempty"`
	MemoryLimitGB *float64 `json:"memory_limit_gb,omitempty"`
	// This app's own CPU limit, by the memory limit's rules: one spelling,
	// admin-only, 0 the install's. An amount of CPU time, never a set of cores.
	CPULimitMillicores *int     `json:"cpu_limit_millicores,omitempty"`
	CPULimitCores      *float64 `json:"cpu_limit_cores,omitempty"`
	// ViewerIdentity lets this app ask who is looking at it. Admin-only to
	// write, like NeverSleep and for a sharper reason: naming a person to a
	// program is a grant, and the person named is neither the app nor its
	// owner, so an owner asking for their own app to learn who its visitors are
	// is precisely the request that wants a second person in it.
	//
	// Readable by anyone who may edit the app, though — see appPolicy. An owner
	// must be able to see *why* their toft.who() call is failing without filing a
	// ticket to find out.
	ViewerIdentity *bool `json:"viewer_identity,omitempty"`
	// NotifyFailures is whether this app's owners and editors are emailed
	// when it breaks. Deliberately *not* admin-only, unlike NeverSleep: an
	// exemption from idle sleep spends the operator's shared footprint, while
	// this spends nothing and belongs to the app's own people. The
	// ActionEditContent check on the whole request is the gate.
	//
	// It silences mail and never the record — every failure is still written to
	// app_events and still shown on the app's Logs tab.
	NotifyFailures *bool `json:"notify_failures,omitempty"`
}

// pendingDeployWindow is how recent a pending deployment must be to block a
// new one. Pending rows older than this were orphaned by a restart (the build
// queue is in-memory) and must not wedge the app.
//
// It moved to internal/builder when a fourth thing gained the right to enqueue
// a build — a notebook's render schedule, which runs beside the queue rather
// than in the API — and the alias stays here so the five call sites in this
// file keep reading as one sentence. It is a property of the queue, and the
// queue is the builder's.
const pendingDeployWindow = builder.PendingDeployWindow

// pageRoles answers from a map already read for the whole page.
//
// `auth.Authorize` takes a resolver rather than a database precisely so the
// answer can come from somewhere else; this is that somewhere. An app with no
// grant is absent and reads as the empty role, which is what a per-row query
// would have returned for it.
type pageRoles map[string]string

func (r pageRoles) EffectiveRole(objectID, _, _ string, _ []string) (string, error) {
	return r[objectID], nil
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	// Nobody with no credential is told anything about the estate here (BB-28).
	// A public app's anonymous surface is the app itself, at the gateway; no
	// anonymous page or client reads this route, and the row it would answer
	// with is an operator's.
	if refuseAnonymousAppRead(w, p) {
		return
	}

	apps, err := s.listAppsFor(p, r.URL.Query().Get("tag"), r.URL.Query().Get("value"))
	if err != nil {
		log.Printf("Error listing apps: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if apps == nil {
		apps = []database.App{}
	}

	// Enrich with runtime metrics
	type AppWithMetrics struct {
		database.App
		Metrics *appMetricsResponse `json:"metrics,omitempty"`
		// The whole account for an administrator, the public shape for
		// everybody else — ownerFor.
		Owner any               `json:"owner,omitempty"`
		Tags  []database.AppTag `json:"tags"`
		// DependencyAlert is the glyph, and it is **not** a column and not a
		// status. A finding is not a status — the app is running — and adding
		// it to the status filter chips would make somebody choose between
		// "crashed" and "vulnerable" for an app that is neither.
		//
		// Absent unless this viewer may edit the app, for the reason the tab is:
		// they can do nothing with a finding, and a package-and-version
		// manifest of a running internal tool is a target map.
		DependencyAlert *database.DependencyAlert `json:"dependency_alert,omitempty"`
		// Runtime is the release line this app is on, and it is **absent for
		// everyone but an administrator**.
		//
		// The same rule as the census this feeds: an estate-wide map of which
		// internal tool runs which interpreter version names, per app, a set of
		// published advisories. A viewer can see more apps than they can edit,
		// so putting a version on every visible row would hand out a partial
		// version of exactly what /admin/estate/languages is gated to withhold.
		// Checked once per request rather than per app, unlike the glyph above:
		// every app has a runtime, so a per-app authorization call would be a
		// query per row on a list that polls.
		Runtime *appRuntime `json:"runtime,omitempty"`
		// HasJobs is whether the app declares a job, for the apps list's
		// `?kind=jobs` — arrived at from Admin → System info's app kinds.
		// Administrators only, like Runtime: it is a census fact.
		HasJobs bool `json:"has_jobs,omitempty"`
		// What this caller may do with the row, so the row can offer exactly
		// that and nothing else.
		//
		// The list's `⋯` menu offered Redeploy, Restart, Stop and Delete on
		// every app the caller could *read*, and the server refused each of
		// them for a viewer — four controls that exist to fail, on every row of
		// somebody's main page. Deriving it in the SPA would have been a second
		// definition of authorization; this is the first one, answered per row.
		CanEdit   bool `json:"can_edit"`
		CanDelete bool `json:"can_delete"`
		// CanOpen is whether following this row leads anywhere. It is false
		// exactly for a row the caller holds at the `list` rung — the app is in
		// their list and opening it meets the gate.
		//
		// It is on the row rather than derived in the SPA for the reason the two
		// above it are, and it earns its place twice: the list can say which
		// rows are which, and the filter that narrows to one or the other has a
		// fact to narrow on. Without it, an estate-sized list under the
		// install-wide switch is a list you cannot tell apart.
		CanOpen bool `json:"can_open"`
		// Mine is whether this app is *yours* — you own it, a group you belong
		// to owns it, or somebody granted it to you. It is the apps list's
		// primary split, and it is deliberately **not** `can_open`.
		//
		// Openable was the first definition and it was wrong in one direction
		// that matters: a `public` app belonging to a team you have never met
		// is openable by everybody, so it landed in your half of the page
		// beside the four apps you actually work on. On an install with a
		// published intranet that is most of the list, and "Mine" stops meaning
		// anything.
		//
		// The rule is a *relationship somebody wrote down*: an effective role
		// of viewer or above, which is exactly ownership or a grant.
		// `authenticated` and `anonymous` rank below viewer because they come
		// from the app's visibility rather than from anybody's decision about
		// you, and `list` ranks below them all. So the ladder already draws
		// this line and this reads it rather than inventing a second one.
		//
		// An app you own *and* published is still yours, because ownership
		// outranks visibility in the same comparison.
		Mine bool `json:"mine"`
		// Deploying is the status of this app's unfinished build — "pending"
		// or "building" — and **absent when there is none**, on the rule the
		// rest of this struct follows.
		//
		// It is not the app's status and must never become one. `apps.status`
		// describes the *workload*: an app that failed its last build and is
		// being rebuilt is still failed, because the thing a viewer would
		// reach is still not there, and an app that is running while a new
		// release builds is still running. That is right, and it is also why
		// the list said "Failed" for six minutes with a build in flight and
		// "Building (0)" in the strip above it — the strip is a pure partition
		// of AppStatus and adding a word to it is what the invariants forbid.
		//
		// So this is a second fact rendered beside the first, the way the
		// pending execution mode already is: the status is what the app is,
		// this is what is happening to it.
		Deploying string `json:"deploying,omitempty"`
	}

	// One query for every app's tags rather than one per app: this list is a
	// hot path and already carries a per-app owner lookup.
	appIDs := make([]string, len(apps))
	for i, app := range apps {
		appIDs[i] = app.ID
	}
	tagsByApp, err := s.db.TagsForApps(appIDs)
	if err != nil {
		log.Printf("Error loading app tags: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The dependency glyph, in one query for the whole page rather than one
	// per app — the same reason the tags above are one query. Only apps that
	// actually have an alert come back, which is normally none, so the
	// per-app permission check below costs nothing on the common case.
	//
	// A failure here degrades to no glyph rather than failing the list: this
	// page is what somebody opens during an incident, and it must render.
	alerts := map[string]database.DependencyAlert{}
	if policy, err := s.dependencyPolicy(); err != nil {
		log.Printf("deps: resolving the policy for the app list: %v", err)
	} else if !policy.Off() {
		if found, err := s.db.DependencyAlerts(policy, appIDs); err != nil {
			log.Printf("deps: reading alerts for the app list: %v", err)
		} else {
			alerts = found
		}
	}

	// The release line each app is on, for the version facet and the census's
	// drill-through. One query for the page, like the tags above; a failure
	// degrades to no facet rather than failing the list, which is what somebody
	// opens during an incident.
	servingRuntime := map[string]database.AppRuntime{}
	if p != nil && p.IsAdmin() {
		if found, err := s.db.ServingRuntimeForApps(appIDs); err != nil {
			log.Printf("estate: reading runtime versions for the app list: %v", err)
		} else {
			servingRuntime = found
		}
	}

	// Which apps declare jobs, for the same reader and on the same terms.
	withJobs := map[string]bool{}
	if p != nil && p.IsAdmin() {
		if found, err := s.db.AppsWithJobs(appIDs); err != nil {
			log.Printf("estate: reading which apps have jobs for the app list: %v", err)
		} else {
			withJobs = found
		}
	}

	// Which apps have a build in flight — one query for the whole estate
	// rather than one per row, for the reason the tags, the alerts and the
	// runtimes above are batched. A failure degrades to no indicator rather
	// than failing the list.
	deploying := map[string]string{}
	if active, err := s.db.ListActiveDeployments(); err != nil {
		log.Printf("Error reading in-flight deployments for the app list: %v", err)
	} else {
		for _, d := range active {
			deploying[d.AppID] = d.Status
		}
	}

	// Every grant this caller holds across the page, in one query rather than
	// two per row — the same reason the tags and the alerts above are batched,
	// and this list polls.
	var roles auth.RoleResolver = s.db
	if p != nil {
		if found, err := s.db.EffectiveRolesForObjects(appIDs, p.Type, p.ID, p.Groups); err != nil {
			log.Printf("Error resolving grants for the app list: %v", err)
		} else {
			roles = pageRoles(found)
		}
	}

	// Merged across backends: a listing that iterated only the shared runtime
	// would render every isolated app as having no process at all.
	runtimeMetrics, _ := s.rts.List()
	// Either an AppWithMetrics or, for a row held at the `list` rung, a
	// listOnlyAppRow — two types so the narrow one cannot inherit a field.
	result := []any{}
	for _, app := range apps {
		owner, _ := s.db.GetUser(app.OwnerID)
		obj := objectFromApp(&app)
		canOpen := auth.Authorize(p, auth.ActionRead, obj, roles).Allow
		mine := auth.RolePrecedence(auth.EffectiveObjectRole(p, obj, roles)) >=
			auth.RolePrecedence(database.ObjectRoleViewer)
		if !canOpen {
			// The list rung shows an app without opening it, so the row is
			// the four facts it discloses and the flags, and nothing it would
			// take opening the app to learn — status, release, port and source
			// included (BB-22).
			row := listOnlyRow(&app, owner)
			row.CanEdit = auth.Authorize(p, auth.ActionEditContent, obj, roles).Allow
			row.CanDelete = auth.Authorize(p, auth.ActionDelete, obj, roles).Allow
			row.Mine = mine
			result = append(result, row)
			continue
		}
		app.PublicURL = builder.PublicURL(s.cfg.AppsBaseURL, &app)
		canEdit := auth.Authorize(p, auth.ActionEditContent, obj, roles).Allow
		processName := app.ProcessName()
		if !canEdit {
			withholdServingFromReader(&app)
		}
		am := AppWithMetrics{App: app}
		am.Owner = ownerFor(p, owner)
		am.CanEdit = canEdit
		am.CanDelete = auth.Authorize(p, auth.ActionDelete, obj, roles).Allow
		am.CanOpen = true
		if m, ok := runtimeMetrics[processName]; ok && canEdit {
			am.Metrics = &appMetricsResponse{
				CPU:      m.CPU,
				Memory:   m.Memory,
				Uptime:   m.Uptime,
				Restarts: m.Restarts,
				Status:   m.Status,
			}
		}
		am.Mine = mine
		am.Tags = withFrameworkTag(&app, tagsByApp[app.ID])
		if alert, ok := alerts[app.ID]; ok {
			// Checked only for the apps that have something to show, so a
			// hundred-app estate with no findings pays for no authorization
			// calls at all.
			if am.CanEdit {
				am.DependencyAlert = &alert
			}
		}
		if p != nil && p.IsAdmin() {
			am.Runtime = appRuntimeOf(app.Framework, servingRuntime[app.ID])
			am.HasJobs = withJobs[app.ID]
		}
		am.Deploying = deploying[app.ID]
		result = append(result, am)
	}

	writeJSON(w, http.StatusOK, result)
}

// listAppsFor returns the apps a principal may read, optionally narrowed to one
// tag (and one of its values). The tag filter is applied inside the same
// visibility-scoped queries as the unfiltered listing, so browsing by tag can
// never surface an app the caller couldn't otherwise see.
func (s *Server) listAppsFor(p *auth.Principal, tagSlug, valueSlug string) ([]database.App, error) {
	scope := scopeFor(p)

	if tagSlug == "" {
		if scope.All {
			return s.db.ListAllApps()
		}
		return s.db.ListApps(scope.PrincipalID, scope.PrincipalType, scope.Groups)
	}

	tag, err := s.db.GetTagBySlug(tagSlug)
	if err != nil {
		return nil, err
	}
	if tag == nil {
		// An unknown tag matches nothing; the browse page renders an empty
		// state rather than 404ing the whole listing.
		return nil, nil
	}
	// A system tag's values live on the app row, not in app_tags.
	if tag.Kind == database.TagKindSystem {
		return s.db.ListAppsByFramework(valueSlug, scope)
	}
	return s.db.ListAppsByTag(tag.Slug, valueSlug, scope)
}

// createModeRefusal is why a new app may not have the execution mode the
// request asked for: the status, and the sentence the reader gets.
//
// Returned rather than written, so the ladder can be exercised without an
// http.ResponseWriter. Three copies of a 403/400/422 ladder is how the third
// one ends up different, and the test that prevents that has to be able to call
// the decision directly.
type createModeRefusal struct {
	status  int
	message string
}

func (m *createModeRefusal) write(w http.ResponseWriter) {
	writeJSON(w, m.status, map[string]string{"error": m.message})
}

// resolveCreateMode decides which execution mode a new app is born in, for all
// three create paths — a git URL, a ZIP upload, a gallery item.
//
// One helper, because this is one decision and it is the same one the PATCH
// already makes on an existing app. The refusals are in the order that spends
// least: who is asking, then whether the word is a mode at all, then whether
// this install offers it.
//
// Saying nothing is not a refusal at any level. An absent field means the
// install's default, which runtime.ParseMode already reads "" as, so a
// publisher who names no mode still creates an app. A publisher who *names*
// one is refused even when the value they sent is the value they would have
// got anyway: ignoring a field the caller may not set is how an API teaches
// people that a setting took effect when it did not.
func resolveCreateMode(p *auth.Principal, caps runtime.Capabilities, requested, installDefault string) (runtime.Mode, *createModeRefusal) {
	if strings.TrimSpace(requested) == "" {
		return defaultCreateMode(caps, installDefault), nil
	}

	// The identical sentence the PATCH uses, because it is the identical
	// decision: execution mode is the operator's density and cost decision,
	// not the app owner's, and creating the app does not change who pays for
	// it. A second phrasing of one policy is a second policy waiting to drift.
	if !p.IsAdmin() {
		return "", &createModeRefusal{http.StatusForbidden,
			"only an administrator can change an app's execution mode"}
	}

	mode, err := runtime.ParseMode(strings.TrimSpace(requested))
	if err != nil {
		return "", &createModeRefusal{http.StatusBadRequest, err.Error()}
	}

	// 422 rather than 400, for the reason the PATCH gives: the request is
	// well-formed and the mode is real, but this install does not offer it.
	// The body says whether an administrator can fix that or only an operator
	// can, which is the difference between a next step and a dead end.
	if why := caps.ExplainUnavailable(mode); why != "" {
		return "", &createModeRefusal{http.StatusUnprocessableEntity, why}
	}

	return mode, nil
}

// defaultCreateMode is what a new app is born as when the request names no
// mode: the install's own default, read through the same availability check as
// everything else.
//
// So an install whose default says isolated and whose capability then goes
// away — an administrator throws the switch off, an operator removes the
// cluster configuration — creates *shared* apps rather than apps that cannot
// build. Absent, not dormant. Nothing already isolated is touched, because
// this is read at insert and never again.
func defaultCreateMode(caps runtime.Capabilities, installDefault string) runtime.Mode {
	mode, err := runtime.ParseMode(strings.TrimSpace(installDefault))
	if err != nil || !caps.Allows(mode) {
		return runtime.ModeShared
	}
	return mode
}

// createMode is resolveCreateMode asked of this install: the capabilities and
// the default, from one settings snapshot, so "isolated is available" and
// "isolated is the default" cannot be read a moment apart and disagree.
func (s *Server) createMode(p *auth.Principal, requested string) (runtime.Mode, *createModeRefusal) {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		// Fail closed, for the reason capabilities() gives: the cost of being
		// wrong in the other direction is spending an operator's money.
		log.Printf("create: reading platform settings, treating isolated mode as unavailable: %v", err)
		return resolveCreateMode(p, runtime.Capabilities{Topology: s.topology()}, requested, "")
	}
	return resolveCreateMode(p, s.capabilitiesFrom(settings), requested, settings.DefaultExecutionMode)
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	// Publish (create) requires publisher or system_admin system role.
	if d := auth.Authorize(p, auth.ActionPublish, nil, s.db); !d.Allow {
		writeAuthzError(w, r, d, true)
		return
	}

	var req CreateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	// An exact slug is the caller's own word, so a bad one is said before any
	// other field is judged — not hidden behind repo_url (BB-070).
	if req.SlugExact && req.Slug != "" {
		if _, ok := fitProposedSlug(w, req.Slug, true); !ok {
			return
		}
	}
	if req.RepoURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo_url is required",
			"hint": `name the repository to clone, as "repo_url": "https://github.com/org/repo" — or push a working tree with toft deploy`})
		return
	}
	// The builder refuses both before it clones; refused here, as PATCH does,
	// so an address it would never fetch takes no slug and leaves no row.
	if req.Branch == "" {
		req.Branch = "main"
	}
	if err := builder.ValidateRepoURL(req.RepoURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass repo_url as the https:// address of a git repository with no credentials in it, like https://github.com/acme/report; a private repository takes git_token"})
		return
	}
	if err := builder.ValidateBranch(req.Branch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass branch as the branch's own name, like main, or leave it out for main"})
		return
	}

	// Enforcement point 1. Before the slug is allocated and before anything is
	// written, so a refused create leaves nothing behind to explain away.
	if s.refusedSource(w, r, req.RepoURL, "") {
		return
	}

	// Pointing an app at a connected git host: a credential decision, so no
	// key may make it, and the connection has to cover the repository. Both
	// here, above every write, for the same reason the line above is here.
	req.GitConnectionID = strings.TrimSpace(req.GitConnectionID)
	if req.GitConnectionID != "" && refusedConnectionByKey(w, r, "git_connection_id") {
		return
	}
	if s.refusedUncoveredRepo(r.Context(), w, req.GitConnectionID, req.RepoURL) {
		return
	}

	// Tier enforcement point 1, in the same position and for the same reason.
	if s.refusedTierCap(w, r) {
		return
	}

	// Beside it, and for the same reason: before the slug is allocated and
	// before CreateAppTx, so a refused create leaves no app row and no slug
	// taken to explain away.
	mode, refusal := s.createMode(p, req.ExecutionMode)
	if refusal != nil {
		refusal.write(w)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if err := validateAppName(req.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass name as a short display name in plain text, like Quarterly report"})
		return
	}
	if refuseEnvNames(w, req.EnvVars, nil) {
		return
	}

	req.RepoSubdir = builder.NormalizeSubdir(req.RepoSubdir)
	if err := builder.ValidateSubdir(req.RepoSubdir); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass repo_subdir as a folder path from the repository root, like apps/web, or leave it out to build from the root"})
		return
	}
	if s.refusedUnreachableRepo(r.Context(), w, req) {
		return
	}

	// Default the slug from the app name (or the folder's or repo's basename)
	// so URLs stay readable; use the user-provided slug when given. The folder
	// wins over the repo because every app of a shared repository has the same
	// repo basename, and would otherwise be named the same thing.
	if req.Slug == "" {
		slugSource := req.RepoURL
		if req.RepoSubdir != "" {
			slugSource = req.RepoSubdir
		}
		req.Slug = defaultSlug(req.Name, slugSource)
	} else {
		var ok bool
		if req.Slug, ok = fitProposedSlug(w, req.Slug, req.SlugExact); !ok {
			return
		}
	}

	if req.SlugExact && s.refusedTakenSlug(w, req.Slug) {
		return
	}
	// Ensure slug uniqueness
	slug, err := s.ensureUniqueSlug(req.Slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// The platform's own namespace, refused for every caller including an
	// administrator. Checked on the slug that would actually be stored — see
	// reserved_slugs.go for why this is explicit rather than left to sanitize.
	if rejectReservedSlug(w, slug) {
		return
	}
	req.Slug = slug

	if req.Name == "" {
		req.Name = req.Slug
	}

	// The audience, decided before anything is created, as the upload create
	// does: an install that publishes nothing refuses here and leaves no app.
	visibilityTo, ok := s.resolveVisibilityTo(w, r, strings.TrimSpace(req.Visibility),
		database.VisibilityPrivate, p, nil, true)
	if !ok {
		return
	}

	// Who owns the new app.
	//
	// A deploy key belongs to nobody, so there is no person to fall back on and
	// the group is not optional: without it the app would have no owner at all,
	// which apps_one_owner refuses at the database and which would be
	// unadministrable even if it did not. This is the one condition on the
	// credential rather than on the user.
	ownerID := appOwnerID(p)
	ownerGroupID := strings.TrimSpace(req.OwnerGroupID)
	if s.refusedOwnerGroup(w, p, ownerGroupID) {
		return
	}
	if ownerGroupID != "" {
		// Exactly one owner, always.
		ownerID = ""
	}

	app := &database.App{
		Slug:         req.Slug,
		Name:         req.Name,
		OwnerID:      ownerID,
		OwnerGroupID: ownerGroupID,
		RepoURL:      req.RepoURL,
		Branch:       req.Branch,
		RepoSubdir:   req.RepoSubdir,
		// Port is assigned per release by the builder, once a release is
		// actually listening — see internal/builder.
		Status:        database.AppStatusBuilding,
		Visibility:    database.VisibilityPrivate,
		ExecutionMode: string(mode),
		// Written with the row rather than by a setter afterwards: a create
		// that half-succeeded would leave an app pointed at nothing, and the
		// coverage check above already ran.
		GitConnectionID: req.GitConnectionID,
	}

	if req.GitToken != "" {
		encrypted, err := s.encryptor.Encrypt(req.GitToken)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		app.GitToken = encrypted
	}

	meta := requestMeta(r.Context())
	actorType, actorID := auditActor(p)
	if err := s.db.CreateAppTx(app, actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		log.Printf("Error creating app: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create app"})
		return
	}

	// Born private and moved, as the upload create does, so the transition
	// has its own audit row naming who made it readable.
	if visibilityTo != "" {
		if err := s.db.SetAppVisibility(app.ID, app.Visibility, visibilityTo,
			actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent, meta.APIKeyID); err != nil {
			log.Printf("apps: setting the visibility of %s at create: %v", app.Slug, err)
		} else {
			app.Visibility = visibilityTo
		}
	}

	// Store env vars. A variable that cannot be stored fails the create, and
	// the app goes with it: an app that starts without the variable its
	// creator supplied is a create that half happened.
	if len(req.EnvVars) > 0 {
		for k, v := range req.EnvVars {
			encrypted, err := s.encryptor.Encrypt(v)
			if err == nil {
				err = s.db.SetEnvVar(app.ID, k, encrypted, req.EnvSensitive[k])
			}
			if err != nil {
				log.Printf("Error storing env var %s for %s: %v", k, app.ID, err)
				if derr := s.db.DeleteApp(app.ID); derr != nil {
					log.Printf("Error removing app %s after its environment failed to store: %v", app.ID, derr)
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store environment variables"})
				return
			}
		}
	}

	// Create deployment and enqueue build
	deploy := &database.Deployment{
		AppID:  app.ID,
		Branch: req.Branch,
		Status: database.DeployStatusPending,
	}
	stampDeployer(r, deploy)
	if err := s.db.CreateDeployment(deploy, s.deployRecord(r, "git")); err != nil {
		log.Printf("Error creating deployment: %v", err)
		// The app goes with it, as the upload path does: a create that failed
		// created nothing, and an app in `building` with no deployment would
		// hold the slug forever.
		if derr := s.db.DeleteApp(app.ID); derr != nil {
			log.Printf("Error removing app %s after its first deployment failed to create: %v", app.ID, derr)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create deployment"})
		return
	}
	s.noteGarthDeploy(r, deploy.AppID)

	s.enqueueBuild(&builder.BuildJob{App: app, Deployment: deploy})

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"app":        app,
		"deployment": deploy,
	})
}

func (s *Server) handleUploadApp(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, auth.ActionPublish, nil, s.db); !d.Allow {
		writeAuthzError(w, r, d, true)
		return
	}

	// The upload switch's own first point — not a source-policy point, which
	// a ZIP with no remote can never be. Refused before the upload is read
	// rather than after: there is no reason to make somebody wait for 100MB to
	// travel before telling them this install does not accept ZIPs.
	if s.uploadsRefused(w) {
		return
	}

	// Tier enforcement point 2, and above `ParseMultipartForm` for exactly the
	// sentence above it: a cap refusal that arrived after 100MB had travelled
	// would be the worst-tempered refusal in the product.
	if s.refusedTierCap(w, r) {
		return
	}

	// Max 100MB upload — the body limit is the cap; the argument below is only
	// how much of it is held in memory.
	limitUploadBody(w, r)
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart form or file too large (max 100MB)", "hint": "send a multipart/form-data body with the archive in a part named file, 100MB at most"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field is required", "hint": "send the archive as a multipart/form-data part named file"})
		return
	}
	defer file.Close()

	suffix := archiveSuffix(header.Filename)
	if suffix == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": unsupportedArchive, "hint": "upload the app as a .zip or .tar.gz archive; toft deploy packs the working tree into one for you"})
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	slug := r.FormValue("slug")
	envVarsJSON := r.FormValue("env_vars")

	// A form value beside the name and the slug, not a JSON body: this route
	// reads its request out of a multipart form. Same field, same decision.
	mode, refusal := s.createMode(p, r.FormValue("execution_mode"))
	if refusal != nil {
		refusal.write(w)
		return
	}

	if err := validateAppName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass name as a short display name in plain text, like Quarterly report"})
		return
	}
	// Parsed once, refused on either error before the row exists: a
	// malformed env_vars or env_sensitive was dropped and the app created
	// with no variables, or with an intended secret stored visible (code
	// review 2026-11-11, P1-5).
	var envVars map[string]string
	var envSensitive map[string]bool
	if envVarsJSON != "" {
		if err := json.Unmarshal([]byte(envVarsJSON), &envVars); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "env_vars is not a JSON object of string values: " + err.Error(),
				"hint": `send env_vars as {"KEY": "value"}; every value is a string`})
			return
		}
		if refuseEnvNames(w, envVars, nil) {
			return
		}
		if raw := r.FormValue("env_sensitive"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &envSensitive); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "env_sensitive is not a JSON object of booleans: " + err.Error(),
					"hint": `send env_sensitive as {"KEY": true}`})
				return
			}
		}
	}

	if slug == "" {
		slug = defaultSlug(name, strings.TrimSuffix(header.Filename, suffix))
	} else {
		var ok bool
		if slug, ok = fitProposedSlug(w, slug, r.FormValue("slug_exact") == "true"); !ok {
			return
		}
	}
	if r.FormValue("slug_exact") == "true" && s.refusedTakenSlug(w, slug) {
		return
	}
	slug, err = s.ensureUniqueSlug(slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if rejectReservedSlug(w, slug) {
		return
	}
	if name == "" {
		name = slug
	}

	// Who owns the new app — the same question handleCreateApp answers, with
	// the same two refusals, because this is the other create path and a
	// credential accepted at one door and refused at the other is how an
	// estate tool ends up half-permitted.
	//
	// This route was reachable by a deploy key and could not complete for one:
	// appOwnerID answers empty for a credential that belongs to nobody, no
	// owner_group_id was read from the form, and the row reached the database
	// with neither owner column set, where apps_one_owner refused it. The fix
	// is to read the form value the JSON path already reads.
	// The audience the archive lands with, decided before anything is created:
	// an install that publishes nothing must refuse here rather than leave an
	// app behind. Applied after the row exists, because visibility moves
	// through its own setter and its own audit row.
	visibilityTo, ok := s.resolveUploadedVisibility(w, r, database.VisibilityPrivate, p, nil, true)
	if !ok {
		return
	}

	ownerID := appOwnerID(p)
	ownerGroupID := strings.TrimSpace(r.FormValue("owner_group_id"))
	if s.refusedOwnerGroup(w, p, ownerGroupID) {
		return
	}
	if ownerGroupID != "" {
		// Exactly one owner, always.
		ownerID = ""
	}

	app := &database.App{
		Slug:          slug,
		Name:          name,
		OwnerID:       ownerID,
		OwnerGroupID:  ownerGroupID,
		RepoURL:       builder.UploadScheme + header.Filename,
		Branch:        "upload",
		Status:        database.AppStatusBuilding,
		Visibility:    database.VisibilityPrivate,
		ExecutionMode: string(mode),
	}

	meta := requestMeta(r.Context())
	actorType, actorID := auditActor(p)
	if err := s.db.CreateAppTx(app, actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		log.Printf("Error creating app: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create app"})
		return
	}

	// The app is born private and moves, rather than being inserted public:
	// the transition is what SetAppVisibility records, and "who made this
	// readable by the world" is a question an app that was never anything else
	// cannot answer.
	if visibilityTo != "" {
		actorType, actorID := auditActor(p)
		if err := s.db.SetAppVisibility(app.ID, app.Visibility, visibilityTo,
			actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent, meta.APIKeyID); err != nil {
			log.Printf("apps: setting the visibility of %s at create: %v", app.Slug, err)
		} else {
			app.Visibility = visibilityTo
		}
	}

	// Store env vars
	if envVarsJSON != "" {
		{
			for k, v := range envVars {
				encrypted, err := s.encryptor.Encrypt(v)
				if err == nil {
					err = s.db.SetEnvVar(app.ID, k, encrypted, envSensitive[k])
				}
				if err != nil {
					// The same repair the archive failure below makes: a
					// create that failed created nothing.
					log.Printf("Error storing env var %s for %s: %v", k, app.ID, err)
					s.db.DeleteApp(app.ID)
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to store environment variables"})
					return
				}
			}
		}
	}

	// The row, and the unpack step recorded on it.
	//
	// Only the unpack: the bytes of a *new* app's first upload arrive before
	// there is an app to hang a deployment off, so there is nothing to record
	// them against and no receive step. That asymmetry with the version route
	// is why those two steps are conditional (`deploysteps.Conditional`) — a
	// timeline may not claim a step happened because a later one did.
	up := s.beginUpload(w, r, app, "upload", "upload")
	if up == nil {
		return
	}
	up.enter(builder.StepUnpack)

	// Extract ZIP to source directory
	stagingDir := builder.UploadStagingDir(s.cfg.AppsDir, app.ID)
	removed, err := extractArchive(file, header.Size, stagingDir, header.Filename)
	if err != nil {
		log.Printf("Error extracting archive: %v", err)
		// The app goes with it, and the deployment row goes with the app: a
		// create that failed created nothing. This is the one upload failure
		// that leaves no history, because there is no app for it to be the
		// history of.
		s.db.DeleteApp(app.ID)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("failed to extract the archive: %v", err), "hint": "check that the archive opens on your own machine, then upload it again; toft deploy builds one from the working tree"})
		return
	}

	// A tree the deploy would refuse is refused here, while the create can
	// still be undone: the build fails it at detect only after the slug is
	// claimed, and an app whose only deploy failed holds its name for nothing.
	// The same preflight `toft check` runs — the manifest, a static app's
	// jobs, the runtime pin, the install's job ceilings, a tree it does not
	// recognise at all — so the two cannot
	// disagree, and its code is the one check reports for the same tree.
	if refusal := s.uploadRefusal(r, stagingDir, app, true); refusal != nil {
		s.db.DeleteApp(app.ID)
		os.RemoveAll(stagingDir)
		refusal["error"] = refusal["message"]
		delete(refusal, "message")
		writeJSON(w, http.StatusUnprocessableEntity, refusal)
		return
	}

	deploy := up.handOff()
	s.enqueueBuild(&builder.BuildJob{App: app, Deployment: deploy, SkipClone: true, FromBundle: true, Removed: removed})

	// `removed` is omitted when nothing was, so an ordinary archive's response
	// is byte-for-byte what it was. Reported here as well as in the build log
	// because `curl` is a first-class caller of this route and should not have
	// to read a log to learn the platform changed what it sent.
	body := map[string]interface{}{
		"app":        app,
		"deployment": deploy,
	}
	if len(removed) > 0 {
		body["removed"] = removed
	}
	writeJSON(w, http.StatusCreated, body)
}

// refusedOwnerGroup answers for the group a new app would be owned by, and
// reports whether it refused: a deploy key that names none (it belongs to
// nobody, so there is no person to fall back on — owner_group_required), or a
// group resolveOwnerTarget will not let own anything (not found, mapped from a
// directory, no member who could administer it). 422 either way, the second
// with resolveOwnerTarget's own code (owner_group_empty, …). Both create routes ask, and so does the check
// before a create (`POST /apps/check` with owner_group_id), so a CI gate that
// passed cannot be followed by a create refused for its group (docs/bugs
// 2026-10-28 #2). An empty id from anyone else passes: the caller owns it.
func (s *Server) refusedOwnerGroup(w http.ResponseWriter, p *auth.Principal, ownerGroupID string) bool {
	if p.IsDeployKey() && ownerGroupID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "a deploy key must name the group that will own the app — pass owner_group_id",
			"code":  "owner_group_required",
			"hint":  "pass owner_group_id with the id of the group that will own the app; GET /api/v1/groups lists the groups and their ids",
		})
		return true
	}
	if ownerGroupID == "" {
		return false
	}
	if _, refusal := s.resolveOwnerTarget(database.PrincipalTypeGroup, ownerGroupID); refusal != nil {
		refusal.write(w)
		return true
	}
	return false
}

// uploadRefusal is the first refusal `toft check` would name for the tree in
// dir, deployed as app — builder.RunPreflight with the same mode, settings and
// memory envelope the check handler passes — as the body an upload answers
// with: code (the problem's own, removed_key, limit_exceeds_ceiling or a
// storage_* code, else its reason), hint, reason and message. Nil when the preflight passes. Both upload
// routes ask, so a deploy is refused with the code check gives the same tree
// instead of uploading, queueing and failing at detect (docs/bugs 2026-10-28
// D6: static + [jobs], a runtime pin this install lacks, a job over the
// install's ceiling all used to claim the slug first).
//
// creating says which route asks. A create refuses an unrecognised tree too —
// unsupported_framework, with the sentence that names what Toft looks for —
// because the app it would leave behind is a failed one holding the slug for
// a directory nobody meant to deploy (docs/bugs 2026-10-28 #1). A deploy to an
// existing app leaves that one to detect: the app is already there, and the
// failed deployment row, with its build log, is where the owner looks.
func (s *Server) uploadRefusal(r *http.Request, dir string, app *database.App, creating bool, deployed ...database.JobDefinition) map[string]any {
	var settings *database.PlatformSettings
	if ps, err := s.db.GetPlatformSettings(); err == nil {
		settings = &ps
	}
	pre := builder.RunPreflight(dir, app.DesiredMode(), settings, s.envelope(), deployed...)
	// The [[storage]] plan too, as check judges it (drills 2026-11-02: a
	// storage refusal got past the upload, and a create left a failed app).
	// A planner that cannot answer is not a refusal: the build plans again
	// and fails the deploy there if it must.
	if err := s.addStoragePlan(r, app, pre); err != nil {
		log.Printf("upload: planning storage for %s: %v", app.Slug, err)
	}
	var pp *builder.PreflightProblem
	for i := range pre.Problems {
		if creating || pre.Problems[i].Reason != string(deploysteps.ReasonUnsupportedFramework) {
			pp = &pre.Problems[i]
			break
		}
	}
	if pp == nil {
		return nil
	}
	body := map[string]any{"message": pp.Message, "code": pp.Reason, "reason": pp.Reason, "retryable": false}
	if pp.Code != "" {
		body["code"] = pp.Code
	}
	if pp.Hint != "" {
		body["hint"] = pp.Hint
	}
	return body
}

// The archive formats an upload may arrive in, and the one place that decides.
// Longest suffix first, so `.tar.gz` is never read as an unknown `.gz`.
//
// A ZIP is what both desktops produce from a folder, and it is what `toft deploy`
// builds. A gzipped tar is what every Unix toolchain produces from a directory
// — `tar czf`, `git archive`, a CI job's build artefact, `docker save`'s
// neighbour — so a caller reaching for `curl` usually has one already and had
// to repack it to get in. Two containers, one unpacker: everything past
// `walkArchive` is identical, which is what keeps the filter, the caps and the
// report from having a second implementation to drift from.
var uploadArchiveSuffixes = []string{".zip", ".tar.gz", ".tgz"}

// archiveSuffix returns the accepted extension filename ends with, or "" if it
// is not one the platform takes. Both upload routes ask before reading a byte,
// and extractArchive asks again to pick the reader — the same question, so the
// same function.
func archiveSuffix(filename string) string {
	lower := strings.ToLower(filename)
	for _, suffix := range uploadArchiveSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return suffix
		}
	}
	return ""
}

// unsupportedArchive is the refusal, in one place because both routes say it.
const unsupportedArchive = "only .zip and .tar.gz files are supported"

// archiveEntry is one member of an uploaded archive, in the terms the prefix
// strip, the filter and the caps need: where it would land, whether it is a
// directory, and what the archive *claims* it expands to. The claim is used for
// the removal report only — the byte cap is counted as the bytes are written,
// because the declared size is part of the archive and an archive that lies
// about it is the whole attack.
type archiveEntry struct {
	name string
	dir  bool
	size uint64
}

// archiveWalker visits every entry of an archive, in order. `open` reads the
// entry's bytes and is valid **only for the duration of the visit**: a tar is a
// stream, and the reader has moved on by the next entry.
//
// A walker is called twice — once to plan, once to write — and the contract
// that makes that safe is that both passes see the same entries in the same
// order. A ZIP's central directory and a tar's stream order are both stable, so
// the plan can name what to write by position.
type archiveWalker func(visit func(archiveEntry, func() (io.ReadCloser, error)) error) error

// walkerFor picks the reader by the filename the upload arrived under. The
// bytes are not sniffed: a caller who names a file `.zip` and sends a tarball
// has sent a broken upload, and "this is not a ZIP" is a better answer than
// quietly deciding it knows better than the name.
func walkerFor(file io.ReaderAt, size int64, filename string) (archiveWalker, error) {
	switch archiveSuffix(filename) {
	case ".zip":
		return zipWalker(file, size)
	case ".tar.gz", ".tgz":
		return tarGzWalker(file, size)
	}
	return nil, fmt.Errorf("%s", unsupportedArchive)
}

func zipWalker(file io.ReaderAt, size int64) (archiveWalker, error) {
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return nil, fmt.Errorf("reading zip: %w", err)
	}
	return func(visit func(archiveEntry, func() (io.ReadCloser, error)) error) error {
		for _, f := range reader.File {
			e := archiveEntry{name: f.Name, dir: f.FileInfo().IsDir(), size: f.UncompressedSize64}
			if err := visit(e, f.Open); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// tarGzWalker reads the archive from the start on each pass. Decompressing
// twice is the price of a format with no directory: the alternative is holding
// the expanded tree in memory to decide about it, which is the bomb the caps
// exist to refuse.
//
// Only regular files and directories are carried across. A symlink, a hardlink,
// a device node or a fifo is dropped — not reported as removed, because the
// removal report is about *what an author shipped that the platform strips*,
// and these are things a release tree has no use for: the source of an app is
// installed and built here, so the one legitimate tree of links (a virtualenv)
// is built on this side rather than uploaded. Dropping them is also what keeps
// the two formats' output the same shape, since the ZIP path has never created
// a link either.
func tarGzWalker(file io.ReaderAt, size int64) (archiveWalker, error) {
	// Opened once here so "this is not a gzip stream" is one error, said before
	// the first pass, rather than the same error twice from inside two walks.
	probe, err := gzip.NewReader(io.NewSectionReader(file, 0, size))
	if err != nil {
		return nil, fmt.Errorf("reading tar.gz: %w", err)
	}
	probe.Close()

	return func(visit func(archiveEntry, func() (io.ReadCloser, error)) error) error {
		gz, err := gzip.NewReader(io.NewSectionReader(file, 0, size))
		if err != nil {
			return fmt.Errorf("reading tar.gz: %w", err)
		}
		defer gz.Close()

		// Bounded on every pass, the planning one included, where nothing is
		// written and so nothing else is counted.
		tr := tar.NewReader(&capReader{r: gz, left: maxArchiveStreamBytes})
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading tar.gz: %w", err)
			}

			// `tar czf` from inside a directory writes every name as `./x`.
			// Trimmed here rather than left to filepath.Join, because the
			// prefix strip splits on the first slash and would otherwise read
			// `.` as the wrapper folder every archive shares.
			name := strings.TrimPrefix(hdr.Name, "./")
			if name == "" || name == "." {
				continue
			}

			switch hdr.Typeflag {
			case tar.TypeDir:
				// A trailing slash is how the exclusion list and the prefix
				// detector recognise a directory, and a ZIP always carries one.
				if !strings.HasSuffix(name, "/") {
					name += "/"
				}
				if err := visit(archiveEntry{name: name, dir: true}, nil); err != nil {
					return err
				}
			case tar.TypeReg:
				e := archiveEntry{name: name, size: uint64(hdr.Size)}
				open := func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }
				if err := visit(e, open); err != nil {
					return err
				}
			}
		}
	}, nil
}

// The bounds on what an archive may expand into. The compressed size is already
// capped at 100MB by both routes that call this, which bounds nothing on its
// own: a few hundred kilobytes of zeroes expands to gigabytes, and a directory
// tree with a million empty files exhausts inodes without ever being large.
//
// These mattered less while every archive here was one a person had chosen and
// uploaded. The gallery makes the platform fetch archives on its own
// initiative from a list it did not write, which is what turns a theoretical
// limit into one worth having.
// Variables rather than constants so a test can lower them: proving the byte
// cap with the real figure would mean writing half a gigabyte to disk on every
// run, which is a slow test nobody keeps.
var (
	maxUnzippedBytes int64 = 500 << 20
	maxUnzippedFiles       = 20000
)

// maxArchiveEntries bounds the *plan*, which the two caps below it cannot: they
// are counted over what survives the filter, and an archive of a million
// excluded entries survives the filter as nothing at all while the list of
// names it takes to work that out is held in memory. Set far above any real
// archive — a large monorepo's `node_modules` is a few hundred thousand entries
// and must still deploy — because this is a backstop and not a policy.
const maxArchiveEntries = 1_000_000

// extractArchive unpacks an uploaded archive into destDir, leaving out what is
// never part of an app's source, and reports what it left out (ADR §60).
//
// This is the enforcement point for the whole rule, because it is the one
// function all three upload paths funnel through — a new app from an archive, a
// new version of an existing app, and a gallery install — and now for both
// container formats too. Not the builder: by the time a build worker sees the
// job, staging has been written, counted, and is about to be *renamed* into the
// release rather than copied. Not the browser: it is one of the three callers,
// and the only one that can be replaced by `curl`.
func extractArchive(file io.ReaderAt, size int64, destDir, filename string) ([]builder.RemovedEntry, error) {
	walk, err := walkerFor(file, size, filename)
	if err != nil {
		return nil, err
	}

	// Pass one: read every name before writing anything. The prefix strip needs
	// all of them, the filter decides what the caps are counted over, and the
	// caps decide whether anything is written at all.
	var all []archiveEntry
	if err := walk(func(e archiveEntry, _ func() (io.ReadCloser, error)) error {
		if len(all) >= maxArchiveEntries {
			return fmt.Errorf("this archive holds more than %d entries, which is more than the platform will read",
				maxArchiveEntries)
		}
		all = append(all, e)
		return nil
	}); err != nil {
		return nil, err
	}

	// Detect if all files share a common root directory (e.g. "my-app/src/...")
	// If so, strip that prefix so the app files land directly in destDir
	prefix := detectArchivePrefix(all)

	// What survives, by position in the walk, so the writing pass can recognise
	// an entry without re-deciding anything about it.
	keep := make(map[int]string, len(all))
	var removed builder.RemovedTally
	for i, e := range all {
		name := e.name
		if prefix != "" {
			name = strings.TrimPrefix(name, prefix)
			if name == "" {
				continue
			}
		}
		// Traversal first, so the report counts only entries that would
		// otherwise have landed somewhere.
		if !withinDir(destDir, name) {
			continue
		}
		if seg := builder.ExcludedArchiveEntry(name, e.dir); seg != "" {
			// Directory entries are counted as neither a file nor bytes — a
			// report saying `node_modules  12,432 files` where 12,431 of them
			// are files and one is the directory itself is a report that does
			// not survive being checked.
			if !e.dir {
				removed.Add(seg, int64(e.size))
			}
			continue
		}
		keep[i] = name
	}

	// Counted after the filter, and that is load-bearing rather than tidy: a
	// mid-sized `node_modules` is twenty thousand files by itself, so counting
	// before it meant the cap fired on freight and then blamed an app whose own
	// source was two hundred files. A removed entry costs neither a file slot
	// nor a written byte; a zip bomb of a million *source* files is refused
	// exactly as it was.
	//
	// Named rather than generic, because static hosting is the framework most
	// likely to meet these: a documentation build or an image-heavy site reaches
	// twenty thousand files long before a codebase does, and "your site is too
	// big" is an answer somebody can act on.
	if len(keep) > maxUnzippedFiles {
		return nil, fmt.Errorf("this archive holds %d files and the limit is %d — "+
			"upload a smaller site, or split it", len(keep), maxUnzippedFiles)
	}

	os.RemoveAll(destDir)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, err
	}

	// Counted as it is written rather than trusted from the header, because the
	// declared uncompressed size is part of the archive and an archive that
	// lies about it is the whole attack.
	var written int64

	// Pass two. Modes are not carried across from either format: what lands here
	// is source, which the build installs and an interpreter or the file server
	// then runs, and a ZIP produced on Windows records no mode at all — so a
	// tar's are dropped rather than making the two formats disagree about what
	// an upload becomes.
	i := -1
	err = walk(func(e archiveEntry, open func() (io.ReadCloser, error)) error {
		i++
		name, ok := keep[i]
		if !ok {
			return nil
		}
		target := filepath.Join(destDir, filepath.FromSlash(name))

		if e.dir {
			os.MkdirAll(target, 0755)
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}

		rc, err := open()
		if err != nil {
			return err
		}

		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}

		n, err := io.Copy(out, io.LimitReader(rc, maxUnzippedBytes-written+1))
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
		if written += n; written > maxUnzippedBytes {
			return fmt.Errorf("this archive expands to more than %d MB, which is the limit — "+
				"upload a smaller site, or split it", maxUnzippedBytes>>20)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return removed.Entries(), nil
}

// withinDir reports whether a slash-separated archive entry stays inside the
// destination once resolved. The traversal guard, unchanged in behaviour and
// given a name only because the loop that decides and the loop that writes are
// no longer the same loop.
func withinDir(destDir, name string) bool {
	target := filepath.Join(destDir, filepath.FromSlash(name))
	clean := filepath.Clean(destDir)
	return strings.HasPrefix(target, clean+string(os.PathSeparator)) || target == clean
}

// detectArchivePrefix checks if all entries share a common top-level directory.
// Returns "dir/" prefix to strip, or "" if no common prefix.
//
// The Finder's own leavings are ignored while the prefix is computed. Zipping a
// folder on a Mac produces `mysite/` *and* `__MACOSX/`, so a literal reading
// sees two top-level directories, strips nothing, and the release root ends up
// holding a directory instead of a site. That is the wrapper-folder failure
// arriving by the exact route a static site is most likely to take — somebody
// drags a folder of HTML in rather than pushing it to git — so it is worth the
// two names.
//
// An excluded directory is never the wrapper. `node_modules/` as an archive's
// only top-level entry would otherwise be read as one, stripped, and its
// contents extracted at the release root with nothing left for the filter to
// match on. Only that one case is special-cased: the *other* directions —
// ignoring `.git/` while looking for a common root, say — would let a real
// directory beside it be mistaken for the wrapper and stripped, which breaks a
// working archive to tidy a broken one.
func detectArchivePrefix(entries []archiveEntry) string {
	if len(entries) == 0 {
		return ""
	}

	var prefix string
	for _, e := range entries {
		if isArchiveLitter(e.name) {
			continue
		}
		parts := strings.SplitN(e.name, "/", 2)
		if len(parts) < 2 {
			return "" // file at root level, no common prefix
		}
		dir := parts[0] + "/"
		if prefix == "" {
			prefix = dir
		} else if prefix != dir {
			return "" // different top-level dirs
		}
	}
	if builder.ExcludedDirs[strings.TrimSuffix(prefix, "/")] {
		return ""
	}
	return prefix
}

// isArchiveLitter reports whether an entry is something the archiver added
// rather than something the author put in the folder. Only the prefix
// computation consults it — the entries themselves are still extracted, and
// then never served, because the file server refuses every dotfile and the
// __MACOSX/ tree holds nothing but AppleDouble files whose names begin with a
// dot anyway.
func isArchiveLitter(name string) bool {
	if strings.HasPrefix(name, "__MACOSX/") {
		return true
	}
	base := name
	if i := strings.LastIndex(strings.TrimSuffix(name, "/"), "/"); i >= 0 {
		base = name[i+1:]
	}
	return base == ".DS_Store"
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	// Before the lookup, so an anonymous caller cannot tell an id that exists
	// from one that does not.
	if refuseAnonymousAppRead(w, auth.GetPrincipal(r.Context())) {
		return
	}
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	s.writeAppDetail(w, r, app)
}

// handleGetAppBySlug answers the same view by the name a person uses.
//
// It exists for a client that holds a slug and nothing else — `toft deploy --app
// my-thing` — whose only alternative is `GET /apps` and a filter: fetching the
// estate to resolve one name, on every command, from a laptop.
//
// **A slug is as much an identifier from the URL as an id is**, so the standing
// rule applies unchanged: the same readable-apps definition decides, and an app
// the caller may not see is 404, not 403. Nothing here can enumerate the estate
// that `GET /apps` would not already answer.
func (s *Server) handleGetAppBySlug(w http.ResponseWriter, r *http.Request) {
	// Before the lookup, for the reason handleGetApp's is: a slug answered 404
	// to nobody and 401 to a taken one would be an enumeration oracle.
	if refuseAnonymousAppRead(w, auth.GetPrincipal(r.Context())) {
		return
	}
	app, err := s.db.GetAppBySlug(chi.URLParam(r, "slug"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if app == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found",
			"hint": "GET /api/v1/apps lists the apps you can see, with their slugs — toft apps from the CLI"})
		return
	}
	s.writeAppDetail(w, r, app)
}

// withholdServingFromReader blanks what a reader below editor is not told
// about an app they can open: which release serves, on what port, and when it
// was deployed or changed state (BB-106). The run-state word (Status) stays —
// every list row shows it. The docs draw this line at editor, beside logs,
// events and deploy history; one projection, used by the detail (both views)
// and the listing, so the two cannot disagree.
func withholdServingFromReader(app *database.App) {
	app.CurrentRelease = ""
	app.Port = 0
	app.LastDeployedAt = nil
	app.ServingSince = nil
	app.StatusChangedAt = nil
}

// writeAppDetail is one app's page, authorized and rendered. Both routes that
// resolve an app go through it, so neither can drift into showing a field the
// other does not — or, worse, into authorizing differently.
func (s *Server) writeAppDetail(w http.ResponseWriter, r *http.Request, app *database.App) {
	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db); !d.Allow {
		writeAuthzError(w, r, d, false)
		return
	}
	app.PublicURL = builder.PublicURL(s.cfg.AppsBaseURL, app)
	// releases_kept is the owner's stored figure, 0 meaning "no opinion";
	// what a deploy keeps is this (BB-025).
	if settings, err := s.db.GetPlatformSettings(); err == nil {
		app.EffectiveReleasesKept = database.ResolveRetention(app, settings).Keep
	}

	// `?view=brief` is the row and the in-flight start, nothing joined: what
	// `toft` reads on every command to turn a slug into an app. The full page
	// below is some twenty queries and a runtime call, which every CLI command
	// paid for a handful of row fields (drills on trytoft, 2026-10-28). The
	// same authorization, and the same `app` object the full view carries.
	canEdit := auth.Authorize(p, auth.ActionEditContent, objectFromApp(app), s.db).Allow
	// What goes out as "app": a copy, so the rest of this handler still
	// reads the serving release it is computing the page from.
	shown := app
	if !canEdit {
		c := *app
		withholdServingFromReader(&c)
		shown = &c
	}
	if r.URL.Query().Get("view") == "brief" {
		writeJSON(w, http.StatusOK, map[string]any{
			"app":      shown,
			"starting": s.wakes.InFlight(app.ID),
		})
		return
	}

	// The owning principal, as whichever kind it is. GetUser("") is nil for a
	// group-owned app, which is the correct answer rather than a gap: there is
	// no person to name, and the group beside it is what the page renders.
	owner, _ := s.db.GetUser(app.OwnerID)
	var ownerGroup *database.Group
	if app.OwnerGroupID != "" {
		ownerGroup, _ = s.db.GetGroup(app.OwnerGroupID)
	}

	// Get metrics
	// The process figures are an editor's, as its logs and events are: a
	// viewer was refused `toft status` and read the same numbers here (BB-106).
	var metrics *appMetricsResponse
	if rt, err := s.runtimeFor(app); err == nil && canEdit {
		if m, err := rt.Metrics(app.ProcessName()); err == nil {
			metrics = &appMetricsResponse{
				CPU:      m.CPU,
				Memory:   m.Memory,
				Uptime:   m.Uptime,
				Restarts: m.Restarts,
				Status:   m.Status,

				ExitCode: m.ExitCode, ExitSignal: m.ExitSignal,
				CrashLooping: m.CrashLooping, OOMKilled: m.OOMKilled,
			}
		}
	}

	// Env vars: key names for anyone who can read the app; values only for
	// someone who may edit it, and then only the visible ones — a sensitive
	// value is write-only forever.
	envVars, _ := s.db.GetEnvVars(app.ID)
	// An editor's: a credential's inventory row is an administrator's, and a
	// viewer does not get the assembly of an app's credentials (ADR §92).
	appCredentials := []database.Credential{}
	var err error
	if canEdit {
		appCredentials, err = s.db.AppCredentials(app.ID)
	}
	if err != nil {
		// Absent rather than a failed page: the app's own configuration is what
		// this route is for, and a credential list that could not be read is
		// not a reason to withhold it.
		log.Printf("reading credentials for %s: %v", app.Slug, err)
		appCredentials = []database.Credential{}
	}
	var envKeys []string
	var envList []envVarResponse
	for _, v := range envVars {
		envKeys = append(envKeys, v.Key)
		if !canEdit {
			continue
		}
		item := envVarResponse{Key: v.Key, Sensitive: v.Sensitive}
		if plain, err := s.encryptor.Decrypt(v.Value); err == nil {
			if !v.Sensitive {
				item.Value = &plain
			} else {
				item.Unset = plain == ""
			}
		}
		envList = append(envList, item)
	}

	// Stored values plus the derived Framework pair.
	appTags, err := s.appTags(app)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The four facts Overview's tiles print, travelling with the app rather
	// than as four requests of their own (ADR §53 D4). A failure to read them
	// is not a failure to render the app: the tiles are the least load-bearing
	// thing on the page, and an Overview that 500s because a count did not come
	// back would be a worse answer than one without tiles.
	summary, summaryErr := s.db.GetAppSummary(app.ID)
	if summaryErr != nil {
		slog.Warn("app summary", "app", app.ID, "error", summaryErr)
		summary = nil
	}

	// Only somebody who may restart the app is told there is something to
	// restart it for, and only while the app has a workload to restart: a
	// `building` or `failed` app has no running process whose configuration
	// could be out of date, and the next deploy applies everything anyway. A
	// static app has no process to read a variable at all, so its count could
	// never be applied (drills 2026-10-30).
	unappliedEnv := 0
	if canEdit && appHasWorkload(app) && app.Framework != frameworkStatic {
		if n, err := s.db.UnappliedEnvChanges(app.ID); err == nil {
			unappliedEnv = n
		} else {
			slog.Warn("unapplied env changes", "app", app.ID, "error", err)
		}
	}

	// Which interpreter is running this app's code, from the release that is
	// actually serving — the newest successful deployment, the same definition
	// the census uses and folded by the same function, so an app's own page and
	// /admin/estate/languages cannot disagree about it.
	//
	// For an editor only, on the rule the dependency glyph follows: a viewer can
	// read more apps than they can act on, and a per-app interpreter version is
	// a partial copy of the map the census is administrator-gated to withhold.
	// The invariant that gates the census names this reader as its exception —
	// an editor may read their own app's version — so it is that word, not
	// administrator, that is checked here.
	//
	// Absent rather than a failed page, like everything else read out here. And
	// absent is never "the default": a nil runtime is a fact the page could not
	// get, which is why the SPA renders the state word and never a guess.
	var appRT *appRuntime
	if canEdit {
		if found, err := s.db.ServingRuntimeForApps([]string{app.ID}); err != nil {
			slog.Warn("reading the serving runtime", "app", app.ID, "error", err)
		} else {
			appRT = appRuntimeOf(app.Framework, found[app.ID])
		}
	}

	openInGarth := ""
	if canEdit {
		openInGarth = s.garthLinkFor(r, app)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"app": shown,
		// The public shape unless an administrator is reading — ownerFor.
		"owner": ownerFor(p, owner),
		// Present only when a group owns the app, so nothing new appears on an
		// install that has no groups.
		"owner_group": ownerGroup,
		"metrics":     metrics,
		"env_keys":    envKeys,
		"env_vars":    envList,
		// What an administrator attached: names and key names, never values —
		// for the owner and for an administrator alike, because values are read
		// at the credential and not through an app.
		//
		// The keys are named because an owner debugging a connection failure
		// must not have to guess where SNOWFLAKE_USER came from, and because
		// without them they will re-declare it and be refused by a rule they
		// cannot see.
		"credentials": appCredentials,
		"tags":        appTags,
		// What the platform has decided about *this app* — the version cap it
		// is held to, and whether it sleeps. Sent to anyone who may read the
		// app, including someone who may not change any of it.
		//
		// This is not /system/settings by another route. That endpoint 404s to
		// non-admins because the shape of the install is infrastructure detail;
		// what an owner is owed is narrower and different: the rules their own
		// app is being judged by, which they otherwise experience only as a
		// deploy that refuses or an app that keeps going to sleep.
		"policy": s.appPolicy(app, canEdit),
		// The credential itself never leaves the server; the UI only needs to
		// know whether one is configured — and, when a connection answers
		// instead, which one, so Settings can say which of the two is in force
		// rather than showing an empty token field beside a working app.
		"has_git_token": app.GitToken != "",
		// A start of this app is running now — a restart past its hold, or a
		// wake. While it is, `status` is what the row said before the start and
		// not its outcome, which is what `toft restart --wait` must not settle
		// on (one replica, so this process's wake group is the install's).
		"starting":       s.wakes.InFlight(app.ID),
		"git_connection": s.appConnectionSummary(app),
		// Absent rather than zeroed when it could not be read, so the page can
		// tell "nothing to say" from "we did not ask".
		// A reader below editor gets the tiles the viewer's Overview prints and
		// nothing else: the schedules, the last failure and the quiet clock's
		// start (with basis "deploy", the deploy time withheld above) are what
		// the routes owning them refuse a viewer.
		"summary": summaryFor(summary, canEdit),
		// What the serving release was built against, for the Runtime section
		// of this app's own settings. Absent for a viewer.
		"runtime": appRT,
		// How many variable changes are saved but not yet running. Derived from
		// the row rather than held in the client, which is the whole point: a
		// toast is gone in four seconds and this has to survive a page reload
		// and be there for whoever opens the app next.
		//
		// Only for someone who may act on it. A viewer cannot restart the app,
		// and telling them its configuration is mid-change is a fact about
		// somebody else's work.
		"unapplied_env_changes": unappliedEnv,
		// "Open in Garth", for someone who may change the app and only when an
		// administrator has said where Garth is (ADR §158). Empty otherwise,
		// so a viewer's page never learns Garth's address.
		"open_in_garth": openInGarth,
	})
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	d := auth.Authorize(p, auth.ActionEditContent, obj, s.db)
	if !d.Allow {
		// A viewer asking to change who can see the app is refused here,
		// before the body is read, and deny_no_action's hint would send them
		// to ask for editor — which would not let them do it either. When the
		// body asks for the owner's change, the owner's refusal is the one
		// that names the next step (black-box BB-32). Nothing is decided by
		// the body: the request is refused either way.
		if d.Reason == "deny_no_action" {
			var peek UpdateAppRequest
			if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&peek) == nil &&
				(peek.Visibility != nil || peek.FrameAncestors != nil) {
				if dv := auth.Authorize(p, auth.ActionChangeVisibility, obj, s.db); !dv.Allow {
					d = dv
				}
			}
		}
		writeAuthzError(w, r, d, readable)
		return
	}
	// A lock constrains reach — who may open, run or change an app — and this
	// handler is where visibility and the environment change. Left off the
	// list, the owner of a locked app made it public under the lock, and it
	// went public to anonymous visitors the moment an administrator lifted it.
	// Whole, not per field: the next field added here would be the next hole.
	if s.refuseIfLocked(w, app) {
		return
	}

	var req UpdateAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	if len(req.EnvVars) > 0 || len(req.EnvSensitive) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "an app's environment changes through PATCH /api/v1/apps/{id}/env, which names only the keys it changes",
			"code":  "use_env_route", "hint": "send the variables to PATCH /api/v1/apps/{id}/env instead, or run toft env set KEY=VALUE"})
		return
	}

	// Set when a branch change unlinks this app from the gallery, so the
	// response can say what happened rather than leaving it to be discovered.
	galleryDetached := false

	// Validate the git-related fields up front so nothing is written on a bad
	// request. Uploaded apps have no remote: no branch, no auto-deploy, no token.
	if (req.Branch != nil || req.RepoSubdir != nil || req.AutoDeploy != nil && *req.AutoDeploy || req.GitToken != nil) && builder.IsUploadedSource(app.RepoURL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this app was deployed from an upload — it has no git repository", "hint": "leave branch, repo_subdir, auto_deploy and git_token out; an uploaded app takes new code from toft deploy"})
		return
	}
	if req.Branch != nil {
		if err := builder.ValidateBranch(*req.Branch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass branch as the branch's own name, like main"})
			return
		}
	}
	if req.RepoSubdir != nil {
		normalized := builder.NormalizeSubdir(*req.RepoSubdir)
		if err := builder.ValidateSubdir(normalized); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass repo_subdir as a folder path from the repository root, like apps/web, or an empty string for the root"})
			return
		}
		req.RepoSubdir = &normalized
	}

	// Re-pointing at a different repository. Validated up front with everything
	// else git-related; written below through its own setter.
	repointTo := ""
	if req.RepoURL != nil {
		// Administrator-only. Changing where an app's code comes from is not
		// content editing: an editor who could do it would be able to
		// substitute the codebase of an app other people trust, and on a
		// restricted install it would be the way around the allowlist that the
		// per-app exemption deliberately is not.
		if !p.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "only an administrator can point an app at a different repository", "hint": "ask an administrator to change the repository, or create a new app from the other repository"})
			return
		}
		if builder.IsUploadedSource(app.RepoURL) {
			// Turning an uploaded app into a git app also needs a branch and an
			// auto-deploy decision, and quietly inventing both here would be a
			// different feature wearing this one's clothes. Refused rather than
			// smuggled in.
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "this app was deployed from an upload — it has no git repository, and " +
					"turning it into one needs a branch and an auto-deploy decision. Create a git " +
					"app instead.", "hint": "create a new app from the repository with POST /api/v1/apps and repo_url, then delete this one when it is no longer needed"})
			return
		}
		repointTo = strings.TrimSpace(*req.RepoURL)
		if err := builder.ValidateRepoURL(repointTo); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass repo_url as the https:// address of a git repository with no credentials in it, like https://github.com/acme/report"})
			return
		}
		// Enforcement point 2. The target must pass the policy in force *now*.
		// Re-pointing is the way an administrator moves a non-compliant app onto
		// an approved mirror; it is not a way to move a compliant one off.
		if s.refusedSource(w, r, repointTo, app.ID) {
			return
		}
		if repointTo == app.RepoURL {
			repointTo = ""
		}
	}

	// The connection, and the coverage rule that bounds it.
	//
	// **The re-point runs it too**, and against whichever of the two the request
	// carries: moving an app to a repository the connection does not cover is
	// refused at the moment of the mistake rather than at the next deploy with
	// a hint. Both are validated up front with everything else git-related, so
	// nothing is written on a bad request.
	connectionTo := ""
	if req.GitConnectionID != nil {
		if refusedConnectionByKey(w, r, "git_connection_id") {
			return
		}
		if builder.IsUploadedSource(app.RepoURL) && repointTo == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "this app was deployed from an upload — it has no git repository to clone", "hint": "leave git_connection_id out; an uploaded app takes new code from toft deploy, not from a clone"})
			return
		}
		connectionTo = strings.TrimSpace(*req.GitConnectionID)
	}
	// Which repository the coverage check is about: the one being moved to, or
	// the one the app already has.
	againstRepo := app.RepoURL
	if repointTo != "" {
		againstRepo = repointTo
	}
	// Which connection: the one being set, or — when only the repository is
	// moving — the one the app is already on. An app re-pointed off what its
	// connection covers is refused even though this request never mentioned the
	// connection.
	againstConnection := app.GitConnectionID
	if req.GitConnectionID != nil {
		againstConnection = connectionTo
	}
	// Only when the request moves the app's git side. Run on every PATCH, a
	// host that could not be reached — or a repository since removed from
	// the installation — refused a change to visibility or a name that never
	// mentioned git (code review 2026-11-11, P1-2).
	if (req.GitConnectionID != nil || repointTo != "") &&
		s.refusedUncoveredRepo(r.Context(), w, againstConnection, againstRepo) {
		return
	}
	// An attached extension tracks a catalog, not a branch. Two update channels
	// on one app is how an app ends up on a version nobody chose, so the switch
	// is refused rather than quietly ignored — and the refusal names both the
	// mechanism that does update it and the way out.
	if req.AutoDeploy != nil && *req.AutoDeploy &&
		app.GalleryKind == gallery.KindExtension && app.GalleryItemID != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "This app was installed from the gallery, and it is updated from the " +
				"gallery — it is pinned to the version the catalog names rather than tracking a " +
				"branch. Detach it from the gallery first if you want to follow a branch instead.", "hint": "DELETE /api/v1/apps/{id}/gallery detaches the app from the gallery, and auto_deploy can then follow a branch"})
		return
	}
	// Git access is the owner's alone; editors can edit content but not
	// swap the credential the platform clones with.
	if req.GitToken != nil && d.EffectiveRole != database.ObjectRoleOwner {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the app owner can change git access", "hint": "ask the app's owner to change its git access"})
		return
	}

	// A version count is refused up front rather than clamped: zero means "no
	// opinion, use the platform default", and a negative number is a bug in
	// the caller. What is deliberately *not* refused is a number above the
	// platform maximum — that is stored as asked and reported as a conflict,
	// so raising the cap later restores what the owner wanted.
	if req.ReleasesKept != nil && *req.ReleasesKept < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the number of versions to keep cannot be negative", "hint": "pass releases_kept as 0 for the platform default, or a positive number of versions"})
		return
	}

	// The sleep exemption is the operator's, for the same reason execution
	// mode is: idle sleep exists to reclaim a shared footprint, and an app
	// that could exempt itself would make it advisory.
	if req.NeverSleep != nil && !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "only an administrator can change whether this app sleeps", "hint": "ask an administrator to change whether this app sleeps"})
		return
	}

	// An app's own hostname, validated here and written below after the last
	// refusal. The validator is the specification (database.ParseAppHostname);
	// the taken check is read here rather than left to the unique index, so a
	// 409 never follows a write this handler already made.
	hostname, hostnameEnabled := app.Hostname, app.HostnameEnabled
	hostnameChanged := false
	if req.Hostname != nil || req.HostnameEnabled != nil {
		if !p.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "only an administrator can give an app a hostname of its own", "hint": "ask an administrator to set the app's hostname"})
			return
		}
		if req.Hostname != nil {
			hostname = strings.TrimSpace(*req.Hostname)
			if hostname == "" {
				hostnameEnabled = false
			}
		}
		if req.HostnameEnabled != nil {
			hostnameEnabled = *req.HostnameEnabled
		}
		if hostnameEnabled && hostname == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "an app's hostname can be switched on only once it has one", "code": "invalid_hostname",
				"hint": "record the name and switch it on together: toft hostname reports.example.com --on"})
			return
		}
		// Validated whenever it will be served, not only when it is typed: a
		// name switched off at startup because BASE_URL moved under it must
		// not be switched back on unchanged.
		if hostname != "" && (req.Hostname != nil || hostnameEnabled) {
			name, err := database.ParseAppHostname(hostname, s.cfg.ReservedHosts(), s.cfg.ExtraOriginHosts())
			if err != nil {
				// Every refusal carries a hint (BB-068): the message says what is
				// wrong with this name, the hint what a good one looks like.
				hint := "give a DNS name alone — lowercase letters, digits and inner dashes, at least two labels, like reports.example.com"
				// A well-formed name the install will not serve is told why the
				// shape was never the problem (BB-082).
				if strings.Contains(err.Error(), "install's own address") {
					hint = "choose a name outside the install's own addresses — one in a domain your organization controls, like reports.example.com, with its DNS pointed at this install"
				}
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_hostname", "hint": hint})
				return
			}
			hostname = name
			holder, err := s.db.HostnameHolder(hostname)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			if holder != "" && holder != app.ID {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": hostname + " is already another app's hostname", "code": "hostname_taken", "hint": "choose a different hostname, or remove this one from the app that holds it first"})
				return
			}
		}
		hostnameChanged = hostname != app.Hostname || hostnameEnabled != app.HostnameEnabled
	}

	// Naming a person to a program is a grant rather than a preference, and the
	// person disclosed is neither the app nor its owner — so the owner is the
	// wrong author of the decision. The app-integrations rule, applied to a
	// column: absent until an administrator attaches it.
	if req.ViewerIdentity != nil && !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "only an administrator can let this app learn who is viewing it", "hint": "ask an administrator to turn on viewer identity in this app's settings"})
		return
	}
	// A static app has no process to ask who is looking, so turning it on
	// would be a setting that does nothing — refused as storage and
	// integrations are (drills 2026-11-03). Turning it off is always allowed.
	if req.ViewerIdentity != nil && *req.ViewerIdentity && app.Framework == frameworkStatic {
		writeJSON(w, http.StatusConflict, map[string]string{"code": "static_app",
			"error": "this is a static app: a folder of files the platform serves, with no process " +
				"to ask who is viewing it. Deploy it as an app in a language Toft supports to use viewer identity.", "hint": "leave viewer_identity out, or send false, for a static app"})
		return
	}
	// A rendered document — a notebook, R Markdown, Quarto — is served as files
	// too: the render ran at build time, so no process is there to ask either.
	// Refused for the static app's reason (the user's call, 2026-11-04).
	if req.ViewerIdentity != nil && *req.ViewerIdentity && builder.Rerenderable(app.Framework) {
		writeJSON(w, http.StatusConflict, map[string]string{"code": "document_app",
			"error": "this app is a rendered document, served as files once it is built, with no " +
				"process to ask who is viewing it. Viewer identity needs an app that serves its own pages.",
			"hint": "leave viewer_identity out, or send false, for a document app"})
		return
	}
	// A batch app runs commands and answers no requests, so there is never a
	// viewer to identify — appint.FrameworkHasAProcess's own reason, which the
	// write did not ask (BB-007, 2026-11-04 black-box list).
	if req.ViewerIdentity != nil && *req.ViewerIdentity && builder.IsBatchFramework(app.Framework) {
		writeJSON(w, http.StatusConflict, map[string]string{"code": "batch_app",
			"error": "this is a batch app: it runs jobs and serves no pages, so there is never a " +
				"viewer for it to identify. Viewer identity needs an app that serves its own pages.",
			"hint": "leave viewer_identity out, or send false, for a batch app"})
		return
	}

	// This app's own ceiling. Admin-only for the reason execution mode is, with
	// one turn more: idle sleep an app exempted itself from would still be
	// idle sleep, while a ceiling an app can raise for itself has stopped being
	// a ceiling at all.
	//
	// Written through its own setter below rather than through the UpdateApp
	// further down, for the reason execution mode is: the builder calls
	// UpdateApp mid-build with an App it loaded before the build started, so a
	// change written there can be silently reverted by an overlapping deploy.
	memoryLimit := app.MemoryLimitMB
	var memoryLimitTo *int
	if req.MemoryLimitMB != nil || req.MemoryLimitGB != nil {
		if !p.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "only an administrator can change this app's memory limit", "hint": "ask an administrator to change this app's memory limit"})
			return
		}
		if req.MemoryLimitMB != nil && req.MemoryLimitGB != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "send memory_limit_mb or memory_limit_gb, not both — they are the " +
					"same number in two units and the platform will not guess which you meant", "hint": "send one of the two, like {\"memory_limit_gb\": 2}"})
			return
		}
		mb := 0
		field := "memory_limit_mb"
		if req.MemoryLimitMB != nil {
			mb = *req.MemoryLimitMB
		} else {
			// Checked before the rounding, not after: 0.0624 rounds to exactly
			// the floor, and accepting it would make the refusal's own number
			// wrong.
			field = "memory_limit_gb"
			gb := *req.MemoryLimitGB
			if gb != 0 && gb < float64(minAppMemoryLimitMB)/mebibytesPerGigabyte {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "memory_limit_gb must be 0, or at least " + minMemoryLimitPhrase, "hint": "pass memory_limit_gb as 0 to clear this app's own limit, or a number at least as large as the minimum the sentence names"})
				return
			}
			mb = int(math.Round(gb * mebibytesPerGigabyte))
		}
		if !storeMemoryLimit(w, mb, field, &memoryLimit) {
			return
		}
		// Validated here, written below beside UpdateApp, after the last
		// refusal: a handler that writes twice validates everything before it
		// writes once, or a 409 on the slug leaves a memory ceiling changed
		// and audited that the caller was told did not change.
		if memoryLimit != app.MemoryLimitMB {
			memoryLimitTo = &memoryLimit
		}
	}

	// This app's own CPU limit, by the memory limit's rules. Never below the
	// install's CPU request: a pod cannot be capped under what it reserves.
	cpuLimit := app.CPULimitMillicores
	var cpuLimitTo *int
	if req.CPULimitMillicores != nil || req.CPULimitCores != nil {
		if !p.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "only an administrator can change this app's CPU limit", "hint": "ask an administrator to change this app's CPU limit"})
			return
		}
		if !storeQuantity(w, req.CPULimitMillicores, req.CPULimitCores, millicoresPerCore,
			"cpu_limit_millicores", "cpu_limit_cores", minCPULimitMillicores, maxCPUMillicores, &cpuLimit) {
			return
		}
		if req := s.envelope().AppCPURequestMillicores; cpuLimit > 0 && cpuLimit < req {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("a CPU limit of %s is below this install's CPU request of %s",
					coresPhrase(cpuLimit), coresPhrase(req)), "hint": "raise the limit to at least the CPU request the sentence names, or send 0 to clear this app's own limit"})
			return
		}
		if cpuLimit != app.CPULimitMillicores {
			cpuLimitTo = &cpuLimit
		}
	}

	// Execution mode is the operator's density decision, not the app owner's:
	// promoting an app spends elastic capacity the operator pays for.
	var executionModeTo *string
	if req.ExecutionMode != nil {
		if !p.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "only an administrator can change an app's execution mode", "hint": "ask an administrator to change the app's execution mode"})
			return
		}
		mode, err := runtime.ParseMode(*req.ExecutionMode)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass execution_mode as shared or isolated"})
			return
		}
		// 422 rather than 400: the request is well-formed and the mode is real,
		// but this install does not offer it. The message says why and what
		// would change that, so an operator reads a decision rather than a
		// malfunction.
		if why := s.capabilities().ExplainUnavailable(mode); why != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": why, "hint": "keep the app on a mode this install offers; an administrator can configure the one the sentence names"})
			return
		}
		// Written through its own setter, not through the UpdateApp below: the
		// builder calls UpdateApp mid-build with an App it loaded before the
		// build started, so a promotion written there could be silently
		// reverted by an overlapping deploy.
		// And, like the memory limit, written below after the last refusal.
		if mode != runtime.Mode(app.ExecutionMode) {
			modeTo := string(mode)
			executionModeTo = &modeTo
		}
	}

	nameChanged := false
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty",
				"hint": `send the new display name, as {"name": "Quarterly report"}, or leave name out to keep it`})
			return
		}
		if err := validateAppName(name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass name as a short display name in plain text, like Quarterly report"})
			return
		}
		nameChanged = name != app.Name
		app.Name = name
	}
	slugChanged := false
	if req.Slug != nil {
		newSlug := sanitizeSlug(*req.Slug)
		if newSlug != app.Slug {
			// A rename is a creation path for a slug, so the reservation binds
			// here too — otherwise the namespace is closed at the front door and
			// open at the side one. Note this deliberately does not stop the
			// platform's own app being renamed *out* of the namespace: that is an
			// administrator giving up the stable URL, which is their business.
			if rejectReservedSlug(w, newSlug) || rejectLongSlug(w, newSlug) {
				return
			}
			existing, _ := s.db.GetAppBySlug(newSlug)
			if existing != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "slug already taken", "code": "name_taken"})
				return
			}
			app.Slug = newSlug
			// A rename moves the URL, and a verdict is about a URL. The
			// service keys its own memory by app id *and* URL and resets a
			// changed one to `unknown`, so a strike counter that survived a
			// rename would latch on one bad sample about the old URL and one
			// about the new — which is not two consecutive bad samples about
			// anything.
			slugChanged = true
		}
	}

	// Visibility change is gated by a separate authorize action.
	//
	// The value is validated and authorized here and *written* below, after
	// UpdateApp, through a setter that carries its own audit row in the same
	// transaction. Two things turn on that arrangement. A PATCH that re-sends
	// the visibility an app already has is not a change: it records nothing,
	// because "who changed who may read this app" is a question an identical
	// before and after cannot answer. And a write that fails must leave no
	// record saying it happened, which the old shape — audit committed, then
	// UpdateApp, then 500 — could not promise.
	// Who may embed this app, validated here and written below through its own
	// audited setter, for the reason visibility is: a build finishing mid-request
	// holds an App loaded before this one started, and UpdateApp would carry the
	// old value back over it.
	//
	// A list identical to the stored one is not a change and records nothing.
	var frameAncestorsTo database.FrameAncestors
	if req.FrameAncestors != nil {
		// The same permission that decides who may read this app decides who
		// may let somebody else's page wrap it. It is not a third rule.
		if d := auth.Authorize(p, auth.ActionChangeVisibility, obj, s.db); !d.Allow {
			writeAuthzError(w, r, d, readable)
			return
		}
		parsed, err := database.ParseFrameAncestors(*req.FrameAncestors)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "pass frame_ancestors as a list of origins, like [\"https://intranet.example.com\"], or an empty list to allow none"})
			return
		}
		if parsed.String() != app.FrameAncestors.String() {
			frameAncestorsTo = parsed
			if frameAncestorsTo == nil {
				// An explicit empty list is a change and has to survive the
				// nil check below, which is what a non-nil empty slice is for.
				frameAncestorsTo = database.FrameAncestors{}
			}
		}
	}

	visibilityTo := ""
	if req.Visibility != nil {
		if d := auth.Authorize(p, auth.ActionChangeVisibility, obj, s.db); !d.Allow {
			writeAuthzError(w, r, d, readable)
			return
		}
		newVis := *req.Visibility
		if newVis != database.VisibilityPublic && newVis != database.VisibilityAuthenticated && newVis != database.VisibilityPrivate {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid visibility", "hint": "pass visibility as public, authenticated or private; toft visibility sets it from the CLI"})
			return
		}
		if newVis != app.Visibility {
			// **The install's own gate, before the license's.**
			//
			// Same shape as the license gate below and for the same reason —
			// on the transition, never on the state — but it is asked first
			// because it is local truth: an install whose administrator has
			// turned public apps off refuses this whatever any vendor says,
			// and asking the entitlement first would put a license's words on
			// a refusal a license had no part in.
			//
			// Nothing here withdraws what is already chosen. An app that is
			// already public keeps its column and keeps appearing as Public on
			// its own page; what the switch takes is the anonymous half of
			// that, and it takes it in AuthObjectOf rather than here. This is
			// only the door.
			if newVis == database.VisibilityPublic && s.refusedPublicApps(w) {
				return
			}
			// **The license gate, on the transition and never on the state.**
			//
			// An app that is already public stays public and keeps serving
			// anonymous visitors: a license gates configuration, never
			// operation, and it never applies retroactively. What it withholds
			// is publishing *another* one — so this fires only when public is
			// where the app is going, and a public app moving to `private` or
			// `authenticated` is never refused, because taking something away
			// is always allowed.
			if newVis == database.VisibilityPublic &&
				s.refusedUnlicensed(w, r, entitlements.FeaturePublicApps) {
				return
			}
			visibilityTo = newVis
		}
	}

	// Who may know this app exists, validated and authorized here and written
	// below through its own audited setter — the arrangement visibility and
	// frame-ancestors use, and for both of their reasons.
	//
	// A re-send of the value the app already has is not a change and records
	// nothing. `listableTo` is a pointer so that "no change" and "change to
	// false" stay tellable apart, which a bare bool could not do.
	var listableTo *bool
	if req.Listable != nil {
		// The same permission that decides who may read this app decides who
		// may withdraw it from everybody's list. It is not a third rule.
		if d := auth.Authorize(p, auth.ActionChangeVisibility, obj, s.db); !d.Allow {
			writeAuthzError(w, r, d, readable)
			return
		}
		if *req.Listable != app.Listable {
			listableTo = req.Listable
		}
	}

	// Every refusal is behind us; the writes begin. The two operator dials go
	// through their own audited setters rather than UpdateApp — the builder
	// calls UpdateApp mid-build with an App it loaded before the build started,
	// so a value written there could be silently reverted by an overlapping
	// deploy — and they go first, so the row UpdateApp writes carries them.
	if memoryLimitTo != nil {
		meta := requestMeta(r.Context())
		if err := s.db.SetAppMemoryLimit(app.ID, app.MemoryLimitMB, *memoryLimitTo,
			p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
			log.Printf("Error setting app memory limit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		app.MemoryLimitMB = *memoryLimitTo
	}
	if cpuLimitTo != nil {
		meta := requestMeta(r.Context())
		if err := s.db.SetAppCPULimit(app.ID, app.CPULimitMillicores, *cpuLimitTo,
			p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
			log.Printf("Error setting app CPU limit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		app.CPULimitMillicores = *cpuLimitTo
	}
	if executionModeTo != nil {
		meta := requestMeta(r.Context())
		if err := s.db.SetAppExecutionMode(app.ID, app.ExecutionMode, *executionModeTo,
			p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
			log.Printf("Error setting execution mode: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		app.ExecutionMode = *executionModeTo
	}

	// Name and slug through their own setter, and only when they changed —
	// never UpdateApp from a handler: it writes every column it owns from a
	// row loaded before this request's validation, which for an app on a git
	// connection used to include a remote call (P1-1).
	if nameChanged || slugChanged {
		if err := s.db.SetAppNameAndSlug(app.ID, app.Name, app.Slug); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update app"})
			return
		}
	}
	if slugChanged {
		// A rename is a move. The serving release was built for the old slug,
		// and a keep-prefix framework — Next.js, Streamlit, marimo — answers
		// 404 at the new one until it is rebuilt there; before this nothing
		// rebuilt it, and an uploaded Next app stayed built for its first slug
		// for good.
		s.queueMove(app.ID)
	}

	if frameAncestorsTo != nil {
		meta := requestMeta(r.Context())
		actorType, actorID := auditActor(p)
		if err := s.db.SetAppFrameAncestors(app.ID, app.FrameAncestors, frameAncestorsTo,
			actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
			log.Printf("apps: changing who may embed %s: %v", app.Slug, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update app"})
			return
		}
		app.FrameAncestors = frameAncestorsTo
	}

	if visibilityTo != "" {
		meta := requestMeta(r.Context())
		actorType, actorID := auditActor(p)
		if err := s.db.SetAppVisibility(app.ID, app.Visibility, visibilityTo,
			actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent, meta.APIKeyID); err != nil {
			log.Printf("apps: changing the visibility of %s: %v", app.Slug, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update app"})
			return
		}
		app.Visibility = visibilityTo
	}

	if listableTo != nil {
		meta := requestMeta(r.Context())
		actorType, actorID := auditActor(p)
		if err := s.db.SetAppListable(app.ID, app.Listable, *listableTo,
			actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent, meta.APIKeyID); err != nil {
			log.Printf("apps: changing who may see that %s exists: %v", app.Slug, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update app"})
			return
		}
		app.Listable = *listableTo
	}

	// Branch, auto-deploy, the repository and the git credential have dedicated
	// setters so a build finishing mid-request can't clobber them (see
	// database.UpdateApp).
	if req.Branch != nil && *req.Branch != app.Branch {
		app.Branch = *req.Branch
		if err := s.db.SetAppBranch(app.ID, app.Branch); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update branch"})
			return
		}
		galleryDetached = s.detachGalleryAfterSourceChange(r, app, "branch change") || galleryDetached
	}
	if req.RepoSubdir != nil && *req.RepoSubdir != app.RepoSubdir {
		app.RepoSubdir = *req.RepoSubdir
		if err := s.db.SetAppRepoSubdir(app.ID, app.RepoSubdir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update the folder"})
			return
		}
		galleryDetached = s.detachGalleryAfterSourceChange(r, app, "folder change") || galleryDetached
	}

	// The re-point itself. last_seen_sha is cleared with it inside the setter:
	// the next deploy is a full clone of a repository this platform has never
	// seen, and carrying the old commit forward would have the poller believe it
	// had already deployed it. The release currently serving is untouched — a
	// settings change never takes down running code — so the switch takes effect
	// on the next build.
	repointedFrom := ""
	if repointTo != "" {
		repointedFrom = app.RepoURL
		meta := requestMeta(r.Context())
		if err := s.db.SetAppRepoURL(app.ID, repointTo, database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: p.ID,
			Action: "app_source_changed", TargetType: "app", TargetID: app.ID,
			Before:    jsonValStr(map[string]string{"repo_url": repointedFrom}),
			After:     jsonValStr(map[string]string{"repo_url": repointTo}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		}); err != nil {
			log.Printf("sources: re-pointing %s: %v", app.Slug, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update the repository"})
			return
		}
		app.RepoURL, app.LastSeenSHA = repointTo, ""

		galleryDetached = s.detachGalleryAfterSourceChange(r, app, "repository change") || galleryDetached
	}

	if req.AutoDeploy != nil && *req.AutoDeploy != app.AutoDeploy {
		// Seed the poller with the commit already live, so flipping the switch
		// deploys only if the remote has moved since the last build.
		seed := ""
		if *req.AutoDeploy {
			var err error
			if seed, err = s.db.LatestSuccessfulSHA(app.ID); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to enable auto-deploy"})
				return
			}
		}
		if err := s.db.SetAutoDeploy(app.ID, *req.AutoDeploy, seed); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update auto-deploy"})
			return
		}
		app.AutoDeploy = *req.AutoDeploy
	}

	// Retention has its own setter for the reason branch and auto-deploy do:
	// the builder writes UpdateApp mid-build from an App it loaded before the
	// build started, and this is a setting the *end* of that build reads.
	if req.AutoDeleteReleases != nil || req.ReleasesKept != nil {
		autoDelete := app.AutoDeleteReleases
		if req.AutoDeleteReleases != nil {
			autoDelete = *req.AutoDeleteReleases
		}
		keep := app.ReleasesKept
		if req.ReleasesKept != nil {
			keep = *req.ReleasesKept
		}
		if autoDelete != app.AutoDeleteReleases || keep != app.ReleasesKept {
			if err := s.db.SetAppRetention(app.ID, autoDelete, keep); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update version retention"})
				return
			}
			// Audited because it decides whether rollback targets are deleted,
			// and "who turned cleanup off before this app filled the disk" is
			// the question that gets asked afterwards.
			meta := requestMeta(r.Context())
			s.auditCommitted(database.AuditEntry{
				ActorType: database.PrincipalTypeUser, ActorID: p.ID,
				Action: "app.retention_change", TargetType: "app", TargetID: app.ID,
				Before: jsonValStr(map[string]any{
					"auto_delete_releases": app.AutoDeleteReleases, "releases_kept": app.ReleasesKept}),
				After: jsonValStr(map[string]any{
					"auto_delete_releases": autoDelete, "releases_kept": keep}),
				RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
			})
			app.AutoDeleteReleases, app.ReleasesKept = autoDelete, keep
		}
	}

	if req.NotifyFailures != nil && *req.NotifyFailures != app.NotifyFailures {
		if err := s.db.SetAppNotifyFailures(app.ID, *req.NotifyFailures); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update failure notifications"})
			return
		}
		// Audited even though it changes nothing about who may do what: it
		// changes who finds out, and "nobody was told" is exactly the kind of
		// thing worth being able to explain afterwards.
		meta := requestMeta(r.Context())
		s.auditCommitted(database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: p.ID,
			Action: "app.notify_failures_change", TargetType: "app", TargetID: app.ID,
			Before:    jsonValStr(map[string]bool{"notify_failures": app.NotifyFailures}),
			After:     jsonValStr(map[string]bool{"notify_failures": *req.NotifyFailures}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		})
		app.NotifyFailures = *req.NotifyFailures
	}

	if req.NeverSleep != nil && *req.NeverSleep != app.NeverSleep {
		if err := s.db.SetAppNeverSleep(app.ID, *req.NeverSleep); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update the sleep exemption"})
			return
		}
		meta := requestMeta(r.Context())
		s.auditCommitted(database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: p.ID,
			Action: "app.never_sleep_change", TargetType: "app", TargetID: app.ID,
			Before:    jsonValStr(map[string]bool{"never_sleep": app.NeverSleep}),
			After:     jsonValStr(map[string]bool{"never_sleep": *req.NeverSleep}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		})
		app.NeverSleep = *req.NeverSleep
	}

	if hostnameChanged {
		meta := requestMeta(r.Context())
		actorType, actorID := auditActor(p)
		err := s.db.SetAppHostname(app.ID, app.Hostname, app.HostnameEnabled, hostname, hostnameEnabled, actorType, actorID, meta)
		if errors.Is(err, database.ErrHostnameTaken) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": hostname + " is already another app's hostname", "code": "hostname_taken", "hint": "choose a different hostname, or remove this one from the app that holds it first"})
			return
		}
		if err != nil {
			log.Printf("apps: changing the hostname of %s: %v", app.Slug, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update the app's hostname"})
			return
		}
		// The platform's cookie choice turns on whether any name is recorded.
		s.hostnamesSeen.Store(nil)
		wasOn := app.HostnameEnabled && app.Hostname != ""
		app.Hostname, app.HostnameEnabled = hostname, hostnameEnabled
		// The move, when where the app should live changed: on, off, or a new
		// name while on. A queued deploy absorbs it (QueueMove).
		s.queueMove(app.ID)
		if isOn := hostnameEnabled && hostname != ""; isOn != wasOn {
			s.notifyHostnameChange(app, isOn)
		}
	}

	if req.ViewerIdentity != nil && *req.ViewerIdentity != app.ViewerIdentity {
		meta := requestMeta(r.Context())
		// The setter writes its own audit row in the same transaction, unlike
		// never_sleep above: this one changes what an app may learn about the
		// people using it, which is authorization-relevant, and the record of it
		// must not be able to go missing because a second write failed.
		if err := s.db.SetAppViewerIdentity(app.ID, *req.ViewerIdentity,
			p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "failed to update whether this app can see who is viewing it"})
			return
		}
		app.ViewerIdentity = *req.ViewerIdentity
	}

	if req.GitToken != nil {
		encrypted := ""
		if *req.GitToken != "" {
			var err error
			if encrypted, err = s.encryptor.Encrypt(*req.GitToken); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}
		// Audit the credential change — the fact of it, never the value — in
		// the transaction that makes it.
		action := "app.git_token_set"
		if encrypted == "" {
			action = "app.git_token_cleared"
		}
		meta := requestMeta(r.Context())
		if err := s.db.SetGitToken(app.ID, encrypted, database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: p.ID,
			Action: action, TargetType: "app", TargetID: app.ID,
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update git access"})
			return
		}
		app.GitToken = encrypted
	}

	// The connection, through its own setter for the reason `git_token` has
	// one: which credential an app clones with is a decision with its own
	// authorization and its own audit line, and a generic column writer is
	// where such a decision goes to be forgotten.
	if req.GitConnectionID != nil && connectionTo != app.GitConnectionID {
		before := app.GitConnectionID
		action := "app.git_connection_set"
		if connectionTo == "" {
			action = "app.git_connection_cleared"
		}
		meta := requestMeta(r.Context())
		if err := s.db.SetAppGitConnection(app.ID, connectionTo, database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: p.ID,
			Action: action, TargetType: "app", TargetID: app.ID,
			Before:    jsonValStr(map[string]string{"git_connection_id": before}),
			After:     jsonValStr(map[string]string{"git_connection_id": connectionTo}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "failed to update which git host this app deploys through"})
			return
		}
		app.GitConnectionID = connectionTo
	}

	// The app is the response everywhere else, so anything this request did that
	// the caller did not ask for rides beside it rather than being left for them
	// to notice.
	var notes []string
	if repointedFrom != "" {
		note := "This app now deploys from " + app.RepoURL + ". The release it is serving keeps " +
			"serving until the next deploy succeeds."
		// **Only for a pasted token.** The sentence below is true of a
		// credential one person issued for one repository and false of a
		// connection, which covers both — and the coverage check above already
		// refused the re-point if it did not. Sending an owner to a field that
		// is empty and irrelevant is the failure the whole credential-provenance
		// work exists to stop.
		if app.GitConnectionID == "" && app.GitToken != "" {
			// Left alone deliberately — clearing somebody's credential as a side
			// effect would be worse — but it was issued for the old host and is
			// very likely wrong for the new one. Saying so here is the difference
			// between a two-minute fix and a confusing auth failure at 3am.
			note += " The stored access token was issued for the old repository and is probably " +
				"not valid for this one — replace it in the app's settings."
		}
		notes = append(notes, note)
	}
	if galleryDetached {
		notes = append(notes, "Changing where this app's code comes from has unlinked it from the "+
			"gallery. It keeps running; it no longer receives gallery updates.")
	}
	if len(notes) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"app":              app,
			"gallery_detached": galleryDetached,
			"repointed":        repointedFrom != "",
			"message":          strings.Join(notes, " "),
		})
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionDelete, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}

	// Stop and delete from the backend that owns this app's workload. A
	// backend that has gone missing must not block the deletion: the row and
	// its grants are what an administrator asked to be rid of.
	if rt, err := s.runtimeFor(app); err == nil {
		rt.Stop(app.ProcessName())
		rt.Delete(app.ProcessName())
	} else {
		log.Printf("Deleting %s: no backend to stop its workload: %v", app.Slug, err)
	}
	// And its runs in flight, waited for: one still writing its log under
	// LOGS_DIR/<app> fails the storage removal below, and one left running
	// outlives the row that is the only thing watching it.
	if s.jobs != nil {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		s.jobs.EndAppRuns(ctx, app.ID)
		cancel()
	}

	// Storage goes before the row, because the row is the only thing that
	// names these bytes: an app id nobody stores is an app directory and a
	// pile of artifacts nothing will ever reclaim. A failure here therefore
	// keeps the app — the operator can retry — rather than reporting a
	// deletion that left the disk full.
	if err := builder.DeleteAppStorage(s.cfg.AppsDir, s.cfg.JobRunDir, s.cfg.LogsDir, s.artifacts, app.ID); err != nil {
		log.Printf("Deleting %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to remove the app's files: " + err.Error(),
		})
		return
	}

	// Delete from database (cascades to env_vars and deployments), with the
	// record that somebody did in the same transaction. Deleting an app is the
	// most consequential act available on this surface — it takes the release
	// history, the grants and the events with it — and until this row existed
	// it was the one act that left the audit log unable to say it happened.
	audit := auditEntryFor(r)
	audit.ActorType, audit.ActorID = auditActor(p)
	if err := s.db.DeleteAppTx(app, audit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete app"})
		return
	}
	s.retireAccount(app.Slug)

	w.WriteHeader(http.StatusNoContent)
}

// retireAccount ends a deleted app's OS account (ADR §122): its number is
// retired, so no later app — not one on the same slug — is given files the
// deleted one left in shared storage, and the account and its per-app group
// leave this host with its build directory. After the row is gone and never
// failing the delete: what is left behind is an account that owns nothing the
// platform still serves.
func (s *Server) retireAccount(slug string) {
	if s.accounts == nil {
		return
	}
	name := s.accounts.AccountName(slug)
	if name == "" {
		return
	}
	if err := s.db.RetireAppAccount(name); err != nil {
		log.Printf("Warning: retiring the account number of %s: %v", name, err)
	}
	if err := s.accounts.RemoveAccount(slug); err != nil {
		log.Printf("Warning: removing %s: %v", name, err)
	}
	// Its build directory too: a new app with this slug gets this account
	// name, and would otherwise build from the deleted app's caches.
	if s.builder != nil {
		if err := s.builder.ForgetAccount(name); err != nil {
			log.Printf("Warning: removing the build directory of %s: %v", name, err)
		}
	}
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}

	// Before anything is queued or cloned. A locked app must not deploy: an
	// owner who could deploy their way out of a lock would be able to lift it,
	// and the platform would have spent a build doing it — except the one lock
	// a deploy is the answer to (refuseDeployIfLocked).
	if s.refuseDeployIfLocked(w, app) {
		return
	}

	// Coalesce: one queued build per app. A push storm or double-click builds
	// once; the queued build clones the tip when it runs, so it covers them all.
	pending, err := s.db.HasPendingDeployment(app.ID, time.Now().UTC().Add(-pendingDeployWindow))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if pending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a deployment is already queued for this app", "code": "deploy_queued"})
		return
	}

	// Enforcement point 3. A manual deploy re-clones, including a redeploy of
	// the commit already running, so an app whose repository has fallen off the
	// approved list is refused here — while the release it is already serving
	// keeps serving, and rollback stays available. A settings change never takes
	// down running code.
	if s.refusedSource(w, r, app.RepoURL, app.ID) {
		return
	}

	// Before the row is created, so a refused deploy leaves no trace to
	// explain away in the history.
	if why := s.capBlocks(app); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": why, "hint": "delete versions you no longer need: toft deployments prune deletes every one but the serving one, or DELETE /api/v1/apps/{id}/deployments"})
		return
	}

	// What an extension installed from the gallery actually builds is the
	// version the catalog names now, not the one it named when it was
	// installed — see galleryRedeployFollow for which redeploys follow and
	// which build the ref on the app. Everything below is the ordinary deploy
	// path either way: a follow is a deployment on the same app, with the same
	// history, the same rollback and the same lock.
	follow := s.galleryRedeployFollow(app, p)
	// The source moves before the build is enqueued because it is what gets
	// cloned; the recorded *version* moves only when the build succeeds, so a
	// failed follow leaves the app truthfully claiming the code that is still
	// serving.
	if err := s.moveToGallerySource(app, follow.RepoURL, follow.Ref, follow.Subdir); err != nil {
		log.Printf("gallery: setting the source for %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	deploy := &database.Deployment{
		AppID:          app.ID,
		Branch:         app.Branch,
		Status:         database.DeployStatusPending,
		TriggeredBy:    database.DeployTriggerManual,
		GalleryVersion: follow.Version,
	}
	stampDeployer(r, deploy)
	if err := s.db.CreateDeployment(deploy, s.deployRecord(r, deployVia(app))); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create deployment"})
		return
	}
	s.noteGarthDeploy(r, deploy.AppID)

	// Audited as the update it is. "Who took this extension to 1.4.0" has one
	// answer whichever button was pressed, and an audit that only knew about
	// one of the two doors would be a log with a hole in it.
	if follow.Update {
		s.auditGallery(r, "gallery.update", app.ID, map[string]string{
			"item_id": follow.Item.ID,
			"from":    app.GalleryVersion,
			"to":      follow.Item.Version,
			"source":  galleryItemSource(follow.Item),
			"via":     "redeploy",
		})
	}

	// Uploaded apps have no remote to clone from; the new release is seeded
	// from the current one.
	s.enqueueBuild(&builder.BuildJob{
		App:        app,
		Deployment: deploy,
		SkipClone:  builder.IsUploadedSource(app.RepoURL),
	})

	writeJSON(w, http.StatusAccepted, deploy)
}

// resolveUploadedVisibility validates and gates an optional `visibility` form
// value on a bundle push, and answers with the value to move to — "" meaning
// "no change" — plus whether the caller may continue.
//
// One function because both upload routes ask it and the two gates have an
// order that matters: the install's own public-apps switch first, because it is
// local truth and an install whose administrator publishes nothing refuses this
// whatever a vendor says, then the license. Both fire on the **transition** and
// never on the state, so an app that is already public is never re-refused and
// moving away from public is never refused at all.
//
// `from` is the visibility the app has now — private, for an app that is about
// to be created. `obj` is nil on the create path, where the caller has already
// been authorized to publish and there is no object yet to authorize against.
func (s *Server) resolveUploadedVisibility(w http.ResponseWriter, r *http.Request, from string,
	p *auth.Principal, obj *auth.Object, readable bool) (string, bool) {

	return s.resolveVisibilityTo(w, r, strings.TrimSpace(r.FormValue("visibility")), from, p, obj, readable)
}

// resolveVisibilityTo is resolveUploadedVisibility for a value already read —
// the upload's form field or the JSON create's "visibility". One set of
// gates, in one order, whichever door the audience came through.
func (s *Server) resolveVisibilityTo(w http.ResponseWriter, r *http.Request, newVis, from string,
	p *auth.Principal, obj *auth.Object, readable bool) (string, bool) {
	if newVis == "" {
		return "", true
	}
	if obj != nil {
		if d := auth.Authorize(p, auth.ActionChangeVisibility, obj, s.db); !d.Allow {
			writeAuthzError(w, r, d, readable)
			return "", false
		}
	}
	if newVis != database.VisibilityPublic && newVis != database.VisibilityAuthenticated &&
		newVis != database.VisibilityPrivate {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid visibility", "hint": "pass visibility as public, authenticated or private, or leave it out to keep the current one"})
		return "", false
	}
	if newVis == from {
		return "", true
	}
	if newVis == database.VisibilityPublic && s.refusedPublicApps(w) {
		return "", false
	}
	if newVis == database.VisibilityPublic &&
		s.refusedUnlicensed(w, r, entitlements.FeaturePublicApps) {
		return "", false
	}
	return newVis, true
}

// applyPromotionDestination applies the two things a bundle push may decide
// about its destination beyond the bytes: the slug the app answers on, and who
// may see it. Both are optional form values; absent means unchanged, so an
// ordinary `toft deploy` behaves exactly as it did.
//
// **Why the upload carries them rather than PATCH /apps/{appID}.** A promotion
// between installs is one act, and what it decides about the destination is the
// code, the name in the URL and the audience. The general edit route carries
// those *and* `git_token`, `repo_url` and `branch` — a credential and the
// provenance of the code — which is why it is off the deploy-key list and stays
// off it. Reading the two here keeps the decision at the moment it is made,
// gives the credential no reach it did not need, and leaves the refused half
// refused.
//
// **Visibility is an authorization decision, and this is a deliberate change to
// what a deploy key may decide.** The gates are the same ones PATCH applies and
// in the same order — the install's own public-apps switch first, because it is
// local truth, then the license, both on the *transition* and never on the
// state — and the audit row names the actor through auditActor, so a promotion
// that publishes an app reads as the key that did it rather than as a person
// with no id. What it cannot do is the other half of that route: no
// `git_token`, no `repo_url`, no `branch`.
//
// **Applied before the bundle is read.** A refusal here — a reserved slug, one
// already taken, an install that publishes nothing — must leave no deployment
// row, and the caller has to learn it before 100MB travels. The consequence is
// stated rather than hidden: a rename that succeeds and an upload that then
// fails leaves the app renamed and serving the release it was already serving,
// which is a settings change that did not take down running code.
//
// Returns false when it has written a refusal.
func (s *Server) applyPromotionDestination(w http.ResponseWriter, r *http.Request,
	app *database.App, p *auth.Principal, obj *auth.Object, readable bool) bool {

	slugChanged := false
	if raw := r.FormValue("slug"); strings.TrimSpace(raw) != "" {
		newSlug := sanitizeSlug(raw)
		if newSlug != app.Slug {
			// The same two refusals a rename gets on PATCH, for the same
			// reason: the namespace cannot be closed at one door and open at
			// another.
			if rejectReservedSlug(w, newSlug) || rejectLongSlug(w, newSlug) {
				return false
			}
			if existing, _ := s.db.GetAppBySlug(newSlug); existing != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "slug already taken", "code": "name_taken"})
				return false
			}
			app.Slug = newSlug
			slugChanged = true
		}
	}

	visibilityTo, ok := s.resolveUploadedVisibility(w, r, app.Visibility, p, obj, readable)
	if !ok {
		return false
	}

	if !slugChanged && visibilityTo == "" {
		return true
	}
	// The same refusal PATCH makes for the same two fields under any lock: a
	// push carrying slug or visibility renamed or published an app the
	// dependency policy had locked, before any build (code review 2026-11-11,
	// P1-3). A locked app's deploy is still its way out; its settings are not.
	if app.Locked() && s.refuseIfLocked(w, app) {
		return false
	}

	if slugChanged {
		// The two columns a rename owns, not the whole row (P1-1).
		if err := s.db.SetAppNameAndSlug(app.ID, app.Name, app.Slug); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update app"})
			return false
		}
		// A rename is a move, as PATCH's is: the serving release was built for
		// the old slug, and a keep-prefix framework answers 404 at the new one
		// until rebuilt — which an upload that then failed never did (code
		// review 2026-11-11, P1-4). The queued move coalesces with the build
		// this push starts.
		s.queueMove(app.ID)
	}

	if visibilityTo != "" {
		meta := requestMeta(r.Context())
		actorType, actorID := auditActor(p)
		if err := s.db.SetAppVisibility(app.ID, app.Visibility, visibilityTo,
			actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent, meta.APIKeyID); err != nil {
			log.Printf("apps: changing the visibility of %s: %v", app.Slug, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update app"})
			return false
		}
		app.Visibility = visibilityTo
	}
	return true
}

// handleUploadVersion accepts a new bundle for an existing app and deploys it.
// The app keeps grants, env vars, API keys and cron jobs; the code changes, and
// the push may carry a new slug and a new visibility with it — see
// applyPromotionDestination for why those two and no others.
//
// **Including an app whose source is a repository.** That is the whole point of
// `toft deploy`: the developer with a half-finished app wants the code on their
// disk running, not twenty commits named `wip`. The app is not converted by it —
// `repo_url` stands, auto-deploy stays on, rollback stays available — and the
// consequence is stated rather than designed around: the repository wins next.
// Anyone who presses Redeploy, and any push the poller sees, replaces this code.
// Overview says so while the two diverge (appServingUpload), and the CLI says so
// at the point of use.
func (s *Server) handleUploadVersion(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}

	// The upload switch, consulted here and not on an uploaded app's own
	// version bump. The rule is that it is consulted wherever refusing still
	// leaves the app a way to deploy: this app has a repository, so a refusal
	// strands nothing, while an app created from a ZIP has no other door and is
	// governed by the decision made when it was created. Without this line an
	// install with uploads off would still take arbitrary bundles onto every
	// git-backed app it has.
	if !builder.IsUploadedSource(app.RepoURL) && s.uploadsRefused(w) {
		return
	}

	// The lock, before a byte of the bundle is read. Upload is the other door
	// into a build, and a rule enforced on one of the two doors is not a rule —
	// so it admits exactly what deploy admits.
	if s.refuseDeployIfLocked(w, app) {
		return
	}

	// The app's staging directory is held from the pending check through the
	// hand-off, so two uploads at once cannot extract into one tree.
	release, ok := claimUploadSlot(app.ID)
	if !ok {
		refuseConcurrentUpload(w)
		return
	}
	defer release()

	pending, err := s.db.HasPendingDeployment(app.ID, time.Now().UTC().Add(-pendingDeployWindow))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if pending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a deployment is already queued for this app", "code": "deploy_queued"})
		return
	}

	// Refused before the upload is read, not after: there is no reason to make
	// someone wait for 100MB to travel before telling them it cannot be
	// deployed.
	if why := s.capBlocks(app); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": why, "hint": "delete versions you no longer need: toft deployments prune deletes every one but the serving one, or DELETE /api/v1/apps/{id}/deployments"})
		return
	}

	// The form is parsed before the row exists, and the error is carried past
	// it rather than acted on here. Both halves of "a refusal is not a deploy,
	// but a failed upload is" turn on that: the destination decisions below
	// read form values and must be able to refuse without leaving a
	// deployment, while a body that never arrived is a deploy that failed and
	// has to be on the app's timeline.
	limitUploadBody(w, r)
	parseErr := r.ParseMultipartForm(100 << 20)

	// What this push decides about the destination beyond the bytes. Above
	// beginUpload deliberately: a slug already taken is a refusal, not a
	// deploy.
	if parseErr == nil && !s.applyPromotionDestination(w, r, app, p, obj, readable) {
		return
	}

	// Every refusal above this line leaves no row: the deploy did not happen.
	// From here it has, and its two upload steps are recorded on it — including
	// the ones that fail. See uploadPhase.
	up := s.beginUpload(w, r, app, "upload", "upload")
	if up == nil {
		return
	}

	// A read that fails is as often an abandoned client as a malformed body —
	// either way the deploy is over, and the row says so rather than the
	// connection simply ending.
	if parseErr != nil {
		up.fail(w, http.StatusBadRequest, "the upload did not arrive: invalid multipart form or file too large (max 100MB)")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		up.fail(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer file.Close()
	if archiveSuffix(header.Filename) == "" {
		up.fail(w, http.StatusBadRequest, unsupportedArchive)
		return
	}

	up.enter(builder.StepUnpack)
	// A previous upload whose build already consumed the staging dir leaves
	// nothing behind, but a failed extract can — start clean either way.
	stagingDir := builder.UploadStagingDir(s.cfg.AppsDir, app.ID)
	os.RemoveAll(stagingDir)
	removed, err := extractArchive(file, header.Size, stagingDir, header.Filename)
	if err != nil {
		log.Printf("Error extracting archive: %v", err)
		up.fail(w, http.StatusBadRequest, fmt.Sprintf("failed to extract the archive: %v", err))
		return
	}
	// Refused here, with the code `toft check` gives the same tree, rather than
	// queued for a build that fails it at detect. The row keeps the failure,
	// as every upload failure does.
	deployed, _ := s.db.ListJobDefinitions(app.ID)
	if refusal := s.uploadRefusal(r, stagingDir, app, false, deployed...); refusal != nil {
		os.RemoveAll(stagingDir)
		msg := refusal["message"].(string)
		delete(refusal, "message")
		reason := deploysteps.Reason(refusal["reason"].(string))
		delete(refusal, "reason")
		up.failAs(w, http.StatusUnprocessableEntity, reason, msg, refusal)
		return
	}

	deploy := up.handOff()
	s.enqueueBuild(&builder.BuildJob{App: app, Deployment: deploy, SkipClone: true, FromBundle: true, Removed: removed})

	// The deployment's own fields stay at the top level — `toft deploy` reads
	// `id` from here — and `removed` joins them as a sibling. Embedding rather
	// than an envelope for exactly that reason: this body is a wire contract
	// with a client that ships separately.
	writeJSON(w, http.StatusAccepted, struct {
		*database.Deployment
		Removed []builder.RemovedEntry `json:"removed,omitempty"`
	}{deploy, removed})
}

// handleRollback makes an earlier successful deployment current again. Its
// release directory is started as-is on a fresh port and health-checked
// before the flip — no source is fetched, nothing is rebuilt — unless it was
// built for a base path the app no longer has, when it is rebuilt from its
// commit for the current one (builder.RollbackRebuilds).
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}

	// Before the release is resolved, for deploy's reason exactly: a rollback
	// *changes what is served* and starts a workload, which is the whole of
	// what a lock withholds. It reached here without this — the enforcement
	// list called itself the specification and this path was not on it — so an
	// owner refused a deploy could reach the same outcome by making an older
	// build current, and the serving release changed under a live lock.
	if s.refuseIfLocked(w, app) {
		return
	}

	// Pin the deployment to the app in the URL — a deployment ID alone is an
	// IDOR (authorization was evaluated against the app in the path).
	source, err := s.db.GetDeployment(chi.URLParam(r, "deployID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if source == nil || source.AppID != app.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found", "hint": "GET /api/v1/apps/{id}/deployments lists this app's deployments; toft deployments list from the CLI"})
		return
	}
	if source.Status != database.DeployStatusSuccess {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "only a successful deployment can be made current", "hint": "pick a deployment whose status is success; toft deployments list shows them"})
		return
	}
	// By the release's id, not a name rebuilt from today's slug: the serving
	// workload keeps the slug it was built under, so after a rename that
	// needed no move this never matched (code review 2026-11-11, P1-7).
	if builder.LiveReleaseID(s.cfg.AppsDir, app) == source.ID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "that deployment is already current", "code": "already_current", "hint": "pick a different deployment to roll back to; toft deployments list marks the serving one"})
		return
	}
	// A rollback to this release already queued or building — a double click,
	// a CLI retry. HasPendingDeployment counts `pending` only, by design, so
	// the first one, once building, did not stop the second; and two
	// rollbacks to one release start one workload name, so the second
	// restarted the serving workload in place (PB-1). The builder also
	// no-ops a rollback to what is already serving; this is the cheap half.
	if history, err := s.db.ListDeployments(app.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	} else {
		for _, d := range history {
			if d.SourceDeployID == source.ID &&
				(d.Status == database.DeployStatusPending || d.Status == database.DeployStatusBuilding) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "a rollback to that deployment is already in progress", "code": "deploy_queued"})
				return
			}
		}
	}
	// On disk here, or fetchable from the artifact store — restoring a pruned
	// release is what the store is for, and the builder does exactly that
	// before starting a rollback. Refusing on the directory alone would reject
	// builds this install can serve; after release deletion exists, it would
	// also make "pruned" and "deleted" indistinguishable to a caller, when only
	// one of them is permanent.
	if !builder.ReleaseAvailable(s.cfg.AppsDir, s.artifacts, app.ID, source.ID, source.ArtifactKey) {
		writeJSON(w, http.StatusGone, map[string]string{"error": "that release no longer exists — run a fresh deploy instead", "hint": "deploy the app again with toft redeploy, or POST /api/v1/apps/{id}/deploy"})
		return
	}

	pending, err := s.db.HasPendingDeployment(app.ID, time.Now().UTC().Add(-pendingDeployWindow))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if pending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a deployment is already queued for this app", "code": "deploy_queued"})
		return
	}

	deploy := &database.Deployment{
		AppID:       app.ID,
		Branch:      source.Branch,
		CommitSHA:   source.CommitSHA,
		Status:      database.DeployStatusPending,
		TriggeredBy: database.DeployTriggerRollback,
		// Carried from the release being restored, so rolling the code back
		// rolls the recorded catalog version back with it. Without this line an
		// app that rolled back out of a bad extension update would go on
		// claiming the version it had just abandoned, and would be offered no
		// update to escape it.
		GalleryVersion: source.GalleryVersion,
		// On the row, not only on the job below: a bulk release deletion
		// between this queue and the worker reads the row, and the release
		// being returned to must be protected from the moment it is chosen.
		SourceDeployID: source.ID,
		// The release the row is: the source, or — when the source is itself
		// a rebuild (a move, a rollback across base paths) — the release that
		// one rebuilt, so the next rollback goes back from there.
		RestoredFrom: builder.OriginalRelease(source),
	}
	stampDeployer(r, deploy)
	if err := s.db.CreateDeployment(deploy, nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create deployment"})
		return
	}

	s.enqueueBuild(&builder.BuildJob{App: app, Deployment: deploy, SourceDeployID: source.ID})
	s.auditPromote(r, app, source)

	// `rebuild` says which row will be serving: a plain rollback's row is the
	// platform's transient record, deleted on success, and the source serves;
	// a rebuilt one's row owns a fresh tree and is what serves (TOFT-BB-003).
	writeJSON(w, http.StatusAccepted, struct {
		*database.Deployment
		Rebuild bool `json:"rebuild"`
	}{deploy, builder.RollbackRebuilds(source, app)})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	// Taken before every read below, so that "work older than this request"
	// means older than the request rather than older than the last check.
	asked := time.Now()

	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}

	// A locked app is the one app its owner may not restart — the whole point
	// of the lock being a column rather than the `stopped` status, which they
	// may leave whenever they like.
	if s.refuseIfLocked(w, app) {
		return
	}
	if s.refuseNoService(w, app) {
		return
	}

	if _, err := s.runtimeFor(app); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "ask an administrator to move the app to an execution mode this install runs, with execution_mode on PATCH /api/v1/apps/{id}"})
		return
	}
	if s.builder == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this install cannot start workloads", "hint": "ask an administrator to configure this install to run apps"})
		return
	}

	// Nothing to start. An app that has never deployed has no release to run,
	// and asking the builder would produce the same refusal a paragraph later
	// with worse wording — "redeploy the app", to somebody who has not deployed
	// it once. 409 rather than 500: the request is well formed and the caller
	// knows what to do about it.
	if app.CurrentRelease == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this app has never been deployed — deploy it before starting it",
			"code":  "no_release",
			"hint":  "deploy it first with toft deploy, or POST /api/v1/apps/{id}/deploy",
		})
		return
	}

	// Captured now, because the work below may outlive this request and the
	// principal and request metadata live on its context. Through auditActor,
	// so a deploy key is recorded as the key and not as a user with no id.
	actorType, actorID := auditActor(auth.GetPrincipal(r.Context()))

	// Bound this waiter, not the start. An isolated start is a pod schedule and
	// an image pull, and `readinessTimeout` is 90 seconds — an HTTP handler that
	// blocks that long is not a design. Past the hold the answer is 202 with the
	// start still running, which is what makes the *Restarting* pill honest
	// rather than a spinner that guesses.
	ctx, cancel := context.WithTimeout(r.Context(), s.wakeHold)
	defer cancel()

	// A person's restart forgives: it is the "or restart it" the supervisor's
	// and the health sweep's endings promise (lifecycle.go).
	action, err := s.restartApp(ctx, app, restartOpts{
		Asked: asked, ActorType: actorType, ActorID: actorID,
		Meta: requestMeta(r.Context()), Forgive: true,
	})
	if err != nil {
		if ctx.Err() != nil {
			// Still running. Nothing is written about the outcome here —
			// restartApp's start owns that, and will write it when it knows.
			writeJSON(w, http.StatusAccepted, map[string]string{
				"status": "restarting",
				"detail": "The app is starting. This can take a minute in isolated mode, while the pod is scheduled and its image pulled.",
			})
			return
		}
		// A release that is gone is a refusal the caller can act on, not a
		// server error: the row and the disk disagree, and only a fresh deploy
		// settles it.
		if builder.MissingRelease(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this app has no release on this machine — deploy it again",
				"code":  "no_release",
				"hint":  "deploy it again with toft redeploy, or POST /api/v1/apps/{id}/deploy",
			})
			return
		}
		// Locked while the start ran: the lock's own refusal, not a 500.
		if errors.Is(err, errRestartLocked) {
			if fresh, ferr := s.db.GetApp(appID); ferr == nil && fresh != nil && s.refuseIfLocked(w, fresh) {
				return
			}
		}
		log.Printf("restarting %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "the app could not be started — its log has the detail"})
		return
	}

	// `status` stays "restarted" for both — clients older than `action` read
	// it — and `action` says which of the two this was, the same judgement
	// the audit row records, so `toft start` need not guess (docs/bugs
	// 2026-10-28 D20).
	body := map[string]string{"status": "restarted", "action": strings.TrimPrefix(action, "app.")}
	static := builder.Framework(app.Framework) == builder.FrameworkStatic
	// A stopped static app that is served again did change: the note that the
	// restart changed nothing is for the restart only.
	if note := restartNote(app.Framework); note != "" && !(static && action == "app.start") {
		body["note"] = note
	}
	writeJSON(w, http.StatusOK, body)
}

// restartNote is what a restart did not do, for the frameworks where that is
// the part a caller came for. A document (a notebook, Quarto, R Markdown) is
// rendered when it is deployed: a restart brings the server of its rendered
// pages back, and the page shows what the last render made of it. Before, the
// answer was a bare "restarted" to somebody who had just changed a variable
// and expected the page to change with it (docs/bugs 2026-10-28 D11).
//
// A static app has less still: its files are served as they are, and no
// process of its own reads an environment, so the restart changed nothing. The
// sentence is the one `toft` printed in the install's silence
// (cli/lifecycle.go staticRestartNote), now said here for every caller.
func restartNote(framework string) string {
	switch builder.Framework(framework) {
	case builder.FrameworkStatic:
		return "This is a static app: its files are served as they are, and no process of its own " +
			"reads an environment — the restart changed nothing, and a variable set on it reaches nothing."
	case builder.FrameworkNotebook, builder.FrameworkQuarto, builder.FrameworkRMarkdown:
		return "This is a document, rendered when it is deployed: the restart served the same pages again. " +
			"A changed variable reaches the page at the next deploy (`toft redeploy`), and a job at its next start."
	}
	return ""
}

// auditLifecycle records a deliberate start, restart or stop.
//
// Until this existed, none of the three was written down anywhere. The only
// restart figure in the product is the Restarts stat on the app page, and it
// comes from the backend's own counter — which counts the restarts the agent or
// the kubelet performed, starts again when the workload is described anew, and
// therefore cannot answer the question that
// actually gets asked the morning after: did somebody bounce this, and who.
//
// app_events is the wrong home for it. That table is machine observations of
// things breaking, and a person pressing a button is neither a machine
// observation nor a thing breaking. audit_log is the actor-driven record, and
// this follows app.never_sleep_change, which is audited for the same reason: a
// deliberate act on somebody else's app, even though it changes no
// authorization state.
//
// Not in a transaction with the restart itself, and deliberately not fatal if
// it fails. The invariant that binds an audit write to its mutation covers
// authorization-relevant changes; this is not one, and refusing to serve a
// restart that already happened because the note about it did not land would
// be the worse failure.
func (s *Server) auditLifecycle(r *http.Request, app *database.App, action, from, to string) {
	actorType, actorID := auditActor(auth.GetPrincipal(r.Context()))
	s.writeLifecycleAudit(actorType, actorID, requestMeta(r.Context()), app, action, from, to)
}

// writeLifecycleAudit is auditLifecycle with the actor and the request's
// metadata already in hand, for the one caller that outlives its request: a
// restart writes its row when the start finishes, which may be after the
// handler has already answered 202.
func (s *Server) writeLifecycleAudit(actorType, actorID string, meta database.RequestMeta, app *database.App, action, from, to string) {
	s.writeLifecycleAuditVia(actorType, actorID, meta, app, action, from, to, "")
}

// writeLifecycleAuditVia is writeLifecycleAudit with how the act arrived, on
// `after` when it was not a person: "schedule" for a `[restart]` firing.
func (s *Server) writeLifecycleAuditVia(actorType, actorID string, meta database.RequestMeta, app *database.App, action, from, to, via string) {
	after := map[string]string{"status": to}
	if via != "" {
		after["via"] = via
	}
	_ = s.db.WriteAuditRow(database.AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action: action, TargetType: "app", TargetID: app.ID,
		Before:    jsonValStr(map[string]string{"status": from}),
		After:     jsonValStr(after),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	if s.refuseNoService(w, app) {
		return
	}

	rt, err := s.runtimeFor(app)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "ask an administrator to move the app to an execution mode this install runs, with execution_mode on PATCH /api/v1/apps/{id}"})
		return
	}
	was := app.Status
	// A workload that is not there is not a failed stop. The app has no process
	// — never deployed, already stopped, asleep — and the state the caller
	// asked for is the state it is in, so this is 200 and the row is written
	// like any other stop. The two backends now say that in the same word; what
	// they used to say was PM2's own error text on one and success on the
	// other, which reached the API as a 500 quoting a backend the caller has no
	// reason to know about.
	if err := rt.Stop(app.ProcessName()); err != nil && runtime.AbsenceReason(err) == "" {
		// The backend's own wording stays in the log. A caller gets a sentence
		// about their app: a stop that genuinely failed is the platform's
		// problem to look at, not a wire format to parse.
		log.Printf("stopping %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "the workload could not be stopped"})
		return
	}

	s.db.UpdateAppStatus(appID, database.AppStatusStopped)
	s.auditLifecycle(r, app, "app.stop", was, database.AppStatusStopped)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

type appMetricsResponse struct {
	// Absent, never zero, when the backend running this app takes no reading —
	// the same shape and the same reason as host_capacity one endpoint over. A
	// consumer gets undefined here and renders "not available"; a 0 would have
	// told it the app was idle.
	CPU      *float64 `json:"cpu,omitempty"`
	Memory   *float64 `json:"memory,omitempty"`
	Uptime   int64    `json:"uptime"`
	Restarts int      `json:"restarts"`
	Status   string   `json:"status"`
	// How the backend says the workload last ended, and its own verdicts,
	// each in its own words and absent when it says nothing: a code nil is
	// "not known" (an exit the agent did not see as parent, a signal death),
	// never 0 (runtime.AppMetrics).
	ExitCode     *int   `json:"exit_code,omitempty"`
	ExitSignal   string `json:"exit_signal,omitempty"`
	CrashLooping bool   `json:"crash_looping,omitempty"`
	OOMKilled    bool   `json:"oom_killed,omitempty"`
}

func (s *Server) handleCheckSlug(w http.ResponseWriter, r *http.Request) {
	if refuseAnonymousAppRead(w, auth.GetPrincipal(r.Context())) {
		return
	}
	slug := sanitizeSlug(chi.URLParam(r, "slug"))
	// A reserved slug is unavailable whether or not an app holds it, so the New
	// App form says no before somebody fills in the rest and is refused on
	// submit. The reason rides along: "unavailable" with no explanation on a name
	// nothing appears to be using is the kind of thing people retry.
	if isReservedSlug(slug) {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    reservedSlugMessage,
		})
		return
	}
	if len(slug) > maxSlugLen {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    slugTooLongMessage(slug),
		})
		return
	}
	exists, err := s.db.SlugExists(slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": !exists})
}

func randomSlug() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "app-" + hex.EncodeToString(b)
}

// defaultSlug picks a readable slug when the caller didn't provide one: the
// app name if given, else the last path segment of the source (repo URL or
// filename), else a random slug.
// A name may run to maxAppNameLen, so what is derived from it is cut to
// maxSlugLen rather than refused: the caller did not choose it.
func defaultSlug(name, source string) string {
	if name != "" {
		return fitSlug(sanitizeSlug(name), maxSlugLen)
	}
	source = strings.TrimSuffix(source, ".git")
	if i := strings.LastIndexAny(source, "/\\"); i >= 0 {
		source = source[i+1:]
	}
	if source != "" {
		return fitSlug(sanitizeSlug(source), maxSlugLen)
	}
	return randomSlug()
}

// maxAppNameLen bounds display names. Names render in HTML (escaped) and as
// plain text wherever an app is listed; nothing legitimate needs more than this.
const maxAppNameLen = 100

// validateAppName rejects names that can't be displayed safely anywhere the
// name appears. Escaping happens at render time — this is defence in depth,
// not the XSS fix.
func validateAppName(name string) error {
	if len(name) > maxAppNameLen {
		return fmt.Errorf("name must be at most %d characters", maxAppNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("name must not contain control characters")
		}
	}
	return nil
}

func sanitizeSlug(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "app"
	}
	return s
}

// refusedUnreachableRepo asks the remote for the branch before anything is
// written. A missing repository, a private one with no credential, or a missing
// branch used to become an app row in `failed` that held its slug, with git's
// own "could not read Username … terminal prompts disabled" as the only
// explanation; now it is a 422 `source_unavailable` with the sentence the
// builder would have written, and nothing is created.
//
// **Only what needs a person refuses.** A network blip is not a reason to
// refuse a create — the build retries the clone and says so — and a repository
// reached through a connection is left to the builder, which resolves that
// credential. A missing *folder* is asked too, by a clone that fetches trees
// and no files; only a definite "no" refuses, never a clone that failed.
func (s *Server) refusedUnreachableRepo(ctx context.Context, w http.ResponseWriter, req CreateAppRequest) bool {
	if s.repoPreflight == nil || req.GitConnectionID != "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := s.repoPreflight(ctx, req.RepoURL, req.Branch, req.GitToken)
	if err == nil {
		return s.refusedMissingSubdir(ctx, w, req)
	}
	class := builder.ClassifyGitError(err.Error())
	if !class.NeedsAttention() {
		return false
	}
	cred := gitconn.Credential{Source: gitconn.SourceNone}
	if req.GitToken != "" {
		cred.Source = gitconn.SourceAppToken
	}
	msg := class.Explain(req.RepoURL, req.Branch, cred)
	if class == builder.GitFailureBranch {
		// Explain's "no longer exists" is the poller's sentence about a branch
		// that went away; at create nothing has gone anywhere.
		msg = fmt.Sprintf("%s has no branch %q — check the branch name.", req.RepoURL, req.Branch)
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":     msg,
		"code":      "source_unavailable",
		"retryable": false,
		"hint":      "check repo_url and branch, and pass git_token when the repository is private",
	})
	return true
}

// refusedMissingSubdir is the folder half of refusedUnreachableRepo, asked
// once the repository and branch answered.
func (s *Server) refusedMissingSubdir(ctx context.Context, w http.ResponseWriter, req CreateAppRequest) bool {
	if s.subdirPreflight == nil || req.RepoSubdir == "" {
		return false
	}
	exists, err := s.subdirPreflight(ctx, req.RepoURL, req.Branch, req.GitToken, req.RepoSubdir)
	if err != nil || exists {
		return false
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error": fmt.Sprintf("%s has no folder %q on branch %q — check the folder's path from the repository root.",
			req.RepoURL, req.RepoSubdir, req.Branch),
		"code":      "source_unavailable",
		"retryable": false,
		"hint":      "pass repo_subdir as the folder's path from the repository root, like apps/web, or leave it out to build from the root",
	})
	return true
}

// refusedTakenSlug answers 409 name_taken when an exact slug is in use. Before
// anything is allocated, so a refused create leaves no app row behind.
func (s *Server) refusedTakenSlug(w http.ResponseWriter, slug string) bool {
	exists, err := s.db.SlugExists(slug)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return true
	}
	if !exists {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": fmt.Sprintf("the slug %q is taken", slug),
		"code":  "name_taken",
		"hint": fmt.Sprintf("if that app is yours, link this directory to it and deploy: "+
			"`toft link %s` then `toft deploy`; otherwise choose another slug", slug),
	})
	return true
}

func (s *Server) ensureUniqueSlug(slug string) (string, error) {
	exists, err := s.db.SlugExists(slug)
	if err != nil {
		return "", err
	}
	if !exists {
		return slug, nil
	}

	for i := 2; i < 100; i++ {
		// The suffix shortens the base rather than overrunning maxSlugLen.
		suffix := fmt.Sprintf("-%d", i)
		candidate := fitSlug(slug, maxSlugLen-len(suffix)) + suffix
		exists, err := s.db.SlugExists(candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not generate unique slug")
}

// appHasWorkload reports whether there is a running or restartable process
// whose configuration could be behind the app's saved variables.
//
// `building` and `failed` are excluded because the deploy in flight — or the
// one that has to come next — applies everything, so a "restart to apply"
// notice beside them would name an action that is both unavailable and
// unnecessary. A `stopped` or `sleeping` app *is* included: its next start is a
// real start, and the person who saved a variable is entitled to know it has
// not taken effect yet.
func appHasWorkload(app *database.App) bool {
	switch app.Status {
	case database.AppStatusRunning, database.AppStatusCrashed,
		database.AppStatusStopped, database.AppStatusSleeping:
		return app.CurrentRelease != ""
	}
	return false
}

// refuseNoService answers 409 `no_service` for a batch app (ADR §107), which has
// no service to start, stop or restart. 409 rather than 400: the request is
// well formed and what stands in the way is what the app is. The answer to
// "stop it running" is to lock it or switch its jobs off.
func (s *Server) refuseNoService(w http.ResponseWriter, app *database.App) bool {
	if !builder.IsBatchFramework(app.Framework) {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": builder.ErrNoService.Error(),
		"code":  "no_service",
		"hint":  "a [batch] app's work is its jobs: `toft job list` shows them and `toft job run <job>` runs one now",
	})
	return true
}

// refuseEnvNames answers 400 for the first name an app may not give its own
// variable — not a POSIX name (`invalid_env_name`), or one the platform sets
// itself (`reserved_env_name`, runner.PlatformEnvNames) — and reports whether
// it did. Names in stored are exempt: they were saved before the check, and
// refusing them would refuse every resubmission of the form that holds them.
//
// The error envelope carries `name` beside the code, so a client can point at
// the field without parsing the sentence.
func refuseEnvNames[V any](w http.ResponseWriter, vars map[string]V, stored map[string]database.EnvVar) bool {
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names) // the same refusal for the same request, every time
	for _, k := range names {
		if _, ok := stored[k]; ok {
			continue
		}
		if code, msg := runner.EnvNameProblem(k); code != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": code, "name": k, "hint": "rename the variable: letters, digits and underscores, not starting with a digit, and not a name the platform sets itself"})
			return true
		}
	}
	return false
}

// auditCommitted records a settings change that has already committed through
// its own setter. These three (retention, failure notifications, the sleep
// exemption) decide nothing about who may do what, so a failed record must not
// undo the change — but it is logged rather than dropped without a word.
func (s *Server) auditCommitted(e database.AuditEntry) {
	if err := s.db.WriteAuditRow(e); err != nil {
		log.Printf("audit: %s on %s %s not recorded: %v", e.Action, e.TargetType, e.TargetID, err)
	}
}
