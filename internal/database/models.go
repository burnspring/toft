package database

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"toft/internal/auth"
	"toft/internal/runtime"
)

// Port is an app's listener port. Zero means "not yet assigned" and is stored
// as NULL so that the UNIQUE constraint on apps.port does not treat every
// unassigned app as a collision — a port is only claimed once a release is
// actually listening on it.
type Port int

func (p *Port) Scan(value interface{}) error {
	if value == nil {
		*p = 0
		return nil
	}
	switch v := value.(type) {
	case int64:
		*p = Port(v)
	case int:
		*p = Port(v)
	default:
		return fmt.Errorf("cannot scan %T into Port", value)
	}
	return nil
}

func (p Port) Value() (driver.Value, error) {
	if p == 0 {
		return nil, nil
	}
	return int64(p), nil
}

type User struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	// Name is the person's display name as their provider last asserted it,
	// refreshed on every sign-in. Empty when the provider does not say one —
	// callers fall back to Login rather than inventing a name.
	Name       string `json:"name"`
	Email      string `json:"email"`
	AvatarURL  string `json:"avatar_url"`
	SystemRole string `json:"system_role"`
	// LockedAt marks a locked account (nil = active). Locked users cannot log
	// in, their live sessions fail the middleware's session check, and they are
	// invalid transfer/grant targets. Apps they own keep running.
	LockedAt  *time.Time `json:"locked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	// LastLoginAt is when the user last minted a session (nil = never signed
	// in since the column existed). Written by RecordLogin at the single
	// session-minting choke point, so it tracks sign-ins, not activity.
	//
	// It is deliberately **not** the dormancy clock. A personal API key
	// authenticates its owner without minting a session, so an account driving
	// a nightly job every night for a year has one sign-in here and would look
	// like the most dormant account on the install. UserActivity is the clock;
	// this is one of its four inputs.
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	// UnlockedAt is when an administrator last reinstated this account (nil =
	// never locked, or never unlocked since the column existed). It is a
	// dormancy-clock input for the reason 00047 gives: an unlock is somebody
	// asserting the account should be usable now, and without it the next sweep
	// would undo every unlock it had caused.
	UnlockedAt *time.Time `json:"unlocked_at,omitempty"`
	// PendingActivationAt marks an account a provider created under a
	// `default_role` of `pending` and no administrator has activated yet (nil =
	// an ordinary account, which is every account on an install that never
	// turns this on). Such an account exists, holds a role it cannot use, and
	// is refused at every sign-in until it is activated — see migration 00028.
	//
	// It is not a lock and does not become one: locking is an administrator
	// taking something away, with a reason and an unlock that resets the
	// dormancy clock; this is an account that was never finished.
	PendingActivationAt *time.Time `json:"pending_activation_at,omitempty"`
	// ScannerSince marks the account an administrator named as the scanner
	// (nil = not one). It is believed for X-Toft-Scan and admitted to the
	// estate read, and no authorization path reads it — see
	// SetScannerAccount.
	ScannerSince *time.Time `json:"scanner_since,omitempty"`
}

// Locked reports whether the account is currently locked.
func (u *User) Locked() bool { return u.LockedAt != nil }

// PendingActivation reports whether the account is still waiting for an
// administrator to activate it. Separate from Locked() on purpose: the two
// refusals are different facts, are told to the person in different words, and
// an account can be neither, either, or — if an administrator locks one before
// activating it — both.
func (u *User) PendingActivation() bool { return u.PendingActivationAt != nil }

type App struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// OwnerID is the owning *user*, and is empty when a group owns the app.
	// OwnerGroupID is the other half: exactly one of the two is set, which the
	// apps_one_owner CHECK makes a database fact rather than a convention.
	//
	// The pair is deliberately not collapsed into (type, id). Postgres cannot
	// put a conditional foreign key on a polymorphic column, and keeping both
	// keys real is what makes "a group that owns apps cannot be deleted" a
	// refusal from the database rather than a rule someone has to remember.
	//
	// Read them through OwnerPrincipal() rather than testing OwnerID for
	// emptiness at each call site. Code that asks "is this person the owner"
	// may still compare OwnerID directly — that question has the same answer it
	// always had, and is false for a group-owned app, which is correct.
	OwnerID      string `json:"owner_id"`
	OwnerGroupID string `json:"owner_group_id,omitempty"`
	RepoURL      string `json:"repo_url"`
	Branch       string `json:"branch"`
	// RepoSubdir is the folder of RepoURL the app is built from; empty is the
	// repository root. builder.ValidateSubdir is its one check.
	RepoSubdir string `json:"repo_subdir"`
	Framework  string `json:"framework"`
	// Port and EndpointHost together are the app's address, and they are
	// whatever the backend that started the workload reported — never a value
	// a caller chose. Read them through Endpoint(); the gateway, cron trigger,
	// readiness wait and reconciler all go through that one accessor so that
	// none of them can quietly reintroduce "an app is a port on this machine."
	Port         Port   `json:"port"`
	EndpointHost string `json:"-"`
	Status       string `json:"status"`
	Visibility   string `json:"visibility"`
	// FrameAncestors is the origins allowed to put this app in an iframe,
	// beyond the install itself. Empty means the install and nothing else —
	// absent is not "anyone", which is the whole point of the column.
	FrameAncestors FrameAncestors `json:"frame_ancestors"`
	// LegacyPrefixRewrite opts an app into proxy-side HTML/redirect rewriting.
	// The default is build-time base paths (see internal/builder/basepath.go);
	// this is an escape hatch for apps that cannot be told their mount point.
	LegacyPrefixRewrite bool `json:"legacy_prefix_rewrite"`
	// CurrentRelease is the process-manager name of the release currently
	// serving traffic. Releases are named per-deployment so a new one can run
	// alongside the old until the proxy is flipped.
	CurrentRelease string `json:"current_release"`
	// ReleaseRef is what the current release *is*, as an opaque handle only a
	// backend resolves: a local directory, or a packed tree in the artifact
	// store. Empty means the directory the `current` symlink points at, which
	// is how every release behaved before artifacts existed.
	ReleaseRef string `json:"-"`
	// ReleaseBasePath is the base path the serving release was built for —
	// "/apps/{slug}", or "" at the root of a hostname — and nil when nothing
	// serves. Written by SetAppRelease beside CurrentRelease, so it changes at
	// the instant the proxy starts reaching a different release.
	//
	// **Serving reads this, never the slug or the hostname.** Those say where
	// the app should live; this says where the code running now was told it
	// lives, and the two differ for the length of every move. Read it through
	// builder.ServingBasePath.
	ReleaseBasePath *string `json:"-"`
	// Hostname is a DNS name of the app's own, set by an administrator, and
	// HostnameEnabled whether it is served. Empty when none. Never written by
	// UpdateApp: SetAppHostname, audited.
	Hostname        string `json:"hostname,omitempty"`
	HostnameEnabled bool   `json:"hostname_enabled"`
	// PublicURL is where a visitor reaches the app now — its own hostname
	// once its release lives there, its gateway path otherwise. Not a column:
	// the API fills it from builder.PublicURL on every read that returns an
	// app, so the SPA and the CLI never compose an app's address themselves.
	PublicURL string `json:"public_url,omitempty"`
	// ExecutionMode is "shared" or "isolated" — never a backend name. See
	// runtime.Mode; it is the whole vocabulary a developer, the API, and this
	// row ever use for where an app runs.
	//
	// It is the *desired* mode. An administrator may change it freely and in
	// either direction; doing so disturbs nothing running, because what the
	// runtime honours is RunningMode below, which only ever changes on a
	// deployment.
	ExecutionMode string `json:"execution_mode"`
	// RunningMode is the mode the *current release* was actually started under,
	// and therefore which backend owns its workload. Written in the same flip
	// as CurrentRelease and EndpointHost, which describe the same live thing.
	//
	// Read it through RunningModeOrShared: an empty value is a row written
	// before modes were per-deployment, and every one of those is a process.
	RunningMode string `json:"running_mode"`
	// BuildTimeoutSeconds overrides the install's build timeout for this app.
	//
	// NULL — nil here — means "use the install's setting", which is the whole
	// of the default: an app that never sets it behaves exactly as it did.
	//
	// It is per-app because builds share a few slots (MAX_CONCURRENT_BUILDS), so
	// the build timeout is also the longest one deploy can hold a slot others wait for.
	// The app that needs twenty minutes for an renv restore or a Quarto render
	// is usually one app, and raising the install's floor to accommodate it
	// would make every other app's worst case twenty minutes as well.
	BuildTimeoutSeconds *int `json:"build_timeout_seconds,omitempty"`
	// MemoryLimitMB is this app's own memory ceiling, when an administrator has
	// set one. **0 means the install's number, never "uncapped"** — the same
	// rule the settings row follows, and for the same reason: an unset value
	// has to change nothing, and there must be no path from a form to a
	// workload with no cap at all.
	//
	// It is per-app because the install's ceiling is arithmetic about a
	// *machine*, and raising it to suit the one report that genuinely needs
	// 4 GB raises it for every app on the box. It is the administrator's and
	// not the owner's, because a ceiling an app can raise for itself is not a
	// ceiling — the owner reads it, an administrator writes it, and the write
	// is audited like every other decision the platform makes about somebody
	// else's app.
	//
	// Mebibytes, for the reason the settings row is: it is what both backends
	// count in. The API offers megabytes and gigabytes over it.
	MemoryLimitMB int `json:"memory_limit_mb"`
	// CPULimitMillicores is this app's own CPU limit, by the same rules as
	// MemoryLimitMB: an administrator's, and 0 is the install's number.
	CPULimitMillicores int `json:"cpu_limit_millicores"`
	// Entrypoint and StartCmd are the framework detector's findings, persisted.
	//
	// The shared backend re-detects them from the release directory whenever it
	// needs them, because it has the directory. An isolated workload does not —
	// it asks the platform what to run at bootstrap, and the platform must be
	// able to answer without a filesystem it no longer has.
	Entrypoint string `json:"-"`
	StartCmd   string `json:"-"`
	// AutoDeploy opts the app into the poller (internal/autodeploy): new
	// commits on the tracked branch deploy automatically.
	AutoDeploy bool `json:"auto_deploy"`
	// LastSeenSHA is the newest remote commit already claimed for a build.
	// Written only through ClaimAutoDeploySHA / SetLastSeen / SetAutoDeploy —
	// never through UpdateApp, so a long build holding a stale App cannot
	// clobber a concurrent claim.
	LastSeenSHA string `json:"-"`
	// LastSeenTree is repo_subdir's git tree id at the commit the last
	// successful build cloned; the poller claims a new commit without building
	// when the folder's tree there is the same. Written by SetLastSeen, cleared
	// wherever LastSeenSHA is, "" for an app at the repository root.
	LastSeenTree string `json:"-"`
	// AutoDeleteReleases and ReleasesKept are the owner's half of release
	// retention: whether a deploy prunes older versions, and how many to keep.
	// Never read raw — ResolveRetention combines them with the platform cap,
	// and the number applied is not always the number stored.
	AutoDeleteReleases bool `json:"auto_delete_releases"`
	ReleasesKept       int  `json:"releases_kept"`
	// EffectiveReleasesKept is the number a deploy actually keeps —
	// ResolveRetention's Keep, the default 3 when ReleasesKept is 0 ("no
	// opinion"), lowered to the platform cap. Not a column: the API fills it
	// on one app's read, so `releases_kept: 0` on a new app is never read as
	// "keeps none". Omitted where nothing filled it.
	EffectiveReleasesKept int `json:"effective_releases_kept,omitempty"`
	// NeverSleep exempts this app from idle sleep. An administrator's setting,
	// not the owner's: the owner sees it and the reason for it (ADR §16), and
	// the API refuses to let them change it.
	NeverSleep bool `json:"never_sleep"`
	// ViewerIdentity is whether this app may ask the platform who is looking at
	// it. An administrator's setting on the same terms as NeverSleep — the owner
	// and the editors see it and the API refuses to let them change it — because
	// naming a person to a program is a grant rather than a preference, and the
	// person named is neither the app nor its owner.
	//
	// Off is the ordinary state of a new app. GET /api/v1/app/viewer answers 403
	// while it is off, which is deliberately *not* the 204 that means nobody is
	// there: an app told "no viewer" when there is one would render its
	// signed-out state to a signed-in person and give its author no reason to
	// suspect a setting.
	ViewerIdentity bool `json:"viewer_identity"`
	// The platform lock: an administrator's decision that nobody may reach this
	// app. Nil LockedAt means not locked.
	//
	// Deliberately not a Status value. Status is the *workload's* state and this
	// is a *decision about the app*, and the two are orthogonal — a crashed app
	// can be locked and a locked app can be stopped, and neither fact erases the
	// other. More pointedly, `stopped` is a state the owner may leave (Stop and
	// Restart authorize with ActionEditContent) and a lock the owner can lift is
	// not a lock. See ADR §54 and docs/invariants.md § App locking.
	//
	// LockedMessage is what a visitor is told, and empty is meaningful rather
	// than missing: it means "use whatever this install says by default".
	// ResolveLockMessage is the only thing that should read it.
	//
	// LockedSource is who placed it: an administrator, or the dependency policy
	// by itself. It exists because `locked_by` cannot answer that — it is a
	// foreign key into users with ON DELETE SET NULL, and NULL there already
	// means both "no lock" and "a lock whose administrator has been deleted",
	// so a lock the platform placed has no id to store and must not take NULL
	// as a third meaning. Read it through LockSource, which fills in the
	// default every row written before it existed carries.
	LockedAt      *time.Time `json:"locked_at,omitempty"`
	LockedBy      string     `json:"locked_by,omitempty"`
	LockedMessage string     `json:"locked_message,omitempty"`
	LockedSource  string     `json:"locked_source,omitempty"`
	// NotifyFailures is whether this app's owners and editors are emailed
	// when it breaks. Unlike NeverSleep this is the app's own people's decision,
	// not an administrator's — anyone who may edit the app may silence it.
	//
	// It controls mail alone. Every failure is recorded in app_events whatever
	// this says, so switching it off cannot erase the platform's memory of an
	// app breaking; it only stops the mail about it.
	NotifyFailures bool `json:"notify_failures"`
	// GalleryItemID is the catalog item this app was deployed from, empty for
	// every app that did not come from the gallery. GalleryKind is that item's
	// kind, and the two mean very different things:
	//
	//   - extension: provenance that *does* something. It is what makes an
	//     in-place update possible, and while it is set the app is refused
	//     auto-deploy, because an extension tracks a catalog rather than a
	//     branch and two update channels on one app is how an app ends up on a
	//     version nobody chose.
	//   - example: provenance that does nothing at all. It exists so the
	//     gallery can say "you already deployed this one". No behaviour
	//     anywhere may branch on it — an example is an ordinary uploaded app
	//     the moment it lands, and an administrator later switching examples
	//     off must not reach back and change what somebody's working app may
	//     do.
	//
	// Cleared by detaching, and by editing the repository or branch, after
	// which the app is simply an app.
	GalleryItemID string `json:"gallery_item_id,omitempty"`
	GalleryKind   string `json:"gallery_kind,omitempty"`
	// GalleryVersion is the catalog version this app is *running*. Advanced by
	// the builder on a successful deploy only, from the value the deployment
	// carried — never at enqueue, or a failed update would leave the app
	// claiming a version it does not have.
	GalleryVersion string `json:"gallery_version,omitempty"`
	// GitToken is the encrypted HTTPS credential for private repos. Never
	// serialized; the API exposes only whether one is set.
	GitToken string `json:"-"`
	// GitConnectionID points the clone at a connected git host instead. Empty
	// is the ordinary case and always will be: `apps.git_token` covers
	// Bitbucket, a host with no connection kind, a repository outside the
	// connected account, and the install that does not want the feature.
	//
	// When both are set the connection wins — an administrator's install-wide
	// decision outranks a token one owner pasted, and a stale personal token
	// quietly beating a working connection is the failure connections exist to
	// end.
	GitConnectionID string    `json:"git_connection_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`

	// LastDeployedAt is when the *serving* release arrived — the honest version
	// of the fact three screens used to render from UpdatedAt.
	//
	// UpdatedAt is `updated_at` and is correct: it is when this row last
	// changed, and 26 of the 34 `UPDATE apps SET` statements in this package
	// touch it, the idle reaper and the health watcher among them. It is a
	// perfectly good column and three readers want it. What it is not is a
	// deploy date, and labelling a column "Last deployed" over it meant a
	// crash, a sleep or a lock reset the figure — so sorting by it during an
	// incident returned the apps that had most recently *broken*.
	//
	// Nil is *never deployed*, which is a third thing from a zero time and
	// from "we do not know": every query that selects `appColumns` computes it
	// (see lastDeployedAtExpr), so a nil here is the answer and not an
	// omission.
	LastDeployedAt *time.Time `json:"last_deployed_at,omitempty"`

	// GarthLinkedAt is when a deploy from a Garth session linked this app
	// (ADR §158); nil when none did or someone unlinked it. Never serialized:
	// the app page learns it only as `open_in_garth`, for editors.
	GarthLinkedAt *time.Time `json:"-"`

	// ServingSince is when CurrentRelease last changed — the flip of a deploy,
	// a rollback or a promote — which is the question "since when is this
	// serving" asks. LastDeployedAt cannot answer it: a rollback serves an old
	// build, whose finished_at is the day it was built. Written only by
	// UpdateApp, nil when nothing serves or the row predates migration 00061.
	ServingSince *time.Time `json:"serving_since,omitempty"`
	// StatusChangedAt is when Status last took a different value, written by
	// UpdateAppStatus and the build sweep. Nil on rows predating 00061.
	StatusChangedAt *time.Time `json:"status_changed_at,omitempty"`

	// PublicWithheld is the install's answer to whether a public app is served
	// anonymously, and it is true on every app on an install where an administrator has turned public
	// apps off — including apps that are private, which is deliberate: it is a
	// fact about the install and the page showing an owner the Public option
	// needs it whatever that app's visibility currently says.
	//
	// Derived on every read rather than stored, it travels on the row rather
	// than being read separately by whoever wants it. That is what makes
	// the substitution unforgettable: AuthObjectOf is a pure function of this
	// struct, so a handler cannot authorize from an app row that has not been
	// told. Never written through this struct — UpdateApp does not carry it.
	PublicWithheld bool `json:"public_withheld,omitempty"`

	// ListableToAll is the install-wide `list` rung: an administrator has said
	// every signed-in person may see that this app exists, without letting them
	// open any of it.
	//
	// It travels on the row for PublicWithheld's reason, and it is the mirror
	// image: that one withdraws something from every app, this one confers
	// something on every app. Both are facts about the install, both are
	// derived in appColumns, and both reach the authorizer only through
	// AuthObjectOf — so the listing and the refusal read one fact and cannot
	// disagree. An app that appears in somebody's list and 404s when they open
	// it is exactly that disagreement.
	//
	// Never written through this struct, and never serialized: unlike
	// PublicWithheld, no screen needs it — the administrator's switch reads the
	// platform settings directly, and an owner's Access tab has nothing to grey
	// out. It exists to reach AuthObjectOf, so putting it on the wire would tell
	// every caller, anonymous ones included, which way an install-wide switch is
	// set, for nobody's benefit.
	ListableToAll bool `json:"-"`

	// Listable is the owner's half of that same rung: whether *this* app appears
	// in the listing of somebody who holds nothing on it, while the switch above
	// is on. It subtracts from the install-wide rung and from nothing else, so a
	// grant always outranks it and it can never hide an app from somebody who was
	// given it.
	//
	// **It goes on the wire, and ListableToAll does not.** They look like twins
	// and are the opposite on both counts that decide it. That one is a fact about
	// the install, identical on every row, that no screen renders — the
	// administrator's switch reads the platform settings directly — so sending it
	// would tell every caller, anonymous ones included, which way an install-wide
	// switch is set, for nobody's benefit. This one is the owner's own answer, it
	// renders as a control on their Sharing tab, and that control cannot be drawn
	// without its current value. `public_withheld` is on the wire for the same
	// reason and is the closer relative.
	//
	// Written by the owner through SetAppListable, never through UpdateApp: the
	// builder holds an App loaded before a long build started, and a value written
	// through UpdateApp can be reverted by a deploy that overlaps the change.
	// Inert above `private`, and stored anyway — an owner who un-lists a private
	// app, publishes it and comes back gets their own answer back.
	Listable bool `json:"listable"`

	// BornUnlisted is the one creation-time input to Listable, and it exists
	// because Listable itself cannot carry the answer: the field defaults to
	// true, a Go bool has no third state, and every caller that does not care
	// would otherwise create an unlisted app.
	//
	// Set only by the gallery install path, from an item asking to be born
	// hidden. False — the zero value every other creation path has — is the
	// ordinary born-listable default. Never serialized, never read back, and
	// never a way to *change* an app: after creation the only writer is
	// SetAppListable, which audits the transition.
	BornUnlisted bool `json:"-"`
}

