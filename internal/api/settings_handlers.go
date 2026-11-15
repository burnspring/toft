package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/runtime"
)

// capabilities answers "what does this install offer" by combining the two
// independent halves: what the operator's configuration makes *possible*, and
// what an administrator has *enabled*.
//
// Read fresh rather than cached at boot, because the enabled half changes at
// runtime and a stale answer here would either offer a capability that has
// been switched off or refuse one that has been switched on. The settings
// table has a handful of rows and this is never on the app request path.
//
// The *possible* half asks the runtimes whether a backend was actually built,
// never the configuration whether one is named. The two disagree on exactly one
// install — `ECS_CLUSTER` set, where the backend is not implemented (ADR §26,
// docs/work-remaining.md §2) — and the configuration's answer there is a lie
// that reaches all the way to enqueue: jobs would be offered, a run would be
// accepted, and the dispatcher would then find no backend and leave it
// `pending` forever. "Optional capabilities are absent, not dormant" has to
// mean absent when the *implementation* is missing, not merely when the
// operator declined to name one.
func (s *Server) capabilities() runtime.Capabilities {
	caps := runtime.Capabilities{
		Topology:         s.topology(),
		IsolatedPossible: s.rts.IsolatedConfigured(),
	}

	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		// Fail closed. A settings read that fails must not accidentally offer
		// a capability nobody turned on — the cost of being wrong in that
		// direction is spending an operator's money.
		log.Printf("settings: reading platform settings, treating capabilities as off: %v", err)
		return caps
	}
	return s.capabilitiesFrom(settings)
}

// capabilitiesFrom is the same answer for a settings row the caller already
// holds — including one it is part-way through changing.
//
// Split out so a request that turns isolated execution on and, in the same
// body, makes it the default for new apps is judged against the install it is
// creating rather than the one it is replacing.
func (s *Server) capabilitiesFrom(settings database.PlatformSettings) runtime.Capabilities {
	return runtime.Capabilities{
		Topology:         s.topology(),
		IsolatedPossible: s.rts.IsolatedConfigured(),
		IsolatedEnabled:  settings.IsolatedEnabled,
		JobsEnabled:      settings.JobsEnabled,
		HostJobsPossible: s.jobs.HostPossible(),
	}
}

// envelope resolves this install's operating dials — the memory ceiling, and
// the rest of them — as they are now.
//
// Per-read and never held, for the reason config.Envelope gives: a handler that
// answered from a number copied at process start is exactly the defect the
// envelope was introduced to end, and it was this file that had one.
func (s *Server) envelope() config.Envelope {
	var src config.DialSource
	if s.db != nil {
		src = s.db
	}
	return config.NewResolver(s.cfg, src).Envelope()
}

// topology names what this install is, derived from configuration rather than
// declared: an operator should not be able to mislabel their own install.
func (s *Server) topology() runtime.Topology {
	switch {
	case s.cfg.KubernetesConfigured():
		return runtime.TopologyKubernetes
	case s.cfg.ECSConfigured():
		return runtime.TopologyECS
	default:
		return runtime.TopologyVM
	}
}

