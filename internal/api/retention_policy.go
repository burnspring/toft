package api

import (
	"log"
	"time"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The per-app version cap, as the deploy paths and the app page see it.
//
// The cap is enforced where a build is *started*, not inside the builder. A
// build that has begun has already cloned, installed and compiled; refusing it
// at the end would burn the whole cost to produce a failure, and refusing it at
// the start is a message the person who pressed Deploy actually reads.
//
// Rollback is never blocked, deliberately. It republishes nothing — the release
// it makes current already exists and already counts — so blocking it would add
// no storage discipline, and it is the one operation an app at the cap with a
// broken release depends on. An owner would have to delete versions to escape a
// bad deploy, which is the moment they least want to be deleting things.

// versionPolicy is an app's resolved retention together with how many versions
// it actually has, or -1 when the count was not needed.
type versionPolicy struct {
	database.Retention
	// Versions is the count of builds that can still be made current, or -1
	// when nothing counted them. Counting walks the release directories, so it
	// is done only when the answer can change a decision or is shown to
	// someone: with automatic cleanup on, the count cannot reach the cap and
	// the page says so rather than quoting a number.
	Versions int
}

// versionPolicyFor resolves the cap for one app.
//
// The policy itself is a database question and is answered here; only the
// count needs the builder, because locating a release is that package's job
// alone. Keeping the two apart is what stops an app detail page — a read —
// from depending on the build subsystem being wired up at all.
func (s *Server) versionPolicyFor(app *database.App) (versionPolicy, error) {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		return versionPolicy{}, err
	}
	p := versionPolicy{Retention: database.ResolveRetention(app, settings), Versions: -1}
	if !p.NeedsCount() || s.builder == nil {
		return p, nil
	}
	if p.Versions, err = s.builder.CountAppVersions(app); err != nil {
		return versionPolicy{}, err
	}
	return p, nil
}

// blocked returns the refusal to show, or "" when the deploy may proceed.
func (p versionPolicy) blocked() string { return p.BlockReason(p.Versions) }

