import { adminGroups } from "@/lib/admin-nav";

/* ------------------------------------------------------------------ *
 * The settings index — install-wide
 *
 * Every setting an administrator can change, keyed by the name the API
 * uses for it, wherever it is rendered. This was the Settings page's own
 * `COPY` and `SECTIONS` tables until they were promoted here: they made
 * the filter box, the "on this page" rail and the rows themselves read
 * from one source, so a setting could not be findable by one and
 * invisible to another — but only for the settings on that one page.
 * Twenty-six are there now; the other sixty live on Email, Dependencies,
 * Messages, Telemetry, Authentication, Integrations, Sources, License,
 * Features and API keys, where nothing could find them: typing `smtp`
 * into the Settings filter answered "No setting by that name", in the one
 * place an administrator was taught to look.
 *
 * Three consumers read this:
 *
 *   1. The Settings page — its sections, its rows and its filter.
 *   2. The command palette, which reached twenty-one pages and zero
 *      settings, and now navigates to `route#anchor`.
 *   3. Any local filter that comes up empty, which can now say where
 *      else to look instead of saying the setting does not exist.
 *
 * `summary` is the line always on screen — what the setting does, in one
 * breath. `why` is the consequence, folded away behind a disclosure: an
 * administrator scanning for the idle-sleep box should not have to read
 * about rebuilding every app on the way there, and an administrator
 * about to throw that switch should not have to go and find out
 * elsewhere. Only `SettingRow` draws it, so an entry rendered by any
 * other editor carries none: a `why` nobody can see is read only by
 * search, and its words belong in `keywords`.
 *
 * **The guard.** A setting in this index and rendered nowhere, or
 * rendered and not in this index, is the failure this must not permit —
 * `settings-index.test.ts` walks both directions. That is what `anchor`
 * and `control` are for: they are what makes the claim checkable rather
 * than aspirational. Adding a setting to a page without an entry here
 * fails that test; so does an entry whose control has left the page.
 * ------------------------------------------------------------------ */

export interface SettingEntry {
  /** The name the API uses. Unique across the install. */
  id: string;
  label: string;
  /** One line, always visible. */
  summary: string;
  /** The consequence, behind the `?` beside the label. `SettingRow` rows only. */
  why?: string;
  /** Words someone might search for that appear in neither of the above. */
  keywords?: string;
  /** The page that renders it. */
  route: string;
  /** The `id` of the element on that page to scroll to. */
  anchor: string;
  /**
   * The `id` of the control itself, when it differs from the setting's own
   * name. On the Settings page the row carries the setting's name, so this is
   * absent there and present nearly everywhere else.
   */
  control?: string;
  /**
   * Not one of its page's generated rows: an editor of its own, with its own
   * Save — a source policy that is a mode and a list saved together, a message
   * whose textarea lives in its own component, a family of switches whose
   * names are data rather than code. The guard looks for the anchor and not
   * for a control.
   */
  composite?: true;
}

/**
 * The entries themselves.
 *
 * `as const satisfies` rather than a plain annotation: it is what keeps each
 * `id` a literal type, which is what lets `SETTING` be typed on the ids the
 * index actually declares. `SETTINGS_INDEX` below is the same array widened
 * back to `SettingEntry[]`, which is what everything reading it wants.
 */