type settingsResponse struct {
	database.PlatformSettings

	// UpdatedAt is when the document last changed. The page sends it back
	// with a write, and a write from a page that read an older document is
	// refused rather than allowed to overwrite what somebody else saved.
	UpdatedAt time.Time `json:"updated_at"`

	// The read-only half: what the install is capable of, so the UI can
	// explain *why* a switch is unavailable rather than just greying it out.
	Topology         string `json:"topology"`
	IsolatedPossible bool   `json:"isolated_possible"`
	// HostJobsPossible is whether job runs can be placed on this machine
	// (ADR §106), which is what lets the jobs switch be thrown without
	// isolated execution.
	HostJobsPossible bool `json:"host_jobs_possible"`
	// JobPlacement is where runs go now — "cluster", "this machine", or ""
	// when jobs cannot run.
	JobPlacement string `json:"job_placement"`
	// SharedContainment is how the shared-mode agent holds this machine's
	// workloads — its tier, why the next one up was refused, and the bridge
	// range at the namespace tier (ADR §128). Absent where
	// nothing answers for it: the mock runtime, or an agent that is down.
	SharedContainment *sharedContainment `json:"shared_containment,omitempty"`

	// What is protecting this install's secrets at rest — "local" or "managed"
	// — and, when managed, which key-encrypting key. Derived from configuration
	// and read-only, for the same reason Topology is: an operator should not be
	// able to mislabel their own install, and a surface that answers "what is
	// protecting the data" must never be able to answer it untruthfully. Not a
	// health warning either: the local backend is a supported configuration
	// chosen deliberately, and a nag would train operators to ignore the
	// startup output where the fatal errors are (ADR §62).
	EncryptionBackend string `json:"encryption_backend"`
	// EncryptionKEK carries no credential, by construction — the URI never
	// does. Empty on the local backend.
	EncryptionKEK string `json:"encryption_kek,omitempty"`
	// EncryptionKeyID is the key new records are written under, so an operator
	// can tell a rotation took from the same page that told them to do one.
	EncryptionKeyID string `json:"encryption_key_id"`
	// IsolatedBlockedReason is empty when isolated mode can be enabled.
	IsolatedBlockedReason string `json:"isolated_blocked_reason,omitempty"`

	// MetadataBlocked is whether BLOCK_AWS_METADATA is set on this process.
	// Read-only and derived from configuration, like Topology beside it.
	//
	// It is stated rather than graded, and the copy that renders it carries the
	// one thing the platform cannot answer: the isolated half is a NetworkPolicy
	// nothing verifies. A cluster with no policy engine accepts the object and
	// ignores it, so an install that shows "On" here may still be unprotected in
	// isolated mode, and only whoever created the cluster knows which. Saying so
	// on the page is the whole of the mitigation — see docs/invariants.md
	// § Process and network boundaries.
	MetadataBlocked bool `json:"metadata_blocked"`

	// AppAccounts is shared mode's filesystem posture: which OS identity a
	// deployed app's processes run as ("per-app", "shared", "platform").
	// Derived from configuration and read-only, for the same reason Topology
	// is.
	//
	// Unlike the encryption backend beside it, this one is allowed to grade
	// itself, because only one of its three values is a supported production
	// posture. "platform" means every app can read the platform database and
	// every other app's secrets, and until now the only thing that said so was
	// a startup log line — on a process nobody reads the log of after the first
	// week, which is the person who can fix it never being told.
	AppAccounts string `json:"app_accounts"`

	// The stored ceiling, in both units. Zero is "not set" and means the
	// environment's number, never "uncapped".
	//
	// Two fields rather than one because megabytes is the right unit for a
	// 256 MB cap and gigabytes for a 16 GB one, and a page that offered only
	// one of them would be asking somebody to do the arithmetic. They are two
	// *renderings* of one row and never two answers: the row holds mebibytes,
	// a write may name exactly one of them, and both are recomputed from what
	// was stored.
	AppMemoryLimitMB int     `json:"app_memory_limit_mb"`
	AppMemoryLimitGB float64 `json:"app_memory_limit_gb"`
	// The ceiling this process was started with, in both units, shown for the
	// reason EnvBaseURL below is: 0 in the fields above means "use the
	// operator's number", and a field whose zero defers to a number the page
	// cannot show is a field nobody can reason about. An administrator seeing
	// "0 — using the install's 512 MB" knows both what is in force and what
	// clearing the box would restore.
	EnvAppMemoryLimitMB int     `json:"env_app_memory_limit_mb"`
	EnvAppMemoryLimitGB float64 `json:"env_app_memory_limit_gb"`

	// The rest of the dials, stored and as this process was started, each for
	// the reason above: a field whose zero defers to a number the page cannot
	// show is a field nobody can reason about, and that sentence is true of
	// every dial. The CPU pair carries both units for the reason the ceiling
	// does — cores is what an operator reserves in, millicores is what the
	// cluster takes.
	AppCPURequestMillicores    int     `json:"app_cpu_request_millicores"`
	AppCPURequestCores         float64 `json:"app_cpu_request_cores"`
	AppCPULimitMillicores      int     `json:"app_cpu_limit_millicores"`
	AppCPULimitCores           float64 `json:"app_cpu_limit_cores"`
	EnvAppCPURequestMillicores int     `json:"env_app_cpu_request_millicores"`
	EnvAppCPURequestCores      float64 `json:"env_app_cpu_request_cores"`
	EnvAppCPULimitMillicores   int     `json:"env_app_cpu_limit_millicores"`
	EnvAppCPULimitCores        float64 `json:"env_app_cpu_limit_cores"`

	// A job run's own defaults, in both units, stored and as this process was
	// started. Separate from the app's: an app's limits do not reach its jobs.
	JobMemoryLimitMB         int     `json:"job_memory_limit_mb"`
	JobMemoryLimitGB         float64 `json:"job_memory_limit_gb"`
	JobCPULimitMillicores    int     `json:"job_cpu_limit_millicores"`
	JobCPULimitCores         float64 `json:"job_cpu_limit_cores"`
	EnvJobMemoryLimitMB      int     `json:"env_job_memory_limit_mb"`
	EnvJobMemoryLimitGB      float64 `json:"env_job_memory_limit_gb"`
	EnvJobCPULimitMillicores int     `json:"env_job_cpu_limit_millicores"`
	EnvJobCPULimitCores      float64 `json:"env_job_cpu_limit_cores"`

	EnvMaxConcurrentBuilds   int    `json:"env_max_concurrent_builds"`
	EnvAutoDeployPollSeconds int    `json:"env_auto_deploy_poll_seconds"`
	EnvRPackageRepository    string `json:"env_r_package_repository"`
	EnvLogLevel              string `json:"env_log_level"`

	// MaxConcurrentBuildsCeiling is the pool this process's workers live in,
	// which the dial cannot exceed. Named on the page, because a ceiling a field
	// cannot pass and does not mention is a field nobody can reason about.
	MaxConcurrentBuildsCeiling int `json:"max_concurrent_builds_ceiling"`
	// AutoDeployPollerRunning is false on an install started with
	// AUTO_DEPLOY_POLL_INTERVAL=0, where no poller was ever constructed and
	// storing a number starts none. The field renders disabled and says so.
	AutoDeployPollerRunning bool `json:"auto_deploy_poller_running"`
	// LogFormat is the half of the logging pair that stays in the environment —
	// read-only here, because a surface that offers a level and says nothing
	// about the format leaves an administrator wondering where the other one
	// went.
	LogFormat string `json:"log_format"`
	// BuildAllowScripts is the other thing that stays in the environment and
	// is not a dial: it is not a quantity but what code a build may execute,
	// and npm lifecycle scripts are arbitrary code from the dependency tree.
	// Reported for the reason app_accounts is — a posture nobody can change
	// from the browser still has to be visible to the person who can.
	BuildAllowScripts bool `json:"build_allow_scripts"`
	// EffectiveLogLevel is what this process is saying *now*, which after a
	// settings write is the stored level and after a fresh start is whichever
	// the envelope resolved.
	EffectiveLogLevel string `json:"effective_log_level"`

	// Counts so an administrator turning something off can see what it
	// affects before they do it, rather than afterwards.
	IsolatedAppCount int `json:"isolated_app_count"`
	// PublicAppCount is how many apps their owners have chosen to publish —
	// the blast radius of turning public apps off, shown beside the switch so
	// an administrator sees it before rather than after. It counts the owners'
	// column and not what is in effect: those are the apps the switch acts on,
	// and it stays truthful once the switch is already off, where "in effect"
	// would read zero and imply the control affects nothing.
	PublicAppCount int `json:"public_app_count"`
	// AppsOverVersionCap is how many apps the *stored* cap already blocks —
	// cleanup off and at or above it. Zero when there is no cap.
	AppsOverVersionCap int `json:"apps_over_version_cap"`
	// RunnerVersion is the contract version this platform speaks; an operator
	// comparing it against their mirrored image tags is how a partial upgrade
	// gets noticed.
	RunnerVersion string   `json:"runner_version"`
	RunnerImages  []string `json:"runner_images"`

	// The three addresses an administrator needs in order to reason about
	// where links in email will land, which until now was answerable only by
	// sending a test message and reading the footer.
	//
	// EffectiveBaseURL is what a notification sent right now would use.
	// EnvBaseURL is the BASE_URL this process was started with, shown so that
	// clearing the setting has a visible consequence rather than being a leap.
	// ObservedBaseURL is the address *this request* arrived at, according to a
	// trusted proxy — empty when nothing vouched for it, which is itself the
	// answer to "why does this install not know its own name".
	EffectiveBaseURL string `json:"effective_base_url"`
	EnvBaseURL       string `json:"env_base_url"`
	ObservedBaseURL  string `json:"observed_base_url,omitempty"`

	// AppsBaseURL is where deployed apps are served — APPS_BASE_URL, or the
	// address derived from BASE_URL and APPS_PORT — and so the origin every
	// address an app builds with the SDKs' url() starts from. Read-only: it is
	// a listener and a certificate, not a label, and the override above does
	// not move it. Equal to the platform's on a single-origin install.
	AppsBaseURL string `json:"apps_base_url"`

	// The gallery's read-only half. Where the catalog lives is compiled in
	// (or supplied by the operator's environment) and shown here rather than
	// offered as a field: a catalog URL editable through the API would be a
	// way to choose what every deploy button on the install offers.
	//
	// GallerySync is the last refresh — its status, its age, and its error if
	// it had one. An administrator looking at an empty gallery needs to be able
	// to tell "the catalog lists nothing" from "we could not reach it", and
	// this is where that answer lives.
	GalleryAvailable     bool                          `json:"gallery_available"`
	GalleryCatalogURL    string                        `json:"gallery_catalog_url,omitempty"`
	GalleryCatalogSource string                        `json:"gallery_catalog_source,omitempty"`
	GallerySync          *database.GalleryCatalogCache `json:"gallery_sync,omitempty"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		// Not 403: what an install is capable of is infrastructure detail, and
		// a non-admin has no business learning the shape of it.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	caps := s.capabilities()
	updatedAt, err := s.db.PlatformSettingsUpdatedAt()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	resp := settingsResponse{
		PlatformSettings:  settings,
		UpdatedAt:         updatedAt,
		Topology:          string(caps.Topology),
		IsolatedPossible:  caps.IsolatedPossible,
		HostJobsPossible:  caps.HostJobsPossible,
		JobPlacement:      jobPlacementWord(caps),
		RunnerVersion:     s.cfg.RunnerVersion,
		EffectiveBaseURL:  s.db.ResolvePublicBaseURL(s.cfg.BaseURL),
		EnvBaseURL:        strings.TrimSuffix(s.cfg.BaseURL, "/"),
		ObservedBaseURL:   publicObservedOrigin(r),
		AppsBaseURL:       strings.TrimSuffix(s.cfg.AppsBaseURL, "/"),
		SharedContainment: s.sharedContainment(),

		EncryptionBackend: s.encryptor.Backend(),
		EncryptionKEK:     s.encryptor.KEKURI(),
		EncryptionKeyID:   s.encryptor.EncryptKeyID(),

		// The same rule main.go warns from, asked of the same two settings —
		// not a second opinion held here.
		AppAccounts: string(runtime.AccountsFor(s.cfg.AppRunAsUser, s.cfg.AppUserPerApp)),

		MetadataBlocked: s.cfg.BlockAWSMetadata,
	}
	if !caps.IsolatedPossible {
		// The reason the switch cannot be thrown, phrased for the person
		// looking at it: on a VM it is a dead end, elsewhere it is missing
		// configuration an operator can supply.
		resp.IsolatedBlockedReason = runtime.Capabilities{
			Topology: caps.Topology, IsolatedEnabled: true,
		}.ExplainUnavailable(runtime.ModeIsolated)
	}
	for family := range s.cfg.RunnerImages {
		resp.RunnerImages = append(resp.RunnerImages, family)
	}
	resp.AppMemoryLimitMB = settings.AppMemoryLimitMB
	resp.AppMemoryLimitGB = gigabytesOf(settings.AppMemoryLimitMB)
	resp.EnvAppMemoryLimitMB = s.cfg.AppMemoryLimitMB
	resp.EnvAppMemoryLimitGB = gigabytesOf(s.cfg.AppMemoryLimitMB)

	resp.AppCPURequestMillicores = settings.AppCPURequestMillicores
	resp.AppCPURequestCores = coresOf(settings.AppCPURequestMillicores)
	resp.AppCPULimitMillicores = settings.AppCPULimitMillicores
	resp.AppCPULimitCores = coresOf(settings.AppCPULimitMillicores)
	resp.EnvAppCPURequestMillicores = s.cfg.AppCPURequestMillicores
	resp.EnvAppCPURequestCores = coresOf(s.cfg.AppCPURequestMillicores)
	resp.EnvAppCPULimitMillicores = s.cfg.AppCPULimitMillicores
	resp.EnvAppCPULimitCores = coresOf(s.cfg.AppCPULimitMillicores)

	resp.JobMemoryLimitMB = settings.JobMemoryLimitMB
	resp.JobMemoryLimitGB = gigabytesOf(settings.JobMemoryLimitMB)
	resp.JobCPULimitMillicores = settings.JobCPULimitMillicores
	resp.JobCPULimitCores = coresOf(settings.JobCPULimitMillicores)
	resp.EnvJobMemoryLimitMB = s.cfg.JobMemoryLimitMB
	resp.EnvJobMemoryLimitGB = gigabytesOf(s.cfg.JobMemoryLimitMB)
	resp.EnvJobCPULimitMillicores = s.cfg.JobCPULimitMillicores
	resp.EnvJobCPULimitCores = coresOf(s.cfg.JobCPULimitMillicores)

	resp.EnvMaxConcurrentBuilds = s.cfg.MaxConcurrentBuilds
	resp.EnvAutoDeployPollSeconds = s.cfg.AutoDeployPollSeconds
	resp.EnvRPackageRepository = s.cfg.RRepos
	resp.EnvLogLevel = strings.ToLower(s.cfg.LogLevel.String())
	// Never blank. config.Load always sets one, and the default it would have
	// chosen is what a configuration assembled some other way is running.
	if resp.LogFormat = s.cfg.LogFormat; resp.LogFormat == "" {
		resp.LogFormat = "text"
	}
	resp.EffectiveLogLevel = strings.ToLower(s.envelope().LogLevel.String())
	resp.BuildAllowScripts = s.cfg.AllowBuildScripts
	resp.MaxConcurrentBuildsCeiling = builder.PoolSize(s.cfg.MaxConcurrentBuilds)
	resp.AutoDeployPollerRunning = s.cfg.AutoDeployPollSeconds > 0
	if n, err := s.db.CountAppsInMode(string(runtime.ModeIsolated)); err == nil {
		resp.IsolatedAppCount = n
	}
	if n, err := s.db.CountAppsOverVersionCap(settings.MaxVersions); err == nil {
		resp.AppsOverVersionCap = n
	}
	if public, _, _, err := s.db.CountAppsByVisibility(); err == nil {
		resp.PublicAppCount = public
	}
	if s.gallery != nil {
		resp.GalleryAvailable = true
		resp.GalleryCatalogURL = s.gallery.CatalogURL()
		resp.GalleryCatalogSource = s.gallery.Source()
		if sync, err := s.db.GetGalleryCatalog(); err == nil {
			resp.GallerySync = &sync
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

type updateSettingsRequest struct {
	// UpdatedAt is the document's updated_at the page read. Sent back with
	// the write, it is refused with 409 when the document has moved since;
	// absent, the handler's own read is the precondition, so a script that
	// has not read the document can be refused by a concurrent write but
	// never revert one.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`

	IsolatedEnabled *bool `json:"isolated_execution_enabled,omitempty"`

	// DefaultExecutionMode is what a new app is born as when whoever creates
	// it names no mode. A seed, never an override — see
	// database.SettingDefaultExecutionMode.
	DefaultExecutionMode *string `json:"default_execution_mode,omitempty"`

	JobsEnabled    *bool `json:"background_jobs_enabled,omitempty"`
	JobConcurrency *int  `json:"job_concurrency_per_app,omitempty"`

	// The install's memory ceiling, in whichever unit the administrator is
	// thinking in. 0 restores the operator's number rather than uncapping
	// anything. Gigabytes takes a decimal and is stored as whole mebibytes;
	// megabytes is already the stored unit. **Exactly one may be sent**: both
	// is refused rather than resolved by precedence, for the reason both
	// environment spellings are.
	AppMemoryLimitMB *int     `json:"app_memory_limit_mb,omitempty"`
	AppMemoryLimitGB *float64 `json:"app_memory_limit_gb,omitempty"`

	// The rest of the dials. Every one takes 0 (or empty) to mean "the
	// operator's value", never "off" and never "unlimited", and the two CPU
	// figures follow the ceiling's rule about units: send one spelling, not
	// both.
	AppCPURequestMillicores *int     `json:"app_cpu_request_millicores,omitempty"`
	AppCPURequestCores      *float64 `json:"app_cpu_request_cores,omitempty"`
	AppCPULimitMillicores   *int     `json:"app_cpu_limit_millicores,omitempty"`
	AppCPULimitCores        *float64 `json:"app_cpu_limit_cores,omitempty"`
	// A job run's defaults, one spelling of each, 0 meaning the operator's.
	JobMemoryLimitMB      *int     `json:"job_memory_limit_mb,omitempty"`
	JobMemoryLimitGB      *float64 `json:"job_memory_limit_gb,omitempty"`
	JobCPULimitMillicores *int     `json:"job_cpu_limit_millicores,omitempty"`
	JobCPULimitCores      *float64 `json:"job_cpu_limit_cores,omitempty"`
	MaxConcurrentBuilds   *int     `json:"max_concurrent_builds,omitempty"`
	AutoDeployPollSeconds *int     `json:"auto_deploy_poll_seconds,omitempty"`
	RPackageRepository    *string  `json:"r_package_repository,omitempty"`
	LogLevel              *string  `json:"log_level,omitempty"`

	// The refusal messages. Sent empty to restore the default — an admin who
	// clears the box means "say the standard thing", not "say nothing".
	LockedMessage    *string `json:"locked_account_message,omitempty"`
	AppLockedMessage *string `json:"app_locked_message,omitempty"`
	NoAccountMessage *string `json:"no_account_message,omitempty"`
	// The pending pair: what an unactivated account is told at the door, and
	// what the activation notice says when somebody lets it in. Same rule —
	// empty restores the default.
	PendingActivationMessage *string `json:"pending_activation_message,omitempty"`
	ActivationMessage        *string `json:"account_activated_message,omitempty"`

	// Sent empty to name no environment.
	EnvironmentTag *string `json:"environment_tag,omitempty"`

	// Sent empty to go back to "Toft" as the sender's name.
	InstallName *string `json:"install_name,omitempty"`

	// Sent empty to show every person times in their own browser's zone.
	DisplayTimeZone *string `json:"display_time_zone,omitempty"`

	// 0 turns idle sleep off. Shared-mode apps only.
	IdleSleepMinutes *int `json:"idle_sleep_minutes,omitempty"`

	// 0 keeps isolated apps awake — a setting apart from the shared one.
	IsolatedIdleSleepMinutes *int `json:"isolated_idle_sleep_minutes,omitempty"`

	// The window isolated apps may sleep in, "HH:MM" in the display zone.
	IsolatedSleepWindowEnabled *bool   `json:"isolated_sleep_window_enabled,omitempty"`
	IsolatedSleepWindowStart   *string `json:"isolated_sleep_window_start,omitempty"`
	IsolatedSleepWindowEnd     *string `json:"isolated_sleep_window_end,omitempty"`

	// 0 removes the per-app version cap.
	MaxVersions *int `json:"max_versions_per_app,omitempty"`

	// Which of the gallery's two kinds may be deployed: "off",
	// "extensions_only" or "all". One field with three values rather than two
	// switches, because two switches can express nonsense.
	GalleryMode *string `json:"gallery_deploy_mode,omitempty"`

	// Whether people may hold personal API keys. Turning it off also stops
	// every key that already exists from authenticating — see
	// database.SettingAPIKeysEnabled for why that is the only honest reading
	// of the switch.
	APIKeysEnabled *bool `json:"api_keys_enabled,omitempty"`

	// The longest a new personal API key may live, in days; 0 removes the
	// cap. See database.SettingAPIKeyMaxLifetimeDays.
	APIKeyMaxLifetimeDays *int `json:"api_key_max_lifetime_days,omitempty"`

	// Whether an app may be reachable without signing in. Off withdraws
	// anonymity from every app that already has it, immediately, and refuses
	// the transition to public — see database.SettingPublicAppsEnabled for why
	// that is one switch and not two.
	PublicAppsEnabled *bool `json:"public_apps_enabled,omitempty"`

	// Whether the gateway counts distinct anonymous browsers with a
	// first-party visitor cookie. Default off. See
	// database.SettingAudienceMeasurementEnabled.
	AudienceMeasurementEnabled *bool `json:"audience_measurement_enabled,omitempty"`

	// Whether the apps gateway fills nosniff and Referrer-Policy where an app
	// sent neither. Default on; an app's own header always wins, and HSTS is
	// not behind it. See database.SettingAppSecurityHeaders.
	AppSecurityHeaders *bool `json:"app_security_headers,omitempty"`

	// Whether a shared app's output is copied to the platform's stream with no
	// per-app cap. See database.SettingAppLogCopyUncapped.
	AppLogCopyUncapped *bool `json:"app_log_copy_uncapped,omitempty"`

	// Whether every signed-in person sees every app in their list without being
	// able to open any of it — the `list` rung granted install-wide. Default
	// off, because turning it on discloses every app's name, slug and owner to
	// everyone who can sign in. See database.SettingAppsListableToAuthenticated.
	AppsListableToAuthenticated *bool `json:"apps_listable_to_authenticated,omitempty"`

	// Dormancy locking. The threshold is a number with a working default and
	// the switch is what turns the sweep on — there is no value of the
	// threshold that means "off", because a control that locks accounts should
	// not be switchable by mistyping a figure.
	//
	// Neither of these can make the sweep lock an administrator. That is not a
	// setting and there is no request that changes it.
	DormantLockEnabled *bool `json:"dormant_lock_enabled,omitempty"`
	DormantLockDays    *int  `json:"dormant_lock_days,omitempty"`

	// The session policy. Two numbers because every standard names them
	// separately, and there is no switch above them: a session always has a
	// lifetime, and 0 on the idle timeout is what turns that half off.
	//
	// Both apply to sessions already outstanding — the ceiling is checked on
	// every request rather than baked into the token — so shortening either is
	// a control that takes effect now rather than at the next sign-in.
	SessionAbsoluteHours *int `json:"session_absolute_hours,omitempty"`
	SessionIdleMinutes   *int `json:"session_idle_minutes,omitempty"`

	// Sent empty to hand the decision back to the BASE_URL environment
	// variable, which is how a stored value that turns out to be wrong is
	// undone without an operator and a restart.
	PublicBaseURL *string `json:"public_base_url,omitempty"`

	// Garth's address, or empty to take the "Open in Garth" link away again
	// (ADR §158). Audited with the rest of this blob.
	GarthURL *string `json:"garth_url,omitempty"`

	// The scheduled-task caps, and the two job retention figures they sit
	// beside. None of these is a capability switch — every one is a number
	// with a working default, and what an administrator gets from them is
	// sight of the rules rather than the ability to turn something on.
	// The dependency policy (ADR §39). The mode is one field with three values
	// for the reason the gallery's is: two switches can express nonsense, and
	// an empty middle state is ambiguous, so the middle state gets a name.
	//
	// The feed *credential* is deliberately absent — it is its own encrypted
	// document, because this blob is audited whole.
	DependencyPolicy           *string `json:"dependency_policy,omitempty"`
	DependencyScanOnDeploy     *bool   `json:"dependency_scan_on_deploy,omitempty"`
	DependencyWatchEnabled     *bool   `json:"dependency_watch_enabled,omitempty"`
	DependencyBlockMalware     *bool   `json:"dependency_block_malware,omitempty"`
	DependencyBlockSeverity    *string `json:"dependency_block_severity,omitempty"`
	DependencyBlockOnlyFixable *bool   `json:"dependency_block_only_fixable,omitempty"`
	DependencyFeedEndpoint     *string `json:"dependency_feed_endpoint,omitempty"`
	DependencyFeedIntervalHrs  *int    `json:"dependency_feed_interval_hours,omitempty"`
	DependencyFeedMaxAgeHours  *int    `json:"dependency_feed_max_age_hours,omitempty"`
	DependencyWaiverMaxDays    *int    `json:"dependency_waiver_max_days,omitempty"`
	// The watch's own three. The interval is not the feed's, the retention is a
	// count rather than days, and the lock threshold is not the block
	// threshold — see applyDependencySettings, where each refusal says why.
	DependencyScanIntervalHrs *int    `json:"dependency_scan_interval_hours,omitempty"`
	DependencyScanHistoryKept *int    `json:"dependency_scan_history_kept,omitempty"`
	DependencyLockOn          *string `json:"dependency_lock_on,omitempty"`

	DefaultJobTimeoutSecs     *int `json:"default_job_timeout_seconds,omitempty"`
	MaxJobTimeoutSecs         *int `json:"max_job_timeout_seconds,omitempty"`
	MaxQueuedRunsPerApp       *int `json:"max_queued_runs_per_app,omitempty"`
	NotebookRenderTimeoutSecs *int `json:"notebook_render_timeout_seconds,omitempty"`
	BuildTimeoutSecs          *int `json:"build_timeout_seconds,omitempty"`
	JobLogRetentionDays       *int `json:"job_log_retention_days,omitempty"`
	JobRunRetentionDays       *int `json:"job_run_retention_days,omitempty"`

	AppEventRetentionDays    *int `json:"app_event_retention_days,omitempty"`
	PlatformLogRetentionDays *int `json:"platform_log_retention_days,omitempty"`

	// How long the per-person rows under a sent announcement are kept. It
	// prunes who was mailed, never the announcement or its counts — those are
	// the install's own record and are never pruned at all.
	AnnouncementRecipientRetentionDays *int `json:"announcement_recipient_retention_days,omitempty"`
	// The floor between two deliveries of one report; 0 means none.
	ReportDeliveryMinIntervalMinutes *int `json:"report_delivery_min_interval_minutes,omitempty"`
	// 0 keeps everything; otherwise 365–3650. Not in the bounded list below,
	// whose fields have no zero.
	AccessRecordRetentionDays *int `json:"access_record_retention_days,omitempty"`

	// The security.txt fields (RFC 9116). The contact is the switch: sent
	// empty, this install stops publishing the file at all — which is the
	// only honest way to turn it off, because a file with a blank contact is
	// a promise nobody is keeping.
	//
	// The expiry is required whenever there is a contact, and a date in the
	// past is refused. That is not fussiness: the failure mode of this file is
	// that it stays correct for a year and then names somebody who left, and
	// the expiry is the field that makes that visible.
	SecurityTxtContact   *string `json:"security_txt_contact,omitempty"`
	SecurityTxtExpires   *string `json:"security_txt_expires,omitempty"`
	SecurityTxtPolicy    *string `json:"security_txt_policy,omitempty"`
	SecurityTxtLanguages *string `json:"security_txt_languages,omitempty"`
}