// Endpoint is where this app's workload accepts requests. A zero port means
// the app has no live workload — callers must treat that as "not addressable"
// rather than dialling anything.
//
// The host falls back to loopback only for rows written before the endpoint
// was persisted; the shared backend does bind loopback, so the fallback is
// correct for exactly the rows that can reach it.
func (a *App) Endpoint() runtime.Endpoint {
	host := a.EndpointHost
	if host == "" {
		host = runtime.BindLoopback
	}
	return runtime.Endpoint{Host: host, Port: int(a.Port)}
}

// RunningModeOrShared is the mode the live workload runs under.
//
// An empty column means a row written before the mode became a property of the
// deployment, and every workload from that era is a process in Toft's own
// footprint — so shared is the correct reading, not a guess.
func (a *App) RunningModeOrShared() runtime.Mode {
	if m, err := runtime.ParseMode(a.RunningMode); err == nil {
		return m
	}
	return runtime.ModeShared
}

// DesiredMode is the mode the *next* deployment will run under.
func (a *App) DesiredMode() runtime.Mode {
	if m, err := runtime.ParseMode(a.ExecutionMode); err == nil {
		return m
	}
	return runtime.ModeShared
}

// OwnerPrincipal is who owns this app, as the (type, id) pair the grants table
// and the transfer API speak in. Exactly one of the two columns is set, so this
// never returns an empty id for a row the database accepted.
//
// It exists so that "who owns this" is asked in one way. The alternative — each
// call site testing OwnerID for emptiness and inferring a group from the
// silence — is how the two columns would drift into meaning something slightly
// different in each of the twenty places that read them.
func (a *App) OwnerPrincipal() (principalType, principalID string) {
	if a.OwnerGroupID != "" {
		return PrincipalTypeGroup, a.OwnerGroupID
	}
	return PrincipalTypeUser, a.OwnerID
}