// appPolicyResponse is what an app's own page says the platform has decided
// about it. Every field is read-only to its owner, which is the point: the
// alternative to showing these is an owner meeting them as a deploy that
// refuses for no visible reason, or an app that keeps going to sleep.
type appPolicyResponse struct {
	// MaxVersions is the platform cap, 0 when there is none. EffectiveKeep is
	// how many versions this app actually keeps, and RequestedKeep what its
	// owner asked for — equal unless the cap lowered it, which is what
	// KeepConflict reports.
	MaxVersions   int  `json:"max_versions_per_app"`
	EffectiveKeep int  `json:"effective_releases_kept"`
	RequestedKeep int  `json:"requested_releases_kept"`
	KeepConflict  bool `json:"releases_kept_conflict"`
	// Versions is how many builds can still be made current, or null when
	// nothing counted them (see versionPolicy.Versions).
	Versions *int `json:"versions,omitempty"`
	// DeploysBlocked is true when the next deploy will be refused, and
	// BlockedReason is the same sentence the deploy would return. Shown before
	// someone presses Deploy rather than after.
	DeploysBlocked bool   `json:"deploys_blocked"`
	BlockedReason  string `json:"blocked_reason,omitempty"`

	// SourceAllowed is whether this app's repository is still one this install
	// deploys from, and SourceReason the sentence a deploy would refuse with.
	//
	// Here for the reason the rest of this block is: an administrator can
	// restrict where apps may come from, and an app whose repository falls off
	// the list keeps serving but stops deploying. Without this, its owner meets
	// that decision as a Deploy button that 403s — and /system/settings, where
	// the rule lives, 404s to them.
	//
	// Always true for an uploaded app: it has no remote, and the upload switch
	// is checked at creation rather than here.
	SourceAllowed bool   `json:"source_allowed"`
	SourceReason  string `json:"source_reason,omitempty"`

	// PlatformMemoryLimitMB is the install's own ceiling, which a per-app one
	// is set *against*. Here for the reason MaxVersions is: an administrator
	// setting a per-app limit on the app's own page cannot read
	// /system/settings from a page an owner may also open, so the figure they
	// are departing from was stated nowhere — and "this app uses the
	// platform's limit" is a sentence about a number the page would not print.
	// 0 means the install has none.
	PlatformMemoryLimitMB int `json:"platform_memory_limit_mb"`
	// PlatformCPULimitMillicores is the install's CPU limit per app, resolved,
	// for the same reason; 0 means none.
	PlatformCPULimitMillicores int `json:"platform_cpu_limit_millicores"`

	// IdleSleepMinutes is the timeout for this app's running mode — the
	// shared or the isolated setting (ADR §16) — 0 when that mode is off.
	IdleSleepMinutes int `json:"idle_sleep_minutes"`
	// Sleeps is whether this app is actually subject to it, and SleepExemption
	// names the reason it is not: "disabled" (nothing in this app's mode
	// sleeps, and nothing else does either, or it is a shared app on an
	// install that sleeps only isolated ones), "isolated" (shared apps sleep
	// on this install and isolated ones do not), "admin" (an administrator
	// exempted this app). A schedule does not hold an app awake:
	// a job runs without the app's web process (ADR §106).
	//
	// The reasons are distinguished rather than collapsed into one flag
	// because they have different owners: one whose exemption is an
	// administrator's decision needs to know they cannot change it. One "never
	// sleeps" badge would hide that.
	Sleeps         bool   `json:"sleeps"`
	SleepExemption string `json:"sleep_exemption,omitempty"`
	// SleepWindow is "19:00–07:00 Europe/London" for an isolated app that
	// sleeps only inside the install's window (ADR §16), and absent otherwise:
	// an owner whose pod is cold every morning is owed the hours it may sleep.
	SleepWindow string `json:"sleep_window,omitempty"`

	// Integrations is every kind attached to this app, with whether an
	// administrator has since switched it off and where its messages land.
	//
	// Here for the reason the sleep exemption is: an administrator's decision
	// about *this app* is one its owner is owed sight of. Without it, "why did
	// nothing appear in our channel" is unanswerable from the app's own page,
	// and the honest answer — an administrator switched it off — is invisible.
	// Read-only, like everything else in this block; changing it is an admin
	// route, and choosing the destination is a route of its own.
	//
	// A list rather than two booleans per kind, which is what it was when there
	// were two kinds. Eight pairs of fields on a response every app page reads
	// is the shape the catalog exists to stop.
	Integrations []appPolicyIntegration `json:"integrations"`

	// LogCopy is where this app's output goes beyond its own Logs tab: copied
	// to the platform's stream (shared), with whether the cap applies and how
	// many lines it has refused since the platform started; or the cluster's
	// log agent (isolated), which the platform cannot see and never claims.
	// Absent where this process runs no copy at all.
	LogCopy *appLogCopy `json:"log_copy,omitempty"`

	// ViewerIdentity is whether this app may learn who is looking at it, and is
	// **absent for a viewer** — the same gate the dependency block below uses,
	// which is the one the app's logs and deployment history already use.
	//
	// Here for exactly the reason the Slack facts are: an administrator's
	// decision about *this app* is one its owner is owed sight of. Off is the
	// ordinary state of a new app, so without this "why does toft.who() keep
	// raising" is unanswerable from the app's own page, and the honest answer —
	// nobody has enabled it — is invisible. Read-only like the rest of the
	// block; the write is admin-only on PATCH /apps/{id}.
	ViewerIdentity *bool `json:"viewer_identity,omitempty"`

	// Gallery is where this app came from, when it came from the catalog and
	// the gallery is switched on. Absent otherwise — including for an app that
	// *did* come from the gallery on an install where an administrator has
	// since turned it off, because off means the gallery is not part of this
	// install and a page that went on mentioning it would be describing
	// something the viewer cannot act on.
	Gallery *appGalleryPolicy `json:"gallery,omitempty"`

	// Dependencies is what this install's dependency policy has decided about
	// this app (ADR §39). Absent while the feature is off, and **absent for a
	// viewer** — the same reasoning that hides the tab from one: they can do
	// nothing with a finding, and a package-and-version manifest of a running
	// internal tool is a target map. §20 is about the people who answer for
	// the app.
	//
	// It rides the app request the page already makes, and that is a
	// requirement rather than an optimization: the header and the tab strip
	// render before anything on a tab has been fetched, so a signal that costs
	// a second round trip is a signal that appears late or not at all.
	Dependencies *appDependencyPolicy `json:"dependencies,omitempty"`
}