const ENTRIES = [
  /* ── Admin → Settings ────────────────────────────────────────────── */

  {
    id: "public_base_url",
    label: "Address",
    summary: "Where people reach this install. Sign-in, email and Slack links point here.",
    keywords: "base url hostname domain link email public apps address apps_base_url",
    route: "/admin/settings",
    anchor: "address",
  },

  {
    id: "garth_url",
    label: "Garth address",
    summary: "Where your organization’s Garth is. Set, app owners and editors get Open in Garth.",
    why: "Each app’s page links its owners and editors to Garth with the app’s name and repository, and Garth asks them to confirm before it opens a session. This install never contacts Garth. Blank removes the link everywhere.",
    keywords: "garth coding agent workbench open in garth editor link developer session",
    route: "/admin/settings",
    anchor: "address",
  },

  {
    id: "environment_tag",
    label: "Environment name",
    summary: "Which environment this is, such as stage or prod. Telemetry reports it.",
    why: "Telemetry reports it as the deployment environment, from the next restart. It is a label only: an app’s variables are named in its toft.toml and given values on each install with toft env set.",
    keywords: "stage prod staging production telemetry deployment environment otel",
    route: "/admin/settings",
    anchor: "address",
  },

  {
    id: "install_name",
    label: "Install name",
    summary: "What emails and notifications call this install. Blank uses “Toft”.",
    why: "It replaces “Toft” as the sender in every email’s header and footer, Slack post and Teams or webhook card, and in the line that says an app’s message came from this install. The install’s address always follows it, so a recipient can check a name they don’t recognize. It is never sent to the vendor.",
    keywords: "name sender from footer branding email slack teams notification origin white label which install",
    route: "/admin/settings",
    anchor: "address",
  },

  {
    id: "display_time_zone",
    label: "Time zone",
    summary: "Times are shown in this zone unless a person chooses their own.",
    why: "Every date and time in Toft is shown in one zone, never the server’s. A person’s own choice on their account page wins; this is the default under it, for an organization that works on one clock or whose people browse from machines set to UTC. The API and every export stay in UTC whatever is chosen.",
    keywords: "timezone time zone tz utc local clock date timestamp display browser region",
    route: "/admin/settings",
    anchor: "address",
  },

  {
    id: "security_txt_contact",
    label: "Security contact",
    summary: "Where to report a security problem, usually a mailto: address.",
    keywords: "security.txt rfc 9116 disclosure vulnerability report researcher contact well-known",
    route: "/admin/settings",
    anchor: "security",
  },
  {
    id: "security_txt_expires",
    label: "Good until",
    summary: "When the published contact expires. Required when there is a contact.",
    why: "A date in the past is refused. An expired date is still served as written, so review it before it lapses.",
    keywords: "expiry expires date stale review renew security.txt",
    route: "/admin/settings",
    anchor: "security",
  },
  {
    id: "security_txt_policy",
    label: "Disclosure policy",
    summary: "An https:// page on how you want problems reported. Optional.",
    keywords: "policy disclosure safe harbour page url security.txt",
    route: "/admin/settings",
    anchor: "security",
  },
  {
    id: "security_txt_languages",
    label: "Preferred languages",
    summary: "Languages a report can be written in, separated by commas. Optional.",
    keywords: "language languages preferred locale tag security.txt",
    route: "/admin/settings",
    anchor: "security",
  },
  {
    id: "app_security_headers",
    label: "Baseline headers on apps",
    summary: "Adds nosniff and a referrer policy to an app’s pages where the app sends neither.",
    why: "An app that sends its own header keeps it. HTTPS-only (HSTS) is sent over https whatever this says.",
    keywords:
      "security headers nosniff x-content-type-options referrer-policy hsts strict-transport-security scanner zap baseline app response",
    route: "/admin/settings",
    anchor: "security",
  },

  {
    id: "isolated_execution_enabled",
    label: "Isolated execution",
    summary: "Lets an app run as its own workload, with its own network and resource limits.",
    why: "Apps run shared by default. The first time this is turned on, every app must be rebuilt, so plan a window. Turning it off later leaves isolated apps running.",
    keywords: "isolation sandbox workload promote runner image",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "default_execution_mode",
    label: "Execution mode for new apps",
    summary: "The mode a new app starts in when its creator doesn’t choose.",
    why: "Read once, when an app is created. Changing it moves no existing app and starts no rebuild.",
    keywords: "default new app create born isolated shared wizard seed",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "background_jobs_enabled",
    label: "Background jobs",
    summary: "Lets apps run scheduled and on-demand work as a process of its own.",
    keywords: "jobs queue enqueue worker command",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "app_memory_limit_mb",
    label: "Memory limit per app",
    summary: "Memory one app may use before it is restarted. 0 uses the install default.",
    why: "A change reaches each app the next time it starts, so restart an app to apply it now. An app over its limit is restarted with nothing in its own log to say why.",
    keywords: "memory limit RAM restart killed oom cap gigabytes GB",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "app_cpu_request_millicores",
    label: "CPU reserved per app",
    summary: "CPU each isolated app reserves, in cores. 0 uses the install default.",
    why: "Without a reservation, apps are packed onto a node until they starve each other. Shared apps ignore it.",
    keywords: "cpu request reserve millicores cores scheduler node starve",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "app_cpu_limit_millicores",
    label: "CPU limit per app",
    summary: "The most CPU time one app may use, in cores. 0 uses the install default.",
    why: "An amount of time, not a set of cores. Over it an app is slowed, not restarted, and its owners are told when that lasts an hour. It can’t be lower than the reservation above.",
    keywords: "cpu limit throttle ceiling millicores cores quota cpu.max app_cpu_throttled",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "max_concurrent_builds",
    label: "Builds at once",
    summary: "How many deploys may build at the same time. 0 uses the install default.",
    why: "Lowering it stops new builds starting and interrupts none. It can’t exceed the build workers the platform started with.",
    keywords: "build queue concurrency parallel deploy workers pool",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "auto_deploy_poll_seconds",
    label: "Auto-deploy check interval",
    summary: "How often tracked branches are checked for new commits. 0 uses the install default.",
    why: "A shorter interval calls your git host more often, which may hit its rate limit.",
    keywords: "auto deploy poll interval branch commits webhook pace",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "r_package_repository",
    label: "R package repository",
    summary: "Where R builds install packages from. Leave blank to use the install default.",
    why: "The default repository has Linux binaries for x86_64 only. On arm64, point this at another mirror or R deploys fail.",
    keywords: "R renv CRAN P3M repository mirror packages binary arm64",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "log_level",
    label: "Platform log level",
    summary: "How much the platform logs about itself.",
    keywords: "log level debug info warn error verbosity diagnostics volume",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "app_log_copy_uncapped",
    label: "Copy every line of app output",
    summary: "Lifts the 100-lines-a-second cap on each shared app’s output reaching the platform’s log.",
    why: "When on, one app’s volume can slow every app’s requests. A full queue still drops, and every drop is counted.",
    keywords: "app logs output stdout collector cap limit uncapped flood drop audit au-4 central logging",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "notebook_render_timeout_seconds",
    label: "Let a notebook render for",
    summary: "How long a notebook may take to render during a deploy. Past this, the deploy fails.",
    why: "The live release stays up. Builds share a queue, so a slow notebook can hold up other deploys.",
    keywords: "notebook jupyter quarto render execute timeout deploy build",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "report_delivery_min_interval_minutes",
    label: "Email one report at most every",
    summary: "The shortest gap between two deliveries of one report. 0 removes the floor.",
    why: "A report may still re-render more often; only the email is held to this. A skipped send is recorded.",
    keywords: "report delivery email recipients schedule notebook render floor interval minimum rate limit relay spam",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "build_timeout_seconds",
    label: "Let a build command run for",
    summary: "How long one build step may take. Past this, the deploy fails.",
    why: "The live release stays up. Builds share a queue, so raising this lets one deploy hold up others. An app can be given more time on its own page.",
    keywords: "build timeout deploy install dependencies compile renv npm slow",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "idle_sleep_minutes",
    label: "Sleep idle shared apps after",
    summary: "Minutes without a request before a shared app is stopped to free memory. 0 turns this off.",
    why: "The next visit wakes it in a second or two. Isolated apps have their own timeout. An app can opt out on its own page.",
    keywords: "sleeping idle hibernate memory reclaim wake shared",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "isolated_idle_sleep_minutes",
    label: "Sleep idle isolated apps after",
    summary: "Minutes without a request before an isolated app’s pod is stopped. 0 turns this off.",
    why: "Frees what the cluster reserves for the pod. The next visit starts it again, which can take a few minutes; visitors see a page that refreshes until it is up. Jobs are not affected.",
    keywords: "sleeping idle hibernate pod cluster reclaim wake isolated scale to zero",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "isolated_sleep_window_enabled",
    label: "Sleep isolated apps only overnight",
    summary: "Isolated apps sleep only inside the window below, and wake one at a time when it ends.",
    why: "Keeps pods warm during the working day and frees them overnight. When off, the timeout applies at any hour.",
    keywords: "sleep window overnight schedule hours night wake morning isolated",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "isolated_sleep_window_start",
    label: "Sleep window starts",
    summary: "From this time of day, idle isolated apps may sleep.",
    keywords: "sleep window start evening night",
    route: "/admin/settings",
    anchor: "execution",
  },
  {
    id: "isolated_sleep_window_end",
    label: "Sleep window ends",
    summary: "At this time, isolated apps still asleep are woken one at a time.",
    keywords: "sleep window end morning wake",
    route: "/admin/settings",
    anchor: "execution",
  },


  {
    id: "job_concurrency_per_app",
    label: "Concurrent runs per app",
    summary: "How many of one app’s runs may be in flight at once.",
    keywords: "concurrency parallel limit",
    route: "/admin/settings",
    anchor: "jobs",
  },
  {
    id: "max_queued_runs_per_app",
    label: "Runs waiting per app",
    summary: "How many of one app’s runs may wait. A start past this is refused until one finishes.",
    keywords: "queue full backlog waiting pending cap limit queue_full",
    route: "/admin/settings",
    anchor: "jobs",
  },
  {
    id: "default_job_timeout_seconds",
    label: "Stop a job after",
    summary: "The timeout for a job whose toft.toml declares none.",
    keywords: "job timeout deadline default seconds duration run",
    route: "/admin/settings",
    anchor: "jobs",
  },
  {
    id: "max_job_timeout_seconds",
    label: "Longest a job may run",
    summary: "The ceiling on any job’s timeout. A longer one in toft.toml is refused at check and deploy.",
    keywords: "job timeout ceiling maximum clamp limit_exceeds_ceiling week",
    route: "/admin/settings",
    anchor: "jobs",
  },
  {
    id: "job_memory_limit_mb",
    label: "Memory per job run",
    summary: "Memory a job run may use. 0 uses the install default. Override it on one job from its app’s Automation tab.",
    keywords: "job memory limit default ram mb gb override",
    route: "/admin/settings",
    anchor: "jobs",
  },
  {
    id: "job_cpu_limit_millicores",
    label: "CPU per job run",
    summary: "The most CPU time a job run may use, in cores. 0 uses the install default.",
    keywords: "job cpu limit default millicores cores throttle override",
    route: "/admin/settings",
    anchor: "jobs",
  },

  {
    id: "dormant_lock_enabled",
    label: "Lock unused accounts",
    summary: "Locks an account nobody has used for the period below. Administrators are never locked.",
    why: "Toft isn’t told when your identity provider removes someone, so this is the backstop. A locked person’s apps keep running; their sessions and personal keys stop until unlocked.",
    keywords:
      "dormant inactive idle stale accounts disable deprovision offboard leaver joiner mover SOC2 ISO27001 CIS compliance audit lifecycle scim",
    route: "/admin/authentication",
    anchor: "dormant",
  },
  {
    id: "dormant_lock_days",
    label: "Lock after",
    summary: "How long an account may go unused before it is locked. At least 30 days.",
    why: "Unused means no sign-in and no use of a personal API key. Turning the lock off unlocks nobody.",
    keywords: "dormant threshold days inactive period grace",
    route: "/admin/authentication",
    anchor: "dormant",
  },

  {
    id: "session_absolute_hours",
    label: "Sign in again after",
    summary: "How long a session lasts from sign-in, however much it is used.",
    why: "The only way a session ends after your identity provider disables someone. Shortening it signs people out on their next request, including you.",
    keywords:
      "session lifetime timeout expiry expire absolute maximum duration jwt token sign out logout NIST 800-63B AAL2 compliance",
    route: "/admin/authentication",
    anchor: "sessions",
  },
  {
    id: "session_idle_minutes",
    label: "End an unused session after",
    summary: "How long an unused session lasts. 0 turns this off.",
    why: "Time in a deployed app counts as use. Apps that hold a WebSocket, like Streamlit, may not, so people can be asked to sign in again.",
    keywords:
      "idle inactivity timeout session lock unattended screen away NIST 800-63B AAL2 PCI compliance websocket streamlit",
    route: "/admin/authentication",
    anchor: "sessions",
  },

  {
    id: "public_apps_enabled",
    label: "Apps open to anyone",
    summary: "Whether an app may be opened by someone who isn’t signed in.",
    why: "When off, every public app stops serving anonymous visitors at once. Turning it back on publishes the same apps again.",
    keywords:
      "public anonymous unauthenticated internet expose exposure publish published logged out visitors sign-in required intranet lockdown",
    route: "/admin/authentication",
    anchor: "anonymous",
    control: "public_apps_enabled",
  },

  {
    id: "audience_measurement_enabled",
    label: "Count anonymous browsers",
    summary: "Counts how many browsers open apps without signing in, with a 13-month cookie.",
    why: "The cookie holds a random ID that no app sees. It is counted into sketches that estimate how many browsers came, never which. Nothing leaves this install. Some jurisdictions require consent for any cookie, so check yours before turning this on.",
    keywords:
      "audience anonymous visitors browsers unique reach cookie analytics measurement page loads traffic cto report hyperloglog consent gdpr eprivacy cnil gpc",
    route: "/admin/authentication",
    anchor: "anonymous",
    control: "audience_measurement_enabled",
  },

  {
    id: "apps_listable_to_authenticated",
    label: "Everyone signed in can see every app",
    summary: "Every app appears in everyone’s list by name and owner, without being openable.",
    why: "It grants no access. People can find an app and ask its owner for it. An owner can hide one app on its Access tab.",
    keywords:
      "list listing visible discoverable directory catalog everyone signed in browse find request access rung hide unlisted turn off grants no access",
    route: "/admin/authentication",
    anchor: "listing",
    control: "apps_listable_to_authenticated",
  },

  {
    id: "api_keys_enabled",
    label: "Personal API keys",
    summary: "Lets people hold long-lived keys so a script can act as them.",
    why: "Turning this off stops every existing personal key immediately. App keys are unaffected.",
    keywords:
      "token credential automation script lpu personal api key long-lived",
    route: "/admin/api-keys",
    anchor: "personal-keys",
  },

  {
    id: "api_key_max_lifetime_days",
    label: "Longest a personal key may last",
    summary: "The most days a new personal key may last. 0 means no limit.",
    why: "With no limit, keys from toft login and RStudio never expire. A limit applies to keys created from now on; existing keys keep their expiry.",
    keywords:
      "token credential expiry expiration lifetime maximum cap rotate rsconnect rstudio cli toft login personal api key",
    route: "/admin/api-keys",
    anchor: "personal-keys",
  },

  {
    id: "gallery_deploy_mode",
    label: "Gallery",
    summary: "What the gallery offers: nothing, extensions only, or extensions and examples.",
    why: "Extensions update in place. An example is a copy that never updates. When off, nothing is fetched from the catalog.",
    keywords:
      "catalog extensions examples samples templates marketplace add-ons sync refresh mirror",
    route: "/admin/settings",
    anchor: "gallery",
  },

  {
    id: "dependency_policy",
    label: "Dependency policy",
    summary: "Checks the packages each app installs against published advisories.",
    why: "Run Warn for a couple of weeks before Refuse, so the first sign isn’t a failed deploy. When off, nothing is downloaded.",
    keywords:
      "dependencies advisory advisories CVE GHSA malware supply chain vulnerability npm pypi package lockfile osv scanning third-party warn block",
    route: "/admin/dependencies",
    anchor: "policy",
  },

  {
    id: "source_policy",
    label: "Source policy",
    summary: "Which repositories this install deploys from, and whether uploads may become apps.",
    keywords:
      "git repository allowlist approved sources trusted supply chain github gitlab bitbucket host organization pattern provenance restrict block impact preview",
    route: "/admin/sources",
    anchor: "sources",
    composite: true,
  },
  {
    id: "source_uploads_enabled",
    label: "Archive uploads",
    summary: "Whether a folder uploaded from a browser may become an app.",
    keywords: "upload zip archive folder deploy provenance",
    route: "/admin/sources",
    anchor: "sources",
    control: "source_uploads_enabled",
  },
  {
    id: "git_connections",
    label: "Connected git hosts",
    summary: "Connect your git host once, so apps don’t each need their own token.",
    keywords:
      "git connection github gitlab app installation token pat credential clone private repository picker webhook enterprise ghes coverage pasted offboarding leaver",
    route: "/admin/sources",
    anchor: "connections",
    composite: true,
  },

  {
    id: "job_log_retention_days",
    label: "Keep job logs for",
    summary: "A background job’s log lines.",
    why: "The run itself is kept after its logs are deleted.",
    keywords: "retention history delete sweep",
    route: "/admin/settings",
    anchor: "retention",
  },
  {
    id: "job_run_retention_days",
    label: "Keep job runs for",
    summary: "Job runs and their output files.",
    keywords: "retention history output download delete",
    route: "/admin/settings",
    anchor: "retention",
  },
  {
    id: "app_event_retention_days",
    label: "Keep failure events for",
    summary: "Failures nobody caused: failed builds, failed health checks, runs that couldn’t start.",
    why: "These are the System half of Activity. The audit trail of what people did is kept on its own setting, below.",
    keywords: "activity events failures history retention delete sweep audit",
    route: "/admin/settings",
    anchor: "retention",
  },

  {
    id: "platform_log_retention_days",
    label: "Keep the platform’s own log for",
    summary: "What Toft logged about each app: fetches, starts, repairs and refusals.",
    why: "This is what stays on an app’s Logs tab after its workload is gone. Keep it at least as long as failure events.",
    keywords:
      "platform log narration app logs retention delete sweep start repair",
    route: "/admin/settings",
    anchor: "retention",
  },

  {
    id: "announcement_recipient_retention_days",
    label: "Keep announcement recipients for",
    summary: "Who each announcement was mailed to, and what happened to each send.",
    why: "Only the per-person list is pruned. The announcement and its counts are kept.",
    keywords:
      "announcement email recipients retention delete sweep who was told",
    route: "/admin/settings",
    anchor: "retention",
  },

  {
    id: "access_record_retention_days",
    label: "Keep the access record for",
    summary: "The audit trail and who opened each app. 0 keeps everything, the default.",
    why: "Per-request access lines are your log collector’s to keep. Shortening an old install’s record takes hours, and every pass is logged in Activity.",
    keywords:
      "audit trail activity log access record who opened viewers usage retention delete prune sweep compliance au-11 year",
    route: "/admin/settings",
    anchor: "retention",
  },

  {
    id: "max_versions_per_app",
    label: "Most deployments an app may keep",
    summary: "The most deployments one app keeps. 0 means no limit.",
    why: "With cleanup on, older deployments are pruned on deploy. With cleanup off, the next deploy is refused at the limit. Rollback is never refused.",
    keywords:
      "releases builds versions retention rollback cleanup prune disk artifact",
    route: "/admin/settings",
    anchor: "retention",
  },

  {
    id: "announcement",
    label: "Announcement banner",
    summary: "A line across the top of every page.",
    keywords:
      "announcement banner maintenance notice broadcast message downtime",
    route: "/admin/messages",
    anchor: "announcement",
    control: "announcement-message",
    composite: true,
  },
  {
    id: "announcement_emails",
    label: "Announcements",
    summary: "Email a message to the people on this install.",
    keywords:
      "announcement email send broadcast everyone publishers recipients outage maintenance notify skipped un-send recall",
    route: "/admin/messages",
    anchor: "announcement-emails",
    composite: true,
  },
  {
    id: "knowledge_base",
    label: "Knowledge base",
    summary: "One app linked from the header on every page.",
    keywords:
      "knowledge base handbook wiki guides documentation style guide faq intranet manual header link description caption",
    route: "/admin/messages",
    anchor: "knowledge-base",
    control: "knowledge-base-app",
    composite: true,
  },
  {
    id: "locked_account_message",
    label: "Locked account message",
    summary: "What someone sees when their account is locked.",
    keywords: "locked disabled access message refusal sign-in wording copy blank default reset",
    route: "/admin/messages",
    anchor: "access",
    control: "locked-message",
    composite: true,
  },
  {
    id: "no_account_message",
    label: "No account message",
    summary: "What someone sees when they sign in but no account can be created.",
    keywords:
      "no account provisioning refused access message sign-in wording copy blank default reset",
    route: "/admin/messages",
    anchor: "access",
    control: "no-account-message",
    composite: true,
  },
  {
    id: "pending_activation_message",
    label: "Awaiting-activation message",
    summary: "What someone sees while their account waits to be activated.",
    keywords:
      "pending activation queue approve provisioning waiting access message sign-in wording copy blank default reset",
    route: "/admin/messages",
    anchor: "access",
    control: "pending-activation-message",
    composite: true,
  },
  {
    id: "account_activated_message",
    label: "Account-activated message",
    summary: "The email someone gets when you activate their account.",
    keywords:
      "activated approved welcome account email notification message wording copy blank default reset",
    route: "/admin/messages",
    anchor: "access",
    control: "account-activated-message",
    composite: true,
  },
  {
    id: "app_locked_message",
    label: "Locked app message",
    summary: "What a visitor sees when an app is locked. Each app can override it.",
    keywords:
      "locked app disabled unavailable message refusal wording copy decommissioned retired blank default reset",
    route: "/admin/messages",
    anchor: "access",
    control: "app-locked-message",
    composite: true,
  },

  /* ── Admin → Authentication ──────────────────────────────────────── */

  {
    id: "sign_in_providers",
    label: "Sign-in providers",
    summary: "Your identity provider, who it lets in, and which social logins are offered.",
    keywords:
      "oidc saml sso okta entra azure google github social login issuer client secret scopes discovery provider auto-provision role provisioning auto-create",
    route: "/admin/authentication",
    anchor: "sso",
    composite: true,
  },
  {
    id: "local_admin_enabled",
    label: "Built-in administrator",
    summary: "A local username and password for setup, and for recovery when sign-in breaks.",
    keywords: "local sign-in admin password bootstrap recovery break-glass login",
    route: "/admin/authentication",
    anchor: "local",
    composite: true,
  },

  /* ── Admin → Dependencies ────────────────────────────────────────── */

  {
    id: "dependency_block_malware",
    label: "Refuse malicious packages",
    summary: "Refuses a deploy that installs a known malicious package, whatever its score.",
    keywords: "malware malicious typosquat MAL supply chain",
    route: "/admin/dependencies",
    anchor: "policy",
  },
  {
    id: "dependency_block_severity",
    label: "Threshold",
    summary: "Findings at or above this severity count against an app, and under Refuse refuse its deploy.",
    why: "Advisories with no score never meet it. They are counted separately.",
    keywords:
      "severity critical high moderate low threshold CVSS unscored",
    route: "/admin/dependencies",
    anchor: "policy",
  },
  {
    id: "dependency_block_only_fixable",
    label: "Only refuse when a fix has been published",
    summary: "Refuses only findings with a published fix. The rest are recorded.",
    keywords: "fixable patched upgrade available advisory",
    route: "/admin/dependencies",
    anchor: "advanced",
  },
  {
    id: "dependency_scan_on_deploy",
    label: "Record packages at deploy",
    summary: "Records the packages each build installs. Refuse needs this on.",
    keywords: "scan deploy build resolve lockfile packages",
    route: "/admin/dependencies",
    anchor: "advanced",
  },
  {
    id: "dependency_watch_enabled",
    label: "Check serving apps in the background",
    summary: "Checks serving apps against new advisories and reports what it finds.",
    why: "A new finding never stops a serving app unless the lock below is on.",
    keywords: "watch background recheck advisory new finding serving",
    route: "/admin/dependencies",
    anchor: "advanced",
  },
  {
    id: "dependency_scan_interval_hours",
    label: "Check apps every",
    summary: "How often serving apps are checked against the advisory data already here.",
    keywords: "sweep interval recheck hours watch schedule serving",
    route: "/admin/dependencies",
    anchor: "advanced",
  },
  {
    id: "dependency_scan_history_kept",
    label: "Checks kept per app",
    summary: "How many readings each app keeps. Lowering it drops the oldest for good.",
    keywords: "history retention readings scans keep count governance",
    route: "/admin/dependencies",
    anchor: "advanced",
  },
  {
    id: "dependency_lock_on",
    label: "Lock a serving app when it installs",
    summary: "Takes a serving app offline by itself when a match is found. Malware by default.",
    keywords: "lock stop malware automatic consequence quarantine serving off",
    route: "/admin/dependencies",
    anchor: "policy",
  },
  {
    id: "dependency_feed_endpoint",
    label: "Feed address",
    summary: "Where advisory data comes from. Use an internal mirror if outbound access is closed.",
    keywords:
      "osv feed mirror advisory database endpoint url offline air-gap egress",
    route: "/admin/dependencies",
    anchor: "feed",
  },
  {
    id: "dependency_feed_interval_hours",
    label: "Refresh advisory data every",
    summary: "How often the advisory data is refreshed.",
    keywords: "interval refresh hours schedule feed fetch",
    route: "/admin/dependencies",
    anchor: "feed",
  },
  {
    id: "dependency_feed_max_age_hours",
    label: "Mark data stale after",
    summary: "Past this the data is marked stale. Stale data never refuses a deploy.",
    keywords: "stale age hours feed freshness treat",
    route: "/admin/dependencies",
    anchor: "feed",
  },
  {
    id: "dependency_feed_credential",
    label: "Mirror credential",
    summary: "Only for a private mirror, as key=value,key=value. Write-only.",
    keywords: "credential token mirror basic auth header",
    route: "/admin/dependencies",
    anchor: "feed",
  },
  {
    id: "dependency_waiver_max_days",
    label: "Waivers may run for",
    summary: "The longest a waiver may last. 0 turns waivers off.",
    keywords: "waiver exception expiry days temporary accept risk longest",
    route: "/admin/dependencies",
    anchor: "advanced",
  },

  /* ── Admin → Email ───────────────────────────────────────────────── */

  {
    id: "email_enabled",
    label: "Send email",
    summary: "When off, no notification is sent and nothing fails.",
    keywords: "smtp mail notifications outbound optional",
    route: "/admin/email",
    anchor: "transport",
    control: "email-enabled",
  },
  {
    id: "email_transport",
    label: "Transport",
    summary: "SMTP, or an HTTPS sending API where outbound port 587 is blocked.",
    keywords: "smtp api transport sendgrid postmark ses resend port 587 443",
    route: "/admin/email",
    anchor: "transport",
    control: "email-transport",
  },
  {
    id: "email_api_provider",
    label: "Sending API provider",
    summary: "Which sending API, when the transport is one.",
    keywords: "resend provider sending api",
    route: "/admin/email",
    anchor: "transport",
    control: "email-provider",
  },
  {
    id: "email_host",
    label: "SMTP host",
    summary: "The mail server this install hands messages to.",
    keywords: "smtp host server relay mx",
    route: "/admin/email",
    anchor: "transport",
    control: "email-host",
  },
  {
    id: "email_port",
    label: "SMTP port",
    summary: "587 for STARTTLS, 465 for implicit TLS.",
    keywords: "smtp port 587 465 25",
    route: "/admin/email",
    anchor: "transport",
    control: "email-port",
  },
  {
    id: "email_security",
    label: "Transport security",
    summary: "STARTTLS, implicit TLS, or none.",
    keywords: "starttls tls ssl encryption smtp security",
    route: "/admin/email",
    anchor: "transport",
    control: "email-security",
  },
  {
    id: "email_username",
    label: "SMTP username",
    summary:
      "The account this install authenticates as. Blank for a relay that needs no credential.",
    keywords: "smtp username login auth relay",
    route: "/admin/email",
    anchor: "transport",
    control: "email-username",
  },
  {
    id: "email_password",
    label: "SMTP password",
    summary: "Write-only. Leaving it blank keeps the stored password.",
    keywords: "smtp password secret credential auth",
    route: "/admin/email",
    anchor: "transport",
    control: "email-password",
  },
  {
    id: "email_api_key",
    label: "Sending API key",
    summary: "The sending service’s API key. Kept apart from the SMTP password.",
    keywords: "api key token sendgrid postmark ses resend secret",
    route: "/admin/email",
    anchor: "transport",
    control: "email-api-key",
  },
  {
    id: "email_skip_verify",
    label: "Accept a self-signed certificate",
    summary: "For an internal relay whose certificate can’t be verified. A workaround, not a fix.",
    keywords: "tls certificate verify self-signed insecure relay skip verification insecure",
    route: "/admin/email",
    anchor: "transport",
    control: "email-skip-verify",
  },
  {
    id: "email_from_address",
    label: "From address",
    summary: "Who mail from this install appears to be from.",
    keywords: "from sender address envelope identity",
    route: "/admin/email",
    anchor: "identity",
    control: "email-from",
  },
  {
    id: "email_from_name",
    label: "From name",
    summary: "The display name beside the address.",
    keywords: "from name display sender identity",
    route: "/admin/email",
    anchor: "identity",
    control: "email-from-name",
  },
  {
    id: "email_reply_to",
    label: "Reply-to",
    summary: "Where replies go, if not the from address.",
    keywords: "reply-to responses inbox",
    route: "/admin/email",
    anchor: "identity",
    control: "email-reply-to",
  },
  {
    id: "email_notify_admin",
    label: "Administrator notifications",
    summary: "Mail to system administrators about this install.",
    keywords: "notifications administrators publisher requests audience",
    route: "/admin/email",
    anchor: "audiences",
    control: "email-notify-admin",
  },
  {
    id: "email_admin_group_address",
    label: "Administrator notification address",
    summary: "Send administrator notifications to one list instead of each administrator.",
    keywords: "distribution list group alias administrators mailing",
    route: "/admin/email",
    anchor: "audiences",
    control: "email-admin-group",
  },
  {
    id: "email_notify_app",
    label: "App and access notifications",
    summary: "Mail to people about their own apps and access.",
    keywords: "notifications people apps access approval denial audience",
    route: "/admin/email",
    anchor: "audiences",
    control: "email-notify-app",
  },

  /* ── Admin → Telemetry ───────────────────────────────────────────── */

  {
    id: "telemetry_enabled",
    label: "Export telemetry",
    summary: "When off, nothing is exported and nothing fails.",
    keywords: "otel opentelemetry export traces metrics observability collector",
    route: "/admin/telemetry",
    anchor: "export",
    control: "telemetry-enabled",
  },
  {
    id: "telemetry_endpoint",
    label: "Collector endpoint",
    summary: "The collector’s base URL.",
    keywords: "otlp collector endpoint url grpc http exporter",
    route: "/admin/telemetry",
    anchor: "export",
    control: "telemetry-endpoint",
  },
  {
    id: "telemetry_headers",
    label: "Credential",
    summary: "Write-only headers for a collector that needs authentication.",
    keywords: "otlp headers credential token authorization secret",
    route: "/admin/telemetry",
    anchor: "export",
    control: "telemetry-headers",
  },
  {
    id: "telemetry_traces",
    label: "Traces",
    summary: "Whether spans are exported.",
    keywords: "traces spans tracing otlp",
    route: "/admin/telemetry",
    anchor: "signals",
    control: "telemetry-traces",
  },
  {
    id: "telemetry_metrics",
    label: "Metrics",
    summary: "Whether metric batches are exported.",
    keywords: "metrics counters gauges otlp",
    route: "/admin/telemetry",
    anchor: "signals",
    control: "telemetry-metrics",
  },
  {
    id: "telemetry_sample_ratio",
    label: "Trace sampling",
    summary: "Share of traces to keep, from 0 to 1.",
    keywords: "sampling ratio head sample rate traces cardinality budget",
    route: "/admin/telemetry",
    anchor: "signals",
    control: "telemetry-ratio",
  },
  {
    id: "telemetry_temporality",
    label: "Metric temporality",
    summary: "Cumulative or delta, whichever your collector expects.",
    keywords: "temporality cumulative delta prometheus otlp metrics",
    route: "/admin/telemetry",
    anchor: "signals",
    control: "telemetry-temporality",
  },

  /* ── Admin → Integrations ────────────────────────────────────────── */

  {
    id: "integration_connections",
    label: "Connections",
    summary: "One credential for one place. Each connection is attached to apps separately.",
    keywords:
      "integration connection slack teams teamsbot bot discord sharepoint onedrive gdrive google drive gchat google chat pagerduty webhook credential scope workspace site channel folder attach access token tokens sdk graph",
    route: "/admin/integrations",
    anchor: "connections",
    composite: true,
  },
  {
    id: "integration_catalog",
    label: "What you can connect",
    summary: "Everything this version can send to.",
    keywords: "integration catalog kinds available connect add integration list webhook",
    route: "/admin/integrations",
    anchor: "catalog",
    composite: true,
  },
  {
    id: "integrations_retain_bodies",
    label: "Keep message bodies",
    summary: "When off, the log keeps a checksum of each message, not its text.",
    keywords: "bodies retention integration log privacy checksum message content",
    route: "/admin/integrations",
    anchor: "log",
    control: "integrations-retain-bodies",
  },
  {
    id: "integrations_retention_days",
    label: "Keep the integration log for",
    summary: "At least 30 days.",
    keywords: "retention integration log days sweep delete",
    route: "/admin/integrations",
    anchor: "log",
    control: "retention-days",
  },

  /* ── Admin → Features ────────────────────────────────────────────── */

  {
    id: "feature_switches",
    label: "Optional features",
    summary: "What this build offers, and what is switched on.",
    keywords:
      "features optional capabilities enable switch",
    route: "/admin/features",
    anchor: "features",
    composite: true,
  },
] as const satisfies readonly SettingEntry[];