// The bounds on the idle-sleep timeout. The floor is not arbitrary: readiness
// alone takes seconds for a slow framework, so a one-minute timeout would sleep
// apps that had barely finished starting and thrash them on every visit. The
// ceiling is a week, past which the setting is indistinguishable from off and
// off is the honest way to say it.
const (
	minIdleSleepMinutes = 5
	maxIdleSleepMinutes = 10080
)

// The bounds on the per-app version cap. The floor keeps one release in hand
// besides the one being served, so rollback survives the cap; the ceiling is
// high enough to be a real limit and low enough that nobody sets it by
// accident and wonders why nothing is ever cleaned up.
const (
	minVersionCap = 2
	maxVersionCap = 100
)

// envTagRe keeps the environment tag a plain label: no dots, slashes, or
// spaces, and no case ambiguity between installs.
var envTagRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{0,30}[a-z0-9])?$`)

// messageLimit caps a refusal message. Long enough for a paragraph with a
// support address in it, short enough that the settings row cannot become a
// place to store a document.
const messageLimit = 600

// afterSettingsReadForTest, when set, runs between handleUpdateSettings' read
// of the document and its write — the window a concurrent write lands in.
// Nil outside tests.
var afterSettingsReadForTest func()

// removedSettings are settings this install once read, and what replaced them.
var removedSettings = map[string]string{
	"max_job_memory_mb": " is gone: a job's memory is no longer declared, so there is nothing " +
		"for a maximum to bound. Set the job default with job_memory_limit_mb, or one job's " +
		"memory on its app's Automation tab",
}

// settingKeys is every key the settings write reads, from its own json tags.
var settingKeys = func() map[string]bool {
	keys := map[string]bool{}
	t := reflect.TypeOf(updateSettingsRequest{})
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}()

// unknownSettingKeys is the keys of a settings write this route does not read,
// sorted so the refusal is the same sentence every time.
func unknownSettingKeys(raw map[string]json.RawMessage) []string {
	var out []string
	for k := range raw {
		if !settingKeys[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	// The read-only keys are refused by name rather than ignored. Silently
	// dropping gallery_catalog_url would leave an administrator believing they
	// had repointed the catalog, which is the one misunderstanding this field
	// must never allow: where the catalog lives decides what every deploy
	// button on the install offers.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	const galleryWhy = " is not a setting. Where the gallery catalog lives is part of how " +
		"this install was built — set GALLERY_CATALOG_URL in the environment to point " +
		"at an internal mirror."
	// Ordered, so a request naming two of them is refused for the same one
	// every time.
	for _, refusal := range []struct{ key, why string }{
		{"gallery_catalog_url", galleryWhy},
		{"gallery_catalog_source", galleryWhy},
		{"gallery_sync", galleryWhy},
		{"docs_url", " is not a setting. Where this install's documentation links point is part " +
			"of how it was built — set DOCS_URL in the environment to a mirror, or to off."},
	} {
		if _, present := raw[refusal.key]; present {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": refusal.key + refusal.why, "hint": "leave " + refusal.key + " out of the request and send the rest again"})
			return
		}
	}

	// Every other key is refused by name too. A setting this route does not
	// read was dropped without a word, so a script still setting
	// max_job_memory_mb, or a misspelt key, read back a 200 and believed it
	// had changed something (drills 2026-10-29).
	if unknown := unknownSettingKeys(raw); len(unknown) > 0 {
		msg := "not a setting this install reads: " + strings.Join(unknown, ", ") + " — nothing was changed"
		for _, k := range unknown {
			if why, ok := removedSettings[k]; ok {
				msg = k + why + " — nothing was changed"
				break
			}
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "code": "unknown_setting",
			"retryable": false,
			"hint": "GET system/settings lists the settings; a read-only one is " +
				"set in the environment, not here"})
		return
	}

	var req updateSettingsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	// The write is a whole document built on this read, so it always carries
	// a precondition. A caller that sent none — a script that never read the
	// document — gets this read's own: read first, so a write landing between
	// the two is refused (409) rather than reverted. Unconditional, a stale
	// read rewrote every row, and a license key or security.txt contact saved
	// on its own page in that window was silently put back (API-SET-1).
	since := req.UpdatedAt
	if since == nil {
		readAt, err := s.db.PlatformSettingsUpdatedAt()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		since = &readAt
	}
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if afterSettingsReadForTest != nil {
		afterSettingsReadForTest()
	}
	// What was stored before this save, for the one transition — the
	// dependency watch coming back on — that has a write of its own after the
	// settings write.
	before := settings

	if req.IsolatedEnabled != nil {
		// An administrator cannot enable what the install cannot do. Refusing
		// here rather than storing the setting keeps one truth: a stored
		// "enabled" that has no effect is a lie the UI would have to explain
		// forever after.
		if *req.IsolatedEnabled && !s.rts.IsolatedConfigured() {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": runtime.Capabilities{Topology: s.topology(), IsolatedEnabled: true}.
					ExplainUnavailable(runtime.ModeIsolated),
				"hint": "save without isolated_execution_enabled: true — the install's operator has to give it a cluster before isolated execution can be switched on",
			})
			return
		}
		settings.IsolatedEnabled = *req.IsolatedEnabled
	}

	if req.DefaultExecutionMode != nil {
		mode, err := runtime.ParseMode(strings.TrimSpace(*req.DefaultExecutionMode))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send "default_execution_mode": "shared", or "isolated" where isolated execution is on`})
			return
		}
		// Checked against the switch *as this request leaves it*, not as the
		// database currently has it, so one request may turn isolated on and
		// make it the default. The same reading the jobs check below does.
		caps := s.capabilitiesFrom(settings)
		// An administrator cannot make the default a mode this install does
		// not offer: a stored default that every create silently ignores is a
		// setting that lies, and the create path would have to degrade it on
		// every request forever after.
		if why := caps.ExplainUnavailable(mode); why != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": why, "hint": `keep "default_execution_mode": "shared", or switch isolated_execution_enabled on in the same request`})
			return
		}
		settings.DefaultExecutionMode = string(mode)
	}

	if req.JobsEnabled != nil {
		// Refused only where a run has nowhere to go at all. Isolated execution
		// is no longer the precondition (ADR §106): without it, runs go to this
		// machine, and the help line under the switch says which.
		if *req.JobsEnabled && !settings.IsolatedEnabled && !s.jobs.HostPossible() {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Background jobs need somewhere to run: enable isolated execution, " +
					"or run Toft with its shared runtime so jobs can run on this machine.",
				"hint": `send "isolated_execution_enabled": true in the same request, or leave background_jobs_enabled off`,
			})
			return
		}
		settings.JobsEnabled = *req.JobsEnabled
	}

	// Turning isolated mode off must not silently strand apps already running
	// in it. They keep running — this gates *new* promotions — and saying so
	// is better than leaving an admin to wonder.
	var warning string
	if req.IsolatedEnabled != nil && !*req.IsolatedEnabled {
		if n, err := s.db.CountAppsInMode(string(runtime.ModeIsolated)); err == nil && n > 0 {
			subject := "apps are"
			if n == 1 {
				subject = "app is"
			}
			warning = strconv.Itoa(n) + " " + subject + " already running in isolated mode and will " +
				"keep running unaffected. This prevents new promotions; return an app to shared " +
				"mode to move it back."
		}
		// Jobs move to this machine when there is one to move to, and stop
		// otherwise — they cannot outlive every place they could run.
		if !s.jobs.HostPossible() {
			settings.JobsEnabled = false
		} else if settings.JobsEnabled {
			warning = strings.TrimSpace(warning + " New job runs will run on this machine rather than in the cluster.")
		}
	}

	if req.JobConcurrency != nil {
		if *req.JobConcurrency < 1 || *req.JobConcurrency > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "job_concurrency_per_app must be between 1 and 100",
				"hint":  `send a whole number from 1 to 100, such as "job_concurrency_per_app": 4`})
			return
		}
		settings.JobConcurrency = *req.JobConcurrency
	}

	if req.AppMemoryLimitMB != nil && req.AppMemoryLimitGB != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "send app_memory_limit_mb or app_memory_limit_gb, not both — " +
				"they are the same number in two units and the platform will not guess which you meant",
			"hint": `drop one of the two — "app_memory_limit_gb": 2 and "app_memory_limit_mb": 2048 say the same thing`})
		return
	}
	if req.AppMemoryLimitMB != nil {
		if mb := *req.AppMemoryLimitMB; !storeMemoryLimit(w, mb, "app_memory_limit_mb", &settings.AppMemoryLimitMB) {
			return
		}
	}
	if req.AppMemoryLimitGB != nil {
		// 0 is allowed and means "hand back to APP_MEMORY_LIMIT_GB"; the floor
		// above it is 64 MiB because a cap below that kills an interpreter
		// before it has finished starting, and an administrator who typed 0.008
		// has made a mistake the platform can see. The ceiling is a sanity
		// bound rather than a policy — the box has whatever memory it has, and
		// refusing at some fraction of it would be a number this code cannot
		// know.
		//
		// Rounded to the nearest mebibyte on the way in, and the response
		// carries the rounded value back in gigabytes, so a field that accepted
		// 0.333 shows 0.33 afterwards and never claims a precision the install
		// does not hold.
		// Checked before the rounding, not after: 0.0624 rounds to exactly the
		// floor, and accepting it would make the refusal's own number wrong.
		gb := *req.AppMemoryLimitGB
		if gb != 0 && gb < float64(minAppMemoryLimitMB)/mebibytesPerGigabyte {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "app_memory_limit_gb must be 0, or at least " + minMemoryLimitPhrase,
				"hint":  `send 0 to use this install's own figure, or 0.0625 or more, such as "app_memory_limit_gb": 2`})
			return
		}
		if !storeMemoryLimit(w, int(math.Round(gb*mebibytesPerGigabyte)),
			"app_memory_limit_gb", &settings.AppMemoryLimitMB) {
			return
		}
	}

	// The scheduler's two figures, each in either unit and never both, and each
	// zero meaning the operator's number. The request is refused before the
	// limit-below-request check so a caller sending nonsense is told which
	// field is nonsense rather than which pair disagrees.
	for _, d := range []struct {
		small     *int
		big       *float64
		perBig    int
		smallName string
		bigName   string
		min       int
		max       int
		dst       *int
	}{
		{req.AppCPURequestMillicores, req.AppCPURequestCores, millicoresPerCore,
			"app_cpu_request_millicores", "app_cpu_request_cores", 0, maxCPUMillicores,
			&settings.AppCPURequestMillicores},
		{req.AppCPULimitMillicores, req.AppCPULimitCores, millicoresPerCore,
			"app_cpu_limit_millicores", "app_cpu_limit_cores", 0, maxCPUMillicores,
			&settings.AppCPULimitMillicores},
		{req.JobCPULimitMillicores, req.JobCPULimitCores, millicoresPerCore,
			"job_cpu_limit_millicores", "job_cpu_limit_cores", minCPULimitMillicores, maxCPUMillicores,
			&settings.JobCPULimitMillicores},
		{req.JobMemoryLimitMB, req.JobMemoryLimitGB, mebibytesPerGigabyte,
			"job_memory_limit_mb", "job_memory_limit_gb", minAppMemoryLimitMB, maxAppMemoryLimitMB,
			&settings.JobMemoryLimitMB},
	} {
		if !storeQuantity(w, d.small, d.big, d.perBig, d.smallName, d.bigName, d.min, d.max, d.dst) {
			return
		}
	}
	// The same rule config.Load applies to the environment's pair, applied to
	// the stored one: a workload cannot be capped below what it reserves, and
	// the cluster would refuse every pod.
	// And to a job's default limit, which a job pod pairs with the same
	// request: saved below it, every run was refused by the cluster (code
	// review 2026-11-11, H-5).
	// Compared as a job pod gets them -- zero is the operator's number -- so a
	// request raised past the install's default job limit, with no job limit
	// stored, is refused too. Asked only of a write that names either figure:
	// a pair stored before this rule must not block an unrelated setting.
	jobLimit, cpuRequest := settings.JobCPULimitMillicores, settings.AppCPURequestMillicores
	if jobLimit == 0 {
		jobLimit = s.cfg.JobCPULimitMillicores
	}
	if cpuRequest == 0 {
		cpuRequest = s.cfg.AppCPURequestMillicores
	}
	touched := req.JobCPULimitMillicores != nil || req.JobCPULimitCores != nil ||
		req.AppCPURequestMillicores != nil || req.AppCPURequestCores != nil
	if touched && jobLimit > 0 && jobLimit < cpuRequest {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("the job CPU limit (%dm) is below the CPU request (%dm): a job pod "+
				"cannot be capped below what it reserves, and the cluster would refuse every run",
				jobLimit, cpuRequest),
			"hint": "raise job_cpu_limit_millicores, or lower app_cpu_request_millicores, in the same request"})
		return
	}
	if settings.AppCPULimitMillicores > 0 &&
		settings.AppCPULimitMillicores < settings.AppCPURequestMillicores {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("the CPU limit (%dm) is below the CPU request (%dm): a workload "+
				"cannot be capped below what it reserves, and the cluster would refuse every pod",
				settings.AppCPULimitMillicores, settings.AppCPURequestMillicores),
			"hint": "raise app_cpu_limit_millicores, or lower app_cpu_request_millicores, in the same request"})
		return
	}

	if req.MaxConcurrentBuilds != nil {
		// The ceiling is the pool this process's workers live in. A number above
		// it is refused naming the variable, because the page cannot conjure a
		// worker and a silently clamped value is a field that lies.
		ceiling := builder.PoolSize(s.cfg.MaxConcurrentBuilds)
		if n := *req.MaxConcurrentBuilds; n < 0 || n > ceiling {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("max_concurrent_builds must be 0 (use this install's "+
					"MAX_CONCURRENT_BUILDS) or between 1 and %d — this platform's build workers "+
					"were started as a pool of %d, and the setting cannot conjure another",
					ceiling, ceiling),
				"hint": fmt.Sprintf("send 0, or a number from 1 to %d — more workers take MAX_CONCURRENT_BUILDS raised in the environment and a restart", ceiling)})
			return
		}
		settings.MaxConcurrentBuilds = *req.MaxConcurrentBuilds
	}

	if req.AutoDeployPollSeconds != nil {
		// A floor, because a poller that asks a git host every second is a
		// self-inflicted rate limit, and a ceiling past which the feature is
		// indistinguishable from off — which is spelled by not running the
		// poller at all, in the environment.
		if n := *req.AutoDeployPollSeconds; n != 0 && (n < minPollSeconds || n > maxPollSeconds) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("auto_deploy_poll_seconds must be 0 to use this install's "+
					"AUTO_DEPLOY_POLL_INTERVAL, or between %d and %d seconds. Turning polling off "+
					"is AUTO_DEPLOY_POLL_INTERVAL=0 in the environment: a stored 0 here means the "+
					"operator's number, like every other dial",
					minPollSeconds, maxPollSeconds),
				"hint": `send 0, or a number of seconds such as "auto_deploy_poll_seconds": 300`})
			return
		}
		settings.AutoDeployPollSeconds = *req.AutoDeployPollSeconds
	}

	if req.RPackageRepository != nil {
		// Deliberately unvalidated beyond a length bound — the legitimate value
		// is routinely a private address, and nothing here dials it. See
		// database.SettingRPackageRepository.
		v := strings.TrimSpace(*req.RPackageRepository)
		if len(v) > 2048 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "r_package_repository is too long",
				"hint":  "send the repository's base address only, such as https://packagemanager.posit.co/cran/latest"})
			return
		}
		settings.RPackageRepository = v
	}

	if req.LogLevel != nil {
		// Refused rather than defaulted, the way LOG_FORMAT is: an administrator
		// who typed `verbose` wants more output, and quietly giving them `info`
		// is how somebody spends an afternoon wondering why debug did nothing.
		v := strings.ToLower(strings.TrimSpace(*req.LogLevel))
		if _, ok := config.ParseLogLevel(v); v != "" && !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "log_level must be debug, info, warn or error — or empty to use this " +
					"install's LOG_LEVEL",
				"hint": `send "log_level": "debug", "info", "warn" or "error", or "" to use the install's own`})
			return
		}
		settings.LogLevel = v
	}

	if req.IdleSleepMinutes != nil {
		n := *req.IdleSleepMinutes
		if n != 0 && (n < minIdleSleepMinutes || n > maxIdleSleepMinutes) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "idle_sleep_minutes must be 0 to turn idle sleep off, or between " +
					strconv.Itoa(minIdleSleepMinutes) + " and " + strconv.Itoa(maxIdleSleepMinutes) +
					" minutes. A shorter timeout would sleep apps that had only just finished starting.",
				"hint": `send 0 to keep apps awake, or a number of minutes such as "idle_sleep_minutes": 60`})
			return
		}
		settings.IdleSleepMinutes = n
	}

	// The same bounds as the shared timeout. A wake that takes minutes is not
	// a reason for a higher floor: the floor is about an app that has only just
	// started, and the reaper's clock starts when the pod is serving, not when
	// the wake began.
	if req.IsolatedIdleSleepMinutes != nil {
		n := *req.IsolatedIdleSleepMinutes
		if n != 0 && (n < minIdleSleepMinutes || n > maxIdleSleepMinutes) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "isolated_idle_sleep_minutes must be 0 to keep isolated apps awake, or between " +
					strconv.Itoa(minIdleSleepMinutes) + " and " + strconv.Itoa(maxIdleSleepMinutes) +
					" minutes. A shorter timeout would sleep apps that had only just finished starting.",
				"hint": `send 0 to keep isolated apps awake, or a number of minutes such as "isolated_idle_sleep_minutes": 120`})
			return
		}
		settings.IsolatedIdleSleepMinutes = n
	}

	if req.IsolatedSleepWindowEnabled != nil {
		settings.IsolatedSleepWindowEnabled = *req.IsolatedSleepWindowEnabled
	}
	for _, f := range []struct {
		key string
		in  *string
		out *string
	}{
		{"isolated_sleep_window_start", req.IsolatedSleepWindowStart, &settings.IsolatedSleepWindowStart},
		{"isolated_sleep_window_end", req.IsolatedSleepWindowEnd, &settings.IsolatedSleepWindowEnd},
	} {
		if f.in == nil {
			continue
		}
		v := strings.TrimSpace(*f.in)
		if _, err := database.ParseClock(v); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": f.key + ": " + err.Error() + ", in 24-hour time",
				"hint":  `send a time such as "` + f.key + `": "19:00"`})
			return
		}
		*f.out = v
	}
	// Checked on the merged document, so that moving one bound onto the other
	// is refused whichever of the two was sent.
	if (req.IsolatedSleepWindowStart != nil || req.IsolatedSleepWindowEnd != nil) &&
		settings.IsolatedSleepWindowStart == settings.IsolatedSleepWindowEnd {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the sleep window's start and end are both " + settings.IsolatedSleepWindowStart +
				", which is a window that never opens. Use the switch to stop confining isolated sleep to a window.",
			"hint": `send different times, such as "isolated_sleep_window_start": "19:00" and "isolated_sleep_window_end": "07:00"`})
		return
	}

	if req.DormantLockEnabled != nil {
		settings.DormantLockEnabled = *req.DormantLockEnabled
	}
	if req.DormantLockDays != nil {
		n := *req.DormantLockDays
		// No zero-means-off case here, unlike idle sleep and the version cap
		// above. Off is the switch. A threshold of zero would lock every
		// account on the install including ones created this morning, and a
		// control that destructive should not be reachable by clearing a field.
		if n < database.MinDormantLockDays || n > database.MaxDormantLockDays {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "dormant_lock_days must be between " +
					strconv.Itoa(database.MinDormantLockDays) + " and " +
					strconv.Itoa(database.MaxDormantLockDays) +
					". Below a month this locks people for taking a long holiday; " +
					"use the switch to turn it off.",
				"hint": `send a number of days such as "dormant_lock_days": 90, or "dormant_lock_enabled": false to stop locking`})
			return
		}
		settings.DormantLockDays = n
	}
	// Counted against what is being saved rather than what is stored, for the
	// reason the version cap is: the administrator is asking what this does,
	// and they are asking before it is true. Only while the sweep will be on
	// afterwards — a threshold edited with the feature off changes nothing yet,
	// and a warning about accounts nobody is going to lock is noise.
	if settings.DormantLockEnabled && (req.DormantLockEnabled != nil || req.DormantLockDays != nil) {
		cutoff := time.Now().UTC().AddDate(0, 0, -settings.DormantLockDays)
		if n, err := s.db.CountDormantLockable(cutoff); err == nil && n > 0 {
			subject := "accounts have"
			if n == 1 {
				subject = "account has"
			}
			warning = strconv.Itoa(n) + " " + subject + " been idle this long already and will be " +
				"locked within the hour. Their apps keep running and keep serving; they lose their " +
				"own sessions and personal API keys until an administrator unlocks them. " +
				"Administrators are never locked automatically."
		}
	}

	// Captured before the assignment below replaces it: the warning is about
	// the difference between what is stored and what is being saved.
	storedAbsoluteHours := settings.SessionAbsoluteHours
	if req.SessionAbsoluteHours != nil {
		n := *req.SessionAbsoluteHours
		// No zero-means-off reading, unlike the idle timeout below: there is no
		// value of this that turns sessions off, and a session with no lifetime
		// at all is not a thing an administrator can ask for.
		if n < auth.MinSessionAbsoluteHours || n > auth.MaxSessionAbsoluteHours {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "session_absolute_hours must be between " +
					strconv.Itoa(auth.MinSessionAbsoluteHours) + " and " +
					strconv.Itoa(auth.MaxSessionAbsoluteHours) +
					". A day is the longest anything can reasonably be called a session.",
				"hint": `send a number of hours such as "session_absolute_hours": 12`})
			return
		}
		settings.SessionAbsoluteHours = n
	}
	if req.SessionIdleMinutes != nil {
		n := *req.SessionIdleMinutes
		if n != 0 && (n < auth.MinSessionIdleMinutes || n > auth.MaxSessionIdleMinutes) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "session_idle_minutes must be 0 to turn the idle timeout off, or between " +
					strconv.Itoa(auth.MinSessionIdleMinutes) + " and " +
					strconv.Itoa(auth.MaxSessionIdleMinutes) +
					" minutes. A shorter timeout signs people out while they are reading a page.",
				"hint": `send 0 for no idle timeout, or a number of minutes such as "session_idle_minutes": 60`})
			return
		}
		settings.SessionIdleMinutes = n
	}
	if req.AccessRecordRetentionDays != nil {
		n := *req.AccessRecordRetentionDays
		if n != 0 && (n < database.MinAccessRecordRetentionDays || n > database.MaxAccessRecordRetentionDays) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "access_record_retention_days must be 0 to keep everything, or from " +
					strconv.Itoa(database.MinAccessRecordRetentionDays) + " to " +
					strconv.Itoa(database.MaxAccessRecordRetentionDays) + " days. A year is the " +
					"floor because it is the common retention requirement, and a shorter record " +
					"would let this install fail its audit from this page.",
				"hint": `send 0 to keep everything, or a number of days such as "access_record_retention_days": 400`,
			})
			return
		}
		settings.AccessRecordRetentionDays = n
	}
	// Counted against what is being saved rather than what is stored, for the
	// reason the version cap and the dormancy threshold are: the administrator
	// is asking what this does, and they are asking before it is true. Shorter
	// than what people are holding is the case worth warning about — a policy
	// that only bit at the next sign-in would be a control an assessor could
	// not test, so this one ends sessions that are open right now.
	if req.SessionAbsoluteHours != nil &&
		settings.SessionAbsoluteHours < storedAbsoluteHours {
		warning = "Sessions already open are held to this too, not just new ones. " +
			"Anyone who signed in more than " + strconv.Itoa(settings.SessionAbsoluteHours) +
			" hours ago is signed out on their next request — including you. " +
			"Personal API keys are unaffected."
	}

	if req.MaxVersions != nil {
		n := *req.MaxVersions
		// The floor is 2, not 1, and the reason is worth stating: a cap of one
		// leaves an app with nothing but the release it is serving, so there is
		// no build to roll back to when a deploy goes wrong. The cap lowers
		// what every app keeps, not only what the over-eager ones keep, so a
		// figure that forbids rollback install-wide is not one to accept
		// quietly.
		if n != 0 && (n < minVersionCap || n > maxVersionCap) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "max_versions_per_app must be 0 for no limit, or between " +
					strconv.Itoa(minVersionCap) + " and " + strconv.Itoa(maxVersionCap) +
					". A cap of 1 would leave no earlier release to roll back to.",
				"hint": `send 0 for no limit, or a cap such as "max_versions_per_app": 10`})
			return
		}
		// Counted against the figure being saved rather than the stored one,
		// because the question an admin is asking is "what does this do", and
		// they are asking it before it is true.
		if n > 0 {
			if blocked, err := s.db.CountAppsOverVersionCap(n); err == nil && blocked > 0 {
				subject := "apps have"
				if blocked == 1 {
					subject = "app has"
				}
				warning = strconv.Itoa(blocked) + " " + subject + " automatic cleanup turned off and " +
					"already at or above this limit. Those apps keep running and keep serving, but " +
					"their next deploy is refused until someone deletes older versions or turns " +
					"cleanup back on."
			}
		}
		settings.MaxVersions = n
	}

	if req.APIKeysEnabled != nil {
		// Warned rather than refused: switching this off is a legitimate — and
		// occasionally urgent — thing to do, and the administrator doing it is
		// entitled to know how many live credentials it takes down with it.
		if !*req.APIKeysEnabled && settings.APIKeysEnabled {
			if keys, err := s.db.ListAllUserAPIKeys(1); err == nil && len(keys) > 0 {
				subject := "keys stop"
				if len(keys) == 1 {
					subject = "key stops"
				}
				warning = strconv.Itoa(len(keys)) + " personal API " + subject +
					" working immediately. They are not deleted — turning this back on restores them."
			}
		}
		settings.APIKeysEnabled = *req.APIKeysEnabled
	}

	if req.APIKeyMaxLifetimeDays != nil {
		n := *req.APIKeyMaxLifetimeDays
		if n < 0 || n > database.MaxAPIKeyMaxLifetimeDays {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "api_key_max_lifetime_days must be 0 for no limit, or between 1 and " +
					strconv.Itoa(database.MaxAPIKeyMaxLifetimeDays),
				"hint": `send 0 for no limit, or a number of days such as "api_key_max_lifetime_days": 90`})
			return
		}
		// Said rather than left to be discovered: a cap bounds the keys made
		// from now on, and an administrator setting one is usually asking
		// about the keys that already exist.
		if n > 0 && n != settings.APIKeyMaxLifetimeDays && warning == "" {
			warning = "New personal API keys will expire within " + strconv.Itoa(n) +
				" days. Keys that already exist keep the expiry they were created with."
		}
		settings.APIKeyMaxLifetimeDays = n
	}

	if req.PublicAppsEnabled != nil {
		// Warned rather than refused, like the switch above: this is a
		// legitimate and occasionally urgent thing to do, and the number of
		// apps it takes off the internet is the one fact the administrator
		// doing it is entitled to have.
		//
		// The warning names what is *reversible*, because that is the half
		// people get wrong about this control: no column is written, so
		// turning it back on republishes exactly the same apps.
		if !*req.PublicAppsEnabled && settings.PublicAppsEnabled {
			if n, _, _, err := s.db.CountAppsByVisibility(); err == nil && n > 0 {
				subject := "apps stop being reachable"
				if n == 1 {
					subject = "app stops being reachable"
				}
				warning = strconv.Itoa(n) + " " + subject + " without signing in, from now. " +
					"Everyone who signs in keeps the access they had, and nothing is changed on the " +
					"apps themselves — turning this back on publishes them again."
			}
		}
		settings.PublicAppsEnabled = *req.PublicAppsEnabled
	}

	if req.AppSecurityHeaders != nil {
		settings.AppSecurityHeaders = *req.AppSecurityHeaders
	}
	if req.AudienceMeasurementEnabled != nil {
		settings.AudienceMeasurementEnabled = *req.AudienceMeasurementEnabled
	}
	if req.AppLogCopyUncapped != nil {
		if *req.AppLogCopyUncapped && !settings.AppLogCopyUncapped {
			warning = "Every line shared apps print now reaches the log collector. One app's " +
				"volume can now slow every app's requests, and a full queue still drops and counts."
		}
		settings.AppLogCopyUncapped = *req.AppLogCopyUncapped
	}

	if req.AppsListableToAuthenticated != nil {
		// Warned rather than refused, and the warning is about *disclosure*
		// rather than about access: nothing becomes readable, and the count is
		// the fact an administrator switching this on is entitled to have,
		// because what they are agreeing to is that many names, slugs and
		// owners becoming visible to everybody who can sign in.
		if *req.AppsListableToAuthenticated && !settings.AppsListableToAuthenticated {
			if apps, err := s.db.ListAllApps(); err == nil && len(apps) > 0 {
				subject := "apps appear"
				if len(apps) == 1 {
					subject = "app appears"
				}
				warning = strconv.Itoa(len(apps)) + " " + subject + " in every signed-in person's " +
					"list, by name and owner. Nobody can open an app they were not already given — " +
					"they can only ask for it."
			}
		}
		settings.AppsListableToAuthenticated = *req.AppsListableToAuthenticated
	}

	if req.GalleryMode != nil {
		mode := strings.TrimSpace(*req.GalleryMode)
		// Refused by name rather than clamped to off: an administrator who
		// typed something meant something, and quietly storing "off" is how a
		// setting comes to disagree with the person who set it.
		if !database.ValidGalleryMode(mode) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "gallery_deploy_mode must be one of " + database.GalleryOff + ", " +
					database.GalleryExtensionsOnly + ", " + database.GalleryAll + ".",
				"hint": `send one of them, such as "gallery_deploy_mode": "` + database.GalleryExtensionsOnly + `"`})
			return
		}
		// Said rather than assumed: this switch curates the catalog and
		// restricts nothing else. An administrator who reads it as "nobody can
		// deploy that code now" has a control they do not have.
		if mode != database.GalleryOff && settings.GalleryMode == database.GalleryOff {
			warning = "The gallery now fetches its catalog from " +
				"the address shown beside this setting. It controls the gallery only — anyone who " +
				"may publish can still deploy any repository from New App."
		}
		settings.GalleryMode = mode
	}

	if msg := s.applyDependencySettings(&req, &settings, &warning); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "hint": "correct the field the sentence names and send the request again — nothing was saved"})
		return
	}

	// The bounded numbers, each refused with a message naming its range rather
	// than clamped silently. An administrator who typed 5000 meant something,
	// and storing 900 without saying so is how a setting comes to disagree with
	// the person who set it.
	for _, f := range []struct {
		val    *int
		field  *int
		name   string
		lo, hi int
		tail   string
	}{
		{req.DefaultJobTimeoutSecs, &settings.DefaultJobTimeoutSecs, "default_job_timeout_seconds",
			database.MinJobTimeoutSeconds, database.MaxJobTimeoutSeconds,
			"This is the timeout of a job that declares none in toft.toml."},
		{req.MaxJobTimeoutSecs, &settings.MaxJobTimeoutSecs, "max_job_timeout_seconds",
			database.MinJobTimeoutSeconds, database.MaxJobTimeoutSeconds,
			"A job declaring more is refused at check and deploy. Lowering it caps the jobs " +
				"already declared above it, and their apps are told."},
		{req.MaxQueuedRunsPerApp, &settings.MaxQueuedRunsPerApp, "max_queued_runs_per_app",
			database.MinQueuedRunsPerApp, database.MaxQueuedRunsPerApp,
			"Runs beyond an app's runs-at-once limit wait in its queue; past this many waiting, " +
				"a start is refused with queue_full."},
		{req.NotebookRenderTimeoutSecs, &settings.NotebookRenderTimeoutSecs, "notebook_render_timeout_seconds",
			database.MinNotebookRenderTimeoutSeconds, database.MaxNotebookRenderTimeoutSeconds,
			"This is how long a notebook's cells may take to execute during a deploy. It is the " +
				"one build step whose length is a property of the work rather than of package " +
				"downloads. Builds run one at a time unless MAX_CONCURRENT_BUILDS says otherwise, " +
				"so a long render holds up every other deploy on the install for that long."},
		{req.BuildTimeoutSecs, &settings.BuildTimeoutSecs, "build_timeout_seconds",
			database.MinBuildTimeoutSeconds, database.MaxBuildTimeoutSeconds,
			"This is how long any one build command may run — installing dependencies, " +
				"compiling, bundling. It was a fixed ten minutes chosen for `npm ci`, which is " +
				"the wrong figure for an R app: restoring a couple of hundred packages is " +
				"ordinary, and a package your repository has no binary for is compiled from " +
				"source. Builds run one at a time unless MAX_CONCURRENT_BUILDS says otherwise, " +
				"so raising this raises how long one deploy can hold up every other one. An " +
				"individual app can be given more without moving this figure."},
		{req.JobLogRetentionDays, &settings.JobLogRetentionDays, "job_log_retention_days",
			database.MinJobLogRetentionDays, database.MaxJobLogRetentionDays,
			"This is how long a background job's log lines are kept."},
		{req.JobRunRetentionDays, &settings.JobRunRetentionDays, "job_run_retention_days",
			database.MinJobRunRetentionDays, database.MaxJobRunRetentionDays,
			"This is how long a background job's run rows are kept."},
		{req.AppEventRetentionDays, &settings.AppEventRetentionDays, "app_event_retention_days",
			database.MinAppEventRetentionDays, database.MaxAppEventRetentionDays,
			"This is how long app failure events are kept — the system half of Activity, and " +
				"every app's own Logs tab. It does not affect the audit trail, which records " +
				"what people did and is kept on access_record_retention_days."},
		{req.PlatformLogRetentionDays, &settings.PlatformLogRetentionDays, "platform_log_retention_days",
			database.MinPlatformLogRetentionDays, database.MaxPlatformLogRetentionDays,
			"This is how long the platform's own log is kept. Keep it at least as long as " +
				"app_event_retention_days: an event's detail points at this log."},
		{req.AnnouncementRecipientRetentionDays, &settings.AnnouncementRecipientRetentionDays,
			"announcement_recipient_retention_days",
			database.MinAnnouncementRecipientRetentionDays, database.MaxAnnouncementRecipientRetentionDays,
			"This is how long the list of who each announcement was mailed to is kept. The " +
				"announcement itself and its counts are never deleted — what goes is the " +
				"row naming each person."},
		{req.ReportDeliveryMinIntervalMinutes, &settings.ReportDeliveryMinIntervalMinutes,
			"report_delivery_min_interval_minutes",
			database.MinReportDeliveryMinIntervalMinutes, database.MaxReportDeliveryMinIntervalMinutes,
			"This is the shortest gap, in minutes, between two deliveries of one report; 0 means none."},
	} {
		if f.val == nil {
			continue
		}
		if *f.val < f.lo || *f.val > f.hi {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": f.name + " must be between " + strconv.Itoa(f.lo) + " and " +
					strconv.Itoa(f.hi) + ". " + f.tail,
				"hint": "send a whole number inside that range, or leave " + f.name + " out to keep the stored value"})
			return
		}
		*f.field = *f.val
	}

	// Rows must outlive logs. Refused rather than clamped: both figures are
	// individually valid, so clamping would silently move one of them and the
	// administrator would find a number they did not type. The invariant is
	// that a run's explanation survives its output — a run row deleted while
	// its logs remain is a log nobody can attribute.
	if settings.JobRunRetentionDays < settings.JobLogRetentionDays {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "job_run_retention_days (" + strconv.Itoa(settings.JobRunRetentionDays) +
				") cannot be less than job_log_retention_days (" +
				strconv.Itoa(settings.JobLogRetentionDays) + "). A run's cost and outcome stay " +
				"interesting long after its output does, and a run's output object is deleted " +
				"with its row — so rows outliving logs is the order that keeps a finished run " +
				"explainable.",
			"hint": "raise job_run_retention_days, or lower job_log_retention_days, in the same request"})
		return
	}

	// A default above the ceiling would refuse a job for declaring nothing.
	if settings.DefaultJobTimeoutSecs > settings.MaxJobTimeoutSecs {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"code": "default_above_ceiling",
			"hint": "lower default_job_timeout_seconds in the same request",
			"error": "default_job_timeout_seconds (" + strconv.Itoa(settings.DefaultJobTimeoutSecs) +
				") cannot be more than max_job_timeout_seconds (" +
				strconv.Itoa(settings.MaxJobTimeoutSecs) + "). The default is what a job that " +
				"declares no timeout gets, and it has to be one the install allows."})
		return
	}

	// The messages are shown to people who cannot get in, so they are plain
	// text and stay that way: the login page renders them as text, and the
	// length cap is here rather than only in the form because the form is not
	// the only caller.
	for _, m := range []struct {
		val   *string
		field *string
		name  string
	}{
		{req.LockedMessage, &settings.LockedMessage, "locked_account_message"},
		{req.NoAccountMessage, &settings.NoAccountMessage, "no_account_message"},
		{req.AppLockedMessage, &settings.AppLockedMessage, "app_locked_message"},
		{req.PendingActivationMessage, &settings.PendingActivationMessage, "pending_activation_message"},
		{req.ActivationMessage, &settings.ActivationMessage, "account_activated_message"},
	} {
		if m.val == nil {
			continue
		}
		v := strings.TrimSpace(*m.val)
		if len([]rune(v)) > messageLimit {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": m.name + " must be " + strconv.Itoa(messageLimit) + " characters or fewer",
				"hint":  "shorten " + m.name + ", or send it empty to restore the default message"})
			return
		}
		// Empty is meaningful: it restores the default, which the database
		// layer substitutes on write.
		*m.field = v
	}

	if req.EnvironmentTag != nil {
		v := strings.TrimSpace(*req.EnvironmentTag)
		if v != "" && !envTagRe.MatchString(v) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "environment_tag must be 1-32 lowercase letters, digits, dashes, or " +
					"underscores (it names this install in telemetry); leave it empty to name none",
				"hint": `send a tag such as "environment_tag": "prod-east", or "" for none`})
			return
		}
		settings.EnvironmentTag = v
	}

	if req.InstallName != nil {
		v, err := database.NormalizeInstallName(*req.InstallName)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "The install name " + err.Error() + ". It is what emails and " +
					"notifications call this install; leave it empty to use “Toft”.",
				"hint": `send a short, plain name such as "install_name": "Analytics apps", or "" for the default`})
			return
		}
		settings.InstallName = v
	}

	if req.DisplayTimeZone != nil {
		v, err := database.NormalizeTimeZone(*req.DisplayTimeZone)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "display_time_zone " + err.Error(),
				"hint":  `send an IANA name such as "display_time_zone": "America/New_York", "UTC", or "" for each viewer's browser zone`})
			return
		}
		settings.DisplayTimeZone = v
	}

	if req.PublicBaseURL != nil {
		v, err := database.NormalizePublicBaseURL(*req.PublicBaseURL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "public_base_url " + err.Error() + ". This is the address links in " +
					"notification email point at; leave it empty to use the BASE_URL this " +
					"install was started with.",
				"hint": `send an address such as "public_base_url": "https://toft.example.com", or "" to use BASE_URL`})
			return
		}
		// Saying so beats a silently wrong link in everybody's mail: the
		// address being stored is not the one this very request arrived at, so
		// either the administrator meant it (a public name this box does not
		// answer to directly is a normal thing behind a proxy) or they have
		// just pointed every notification somewhere nobody can sign in.
		if observed := publicObservedOrigin(r); v != "" && observed != "" && observed != normalizeOrigin(v) &&
			warning == "" {
			warning = "Saved, but note that you reached this install at " + observed +
				" and links in email will now point at " + v + ". If those should be the same " +
				"address, use the one shown as this install's observed address."
		}
		settings.PublicBaseURL = v
	}

	if req.GarthURL != nil {
		v, err := database.NormalizeGarthURL(*req.GarthURL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "garth_url " + err.Error() + ". This is where your organization's Garth " +
					"answers; with it set, an app's owners and editors get an Open in Garth link.",
				"hint": `send an address such as "garth_url": "https://garth.example.com", or "" to remove the link`})
			return
		}
		settings.GarthURL = v
	}

	if msg := applySecurityTxtSettings(&req, &settings, &warning); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "hint": "correct the field the sentence names and send the request again — nothing was saved"})
		return
	}

	meta := requestMeta(r.Context())
	if err := s.db.SetPlatformSettingsIfUnchanged(&settings, since, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if errors.Is(err, database.ErrSettingsChanged) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "settings changed underneath you — reload the page and make the change again",
				"code":  "settings_changed",
				"hint":  "GET /api/v1/system/settings for the current values and their updated_at, then send your change again"})
			return
		}
		log.Printf("settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	updatedAt, _ := s.db.PlatformSettingsUpdatedAt()

	// The one dial that takes effect now. Applied after the write rather than
	// before it, so a failed write never changes what this process is saying,
	// and resolved through the envelope so an emptied field goes back to the
	// environment's level rather than to nothing.
	if s.logLevel != nil {
		s.logLevel.Set(s.envelope().LogLevel)
	}
	// The re-baseline, placed here for the same reason: it is a write across
	// every app, and a save refused two screens up must not have made it.
	if resumesDependencyWatch(&req, &before, &settings) {
		s.rebaselineOnResume()
	}
	// Switching the gallery on fetches the catalog now, as the save's own
	// sentence says. It waited for the six-hourly timer, so a publisher opening
	// Gallery straight after saw an empty page (docs/bugs
	// 2026-10-22-e2e-apptypes #1). In the background: the request is not
	// held on a third party, and a failure is the timer's to retry.
	if s.gallery != nil && before.GalleryMode == database.GalleryOff &&
		settings.GalleryMode != database.GalleryOff {
		go func() {
			if err := s.gallery.Sync(context.Background()); err != nil {
				log.Printf("gallery: sync on enabling: %v", err)
			}
		}()
	}

	if over := s.limitsAboveThisMachine(&before, &settings); over != "" {
		warning = strings.TrimSpace(warning + " " + over)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"settings": settingsWritten{
			PlatformSettings:        settings,
			UpdatedAt:               updatedAt,
			AppMemoryLimitMB:        settings.AppMemoryLimitMB,
			AppMemoryLimitGB:        gigabytesOf(settings.AppMemoryLimitMB),
			AppCPURequestMillicores: settings.AppCPURequestMillicores,
			AppCPURequestCores:      coresOf(settings.AppCPURequestMillicores),
			AppCPULimitMillicores:   settings.AppCPULimitMillicores,
			AppCPULimitCores:        coresOf(settings.AppCPULimitMillicores),
			JobMemoryLimitMB:        settings.JobMemoryLimitMB,
			JobMemoryLimitGB:        gigabytesOf(settings.JobMemoryLimitMB),
			JobCPULimitMillicores:   settings.JobCPULimitMillicores,
			JobCPULimitCores:        coresOf(settings.JobCPULimitMillicores),
		},
		"warning": warning,
	})
}