// appDependencyPolicy is the app's own view of the dependency policy.
//
// Everything here is read-only to its owner, which is the point: without it an
// administrator's threshold change is met as a deploy that mysteriously
// refuses, and /system/settings — where the rule lives — 404s to them.
type appDependencyPolicy struct {
	Mode      string `json:"mode"`
	Enforcing bool   `json:"enforcing"`

	// ScannedAt and FeedAsOf scope every number here. **Scanned false never
	// renders as zero findings** — it is the state most of an estate is in on
	// the first day — and ScanReason distinguishes the two causes, because a
	// redeploy fixes one and would change nothing about the other.
	Scanned    bool       `json:"scanned"`
	ScanReason string     `json:"scan_reason,omitempty"`
	ScannedAt  *time.Time `json:"scanned_at,omitempty"`
	FeedAsOf   *time.Time `json:"feed_as_of,omitempty"`
	FeedStale  bool       `json:"feed_stale"`

	// Release is which release the findings describe, so the tab and the
	// header cannot disagree about that.
	Release string `json:"release,omitempty"`

	Counts dependencyCounts `json:"counts"`

	// DeploysBlocked is whether the *next* deploy would be refused, and
	// BlockedReason is the same sentence the deploy would return — the one
	// wording, in a fifth place. Shown before somebody presses Redeploy rather
	// than after.
	DeploysBlocked bool   `json:"deploys_blocked"`
	BlockedReason  string `json:"blocked_reason,omitempty"`

	// MalwareServing is the one condition that earns a notice in **every**
	// mode, including warn and including an install whose threshold would not
	// block it: there is no low-severity credential stealer, so there is no
	// mode in which somebody should have to go looking.
	MalwareServing bool `json:"malware_serving"`

	// Waivers are this app's live ones with their expiry, so the row can read
	// "expires in 6 days" rather than a bare date. A lapsing waiver is the one
	// transition nobody performs, and it must not be a surprise.
	Waivers []database.DependencyWaiver `json:"waivers"`
}