export const SETTINGS_INDEX: readonly SettingEntry[] = ENTRIES;

export type SettingEntryId = (typeof ENTRIES)[number]["id"];

/**
 * Looked up by the name the API uses.
 *
 * Typed on the union of ids the index actually declares, so a page that renders
 * a setting the index has never heard of does not compile — the first half of
 * the guard, paid at build time rather than in a test.
 */
export const SETTING = Object.fromEntries(
  SETTINGS_INDEX.map((entry) => [entry.id, entry]),
) as Record<SettingEntryId, SettingEntry>;

/* ------------------------------------------------------------------ *
 * The Settings page's sections
 *
 * A section declares its own words; which settings it holds is derived
 * from the index, by anchor. That is what stops a reordering from
 * putting one section's rows under another's heading, and what makes
 * "this setting is in a section that does not exist" impossible rather
 * than merely unlikely.
 * ------------------------------------------------------------------ */

export interface SettingsSection {
  id: string;
  title: string;
  blurb: string;
  /** For sections whose content is not a list of settings rows. */
  keywords?: string;
  /** Rendered under the rows, in the section's own voice. */
  footnote?: string;
}

export const SETTINGS_SECTIONS: SettingsSection[] = [
  {
    id: "address",
    title: "This install",
    blurb:
      "Where people reach it, and which environment it is.",
  },
  {
    id: "security",
    title: "Security",
    blurb:
      "What this install tells researchers, scanners and browsers.",
    keywords:
      "security.txt rfc 9116 disclosure vulnerability researcher report contact expiry policy well-known headers hsts nosniff",
  },
  {
    id: "execution",
    title: "Execution",
    blurb:
      "Where apps run and what they may use.",
  },
  {
    id: "jobs",
    title: "Jobs",
    blurb:
      "Limits on the jobs apps declare in toft.toml.",
    keywords: "jobs automation schedule cron queue placement cluster machine watchdog cgroup agent namespace",
    footnote:
      "Lowering a ceiling caps jobs already deployed above it, and their apps are told.",
  },
  {
    id: "retention",
    title: "Retention",
    blurb:
      "How long history is kept.",
    keywords:
      "retention history sweep delete prune disk space storage age days keep logs runs versions incident forensics",
  },
  {
    id: "gallery",
    title: "Gallery",
    blurb:
      "Extensions and examples published by the Toft team.",
    keywords: "catalog sync refresh mirror address extensions examples samples",
  },
];