// settingsWritten is the stored document as the wire carries it back: the
// document, plus the one field whose unit differs between the row and the API.
// It exists so the echo after a write is the same shape as the read before it —
// an administrator who typed 0.333 sees 0.33 come back from the same field.
type settingsWritten struct {
	database.PlatformSettings
	UpdatedAt               time.Time `json:"updated_at"`
	AppMemoryLimitMB        int       `json:"app_memory_limit_mb"`
	AppMemoryLimitGB        float64   `json:"app_memory_limit_gb"`
	AppCPURequestMillicores int       `json:"app_cpu_request_millicores"`
	AppCPURequestCores      float64   `json:"app_cpu_request_cores"`
	AppCPULimitMillicores   int       `json:"app_cpu_limit_millicores"`
	AppCPULimitCores        float64   `json:"app_cpu_limit_cores"`
	JobMemoryLimitMB        int       `json:"job_memory_limit_mb"`
	JobMemoryLimitGB        float64   `json:"job_memory_limit_gb"`
	JobCPULimitMillicores   int       `json:"job_cpu_limit_millicores"`
	JobCPULimitCores        float64   `json:"job_cpu_limit_cores"`
}

// storeQuantity holds one dial that has two wire spellings inside its range and
// writes it, whichever unit it arrived in.
//
// The bounds are on the stored number, so the two spellings cannot disagree
// about where the floor is; the large one is checked before the rounding, so a
// value that would round up to the floor is refused rather than accepted at a
// number the caller did not type. It answers false when it has already written
// the refusal.
func storeQuantity(w http.ResponseWriter, small *int, big *float64, perBig int,
	smallName, bigName string, min, max int, dst *int) bool {
	if small == nil && big == nil {
		return true
	}
	if small != nil && big != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "send " + smallName + " or " + bigName + ", not both — they are the same " +
				"number in two units and the platform will not guess which you meant",
			"hint": "drop " + bigName + " and send only " + smallName + ", or the other way round"})
		return false
	}
	n := 0
	field := smallName
	if small != nil {
		n = *small
	} else {
		field = bigName
		f := *big
		if f != 0 && f < float64(min)/float64(perBig) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("%s must be 0, or at least %g", field, float64(min)/float64(perBig)),
				"hint":  "send 0 to use this install's own figure, or a value at or above that floor"})
			return false
		}
		n = int(math.Round(f * float64(perBig)))
	}
	if n < 0 || (n != 0 && n < min) || n > max {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%s must be 0 to use this install's own figure, or between %d and %d",
				field, min, max),
			"hint": "leave " + field + " out to keep the stored value, or send one inside that range"})
		return false
	}
	*dst = n
	return true
}

