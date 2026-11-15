package database

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"toft/internal/advisory"
	"toft/internal/auth"
	"toft/internal/config"
	"toft/internal/runtime"
	"toft/internal/sources"
)

// Platform-wide key/value settings, over the `settings` table from the initial
// schema. Two kinds of thing live here and they are worth telling apart:
//
//   - Content an admin publishes, like the announcement banner. Free-form,
//     read by anyone.
//   - Capability switches, like whether isolated execution is offered. These
//     decide what the platform will do and what it will spend, so they are
//     read through a typed accessor and every change is audited.
//
// What does *not* live here is deployment configuration — where the database
// is, which cluster to use, what the runner image is called. Those are
// environment variables because they are facts about how the install was
// stood up, not decisions an administrator revisits.

// Setting is one raw row.
type Setting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

// GetSetting returns one setting, or nil when it has never been set.
func (db *DB) GetSetting(key string) (*Setting, error) {
	s := &Setting{}
	var updatedBy sql.NullString
	err := db.conn.QueryRow(
		`SELECT key, value, updated_at, COALESCE(updated_by,'') FROM settings WHERE key = ?`, key).
		Scan(&s.Key, &s.Value, &s.UpdatedAt, &updatedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.UpdatedBy = updatedBy.String
	return s, nil
}

// PutSetting writes a setting and returns the stored row.
func (db *DB) PutSetting(key, value, actorID string) (*Setting, error) {
	now := time.Now().UTC()
	_, err := db.conn.Exec(`
		INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		key, value, now, nullIfEmpty(actorID))
	if err != nil {
		return nil, err
	}
	db.invalidateRequestPolicy()
	return &Setting{Key: key, Value: value, UpdatedAt: now, UpdatedBy: actorID}, nil
}

// DeleteSetting removes a setting. Deleting one that was never set is not an
// error — clearing an announcement that is already clear is a no-op.
func (db *DB) DeleteSetting(key string) error {
	_, err := db.conn.Exec(`DELETE FROM settings WHERE key = ?`, key)
	db.invalidateRequestPolicy()
	return err
}

// PutSettingAudited writes one settings row and its audit entry in the same
// transaction, and DeleteSettingAudited does the same for a clear.
//
// The pair exists because the two shapes already in this file are both wrong
// for a single administrator-published key. `PutSetting` writes and records
// nothing, so an install-wide change to what everybody sees leaves no trace of
// who made it. `SetPlatformSettings` records properly but its one
// `platform_settings.update` row carries the whole struct as before and after,
// which is a lie about a change to one key.
//
// One transaction rather than a write followed by a best-effort log: a setting
// everybody can see with no row saying who set it is exactly the outcome the
// audit exists to prevent, and it is cheap to make impossible here.
//
// The caller owns Action, ActorType, ActorID, TargetType, TargetID, Before and
// After. Everything request-scoped is already on the entry from auditEntryFor.
func (db *DB) PutSettingAudited(key, value string, e AuditEntry) (*Setting, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if _, err := tx.Exec(`
		INSERT INTO settings (key, value, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		key, value, now, nullIfEmpty(e.ActorID)); err != nil {
		return nil, err
	}
	if err := writeAuditTx(tx, e); err != nil {
		return nil, fmt.Errorf("auditing %s: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// After the commit, never before — the same rule SetPlatformSettings
	// states: a cache dropped ahead of a rollback reloads the old value and
	// looks like the write worked.
	db.invalidateRequestPolicy()
	return &Setting{Key: key, Value: value, UpdatedAt: now, UpdatedBy: e.ActorID}, nil
}

// DeleteSettingAudited removes a setting and records the clear. Clearing one
// that was never set is not an error and still writes the row: "somebody
// pressed clear" is a fact worth having whether or not there was anything to
// remove.
func (db *DB) DeleteSettingAudited(key string, e AuditEntry) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, key); err != nil {
		return err
	}
	if err := writeAuditTx(tx, e); err != nil {
		return fmt.Errorf("auditing %s clear: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	db.invalidateRequestPolicy()
	return nil
}

// --- Capability switches ---

// Keys are constants rather than free strings so a typo is a compile error
// rather than a silently-defaulted setting.
const (
	// SettingIsolatedEnabled is the administrator's half of whether isolated
	// mode is on offer. The operator's half is whether a cluster is configured
	// at all; both must be true.
	//
	// Default false, deliberately. Enabling isolated mode on an existing
	// install requires a one-time rebuild of every app — natively built
	// artifacts are architecture- and libc-specific and the runner image
	// expects its own — so it must be a decision someone makes, never
	// something that switches itself on when an operator adds cluster
	// configuration.
	SettingIsolatedEnabled = "isolated_execution_enabled"

	// SettingDefaultExecutionMode is what a *new* app is born as when the
	// request names no mode: "shared" (the default) or "isolated".
	//
	// A default, not a policy. It is read once, at insert, and never again — an
	// existing app's mode is its own column and this key never touches it in
	// either direction, so turning it on schedules no fleet-wide rebuild and
	// turning it off demotes nothing.
	//
	// It exists because the per-request field is administrator-only, and on a
	// real install most apps are created by developers with the publisher role.
	// Without it, an operator who has decided every app should be isolated has
	// no way to get one from the people who create most of them.
	SettingDefaultExecutionMode = "default_execution_mode"

	// SettingJobsEnabled gates background jobs. On by default: jobs are
	// Toft's one automation concept, and they run on every install — in a
	// container where a cluster is available, on this machine otherwise
	// (ADR §106). An administrator may still switch them off.
	SettingJobsEnabled = "background_jobs_enabled"

	// SettingJobConcurrency is how many of one app's runs may execute at once.
	// A run past it waits in the queue rather than being refused, so it
	// protects the shared footprint from one app's burst without failing it.
	SettingJobConcurrency = "job_concurrency_per_app"

	// SettingAppMemoryLimitMB caps each app's workload, in megabytes.
	//
	// **Zero means "the operator's number", not "uncapped".** APP_MEMORY_LIMIT_MB
	// is where this install's cap has always come from and an unset row must
	// change nothing, so the environment is the floor and this key is an
	// administrator's override of it. An administrator who genuinely wants no
	// cap sets the environment variable to 0; a runtime switch that could
	// uncap every workload on the box is not something the settings page
	// should offer.
	//
	// **It reaches a workload at its next start and never before**, because a
	// backend bakes the cap into the process — the limit in the agent's
	// description of it (its watchdog's line, or the cgroup's memory.max), the
	// container's memory limit — and a start carries the configuration as it
	// is now. The page says so rather than implying a live change; the
	// alternative is an administrator raising the number, watching an app keep
	// dying, and concluding the setting does nothing.
	SettingAppMemoryLimitMB = "app_memory_limit_mb"

	// The rest of the operating dials (ADR §14). Each follows
	// SettingAppMemoryLimitMB's two rules exactly: **zero or empty means the
	// operator's value, never "off" and never "unlimited"**, and each reaches
	// its consumer the next time that consumer reads it — a start, a dequeue, a
	// tick — rather than immediately. The whole set is config.Dials, and that
	// list is the specification.
	//
	// SettingAppCPURequestMillicores and SettingAppCPULimitMillicores are the
	// scheduler's, isolated mode only. Stored in millicores, offered on the
	// wire in cores as well, for the reason the memory ceiling is offered in
	// gigabytes.
	SettingAppCPURequestMillicores = "app_cpu_request_millicores"
	SettingAppCPULimitMillicores   = "app_cpu_limit_millicores"

	// SettingJobMemoryLimitMB and SettingJobCPULimitMillicores are a job
	// run's own defaults, separate from the app's: neither the app's limits
	// nor the manifest reach a job, and an administrator overrides them per
	// job (job_definitions.override_*).
	SettingJobMemoryLimitMB      = "job_memory_limit_mb"
	SettingJobCPULimitMillicores = "job_cpu_limit_millicores"

	// SettingMaxConcurrentBuilds is how many builds may **start**, not the size
	// of the worker pool: the pool is fixed at process start, because resizing
	// one at runtime means stopping workers, which means abandoning a build or
	// waiting on one. Lowering this stops new builds starting and interrupts
	// none; raising it takes effect at the next dequeue, bounded above by the
	// pool MAX_CONCURRENT_BUILDS built.
	SettingMaxConcurrentBuilds = "max_concurrent_builds"

	// SettingAutoDeployPollSeconds is how often the poller looks.
	//
	// **Its zero means something different from the environment's**, and that
	// is stated rather than designed around: AUTO_DEPLOY_POLL_INTERVAL=0 means
	// no poller is ever constructed, while a stored 0 means "the operator's
	// number" like every other dial. Turning auto-deploy off stays where it has
	// always been, because "is auto-deploy offered" is a capability question and
	// would be a different key.
	SettingAutoDeployPollSeconds = "auto_deploy_poll_seconds"

	// SettingRPackageRepository is where R installs binaries from.
	//
	// A dial rather than configuration because it is the one build parameter
	// whose wrong value is discovered from a deploy log by the person who cannot
	// fix it: P3M publishes trixie binaries for x86_64 only, so every arm64
	// install's R deploys fail until this names a different mirror, and the fix
	// is otherwise in a unit file on a box they may not have.
	//
	// **Deliberately unvalidated**, as TOFT_R_REPOS is. The rule it looks
	// like it breaks — a URL somebody typed is server-side input, refused at
	// save and at dial — governs a URL the platform then dials, and the
	// legitimate value here is routinely the private address safefetch exists to
	// refuse. Nothing dials it: Rscript is handed a repository option and no
	// socket, and the field's copy says so.
	SettingRPackageRepository = "r_package_repository"

	// SettingLogLevel is how much the platform says about itself.
	//
	// The one dial that takes effect **without waiting for anything to
	// restart**: obs.Init installs a *slog.LevelVar, so every slog call and
	// every log.Printf in the tree picks the new level up at the next line
	// written. It may, because it reaches no workload at all — it is the
	// platform's own verbosity, and raising it to diagnose a deploy used to mean
	// restarting the platform, which on a busy install ends the state being
	// diagnosed.
	//
	// The log *format* is not here and never will be: that is a fact about who
	// is reading the stream, decided when the install was stood up, and changing
	// it mid-flight hands a collector a format change it did not ask for.
	SettingLogLevel = "log_level"

	// SettingLockedMessage is what a locked user is told, in the
	// administrator's own words. The platform knows the account is locked; it
	// does not know why, and only the organization that locked it can say what
	// the person should do next — "you left the company" and "your access
	// review lapsed, ask your manager" want very different sentences.
	SettingLockedMessage = "locked_account_message"

	// SettingNoAccountMessage is what someone who authenticated successfully
	// but has no account is told, when the provider they used does not
	// auto-provision. Same reasoning: the platform can only say "no account",
	// while the admin knows which ticket queue creates one.
	SettingNoAccountMessage = "no_account_message"

	// SettingPendingActivationMessage is what an account created under a
	// provider's `pending` policy is told at the door, every time it signs in,
	// until an administrator activates it. Fourth of the family and the second
	// that refuses somebody who authenticated perfectly well — the difference
	// from SettingNoAccountMessage being that here the account *does* exist and
	// is in a queue, so the sentence has to say "wait" rather than "ask for
	// one".
	//
	// The default deliberately promises nothing about email: the activation
	// notice is a notification like every other and either switch may be off,
	// so an install that knows its relay works is the one that may add the
	// promise — see the copy-may-not-promise-mail rule.
	SettingPendingActivationMessage = "pending_activation_message"

	// SettingActivationMessage is the sentence the activation notice carries
	// when an administrator lets somebody in. It is not a refusal and so not
	// really of this family: the other four are read by somebody being turned
	// away, and this one is read by somebody who has just been admitted. It is
	// beside them because it is the same act — the organization saying, in its
	// own words, what has happened to your account here.
	SettingActivationMessage = "account_activated_message"

	// SettingAppLockedMessage is what a visitor to a locked app is told, when
	// the administrator who locked that particular app did not write something
	// more specific.
	//
	// The third of this family and the first that is not about a person: the two
	// above refuse somebody at the door, this one refuses them at an app they
	// are otherwise entitled to open. It is the install-wide default only — a
	// lock carries its own message, and ResolveLockMessage prefers it, because
	// "decommissioned, the data moved to Finance Hub" is a better sentence than
	// anything an install-wide default can say and only the person locking that
	// app knows it.
	SettingAppLockedMessage = "app_locked_message"

	// SettingEnvironmentTag names which environment this install is — "stage",
	// "prod" — as a label: telemetry reports it as the deployment environment.
	// It selects nothing about an app's variables; those are declared by name
	// in toft.toml [env] and sent per install (ADR §14). Empty, the default,
	// names none.
	SettingEnvironmentTag = "environment_tag"

	// SettingInstallName is what this install calls itself to the people its
	// messages reach: the sender in every mail frame, Slack post and card, in
	// place of "Toft". Empty, the default, means "Toft" — every message reads
	// exactly as it did before the setting existed. Read through
	// ResolveSenderName, never directly, and never sent to the vendor.
	SettingInstallName = "install_name"

	// SettingDisplayTimeZone is the zone every exact time in the SPA is shown
	// in for a person who has not chosen one (ADR §154). Empty, the default,
	// means each viewer's own browser zone — exactly what the product did
	// before the setting existed. Otherwise "UTC" or an IANA name, checked by
	// NormalizeTimeZone. Display only: nothing is stored or computed in it.
	SettingDisplayTimeZone = "display_time_zone"

	// SettingMaxVersions caps how many versions of one app may exist. It is
	// the administrator's half of release retention; the owner's half is
	// whether cleanup is automatic and how many versions they want (see
	// ResolveRetention).
	//
	// 0 means no cap, and is the default. An install that has been running for
	// a year has apps with long deployment histories, and a cap that arrived
	// switched on would block their next deploy with no warning — the same
	// reason idle sleep and isolated execution are off until someone says
	// otherwise.
	SettingMaxVersions = "max_versions_per_app"

	// SettingPublicBaseURL is the address links in outbound mail point at —
	// the one job of BASE_URL that cannot be derived from a request, because
	// no request is in flight when a notification is sent.
	//
	// It is a setting rather than deployment configuration for a reason worth
	// stating, since the comment at the top of this file says configuration
	// does not belong here. Everything BASE_URL does for *inbound* traffic —
	// which origins may call the API, where a sign-in lands — is answered per
	// request from the origin a trusted proxy declares, and must stay that way:
	// a stored value that gated inbound requests could lock every administrator
	// out of the page that edits it. What is left is outbound, where a wrong
	// value sends a bad link rather than closing the door, and where the person
	// who knows the right answer is the administrator reading the notification.
	//
	// Empty, the default, means fall back to the BASE_URL environment variable.
	// The environment therefore remains a complete way to configure an install,
	// and a stored value that turns out to be wrong is overridden by clearing
	// it rather than by a restart.
	SettingPublicBaseURL = "public_base_url"

	// SettingGarthURL is the address of the organization's Garth — the team
	// coding-agent product — which turns on an "Open in Garth" link on an app's
	// page for the people who may change the app (ADR §158).
	//
	// Empty, the default, means no link anywhere: nothing about an install
	// that does not run Garth mentions it. The value is not a secret — it is
	// printed in a link — and Toft never dials it; the link only navigates,
	// and Garth asks the person to confirm before it does anything.
	SettingGarthURL = "garth_url"

	// SettingIdleSleepMinutes is how long a shared-mode app may go untouched
	// before the platform stops its process and marks it `sleeping`; the next
	// request wakes it.
	//
	// 0 means off, and is the default. An upgrade must not start stopping
	// people's apps because a new setting appeared — "optional capabilities are
	// absent, not dormant" applies to this one as much as to isolated mode.
	SettingIdleSleepMinutes = "idle_sleep_minutes"

	// SettingIsolatedIdleSleepMinutes is the same timeout for isolated apps,
	// and a setting of its own because the trade is a different one (ADR
	// §16): a slept pod is deleted and its wake is a schedule, perhaps a pull,
	// and the app's own start, which can be minutes where a shared wake is
	// seconds — while what it gives back is a reservation the cluster holds
	// whether or not anybody visits. One number for both would make an
	// administrator choose between the two populations.
	//
	// 0 means off, and is the default, for the reason the shared one is.
	SettingIsolatedIdleSleepMinutes = "isolated_idle_sleep_minutes"

	// SettingIsolatedSleepWindowEnabled confines isolated idle sleep to a daily
	// window — overnight, typically — and wakes, one at a time, every isolated
	// app still asleep once the window closes, so a working day starts on warm
	// pods rather than on a wake per first visitor. Off, the timeout alone
	// decides, at any hour. Off by default; it does nothing while the isolated
	// timeout is 0.
	SettingIsolatedSleepWindowEnabled = "isolated_sleep_window_enabled"
	// SettingIsolatedSleepWindowStart and SettingIsolatedSleepWindowEnd are the
	// window as "HH:MM", read in the install's display time zone (UTC when it
	// has none — ADR §154's one zone). A window whose end is earlier than its
	// start crosses midnight, which is the ordinary case.
	SettingIsolatedSleepWindowStart = "isolated_sleep_window_start"
	SettingIsolatedSleepWindowEnd   = "isolated_sleep_window_end"

	// SettingAPIKeysEnabled is whether users may hold personal API keys —
	// long-lived credentials that authenticate their owner to the platform API
	// and to the apps gateway, for automation outside Toft.
	//
	// Default false, like every other optional capability: a fresh install and
	// a deliberately-disabled one behave identically, and an upgrade never
	// hands out a new credential class nobody asked for.
	//
	// Turning it off does not merely stop minting. Live keys stop
	// authenticating immediately, because an administrator reaching for this
	// switch during an incident means "the tokens stop working now", and a
	// switch that only prevented new ones would be read as doing something it
	// did not do.
	SettingAPIKeysEnabled = "api_keys_enabled"

	// SettingAPIKeyMaxLifetimeDays caps how long a new personal API key may
	// live, in days. 0 — the default — is no cap.
	//
	// What it bounds is the *new* key: one created by hand, or by `toft login`
	// and RStudio's Add account, which otherwise create keys that never expire
	// (ADR §102). Keys already held keep the expiry they were made with —
	// shortening a credential somebody is using is revoking it, and revocation
	// is a separate, visible act. Posit Connect's `APIKeyMaxLifetime` is the
	// same control with the same reading.
	SettingAPIKeyMaxLifetimeDays = "api_key_max_lifetime_days"

	// SettingPublicAppsEnabled is whether this install serves anybody who is
	// not signed in. See ADR §70.
	//
	// Default **true**, unlike the capability switches above it, and for the
	// reason the source policy and the uploads switch are permissive: this one
	// does not offer a capability, it takes one away. An install that never
	// opens the page behaves exactly as it did before the setting existed.
	//
	// Off does not merely stop the next app being published. Every app whose
	// owner already chose Public is handed to the authorizer as
	// `authenticated` from that moment — `authenticated` and never `private`,
	// because what is withdrawn is anonymity, and
	// nothing a colleague was granted goes with it.
	//
	// `apps.visibility` is not written, here or anywhere. The owner's sentence
	// stays the owner's, so turning the switch back on republishes exactly the
	// apps that were public before — there is no list to keep and nothing to
	// restore by hand.
	SettingPublicAppsEnabled = "public_apps_enabled"

	// SettingAudienceMeasurementEnabled is whether the apps gateway counts
	// distinct anonymous browsers (ADR §150): a 13-month
	// first-party cookie, toft_visitor, issued on an anonymous page load and
	// counted when it comes back, into one HyperLogLog sketch per app per day.
	//
	// Default **false**, and off is absent: no cookie, no sketch, and no
	// anonymous_browsers field on any response. It stores an identifier in a
	// visitor's browser, which some jurisdictions let an install do without
	// consent only for audience measurement kept first-party and never shared
	// — so the person deciding is the administrator who reads that note, not
	// the operator who installed the chart, and it is a setting, not an
	// environment variable. Page loads are not behind it: they are a unit of a
	// counter the install already keeps.
	SettingAudienceMeasurementEnabled = "audience_measurement_enabled"

	// SettingAppSecurityHeaders is whether the apps gateway fills
	// `X-Content-Type-Options: nosniff` and `Referrer-Policy` on an app's
	// response where the app sent neither header itself.
	//
	// Default **true**, upgrades included, and that is against SettingPublic
	// AppsEnabled's line: that switch takes an audience away, so its default
	// is what the platform did before it existed. This one takes nothing from
	// a viewer and nothing an app cannot put back with one header of its own,
	// because the fill only covers silence: an app that sends either header,
	// with any value, keeps what it sent. A baseline every scanner names,
	// shipped off, is one nobody turns on. The cost is an app that serves a
	// script under the wrong Content-Type, which nosniff makes the browser
	// refuse on the day of the upgrade; the release notes say so.
	//
	// HSTS is not behind it. That header is about the host, not about anybody's
	// code, and is sent wherever the install's address or the request is https
	// (internal/api/security_headers.go).
	SettingAppSecurityHeaders = "app_security_headers"

	// SettingAppLogCopyUncapped lifts the per-app cap on copying a shared
	// app's output to the platform's stream (internal/appout). Default
	// **false**.
	//
	// A switch that lifts a cap, against ADR §14's line that a page which can
	// set a dial to unlimited is a page from which one administrator takes
	// the box down. What stands of that: the cap's numbers are code, and no
	// page sets them. What is reversed: the cap can be lifted by name. The
	// outage that sentence fears is a row per line, or a ceiling nobody chose
	// to remove; uncapping writes no row and moves no workload, and its cost —
	// stdout bandwidth, the collector's bill, and one app's volume reaching
	// every app's latency — is stated beside the switch. The queue's bound
	// still drops and counts: no switch buys a copy that blocks the platform.
	SettingAppLogCopyUncapped = "app_log_copy_uncapped"

	// SettingAppsListableToAuthenticated puts every app in every signed-in
	// person's list without letting them open any of it — the `list` rung
	// (ADR §90) granted install-wide instead of row by row.
	//
	// Default **true**, and it is the only default in this file that turns
	// something *on* which discloses rather than off. The argument is that a
	// discovery feature nobody discovers is not a feature: shipped off, an
	// install upgrading behaves exactly as it did the day before, so nobody
	// encounters it, evaluates it or asks for it — and the access-request flow
	// it exists to feed cannot start, because somebody has to already know you
	// want an app before they can grant you the rung to ask for it.
	//
	// What it costs is stated rather than softened: on the first request after
	// an upgrade, every account on the install can enumerate every app's name,
	// slug, owner and framework, and nobody consented to that. Three things
	// reduce it and none removes it — the switch is one toggle away and takes
	// the whole install back immediately, an owner can withdraw one app with
	// apps.listable, and the disclosure is the listing's row and not one field
	// more.
	//
	// Read as "not false", so an install with no row at all reads as **on** —
	// which is the whole point, since the installs this is meant to reach are
	// exactly the ones that never opened the settings page. The SQL half
	// (appsListableToAuthenticatedExpr) reads the same way round, and the two
	// must agree or an app is listed and then 404s when opened.
	//
	// *Absent, not off* does not apply — this is an operating dial rather than a
	// capability seam, so the environment is its floor and it is never taken
	// away. What it changes is one clause in listableAppsPredicate; it writes no
	// grant row, so turning it off withdraws exactly what it granted and nothing
	// an owner wrote goes with it.
	SettingAppsListableToAuthenticated = "apps_listable_to_authenticated"

	// SettingDormantLockEnabled is whether the platform locks accounts nobody
	// has used for SettingDormantLockDays. See ADR §48.
	//
	// Default false, like every other optional capability, and here the default
	// is doing more work than usual: an upgrade that started locking people's
	// accounts because a new setting appeared would take the install's own
	// users out one by one, on a clock nobody set. A fresh install and a
	// deliberately-disabled one behave identically.
	//
	// What it never does is lock an administrator. That is not a setting and
	// there is no value of these two keys that changes it — see
	// dormancy.Sweeper, which reports them instead.
	SettingDormantLockEnabled = "dormant_lock_enabled"

	// SettingDormantLockDays is how long an account may go unused before the
	// sweep locks it.
	//
	// "Unused" is the dormancy clock, and it is deliberately not
	// `last_login_at`: a personal API key authenticates its owner without
	// minting a session, so an account driving a nightly CI job has signed in
	// exactly once and would look like the most dormant account on the install.
	// See UserActivity.
	SettingDormantLockDays = "dormant_lock_days"

	// SettingSessionAbsoluteHours is how long a browser session may live at
	// all, measured from the moment the person authenticated and unmoved by
	// how much they use it.
	//
	// A setting rather than an environment variable, because this is a decision
	// an administrator revisits rather than a fact about how the install was
	// stood up — the line this file already draws. It is also the point: a
	// control an organization cannot configure is a control it cannot assert,
	// and every security questionnaire asks to be told what this is set to
	// rather than what it was compiled as.
	//
	// There is a second reason, and on a running install it is the larger one.
	// The IdP cannot end a Toft session — no refresh token is stored, by
	// design — so somebody disabled in Okta at 09:00 keeps working here until
	// their session runs out. Until back-channel logout exists, shortening this
	// is the only lever an install has.
	SettingSessionAbsoluteHours = "session_absolute_hours"

	// SettingSessionIdleMinutes is how long a session may sit unused before it
	// ends. 0 is off, and is a real value rather than a gap: it collapses the
	// expiry rule to a flat lifetime from sign-in, which is exactly what this
	// platform did before either number existed.
	//
	// Nothing is written down to enforce it — the browser's own signed cookie
	// carries the clock and being used is what renews it. That is why this
	// needs no exemption from the rule forbidding a database write on the proxy
	// request path.
	SettingSessionIdleMinutes = "session_idle_minutes"

	// SettingGalleryMode is what the gallery offers: nothing, extensions only,
	// or extensions and examples.
	//
	// One setting with three values rather than two booleans, because two
	// booleans can express nonsense — examples on while the gallery is off —
	// and somebody would eventually store it.
	//
	// Default off, like every other optional capability, with one consequence
	// that is worth stating because it is the reason the default matters on an
	// on-premise product: while it is off the syncer makes **no outbound
	// request at all**. A fresh install does not phone home to GitHub, and an
	// administrator who never wants it to can simply leave this alone.
	//
	// What it does *not* do is restrict where apps may come from. Anyone who
	// may publish can still deploy any repository from New App; this curates
	// the catalog and nothing else, and the admin panel says so beside the
	// control.
	SettingGalleryMode = "gallery_deploy_mode"

	// SettingGitSourcePolicy is which git repositories this install will deploy
	// from at all: "any" (the default and today's behavior), "allowlist", or
	// "none". See internal/sources, and SetSourcePolicy for why this key is not
	// written by SetPlatformSettings.
	//
	// Unlike every other capability switch the default is *permissive*, and
	// deliberately so: this one does not offer a capability, it removes one. An
	// install that never touches it never notices the feature exists, which is
	// the only acceptable default for a control that can stop deploys.
	SettingGitSourcePolicy = "git_source_policy"

	// SettingSourceUploads is whether a ZIP may become an app. Default on, and
	// separate from the source policy on purpose: some installs legitimately
	// want upload-only (no internal git at all), which is the exact inverse of
	// what the allowlist is for, and one combined switch cannot express both.
	//
	// Separate is not independent. An install with git restricted and uploads
	// still on has a fence with a gate next to it, and the settings surface says
	// so in as many words.
	SettingSourceUploads = "source_uploads_enabled"

	// SettingNotebookRenderTimeout bounds how long a notebook's render may take.
	//
	// The one build command whose duration is a property of the *app's work*
	// rather than of package resolution: `uv pip install` takes as long as the
	// index takes, and a report takes as long as the query behind it. Every
	// other build command inherits a compiled-in ten minutes chosen for
	// `npm ci`, and inheriting it here would mean a legitimate twenty-minute
	// report could not exist on any install.
	//
	// Its copy has to name the queue. builds share a few slots (MAX_CONCURRENT_BUILDS,
	// half the CPUs up to four), so raising this raises how long one notebook can
	// hold a slot every other deploy is queued for.
	SettingNotebookRenderTimeout = "notebook_render_timeout_seconds"

	// SettingBuildTimeout bounds every *other* build command — the compiled-in
	// ten minutes, made an install's to set.
	//
	// Ten minutes was chosen for `npm ci` and held for every language after it,
	// which was fine while every language resolved packages at roughly the same
	// speed. R is where it stops being fine: an renv restore of a couple of
	// hundred packages is an ordinary R workload, and where the operator's
	// package repository serves no binary for one of them the install compiles
	// it — twenty to forty minutes for anything touching the tidyverse.
	//
	// The same copy warning applies as for the render above, and harder:
	// builds share a few slots, so raising this raises how long one deploy can
	// hold a slot every other one on the install is queued for. A per-app override
	// exists for exactly that reason — the app that needs twenty minutes is
	// usually one app, and raising the floor for the whole estate to
	// accommodate it is the change nobody wants.
	SettingBuildTimeout = "build_timeout_seconds"

	// The job timeout. The developer declares a job's timeout in toft.toml
	// (its memory and CPU are an administrator's: SettingJobMemoryLimitMB);
	// these are the install's default for a job that declares none, and the
	// ceiling a declaration may not pass. A declaration above the ceiling is
	// refused at check and deploy, never silently reduced — a developer must
	// never believe they have 45 minutes while getting 15.
	SettingDefaultJobTimeout = "default_job_timeout_seconds"
	SettingMaxJobTimeout     = "max_job_timeout_seconds"

	// SettingMaxQueuedRuns is how many of one app's runs may wait in the queue.
	// Past it a start is refused with queue_full: the one refusal a start gets
	// for being busy, since runs beyond the app's runs-at-once limit queue.
	SettingMaxQueuedRuns = "max_queued_runs_per_app"

	// SettingJobLogRetentionDays is how long a background job's log lines are
	// kept, and SettingJobRunRetentionDays how long its run rows are. Rows must
	// outlive logs: a run's cost and outcome stay interesting long after its
	// output does, and a run's *output object* is on the row clock for the same
	// reason. The inversion is refused rather than clamped, because an
	// administrator who typed it meant something the system cannot do.
	SettingJobLogRetentionDays = "job_log_retention_days"
	SettingJobRunRetentionDays = "job_run_retention_days"

	// SettingAppEventRetentionDays is how long app failure events are kept —
	// the `app_events` half of Admin → Activity, and every app's own Logs tab.
	//
	// It governs one of the two tables that feed that page and deliberately not
	// the other. `audit_log` is who *did* something and has a dial of its own,
	// SettingAccessRecordRetentionDays; these are machine observations that
	// repeat, and a chatty app can write thousands of them. The copy beside
	// each has to say which record it governs — an administrator who shortens
	// this and believes they have shortened the audit trail has been misled
	// about the record their install keeps.
	SettingAppEventRetentionDays = "app_event_retention_days"

	// SettingAccessRecordRetentionDays is how long the install can say who did
	// what and who opened which app: audit_log, app_usage_daily and
	// api_key_usage_daily, cut by one sweep on one cutoff. ADR §46 asked for
	// this to be answered once, so it is one dial over the three — an
	// administrator who shortens one record believing they shortened all of
	// them has been misled, the failure the dial above names.
	//
	// **0 keeps everything, and is the default.** Sweeps run at boot, so the
	// default is the upgrade's decision: 365 would delete every audit row older
	// than a year on every install at its first restart. Otherwise 365–3650,
	// refused below the floor rather than clamped — a year is the common
	// requirement, and the "active this year" figure reads a year of the
	// rollup. A stored value that will not parse or is out of range reads as 0:
	// a corrupt row must fail toward keeping, since the other direction is a
	// delete nobody asked for.
	SettingAccessRecordRetentionDays = "access_record_retention_days"

	// SettingPlatformLogRetentionDays is how long the platform's own account of
	// each app is kept — what it fetched, resolved, repaired, refused and
	// stopped retrying, on that app's Logs tab.
	//
	// Its own clock rather than a share of the one above, because ninety days
	// of every start's stage marks is a different quantity from ninety days of
	// failures, and an administrator who shortens one has not asked for the
	// other.
	//
	// **Its default is the event's, and that is the interesting part.** An
	// app_event's detail is what points at this log — "Check the app's logs for
	// what it printed on its way out" — so a default shorter than the event's
	// would rebuild the original bug on a delay: the pointer outliving its
	// destination, again, just later. `job_log_retention_days` is the wrong
	// neighbour to copy; a job's log is its deliverable and this is narration.
	SettingPlatformLogRetentionDays = "platform_log_retention_days"

	// SettingAnnouncementRecipientRetentionDays is how long the per-person rows
	// under a sent announcement are kept.
	//
	// **The announcement row and its five counts are never pruned, and that is
	// the whole point of this being a separate clock.** Two facts with
	// different lifetimes: "on the 3rd of March this install told 214 people the
	// queue was down, and 12 sends failed" is the install's own record and there
	// is no date on which it stops being worth having. "We mailed
	// bob@example.com on the 3rd of March" is a named person's data with a
	// shorter honest answer — the same distinction the app send ledger already
	// draws about recipients being third-party personal data with their own
	// retention answer.
	//
	// The copy beside it has to say which half goes, because an administrator
	// who shortens this and believes they have shortened the record of what
	// their install has announced has been misled.
	SettingAnnouncementRecipientRetentionDays = "announcement_recipient_retention_days"

	// SettingReportDeliveryMinIntervalMinutes is the floor between two
	// deliveries of the same report.
	//
	// **A cron accepts `* * * * *`.** Nothing cares today, because a re-render
	// that runs too often is only a build queue's problem and the build queue is
	// bounded — but with a recipient list attached the same expression is a mail
	// a minute to fifteen people, through somebody's corporate relay.
	//
	// Nobody chooses that deliberately; it arrives as a mistyped cron and is
	// discovered by the recipients. So the send and the render are two decisions
	// on one row, and the second gets a floor the first does not need: a
	// schedule that renders more often than this still renders, and mails at
	// this rate.
	//
	// Enforced at the **send** rather than at the save. A save-time refusal
	// would make the cron field mean different things depending on whether a
	// list is attached, and a list added to an existing minutely schedule would
	// be refused at exactly the moment nobody expects an argument. A suppressed
	// send is a recorded skip, never a silence.
	SettingReportDeliveryMinIntervalMinutes = "report_delivery_min_interval_minutes"

	// The dependency policy: which third-party packages this install will run.
	// See internal/deps and internal/advisory, and ADR §39.
	//
	// Unlike the source policy beside it, the default is **off** rather than
	// permissive, and the difference is the direction each one points. The
	// source policy removes a capability, so an install that never touches it
	// should never notice it. This one *adds* one — and adds one with a
	// standing outbound data dependency, which is never something an install
	// should acquire by upgrading.
	SettingDependencyPolicy = "dependency_policy"

	// SettingDependencyScanOnDeploy is whether a build resolves and records
	// its dependencies at all. Off is a coherent position rather than
	// laziness: resolution costs a build step, and Python sdist resolution can
	// execute setup.py from a package that has not yet been judged. An install
	// that will not have package code run on its build host to find out
	// whether package code is safe has made a defensible trade.
	SettingDependencyScanOnDeploy = "dependency_scan_on_deploy"

	// SettingDependencyWatchEnabled is the sweep over serving releases. Off
	// means no goroutine and no re-evaluation — on-demand re-check keeps
	// working, which is the point: the question stays answerable, just only
	// when somebody asks.
	SettingDependencyWatchEnabled = "dependency_watch_enabled"

	// SettingDependencyBlockMalware blocks a malicious package regardless of
	// any severity threshold. **Malware is not a severity** — there is no
	// low-severity credential stealer, and one threshold cannot express both
	// questions.
	SettingDependencyBlockMalware = "dependency_block_malware"

	// SettingDependencyBlockSeverity is the vulnerability threshold: one of
	// advisory's four levels, or "off" for no severity blocking at all.
	SettingDependencyBlockSeverity = "dependency_block_severity"

	// SettingDependencyBlockOnlyFixable narrows the vulnerability half to
	// advisories that name a fixed version. Default on, because an unfixable
	// transitive CVE that blocks a deploy gives the developer nothing to do,
	// and a control whose only available response is a waiver trains everyone
	// to waive. Unfixable findings are still recorded, shown and counted.
	SettingDependencyBlockOnlyFixable = "dependency_block_only_fixable"

	// SettingDependencyFeedEndpoint is where snapshots come from. Audited **in
	// the clear**: "who re-pointed this install's advisory feed, and where to"
	// is precisely what that log exists to answer. The mirror credential, by
	// contrast, is its own encrypted document.
	SettingDependencyFeedEndpoint = "dependency_feed_endpoint"

	SettingDependencyFeedIntervalHours = "dependency_feed_interval_hours"

	// SettingDependencyFeedMaxAgeHours is when a corpus is called stale. It
	// warns and never blocks — deliberately the opposite of the source
	// policy's fail-closed, because the difference is where the data lives. A
	// source list is this install's own; a feed is a document on somebody
	// else's infrastructure, and an outage there must not become "nobody at
	// this company can deploy anything".
	SettingDependencyFeedMaxAgeHours = "dependency_feed_max_age_hours"

	// SettingDependencyWaiverMaxDays caps how long an accepted risk may run.
	// 0 disables waivers entirely, which changes the refusal copy: offering a
	// waiver sends a developer to an administrator who has no button.
	SettingDependencyWaiverMaxDays = "dependency_waiver_max_days"

	// SettingDependencyScanIntervalHours is how often the watch re-judges every
	// serving release. Its own dial rather than the feed's, because the two
	// cost different things: the feed interval governs an outbound fetch of
	// tens of megabytes, and a sweep is a local join. No cross-field refusal —
	// a sweep more frequent than the feed still finds things, because a
	// redeploy changes the serving release under a corpus that has not moved.
	SettingDependencyScanIntervalHours = "dependency_scan_interval_hours"

	// SettingDependencyScanHistoryKept is how many readings each app keeps.
	//
	// A **count**, not a number of days, and deliberately not
	// app_event_retention_days: an event is a problem that opened and closed, a
	// scan is an observation, and one dial for both would make "keep six months
	// of scans" depend on how often the install sweeps. An install sweeping
	// hourly and one sweeping weekly want the same number of readings.
	SettingDependencyScanHistoryKept = "dependency_scan_history_kept"

	// SettingDependencyLockOn is the threshold at which the platform locks a
	// serving app by itself: off, malware, critical or high.
	//
	// **Not SettingDependencyBlockSeverity**, and the difference is the only
	// axis that matters. That threshold decides what refuses a *deploy* — a
	// person is present, waiting, holding the code, able to bump a version in
	// the next minute. This one decides what takes a *running app off the air*,
	// with nobody present, possibly at 3am, possibly while somebody is using
	// it. An install that set `block_severity: high` for its deploy gate did
	// not thereby ask for its estate to go dark.
	//
	// `malware` by default, and the only value the copy recommends. See
	// deps.Policy.LocksOn and ADR §39 D4 for why that one reverses *a serving
	// app is never stopped* and the other two do not get to.
	SettingDependencyLockOn = "dependency_lock_on"

	// The four fields of /.well-known/security.txt (RFC 9116) — how somebody
	// who has found a problem in an app on this install reaches the people who
	// can do something about it.
	//
	// **Absent, not empty.** With no contact configured the path is a 404 on
	// both origins, not a file with blank fields: an expired or contentless
	// security.txt is worse than none, because it is a published promise that
	// nobody is reading. The contact is what turns the file on; nothing else
	// here does.
	SettingSecurityTxtContact = "security_txt_contact"
	// SettingSecurityTxtExpires is a date, `YYYY-MM-DD`, and the RFC requires
	// it. It is also the point of the whole feature: the failure mode of this
	// file is that it is correct for a year and then names somebody who left,
	// so the field an administrator has to fill in is the one that goes stale
	// visibly. Served as the end of that day in UTC — "expires on the 4th"
	// means the 4th is still good.
	SettingSecurityTxtExpires = "security_txt_expires"
	// The two optional fields. A policy URL, and the languages whoever reads
	// the mailbox can read a report in.
	SettingSecurityTxtPolicy    = "security_txt_policy"
	SettingSecurityTxtLanguages = "security_txt_languages"
)

// The three values SettingDependencyPolicy takes.
//
// Three rather than two because `warn` is what makes `block` adoptable: an
// administrator switching this on for the first time across forty apps has no
// idea what it breaks. `warn` resolves, records and shows everything and fails
// no build, and it is the honest recommendation in the copy beside the switch.
// An empty middle state is ambiguous, so the middle state gets a name — ADR
// §29 D1's shape, for §29 D1's reason.
const (
	DependencyPolicyOff   = "off"
	DependencyPolicyWarn  = "warn"
	DependencyPolicyBlock = "block"
)

// ValidDependencyPolicy reports whether a value is one of the three.
func ValidDependencyPolicy(mode string) bool {
	switch mode {
	case DependencyPolicyOff, DependencyPolicyWarn, DependencyPolicyBlock:
		return true
	}
	return false
}

// DependencySeverityOff is the threshold value that turns severity blocking
// off without turning the feature off. Distinct from an empty string, which is
// what an *advisory* with no rating carries — the two must not be one value.
const DependencySeverityOff = "off"

// ValidDependencySeverityThreshold reports whether a value is a settable
// threshold: one of the four named levels, or "off".
func ValidDependencySeverityThreshold(s string) bool {
	return s == DependencySeverityOff || advisory.ValidSeverity(s)
}

// The four values SettingDependencyLockOn takes.
//
// Four rather than the severity threshold's five, and the missing one is the
// point: there is no `moderate` and no `low`. A moderate transitive CVE with no
// fix is on nearly every Node app, and a value that would take an estate off
// the air over one is not an option an administrator should be able to pick by
// accident. Malware is first because it is the only one the copy recommends.
const (
	DependencyLockOff      = "off"
	DependencyLockMalware  = "malware"
	DependencyLockCritical = advisory.SeverityCritical
	DependencyLockHigh     = advisory.SeverityHigh
)

// ValidDependencyLockOn reports whether a value is one of the four.
func ValidDependencyLockOn(s string) bool {
	switch s {
	case DependencyLockOff, DependencyLockMalware, DependencyLockCritical, DependencyLockHigh:
		return true
	}
	return false
}

// Bounds on the numeric settings above.
//
// Here rather than beside the handler that validates them, because the handler
// is not the only reader: GetPlatformSettings clamps a corrupt row against the
// same figures, and two copies of a range is how a stored value comes to sit
// outside the range the form will accept.
const (
	// A minute is below the point where a render is really a syntax check; an
	// hour is where a build queue that defaults to one worker is the binding
	// constraint and the answer is to raise MAX_CONCURRENT_BUILDS first.
	MinNotebookRenderTimeoutSeconds = 60
	MaxNotebookRenderTimeoutSeconds = 3600

	// A minute is below the point where `npm ci` on a cold cache fails for
	// everyone; an hour is where a build is really a job and the answer is to
	// vendor or pre-build rather than to wait. The same bounds as the render
	// above, because it is the same trade against the same queue.
	MinBuildTimeoutSeconds = 60
	MaxBuildTimeoutSeconds = 3600

	// Ten is past the point where retrying is the answer: an automation that
	// needs eleven attempts is not having a bad minute, it is broken, and the
	// platform spending a morning discovering that is worse than a failed run
	// somebody reads.
	// Under an hour is polling a third party's storage bucket for a document
	// that changes a few times a day; a week is past the point where the age
	// on every finding is the more honest thing to read than the finding.
	MinDependencyFeedIntervalHours = 1
	MaxDependencyFeedIntervalHours = 168

	// Six hours is the shortest staleness warning that a six-hourly refresh
	// can clear; a year is a corpus nobody should be deciding against.
	MinDependencyFeedMaxAgeHours = 6
	MaxDependencyFeedMaxAgeHours = 8760

	// 0 disables waivers entirely and is a real value, not "unset". The
	// ceiling is a year, which is already past the point where "the next
	// deploy is the review" has stopped being true of anything.
	MinDependencyWaiverMaxDays = 0
	MaxDependencyWaiverMaxDays = 365

	// An hour is the shortest interval at which re-deriving is telling you
	// something a redeploy did not; a week is where the age on the reading is
	// the more honest thing to read than the reading. The same range as the
	// feed's, because it is bounded by the same two arguments — and a separate
	// dial because a sweep is a local join and a feed refresh is not.
	MinDependencyScanIntervalHours = 1
	MaxDependencyScanIntervalHours = 168

	// One reading is a history that answers "when did we last look" and
	// nothing else, which is a coherent position for an install that only
	// wants the latest. A year of daily readings is past the point where a list
	// is the right tool. Zero is not offered: a history of nothing is the
	// absence of the feature, and the feature's switch is the watch.
	MinDependencyScanHistoryKept = 1
	MaxDependencyScanHistoryKept = 365

	// A minute is below the point where a job is anything but a request; a
	// week is past the point where a run is a job rather than a service.
	MinJobTimeoutSeconds = 60
	MaxJobTimeoutSeconds = 7 * 24 * 3600

	MinQueuedRunsPerApp = 1
	MaxQueuedRunsPerApp = 10000

	MinJobLogRetentionDays = 1
	MaxJobLogRetentionDays = 365
	MinJobRunRetentionDays = 1
	MaxJobRunRetentionDays = 3650

	// A week is the floor rather than a day, because this table is what answers
	// "did this app fail over the weekend" and a figure shorter than a weekend
	// cannot. Ten years is the same ceiling the job rows have: both are the row
	// that explains an incident, and neither is the bulky half of anything.
	MinAppEventRetentionDays = 7
	MaxAppEventRetentionDays = 3650

	// The same floor and ceiling as the events beside them, and for the reason
	// the default matches: the event is the thing that points at this log, so
	// an install cannot be allowed to keep the pointer for a week longer than
	// what it points at without saying so on purpose.
	MinPlatformLogRetentionDays = 7
	MaxPlatformLogRetentionDays = 3650

	// A month is the floor. Below that the question this table exists to answer
	// — "was Bob told, and if not, why not" — stops having an answer within the
	// window in which anybody asks it. Ten years is the same ceiling every
	// other row-level retention here has.
	// The floor's own bounds. Zero is permitted and means no floor at all — an
	// install that has decided its relay can take it, stated rather than
	// smuggled in as a very small number.
	MinReportDeliveryMinIntervalMinutes = 0
	MaxReportDeliveryMinIntervalMinutes = 1440

	MinAnnouncementRecipientRetentionDays = 30
	MaxAnnouncementRecipientRetentionDays = 3650

	// The access record's bounds, when it is not 0 (keep everything).
	MinAccessRecordRetentionDays = 365
	MaxAccessRecordRetentionDays = 3650
)

// The bounds on the dormancy threshold.
//
// The floor is thirty days and it is not arbitrary: below a month this control
// locks people who took a long holiday, and an account locked for going on
// leave teaches everybody that the lock means nothing. Thirty is already
// shorter than the forty-five CIS asks for, so an install that wants to be
// strict can be, without being able to set a figure that is simply wrong.
//
// The ceiling is two years, past which the setting is indistinguishable from
// off and off is the honest way to say it.
const (
	MinDormantLockDays = 30
	MaxDormantLockDays = 730
)

// MaxAPIKeyMaxLifetimeDays is the longest cap SettingAPIKeyMaxLifetimeDays
// takes: ten years, the longest expiry a key can be created with at all.
const MaxAPIKeyMaxLifetimeDays = 3650

// Defaults for the two refusal messages. Deliberately plain and actionable:
// they are read by someone who has just been refused, and a default that says
// nothing useful is worse than one an admin never gets around to editing.
const (
	DefaultLockedMessage = "Your account has been locked and cannot be used. " +
		"Contact an administrator if you think this is a mistake."
	DefaultNoAccountMessage = "You signed in successfully, but you do not have an account on this " +
		"platform and accounts are not created automatically. Ask an administrator to set one up for you."
	// The account exists; nobody has let it in yet. It says "wait", where the
	// one above says "ask" — and it promises no email, because the activation
	// notice answers to the same switches every other notification does.
	DefaultPendingActivationMessage = "Your account has been created and is waiting for an " +
		"administrator to activate it. You will be able to sign in once it has been."
	// The one message in this family somebody is glad to read.
	DefaultActivationMessage = "Your account has been activated. You can sign in now."
)

// PlatformSettings is the capability document, read as one so a caller never
// makes a decision on a half-loaded view.
type PlatformSettings struct {
	IsolatedEnabled bool `json:"isolated_execution_enabled"`

	// DefaultExecutionMode seeds a new app's mode. Never read for an app that
	// already exists — see SettingDefaultExecutionMode.
	DefaultExecutionMode string `json:"default_execution_mode"`

	JobsEnabled    bool `json:"background_jobs_enabled"`
	JobConcurrency int  `json:"job_concurrency_per_app"`

	// AppMemoryLimitMB is the administrator's override of the install's memory
	// ceiling. 0 is "not set" and means the operator's number stands — see
	// SettingAppMemoryLimitMB.
	//
	// **Stored and resolved in mebibytes, carried on the wire in gigabytes.**
	// MiB is the lossless meeting point of the two backends — the agent takes
	// integer MiB, Kubernetes takes a binary quantity — and storing a float
	// would be storing a value that gets rounded at every read by whichever
	// caller rounds first.
	// So the row keeps its key and its integer megabytes, and the API converts
	// once at the handler: in on PUT, out on GET. Absent from the JSON for that
	// reason — a document carrying the same number in two units is a document
	// with two answers.
	AppMemoryLimitMB int `json:"-"`

	// The rest of the operating dials. Every one carries the same rule as the
	// ceiling above: 0, or empty, is "not set" and means the operator's value.
	// The two CPU figures are absent from the JSON for the reason the memory
	// ceiling is — the wire offers them in two units and the row holds one.
	AppCPURequestMillicores int    `json:"-"`
	AppCPULimitMillicores   int    `json:"-"`
	JobMemoryLimitMB        int    `json:"-"`
	JobCPULimitMillicores   int    `json:"-"`
	MaxConcurrentBuilds     int    `json:"max_concurrent_builds"`
	AutoDeployPollSeconds   int    `json:"auto_deploy_poll_seconds"`
	RPackageRepository      string `json:"r_package_repository"`
	LogLevel                string `json:"log_level"`

	// The refusal messages. Never empty as read: an unset row, or one an admin
	// cleared, falls back to the default, because the one thing a refused
	// person must not get is a blank explanation.
	LockedMessage    string `json:"locked_account_message"`
	NoAccountMessage string `json:"no_account_message"`
	// PendingActivationMessage is the fourth refusal — an account that exists
	// and has not been activated. ActivationMessage is its counterpart and not
	// a refusal at all: it is what the activation notice says once somebody
	// lets that account in. Both follow the never-empty-as-read rule above.
	PendingActivationMessage string `json:"pending_activation_message"`
	ActivationMessage        string `json:"account_activated_message"`
	// AppLockedMessage is the third of the family and the first that refuses
	// somebody at an app rather than at the door. Same rule: never empty as
	// read. A lock on a particular app may override it — see ResolveLockMessage,
	// which is the only thing that should combine the two.
	AppLockedMessage string `json:"app_locked_message"`

	// EnvironmentTag is empty when the install names no environment — unlike
	// the messages, empty is a meaningful stored value, not a gap to fill.
	EnvironmentTag string `json:"environment_tag"`

	// InstallName is empty when no administrator has named the install, and
	// empty is meaningful: it reads as "Toft". See SettingInstallName.
	InstallName string `json:"install_name"`

	// DisplayTimeZone is empty when times follow each viewer's browser. See
	// SettingDisplayTimeZone.
	DisplayTimeZone string `json:"display_time_zone"`

	// PublicBaseURL is empty when no administrator has set one and the
	// BASE_URL environment variable stands. See SettingPublicBaseURL.
	PublicBaseURL string `json:"public_base_url"`

	// GarthURL is empty when no administrator has set one, and then no app
	// page offers "Open in Garth". See SettingGarthURL.
	GarthURL string `json:"garth_url"`

	// IdleSleepMinutes is 0 when idle sleep is off. See SettingIdleSleepMinutes.
	IdleSleepMinutes int `json:"idle_sleep_minutes"`

	// IsolatedIdleSleepMinutes is 0 when isolated apps never sleep. See
	// SettingIsolatedIdleSleepMinutes.
	IsolatedIdleSleepMinutes int `json:"isolated_idle_sleep_minutes"`

	// The isolated sleep window. See SettingIsolatedSleepWindowEnabled.
	IsolatedSleepWindowEnabled bool   `json:"isolated_sleep_window_enabled"`
	IsolatedSleepWindowStart   string `json:"isolated_sleep_window_start"`
	IsolatedSleepWindowEnd     string `json:"isolated_sleep_window_end"`

	// MaxVersions is 0 when there is no cap. See SettingMaxVersions.
	MaxVersions int `json:"max_versions_per_app"`

	// APIKeysEnabled offers personal API keys. Off means absent: the routes
	// 404, nothing appears in the UI, and existing keys stop authenticating.
	APIKeysEnabled bool `json:"api_keys_enabled"`

	// APIKeyMaxLifetimeDays is 0 when there is no cap. See
	// SettingAPIKeyMaxLifetimeDays.
	APIKeyMaxLifetimeDays int `json:"api_key_max_lifetime_days"`

	// PublicAppsEnabled is whether an app may be reachable without signing in.
	// Off withdraws anonymity from every app that already has it and refuses
	// the transition to `public`; it writes no app's column. See
	// SettingPublicAppsEnabled.
	PublicAppsEnabled bool `json:"public_apps_enabled"`

	// AudienceMeasurementEnabled is whether anonymous browsers are counted.
	// See SettingAudienceMeasurementEnabled.
	AudienceMeasurementEnabled bool `json:"audience_measurement_enabled"`

	// AppSecurityHeaders is whether the gateway fills nosniff and
	// Referrer-Policy where an app sent neither. See SettingAppSecurityHeaders.
	AppSecurityHeaders bool `json:"app_security_headers"`

	// AppLogCopyUncapped lifts the per-app cap on the output copy. See
	// SettingAppLogCopyUncapped.
	AppLogCopyUncapped bool `json:"app_log_copy_uncapped"`

	// AppsListableToAuthenticated puts every app in every signed-in person's
	// list without letting them open any of it. See
	// SettingAppsListableToAuthenticated.
	AppsListableToAuthenticated bool `json:"apps_listable_to_authenticated"`

	// Dormancy locking. DormantLockDays only means anything while
	// DormantLockEnabled is on, and the surfaces say so rather than showing a
	// threshold for a sweep that is not running. See ADR §48.
	DormantLockEnabled bool `json:"dormant_lock_enabled"`
	DormantLockDays    int  `json:"dormant_lock_days"`

	// The session policy: two numbers, never one. An absolute lifetime and an
	// idle timeout answer different questions and every standard names them
	// separately, so a single combined "session timeout" would have to pick
	// which one it meant. Unlike the dormancy pair there is no switch above
	// them — a session always has a lifetime; the idle number's own `0` is
	// what turns that half off. Read as a policy through SessionPolicy().
	SessionAbsoluteHours int `json:"session_absolute_hours"`
	SessionIdleMinutes   int `json:"session_idle_minutes"`

	// GalleryMode is one of GalleryOff, GalleryExtensionsOnly, GalleryAll.
	// See SettingGalleryMode.
	GalleryMode string `json:"gallery_deploy_mode"`

	// The two source switches. Read here so that one settings read answers
	// every question about the install; *written* only through SetSourcePolicy,
	// which audits the pattern list as a diff — see the note on that method.
	GitSourcePolicy      string `json:"git_source_policy"`
	SourceUploadsEnabled bool   `json:"source_uploads_enabled"`

	// The limits. Unlike the capability switches above, none of these is ever
	// off: they are limits with meaningful defaults, and the question they
	// answer is "what number", not "whether".
	DefaultJobTimeoutSecs     int `json:"default_job_timeout_seconds"`
	MaxJobTimeoutSecs         int `json:"max_job_timeout_seconds"`
	MaxQueuedRunsPerApp       int `json:"max_queued_runs_per_app"`
	NotebookRenderTimeoutSecs int `json:"notebook_render_timeout_seconds"`
	BuildTimeoutSecs          int `json:"build_timeout_seconds"`
	JobLogRetentionDays       int `json:"job_log_retention_days"`
	JobRunRetentionDays       int `json:"job_run_retention_days"`

	// AppEventRetentionDays is the failure-event sweep's clock. It sits with
	// the caps above rather than with a capability switch for the same reason
	// they do: the question is "what number", never "whether" — there is no
	// value here that turns the record off.
	AppEventRetentionDays int `json:"app_event_retention_days"`

	// PlatformLogRetentionDays is the platform log sweep's clock. Beside the
	// events' rather than the job log's: this is narration about an app, not
	// an app's own output, and it has to outlive the events that point at it.
	PlatformLogRetentionDays int `json:"platform_log_retention_days"`

	// AnnouncementRecipientRetentionDays is the announcement recipient sweep's
	// clock. It prunes the per-person rows and never the announcement or its
	// counts — see SettingAnnouncementRecipientRetentionDays.
	AnnouncementRecipientRetentionDays int `json:"announcement_recipient_retention_days"`

	// AccessRecordRetentionDays is 0 to keep everything, else 365–3650. See
	// SettingAccessRecordRetentionDays.
	AccessRecordRetentionDays int `json:"access_record_retention_days"`
	// ReportDeliveryMinIntervalMinutes is the floor between two deliveries of
	// one report — see SettingReportDeliveryMinIntervalMinutes.
	ReportDeliveryMinIntervalMinutes int `json:"report_delivery_min_interval_minutes"`

	// The dependency policy. DependencyPolicy is off/warn/block; everything
	// under it only means anything while it is not off, and the surfaces say
	// so rather than rendering ten controls for a feature nobody enabled.
	DependencyPolicy           string `json:"dependency_policy"`
	DependencyScanOnDeploy     bool   `json:"dependency_scan_on_deploy"`
	DependencyWatchEnabled     bool   `json:"dependency_watch_enabled"`
	DependencyBlockMalware     bool   `json:"dependency_block_malware"`
	DependencyBlockSeverity    string `json:"dependency_block_severity"`
	DependencyBlockOnlyFixable bool   `json:"dependency_block_only_fixable"`
	DependencyFeedEndpoint     string `json:"dependency_feed_endpoint"`
	DependencyFeedIntervalHrs  int    `json:"dependency_feed_interval_hours"`
	DependencyFeedMaxAgeHours  int    `json:"dependency_feed_max_age_hours"`
	DependencyWaiverMaxDays    int    `json:"dependency_waiver_max_days"`
	// The watch's own three dials: how often it looks, how many readings each
	// app keeps, and the one threshold at which looking has a consequence.
	DependencyScanIntervalHrs int    `json:"dependency_scan_interval_hours"`
	DependencyScanHistoryKept int    `json:"dependency_scan_history_kept"`
	DependencyLockOn          string `json:"dependency_lock_on"`

	// The security.txt fields. All four are empty on an install that has never
	// filled them in, and that is a complete state rather than a missing one:
	// the file is not served at all until there is a contact, which is the
	// only reading of "absent, not empty" that does not publish a promise
	// nobody is keeping.
	SecurityTxtContact   string `json:"security_txt_contact"`
	SecurityTxtExpires   string `json:"security_txt_expires"`
	SecurityTxtPolicy    string `json:"security_txt_policy"`
	SecurityTxtLanguages string `json:"security_txt_languages"`
}

// SecurityTxtConfigured reports whether this install publishes a security.txt.
//
// The contact alone decides it — one fact, read the same way by the handler
// that serves the file, the two routers that mount it and the settings page
// that says whether anything is published. An expiry with no contact publishes
// nothing, which is why this does not ask about it.
func (s PlatformSettings) SecurityTxtConfigured() bool {
	return strings.TrimSpace(s.SecurityTxtContact) != ""
}

// The durations the caps above express, in the unit their callers use.
// Methods rather than fields so there is one conversion rather than one per
// call site.

// IdleSleepMinutesFor is the idle timeout that governs a workload of the
// given mode — the reaper's threshold and the figure an app's own page prints,
// read through one function so the two cannot pick different settings. 0 is
// off.
func (s PlatformSettings) IdleSleepMinutesFor(isolated bool) int {
	if isolated {
		return s.IsolatedIdleSleepMinutes
	}
	return s.IdleSleepMinutes
}

func (s PlatformSettings) NotebookRenderTimeout() time.Duration {
	return time.Duration(s.NotebookRenderTimeoutSecs) * time.Second
}

// BuildTimeout is the install's default clock for one build command.
func (s PlatformSettings) BuildTimeout() time.Duration {
	return time.Duration(s.BuildTimeoutSecs) * time.Second
}

// DefaultJobTimeout and MaxJobTimeout are the job timeout settings as
// durations.
func (s PlatformSettings) DefaultJobTimeout() time.Duration {
	return time.Duration(s.DefaultJobTimeoutSecs) * time.Second
}

func (s PlatformSettings) MaxJobTimeout() time.Duration {
	return time.Duration(s.MaxJobTimeoutSecs) * time.Second
}

func (s PlatformSettings) JobLogRetention() time.Duration {
	return time.Duration(s.JobLogRetentionDays) * 24 * time.Hour
}

func (s PlatformSettings) JobRunRetention() time.Duration {
	return time.Duration(s.JobRunRetentionDays) * 24 * time.Hour
}

func (s PlatformSettings) AppEventRetention() time.Duration {
	return time.Duration(s.AppEventRetentionDays) * 24 * time.Hour
}

func (s PlatformSettings) PlatformLogRetention() time.Duration {
	return time.Duration(s.PlatformLogRetentionDays) * 24 * time.Hour
}

func (s PlatformSettings) AnnouncementRecipientRetention() time.Duration {
	return time.Duration(s.AnnouncementRecipientRetentionDays) * 24 * time.Hour
}

// SessionPolicy is the two session numbers in the form everything that
// enforces them wants. One conversion, in one place: the settings are hours and
// minutes because that is what an administrator types, and every enforcement
// point wants durations.
func (s PlatformSettings) SessionPolicy() auth.SessionPolicy {
	return auth.SessionPolicy{
		Absolute: time.Duration(s.SessionAbsoluteHours) * time.Hour,
		Idle:     time.Duration(s.SessionIdleMinutes) * time.Minute,
	}
}

// The three values SettingGalleryMode takes. Under GalleryExtensionsOnly the
// examples are not listed at all rather than listed-and-refused: a catalogue of
// things you may not have is an advertisement, and this is an internal tool.
const (
	GalleryOff            = "off"
	GalleryExtensionsOnly = "extensions_only"
	GalleryAll            = "all"
)

// ValidGalleryMode reports whether a value is one of the three.
func ValidGalleryMode(mode string) bool {
	switch mode {
	case GalleryOff, GalleryExtensionsOnly, GalleryAll:
		return true
	}
	return false
}

// DefaultPlatformSettings is what an install starts as.
//
// **Every capability off** is the rule and it holds for the capability seams: a
// fresh install and a deliberately-disabled one behave identically, which is
// what makes "absent means not offered" true at this layer too.
//
// Three kinds of setting are not capabilities and do not follow it, and each
// says so where it sits. The figures below are what the platform uses when
// nobody has said otherwise. The permissive block takes a capability *away*, so
// its defaults are what the platform did before it existed. And one setting —
// AppsListableToAuthenticated — confers something and is on anyway, which is
// the only entry here that discloses by default and the one that had to be
// argued rather than inherited.
func DefaultPlatformSettings() PlatformSettings {
	return PlatformSettings{
		IsolatedEnabled: false,
		// Shared, so an install that never touches this setting creates
		// exactly the apps it created before it existed.
		DefaultExecutionMode: string(runtime.ModeShared),

		JobsEnabled:              true,
		JobConcurrency:           3,
		LockedMessage:            DefaultLockedMessage,
		NoAccountMessage:         DefaultNoAccountMessage,
		PendingActivationMessage: DefaultPendingActivationMessage,
		ActivationMessage:        DefaultActivationMessage,
		AppLockedMessage:         DefaultAppLockedMessage,
		IdleSleepMinutes:         0,
		IsolatedIdleSleepMinutes: 0,

		IsolatedSleepWindowEnabled: false,
		IsolatedSleepWindowStart:   DefaultSleepWindowStart,
		IsolatedSleepWindowEnd:     DefaultSleepWindowEnd,
		MaxVersions:                0,
		APIKeysEnabled:             false,
		APIKeyMaxLifetimeDays:      0,
		GalleryMode:                GalleryOff,

		// Off, and ninety days if somebody switches it on — the figure a
		// reasonable install would have picked, and twice the forty-five CIS
		// asks for, because the first thing this control does on a real estate
		// is lock a batch of accounts at once and the safer place to start is
		// the loose end of the range.
		DormantLockEnabled: false,
		DormantLockDays:    90,

		// The check-in's usage block, on by default. The counts are what make
		// a support conversation about seats possible at all, nothing refuses
		// anything on one, and the install renders the exact payload beside the
		// switch — so the honest default is on and visible rather than off and
		// forgotten.

		// The session policy, and the defaults are opposite in character. The
		// absolute lifetime is unchanged from the constant it replaced, so no
		// install's sessions get longer or shorter on upgrade in the dimension
		// they already had — and eight hours is already stricter than AAL2's
		// twelve-hour ceiling, so there is no reason to relax toward the
		// standard.
		//
		// The idle timeout is the one default here that changes behaviour, and
		// it is deliberate: shipping the standard switched off means every
		// install is non-compliant until somebody finds the box. An active
		// person never reaches it — being used is what renews a session — and
		// the escape hatch is `0`, which is exactly today's behaviour.
		SessionAbsoluteHours: auth.DefaultSessionAbsoluteHours,
		SessionIdleMinutes:   auth.DefaultSessionIdleMinutes,

		// Permissive, unlike everything above. These two do not offer a
		// capability — they take one away — so their defaults are what the
		// platform did before they existed.
		GitSourcePolicy:      sources.ModeAny,
		SourceUploadsEnabled: true,
		// On, for the same reason, and it is the one default in this block
		// that decides who can reach an app: an upgrade that switched this off
		// would take every published app on the estate off the internet on a
		// morning nobody chose.
		PublicAppsEnabled: true,
		// On, and it takes nothing away from anybody: see
		// SettingAppSecurityHeaders for why an upgrade turns it on.
		AppSecurityHeaders: true,

		// On, and in neither block above. The three permissive defaults take a
		// capability away, which is what makes their sentence coherent; this one
		// confers something — every signed-in person sees that every app exists.
		// It is the one default here that discloses, and it is on because a
		// discovery feature that has to be discovered is a feature that does not
		// exist. See SettingAppsListableToAuthenticated for what that costs an
		// install on upgrade, which is stated there rather than softened.
		AppsListableToAuthenticated: true,

		// Not off, unlike everything above them. These are the figures the
		// platform uses when nobody has said otherwise, and each was chosen to
		// be the answer a reasonable install would have picked.
		DefaultJobTimeoutSecs: 3600,
		MaxJobTimeoutSecs:     86400,
		MaxQueuedRunsPerApp:   50,
		// Ten minutes, which is exactly what a render inherited before it had a
		// setting — so an install that never opens this page keeps the
		// behaviour it already had, and one with a slow report can say so.
		NotebookRenderTimeoutSecs: 600,
		// The compiled-in value this replaces, so an install that never touches
		// the setting behaves exactly as it did.
		BuildTimeoutSecs:    600,
		JobLogRetentionDays: 14,
		JobRunRetentionDays: 90,

		// Ninety days, which is what the sweep did as a constant before it was
		// a setting: long enough to answer "has this been happening all
		// quarter?", short enough that a chatty app cannot grow the table
		// without bound. An install that never opens this page keeps exactly
		// the behaviour it already had.
		AppEventRetentionDays: 90,
		// Ninety, matching the events above, because an event's detail is what
		// sends somebody here.
		PlatformLogRetentionDays: 90,

		// Six months. Long enough that "did we tell everyone about the
		// retention change" is still answerable person by person a couple of
		// quarters later, short enough that an install running a monthly
		// announcement to eight hundred people is not keeping five years of
		// named rows it will never read. The counts stay forever either way.
		AnnouncementRecipientRetentionDays: 180,
		// Fifteen minutes. The failure this exists to stop is a mistyped cron,
		// which is minutely, so anything above five fixes it — and fifteen
		// leaves an hourly dashboard untouched while making a per-minute list
		// impossible. Smaller is safer here in the sense that matters: it
		// refuses less.
		ReportDeliveryMinIntervalMinutes: 15,

		// Off, like every other optional capability, and here the default
		// carries one extra consequence worth stating: while it is off the
		// syncer makes **no outbound request at all**. A fresh install does
		// not contact osv.dev because nobody asked it to.
		//
		// The figures under it are what the feature would do *if* somebody
		// switched it on, and each is the answer a reasonable install would
		// have picked: scan and watch, block malware always, block critical
		// vulnerabilities that have a fix, refresh every six hours, call the
		// corpus stale after three days, cap an accepted risk at a quarter.
		DependencyPolicy:           DependencyPolicyOff,
		DependencyScanOnDeploy:     true,
		DependencyWatchEnabled:     true,
		DependencyBlockMalware:     true,
		DependencyBlockSeverity:    advisory.SeverityCritical,
		DependencyBlockOnlyFixable: true,
		DependencyFeedEndpoint:     advisory.DefaultEndpoint,
		DependencyFeedIntervalHrs:  6,
		DependencyFeedMaxAgeHours:  72,
		DependencyWaiverMaxDays:    90,
		// Six is the constant the sweep has always used, so no install's
		// behaviour changes on upgrade. Thirty readings is a week at six-hourly
		// and a month at daily — enough to answer "how long has this been
		// true" either way. And the lock is **malware**: the one class where
		// every hour of serving is the harm, and inert until an administrator
		// switches the policy on at all.
		DependencyScanIntervalHrs: 6,
		DependencyScanHistoryKept: 30,
		DependencyLockOn:          DependencyLockMalware,
	}
}

// clampSettings holds every numeric setting inside its declared range.
//
// Applied on both the read and the write path. On the write path it is
// belt-and-braces behind a handler that has already refused out-of-range
// values with a message; on the read path it is the only thing standing
// between a corrupt row and a scheduler that waits zero seconds for every app.
func clampSettings(s *PlatformSettings) {
	clamp := func(v *int, lo, hi, def int) {
		if *v < lo || *v > hi {
			*v = def
		}
	}
	if s.JobConcurrency < 1 {
		s.JobConcurrency = 1
	}
	// A negative row can only be corruption, and the safe reading of it is
	// "off" rather than "sleep everything immediately".
	if s.IdleSleepMinutes < 0 {
		s.IdleSleepMinutes = 0
	}
	if s.IsolatedIdleSleepMinutes < 0 {
		s.IsolatedIdleSleepMinutes = 0
	}
	// An unreadable bound reads as the default rather than as a window that
	// can never open or never close.
	if _, err := ParseClock(s.IsolatedSleepWindowStart); err != nil {
		s.IsolatedSleepWindowStart = DefaultSleepWindowStart
	}
	if _, err := ParseClock(s.IsolatedSleepWindowEnd); err != nil {
		s.IsolatedSleepWindowEnd = DefaultSleepWindowEnd
	}
	// Same reading for the same reason: corruption becomes "no cap" rather
	// than a cap of -1, which would block every deploy on the install.
	if s.MaxVersions < 0 {
		s.MaxVersions = 0
	}
	// And for the key cap: a corrupt row is "no cap" rather than a cap that
	// would refuse every key the install tries to create.
	if s.APIKeyMaxLifetimeDays < 0 || s.APIKeyMaxLifetimeDays > MaxAPIKeyMaxLifetimeDays {
		s.APIKeyMaxLifetimeDays = 0
	}

	// A row that is not one of the three can only be corruption or a
	// downgrade, and the safe reading of either is "the gallery is not
	// offered" rather than "offer everything".
	if !ValidGalleryMode(s.GalleryMode) {
		s.GalleryMode = GalleryOff
	}
	if mode, err := runtime.ParseMode(strings.TrimSpace(s.DefaultExecutionMode)); err != nil {
		s.DefaultExecutionMode = string(runtime.ModeShared)
	} else {
		// Normalized before the write, so "" and "shared" are one stored
		// string rather than two readings of one setting.
		s.DefaultExecutionMode = string(mode)
	}

	// A source policy this binary does not understand is read as the most
	// restrictive one, which is the opposite of the gallery rule above and is
	// the right way round for each. An unknown *gallery* mode read as "off"
	// offers nothing; an unknown *source* mode read as "any" would silently
	// remove a control somebody deliberately set. Failing closed here is loud —
	// deploys refuse, the admin panel shows the value — and one click fixes it;
	// failing open is silent and nobody ever finds out.
	if !sources.ValidMode(s.GitSourcePolicy) {
		s.GitSourcePolicy = sources.ModeNone
	}

	d := DefaultPlatformSettings()
	clamp(&s.MaxJobTimeoutSecs, MinJobTimeoutSeconds, MaxJobTimeoutSeconds, d.MaxJobTimeoutSecs)
	clamp(&s.DefaultJobTimeoutSecs, MinJobTimeoutSeconds, MaxJobTimeoutSeconds, d.DefaultJobTimeoutSecs)
	// A default above the ceiling would be a job refused for declaring nothing.
	if s.DefaultJobTimeoutSecs > s.MaxJobTimeoutSecs {
		s.DefaultJobTimeoutSecs = s.MaxJobTimeoutSecs
	}
	clamp(&s.MaxQueuedRunsPerApp, MinQueuedRunsPerApp, MaxQueuedRunsPerApp, d.MaxQueuedRunsPerApp)
	clamp(&s.NotebookRenderTimeoutSecs, MinNotebookRenderTimeoutSeconds, MaxNotebookRenderTimeoutSeconds, d.NotebookRenderTimeoutSecs)
	clamp(&s.BuildTimeoutSecs, MinBuildTimeoutSeconds, MaxBuildTimeoutSeconds, d.BuildTimeoutSecs)
	clamp(&s.JobLogRetentionDays, MinJobLogRetentionDays, MaxJobLogRetentionDays, d.JobLogRetentionDays)
	clamp(&s.JobRunRetentionDays, MinJobRunRetentionDays, MaxJobRunRetentionDays, d.JobRunRetentionDays)
	clamp(&s.AppEventRetentionDays, MinAppEventRetentionDays, MaxAppEventRetentionDays, d.AppEventRetentionDays)
	clamp(&s.PlatformLogRetentionDays, MinPlatformLogRetentionDays, MaxPlatformLogRetentionDays, d.PlatformLogRetentionDays)
	clamp(&s.ReportDeliveryMinIntervalMinutes, MinReportDeliveryMinIntervalMinutes,
		MaxReportDeliveryMinIntervalMinutes, 15)
	clamp(&s.AnnouncementRecipientRetentionDays, MinAnnouncementRecipientRetentionDays,
		MaxAnnouncementRecipientRetentionDays, d.AnnouncementRecipientRetentionDays)
	// A corrupt row here becomes ninety days rather than zero, and the reason
	// is worth stating: this is the one clamped figure whose out-of-range
	// reading would lock every account on the install at once. Zero is not a
	// meaningful value for it — "off" is DormantLockEnabled, and a threshold of
	// nothing is never what anybody meant.
	clamp(&s.DormantLockDays, MinDormantLockDays, MaxDormantLockDays, d.DormantLockDays)

	// The absolute lifetime cannot use the zero-means-off reading: there is no
	// value of it that turns sessions off, and a corrupt row read as zero would
	// refuse every session on the install at once.
	clamp(&s.SessionAbsoluteHours, auth.MinSessionAbsoluteHours,
		auth.MaxSessionAbsoluteHours, d.SessionAbsoluteHours)
	// The idle timeout can, and does: zero is a real value — the timeout off —
	// so this cannot use clamp, which reads a zero as "unset" and would quietly
	// restore the default over an administrator's deliberate choice.
	if s.SessionIdleMinutes != 0 &&
		(s.SessionIdleMinutes < auth.MinSessionIdleMinutes ||
			s.SessionIdleMinutes > auth.MaxSessionIdleMinutes) {
		s.SessionIdleMinutes = d.SessionIdleMinutes
	}
	// The access record's zero is "keep everything", and a value out of range
	// reads as that rather than as a default: this dial deletes, and a corrupt
	// row must fail toward keeping.
	if s.AccessRecordRetentionDays != 0 &&
		(s.AccessRecordRetentionDays < MinAccessRecordRetentionDays ||
			s.AccessRecordRetentionDays > MaxAccessRecordRetentionDays) {
		s.AccessRecordRetentionDays = 0
	}

	// Rows outlive logs. Two separately-valid figures can still be a pair the
	// system cannot honour, and the honest repair is to lift the rows rather
	// than to drop the logs — deleting a run's explanation early is the one
	// direction that loses information nobody can get back.
	if s.JobRunRetentionDays < s.JobLogRetentionDays {
		s.JobRunRetentionDays = s.JobLogRetentionDays
	}

	// A dependency policy this binary does not understand is read as **off**,
	// which is the opposite of the source policy two blocks up and is the
	// right way round for each. An unknown *source* mode read as "any" would
	// silently remove a control somebody set. An unknown *dependency* mode
	// read as "block" would refuse every deploy on the install over a corrupt
	// row — and this control's failure mode is a deploy that fails, so the
	// safe reading is the one that does not manufacture an outage. Failing
	// open here is visible: the admin page shows off, and one click fixes it.
	if !ValidDependencyPolicy(s.DependencyPolicy) {
		s.DependencyPolicy = DependencyPolicyOff
	}
	// A threshold this binary does not understand is read as the strictest
	// one. Unlike the mode, getting this wrong cannot take the install down —
	// the mode already decided whether anything blocks — so the safe reading
	// is the one that does not quietly widen what passes.
	if !ValidDependencySeverityThreshold(s.DependencyBlockSeverity) {
		s.DependencyBlockSeverity = advisory.SeverityCritical
	}
	if strings.TrimSpace(s.DependencyFeedEndpoint) == "" {
		s.DependencyFeedEndpoint = d.DependencyFeedEndpoint
	}
	clamp(&s.DependencyFeedIntervalHrs, MinDependencyFeedIntervalHours,
		MaxDependencyFeedIntervalHours, d.DependencyFeedIntervalHrs)
	clamp(&s.DependencyFeedMaxAgeHours, MinDependencyFeedMaxAgeHours,
		MaxDependencyFeedMaxAgeHours, d.DependencyFeedMaxAgeHours)
	// Zero is a real value here — waivers off — so this cannot use clamp,
	// which reads a zero as "unset" and would quietly restore the default.
	if s.DependencyWaiverMaxDays < MinDependencyWaiverMaxDays ||
		s.DependencyWaiverMaxDays > MaxDependencyWaiverMaxDays {
		s.DependencyWaiverMaxDays = d.DependencyWaiverMaxDays
	}
	clamp(&s.DependencyScanIntervalHrs, MinDependencyScanIntervalHours,
		MaxDependencyScanIntervalHours, d.DependencyScanIntervalHrs)
	clamp(&s.DependencyScanHistoryKept, MinDependencyScanHistoryKept,
		MaxDependencyScanHistoryKept, d.DependencyScanHistoryKept)
	// A lock threshold this binary does not understand is read as **off**, the
	// mode's direction and not the severity threshold's. Getting the block
	// threshold wrong cannot take the install down, so it fails to the
	// strictest value; getting this one wrong takes apps off the air with
	// nobody present, so a corrupt row must not be able to.
	if !ValidDependencyLockOn(s.DependencyLockOn) {
		s.DependencyLockOn = DependencyLockOff
	}
}

// platformSettingKeys is every key GetPlatformSettings reads. One list, so a
// setting added to the struct and the switch below cannot be left out of the
// query — where it would read as its default forever and an administrator's
// saved value would silently never apply.
func platformSettingKeys() []string {
	return []string{
		SettingIsolatedEnabled, SettingDefaultExecutionMode,
		SettingJobsEnabled, SettingJobConcurrency, SettingAppMemoryLimitMB,
		SettingAppCPURequestMillicores, SettingAppCPULimitMillicores,
		SettingJobMemoryLimitMB, SettingJobCPULimitMillicores,
		SettingMaxConcurrentBuilds, SettingAutoDeployPollSeconds,
		SettingRPackageRepository, SettingLogLevel,
		SettingLockedMessage, SettingNoAccountMessage, SettingAppLockedMessage,
		SettingPendingActivationMessage, SettingActivationMessage,
		SettingEnvironmentTag, SettingInstallName, SettingDisplayTimeZone,
		SettingIdleSleepMinutes, SettingIsolatedIdleSleepMinutes,
		SettingIsolatedSleepWindowEnabled, SettingIsolatedSleepWindowStart, SettingIsolatedSleepWindowEnd,
		SettingMaxVersions, SettingPublicBaseURL, SettingGarthURL,
		SettingAPIKeysEnabled, SettingAPIKeyMaxLifetimeDays, SettingPublicAppsEnabled,
		SettingAudienceMeasurementEnabled,
		SettingAppSecurityHeaders, SettingAppLogCopyUncapped,
		SettingAppsListableToAuthenticated, SettingGalleryMode,
		SettingDormantLockEnabled, SettingDormantLockDays,
		SettingSessionAbsoluteHours, SettingSessionIdleMinutes,
		SettingGitSourcePolicy, SettingSourceUploads,
		SettingDefaultJobTimeout, SettingMaxJobTimeout,
		SettingMaxQueuedRuns,
		SettingNotebookRenderTimeout,
		SettingBuildTimeout,
		SettingJobLogRetentionDays, SettingJobRunRetentionDays,
		SettingAppEventRetentionDays,
		SettingPlatformLogRetentionDays,
		SettingAnnouncementRecipientRetentionDays, SettingAccessRecordRetentionDays,
		SettingReportDeliveryMinIntervalMinutes,
		SettingDependencyPolicy, SettingDependencyScanOnDeploy,
		SettingDependencyWatchEnabled, SettingDependencyBlockMalware,
		SettingDependencyBlockSeverity, SettingDependencyBlockOnlyFixable,
		SettingDependencyFeedEndpoint, SettingDependencyFeedIntervalHours,
		SettingDependencyFeedMaxAgeHours, SettingDependencyWaiverMaxDays,
		SettingDependencyScanIntervalHours, SettingDependencyScanHistoryKept,
		SettingDependencyLockOn,
		SettingSecurityTxtContact, SettingSecurityTxtExpires,
		SettingSecurityTxtPolicy, SettingSecurityTxtLanguages,
	}
}

// GetPlatformSettings reads the capability switches, filling anything unset
// from the defaults. Missing rows are not an error — that is a fresh install.
func (db *DB) GetPlatformSettings() (PlatformSettings, error) {
	s := DefaultPlatformSettings()

	// The key list and the placeholder list are generated from one slice
	// rather than written twice. They were a hand-counted `IN (?, ?, …)`
	// beside a hand-written argument list, which is a pair that has to be
	// edited together and silently reads a setting as its default when it is
	// not — the failure mode being an administrator's saved value that never
	// takes effect.
	keys := platformSettingKeys()
	args := make([]any, len(keys))
	placeholders := make([]string, len(keys))
	for i, k := range keys {
		args[i] = k
		placeholders[i] = "?"
	}

	rows, err := db.conn.Query(
		`SELECT key, value FROM settings WHERE key IN (`+strings.Join(placeholders, ", ")+`)`, args...)
	if err != nil {
		return s, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return s, err
		}
		switch key {
		case SettingIsolatedEnabled:
			s.IsolatedEnabled = value == "true"
		case SettingDefaultExecutionMode:
			// A stored value that is not a mode falls back to the default
			// rather than becoming one — the same rule the messages follow,
			// for the same reason: nothing downstream should have to cope
			// with a mode nobody can parse.
			if _, err := runtime.ParseMode(strings.TrimSpace(value)); err == nil {
				s.DefaultExecutionMode = strings.TrimSpace(value)
			}
		case SettingJobsEnabled:
			s.JobsEnabled = value == "true"
		case SettingJobConcurrency:
			if n, err := strconv.Atoi(value); err == nil {
				s.JobConcurrency = n
			}
		case SettingAppMemoryLimitMB:
			if n, err := strconv.Atoi(value); err == nil {
				s.AppMemoryLimitMB = n
			}
		case SettingAppCPURequestMillicores:
			if n, err := strconv.Atoi(value); err == nil {
				s.AppCPURequestMillicores = n
			}
		case SettingAppCPULimitMillicores:
			if n, err := strconv.Atoi(value); err == nil {
				s.AppCPULimitMillicores = n
			}
		case SettingJobMemoryLimitMB:
			if n, err := strconv.Atoi(value); err == nil {
				s.JobMemoryLimitMB = n
			}
		case SettingJobCPULimitMillicores:
			if n, err := strconv.Atoi(value); err == nil {
				s.JobCPULimitMillicores = n
			}
		case SettingMaxConcurrentBuilds:
			if n, err := strconv.Atoi(value); err == nil {
				s.MaxConcurrentBuilds = n
			}
		case SettingAutoDeployPollSeconds:
			if n, err := strconv.Atoi(value); err == nil {
				s.AutoDeployPollSeconds = n
			}
		case SettingRPackageRepository:
			s.RPackageRepository = strings.TrimSpace(value)
		case SettingLogLevel:
			s.LogLevel = strings.ToLower(strings.TrimSpace(value))
		case SettingLockedMessage:
			if v := strings.TrimSpace(value); v != "" {
				s.LockedMessage = v
			}
		case SettingNoAccountMessage:
			if v := strings.TrimSpace(value); v != "" {
				s.NoAccountMessage = v
			}
		case SettingPendingActivationMessage:
			if v := strings.TrimSpace(value); v != "" {
				s.PendingActivationMessage = v
			}
		case SettingActivationMessage:
			if v := strings.TrimSpace(value); v != "" {
				s.ActivationMessage = v
			}
		case SettingAppLockedMessage:
			// Same shape as the two above: a stored blank falls back to the
			// default rather than becoming one, because a visitor refused with
			// an empty explanation is the failure this whole family exists to
			// prevent.
			if v := strings.TrimSpace(value); v != "" {
				s.AppLockedMessage = v
			}
		case SettingEnvironmentTag:
			s.EnvironmentTag = strings.TrimSpace(value)
		case SettingInstallName:
			s.InstallName = strings.TrimSpace(value)
		case SettingDisplayTimeZone:
			// A stored zone this binary no longer knows reads as the browser,
			// never as a page that cannot format a date.
			if v, err := NormalizeTimeZone(value); err == nil {
				s.DisplayTimeZone = v
			}
		case SettingPublicBaseURL:
			s.PublicBaseURL = strings.TrimSpace(value)
		case SettingGarthURL:
			s.GarthURL = strings.TrimSpace(value)
		case SettingIdleSleepMinutes:
			if n, err := strconv.Atoi(value); err == nil {
				s.IdleSleepMinutes = n
			}
		case SettingIsolatedIdleSleepMinutes:
			if n, err := strconv.Atoi(value); err == nil {
				s.IsolatedIdleSleepMinutes = n
			}
		case SettingIsolatedSleepWindowEnabled:
			s.IsolatedSleepWindowEnabled = value == "true"
		case SettingIsolatedSleepWindowStart:
			s.IsolatedSleepWindowStart = strings.TrimSpace(value)
		case SettingIsolatedSleepWindowEnd:
			s.IsolatedSleepWindowEnd = strings.TrimSpace(value)
		case SettingMaxVersions:
			if n, err := strconv.Atoi(value); err == nil {
				s.MaxVersions = n
			}
		case SettingAPIKeysEnabled:
			s.APIKeysEnabled = value == "true"
		case SettingAPIKeyMaxLifetimeDays:
			readInt(value, &s.APIKeyMaxLifetimeDays)
		case SettingPublicAppsEnabled:
			// The one boolean here read as "not false" rather than "is true":
			// the default is on, so an unparseable row must leave it on. The
			// SQL half of this rule (publicAppsOffExpr) reads the same way, and
			// the two must agree or an app would be listed anonymously and
			// refused at the gateway.
			s.PublicAppsEnabled = value != "false"
		case SettingAppsListableToAuthenticated:
			// "Not false", like PublicAppsEnabled above and for the same reason:
			// the default is on, so an unparseable row must leave it on, and the
			// SQL half reads the same way round.
			s.AppsListableToAuthenticated = value != "false"
		case SettingAppLogCopyUncapped:
			s.AppLogCopyUncapped = value == "true"
		case SettingAudienceMeasurementEnabled:
			s.AudienceMeasurementEnabled = value == "true"
		case SettingAppSecurityHeaders:
			// "Not false": on by default, so an unparseable row leaves it on.
			s.AppSecurityHeaders = value != "false"
		case SettingDormantLockEnabled:
			s.DormantLockEnabled = value == "true"
		case SettingDormantLockDays:
			readInt(value, &s.DormantLockDays)
		case SettingSessionAbsoluteHours:
			readInt(value, &s.SessionAbsoluteHours)
		case SettingSessionIdleMinutes:
			readInt(value, &s.SessionIdleMinutes)
		case SettingGalleryMode:
			s.GalleryMode = strings.TrimSpace(value)
		case SettingGitSourcePolicy:
			s.GitSourcePolicy = strings.TrimSpace(value)
		case SettingSourceUploads:
			s.SourceUploadsEnabled = value == "true"
		case SettingDefaultJobTimeout:
			readInt(value, &s.DefaultJobTimeoutSecs)
		case SettingMaxJobTimeout:
			readInt(value, &s.MaxJobTimeoutSecs)
		case SettingMaxQueuedRuns:
			readInt(value, &s.MaxQueuedRunsPerApp)
		case SettingNotebookRenderTimeout:
			readInt(value, &s.NotebookRenderTimeoutSecs)
		case SettingBuildTimeout:
			readInt(value, &s.BuildTimeoutSecs)
		case SettingJobLogRetentionDays:
			readInt(value, &s.JobLogRetentionDays)
		case SettingJobRunRetentionDays:
			readInt(value, &s.JobRunRetentionDays)
		case SettingAppEventRetentionDays:
			readInt(value, &s.AppEventRetentionDays)
		case SettingPlatformLogRetentionDays:
			readInt(value, &s.PlatformLogRetentionDays)
		case SettingAnnouncementRecipientRetentionDays:
			readInt(value, &s.AnnouncementRecipientRetentionDays)
		case SettingAccessRecordRetentionDays:
			readInt(value, &s.AccessRecordRetentionDays)
		case SettingReportDeliveryMinIntervalMinutes:
			readInt(value, &s.ReportDeliveryMinIntervalMinutes)
		case SettingDependencyPolicy:
			s.DependencyPolicy = strings.TrimSpace(value)
		case SettingDependencyScanOnDeploy:
			s.DependencyScanOnDeploy = value == "true"
		case SettingDependencyWatchEnabled:
			s.DependencyWatchEnabled = value == "true"
		case SettingDependencyBlockMalware:
			s.DependencyBlockMalware = value == "true"
		case SettingDependencyBlockSeverity:
			s.DependencyBlockSeverity = strings.TrimSpace(value)
		case SettingDependencyBlockOnlyFixable:
			s.DependencyBlockOnlyFixable = value == "true"
		case SettingDependencyFeedEndpoint:
			s.DependencyFeedEndpoint = strings.TrimSpace(value)
		case SettingDependencyFeedIntervalHours:
			readInt(value, &s.DependencyFeedIntervalHrs)
		case SettingDependencyFeedMaxAgeHours:
			readInt(value, &s.DependencyFeedMaxAgeHours)
		case SettingSecurityTxtContact:
			s.SecurityTxtContact = strings.TrimSpace(value)
		case SettingSecurityTxtExpires:
			s.SecurityTxtExpires = strings.TrimSpace(value)
		case SettingSecurityTxtPolicy:
			s.SecurityTxtPolicy = strings.TrimSpace(value)
		case SettingSecurityTxtLanguages:
			s.SecurityTxtLanguages = strings.TrimSpace(value)
		case SettingDependencyWaiverMaxDays:
			readInt(value, &s.DependencyWaiverMaxDays)
		case SettingDependencyScanIntervalHours:
			readInt(value, &s.DependencyScanIntervalHrs)
		case SettingDependencyScanHistoryKept:
			readInt(value, &s.DependencyScanHistoryKept)
		case SettingDependencyLockOn:
			s.DependencyLockOn = strings.TrimSpace(value)
		}
	}
	clampSettings(&s)
	return s, rows.Err()
}

// SessionPolicy is the install's session policy, cached in memory with the
// other per-request settings (requestPolicy).
//
// Cached because it is read on **every authenticated request**: the absolute
// ceiling is checked there, and the alternative — baking both deadlines into
// the token at mint time — is cheaper and was rejected. An administrator who
// shortens the lifetime from eight hours to one expects it to mean *now*, which
// is the rule the machine-credentials invariant already states as "off means
// absent, and off means now". A policy that only applied to sessions minted
// after the change would be a control an assessor could not test.
//
// A read failure answers with the defaults and caches nothing, so the next
// request tries again. The alternative — a zero policy — would refuse every
// session on the install over one database blip, which is failing closed in the
// one direction that is worse than the risk.
func (db *DB) SessionPolicy() auth.SessionPolicy {
	return db.requestPolicy().session
}

// AppSecurityHeaders is whether the apps gateway fills nosniff and
// Referrer-Policy where an app sent neither, from the same cache as the
// session policy. It is read on every proxied request, and the proxy path does
// no database read (the data-path invariant). A read failure answers on, the
// default, and caches nothing.
func (db *DB) AppSecurityHeaders() bool {
	return db.requestPolicy().appSecurityHeaders
}

// AudienceMeasurement is whether the gateway issues and counts the visitor
// cookie, from the same cache: read on every anonymous gateway request. A
// read failure answers off, the default, and caches nothing.
func (db *DB) AudienceMeasurement() bool {
	return db.requestPolicy().audienceMeasurement
}

// requestPolicy is the settings read on every request, cached as one value.
//
// **One cache over two**, because the invalidation is the part that matters:
// a second pointer would need dropping at every write site as well, and the
// failure is an administrator who turns a switch off, watches the save
// succeed, and sees nothing change. **A database type over a field on
// auth.SessionPolicy**, because a gateway boolean there would be carried by
// every session check and mean nothing to any of them.
//
// **And a lifetime, because the invalidation is per process.** A write drops
// the cache of the node that made it and no other, so on an install with
// several platform replicas a shortened session ceiling took effect on one and
// never on the rest (DB1-2). requestPolicyTTL bounds that: every node converges
// within it, at the price of one settings read per node per TTL.
func (db *DB) requestPolicy() cachedRequestPolicy {
	if p := db.cachedPolicy.Load(); p != nil && time.Since(p.loaded) < requestPolicyTTL {
		return *p
	}
	s, err := db.GetPlatformSettings()
	if err != nil {
		return cachedRequestPolicy{session: auth.DefaultSessionPolicy(), appSecurityHeaders: true}
	}
	p := cachedRequestPolicy{session: s.SessionPolicy(), appSecurityHeaders: s.AppSecurityHeaders,
		audienceMeasurement: s.AudienceMeasurementEnabled, loaded: time.Now()}
	db.cachedPolicy.Store(&p)
	return p
}

// requestPolicyTTL is how stale another node's write may be here.
var requestPolicyTTL = 30 * time.Second

// cachedRequestPolicy is what requestPolicy holds.
type cachedRequestPolicy struct {
	session             auth.SessionPolicy
	appSecurityHeaders  bool
	audienceMeasurement bool
	loaded              time.Time
}

// invalidateRequestPolicy drops the cache.
//
// SetPlatformSettings is the only path that writes the cached keys, and it is
// the invalidation that matters. PutSetting and DeleteSetting call it too —
// they are the generic single-key doors, they cost one reload when they fire,
// and the failure they guard against is a policy change an administrator
// watched succeed and that never took effect. The feature-specific writers
// (SetSourcePolicy, the dependency policy, the FAQ corpus) touch keys this
// cache does not hold and deliberately do not call it.
func (db *DB) invalidateRequestPolicy() {
	db.cachedPolicy.Store(nil)
}

// readInt leaves the default in place for a row that will not parse. An
// unreadable value is the same situation as a missing one, and the default is
// the answer to both.
func readInt(value string, into *int) {
	if n, err := strconv.Atoi(value); err == nil {
		*into = n
	}
}

// SetPlatformSettings writes the capability switches and audits the change in
// the same transaction.
//
// Audited because "who turned isolated execution on, and when" is exactly the
// question the audit log exists to answer — and because these switches decide
// what the platform will spend money on.
// Takes a pointer because it normalizes: a blank message becomes the default
// here, and the caller that renders the saved settings back to the admin must
// show what was stored rather than what was submitted.
func (db *DB) SetPlatformSettings(s *PlatformSettings, actorID, requestID, ip, userAgent string) error {
	return db.SetPlatformSettingsIfUnchanged(s, nil, actorID, requestID, ip, userAgent)
}

// ErrSettingsChanged is the refusal SetPlatformSettingsIfUnchanged makes when
// the document moved after the caller read it.
var ErrSettingsChanged = errors.New("settings changed underneath you")

// PlatformSettingsUpdatedAt is when the platform settings document last
// changed: the newest updated_at among its keys, or the zero time for a
// document nobody has written. The page reads it with the document and hands
// it back with the write, which is the whole of the concurrency control.
func (db *DB) PlatformSettingsUpdatedAt() (time.Time, error) {
	return platformSettingsUpdatedAt(db.conn)
}

func platformSettingsUpdatedAt(q interface {
	QueryRow(query string, args ...any) *sql.Row
}) (time.Time, error) {
	keys := platformSettingKeys()
	args := make([]any, len(keys))
	placeholders := make([]string, len(keys))
	for i, k := range keys {
		args[i] = k
		placeholders[i] = "?"
	}
	var at sql.NullTime
	err := q.QueryRow(`SELECT MAX(updated_at) FROM settings WHERE key IN (`+
		strings.Join(placeholders, ", ")+`)`, args...).Scan(&at)
	if err != nil {
		return time.Time{}, err
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return at.Time.UTC(), nil
}

// SetPlatformSettingsIfUnchanged is SetPlatformSettings with the one
// precondition a whole-document write needs: `since` is the updated_at the
// caller read, and the write is refused with ErrSettingsChanged if the
// document has moved past it. Two administrators saving from two pages loaded
// a moment apart used to have the second overwrite the first's change with
// the value the second page still held. nil means no precondition.
func (db *DB) SetPlatformSettingsIfUnchanged(s *PlatformSettings, since *time.Time, actorID, requestID, ip, userAgent string) error {
	before, err := db.GetPlatformSettings()
	if err != nil {
		return err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if since != nil {
		// Inside the transaction, so the check and the write see one state.
		current, err := platformSettingsUpdatedAt(tx)
		if err != nil {
			return err
		}
		if current.Truncate(time.Millisecond).After(since.UTC().Truncate(time.Millisecond)) {
			return ErrSettingsChanged
		}
	}

	now := time.Now().UTC()
	write := func(key, value string) error {
		_, err := tx.Exec(`
			INSERT INTO settings (key, value, updated_at, updated_by)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET
				value=excluded.value, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
			key, value, now, nullIfEmpty(actorID))
		return err
	}

	clampSettings(s)
	// Normalized before the write, not after the read, so the stored row and
	// what every reader sees are the same string — a cleared message is stored
	// as the default rather than as an empty row that each reader has to
	// remember to fill in.
	if s.LockedMessage = strings.TrimSpace(s.LockedMessage); s.LockedMessage == "" {
		s.LockedMessage = DefaultLockedMessage
	}
	if s.NoAccountMessage = strings.TrimSpace(s.NoAccountMessage); s.NoAccountMessage == "" {
		s.NoAccountMessage = DefaultNoAccountMessage
	}
	if s.PendingActivationMessage = strings.TrimSpace(s.PendingActivationMessage); s.PendingActivationMessage == "" {
		s.PendingActivationMessage = DefaultPendingActivationMessage
	}
	if s.ActivationMessage = strings.TrimSpace(s.ActivationMessage); s.ActivationMessage == "" {
		s.ActivationMessage = DefaultActivationMessage
	}
	if s.AppLockedMessage = strings.TrimSpace(s.AppLockedMessage); s.AppLockedMessage == "" {
		s.AppLockedMessage = DefaultAppLockedMessage
	}
	if err := write(SettingIsolatedEnabled, boolText(s.IsolatedEnabled)); err != nil {
		return err
	}
	if err := write(SettingDefaultExecutionMode, s.DefaultExecutionMode); err != nil {
		return err
	}
	if err := write(SettingJobsEnabled, boolText(s.JobsEnabled)); err != nil {
		return err
	}
	if err := write(SettingAppMemoryLimitMB, strconv.Itoa(s.AppMemoryLimitMB)); err != nil {
		return err
	}
	for _, d := range []struct {
		key   string
		value string
	}{
		{SettingAppCPURequestMillicores, strconv.Itoa(s.AppCPURequestMillicores)},
		{SettingAppCPULimitMillicores, strconv.Itoa(s.AppCPULimitMillicores)},
		{SettingJobMemoryLimitMB, strconv.Itoa(s.JobMemoryLimitMB)},
		{SettingJobCPULimitMillicores, strconv.Itoa(s.JobCPULimitMillicores)},
		{SettingMaxConcurrentBuilds, strconv.Itoa(s.MaxConcurrentBuilds)},
		{SettingAutoDeployPollSeconds, strconv.Itoa(s.AutoDeployPollSeconds)},
		{SettingRPackageRepository, strings.TrimSpace(s.RPackageRepository)},
		{SettingLogLevel, strings.ToLower(strings.TrimSpace(s.LogLevel))},
	} {
		if err := write(d.key, d.value); err != nil {
			return err
		}
	}
	if err := write(SettingJobConcurrency, strconv.Itoa(s.JobConcurrency)); err != nil {
		return err
	}
	if err := write(SettingLockedMessage, s.LockedMessage); err != nil {
		return err
	}
	if err := write(SettingNoAccountMessage, s.NoAccountMessage); err != nil {
		return err
	}
	if err := write(SettingPendingActivationMessage, s.PendingActivationMessage); err != nil {
		return err
	}
	if err := write(SettingActivationMessage, s.ActivationMessage); err != nil {
		return err
	}
	if err := write(SettingAppLockedMessage, s.AppLockedMessage); err != nil {
		return err
	}
	s.EnvironmentTag = strings.TrimSpace(s.EnvironmentTag)
	if err := write(SettingEnvironmentTag, s.EnvironmentTag); err != nil {
		return err
	}
	// Trimmed, not validated: NormalizeInstallName is the API's, for the reason
	// the public address below gives.
	s.InstallName = strings.TrimSpace(s.InstallName)
	if err := write(SettingInstallName, s.InstallName); err != nil {
		return err
	}
	if err := write(SettingDisplayTimeZone, strings.TrimSpace(s.DisplayTimeZone)); err != nil {
		return err
	}
	// Normalized, not validated, here: a caller that accepts one from a person
	// validates it (NormalizePublicBaseURL) and reports the problem to them.
	// Reaching this far with a malformed value would mean an internal caller,
	// and storing "" is the reading that falls back to BASE_URL rather than the
	// one that puts a broken link in everybody's mail.
	s.PublicBaseURL = strings.TrimSuffix(strings.TrimSpace(s.PublicBaseURL), "/")
	if err := write(SettingPublicBaseURL, s.PublicBaseURL); err != nil {
		return err
	}
	// Normalized, not validated, for the reason the public address gives:
	// NormalizeGarthURL is the API's.
	s.GarthURL = strings.TrimSuffix(strings.TrimSpace(s.GarthURL), "/")
	if err := write(SettingGarthURL, s.GarthURL); err != nil {
		return err
	}
	if err := write(SettingIdleSleepMinutes, strconv.Itoa(s.IdleSleepMinutes)); err != nil {
		return err
	}
	if err := write(SettingIsolatedIdleSleepMinutes, strconv.Itoa(s.IsolatedIdleSleepMinutes)); err != nil {
		return err
	}
	if err := write(SettingIsolatedSleepWindowEnabled, boolText(s.IsolatedSleepWindowEnabled)); err != nil {
		return err
	}
	if err := write(SettingIsolatedSleepWindowStart, s.IsolatedSleepWindowStart); err != nil {
		return err
	}
	if err := write(SettingIsolatedSleepWindowEnd, s.IsolatedSleepWindowEnd); err != nil {
		return err
	}
	if err := write(SettingMaxVersions, strconv.Itoa(s.MaxVersions)); err != nil {
		return err
	}
	if err := write(SettingPublicAppsEnabled, boolText(s.PublicAppsEnabled)); err != nil {
		return err
	}
	if err := write(SettingAppSecurityHeaders, boolText(s.AppSecurityHeaders)); err != nil {
		return err
	}
	if err := write(SettingAudienceMeasurementEnabled, boolText(s.AudienceMeasurementEnabled)); err != nil {
		return err
	}
	if err := write(SettingAppLogCopyUncapped, boolText(s.AppLogCopyUncapped)); err != nil {
		return err
	}
	if err := write(SettingAPIKeysEnabled, boolText(s.APIKeysEnabled)); err != nil {
		return err
	}
	if err := write(SettingAPIKeyMaxLifetimeDays, strconv.Itoa(s.APIKeyMaxLifetimeDays)); err != nil {
		return err
	}
	if err := write(SettingAppsListableToAuthenticated, boolText(s.AppsListableToAuthenticated)); err != nil {
		return err
	}
	if err := write(SettingDormantLockEnabled, boolText(s.DormantLockEnabled)); err != nil {
		return err
	}
	if err := write(SettingGalleryMode, s.GalleryMode); err != nil {
		return err
	}
	// SettingGitSourcePolicy and SettingSourceUploads are read by
	// GetPlatformSettings and deliberately *not* written here. They belong to
	// SetSourcePolicy, which writes them beside the pattern list and audits the
	// whole thing as a diff. Writing them from this blob as well would give the
	// two keys two owners, and the audit trail for a security control would
	// depend on which page an administrator happened to use.
	for _, kv := range []struct {
		key string
		val int
	}{
		{SettingDefaultJobTimeout, s.DefaultJobTimeoutSecs},
		{SettingMaxJobTimeout, s.MaxJobTimeoutSecs},
		{SettingMaxQueuedRuns, s.MaxQueuedRunsPerApp},
		{SettingNotebookRenderTimeout, s.NotebookRenderTimeoutSecs},
		{SettingBuildTimeout, s.BuildTimeoutSecs},
		{SettingJobLogRetentionDays, s.JobLogRetentionDays},
		{SettingJobRunRetentionDays, s.JobRunRetentionDays},
		{SettingAppEventRetentionDays, s.AppEventRetentionDays},
		{SettingPlatformLogRetentionDays, s.PlatformLogRetentionDays},
		{SettingAnnouncementRecipientRetentionDays, s.AnnouncementRecipientRetentionDays},
		{SettingAccessRecordRetentionDays, s.AccessRecordRetentionDays},
		{SettingReportDeliveryMinIntervalMinutes, s.ReportDeliveryMinIntervalMinutes},
		{SettingDormantLockDays, s.DormantLockDays},
		{SettingSessionAbsoluteHours, s.SessionAbsoluteHours},
		{SettingSessionIdleMinutes, s.SessionIdleMinutes},
		{SettingDependencyFeedIntervalHours, s.DependencyFeedIntervalHrs},
		{SettingDependencyFeedMaxAgeHours, s.DependencyFeedMaxAgeHours},
		{SettingDependencyWaiverMaxDays, s.DependencyWaiverMaxDays},
		{SettingDependencyScanIntervalHours, s.DependencyScanIntervalHrs},
		{SettingDependencyScanHistoryKept, s.DependencyScanHistoryKept},
	} {
		if err := write(kv.key, strconv.Itoa(kv.val)); err != nil {
			return err
		}
	}

	// The dependency policy's own keys. All of them here, including the
	// endpoint — unlike the source policy, this feature has no separate list
	// with its own diff audit, so there is no second owner for these rows and
	// no ambiguity about which page wrote one.
	//
	// The endpoint is audited **in the clear** as part of the settings blob,
	// deliberately: "who re-pointed this install's advisory feed, and where
	// to" is precisely the question that log exists to answer. The mirror
	// credential is not here at all — it is its own encrypted document, for
	// the reason the SMTP password and the collector headers are.
	s.DependencyFeedEndpoint = strings.TrimSpace(s.DependencyFeedEndpoint)
	for _, kv := range []struct{ key, val string }{
		{SettingDependencyPolicy, s.DependencyPolicy},
		{SettingDependencyScanOnDeploy, boolText(s.DependencyScanOnDeploy)},
		{SettingDependencyWatchEnabled, boolText(s.DependencyWatchEnabled)},
		{SettingDependencyBlockMalware, boolText(s.DependencyBlockMalware)},
		{SettingDependencyBlockSeverity, s.DependencyBlockSeverity},
		{SettingDependencyBlockOnlyFixable, boolText(s.DependencyBlockOnlyFixable)},
		{SettingDependencyFeedEndpoint, s.DependencyFeedEndpoint},
		{SettingDependencyLockOn, s.DependencyLockOn},
		// Audited in the clear with the endpoints: a contact address and a
		// policy URL are published to the anonymous internet by design, so
		// there is nothing here a credential rule would be protecting — and
		// "who changed where security reports go" is exactly what that log is
		// for.
		{SettingSecurityTxtContact, strings.TrimSpace(s.SecurityTxtContact)},
		{SettingSecurityTxtExpires, strings.TrimSpace(s.SecurityTxtExpires)},
		{SettingSecurityTxtPolicy, strings.TrimSpace(s.SecurityTxtPolicy)},
		{SettingSecurityTxtLanguages, strings.TrimSpace(s.SecurityTxtLanguages)},
	} {
		if err := write(kv.key, kv.val); err != nil {
			return err
		}
	}

	if err := writeAuditTx(tx, AuditEntry{
		ActorType: PrincipalTypeUser, ActorID: actorID,
		Action:     "platform_settings.update",
		TargetType: "platform", TargetID: "settings",
		Before:    jsonStr(before),
		After:     jsonStr(s),
		RequestID: requestID, IP: ip, UserAgent: userAgent,
	}); err != nil {
		return fmt.Errorf("auditing settings change: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	// After the commit, never before: a cache dropped ahead of a rollback would
	// reload the old numbers and look like the write worked.
	db.invalidateRequestPolicy()
	return nil
}

// NormalizePublicBaseURL validates an administrator-supplied public base URL
// and returns it in the one form everything downstream expects: scheme, host,
// no trailing slash, no path. Empty in means empty out — clearing the setting
// is how an administrator hands the decision back to BASE_URL.
//
// Strict about the scheme because the value's whole purpose is to be pasted
// into an email and clicked: a javascript: or data: URL in a notification the
// platform sent would be a phishing primitive with the platform's name on it.
func NormalizePublicBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return "", fmt.Errorf("needs a scheme — write it as https://%s", raw)
	default:
		return "", fmt.Errorf("scheme %q is not allowed; use http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("needs a hostname")
	}
	// A path would be doubled by every caller that appends its own.
	if p := strings.Trim(u.Path, "/"); p != "" {
		return "", fmt.Errorf("must be an address only, without the path %q", u.Path)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("must be an address only, without a query or fragment")
	}

	return strings.ToLower(u.Scheme) + "://" + u.Host, nil
}

// NormalizeGarthURL validates an administrator-supplied Garth address and
// returns it without a trailing slash, so `{garth}/open/toft` is one join.
// Empty in means empty out, which is how the link is turned off.
//
// https only, unlike the public base URL: the link carries an app's repository
// to another product, and a Garth worth linking to is served over TLS. No
// userinfo, query or fragment, because each would either leak into every link
// or be silently mangled by the path appended to it. A path is allowed — Garth
// may live under one.
func NormalizeGarthURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "":
		return "", fmt.Errorf("needs a scheme — write it as https://%s", raw)
	default:
		return "", fmt.Errorf("scheme %q is not allowed; use https", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("needs a hostname")
	}
	if u.User != nil {
		return "", fmt.Errorf("must not carry a user name or password")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("must be an address only, without a query or fragment")
	}
	return "https://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/"), nil
}

// ResolvePublicBaseURL answers "where do links in outbound mail point", the
// stored setting winning over the BASE_URL the process was started with.
//
// One place, because the alternative is each notification path deciding for
// itself and two of them disagreeing about which address the install answers
// to — the failure this whole setting exists to make visible.
func (db *DB) ResolvePublicBaseURL(envBaseURL string) string {
	fallback := strings.TrimSuffix(strings.TrimSpace(envBaseURL), "/")
	if db == nil {
		return fallback
	}
	row, err := db.GetSetting(SettingPublicBaseURL)
	if err != nil || row == nil {
		return fallback
	}
	if v := strings.TrimSuffix(strings.TrimSpace(row.Value), "/"); v != "" {
		return v
	}
	return fallback
}

// DefaultSenderName is what an install with no name calls itself.
const DefaultSenderName = "Toft"

// InstallNameLimit bounds the install name. It sits in a Slack context line and
// a mail footer, and both wrap badly on a phone well before a sentence would.
const InstallNameLimit = 60

// installNameURLish matches a dotted host with a TLD-like suffix — "acme.com",
// "apps.example.org" — which is what makes a name read as an address.
var installNameURLish = regexp.MustCompile(`(?i)(^|[^a-z0-9-])[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,63}($|[^a-z0-9.-])`)

// NormalizeInstallName trims an administrator's name for the install and says
// what is wrong with it, if anything. Empty is valid: it restores "Toft".
//
// URL-shaped names are refused because every footer prints the name beside the
// install's real address: a name that looks like a link puts a second,
// unverified address into every message, and the recipient checking one against
// the other is the point of printing both.
func NormalizeInstallName(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if n := utf8.RuneCountInString(v); n > InstallNameLimit {
		return "", fmt.Errorf("must be %d characters or fewer", InstallNameLimit)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("must be one line, without control characters")
		}
	}
	if strings.ContainsAny(v, "<>") {
		return "", fmt.Errorf("can’t contain < or >")
	}
	if strings.Contains(v, "://") || strings.HasPrefix(strings.ToLower(v), "www.") ||
		installNameURLish.MatchString(v) {
		return "", fmt.Errorf("can’t look like an address")
	}
	return v, nil
}

// ResolveSenderName answers "what does this install call itself in its
// messages": the administrator's name for it, or "Toft" when there is none.
//
// Beside ResolvePublicBaseURL because it answers the same kind of question, and
// read uncached for the same reason — it is one row, and a renamed install must
// not go on sending under the old name until somebody restarts it. Called by
// the transports (mail at send, Slack and the posters before they render),
// never by the sites that build a message's data: a name passed through those
// would be filled at some and forgotten at others.
func (db *DB) ResolveSenderName() string {
	if db == nil {
		return DefaultSenderName
	}
	row, err := db.GetSetting(SettingInstallName)
	if err != nil || row == nil {
		return DefaultSenderName
	}
	if v := strings.TrimSpace(row.Value); v != "" {
		return v
	}
	return DefaultSenderName
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// CountAppsInMode reports how many apps are in a mode, so an administrator
// turning isolated execution off is told what it affects before they do it
// rather than afterwards.
func (db *DB) CountAppsInMode(mode string) (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM apps WHERE execution_mode=?`, mode).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// --- The install identity ---

// SettingInstallID names this install, and is the only settings key with no
// writer anywhere in the product.
//
// It is written once, by the migration that introduces it, and read
// everywhere else. `PlatformSettings` does not carry it and
// `SetPlatformSettings` never touches it, so there is no path through the API
// that can change it — which is what makes it safe for a compliance report or
// a support conversation to open with it.
const SettingInstallID = "install.id"

// InstallID returns this install's identity, or "" on an install whose
// migration has not run.
//
// It is **not a secret**. It identifies an install, not a principal; it
// authenticates nothing and authorizes nothing, and it is meant to be read out.
// Treating it as a credential would be theatre.
func (db *DB) InstallID() (string, error) {
	s, err := db.GetSetting(SettingInstallID)
	if err != nil || s == nil {
		return "", err
	}
	return s.Value, nil
}

// Dials is the administrator's half of the operating envelope: the dial
// settings, in the units their rows carry.
//
// It returns no error, and that is the rule rather than a convenience. A
// settings document that will not read must not stop a workload starting, so an
// unreadable row resolves to the environment's number exactly as an unset one
// does — see config.DialSource. This is the *only* place the settings document
// is turned into overrides; a dial read anywhere else is the defect the
// envelope exists to end.
func (db *DB) Dials() config.DialOverrides {
	s, err := db.GetPlatformSettings()
	if err != nil {
		return config.DialOverrides{}
	}
	return config.DialOverrides{
		AppMemoryLimitMB:        s.AppMemoryLimitMB,
		AppCPURequestMillicores: s.AppCPURequestMillicores,
		AppCPULimitMillicores:   s.AppCPULimitMillicores,
		JobMemoryLimitMB:        s.JobMemoryLimitMB,
		JobCPULimitMillicores:   s.JobCPULimitMillicores,
		MaxConcurrentBuilds:     s.MaxConcurrentBuilds,
		AutoDeployPollSeconds:   s.AutoDeployPollSeconds,
		RRepos:                  s.RPackageRepository,
		LogLevel:                s.LogLevel,
	}
}