/**
 * Where a section of the old Settings page went.
 *
 * Six of its thirteen sections moved to the page that shows what they govern
 * (ADR §50 D2), and a link is a promise: `/admin/settings#accounts` is in
 * documentation, in other pages' copy, and in whatever an administrator
 * bookmarked. The Settings page reads this and redirects rather than landing
 * somebody at the top of a page that no longer holds what they came for.
 *
 * The precedent is `LEGACY_TABS` and the `<Navigate>` pair for
 * `/admin/job-runs` and `/admin/scheduled` (ADR §33).
 */
export const MOVED_ANCHORS: Record<string, string> = {
  // The scheduled-task limits went with tasks; the job limits took their place.
  scheduled: "/admin/settings#jobs",
  license: "/admin/features",
  install: "/admin/info#install",
  accounts: "/admin/authentication#dormant",
  access: "/admin/api-keys#personal-keys",
  dependencies: "/admin/dependencies#policy",
  sources: "/admin/sources#sources",
  messages: "/admin/messages#announcement",
  // The environment name joined the install's address: both say which
  // install this is.
  environment: "/admin/settings#address",
};

/** Everything one page renders, in index order. */
export function settingsOn(route: string): SettingEntry[] {
  return SETTINGS_INDEX.filter((entry) => entry.route === route);
}