// coresPhrase is a millicore figure as a person reads it: "2 cores", "0.5 cores",
// "1 core".
func coresPhrase(millicores int) string {
	if millicores == millicoresPerCore {
		return "1 core"
	}
	return strconv.FormatFloat(coresOf(millicores), 'f', -1, 64) + " cores"
}

// coresOf states a stored millicore figure in the unit an operator reserves in.
func coresOf(millicores int) float64 {
	return float64(millicores) / millicoresPerCore
}

// The bounds on the dials that are not the memory ceiling.
//
// The CPU ceiling is 64 cores rather than a fraction of the box: the machine has
// whatever it has, and a number this code cannot know is not a policy. The poll
// floor keeps the platform from asking a git host every second, and its ceiling
// is a day, past which polling is indistinguishable from off — and off is spelled
// in the environment, by not constructing the poller at all.
const (
	millicoresPerCore = 1000
	maxCPUMillicores  = 64000
	// minCPULimitMillicores is the smallest CPU limit an administrator may
	// set on an app or a job: a twentieth of a core, below which an
	// interpreter spends its start throttled.
	minCPULimitMillicores = 50
	minPollSeconds        = 10
	maxPollSeconds        = 86400
)

// storeMemoryLimit holds one ceiling inside its range and writes it, whichever
// unit it arrived in. The bounds are on the stored number so the two spellings
// cannot disagree about where the floor is, and the refusal names both units,
// because the person reading it typed one of them and the reason is about the
// other. It answers false when it has already written the refusal.
func storeMemoryLimit(w http.ResponseWriter, mb int, field string, dst *int) bool {
	if mb != 0 && mb < minAppMemoryLimitMB {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": field + " must be 0, or at least " + minMemoryLimitPhrase,
			"hint":  "send 0 to use this install's own figure, or a figure at or above that floor"})
		return false
	}
	if mb > maxAppMemoryLimitMB {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": field + " must be at most " + strconv.Itoa(maxAppMemoryLimitMB) + " MB (" +
				strconv.Itoa(maxAppMemoryLimitMB/mebibytesPerGigabyte) + " GB)",
			"hint": "send a smaller figure, or 0 to use this install's own"})
		return false
	}
	*dst = mb
	return true
}