// GroupOwned is whether a group rather than a person owns this app. Worth a
// name because the interesting cases are all negative: a group-owned app blocks
// no user deletion, contributes no personal owner to the recipient list, and
// has no single name to render in an Owner column.
func (a *App) GroupOwned() bool { return a.OwnerGroupID != "" }

// ProcessName is the name the process manager knows this app by.
func (a *App) ProcessName() string {
	if a.CurrentRelease != "" {
		return a.CurrentRelease
	}
	return a.Slug
}

type EnvVar struct {
	ID    string `json:"id"`
	AppID string `json:"app_id"`
	Key   string `json:"key"`
	Value string `json:"value"` // encrypted
	// Sensitive values are write-only: the API never returns them once stored.
	// Visible values are configuration and are shown to anyone who may edit the
	// app. Flipping sensitive→visible requires supplying a new value — the
	// stored one is never revealed.
	Sensitive bool `json:"sensitive"`
}

type Deployment struct {
	ID        string `json:"id"`
	AppID     string `json:"app_id"`
	CommitSHA string `json:"commit_sha"`
	Branch    string `json:"branch"`
	Status    string `json:"status"`
	// TriggeredBy records what started the build: DeployTriggerManual or
	// DeployTriggerAuto.
	TriggeredBy string `json:"triggered_by"`
	// DeployedByType and DeployedByID are the principal that asked for this
	// deployment — DeployerUser, DeployerServiceAccount or DeployerDeployKey —
	// and empty when none did (auto, render, move). A service account's id is
	// its owning user's: it has no row to read later. triggered_by is the
	// category; this is who, which toft.toml's storage is judged against.
	// Written at insert and carried on the in-memory row the builder is
	// handed; not on the wire.
	DeployedByType string `json:"-"`
	DeployedByID   string `json:"-"`
	// ExecutionMode is the mode this release was built and started under, and
	// is what the runtime honours for that release's lifetime. Recorded per
	// deployment rather than read from the app so that changing an app's
	// desired mode never retroactively reclassifies history — and so a rollback
	// knows what the release it is restoring actually ran as.
	ExecutionMode string `json:"execution_mode"`
	BuildLog      string `json:"build_log"`
	// ArtifactKey is what this deployment published, or "" on an install with
	// no artifact store and on rollbacks (which republish nothing). Kept per
	// deployment so a rollback target stays locatable after the app has moved
	// on to newer releases.
	ArtifactKey string `json:"-"`
	// DiskBytes is the unpacked release tree's size on this host and
	// ArtifactBytes is the packed artifact's size in the object store. Both are
	// nil when nothing has measured them — a row older than the columns, or a
	// release whose directory and artifact are both gone. Nil is not zero: an
	// empty release really can measure zero, and rendering "unknown" as "free"
	// is how a storage report starts lying.
	DiskBytes     *int64 `json:"disk_bytes"`
	ArtifactBytes *int64 `json:"artifact_bytes"`
	// GalleryVersion is the catalog version this deployment is *trying* to
	// reach, empty for every deployment that has nothing to do with the
	// gallery. The builder copies it onto the app when — and only when — the
	// build succeeds, which is what keeps the app's recorded version a
	// statement about what is running rather than about what was asked for.
	//
	// A rollback carries the source deployment's value, so rolling the code
	// back rolls the version back with it.
	GalleryVersion string `json:"gallery_version,omitempty"`
	// BasePath is the base path this release was built for, nil on rows from
	// before the column. A rollback compares it with where the app lives now,
	// and rebuilds rather than starting a tree told a different address.
	BasePath *string `json:"base_path,omitempty"`
	// SourceDeployID is the release a rollback restores, on the row from the
	// moment it is queued. It used to ride only on the in-memory build job,
	// so a bulk release deletion between queue and worker could remove the
	// very release the person was returning to; `protects` reads this.
	SourceDeployID string `json:"source_deploy_id,omitempty"`
	// RestoredFrom is the release a rollback restored, set when it is queued
	// and never cleared: a rollback rebuilt for a new base path gives up
	// SourceDeployID (it owns its tree then) and keeps this, so a client can
	// still tell which release the row is (BB-005).
	RestoredFrom string `json:"restored_from,omitempty"`
	// RuntimeLanguage and RuntimeVersion are what this release was actually
	// built against — `python` and `3.13.7`, or `go` and `1.26.5`.
	//
	// Recorded rather than merely used, because a declared *range* makes the
	// outcome depend on install state: `>=3.12,<3.14` honestly resolves to 3.13
	// today and to 3.12 on a rebuild months later. Without the record, the only
	// evidence of which one is running is a build-log line that retention will
	// eventually take away.
	//
	// It is also what makes the feature's own worst failure mode visible. A
	// platform predating the `[runtime]` table ignores it in silence, because
	// toft.toml deliberately ignores tables its readers do not name — so a
	// developer can believe they have pinned something they have not. The
	// resolved version is written and printed on every build, pinned or not,
	// and a pin that was not honoured shows up on the deploy that ignored it.
	//
	// Empty on rows predating the columns, and empty for a static app, which has
	// no interpreter to record. Absent is never rendered as "the default".
	RuntimeLanguage string `json:"runtime_language,omitempty"`
	RuntimeVersion  string `json:"runtime_version,omitempty"`
	// CurrentStep is the build phase this deploy is in, or the one it ended at,
	// from `builder.BuildStep`. StepStartedAt is when that phase began, so a
	// live view counts up on its own rather than polling for a number the
	// server already sent. StepTimings is what each finished phase cost, as
	// JSON, written once when the build ends.
	//
	// Empty on every row that predates the columns, and the UI leans on that:
	// a nine-phase timeline drawn for a year-old deployment would be
	// fabricated rather than merely stale, so absent means "no timeline",
	// never "starts at setup".
	CurrentStep   string     `json:"current_step,omitempty"`
	StepStartedAt *time.Time `json:"step_started_at,omitempty"`
	StepTimings   string     `json:"step_timings,omitempty"`
	// Phase is which of the deploy's three stages CurrentStep belongs to —
	// upload, scan or deploy. **Derived, never stored**: it is a pure function
	// of the step (`deploysteps.PhaseOf`), applied where the row is read, so
	// there is no second column to disagree with the first about which stage a
	// deploy died in. Empty exactly when CurrentStep is empty or unrecognised.
	Phase      string     `json:"phase,omitempty"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	// FailureReason and FailureMessage are why a failed deploy failed: a key a
	// program branches on (`deploysteps.Reason`) and the paragraph a person
	// reads. Both empty on every deploy that did not fail, and on every row
	// written before the columns — which means *unclassified*, never
	// internal_error.
	//
	// On the row rather than only on the live progress frame, because a frame
	// is a snapshot and SnapshotProgress is what a reconnecting watcher
	// receives. Without these, `toft deploy --watch` on a build that had already
	// failed reported the step and nothing else.
	FailureReason string `json:"failure_reason,omitempty"`
	// FailureCode is which refusal inside FailureReason, when there is one —
	// storage_unknown under storage_refused, removed_key under
	// manifest_invalid: the code `toft check` names for the same tree.
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
	// FailureOutput is the failed start's own output as JSON
	// ({at, stream, line} per line), read by the log route alone.
	FailureOutput string `json:"-"`
	// Warnings is what this deploy noticed and carried on past, as a JSON array
	// of sentences — a `[tasks]` table in toft.toml where the file wanted
	// `[jobs]`, and anything else the manifest reader ignored.
	//
	// On the row for the reason the two above are: a progress frame is a
	// snapshot, so a warning published only on a live frame is the one thing a
	// reconnecting watcher misses — and the watcher that reconnects is the
	// unattended one. It also has to outlive the build log it is also written
	// to, which retention will eventually trim.
	//
	// Empty on every row written before the column, which means *none were
	// recorded* and never *none were found*.
	Warnings string `json:"-"`
}

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Email is where accountability reaches this group when it owns an app: a
	// rota, an alias, a ticket queue. Empty means unset, and unset means the
	// members are expanded instead — a group that has not thought about it
	// still gets told, which is the safe direction.
	//
	// Not a credential. It is read back in the clear and cleared with an empty
	// value, like email_admin_group_address and unlike anything with a hash.
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// GroupWithCount is the group-list row: the roster itself is gated per group
// (admins and members only), but its size is not sensitive.
//
// OwnsApps is the count that makes the two lifecycle refusals legible before
// they are hit — a group that owns apps cannot be deleted, and cannot be
// emptied of eligible members. A number in the list is how an administrator
// finds that out by looking rather than by being refused.
type GroupWithCount struct {
	Group
	MemberCount int `json:"member_count"`
	OwnsApps    int `json:"owns_apps"`
}

type ObjectGrant struct {
	ObjectID      string    `json:"object_id"`
	PrincipalType string    `json:"principal_type"`
	PrincipalID   string    `json:"principal_id"`
	Role          string    `json:"role"`
	GrantedBy     string    `json:"granted_by"`
	GrantedAt     time.Time `json:"granted_at"`
}

type AccessRequest struct {
	ID            string `json:"id"`
	RequesterID   string `json:"requester_id"`
	RequestType   string `json:"request_type"`
	ObjectID      string `json:"object_id"`
	RequestedRole string `json:"requested_role"`
	Status        string `json:"status"`
	// ApproverID is **who decided**, not who was asked. It is empty on a
	// pending request and written at decision time, naming the person who
	// acted — "the group approved" is not an acceptable record of a decision
	// (NIST AC-5).
	//
	// It used to be written at creation as the app's owner, which read as
	// routing. Routing is now ApproverGroupID, or the app's owner column, and
	// neither is this.
	ApproverID string `json:"approver_id"`
	// ApproverGroupID routes a request against a group-owned app to the group:
	// any member may decide it, and the one who does lands in ApproverID.
	ApproverGroupID string     `json:"approver_group_id,omitempty"`
	DecidedAt       *time.Time `json:"decided_at"`
	Reason          string     `json:"reason"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Status constants
const (
	AppStatusBuilding = "building"
	AppStatusRunning  = "running"
	AppStatusCrashed  = "crashed"
	AppStatusStopped  = "stopped"
	AppStatusFailed   = "failed"
	// AppStatusSleeping is an app the idle reaper stopped, distinct from
	// `stopped` because the two mean opposite things to every reader: a
	// sleeping app wakes itself on the next request and still picks up a git
	// push, while a stopped one was stopped by a person and must stay down
	// until another person says otherwise. Sharing one status would force a
	// choice between a working Stop button and idle sleep.
	AppStatusSleeping = "sleeping"
	// AppStatusReady is a batch app with a published release (ADR §107): built,
	// live, and serving nothing, because it has no service. A word of its own
	// rather than `running`, which the proxy, the reaper, the reachability
	// probe and the license's running count all read as "a process answers" —
	// borrowing it would make each of them quietly wrong rather than visibly
	// unhandled. TestEveryStatusReaderNamesReady holds every reader to it.
	AppStatusReady = "ready"

	DeployStatusPending  = "pending"
	DeployStatusBuilding = "building"
	DeployStatusSuccess  = "success"
	DeployStatusFailed   = "failed"

	// Deployment triggers
	DeployTriggerManual   = "manual"
	DeployTriggerAuto     = "auto"
	DeployTriggerRollback = "rollback"
	// DeployTriggerRender is a notebook's scheduled re-render. It is a deploy
	// like any other — re-clone, re-install, re-render, new release, new
	// artifact — and it is its own trigger rather than `auto` because `auto`
	// means "the poller saw a push", and a deployments list that said so about a
	// nightly report would be describing a commit that does not exist.
	DeployTriggerRender = "render"
	// DeployTriggerMove is a rebuild of the code already running, at a new
	// base path — a rename, or a hostname switched on or off. Never new code:
	// the source is the live release's tree and the commit is the live one's.
	DeployTriggerMove = "move"

	// The deploying principal's kinds (deployments.deployed_by_type).
	DeployerUser           = "user"
	DeployerServiceAccount = "service_account"
	DeployerDeployKey      = "deploy_key"

	// System roles
	SystemRoleAdmin     = "system_admin"
	SystemRolePublisher = "publisher"
	SystemRoleViewer    = "viewer"

	// ProvisionPending is a value of a provider's `default_role` and is **not a
	// system role**. It is in this block because it is chosen from the same
	// control and refused by the same validator, and nowhere near the three
	// above in effect: nothing compares `system_role` to it, nothing may store
	// it in that column, and a fourth rung on the ladder is exactly what it is
	// not. It says the account this provider creates is not finished — see
	// User.PendingActivation and migration 00028.
	ProvisionPending = "pending"

	// Object roles. Aliases, not copies: the ladder is spelled once, in
	// internal/auth, because that is the package whose comparisons an
	// out-of-step literal would silently corrupt.
	ObjectRoleOwner  = auth.ObjectRoleOwner
	ObjectRoleEditor = auth.ObjectRoleEditor
	ObjectRoleViewer = auth.ObjectRoleViewer
	ObjectRoleList   = auth.ObjectRoleList

	// Visibility
	VisibilityPublic        = "public"
	VisibilityAuthenticated = "authenticated"
	VisibilityPrivate       = "private"

	// Principal types
	PrincipalTypeUser           = "user"
	PrincipalTypeGroup          = "group"
	PrincipalTypeServiceAccount = "service_account"
	// PrincipalTypeOperator is whoever has a shell on the host: the actor behind
	// the CLI recovery commands. Never a principal in an authorization decision —
	// it exists so the audit log can distinguish "an administrator did this
	// through the UI" from "someone with root on the box did this", which are very
	// different facts about the same change.
	PrincipalTypeOperator = "operator"
	// PrincipalTypeSystem is the platform acting on its own schedule, with no
	// person behind the request at all — today, the dormancy sweep. Like
	// operator it is never a principal in an authorization decision; it exists
	// because "an administrator locked this account" and "the install locked it
	// on a timer nobody was watching" are very different facts, and an audit
	// trail that told them apart only by which admin id happened to be stamped
	// on the row would be inventing an actor.
	PrincipalTypeSystem = "system"
	// PrincipalTypeProvider is a directory acting through the SCIM feed, and it
	// is the third of these for the third of the same reason. Never a principal
	// in an authorization decision: a feed's authority is the closed list of
	// things its endpoint implements, not a role. It exists because "an
	// administrator locked this account", "the install locked it on a timer" and
	// "the directory said this person is gone" are three different facts, and an
	// audit trail that stamped the third with a borrowed identity — the
	// administrator who pasted the token months ago — would be inventing an
	// actor in exactly the way §48 refused for the sweep.
	PrincipalTypeProvider = "provider"
	// PrincipalTypeApp is an app acting as itself through /api/v1/app, on its
	// own token. Never a principal in an authorization decision either — its
	// authority there is its mappings — and it exists because "a person asked
	// for this URL" and "the app's code did" are different facts about one
	// object, and stamping the second with the app's owner would invent an
	// actor.
	PrincipalTypeApp = "app"

	// Access request
	AccessReqTypeObject      = "object_access"
	AccessReqTypePromotion   = "role_promotion"
	AccessReqStatusPending   = "pending"
	AccessReqStatusApproved  = "approved"
	AccessReqStatusDenied    = "denied"
	AccessReqStatusWithdrawn = "withdrawn"
)

// AuthObject is this app as the authorizer sees it.
//
// See [AuthObjectOf] for why the *effective* visibility is computed rather than
// read: it is the install-wide switch (§70), and this is the one place it
// applies.
func (a *App) AuthObject() *auth.Object {
	// The rung's two halves arrive here as one fact: the install says whether *anything* is listed,
	// the owner says whether *this one* is, and the authorizer has no use for the
	// difference. Narrowing it here rather than inside EffectiveObjectRole is what
	// keeps internal/auth out of this change entirely — the floor reads the same
	// field it always did, and the fact reaching it is simply smaller.
	return AuthObjectOf(a.ID, a.OwnerID, a.Visibility,
		a.PublicWithheld, a.ListableToAll && a.Listable)
}

// AuthObjectOf is the one place an app becomes an auth.Object, and therefore
// the one place the *effective* visibility is computed.
//
// **One thing withholds anonymity**: an install whose administrator has turned
// public apps off altogether (§70). It is a parameter rather than a lookup so
// that this stays a pure function of the facts handed to it.
//
// A public app whose anonymity is withheld is handed to the
// authorizer as `authenticated`. That is provably a change for one action and
// no other: matrixAllows admits ActionRead at anonymous and every other action
// at owner, editor or viewer, so nothing admits at authenticated and refuses
// at anonymous. Raising a signed-in caller from anonymous to authenticated has
// no consequence; lowering an anonymous caller out of the ladder's bottom rung
// is the entire effect. Choosing `private` instead would be a larger claim
// than any verdict supports — it would strip a signed-in colleague of access
// the owner deliberately granted.
//
// `apps.visibility` itself is never written. The owner's sentence stays the
// owner's, and it is what the page still shows.
//
// It takes the facts as well as having a method form because the access
// review builds its objects from grant rows and owned-app rows rather than from
// app rows, and two spellings of "the app as the authorizer sees it" is how a
// report about who can reach what comes to describe an install that does not
// exist.
//
// It lives here rather than in internal/api because it has a third reader now:
// internal/cronjobs resolves the role a scheduled firing carries, and a
// scheduler that computed effective visibility its own way would be the second
// spelling this comment exists to prevent.
func AuthObjectOf(id, ownerID, visibility string, anonymityWithheld, listableToAll bool) *auth.Object {
	if anonymityWithheld && visibility == VisibilityPublic {
		visibility = VisibilityAuthenticated
	}
	return &auth.Object{ID: id, OwnerID: ownerID, Visibility: visibility, ListableToAll: listableToAll}
}

// LiveStatus is what an app with a published release is: `ready` for a batch
// app, which serves nothing, and `running` for everything else. The one place
// that decision is written, for the deploy, the failed-deploy fallback and the
// orphaned-build sweep alike.
func LiveStatus(framework string) string {
	if strings.HasPrefix(framework, "batch-") {
		return AppStatusReady
	}
	return AppStatusRunning
}