/** The settings a Settings-page section holds, in index order. */
export function settingsIn(sectionId: string): SettingEntry[] {
  return SETTINGS_INDEX.filter(
    (entry) => entry.route === "/admin/settings" && entry.anchor === sectionId,
  );
}

/** Every word a search should match against. Both scopes read it. */
export function settingText(entry: {
  label: string;
  summary: string;
  why?: string;
  keywords?: string;
}): string {
  return `${entry.label} ${entry.summary} ${entry.why ?? ""} ${entry.keywords ?? ""}`;
}

/**
 * Every setting matching `query`, install-wide.
 *
 * `exclude` is how a page's own filter asks for "the ones that are somewhere
 * else": the Settings box has already shown its own matches, and repeating
 * them under "3 matches on other pages" would be a lie about where they are.
 */
export function searchSettings(
  query: string,
  exclude?: string,
): SettingEntry[] {
  const q = query.trim().toLowerCase();
  if (!q) return [];
  return SETTINGS_INDEX.filter(
    (entry) =>
      entry.route !== exclude && settingText(entry).toLowerCase().includes(q),
  );
}

/**
 * What to call the page a setting lives on.
 *
 * Read from `adminGroups` rather than stored beside each entry: the sidebar's
 * list is already the one list of admin destinations, and a second copy of a
 * page's name would drift the first time one is renamed.
 */