// The bounds on the memory ceiling, in the unit it is stored in, and the one
// refusal that names both units — because the person reading it typed
// gigabytes and the reason is about megabytes.
const (
	minAppMemoryLimitMB  = 64
	maxAppMemoryLimitMB  = 1048576
	mebibytesPerGigabyte = 1024

	minMemoryLimitPhrase = "64 MB (0.0625 GB): " +
		"a cap below that kills an interpreter before it has finished starting"
)

// gigabytesOf states a stored ceiling in the unit the wire carries, with no
// precision the install does not hold. A gigabyte is 1024 mebibytes here, which
// is what both backends count in.
func gigabytesOf(mb int) float64 {
	return float64(mb) / mebibytesPerGigabyte
}

// sharedContainment is how the shared-mode agent holds this machine's
// workloads. Asked of the backend, never derived here: the tier is detected
// once, by the agent, at its start, and a second opinion would be the platform
// guessing.
type sharedContainment struct {
	Tier   string `json:"tier"`
	Reason string `json:"reason,omitempty"`
	Bridge string `json:"bridge,omitempty"`
}

func (s *Server) sharedContainment() *sharedContainment {
	c, ok := s.rts.Shared().(interface {
		Containment() (tier, reason string)
	})
	if !ok {
		return nil
	}
	tier, reason := c.Containment()
	if tier == "" {
		return nil
	}
	out := &sharedContainment{Tier: tier, Reason: reason}
	if b, ok := s.rts.Shared().(interface{ Bridge() string }); ok {
		out.Bridge = b.Bridge()
	}
	return out
}