// appGalleryPolicy is the app's own view of its catalog attachment.
//
// It carries five distinguishable states, and keeping them apart is the whole
// job: up to date; an update is available; the item is no longer in the
// catalog; the versions cannot be compared; and — for an example — there is no
// update channel at all. Collapsing any of them into "up to date" would be the
// platform claiming something it does not know.
type appGalleryPolicy struct {
	ItemID string `json:"item_id"`
	Kind   string `json:"kind"`
	// Name and Source come from the catalog when the item is still listed, so
	// an app page can say what it is rather than showing a GUID.
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`

	// InstalledVersion is what is running: written when a build succeeded, so
	// it is empty on an app whose first build has not finished.
	InstalledVersion string `json:"installed_version,omitempty"`
	// CatalogVersion is what the catalog offers now, empty when the item is
	// retired.
	CatalogVersion string `json:"catalog_version,omitempty"`

	// Retired means the item is no longer listed. No update can be offered, and
	// this state must never render as "up to date".
	Retired bool `json:"retired"`
	// VersionUnknown means one of the two versions could not be read. Same
	// rule: "cannot tell" is not "current".
	VersionUnknown bool `json:"version_unknown"`
	// VersionNotRecorded is why, in the commonest case: nothing was written
	// down at install, rather than something written down that will not parse.
	// A different sentence, because "cannot be compared" reads as a fault in an
	// app that is working. Set only once the app has deployed — before that an
	// empty version means the first build has not landed.
	VersionNotRecorded bool `json:"version_not_recorded"`
	UpdateAvailable    bool `json:"update_available"`
	// Updatable is whether this app has an update channel at all. False for
	// every example, by design: an example is a snapshot its owner owns from
	// the moment it lands.
	Updatable bool `json:"updatable"`
}

// appPolicy assembles the read-only policy block for one app.
//
// Failures degrade rather than propagate: this is explanatory material on a
// page that has plenty else to show, and an app detail request must not 500
// because the settings table was briefly unavailable.
func (s *Server) appPolicy(app *database.App, canEdit bool) appPolicyResponse {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		log.Printf("retention: reading platform settings for %s's policy: %v", app.Slug, err)
		// SourceAllowed is set explicitly: it is the one field here whose zero
		// value is an accusation. A page that said "blocked" because a settings
		// read failed would send an owner to an administrator over nothing, and
		// the deploy paths do their own checking regardless.
		return appPolicyResponse{SourceAllowed: true}
	}

	p, err := s.versionPolicyFor(app)
	if err != nil {
		log.Printf("retention: resolving %s's version policy: %v", app.Slug, err)
		p = versionPolicy{Retention: database.ResolveRetention(app, settings), Versions: -1}
	}

	resp := appPolicyResponse{
		MaxVersions:           p.Max,
		EffectiveKeep:         p.Keep,
		RequestedKeep:         p.Requested,
		KeepConflict:          p.Conflict,
		PlatformMemoryLimitMB: settings.AppMemoryLimitMB,
		IdleSleepMinutes:      settings.IdleSleepMinutesFor(app.RunningModeOrShared() == runtime.ModeIsolated),

		PlatformCPULimitMillicores: s.envelope().AppCPULimitMillicores,
	}
	if p.Versions >= 0 {
		n := p.Versions
		resp.Versions = &n
	}
	if why := p.blocked(); why != "" {
		resp.DeploysBlocked, resp.BlockedReason = true, why
	}

	resp.SourceAllowed, resp.SourceReason = s.appSourcePolicy(app)
	resp.Sleeps, resp.SleepExemption = s.sleepPolicy(app, settings)
	if resp.Sleeps && app.RunningModeOrShared() == runtime.ModeIsolated && settings.IsolatedSleepWindowEnabled {
		resp.SleepWindow = settings.IsolatedSleepWindowStart + "–" + settings.IsolatedSleepWindowEnd +
			" " + settings.SleepWindowLocation().String()
	}
	resp.Gallery = s.galleryPolicy(app)
	resp.Integrations = s.integrationsPolicy(app)
	resp.LogCopy = s.logCopyPolicy(app)
	if !canEdit {
		// The read-only summary a reader is promised: which kinds, titled, and
		// whether each is on — not which channel it posts to, which the
		// integrations route keeps from a viewer.
		for i := range resp.Integrations {
			resp.Integrations[i].Destination = ""
			resp.Integrations[i].Placements = 0
		}
	}
	if canEdit {
		resp.Dependencies = s.dependencyPolicyFor(app)
		on := app.ViewerIdentity
		resp.ViewerIdentity = &on
	}
	return resp
}

// appLogCopy is the log_copy block.
type appLogCopy struct {
	Mode              string `json:"mode"` // copied | cluster
	Capped            *bool  `json:"capped,omitempty"`
	DroppedSinceStart *int64 `json:"dropped_since_start,omitempty"`
}

func (s *Server) logCopyPolicy(app *database.App) *appLogCopy {
	if app.DesiredMode() == runtime.ModeIsolated {
		return &appLogCopy{Mode: "cluster"}
	}
	if s.appOutput == nil {
		return nil
	}
	capped := !s.appOutput.Uncapped()
	dropped := s.appOutput.DroppedSinceStart(app.ID)
	return &appLogCopy{Mode: "copied", Capped: &capped, DroppedSinceStart: &dropped}
}

// appPolicyIntegration is one attached kind, as the app's own page reports it.
type appPolicyIntegration struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// Enabled is the administrator's per-app switch. An owner sees it and
	// cannot change it — ADR §20: a platform decision about one app is visible
	// on that app.
	Enabled bool `json:"enabled"`
	// Destination is where the messages actually land, which is the app's own
	// choice when its people made one and the connection's default otherwise.
	// Naming the default rather than reporting an absence is deliberate:
	// "posting to #admin" is something an owner can act on, and "no channel set"
	// reads like the integration is broken.
	Destination string `json:"destination,omitempty"`
	// Placements is how many objects this app has left in that system, for a
	// kind that leaves any. It is what the delete confirmation reads.
	Placements int `json:"placements,omitempty"`
}

// integrationsPolicy is this app's attachments, as facts about the app rather
// than about the install.
//
// It degrades to "nothing attached" on any error, for the reason the rest of
// this file does: explanatory material must not 500 an app page.
func (s *Server) integrationsPolicy(app *database.App) []appPolicyIntegration {
	out := []appPolicyIntegration{}
	rows, err := s.db.ListAppIntegrations(app.ID)
	if err != nil {
		log.Printf("integrations: reading %s's attachments for its policy: %v", app.Slug, err)
		return out
	}
	residue := false
	for _, row := range rows {
		kind := appint.Lookup(row.Kind)
		if kind == nil {
			continue
		}
		entry := appPolicyIntegration{Kind: row.Kind, Title: kind.Title, Enabled: row.Enabled}
		entry.Destination = row.Destination.Label
		if entry.Destination == "" {
			entry.Destination = row.Destination.ID
		}
		if entry.Destination == "" {
			if conn, err := s.db.GetConnection(row.ConnectionID); err == nil && conn != nil {
				entry.Destination = conn.DefaultDestination.Label
				if entry.Destination == "" {
					entry.Destination = conn.DefaultDestination.ID
				}
			}
		}
		residue = residue || kind.LeavesResidue
		out = append(out, entry)
	}
	// Counted once, and only where a kind that leaves residue is attached:
	// every other app pays nothing for a question that cannot have an answer.
	if residue {
		if n, err := s.db.CountAppPlacements(app.ID); err == nil && n > 0 {
			for i := range out {
				if k := appint.Lookup(out[i].Kind); k != nil && k.LeavesResidue {
					out[i].Placements = n
				}
			}
		}
	}
	return out
}

// sleepPolicy answers whether an app is subject to idle sleep, and why not.
//
// The order is the reaper's own: the checks it makes, in the order it makes
// them, so the page cannot claim an app is held awake by a schedule when the
// sweep would have skipped it two conditions earlier.
func (s *Server) sleepPolicy(app *database.App, settings database.PlatformSettings) (bool, string) {
	isolated := app.RunningModeOrShared() == runtime.ModeIsolated
	if settings.IdleSleepMinutesFor(isolated) <= 0 {
		// Named apart from "disabled" for the one case the owner could
		// otherwise not tell from an install that sleeps nothing: their
		// neighbours in shared mode are slept and their app is not, because
		// of where it runs.
		if isolated && settings.IdleSleepMinutes > 0 {
			return false, "isolated"
		}
		return false, "disabled"
	}
	if app.NeverSleep {
		return false, "admin"
	}
	return true, ""
}

// capBlocks answers the deploy handlers: the refusal to send, or "" to go
// ahead. A settings read that fails does not block the deploy — a cap the
// platform cannot read is not a cap it should enforce, and the alternative is
// an install where nobody can deploy because one query is failing.
func (s *Server) capBlocks(app *database.App) string {
	p, err := s.versionPolicyFor(app)
	if err != nil {
		log.Printf("retention: resolving the version cap for %s, allowing the deploy: %v", app.Slug, err)
		return ""
	}
	return p.blocked()
}