export function settingPageLabel(route: string): string {
  const item = adminGroups
    .flatMap((group) => group.items)
    .find((i) => i.href === route);
  return item ? item.label : route;
}

/** Where a setting is, as a link target. */
export function settingHref(entry: SettingEntry): string {
  return `${entry.route}#${entry.anchor}`;
}

/* ------------------------------------------------------------------ *
 * The settings index — one app
 *
 * The same idea one level down, and it is a second table rather than
 * more rows in the first because the two are not the same kind of
 * thing. An install setting is one row in `settings` with one value; an
 * app setting is a column on `apps` with a value **per app**, and an
 * install with forty apps has forty copies of every entry below. That
 * difference is why these carry a `tab` and an `anchor` instead of a
 * `route`, and why the palette offers them only for an app already in
 * scope: every setting × every app would drown every other group in it.
 *
 * The gap this closes is the one ADR §50 was written to close, still
 * open one level down. `lib/settings-index.ts` said in its first line
 * that it held *every setting an administrator can change* — and auto-
 * deploy, idle sleep, versions to keep, execution mode, failure
 * notifications, visibility, the tracked branch and an environment
 * variable were none of them findable by name anywhere. The app's
 * Settings tab had no filter of its own either, so it was not that the
 * answer was scoped too narrowly; there was no answer at all.
 *
 * Three consumers again, and the third is again the one that changes an
 * answer:
 *
 *   1. The Settings tab's filter box, which it did not have.
 *   2. The command palette, for the apps already in scope — the page
 *      you are on and the Recents list `lib/recents.ts` keeps.
 *   3. A filter that comes up empty, saying which *other tab of this
 *      app* holds what was asked for: type `visibility` on Settings and
 *      be told it is on Access.
 * ------------------------------------------------------------------ */

/**
 * The tabs of the app detail page the index knows about.
 *
 * Settings and Access render settings. Overview renders none — it is here
 * because a *section* can live there: Storage is a read-only fact about the
 * app, and the index has to be able to name where it went so a search for
 * "mount path" and an old `#storage` link both land on it.
 */
export type AppTab = "settings" | "access" | "overview" | "reach";

export interface AppSettingEntry {
  /** The name the API uses on `PATCH /apps/{id}`. Unique across the app. */
  id: string;
  label: string;
  /** One line, always visible. */
  summary: string;
  /** Words someone might search for that appear in neither of the above. */
  keywords?: string;
  /** The tab that renders it. */
  tab: AppTab;
  /** The `id` of the element on that tab to scroll to. */
  anchor: string;
  /** The `id` of the control itself, when it differs from the setting's name. */
  control?: string;
  /**
   * Not a single labelled control: a list of rows, a family of tag pickers, a
   * three-way choice drawn as cards. The guard looks for the anchor and not
   * for a control, exactly as it does install-wide.
   */
  composite?: true;
}