// limitsAboveThisMachine is a warning, never a refusal, for a job limit raised
// above what the machine the platform runs on has. Accepted silently, a limit
// of 64 GB on a 16 GB box read as headroom the box does not have. Not
// refused, because on a cluster the platform's machine is not where jobs run
// and a node may well be larger — which is also why it is only said when the
// backend reports a machine at all (shared mode).
func (s *Server) limitsAboveThisMachine(before, after *database.PlatformSettings) string {
	memChanged := after.JobMemoryLimitMB != before.JobMemoryLimitMB
	cpuChanged := after.JobCPULimitMillicores != before.JobCPULimitMillicores
	if !memChanged && !cpuChanged {
		return ""
	}
	if s.rts == nil {
		return ""
	}
	m, err := s.rts.SystemMetrics()
	if err != nil || m == nil {
		return ""
	}
	var parts []string
	if hostMB := int(m.MemoryTotal >> 20); memChanged && hostMB > 0 && after.JobMemoryLimitMB > hostMB {
		parts = append(parts, fmt.Sprintf("the job memory limit (%d MB) is more than this machine has (%d MB)",
			after.JobMemoryLimitMB, hostMB))
	}
	if cores := m.CPUCount * millicoresPerCore; cpuChanged && cores > 0 && after.JobCPULimitMillicores > cores {
		parts = append(parts, fmt.Sprintf("the job CPU limit (%dm) is more than this machine's %d cores",
			after.JobCPULimitMillicores, m.CPUCount))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Saved, but " + strings.Join(parts, ", and ") + ", so a job will meet the machine's ceiling first."
}