const APP_ENTRIES = [
  /* ── Settings tab ────────────────────────────────────────────────── */

  {
    id: "name",
    label: "App name",
    summary: "What people see this app called.",
    keywords: "title rename label display",
    tab: "settings",
    anchor: "identity",
    control: "app-name",
  },
  {
    id: "slug",
    label: "Slug",
    summary: "The last part of this app’s address.",
    keywords: "url address path rename link bookmark process name redirect",
    tab: "settings",
    anchor: "identity",
    control: "app-slug",
  },
  {
    id: "tags",
    label: "Tags",
    summary: "This app’s value for each tag an administrator defined. Saved as you pick.",
    keywords: "tag label metadata category owner team environment classify",
    tab: "settings",
    anchor: "identity",
    composite: true,
  },

  {
    id: "branch",
    label: "Tracked branch",
    summary: "Redeploys and auto-deploys build the tip of this branch.",
    keywords: "git branch main master tip track deploy",
    tab: "settings",
    anchor: "source",
    control: "app-branch",
  },
  {
    id: "repo_subdir",
    label: "Folder",
    summary: "The folder of the repository this app is built from. Blank is the root.",
    keywords: "folder subdirectory directory path monorepo root repository",
    tab: "settings",
    anchor: "source",
    control: "app-repo-subdir",
  },
  {
    id: "auto_deploy",
    label: "Auto-deploy on push",
    summary: "Checks the tracked branch about once a minute and deploys new commits.",
    keywords:
      "auto deploy push webhook poll continuous commits gallery catalog",
    tab: "settings",
    anchor: "source",
    control: "auto-deploy",
  },
  {
    id: "git_token",
    label: "Git access token",
    summary: "Needed for a private repository. Write-only; leave blank to keep the stored one.",
    keywords:
      "credential token private repository github gitlab bitbucket pat clone secret encrypted",
    tab: "settings",
    anchor: "source",
    control: "git-token",
  },
  {
    id: "render_schedule",
    label: "Re-render on a schedule",
    summary: "Declared as [render] in toft.toml; paused here until the next deploy. Each re-render is a full deploy.",
    keywords: "notebook render cron schedule rebuild report quarto rmarkdown r markdown refresh",
    tab: "settings",
    anchor: "source",
    control: "render-schedule",
  },
  {
    id: "render_recipients",
    label: "Who gets the re-rendered report",
    summary: "People emailed a link each time the report re-renders. Never the report itself.",
    keywords:
      "notebook report recipients email send subscribers deliver attachment link",
    tab: "settings",
    anchor: "source",
    control: "report-recipient-search",
  },

  {
    id: "execution_mode",
    label: "Isolated execution",
    summary: "Runs this app as its own workload instead of inside Toft’s.",
    keywords: "isolated shared workload promote sandbox namespace jobs runner background",
    tab: "settings",
    anchor: "runtime",
    control: "execution-mode",
  },
  {
    id: "hostname",
    label: "Hostname",
    summary: "A DNS name of this app’s own, served at its root. Set by an administrator.",
    keywords: "hostname domain dns vanity address origin url custom subdomain",
    tab: "settings",
    anchor: "identity",
    control: "app-hostname",
  },
  {
    id: "hostname_enabled",
    label: "Serve at this hostname",
    summary: "Serves this app at its hostname, with the old address redirecting.",
    keywords: "hostname domain dns vanity serve switch redirect custom subdomain",
    tab: "settings",
    anchor: "identity",
    control: "app-hostname-enabled",
  },
  {
    id: "never_sleep",
    label: "Never sleep",
    summary: "Keeps this app running between visits instead of sleeping when idle.",
    keywords: "sleep idle hibernate wake exempt always-on cold start",
    tab: "settings",
    anchor: "runtime",
    control: "never-sleep",
  },
  {
    id: "restart_schedule",
    label: "Restart on a schedule",
    summary: "Declared as [restart] in toft.toml; paused here until the next deploy. Restarts only a running app.",
    keywords: "restart bounce nightly cron schedule leak recycle reboot",
    tab: "settings",
    anchor: "runtime",
    control: "restart-schedule",
  },
  {
    id: "memory_limit_mb",
    label: "Memory limit",
    summary: "Memory this app may use before it is restarted. Blank uses the install default.",
    keywords: "memory limit RAM restart killed oom cap per-app MB GB gigabytes",
    tab: "settings",
    anchor: "runtime",
    control: "memory-limit",
  },
  {
    id: "cpu_limit_millicores",
    label: "CPU limit",
    summary: "The most CPU time this app may use. Blank uses the install default.",
    keywords: "cpu limit throttle cores millicores per-app cpu.max",
    tab: "settings",
    anchor: "runtime",
    control: "cpu-limit",
  },
  {
    id: "viewer_identity",
    label: "Can see who is viewing",
    summary: "Lets this app ask for the name, email and groups of the person viewing it.",
    keywords:
      "viewer identity who toft.who whoami sso user email groups role disclose personalise withheld",
    tab: "settings",
    anchor: "runtime",
    control: "viewer-identity",
  },
  {
    id: "auto_delete_releases",
    label: "Delete old deployments automatically",
    summary: "Each deploy deletes deployments beyond the number kept, with their build logs.",
    keywords:
      "cleanup prune releases versions disk artifact retention build record log deployments rollback",
    tab: "settings",
    anchor: "runtime",
    control: "auto-delete-releases",
  },
  {
    id: "releases_kept",
    label: "Deployments to keep",
    summary: "Deployments kept after a deploy, counting the live one. Blank uses the default.",
    keywords: "deployments versions releases builds keep retention rollback count history",
    tab: "settings",
    anchor: "runtime",
    control: "releases-kept",
  },
  {
    id: "env_vars",
    label: "Environment variables",
    summary: "Variables this app starts with. Sensitive values are write-only once saved.",
    keywords: "env environment variables secrets config dotenv values keys",
    tab: "settings",
    anchor: "runtime",
    composite: true,
  },

  {
    id: "notify_failures",
    label: "Email this app’s owners and editors when it breaks",
    summary: "Email on a failed auto-deploy, a broken git token, a crash or a failed job run.",
    keywords:
      "notification email alert failure crash breaks notify owners credential",
    tab: "settings",
    anchor: "notifications",
    control: "notify-failures",
  },

  /* ── Access tab ──────────────────────────────────────────────────── */

  {
    id: "visibility",
    label: "Who can open it",
    summary: "Only listed people and groups, anyone signed in, or anyone at all.",
    keywords:
      "visibility private restricted public authenticated logged-in share who can see access",
    tab: "access",
    anchor: "visibility",
    composite: true,
  },
  {
    id: "app-listable",
    label: "Show this app in everyone\u2019s list",
    summary: "Whether everyone signed in can see that this app exists, without opening it.",
    keywords:
      "list listing listable hide hidden discoverable directory catalog unlisted browse find name slug rung",
    tab: "access",
    anchor: "visibility",
    control: "app-listable",
  },
  {
    id: "platform_lock",
    label: "Platform lock",
    summary: "An administrator’s hold: nobody may open this app, whatever its visibility.",
    keywords:
      "lock locked disable disabled suspend retire decommission unused dormant quiet block shut off",
    tab: "access",
    anchor: "lock",
    composite: true,
  },
  {
    id: "embedding",
    label: "Embedding",
    summary: "Which other sites may show this app in a frame.",
    keywords:
      "embed embedding iframe frame frame-ancestors clickjacking csp intranet portal wrap inline origin",
    tab: "access",
    anchor: "embedding",
    control: "embed-origin",
  },
] as const satisfies readonly AppSettingEntry[];

export const APP_SETTINGS_INDEX: readonly AppSettingEntry[] = APP_ENTRIES;

export type AppSettingEntryId = (typeof APP_ENTRIES)[number]["id"];

/** Looked up by the name the API uses, typed on the ids the index declares. */
export const APP_SETTING = Object.fromEntries(
  APP_SETTINGS_INDEX.map((entry) => [entry.id, entry]),
) as Record<AppSettingEntryId, AppSettingEntry>;

/* ------------------------------------------------------------------ *
 * The sections of an app's tabs
 *
 * Six groups on Settings, named for **what you are deciding** rather
 * than for what each control happens to be — the axis ADR §50 D4 used
 * on the admin sidebar, applied inside a page. Fourteen sections of
 * which seven held exactly one control was the finding; a section costs
 * a heading, a blurb, a card and its padding, and on more than half of
 * them the framing was taller than the thing it framed.
 *
 * Several hold no setting at all: Data is a read-only mapping and a way
 * out, the Danger zone is three actions, API keys is a credential list
 * — an app key is a *grant*, which is why it is on Access. `settings:
 * false` is how one says so, in the table rather than in an allow-list
 * inside the test, because a section that holds no setting by design is
 * a design statement and a section that holds none by accident is the
 * bug the guard exists to catch.
 * ------------------------------------------------------------------ */

export interface AppSettingsSection {
  id: string;
  /** Which tab renders it. Only the Settings tab's are filtered and railed. */
  tab: AppTab;
  title: string;
  blurb: string;
  keywords?: string;
  /** False for a section of read-only facts or actions rather than settings. */
  settings?: false;
}

export const APP_SETTINGS_SECTIONS: AppSettingsSection[] = [
  {
    id: "identity",
    tab: "settings",
    title: "Identity",
    blurb: "What this app is called, by people and by machines.",
    keywords: "name slug id tags rename address identifier label",
  },
  {
    id: "source",
    tab: "settings",
    title: "Source & deploys",
    blurb: "Where the code comes from, and what makes it change.",
    keywords:
      "repository git branch folder subdirectory auto-deploy token push origin clone upload zip gallery catalog extension example update unlink",
  },
  {
    id: "runtime",
    tab: "settings",
    title: "Runtime",
    blurb: "How it runs, and what it runs with.",
    keywords:
      "execution isolated shared workload sleep idle hibernate environment variables secrets versions releases retention rollback",
  },
  {
    id: "notifications",
    tab: "settings",
    title: "Notifications",
    blurb: "Who hears about it when this app breaks, and where.",
    keywords: "email alert notify failure crash owners slack channel post",
  },
  {
    id: "data",
    tab: "settings",
    title: "Data",
    blurb: "What survives a release.",
    keywords: "saved data document json clear reset toft.data",
    settings: false,
  },

  /* ── Overview ────────────────────────────────────────────────────── */

  {
    id: "storage",
    tab: "overview",
    title: "Storage",
    blurb: "Durable paths an administrator has mapped into this app.",
    keywords: "storage volume object store mount path bucket efs s3 durable",
    settings: false,
  },
  {
    id: "danger",
    tab: "settings",
    title: "Danger zone",
    blurb: "Handing this app on, switching it off, and removing it.",
    keywords: "transfer ownership stop delete remove destroy",
    settings: false,
  },

  /* ── Access ──────────────────────────────────────────────────────── */

  {
    id: "visibility",
    tab: "access",
    title: "Who can open it",
    blurb:
      "Restricted to the people and groups listed, open to anyone signed in, or public.",
  },
  {
    id: "lock",
    tab: "access",
    title: "Platform lock",
    blurb: "An administrator’s hold on this app.",
    keywords: "lock locked retire suspend administrator hold unlock",
    settings: false,
  },
  {
    id: "embedding",
    tab: "access",
    title: "Embedding",
    blurb: "Which other sites may put this app inside a page of their own.",
    keywords: "iframe frame embed origin site frame-ancestors",
  },
  {
    id: "access-requests",
    tab: "access",
    title: "Waiting for you",
    blurb: "People who can see this app and have asked to open it.",
    keywords: "access request ask approve decline deny queue waiting list rung",
    settings: false,
  },
  {
    id: "people",
    tab: "access",
    title: "People and groups",
    blurb: "Who can open this app, and who can change it.",
    // `collaborator` stays alongside `editor`: somebody who learned the old
    // word must still find the setting.
    keywords: "grant share editor collaborator viewer role person group member",
    settings: false,
  },
];

/** Everything one tab renders, in index order. */
export function appSettingsOn(tab: AppTab): AppSettingEntry[] {
  return APP_SETTINGS_INDEX.filter((entry) => entry.tab === tab);
}

/** The sections one tab renders, in table order. */
export function appSectionsOn(tab: AppTab): AppSettingsSection[] {
  return APP_SETTINGS_SECTIONS.filter((section) => section.tab === tab);
}

/** The settings a section holds, in index order. */
export function appSettingsIn(section: AppSettingsSection): AppSettingEntry[] {
  return APP_SETTINGS_INDEX.filter(
    (entry) => entry.tab === section.tab && entry.anchor === section.id,
  );
}

/**
 * Every app setting matching `query`.
 *
 * `exclude` is the asking tab, for the same reason the install-wide search
 * takes one: the Settings filter has already shown its own matches, and
 * repeating them under "1 match on another tab" would be a lie about where
 * they are.
 */
export function searchAppSettings(
  query: string,
  exclude?: AppTab,
): AppSettingEntry[] {
  const q = query.trim().toLowerCase();
  if (!q) return [];
  return APP_SETTINGS_INDEX.filter(
    (entry) =>
      entry.tab !== exclude && settingText(entry).toLowerCase().includes(q),
  );
}

/** What to call the tab a setting lives on. */
export const APP_TAB_LABEL: Record<AppTab, string> = {
  settings: "Settings",
  access: "Access",
  overview: "Overview",
  // Reach renders no setting at all — it is a read of what the app was given,
  // and every row on it links to the tab that owns the thing. It is spelled
  // here so a link can name it; `APP_TABS` is derived from the index's own
  // entries, so a tab with none is never asked for a source.
  reach: "Reach",
};

/** Where an app setting is, as a link target. Overview owns the bare path, so
 *  every tab that renders a setting has a segment of its own. */
export function appSettingHref(appId: string, entry: AppSettingEntry): string {
  return `/apps/${appId}/${entry.tab}#${entry.anchor}`;
}

/**
 * Where a section of an app's Settings tab went.
 *
 * `MOVED_ANCHORS` one level down, and the same promise `LEGACY_TABS` makes for
 * a retired tab name: a link is a link. Fourteen sections became six groups and
 * one of them changed tab — **API keys is on Access now**, because an app key
 * `toft_` is a *grant*, and Access is the page that lists who may reach this app.
 * Keyed by the old anchor and applied only on the Settings tab, since that is
 * the only tab whose sections moved.
 */
export const APP_MOVED_ANCHORS: Record<
  string,
  { tab: AppTab; anchor: string }
> = {
  "api-keys": { tab: "access", anchor: "people" },
  general: { tab: "settings", anchor: "identity" },
  tags: { tab: "settings", anchor: "identity" },
  gallery: { tab: "settings", anchor: "source" },
  execution: { tab: "settings", anchor: "runtime" },
  versions: { tab: "settings", anchor: "runtime" },
  sleep: { tab: "settings", anchor: "runtime" },
  environment: { tab: "settings", anchor: "runtime" },
  slack: { tab: "settings", anchor: "notifications" },
  storage: { tab: "overview", anchor: "storage" },
  "saved-data": { tab: "settings", anchor: "data" },
};
