-- +goose Up
--
-- The Launchpad schema, in one file.
--
-- **Generated. Do not edit by hand.** A change to the schema is a migration
-- above this one. This file is rewritten only by a rebaseline, with
--
--     scripts/schema-snapshot.sh --baseline
--
-- and a rebaseline happens at a major version and nowhere else: it takes head's
-- own version number, never a new one and never 1, so an install that has
-- already run the chain reads it as applied and does nothing. The version it
-- carries is also the floor -- a database below it is refused at startup by
-- name rather than migrated. ADR § The schema is decided once and captured.
--
--
-- PostgreSQL database dump
--

--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;

--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: -
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';

--
-- Name: enforce_grant_has_bucket(); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.enforce_grant_has_bucket() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    resource_kind   TEXT;
    resource_bucket TEXT;
BEGIN
    SELECT kind, bucket INTO resource_kind, resource_bucket
      FROM storage_resources WHERE id = NEW.resource_id;
    IF resource_kind IS NULL THEN
        RAISE EXCEPTION 'store_grants: resource % does not exist', NEW.resource_id;
    END IF;
    IF COALESCE(resource_bucket, '') = '' THEN
        RAISE EXCEPTION 'store_grants: resource % names no bucket, so there is nothing to sign for — only a resource with objects behind it takes grants', NEW.resource_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

--
-- Name: enforce_group_membership_cap(); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.enforce_group_membership_cap() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (SELECT COUNT(*) FROM group_members WHERE user_id = NEW.user_id) >= 10 THEN
        RAISE EXCEPTION 'user belongs to maximum of 10 groups';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

--
-- Name: enforce_storage_manager_access(); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.enforce_storage_manager_access() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    resource_kind TEXT;
BEGIN
    SELECT kind INTO resource_kind FROM storage_resources WHERE id = NEW.resource_id;
    IF resource_kind IS NULL THEN
        RAISE EXCEPTION 'storage_managers: resource % does not exist', NEW.resource_id;
    END IF;
    IF resource_kind = 'volume' AND NEW.max_access NOT IN ('read', 'write') THEN
        RAISE EXCEPTION 'storage_managers: resource % is a volume, which offers read and write — % is not one of them',
            NEW.resource_id, NEW.max_access;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

--
-- Name: enforce_storage_mapping_authority(); Type: FUNCTION; Schema: public; Owner: -
--

-- +goose StatementBegin
CREATE FUNCTION public.enforce_storage_mapping_authority() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_subject_type TEXT;
    v_subject_id   TEXT;
    v_ceiling      TEXT;
    v_rank_of      INT;
    v_rank_max     INT;
BEGIN
    IF NEW.authority = 'admin' THEN
        RETURN NEW;
    END IF;
    v_subject_type := split_part(NEW.authority, ':', 1);
    v_subject_id   := substr(NEW.authority, length(v_subject_type) + 2);
    IF v_subject_type NOT IN ('user', 'group') OR v_subject_id = '' THEN
        RAISE EXCEPTION 'storage_mappings: authority % is not admin, user:<id> or group:<id>', NEW.authority;
    END IF;

    SELECT m.max_access INTO v_ceiling FROM storage_managers m
     WHERE m.resource_id = NEW.resource_id
       AND m.subject_type = v_subject_type
       AND m.subject_id = v_subject_id;
    IF v_ceiling IS NULL THEN
        RAISE EXCEPTION 'storage_mappings: % does not manage resource %', NEW.authority, NEW.resource_id;
    END IF;

    v_rank_of  := CASE NEW.access WHEN 'read' THEN 1 WHEN 'write' THEN 2 WHEN 'overwrite' THEN 3 WHEN 'full' THEN 4 ELSE 0 END;
    v_rank_max := CASE v_ceiling  WHEN 'read' THEN 1 WHEN 'write' THEN 2 WHEN 'overwrite' THEN 3 WHEN 'full' THEN 4 ELSE 0 END;
    IF v_rank_of > v_rank_max THEN
        RAISE EXCEPTION 'storage_mappings: % may attach resource % at % at most, not %',
            NEW.authority, NEW.resource_id, v_ceiling, NEW.access;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

--
-- Name: access_requests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.access_requests (
    id text NOT NULL,
    requester_id text,
    request_type text NOT NULL,
    object_id text,
    requested_role text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    approver_id text,
    decided_at timestamp with time zone,
    reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    approver_group_id text,
    CONSTRAINT access_requests_request_type_check CHECK ((request_type = ANY (ARRAY['object_access'::text, 'role_promotion'::text]))),
    CONSTRAINT access_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'denied'::text, 'withdrawn'::text])))
);

--
-- Name: TABLE access_requests; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.access_requests IS 'A request for access somebody has to decide: an object grant on one app, or a promotion up the system-role ladder. Routed rather than assigned — approver_id is empty until somebody acts, and approver_group_id is where the request went.';

--
-- Name: COLUMN access_requests.reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.access_requests.reason IS 'Somebody''s words, or the platform''s when a request is auto-denied. Not a vocabulary.';

--
-- Name: advisories; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.advisories (
    id text NOT NULL,
    source text DEFAULT ''::text NOT NULL,
    kind text DEFAULT 'vuln'::text NOT NULL,
    severity text DEFAULT ''::text NOT NULL,
    cvss_vector text DEFAULT ''::text NOT NULL,
    summary text DEFAULT ''::text NOT NULL,
    url text DEFAULT ''::text NOT NULL,
    published_at timestamp with time zone,
    withdrawn_at timestamp with time zone,
    snapshot_id text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE advisories; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.advisories IS 'One advisory, normalized out of whatever the feed shipped. The id is the upstream identifier (GHSA-, MAL-, PYSEC-, GO-) rather than a surrogate, because every other surface in this feature names an advisory by that string.';

--
-- Name: COLUMN advisories.source; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.advisories.source IS 'Where the feed was fetched from — a host, not a vocabulary. No CHECK.';

--
-- Name: COLUMN advisories.snapshot_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.advisories.snapshot_id IS 'Which feed snapshot brought this row in. Not a Launchpad reference: it names a fetch, and advisory_feed_state holds the current one per ecosystem.';

--
-- Name: advisory_affected; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.advisory_affected (
    id bigint NOT NULL,
    advisory_id text NOT NULL,
    ecosystem text NOT NULL,
    package text NOT NULL,
    introduced text DEFAULT ''::text NOT NULL,
    fixed text DEFAULT ''::text NOT NULL,
    last_affected text DEFAULT ''::text NOT NULL,
    versions text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE advisory_affected; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.advisory_affected IS 'One affected range; an advisory usually has several. Version bounds are stored as the strings the feed published, never parsed — npm semver, PEP 440 and Go semver each order versions differently, and a canonical numeric form here would be a fourth opinion computed once at sync time and impossible to fix without a re-sync.';

--
-- Name: COLUMN advisory_affected.introduced; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.advisory_affected.introduced IS 'The string the feed published, never parsed. npm semver, PEP 440 and Go semver each order versions differently, and a canonical form stored here would be a fourth opinion computed once at sync time.';

--
-- Name: COLUMN advisory_affected.fixed; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.advisory_affected.fixed IS 'The string the feed published, never parsed — see introduced.';

--
-- Name: advisory_affected_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.advisory_affected_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: advisory_affected_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.advisory_affected_id_seq OWNED BY public.advisory_affected.id;

--
-- Name: advisory_feed_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.advisory_feed_state (
    ecosystem text NOT NULL,
    endpoint text DEFAULT ''::text NOT NULL,
    last_sync_at timestamp with time zone,
    last_success_at timestamp with time zone,
    last_error text DEFAULT ''::text NOT NULL,
    snapshot_id text DEFAULT ''::text NOT NULL,
    etag text DEFAULT ''::text NOT NULL,
    advisory_count integer DEFAULT 0 NOT NULL,
    origin text DEFAULT 'sync'::text NOT NULL,
    snapshot_at timestamp with time zone
);

--
-- Name: TABLE advisory_feed_state; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.advisory_feed_state IS 'The feed state, one row per ecosystem rather than one for the whole feed, because the snapshots are fetched and swapped independently: npm succeeding while PyPI 404s must leave npm current and PyPI stale.';

--
-- Name: COLUMN advisory_feed_state.snapshot_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.advisory_feed_state.snapshot_id IS 'The snapshot currently in force for this ecosystem. Not a Launchpad reference.';

--
-- Name: allowed_git_sources; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.allowed_git_sources (
    id text NOT NULL,
    pattern text NOT NULL,
    note text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by text
);

--
-- Name: TABLE allowed_git_sources; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.allowed_git_sources IS 'The approved-source list: which git repositories this install will deploy from at all. Its own table rather than a blob in settings because an entry carries provenance and a note, usage is counted per entry, and the settings audit writes the whole document on every change — the diff is this feature''s compensating control, so it has to stay legible. The mode itself (git_source_policy) and the upload switch stay settings rows.';

--
-- Name: announcement_email_recipients; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.announcement_email_recipients (
    id text NOT NULL,
    announcement_id text NOT NULL,
    user_id text,
    login text DEFAULT ''::text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    status text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone
);

--
-- Name: TABLE announcement_email_recipients; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.announcement_email_recipients IS 'One row per resolved user, whatever happened to them. A count cannot answer "why did Bob not get it", which is the only question anybody asks a record like this. Address and login are copied rather than joined: the record has to outlive the account.';

--
-- Name: COLUMN announcement_email_recipients.reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.announcement_email_recipients.reason IS 'Why this recipient was skipped or how the send failed. Two skip reasons are named back to the administrator — locked and no_address — and a failure carries the relay''s own words, so this is not a closed vocabulary.';

--
-- Name: announcement_emails; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.announcement_emails (
    id text NOT NULL,
    subject text NOT NULL,
    body text NOT NULL,
    link_url text DEFAULT ''::text NOT NULL,
    link_text text DEFAULT ''::text NOT NULL,
    audience text NOT NULL,
    status text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    cloned_from text,
    sent_by text,
    sent_at timestamp with time zone,
    completed_at timestamp with time zone,
    resolved integer DEFAULT 0 NOT NULL,
    delivered integer DEFAULT 0 NOT NULL,
    failed integer DEFAULT 0 NOT NULL,
    skipped integer DEFAULT 0 NOT NULL,
    abandoned integer DEFAULT 0 NOT NULL
);

--
-- Name: TABLE announcement_emails; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.announcement_emails IS 'The announcement an administrator sends — the first mail in the product whose arrival is the point. Every other notification may vanish; a frozen row saying "sent to 214 people on the 3rd of March" has to be true. Two tables rather than a settings key for that reason.';

--
-- Name: api_key_usage_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_key_usage_daily (
    key_id text NOT NULL,
    key_kind text NOT NULL,
    day date NOT NULL,
    app_id text DEFAULT ''::text NOT NULL,
    status_class smallint NOT NULL,
    requests bigint DEFAULT 0 NOT NULL
);

--
-- Name: TABLE api_key_usage_daily; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.api_key_usage_daily IS 'Per-key traffic, the same shape as app_usage_daily and for the same reason: counters aggregate in memory (internal/usage) and flush on an interval, so no request path pays a database write.';

--
-- Name: COLUMN api_key_usage_daily.key_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.api_key_usage_daily.key_id IS 'No foreign key, deliberately — a deleted key''s traffic is still a fact, exactly as app_usage_daily survives a deleted user.';

--
-- Name: COLUMN api_key_usage_daily.app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.api_key_usage_daily.app_id IS 'The empty string for a platform API call, the app''s id for a gateway call, so "this token only ever calls app X" is answerable. No foreign key, for key_id''s reason.';

--
-- Name: app_api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_api_keys (
    id text NOT NULL,
    app_id text NOT NULL,
    name text NOT NULL,
    key_hash text NOT NULL,
    created_by text,
    key_prefix text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    disabled_at timestamp with time zone,
    last_used_at timestamp with time zone,
    caller_role text
);

--
-- Name: TABLE app_api_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_api_keys IS 'Per-app API keys for machine clients on the apps gateway. Only a SHA-256 of the secret is stored; the plaintext is shown once at mint time.';

--
-- Name: COLUMN app_api_keys.key_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_api_keys.key_hash IS 'A SHA-256 of the secret. The plaintext is shown once at mint time and never stored.';

--
-- Name: COLUMN app_api_keys.caller_role; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_api_keys.caller_role IS 'The word this key is described to its own app by. Nullable and never backfilled: NULL means the app is told "app", exactly as before the column existed, which is the whole difference between absent and off. No CHECK, deliberately — the mintable set depends on the minter''s own effective role on the app, so a constraint could only encode the half that does not, and the backstop that matters is per-request: VerifyRole checks the word against the closed list of six on the way out.';

--
-- Name: app_capability_usage_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_capability_usage_daily (
    app_id text NOT NULL,
    capability text NOT NULL,
    day date NOT NULL,
    requests bigint DEFAULT 0 NOT NULL
);

--
-- Name: TABLE app_capability_usage_daily; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_capability_usage_daily IS 'How much each app uses a governed capability, counted in the existing usage flush rather than written as an audit row per call — the viewer-identity route is a read a Streamlit app makes on every rerun. It buys "app X resolved an identity 4,000 times last month" and deliberately not whose identity, which would be a surveillance log of every viewer of every app.';

--
-- Name: COLUMN app_capability_usage_daily.app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_capability_usage_daily.app_id IS 'No foreign key, for app_usage_daily.app_id''s reason.';

--
-- Name: app_consumption_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_consumption_daily (
    app_id text NOT NULL,
    day date NOT NULL,
    cpu_seconds double precision DEFAULT 0 NOT NULL,
    memory_byte_seconds double precision DEFAULT 0 NOT NULL,
    peak_memory_bytes bigint DEFAULT 0 NOT NULL,
    observed_seconds double precision DEFAULT 0 NOT NULL,
    has_gap boolean DEFAULT false NOT NULL
);

--
-- Name: TABLE app_consumption_daily; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_consumption_daily IS 'The coarse consumption record, kept for history and trends. Job runs are included in the app totals here but lose their individual identity: per-run detail is an investigation question and lives in app_consumption_intervals for as long as that is retained.';

--
-- Name: COLUMN app_consumption_daily.app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_consumption_daily.app_id IS 'No foreign key, for app_usage_daily.app_id''s reason.';

--
-- Name: app_consumption_intervals; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_consumption_intervals (
    id bigint NOT NULL,
    app_id text NOT NULL,
    job_run_id text DEFAULT ''::text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    ended_at timestamp with time zone NOT NULL,
    cpu_seconds double precision DEFAULT 0 NOT NULL,
    memory_byte_seconds double precision DEFAULT 0 NOT NULL,
    peak_memory_bytes bigint DEFAULT 0 NOT NULL,
    has_gap boolean DEFAULT false NOT NULL
);

--
-- Name: TABLE app_consumption_intervals; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_consumption_intervals IS 'The fine-grained consumption record, one row per workload per flush interval. Kept briefly for investigation and then rolled up. The largest table in the schema by two orders of magnitude.';

--
-- Name: COLUMN app_consumption_intervals.app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_consumption_intervals.app_id IS 'No foreign key, for app_usage_daily.app_id''s reason.';

--
-- Name: COLUMN app_consumption_intervals.job_run_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_consumption_intervals.job_run_id IS 'The job run this interval belongs to, when one does. No foreign key: consumption outlives the run record.';

--
-- Name: app_consumption_intervals_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.app_consumption_intervals_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: app_consumption_intervals_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.app_consumption_intervals_id_seq OWNED BY public.app_consumption_intervals.id;

--
-- Name: app_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_credentials (
    app_id text NOT NULL,
    credential_id text NOT NULL,
    attached_by text,
    attached_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE app_credentials; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_credentials IS 'Which apps get a credential. One row per pair, written by an administrator and nobody else: an app has none until an administrator attaches one — absent, not off — and an app owner cannot grant themselves production data by editing their own app. No enabled column, unlike app_integrations: a disabled attachment would be a third state between attached and not, and the answer to "should this app stop reaching the warehouse" is detach.';

--
-- Name: app_cron_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_cron_jobs (
    id text NOT NULL,
    app_id text NOT NULL,
    name text NOT NULL,
    schedule text NOT NULL,
    path text NOT NULL,
    method text DEFAULT 'GET'::text NOT NULL,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    overlap text DEFAULT 'skip'::text NOT NULL,
    max_attempts integer DEFAULT 1 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    next_run_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE app_cron_jobs; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_cron_jobs IS 'Scheduled HTTP triggers: on schedule, the platform requests path on the app over loopback (internal/cronjobs). No commands are ever executed — that is the line between this and the job system.';

--
-- Name: COLUMN app_cron_jobs.path; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_cron_jobs.path IS 'A path the app published. A scheduled task executes no command; this is an HTTP request, and that is the boundary between it and the job system.';

--
-- Name: COLUMN app_cron_jobs.method; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_cron_jobs.method IS 'GET or POST only. The other verbs would invite a schedule that quietly deletes something — which an app can still do, but by publishing a path that does it, in its own code, rather than by a verb typed into a panel.';

--
-- Name: app_cron_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_cron_runs (
    id text NOT NULL,
    job_id text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    finished_at timestamp with time zone,
    status text NOT NULL,
    http_status integer,
    error text DEFAULT ''::text NOT NULL,
    duration_ms bigint DEFAULT 0 NOT NULL,
    trigger text DEFAULT 'manual'::text NOT NULL,
    actor_id text,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    result text DEFAULT ''::text NOT NULL,
    result_truncated boolean DEFAULT false NOT NULL,
    attempt integer DEFAULT 1 NOT NULL,
    retry_of text,
    retry_at timestamp with time zone
);

--
-- Name: TABLE app_cron_runs; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_cron_runs IS 'One row per firing of a scheduled task, including the ones that were skipped. A skipped run is recorded; a refused one is not.';

--
-- Name: COLUMN app_cron_runs.result; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_cron_runs.result IS 'What the app returned, as a JSON document. Not a vocabulary — no CHECK.';

--
-- Name: app_data; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_data (
    app_id text NOT NULL,
    document text NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE app_data; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_data IS 'One JSON document an app may keep. One row per app and the app id is the key: this is a document, not a store. No second dimension means no listing, no pagination, no prefix scan and no per-key authorization surface. Cascades, because it is part of the app rather than a resource mapped to it.';

--
-- Name: COLUMN app_data.document; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_data.document IS 'The app''s own bytes, stored as text and never read by the platform. Not JSONB: typing it would force every scalar into a quoted literal and add a parse step to bytes whose whole job is to be the value.';

--
-- Name: app_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_events (
    id text NOT NULL,
    app_id text NOT NULL,
    kind text NOT NULL,
    severity text NOT NULL,
    source text NOT NULL,
    summary text NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    deployment_id text,
    fingerprint text DEFAULT ''::text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    notified_at timestamp with time zone,
    notice_sent boolean DEFAULT false NOT NULL,
    resolved_at timestamp with time zone,
    CONSTRAINT app_events_severity_check CHECK ((severity = ANY (ARRAY['error'::text, 'warning'::text, 'info'::text])))
);

--
-- Name: TABLE app_events; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_events IS 'A durable, app-scoped record of everything that breaks in a hosted app. Deliberately not audit_log: that table is actor-driven, and a poller discovering the same expired token once a minute is worse noise than a sign-in. The two are merged at read time instead.';

--
-- Name: COLUMN app_events.kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_events.kind IS 'What happened. Open by construction: every subsystem that can fail names its own kinds, and a CHECK would make adding one a migration.';

--
-- Name: COLUMN app_events.deployment_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_events.deployment_id IS 'Which release the event is about, when one applies. No foreign key: events outlive the deployments they name.';

--
-- Name: app_integration_sends; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_integration_sends (
    id text NOT NULL,
    app_id text NOT NULL,
    kind text NOT NULL,
    action text NOT NULL,
    status text NOT NULL,
    destination text DEFAULT ''::text NOT NULL,
    recipients jsonb DEFAULT '[]'::jsonb NOT NULL,
    subject text DEFAULT ''::text NOT NULL,
    body_bytes integer DEFAULT 0 NOT NULL,
    body_sha256 text DEFAULT ''::text NOT NULL,
    body text,
    attachments jsonb DEFAULT '[]'::jsonb NOT NULL,
    idempotency_key text,
    deployment_id text,
    error text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    placement_id text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE app_integration_sends; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_integration_sends IS 'The send ledger. Deliberately not app_events: that table is what broke, carrying the cooldown machinery a send has no use for, and forty successful sends a day would drown the one deploy failure that feed exists to surface. Recipients are third-party personal data with their own retention answer.';

--
-- Name: COLUMN app_integration_sends.kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_integration_sends.kind IS 'The integration kind. No CHECK, for integration_placements.kind''s reason.';

--
-- Name: COLUMN app_integration_sends.action; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_integration_sends.action IS 'What was asked of the far side — post, file, send and their kin. Open, because the set grows with appint.Known.';

--
-- Name: COLUMN app_integration_sends.deployment_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_integration_sends.deployment_id IS 'Which release sent it, when one did. No foreign key — a send outlives the release.';

--
-- Name: COLUMN app_integration_sends.placement_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_integration_sends.placement_id IS 'The placement this send produced, when it produced one. No foreign key: placements outlive the ledger.';

--
-- Name: app_integrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_integrations (
    app_id text NOT NULL,
    kind text NOT NULL,
    connection_id text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    destination_id text DEFAULT ''::text NOT NULL,
    destination_label text DEFAULT ''::text NOT NULL,
    attached_by text,
    attached_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE app_integrations; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_integrations IS 'One attachment per (app, kind): the integration an administrator gave this app. An app has none of a kind until then — absent, not off — and a second attachment of the same kind is refused rather than swapped.';

--
-- Name: COLUMN app_integrations.kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_integrations.kind IS 'The integration kind. No CHECK, for integration_placements.kind''s reason.';

--
-- Name: COLUMN app_integrations.destination_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_integrations.destination_id IS 'The far side''s own id — a Slack channel, a folder — and not a local row.';

--
-- Name: app_platform_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_platform_log (
    id bigint NOT NULL,
    app_id text NOT NULL,
    workload_name text NOT NULL,
    deployment_id text,
    at timestamp with time zone DEFAULT now() NOT NULL,
    seq integer DEFAULT 0 NOT NULL,
    stream text DEFAULT 'stdout'::text NOT NULL,
    line text NOT NULL
);

--
-- Name: TABLE app_platform_log; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_platform_log IS 'What the platform said about one app, kept. Not app_events, and the distinction is load-bearing: app_events records that something happened and this records what the platform did — the event keeps the verdict, the log takes the narration that led to it. Not the app''s own output either, which is why there is no source column.';

--
-- Name: COLUMN app_platform_log.workload_name; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_platform_log.workload_name IS 'The only thing that distinguishes two releases'' narration when deployment_id is NULL, which it is for every line a background actor writes.';

--
-- Name: COLUMN app_platform_log.deployment_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_platform_log.deployment_id IS 'Which release the line is about. NULL for every line a background actor writes, since none of them holds a deployment — which is why workload_name is what distinguishes two releases'' narration.';

--
-- Name: app_platform_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.app_platform_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: app_platform_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.app_platform_log_id_seq OWNED BY public.app_platform_log.id;

--
-- Name: app_reachability; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_reachability (
    app_id text NOT NULL,
    last_verdict text DEFAULT ''::text NOT NULL,
    last_stage text DEFAULT ''::text NOT NULL,
    last_detail text DEFAULT ''::text NOT NULL,
    status_code integer DEFAULT 0 NOT NULL,
    tls_not_after timestamp with time zone,
    checked_at timestamp with time zone,
    probed_url text DEFAULT ''::text NOT NULL,
    strikes integer DEFAULT 0 NOT NULL,
    suspended_at timestamp with time zone
);

--
-- Name: TABLE app_reachability; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_reachability IS 'What the outside world says about one public app, and whether the platform has stopped believing the claim. apps.visibility is the owner''s sentence and the platform never writes it, so the suspension is a separate fact in a separate table. Deleting the row is the whole of clearing the state — no tombstone, because anything left behind would eventually be read as history and this is a state rather than a log.';

--
-- Name: COLUMN app_reachability.last_verdict; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_reachability.last_verdict IS 'unknown is not not_ok and silence is not a verdict: a gate opens only on an explicit ok. The empty string is an app that has never been probed.';

--
-- Name: COLUMN app_reachability.last_detail; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_reachability.last_detail IS 'The vendor''s prose. Stored because it is the only way to tell two situations apart on the wire, rendered so a support reply can quote the sentence somebody else wrote, and never parsed — a branch on its text would be this install depending on a string the vendor is free to reword.';

--
-- Name: app_render_schedule_recipients; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_render_schedule_recipients (
    schedule_id text NOT NULL,
    user_id text NOT NULL,
    added_at timestamp with time zone DEFAULT now() NOT NULL,
    added_by text
);

--
-- Name: TABLE app_render_schedule_recipients; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_render_schedule_recipients IS 'Who a schedule''s render is announced to. User ids, not addresses: every recipient is an account that can already open the app, and checking that at send time is what makes revoking access take somebody off the mail with no second action.';

--
-- Name: COLUMN app_render_schedule_recipients.schedule_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_render_schedule_recipients.schedule_id IS 'The schedule this list belongs to. Cascades on delete, deliberately, and the UI says so before it deletes: a list kept past its schedule would reappear if one were recreated, which is a worse surprise than losing an afternoon of picking people.';

--
-- Name: COLUMN app_render_schedule_recipients.user_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_render_schedule_recipients.user_id IS 'A user id and not an address, because a link is worthless to somebody refused at the app. Cascades on delete: the recipient list is meaningless without the account.';

--
-- Name: app_render_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_render_schedules (
    id text NOT NULL,
    app_id text NOT NULL,
    name text DEFAULT 'Re-render'::text NOT NULL,
    schedule text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    next_run_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE app_render_schedules; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_render_schedules IS 'At most one render schedule per app, and what it fires is a deploy. A table rather than a column on apps because the shared claim selects app_id by name and apps has none.';

--
-- Name: app_tags; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_tags (
    app_id text NOT NULL,
    tag_id text NOT NULL,
    value text NOT NULL,
    value_slug text NOT NULL,
    added_by text,
    added_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE app_tags; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_tags IS 'One value per (app, tag). The primary key is what enforces "an app holds at most one value per tag", which is why there is no grouping or mutual-exclusion machinery anywhere in this feature.';

--
-- Name: app_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_tokens (
    id text NOT NULL,
    app_id text NOT NULL,
    deployment_id text,
    token_hash text NOT NULL,
    prefix text NOT NULL,
    workload text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone,
    revoked_at timestamp with time zone
);

--
-- Name: TABLE app_tokens; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_tokens IS 'The app token: an app''s outbound credential, and the third machine class. The axis that separates it from the other two is direction — it is how an app''s own process calls out to /api/v1/app/*. Opaque and hashed rather than a JWT, because revocation has to be immediate when a deployment is replaced and a bearer JWT cannot be withdrawn.';

--
-- Name: COLUMN app_tokens.deployment_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_tokens.deployment_id IS 'The release this token was minted for. No foreign key: the token is revoked when the release is replaced, and the row is kept for the record.';

--
-- Name: app_usage_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_usage_daily (
    app_id text NOT NULL,
    day date NOT NULL,
    user_id text DEFAULT ''::text NOT NULL,
    requests bigint DEFAULT 0 NOT NULL
);

--
-- Name: TABLE app_usage_daily; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_usage_daily IS 'Per-app, per-viewer, per-day request counters, aggregated in memory by internal/usage and flushed on an interval.';

--
-- Name: COLUMN app_usage_daily.app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_usage_daily.app_id IS 'No foreign key: a deleted app''s cost is still a fact.';

--
-- Name: COLUMN app_usage_daily.user_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_usage_daily.user_id IS 'Not authorship and not a reference: a dimension of a counter, part of the primary key, where the empty string is a real value meaning an anonymous viewer of a public app. No foreign key, because a deleted user''s traffic is still a fact — an accounting record that vanishes when its subject does cannot answer "what did we spend last quarter".';

--
-- Name: apps; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.apps (
    id text NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    repo_url text NOT NULL,
    branch text DEFAULT 'main'::text NOT NULL,
    port integer,
    status text DEFAULT 'stopped'::text NOT NULL,
    framework text DEFAULT 'nextjs'::text NOT NULL,
    visibility text DEFAULT 'private'::text NOT NULL,
    legacy_prefix_rewrite boolean DEFAULT false NOT NULL,
    current_release text DEFAULT ''::text NOT NULL,
    auto_deploy boolean DEFAULT false NOT NULL,
    last_seen_sha text DEFAULT ''::text NOT NULL,
    git_token text DEFAULT ''::text NOT NULL,
    endpoint_host text DEFAULT '127.0.0.1'::text NOT NULL,
    release_ref text DEFAULT ''::text NOT NULL,
    execution_mode text DEFAULT 'shared'::text NOT NULL,
    running_mode text DEFAULT 'shared'::text NOT NULL,
    entrypoint text DEFAULT ''::text NOT NULL,
    start_cmd text DEFAULT ''::text NOT NULL,
    auto_delete_releases boolean DEFAULT true NOT NULL,
    releases_kept integer DEFAULT 0 NOT NULL,
    never_sleep boolean DEFAULT false NOT NULL,
    notify_failures boolean DEFAULT true NOT NULL,
    gallery_item_id text DEFAULT ''::text NOT NULL,
    gallery_kind text DEFAULT ''::text NOT NULL,
    gallery_version text DEFAULT ''::text NOT NULL,
    dependency_baseline_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    owner_user_id text,
    owner_group_id text,
    locked_at timestamp with time zone,
    locked_by text,
    locked_message text DEFAULT ''::text NOT NULL,
    viewer_identity boolean DEFAULT false NOT NULL,
    env_version integer DEFAULT 0 NOT NULL,
    env_applied_version integer DEFAULT 0 NOT NULL,
    git_connection_id text,
    build_timeout_seconds integer,
    frame_ancestors text DEFAULT ''::text NOT NULL,
    memory_limit_mb integer DEFAULT 0 NOT NULL,
    CONSTRAINT apps_execution_mode_check CHECK ((execution_mode = ANY (ARRAY['shared'::text, 'isolated'::text]))),
    CONSTRAINT apps_one_owner CHECK ((num_nonnulls(owner_user_id, owner_group_id) = 1))
);

--
-- Name: TABLE apps; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.apps IS 'The deployed application. Wide, and deliberately not split: it is read on the app-lookup path, which the Data path invariant forbids adding work to, and a vertical split would buy tidiness and pay for it in joins on the hot path.';

--
-- Name: COLUMN apps.id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.id IS 'text, not uuid, for users.id''s reason.';

--
-- Name: COLUMN apps.status; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.status IS 'sleeping is not stopped: a sleeping app wakes itself on the next request and still picks up a git push, while a stopped one was stopped by a person and must stay down until another person says otherwise.';

--
-- Name: COLUMN apps.visibility; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.visibility IS 'The owner''s sentence, and the platform never writes it. The effective visibility a reader authorizes against is computed in AuthObjectOf, because two things — an unreachable public app and an install that publishes nothing — withhold anonymity without changing what the owner said.';

--
-- Name: COLUMN apps.execution_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.execution_mode IS 'The mode the app is meant to run under. A mode, never a backend name: an app is isolated, not fargate or pod, so an install that changes cluster technology does not rewrite every app row.';

--
-- Name: COLUMN apps.running_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.running_mode IS 'The mode the app''s current release is actually running under, denormalized beside current_release. Splitting it from execution_mode is what makes changing the desired mode safe and reversible. The empty string is a row written before modes were per-deployment, and every one of those is a process.';

--
-- Name: COLUMN apps.gallery_item_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.gallery_item_id IS 'Provenance only — which catalog item deployed this app. No foreign key, and no platform behaviour anywhere may branch on it.';

--
-- Name: COLUMN apps.locked_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.locked_at IS 'A lock is not a status, and stopped is not a lock. Orthogonal to status for the reason app_reachability is orthogonal to visibility: the day a lock joined the status vocabulary, an app would be remembered as stopped or remembered as locked and never both.';

--
-- Name: COLUMN apps.locked_by; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.locked_by IS 'Who locked it. Nullable since the second review: NULL means no lock or a deleted administrator, and applock.go reads it back through COALESCE so Go still sees the empty string and the API field is unchanged. 00003 argued against the constraint — an administrator can leave the company and the fact that they locked this app in March is still the record — and clearing the attribution is not rewriting the record: locked_at and locked_message are untouched.';

--
-- Name: COLUMN apps.memory_limit_mb; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.memory_limit_mb IS 'One app''s memory ceiling in mebibytes, or 0 for the install''s own -- never "uncapped". An administrator''s value and not an owner''s: a ceiling an app could raise for itself is not a ceiling. Mebibytes because it is what both backends count in (PM2 takes %dM, Kubernetes a binary quantity); the API offers megabytes and gigabytes over it.';

--
-- Name: audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_log (
    id text NOT NULL,
    actor_type text NOT NULL,
    actor_id text,
    action text NOT NULL,
    target_type text DEFAULT ''::text NOT NULL,
    target_id text DEFAULT ''::text NOT NULL,
    before jsonb,
    after jsonb,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    request_id text DEFAULT ''::text NOT NULL,
    ip text DEFAULT ''::text NOT NULL,
    user_agent text DEFAULT ''::text NOT NULL,
    api_key_id text
);

--
-- Name: TABLE audit_log; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.audit_log IS 'Append-only audit trail; there is deliberately no deletion path, and deliberately no foreign keys. An audit record has to survive the deletion of its actor and its target — that is most of the point of keeping it — and a foreign key is precisely the thing that would prevent the deletion or erase the attribution.';

--
-- Name: COLUMN audit_log.actor_type; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.actor_type IS 'The principal kind behind the act — user, deploy_key, operator, system, provider and their kin. No CHECK: the vocabulary is auth.Principal.Type, which grows with every new way of authenticating, and a constraint here would be a second, always-stale statement of a list that already lives in one place.';

--
-- Name: COLUMN audit_log.actor_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.actor_id IS 'Not a foreign key, by design. This is the one place a foreign key is exactly wrong: NULL means the system acted, and a non-NULL value may name an account that no longer exists — which is correct, and is most of the point of keeping the row.';

--
-- Name: COLUMN audit_log.action; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.action IS 'The verb, dotted and open by construction: every feature adds its own, and a CHECK would make shipping a feature a migration.';

--
-- Name: COLUMN audit_log.target_type; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.target_type IS 'What was acted on. Open for the same reason as action.';

--
-- Name: COLUMN audit_log.target_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.target_id IS 'No foreign key, deliberately: the record has to survive the deletion of what it names.';

--
-- Name: COLUMN audit_log.request_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.request_id IS 'The request that caused this, for correlation with the access log. Not a Launchpad reference.';

--
-- Name: COLUMN audit_log.api_key_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.api_key_id IS 'Which credential spoke, when it was not a browser. No foreign key — a revoked and deleted key must not erase the record of what it did.';

--
-- Name: auth_providers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auth_providers (
    id text NOT NULL,
    kind text NOT NULL,
    display_name text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    client_id text DEFAULT ''::text NOT NULL,
    client_secret text DEFAULT ''::text NOT NULL,
    issuer text DEFAULT ''::text NOT NULL,
    auth_url text DEFAULT ''::text NOT NULL,
    token_url text DEFAULT ''::text NOT NULL,
    userinfo_url text DEFAULT ''::text NOT NULL,
    scopes text DEFAULT ''::text NOT NULL,
    allowed_domains text DEFAULT ''::text NOT NULL,
    default_role text DEFAULT 'viewer'::text NOT NULL,
    link_by_verified_email boolean DEFAULT false NOT NULL,
    auto_provision boolean DEFAULT true NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    groups_claim text DEFAULT ''::text NOT NULL,
    scim_token_hash text DEFAULT ''::text NOT NULL,
    scim_token_set_at timestamp with time zone,
    scim_last_call_at timestamp with time zone
);

--
-- Name: TABLE auth_providers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.auth_providers IS 'Admin-managed sign-in providers. Identity has its own table because a hardcoded provider makes the choice operator-only — a restart to change — and makes a second provider inexpressible.';

--
-- Name: COLUMN auth_providers.client_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.auth_providers.client_id IS 'The OAuth client id at the provider. Not a Launchpad reference.';

--
-- Name: cli_device_codes; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cli_device_codes (
    device_code_hash text NOT NULL,
    user_code text NOT NULL,
    hostname text DEFAULT ''::text NOT NULL,
    client_version text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_polled_at timestamp with time zone,
    approved_user_id text,
    approved_at timestamp with time zone,
    denied_at timestamp with time zone
);

--
-- Name: TABLE cli_device_codes; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.cli_device_codes IS 'lp login: one pending authorization, from the moment a terminal asks until a browser answers. Every row is scaffolding for a credential that lives in user_api_keys, and nothing in it survives the answer. No secret rests here — the device code is stored as a SHA-256 and the minted key''s plaintext is never written at all.';

--
-- Name: COLUMN cli_device_codes.device_code_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.cli_device_codes.device_code_hash IS 'Stored as a SHA-256, so the row cannot be replayed by anyone who reads the table.';

--
-- Name: credential_values; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.credential_values (
    credential_id text NOT NULL,
    key text NOT NULL,
    value text NOT NULL
);

--
-- Name: TABLE credential_values; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.credential_values IS 'The credential values, encrypted through internal/crypto. A bag of key/value pairs rather than one opaque secret per name, because real connections are three or four fields. Never returned by any route to anybody, including an administrator: write-only in a stronger sense than a sensitive env var, because nobody holds this except whoever set it.';

--
-- Name: credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.credentials (
    id text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE credentials; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.credentials IS 'The credential itself: a name an administrator types, and nothing secret. Deliberately not typed per vendor — knowing nothing about Snowflake is what lets one concept cover a warehouse, an internal API and a bucket, and what stops the platform being wrong about any of them.';

--
-- Name: cron_run_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.cron_run_logs (
    id bigint NOT NULL,
    run_id text NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    stream text DEFAULT 'stdout'::text NOT NULL,
    line text NOT NULL
);

--
-- Name: TABLE cron_run_logs; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.cron_run_logs IS 'What a scheduled task printed, when it chose to say. Opt-in from the app necessarily: the platform makes an HTTP request and owns nothing of what happens inside it. That is the whole difference from job_run_logs, whose lines the runner captures because the platform owns the process.';

--
-- Name: cron_run_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.cron_run_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: cron_run_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.cron_run_logs_id_seq OWNED BY public.cron_run_logs.id;

--
-- Name: dependency_waivers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dependency_waivers (
    id text NOT NULL,
    app_id text NOT NULL,
    advisory_id text NOT NULL,
    package text DEFAULT ''::text NOT NULL,
    reason text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone
);

--
-- Name: TABLE dependency_waivers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.dependency_waivers IS 'An accepted risk, with an expiry and somebody''s name on it. Per app and per advisory: there is no blanket exemption, because that would be an exception invisible in the list an administrator audits.';

--
-- Name: COLUMN dependency_waivers.advisory_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.dependency_waivers.advisory_id IS 'No foreign key: a waiver has to survive an advisory leaving the feed, which is what a re-sync does.';

--
-- Name: COLUMN dependency_waivers.reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.dependency_waivers.reason IS 'Somebody''s words. Not a vocabulary.';

--
-- Name: deploy_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deploy_keys (
    id text NOT NULL,
    name text NOT NULL,
    key_hash text NOT NULL,
    key_prefix text DEFAULT ''::text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    last_used_at timestamp with time zone,
    disabled_at timestamp with time zone,
    revoked_at timestamp with time zone,
    scope text DEFAULT 'deploy'::text NOT NULL,
    CONSTRAINT deploy_keys_scope_check CHECK ((scope = ANY (ARRAY['read'::text, 'deploy'::text])))
);

--
-- Name: TABLE deploy_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.deploy_keys IS 'The third key class, separated from the other two by who it belongs to: an app key is a grant on one app, a personal key is the person, and a deploy key belongs to nobody — an administrator mints it, possession is the authorization, and it is revoked explicitly or not at all.';

--
-- Name: COLUMN deploy_keys.key_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deploy_keys.key_hash IS 'A SHA-256 of the secret — see app_api_keys.key_hash.';

--
-- Name: deployment_dependencies; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deployment_dependencies (
    deployment_id text NOT NULL,
    ecosystem text NOT NULL,
    package text NOT NULL,
    version text NOT NULL,
    direct boolean DEFAULT false NOT NULL,
    dev boolean DEFAULT false NOT NULL,
    path text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE deployment_dependencies; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.deployment_dependencies IS 'What a release actually installed. The one table that grows, and what makes everything else cheap: the sweep joins stored inventory against refreshed advisories, so watching costs no rebuild and touches no registry. Pruned through the deployments cascade — retention of the inventory is the release''s retention and deliberately not a knob of its own.';

--
-- Name: deployments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deployments (
    id text NOT NULL,
    app_id text,
    commit_sha text DEFAULT ''::text NOT NULL,
    branch text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    build_log text DEFAULT ''::text NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    triggered_by text DEFAULT 'manual'::text NOT NULL,
    artifact_key text DEFAULT ''::text NOT NULL,
    execution_mode text DEFAULT 'shared'::text NOT NULL,
    disk_bytes bigint,
    artifact_bytes bigint,
    gallery_version text DEFAULT ''::text NOT NULL,
    current_step text DEFAULT ''::text NOT NULL,
    step_started_at timestamp with time zone,
    step_timings text DEFAULT ''::text NOT NULL,
    dependency_verdict text DEFAULT ''::text NOT NULL,
    dependency_checked_at timestamp with time zone,
    dependency_feed_as_of timestamp with time zone,
    runtime_language text DEFAULT ''::text NOT NULL,
    runtime_version text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE deployments; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.deployments IS 'One build of one app. Three phases — upload, scan, deploy — and the phase is derived from the timestamps rather than stored.';

--
-- Name: COLUMN deployments.triggered_by; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.triggered_by IS 'Not a person. This is the category of trigger — manual, auto, rollback, render — and the CHECK beside it is the whole vocabulary. Named _by for historical reasons and deliberately not renamed: the name is on the wire in models.go JSON tags and the SPA reads it.';

--
-- Name: COLUMN deployments.execution_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.execution_mode IS 'The effective mode this release ran under. Constrained here as well as on apps, because the invariant governs all three spellings of it — shared or isolated, never a backend name.';

--
-- Name: COLUMN deployments.dependency_verdict; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.dependency_verdict IS 'The policy verdict as a JSON document, not a vocabulary — no CHECK. A restart is not a deploy: this is the release''s verdict, not today''s.';

--
-- Name: encryption_data_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.encryption_data_keys (
    id text NOT NULL,
    wrapped bytea NOT NULL,
    kek_uri text NOT NULL,
    active boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE encryption_data_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.encryption_data_keys IS 'Where a wrapped data key lives, for the managed encryption backend (ADR §62). A table rather than a field beside each ciphertext because the stored record is one format, v2:<keyid>:<hex>, and a fourth field for one backend would make it two. What is in here is not a secret: the wrapped key is useless without the KMS grant.';

--
-- Name: COLUMN encryption_data_keys.id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.encryption_data_keys.id IS 'Derived from the key material, never configured — the same fingerprint the local backend derives, so a record never says which sort of key wrote it. That is what lets an install move from a local key to a KMS without rewriting anything.';

--
-- Name: env_vars; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.env_vars (
    id text NOT NULL,
    app_id text,
    key text NOT NULL,
    value text NOT NULL,
    sensitive boolean DEFAULT true NOT NULL
);

--
-- Name: TABLE env_vars; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.env_vars IS 'Environment variables for one app. A sensitive value is write-only in the API forever — un-marking one requires supplying a new value, so the flag can never be used as a reveal.';

--
-- Name: features; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.features (
    name text NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    enabled_at timestamp with time zone,
    enabled_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE features; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.features IS 'One row per feature an administrator can turn on. Absent means never enabled, which is a different state from enabled-and-then-switched-off: the first shows nothing in the UI at all, the second keeps its row visible so an administrator can see why the thing they installed stopped answering.';

--
-- Name: gallery_catalog; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.gallery_catalog (
    id integer DEFAULT 1 NOT NULL,
    document text DEFAULT ''::text NOT NULL,
    etag text DEFAULT ''::text NOT NULL,
    status text DEFAULT ''::text NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    item_count integer DEFAULT 0 NOT NULL,
    dropped_count integer DEFAULT 0 NOT NULL,
    fetched_at timestamp with time zone,
    CONSTRAINT gallery_catalog_id_check CHECK ((id = 1))
);

--
-- Name: TABLE gallery_catalog; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.gallery_catalog IS 'The cached catalog. One row, always: the catalog is a document the platform caches, not a set of entities it owns. A failed fetch never clears document — the last good catalog is served with its age beside it, because an empty gallery and an unreachable one must stay tellable apart.';

--
-- Name: COLUMN gallery_catalog.document; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.gallery_catalog.document IS 'The last catalog fetched. A failed fetch never clears it: an empty gallery and an unreachable one must stay tellable apart by the person looking at the page.';

--
-- Name: git_connection_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_connection_identities (
    id text NOT NULL,
    connection_id text NOT NULL,
    user_id text NOT NULL,
    access_token text DEFAULT ''::text NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE git_connection_identities; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.git_connection_identities IS 'The picker''s credential, which is a person''s and is read by exactly one code path: the repository listing. It is not the clone credential and there is no conversion between them — listing repositories with the install''s credential would turn a picker into an estate directory, handing any signed-in developer a catalogue of the organization''s private repositories.';

--
-- Name: git_connections; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.git_connections (
    id text NOT NULL,
    kind text NOT NULL,
    display_name text NOT NULL,
    base_url text NOT NULL,
    auth_method text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    github_app_id text DEFAULT ''::text NOT NULL,
    installation_id text DEFAULT ''::text NOT NULL,
    client_id text DEFAULT ''::text NOT NULL,
    client_secret text DEFAULT ''::text NOT NULL,
    clone_secret text DEFAULT ''::text NOT NULL,
    credential_kind text DEFAULT ''::text NOT NULL,
    credential_owner text DEFAULT ''::text NOT NULL,
    credential_expires_at timestamp with time zone,
    webhook_secret text DEFAULT ''::text NOT NULL,
    coverage text DEFAULT ''::text NOT NULL,
    last_error text DEFAULT ''::text NOT NULL,
    last_error_at timestamp with time zone,
    last_ok_at timestamp with time zone,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT git_connections_auth_method_check CHECK ((auth_method = ANY (ARRAY['app'::text, 'token'::text]))),
    CONSTRAINT git_connections_kind_check CHECK ((kind = ANY (ARRAY['github'::text, 'gitlab'::text])))
);

--
-- Name: TABLE git_connections; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.git_connections IS 'A connected git host: a held credential this install clones through. Coverage is the boundary and it is checked at the write, never at the clone; a developer is never told which auth method was used.';

--
-- Name: COLUMN git_connections.github_app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.git_connections.github_app_id IS 'The GitHub App''s own identifier. Renamed from app_id, which said something false: app_id means a Launchpad app in twenty-one other tables in this schema.';

--
-- Name: COLUMN git_connections.installation_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.git_connections.installation_id IS 'The git host''s own installation identifier. Not a Launchpad reference.';

--
-- Name: COLUMN git_connections.client_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.git_connections.client_id IS 'The OAuth client id at the git host. Not a Launchpad reference.';

--
-- Name: group_claim_mappings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.group_claim_mappings (
    group_id text NOT NULL,
    provider_id text NOT NULL,
    claim_value text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by text
);

--
-- Name: TABLE group_claim_mappings; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.group_claim_mappings IS 'One claim value, attached to one group, for one provider. Nothing is created by a sign-in: an administrator creates the group and attaches values, and an unmapped claim value is ignored in silence. provider_id is in the key rather than beside it because two IdPs can both assert "engineering" and mean different rooms of people.';

--
-- Name: COLUMN group_claim_mappings.provider_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.group_claim_mappings.provider_id IS 'No foreign key, for user_identities.provider_id''s reason and one more: a mapping that vanished with a deleted provider row would revoke access as a side effect of a settings change. A mapping for a provider that no longer exists is inert and visible, which is the right pair.';

--
-- Name: group_members; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.group_members (
    group_id text NOT NULL,
    user_id text NOT NULL,
    added_at timestamp with time zone,
    source text DEFAULT 'manual'::text NOT NULL,
    source_provider_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT group_members_source_check CHECK ((source = ANY (ARRAY['manual'::text, 'directory'::text])))
);

--
-- Name: TABLE group_members; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.group_members IS 'Who is in a group. A group is a set of humans and never an actor, and membership is either manual or asserted by a directory feed.';

--
-- Name: COLUMN group_members.source_provider_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.group_members.source_provider_id IS 'Which directory asserted this membership, if one did. No foreign key, for group_claim_mappings.provider_id''s reason.';

--
-- Name: groups; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.groups (
    id text NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    email text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE groups; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.groups IS 'A local group: a set of humans an administrator curates. Local rows, never directory claims — what a directory asserts arrives through group_claim_mappings and lands here as membership.';

--
-- Name: integration_connections; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.integration_connections (
    id text NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    scope_id text DEFAULT ''::text NOT NULL,
    scope_label text DEFAULT ''::text NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    credential_secret text DEFAULT ''::text NOT NULL,
    default_destination_id text DEFAULT ''::text NOT NULL,
    default_destination_label text DEFAULT ''::text NOT NULL,
    for_platform boolean DEFAULT true NOT NULL,
    for_apps boolean DEFAULT true NOT NULL,
    platform_audiences jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE integration_connections; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.integration_connections IS 'One configured connection to a far side — a Slack workspace, a mail relay, a SharePoint site. A connection is welded to its scope and a new scope is a new connection; a connection that exists and offers itself to apps is the install switch for its kind.';

--
-- Name: COLUMN integration_connections.kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_connections.kind IS 'The integration kind. No CHECK, for integration_placements.kind''s reason.';

--
-- Name: COLUMN integration_connections.scope_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_connections.scope_id IS 'The far side''s own id for the scope this connection is welded to. Not a Launchpad reference, and a new scope is a new connection rather than an edit to this.';

--
-- Name: COLUMN integration_connections.default_destination_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_connections.default_destination_id IS 'The far side''s own id, not a Launchpad reference.';

--
-- Name: integration_placements; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.integration_placements (
    id text NOT NULL,
    connection_id text NOT NULL,
    connection_name text DEFAULT ''::text NOT NULL,
    kind text NOT NULL,
    destination_id text DEFAULT ''::text NOT NULL,
    destination_label text DEFAULT ''::text NOT NULL,
    item_id text DEFAULT ''::text NOT NULL,
    item_url text DEFAULT ''::text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    bytes bigint DEFAULT 0 NOT NULL,
    content_type text DEFAULT ''::text NOT NULL,
    sha256 text DEFAULT ''::text NOT NULL,
    app_id text DEFAULT ''::text NOT NULL,
    app_name text DEFAULT ''::text NOT NULL,
    send_id text DEFAULT ''::text NOT NULL,
    placed_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE integration_placements; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.integration_placements IS 'A placement: what an app left behind in somebody else''s system. Every other durable record about an app is designed to be deleted, and that is right for everything it covers. It is wrong exactly once — a file this platform placed in an organization''s document library is evidence about the document library, and deleting an app is not a statement about it. So this table carries no foreign key at all, to the app or to the connection, because the reference has to outlive both.';

--
-- Name: COLUMN integration_placements.connection_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.connection_id IS 'No foreign key, for app_id''s reason: the placement outlives the connection too.';

--
-- Name: COLUMN integration_placements.kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.kind IS 'The integration kind. No CHECK: appint.Known is the specification, and a constraint would make adding an integration a migration.';

--
-- Name: COLUMN integration_placements.destination_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.destination_id IS 'The far side''s own id for the folder or channel — not a Launchpad reference.';

--
-- Name: COLUMN integration_placements.item_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.item_id IS 'The far side''s own id for what was placed — not a Launchpad reference.';

--
-- Name: COLUMN integration_placements.content_type; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.content_type IS 'A MIME type as the far side recorded it. Open by construction.';

--
-- Name: COLUMN integration_placements.app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.app_id IS 'No foreign key, deliberately: the reference has to outlive the app. A file placed in an organization''s document library is evidence about the library, and deleting an app is not a statement about it.';

--
-- Name: COLUMN integration_placements.send_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.send_id IS 'The ledger row this placement came from. No foreign key: the ledger is pruned on its own retention and the placement outlives it.';

--
-- Name: job_definitions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.job_definitions (
    id text NOT NULL,
    app_id text NOT NULL,
    name text NOT NULL,
    source text DEFAULT 'ui'::text NOT NULL,
    command text NOT NULL,
    schedule text,
    params_spec jsonb DEFAULT '{}'::jsonb NOT NULL,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    cpu_millis integer DEFAULT 500 NOT NULL,
    memory_mb integer DEFAULT 512 NOT NULL,
    timeout_sec integer DEFAULT 3600 NOT NULL,
    viewer_runnable boolean DEFAULT false NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    max_attempts integer DEFAULT 1 NOT NULL,
    next_run_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE job_definitions; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.job_definitions IS 'One job system, multiple triggers: cron scheduling, the on-demand API and app-initiated enqueues are all producers into this one durable queue. There is no separate cron subsystem — app_cron_jobs is the HTTP-trigger feature, which is a different thing. This is the codebase''s first durable queue, and it has to be: a run that vanishes with a platform restart is a job that silently never happened.';

--
-- Name: job_run_logs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.job_run_logs (
    id bigint NOT NULL,
    run_id text NOT NULL,
    seq integer NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    stream text DEFAULT 'stdout'::text NOT NULL,
    line text NOT NULL
);

--
-- Name: TABLE job_run_logs; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.job_run_logs IS 'Log lines shipped over the callback channel. A cloud log service as source of truth would mean reading it back for the UI — latency, extra permissions, and a third and fourth log path beside process tailing — so one path is used that behaves identically across both isolated backends.';

--
-- Name: job_run_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.job_run_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: job_run_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.job_run_logs_id_seq OWNED BY public.job_run_logs.id;

--
-- Name: job_runs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.job_runs (
    id text NOT NULL,
    definition_id text,
    app_id text NOT NULL,
    job_name text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    trigger text DEFAULT 'manual'::text NOT NULL,
    actor_id text,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    execution_mode text DEFAULT 'isolated'::text NOT NULL,
    external_id text DEFAULT ''::text NOT NULL,
    release_ref text DEFAULT ''::text NOT NULL,
    claimed_by text DEFAULT ''::text NOT NULL,
    claimed_at timestamp with time zone,
    attempt integer DEFAULT 1 NOT NULL,
    retry_of text,
    not_before timestamp with time zone,
    idempotency_key text,
    output_key text,
    queued_at timestamp with time zone DEFAULT now() NOT NULL,
    started_at timestamp with time zone,
    finished_at timestamp with time zone,
    deadline_at timestamp with time zone,
    exit_code integer,
    failure_stage text DEFAULT ''::text NOT NULL,
    failure_reason text DEFAULT ''::text NOT NULL,
    progress integer DEFAULT 0 NOT NULL,
    result jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT job_runs_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'claimed'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text, 'timed_out'::text, 'resource_exceeded'::text, 'infrastructure_error'::text])))
);

--
-- Name: TABLE job_runs; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.job_runs IS 'One run of one job definition. Claimed rather than assigned: one claim, two tables, and a retry is opt-in, unattended-only, and never silent.';

--
-- Name: COLUMN job_runs.definition_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.definition_id IS 'The definition this run came from. NULL for a run with no stored definition behind it.';

--
-- Name: COLUMN job_runs.execution_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.execution_mode IS 'Constrained for deployments.execution_mode''s reason.';

--
-- Name: COLUMN job_runs.external_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.external_id IS 'The backend''s own handle for the workload — a pod name, a task ARN. Not a Launchpad reference.';

--
-- Name: COLUMN job_runs.claimed_by; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.claimed_by IS 'Not a person. A worker''s identity, written by whichever node won the claim. Named _by for historical reasons and deliberately not renamed, for the reason deployments.triggered_by gives.';

--
-- Name: COLUMN job_runs.failure_reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.failure_reason IS 'A sentence, not a vocabulary — no CHECK. failure_stage is the constrained half.';

--
-- Name: license_usage_samples; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.license_usage_samples (
    id bigint NOT NULL,
    sampled_at timestamp with time zone DEFAULT now() NOT NULL,
    counts_definition integer DEFAULT 1 NOT NULL,
    seats_total integer NOT NULL,
    seats_active integer NOT NULL,
    apps_deployed integer NOT NULL,
    apps_running integer NOT NULL
);

--
-- Name: TABLE license_usage_samples; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.license_usage_samples IS 'What the install counted, sampled over time. The counts ship on the check-in and refuse nothing: there is no seat cap on any tier, and nothing licensed refuses anything on a count.';

--
-- Name: license_usage_samples_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.license_usage_samples_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

--
-- Name: license_usage_samples_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.license_usage_samples_id_seq OWNED BY public.license_usage_samples.id;

--
-- Name: object_grants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.object_grants (
    object_id text NOT NULL,
    principal_type text NOT NULL,
    principal_id text NOT NULL,
    role text NOT NULL,
    granted_by text,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT object_grants_principal_type_check CHECK ((principal_type = ANY (ARRAY['user'::text, 'group'::text, 'service_account'::text]))),
    CONSTRAINT object_grants_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'editor'::text, 'viewer'::text])))
);

--
-- Name: TABLE object_grants; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.object_grants IS 'ReBAC object grants. The object is always an app: there is no object_type column because there has never been a second kind of object, and the foreign key is only expressible because that is true.';

--
-- Name: COLUMN object_grants.principal_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.object_grants.principal_id IS 'A user id or a group id, told apart by principal_type. No foreign key, because it is two references in one column.';

--
-- Name: report_deliveries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.report_deliveries (
    id text NOT NULL,
    app_id text NOT NULL,
    schedule_id text NOT NULL,
    deployment_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    delivered integer DEFAULT 0 NOT NULL,
    skipped integer DEFAULT 0 NOT NULL,
    failed integer DEFAULT 0 NOT NULL
);

--
-- Name: TABLE report_deliveries; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.report_deliveries IS 'One row per delivery attempt, whether or not anything was sent. A run that sent nothing still gets a row — suppressed by the floor, or every recipient skipped — because "did Monday''s report go out" has to be answerable and silence is not an answer.';

--
-- Name: COLUMN report_deliveries.schedule_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.report_deliveries.schedule_id IS 'The schedule that fired. No foreign key: a delivery outlives the schedule, and deleting a schedule is not a statement about what already went out.';

--
-- Name: COLUMN report_deliveries.deployment_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.report_deliveries.deployment_id IS 'The render this delivery announced. It is the only thing that makes a delivery reviewable against what was actually published.';

--
-- Name: report_delivery_recipients; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.report_delivery_recipients (
    id text NOT NULL,
    delivery_id text NOT NULL,
    user_id text,
    login text DEFAULT ''::text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    status text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT report_delivery_recipients_status_check CHECK ((status = ANY (ARRAY['delivered'::text, 'skipped'::text, 'failed'::text])))
);

--
-- Name: TABLE report_delivery_recipients; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.report_delivery_recipients IS 'What happened to each person, and why. login and email are copied rather than joined: the row has to keep making sense after the account is deleted. Somebody skipped is not removed from the recipient list — the list is the owner''s intent and this row is what happened, and collapsing the two would let a week''s outage quietly rewrite who the report is for.';

--
-- Name: COLUMN report_delivery_recipients.user_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.report_delivery_recipients.user_id IS 'A user id, cleared rather than cascaded when the account goes — login and email are copied onto the row, so the record keeps making sense after the person is deleted. This was the one omission the second review found: 00031 argued for exactly this mechanism and then wrote no constraint at all, so a deleted user left an id pointing at no row.';

--
-- Name: COLUMN report_delivery_recipients.reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.report_delivery_recipients.reason IS 'A sentence — "access removed", "no address on file", "too soon" — because an owner asking why Ana stopped receiving the Monday report should find the answer rather than an absence. Not a vocabulary.';

--
-- Name: settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.settings (
    key text NOT NULL,
    value text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text
);

--
-- Name: TABLE settings; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.settings IS 'Platform-wide key/value settings. value stays TEXT and is deliberately not JSONB: this is a scalar store, and typing the column as JSONB would force every scalar to be written as a quoted JSON literal and add a parse step to bytes whose whole job is to be the setting.';

--
-- Name: COLUMN settings.value; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.settings.value IS 'Stored as text and deliberately not JSONB, for app_data.document''s reason. This is a scalar store: most values are a URL, a model name, a word or a flag.';

--
-- Name: storage_managers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_managers (
    id text NOT NULL,
    resource_id text NOT NULL,
    subject_type text NOT NULL,
    subject_id text NOT NULL,
    max_access text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_managers_access_check CHECK ((max_access = ANY (ARRAY['read'::text, 'write'::text, 'overwrite'::text, 'full'::text]))),
    CONSTRAINT storage_managers_subject_type_check CHECK ((subject_type = ANY (ARRAY['user'::text, 'group'::text])))
);

--
-- Name: TABLE storage_managers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_managers IS 'Who may attach a storage resource to an app, without being an administrator. The fourth fact about a resource and a fourth table: storage_mappings says where it appears on an app''s filesystem, store_grants says who may reach a store''s bytes from outside, and neither says who may decide the first one.';

--
-- Name: COLUMN storage_managers.subject_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_managers.subject_id IS 'A user id or a group id, told apart by subject_type. No foreign key, for object_grants.principal_id''s reason.';

--
-- Name: storage_mappings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_mappings (
    id text NOT NULL,
    resource_id text NOT NULL,
    app_id text NOT NULL,
    path text NOT NULL,
    access text DEFAULT 'read'::text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    authority text DEFAULT 'admin'::text NOT NULL,
    folder text DEFAULT ''::text NOT NULL,
    CONSTRAINT storage_mappings_access_check CHECK ((access = ANY (ARRAY['read'::text, 'write'::text, 'overwrite'::text, 'full'::text])))
);

--
-- Name: TABLE storage_mappings; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_mappings IS 'Where a resource appears on one app''s filesystem, and at what level. A mapping is an app''s and a grant is a person''s; neither implies the other. Stamped with the authority that wrote it, so revoking that authority collects exactly those.';

--
-- Name: storage_resources; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_resources (
    id text NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    bucket text DEFAULT ''::text NOT NULL,
    prefix text DEFAULT ''::text NOT NULL,
    claim_name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    backing text DEFAULT 'efs'::text NOT NULL,
    storage_class text DEFAULT ''::text NOT NULL,
    storage_root text DEFAULT ''::text NOT NULL,
    CONSTRAINT storage_resources_backing_check CHECK ((((kind = 'volume'::text) AND (backing = ANY (ARRAY['efs'::text, 's3files'::text, 'host'::text, 'azurefiles'::text]))) OR ((kind = 'store'::text) AND (backing = ANY (ARRAY['mountpoint'::text, 'blob'::text]))))),
    CONSTRAINT storage_resources_kind_check CHECK ((kind = ANY (ARRAY['volume'::text, 'store'::text]))),
    CONSTRAINT storage_resources_root_check CHECK ((((storage_root = ''::text) AND (backing <> 'host'::text)) OR ((storage_root <> ''::text) AND (kind = 'volume'::text))))
);

--
-- Name: TABLE storage_resources; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_resources IS 'Durable storage for apps: admin-provisioned resources, mapped onto apps at absolute paths and delivered to app code as a filesystem path and nothing else. Two kinds, one admin surface — a volume is a real POSIX filesystem, a store is an object-store prefix. The durable resource always pre-exists; Launchpad creates only the objects that bind it to a workload.';

--
-- Name: store_grants; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.store_grants (
    id text NOT NULL,
    resource_id text NOT NULL,
    subject_type text NOT NULL,
    subject_id text NOT NULL,
    access text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    folder text DEFAULT ''::text NOT NULL,
    CONSTRAINT store_grants_access_check CHECK ((access = ANY (ARRAY['read'::text, 'write'::text, 'overwrite'::text, 'full'::text]))),
    CONSTRAINT store_grants_subject_type_check CHECK ((subject_type = ANY (ARRAY['user'::text, 'group'::text])))
);

--
-- Name: TABLE store_grants; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.store_grants IS 'Who may reach a store from outside the platform: resource to person, with no path and no ordering against a deploy. Deliberately not a generalization of storage_mappings, and not an object_grants row — object_grants.object_id references apps(id) and carries no type discriminator. The grant is on the resource and therefore on the whole prefix; there are no sub-prefix grants.';

--
-- Name: COLUMN store_grants.subject_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.store_grants.subject_id IS 'A user id or a group id, told apart by subject_type. No foreign key, for object_grants.principal_id''s reason.';

--
-- Name: tags; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.tags (
    id text NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    kind text DEFAULT 'custom'::text NOT NULL,
    color text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE tags; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.tags IS 'Tags are keys; an app holds at most one value per tag, enforced by the app_tags primary key. Admins define the keys; users type the values. The one tag that ships by default is Framework, whose values are derived from apps.framework at read time and never stored in app_tags.';

--
-- Name: user_api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_api_keys (
    id text NOT NULL,
    user_id text NOT NULL,
    name text NOT NULL,
    key_hash text NOT NULL,
    key_prefix text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    disabled_at timestamp with time zone,
    last_used_at timestamp with time zone,
    last_used_ip text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE user_api_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.user_api_keys IS 'Personal API keys: a long-lived credential that authenticates its owner to the platform API and to the apps gateway. Unlike app_api_keys.created_by, user_id is ON DELETE CASCADE — an app key belongs to the app and outlives its creator, while a personal key is the person and must not outlive them.';

--
-- Name: COLUMN user_api_keys.key_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_api_keys.key_hash IS 'A SHA-256 of the secret — see app_api_keys.key_hash.';

--
-- Name: user_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_identities (
    provider_id text NOT NULL,
    subject text NOT NULL,
    user_id text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    login text DEFAULT ''::text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    avatar_url text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_login_at timestamp with time zone,
    external_id text DEFAULT ''::text NOT NULL
);

--
-- Name: TABLE user_identities; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.user_identities IS 'One external account, one platform user. No foreign key to auth_providers, deliberately: a provider may come from the operator''s environment with no row at all, so the constraint would reject the identity of anyone signing in through it, and deleting a managed provider must not cascade away the record of who its users were.';

--
-- Name: COLUMN user_identities.provider_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_identities.provider_id IS 'No foreign key: a provider may come from the operator''s environment with no auth_providers row at all, so the constraint would reject the identity of anyone signing in through it.';

--
-- Name: COLUMN user_identities.external_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_identities.external_id IS 'The provider''s own id for the account, not a Launchpad reference.';

--
-- Name: user_passwords; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.user_passwords (
    user_id text NOT NULL,
    password_hash text NOT NULL,
    must_change boolean DEFAULT false NOT NULL,
    failed_attempts integer DEFAULT 0 NOT NULL,
    locked_until timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

--
-- Name: TABLE user_passwords; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.user_passwords IS 'One password, one account. Not a column on users, because a password is a credential and users is a directory: an install that never uses local sign-in has an empty table here, and nothing about a person implies one exists.';

--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id text NOT NULL,
    login text NOT NULL,
    email text DEFAULT ''::text NOT NULL,
    avatar_url text DEFAULT ''::text NOT NULL,
    name text DEFAULT ''::text NOT NULL,
    system_role text DEFAULT 'viewer'::text NOT NULL,
    token_version integer DEFAULT 0 NOT NULL,
    locked_at timestamp with time zone,
    last_login_at timestamp with time zone,
    unlocked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    pending_activation_at timestamp with time zone
);

--
-- Name: TABLE users; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.users IS 'The platform''s notion of a person. Which external accounts prove you are that person is a separate, many-to-one fact — see user_identities.';

--
-- Name: COLUMN users.id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.id IS 'text, not uuid, and it stays text. Three seeded ids are not UUIDs — builtin-admin, local and tag-framework — they are compiled into the binary and asserted by TestFreshDatabaseCarriesItsSeeds, and a uuid column would refuse all three. The price is named rather than hidden: every index entry carrying an id pays 36 bytes for it.';

--
-- Name: advisory_affected id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.advisory_affected ALTER COLUMN id SET DEFAULT nextval('public.advisory_affected_id_seq'::regclass);

--
-- Name: app_consumption_intervals id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_consumption_intervals ALTER COLUMN id SET DEFAULT nextval('public.app_consumption_intervals_id_seq'::regclass);

--
-- Name: app_platform_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_platform_log ALTER COLUMN id SET DEFAULT nextval('public.app_platform_log_id_seq'::regclass);

--
-- Name: cron_run_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_run_logs ALTER COLUMN id SET DEFAULT nextval('public.cron_run_logs_id_seq'::regclass);

--
-- Name: job_run_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_run_logs ALTER COLUMN id SET DEFAULT nextval('public.job_run_logs_id_seq'::regclass);

--
-- Name: license_usage_samples id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.license_usage_samples ALTER COLUMN id SET DEFAULT nextval('public.license_usage_samples_id_seq'::regclass);

--
-- Name: access_requests access_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_requests
    ADD CONSTRAINT access_requests_pkey PRIMARY KEY (id);

--
-- Name: access_requests access_requests_requested_role_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.access_requests
    ADD CONSTRAINT access_requests_requested_role_check CHECK ((requested_role = ANY (ARRAY[''::text, 'viewer'::text, 'editor'::text, 'publisher'::text]))) NOT VALID;

--
-- Name: advisories advisories_kind_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.advisories
    ADD CONSTRAINT advisories_kind_check CHECK ((kind = ANY (ARRAY['vuln'::text, 'malware'::text]))) NOT VALID;

--
-- Name: advisories advisories_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.advisories
    ADD CONSTRAINT advisories_pkey PRIMARY KEY (id);

--
-- Name: advisories advisories_severity_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.advisories
    ADD CONSTRAINT advisories_severity_check CHECK ((severity = ANY (ARRAY[''::text, 'critical'::text, 'high'::text, 'moderate'::text, 'low'::text]))) NOT VALID;

--
-- Name: advisory_affected advisory_affected_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.advisory_affected
    ADD CONSTRAINT advisory_affected_pkey PRIMARY KEY (id);

--
-- Name: advisory_feed_state advisory_feed_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.advisory_feed_state
    ADD CONSTRAINT advisory_feed_state_pkey PRIMARY KEY (ecosystem);

--
-- Name: allowed_git_sources allowed_git_sources_pattern_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.allowed_git_sources
    ADD CONSTRAINT allowed_git_sources_pattern_key UNIQUE (pattern);

--
-- Name: allowed_git_sources allowed_git_sources_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.allowed_git_sources
    ADD CONSTRAINT allowed_git_sources_pkey PRIMARY KEY (id);

--
-- Name: announcement_email_recipients announcement_email_recipients_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_email_recipients
    ADD CONSTRAINT announcement_email_recipients_pkey PRIMARY KEY (id);

--
-- Name: announcement_email_recipients announcement_email_recipients_status_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.announcement_email_recipients
    ADD CONSTRAINT announcement_email_recipients_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'delivered'::text, 'failed'::text, 'skipped'::text, 'abandoned'::text]))) NOT VALID;

--
-- Name: announcement_emails announcement_emails_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_emails
    ADD CONSTRAINT announcement_emails_pkey PRIMARY KEY (id);

--
-- Name: announcement_emails announcement_emails_status_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.announcement_emails
    ADD CONSTRAINT announcement_emails_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'sending'::text, 'sent'::text]))) NOT VALID;

--
-- Name: api_key_usage_daily api_key_usage_daily_key_kind_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.api_key_usage_daily
    ADD CONSTRAINT api_key_usage_daily_key_kind_check CHECK ((key_kind = ANY (ARRAY['app'::text, 'user'::text]))) NOT VALID;

--
-- Name: api_key_usage_daily api_key_usage_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_key_usage_daily
    ADD CONSTRAINT api_key_usage_daily_pkey PRIMARY KEY (key_id, day, app_id, status_class);

--
-- Name: app_api_keys app_api_keys_app_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_api_keys
    ADD CONSTRAINT app_api_keys_app_id_name_key UNIQUE (app_id, name);

--
-- Name: app_api_keys app_api_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_api_keys
    ADD CONSTRAINT app_api_keys_key_hash_key UNIQUE (key_hash);

--
-- Name: app_api_keys app_api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_api_keys
    ADD CONSTRAINT app_api_keys_pkey PRIMARY KEY (id);

--
-- Name: app_capability_usage_daily app_capability_usage_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_capability_usage_daily
    ADD CONSTRAINT app_capability_usage_daily_pkey PRIMARY KEY (app_id, capability, day);

--
-- Name: app_consumption_daily app_consumption_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_consumption_daily
    ADD CONSTRAINT app_consumption_daily_pkey PRIMARY KEY (app_id, day);

--
-- Name: app_consumption_intervals app_consumption_intervals_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_consumption_intervals
    ADD CONSTRAINT app_consumption_intervals_pkey PRIMARY KEY (id);

--
-- Name: app_credentials app_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_credentials
    ADD CONSTRAINT app_credentials_pkey PRIMARY KEY (app_id, credential_id);

--
-- Name: app_cron_jobs app_cron_jobs_app_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_cron_jobs
    ADD CONSTRAINT app_cron_jobs_app_id_name_key UNIQUE (app_id, name);

--
-- Name: app_cron_jobs app_cron_jobs_method_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.app_cron_jobs
    ADD CONSTRAINT app_cron_jobs_method_check CHECK ((method = ANY (ARRAY['GET'::text, 'POST'::text]))) NOT VALID;

--
-- Name: app_cron_jobs app_cron_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_cron_jobs
    ADD CONSTRAINT app_cron_jobs_pkey PRIMARY KEY (id);

--
-- Name: app_cron_runs app_cron_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_cron_runs
    ADD CONSTRAINT app_cron_runs_pkey PRIMARY KEY (id);

--
-- Name: app_cron_runs app_cron_runs_status_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.app_cron_runs
    ADD CONSTRAINT app_cron_runs_status_check CHECK ((status = ANY (ARRAY['running'::text, 'success'::text, 'failed'::text, 'skipped'::text]))) NOT VALID;

--
-- Name: app_cron_runs app_cron_runs_trigger_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.app_cron_runs
    ADD CONSTRAINT app_cron_runs_trigger_check CHECK ((trigger = ANY (ARRAY['cron'::text, 'manual'::text, 'app'::text]))) NOT VALID;

--
-- Name: app_data app_data_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_data
    ADD CONSTRAINT app_data_pkey PRIMARY KEY (app_id);

--
-- Name: app_events app_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_events
    ADD CONSTRAINT app_events_pkey PRIMARY KEY (id);

--
-- Name: app_events app_events_source_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.app_events
    ADD CONSTRAINT app_events_source_check CHECK ((source = ANY (ARRAY['builder'::text, 'autodeploy'::text, 'health'::text, 'cron'::text, 'jobs'::text, 'integrations'::text, 'reachability'::text]))) NOT VALID;

--
-- Name: app_integration_sends app_integration_sends_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_integration_sends
    ADD CONSTRAINT app_integration_sends_pkey PRIMARY KEY (id);

--
-- Name: app_integration_sends app_integration_sends_status_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.app_integration_sends
    ADD CONSTRAINT app_integration_sends_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'sent'::text, 'failed'::text, 'blocked'::text, 'abandoned'::text]))) NOT VALID;

--
-- Name: app_integrations app_integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_integrations
    ADD CONSTRAINT app_integrations_pkey PRIMARY KEY (app_id, kind);

--
-- Name: app_platform_log app_platform_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_platform_log
    ADD CONSTRAINT app_platform_log_pkey PRIMARY KEY (id);

--
-- Name: app_reachability app_reachability_last_verdict_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.app_reachability
    ADD CONSTRAINT app_reachability_last_verdict_check CHECK ((last_verdict = ANY (ARRAY[''::text, 'ok'::text, 'not_ok'::text, 'unknown'::text]))) NOT VALID;

--
-- Name: app_reachability app_reachability_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_reachability
    ADD CONSTRAINT app_reachability_pkey PRIMARY KEY (app_id);

--
-- Name: app_render_schedule_recipients app_render_schedule_recipients_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_render_schedule_recipients
    ADD CONSTRAINT app_render_schedule_recipients_pkey PRIMARY KEY (schedule_id, user_id);

--
-- Name: app_render_schedules app_render_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_render_schedules
    ADD CONSTRAINT app_render_schedules_pkey PRIMARY KEY (id);

--
-- Name: app_tags app_tags_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_tags
    ADD CONSTRAINT app_tags_pkey PRIMARY KEY (app_id, tag_id);

--
-- Name: app_tokens app_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_tokens
    ADD CONSTRAINT app_tokens_pkey PRIMARY KEY (id);

--
-- Name: app_tokens app_tokens_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_tokens
    ADD CONSTRAINT app_tokens_token_hash_key UNIQUE (token_hash);

--
-- Name: app_usage_daily app_usage_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_usage_daily
    ADD CONSTRAINT app_usage_daily_pkey PRIMARY KEY (app_id, day, user_id);

--
-- Name: apps apps_gallery_kind_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.apps
    ADD CONSTRAINT apps_gallery_kind_check CHECK ((gallery_kind = ANY (ARRAY[''::text, 'example'::text, 'extension'::text]))) NOT VALID;

--
-- Name: apps apps_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_pkey PRIMARY KEY (id);

--
-- Name: apps apps_running_mode_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.apps
    ADD CONSTRAINT apps_running_mode_check CHECK ((running_mode = ANY (ARRAY[''::text, 'shared'::text, 'isolated'::text]))) NOT VALID;

--
-- Name: apps apps_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_slug_key UNIQUE (slug);

--
-- Name: apps apps_status_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.apps
    ADD CONSTRAINT apps_status_check CHECK ((status = ANY (ARRAY['building'::text, 'running'::text, 'crashed'::text, 'stopped'::text, 'failed'::text, 'sleeping'::text]))) NOT VALID;

--
-- Name: apps apps_visibility_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.apps
    ADD CONSTRAINT apps_visibility_check CHECK ((visibility = ANY (ARRAY['public'::text, 'authenticated'::text, 'private'::text]))) NOT VALID;

--
-- Name: audit_log audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id);

--
-- Name: auth_providers auth_providers_default_role_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.auth_providers
    ADD CONSTRAINT auth_providers_default_role_check CHECK ((default_role = ANY (ARRAY['system_admin'::text, 'publisher'::text, 'viewer'::text, 'pending'::text]))) NOT VALID;

--
-- Name: auth_providers auth_providers_kind_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.auth_providers
    ADD CONSTRAINT auth_providers_kind_check CHECK ((kind = ANY (ARRAY['oidc'::text, 'github'::text, 'password'::text]))) NOT VALID;

--
-- Name: auth_providers auth_providers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_providers
    ADD CONSTRAINT auth_providers_pkey PRIMARY KEY (id);

--
-- Name: cli_device_codes cli_device_codes_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cli_device_codes
    ADD CONSTRAINT cli_device_codes_pkey PRIMARY KEY (device_code_hash);

--
-- Name: cli_device_codes cli_device_codes_user_code_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cli_device_codes
    ADD CONSTRAINT cli_device_codes_user_code_key UNIQUE (user_code);

--
-- Name: credential_values credential_values_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credential_values
    ADD CONSTRAINT credential_values_pkey PRIMARY KEY (credential_id, key);

--
-- Name: credentials credentials_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credentials
    ADD CONSTRAINT credentials_name_key UNIQUE (name);

--
-- Name: credentials credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credentials
    ADD CONSTRAINT credentials_pkey PRIMARY KEY (id);

--
-- Name: cron_run_logs cron_run_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_run_logs
    ADD CONSTRAINT cron_run_logs_pkey PRIMARY KEY (id);

--
-- Name: dependency_waivers dependency_waivers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dependency_waivers
    ADD CONSTRAINT dependency_waivers_pkey PRIMARY KEY (id);

--
-- Name: deploy_keys deploy_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deploy_keys
    ADD CONSTRAINT deploy_keys_key_hash_key UNIQUE (key_hash);

--
-- Name: deploy_keys deploy_keys_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deploy_keys
    ADD CONSTRAINT deploy_keys_name_key UNIQUE (name);

--
-- Name: deploy_keys deploy_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deploy_keys
    ADD CONSTRAINT deploy_keys_pkey PRIMARY KEY (id);

--
-- Name: deployment_dependencies deployment_dependencies_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_dependencies
    ADD CONSTRAINT deployment_dependencies_pkey PRIMARY KEY (deployment_id, ecosystem, package, version);

--
-- Name: deployments deployments_execution_mode_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.deployments
    ADD CONSTRAINT deployments_execution_mode_check CHECK ((execution_mode = ANY (ARRAY['shared'::text, 'isolated'::text]))) NOT VALID;

--
-- Name: deployments deployments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_pkey PRIMARY KEY (id);

--
-- Name: deployments deployments_status_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.deployments
    ADD CONSTRAINT deployments_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'building'::text, 'success'::text, 'failed'::text]))) NOT VALID;

--
-- Name: deployments deployments_triggered_by_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.deployments
    ADD CONSTRAINT deployments_triggered_by_check CHECK ((triggered_by = ANY (ARRAY['manual'::text, 'auto'::text, 'rollback'::text, 'render'::text]))) NOT VALID;

--
-- Name: encryption_data_keys encryption_data_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.encryption_data_keys
    ADD CONSTRAINT encryption_data_keys_pkey PRIMARY KEY (id);

--
-- Name: env_vars env_vars_app_id_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.env_vars
    ADD CONSTRAINT env_vars_app_id_key_key UNIQUE (app_id, key);

--
-- Name: env_vars env_vars_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.env_vars
    ADD CONSTRAINT env_vars_pkey PRIMARY KEY (id);

--
-- Name: features features_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.features
    ADD CONSTRAINT features_pkey PRIMARY KEY (name);

--
-- Name: gallery_catalog gallery_catalog_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.gallery_catalog
    ADD CONSTRAINT gallery_catalog_pkey PRIMARY KEY (id);

--
-- Name: gallery_catalog gallery_catalog_status_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.gallery_catalog
    ADD CONSTRAINT gallery_catalog_status_check CHECK ((status = ANY (ARRAY[''::text, 'ok'::text, 'unreachable'::text, 'unparseable'::text, 'too_new'::text]))) NOT VALID;

--
-- Name: git_connection_identities git_connection_identities_connection_id_user_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_connection_identities
    ADD CONSTRAINT git_connection_identities_connection_id_user_id_key UNIQUE (connection_id, user_id);

--
-- Name: git_connection_identities git_connection_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_connection_identities
    ADD CONSTRAINT git_connection_identities_pkey PRIMARY KEY (id);

--
-- Name: git_connections git_connections_credential_kind_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.git_connections
    ADD CONSTRAINT git_connections_credential_kind_check CHECK ((credential_kind = ANY (ARRAY[''::text, 'fine_grained'::text, 'classic'::text, 'group'::text, 'app'::text]))) NOT VALID;

--
-- Name: git_connections git_connections_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_connections
    ADD CONSTRAINT git_connections_pkey PRIMARY KEY (id);

--
-- Name: group_claim_mappings group_claim_mappings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.group_claim_mappings
    ADD CONSTRAINT group_claim_mappings_pkey PRIMARY KEY (group_id, provider_id, claim_value);

--
-- Name: group_members group_members_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.group_members
    ADD CONSTRAINT group_members_pkey PRIMARY KEY (group_id, user_id);

--
-- Name: groups groups_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.groups
    ADD CONSTRAINT groups_name_key UNIQUE (name);

--
-- Name: groups groups_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.groups
    ADD CONSTRAINT groups_pkey PRIMARY KEY (id);

--
-- Name: integration_connections integration_connections_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.integration_connections
    ADD CONSTRAINT integration_connections_pkey PRIMARY KEY (id);

--
-- Name: integration_placements integration_placements_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.integration_placements
    ADD CONSTRAINT integration_placements_pkey PRIMARY KEY (id);

--
-- Name: job_definitions job_definitions_app_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_definitions
    ADD CONSTRAINT job_definitions_app_id_name_key UNIQUE (app_id, name);

--
-- Name: job_definitions job_definitions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_definitions
    ADD CONSTRAINT job_definitions_pkey PRIMARY KEY (id);

--
-- Name: job_definitions job_definitions_source_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.job_definitions
    ADD CONSTRAINT job_definitions_source_check CHECK ((source = ANY (ARRAY['manifest'::text, 'ui'::text]))) NOT VALID;

--
-- Name: job_run_logs job_run_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_run_logs
    ADD CONSTRAINT job_run_logs_pkey PRIMARY KEY (id);

--
-- Name: job_runs job_runs_execution_mode_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.job_runs
    ADD CONSTRAINT job_runs_execution_mode_check CHECK ((execution_mode = ANY (ARRAY['shared'::text, 'isolated'::text]))) NOT VALID;

--
-- Name: job_runs job_runs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_runs
    ADD CONSTRAINT job_runs_pkey PRIMARY KEY (id);

--
-- Name: job_runs job_runs_trigger_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.job_runs
    ADD CONSTRAINT job_runs_trigger_check CHECK ((trigger = ANY (ARRAY['cron'::text, 'manual'::text, 'api'::text, 'app'::text]))) NOT VALID;

--
-- Name: license_usage_samples license_usage_samples_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.license_usage_samples
    ADD CONSTRAINT license_usage_samples_pkey PRIMARY KEY (id);

--
-- Name: object_grants object_grants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.object_grants
    ADD CONSTRAINT object_grants_pkey PRIMARY KEY (object_id, principal_type, principal_id);

--
-- Name: report_deliveries report_deliveries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.report_deliveries
    ADD CONSTRAINT report_deliveries_pkey PRIMARY KEY (id);

--
-- Name: report_delivery_recipients report_delivery_recipients_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.report_delivery_recipients
    ADD CONSTRAINT report_delivery_recipients_pkey PRIMARY KEY (id);

--
-- Name: settings settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.settings
    ADD CONSTRAINT settings_pkey PRIMARY KEY (key);

--
-- Name: storage_managers storage_managers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_managers
    ADD CONSTRAINT storage_managers_pkey PRIMARY KEY (id);

--
-- Name: storage_managers storage_managers_unique_subject; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_managers
    ADD CONSTRAINT storage_managers_unique_subject UNIQUE (resource_id, subject_type, subject_id);

--
-- Name: storage_mappings storage_mappings_app_id_path_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_mappings
    ADD CONSTRAINT storage_mappings_app_id_path_key UNIQUE (app_id, path);

--
-- Name: storage_mappings storage_mappings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_mappings
    ADD CONSTRAINT storage_mappings_pkey PRIMARY KEY (id);

--
-- Name: storage_mappings storage_mappings_resource_app_folder_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_mappings
    ADD CONSTRAINT storage_mappings_resource_app_folder_key UNIQUE (resource_id, app_id, folder);

--
-- Name: storage_resources storage_resources_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_resources
    ADD CONSTRAINT storage_resources_pkey PRIMARY KEY (id);

--
-- Name: storage_resources storage_resources_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_resources
    ADD CONSTRAINT storage_resources_slug_key UNIQUE (slug);

--
-- Name: store_grants store_grants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.store_grants
    ADD CONSTRAINT store_grants_pkey PRIMARY KEY (id);

--
-- Name: store_grants store_grants_unique_subject; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.store_grants
    ADD CONSTRAINT store_grants_unique_subject UNIQUE (resource_id, subject_type, subject_id, folder);

--
-- Name: tags tags_kind_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.tags
    ADD CONSTRAINT tags_kind_check CHECK ((kind = ANY (ARRAY['system'::text, 'custom'::text]))) NOT VALID;

--
-- Name: tags tags_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tags
    ADD CONSTRAINT tags_pkey PRIMARY KEY (id);

--
-- Name: tags tags_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tags
    ADD CONSTRAINT tags_slug_key UNIQUE (slug);

--
-- Name: user_api_keys user_api_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_api_keys
    ADD CONSTRAINT user_api_keys_key_hash_key UNIQUE (key_hash);

--
-- Name: user_api_keys user_api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_api_keys
    ADD CONSTRAINT user_api_keys_pkey PRIMARY KEY (id);

--
-- Name: user_api_keys user_api_keys_user_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_api_keys
    ADD CONSTRAINT user_api_keys_user_id_name_key UNIQUE (user_id, name);

--
-- Name: user_identities user_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_identities
    ADD CONSTRAINT user_identities_pkey PRIMARY KEY (provider_id, subject);

--
-- Name: user_passwords user_passwords_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_passwords
    ADD CONSTRAINT user_passwords_pkey PRIMARY KEY (user_id);

--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

--
-- Name: users users_system_role_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.users
    ADD CONSTRAINT users_system_role_check CHECK ((system_role = ANY (ARRAY['system_admin'::text, 'publisher'::text, 'viewer'::text]))) NOT VALID;

--
-- Name: access_requests_approver_group_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_requests_approver_group_idx ON public.access_requests USING btree (approver_group_id);

--
-- Name: access_requests_approver_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_requests_approver_idx ON public.access_requests USING btree (approver_id) WHERE (approver_id IS NOT NULL);

--
-- Name: access_requests_object_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_requests_object_idx ON public.access_requests USING btree (object_id);

--
-- Name: access_requests_one_pending_promotion_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX access_requests_one_pending_promotion_idx ON public.access_requests USING btree (requester_id) WHERE ((request_type = 'role_promotion'::text) AND (status = 'pending'::text));

--
-- Name: access_requests_requester_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_requests_requester_idx ON public.access_requests USING btree (requester_id);

--
-- Name: access_requests_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX access_requests_status_idx ON public.access_requests USING btree (status);

--
-- Name: advisories_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX advisories_kind_idx ON public.advisories USING btree (kind);

--
-- Name: advisory_affected_advisory_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX advisory_affected_advisory_idx ON public.advisory_affected USING btree (advisory_id);

--
-- Name: advisory_affected_pkg_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX advisory_affected_pkg_idx ON public.advisory_affected USING btree (ecosystem, package);

--
-- Name: allowed_git_sources_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX allowed_git_sources_created_by_idx ON public.allowed_git_sources USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: announcement_email_recipients_announcement_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX announcement_email_recipients_announcement_idx ON public.announcement_email_recipients USING btree (announcement_id, status);

--
-- Name: announcement_email_recipients_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX announcement_email_recipients_user_idx ON public.announcement_email_recipients USING btree (user_id) WHERE (user_id IS NOT NULL);

--
-- Name: announcement_emails_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX announcement_emails_created_by_idx ON public.announcement_emails USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: announcement_emails_sending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX announcement_emails_sending_idx ON public.announcement_emails USING btree (created_at) WHERE (status = 'sending'::text);

--
-- Name: announcement_emails_sent_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX announcement_emails_sent_by_idx ON public.announcement_emails USING btree (sent_by) WHERE (sent_by IS NOT NULL);

--
-- Name: announcement_emails_status_created_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX announcement_emails_status_created_idx ON public.announcement_emails USING btree (status, created_at DESC);

--
-- Name: api_key_usage_daily_day_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_key_usage_daily_day_idx ON public.api_key_usage_daily USING btree (day);

--
-- Name: app_api_keys_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_api_keys_created_by_idx ON public.app_api_keys USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: app_consumption_intervals_app_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_consumption_intervals_app_time_idx ON public.app_consumption_intervals USING btree (app_id, started_at);

--
-- Name: app_consumption_intervals_ended_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_consumption_intervals_ended_idx ON public.app_consumption_intervals USING btree (ended_at);

--
-- Name: app_consumption_intervals_run_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_consumption_intervals_run_idx ON public.app_consumption_intervals USING btree (job_run_id, started_at) WHERE (job_run_id <> ''::text);

--
-- Name: app_credentials_attached_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_credentials_attached_by_idx ON public.app_credentials USING btree (attached_by) WHERE (attached_by IS NOT NULL);

--
-- Name: app_credentials_credential_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_credentials_credential_idx ON public.app_credentials USING btree (credential_id);

--
-- Name: app_cron_jobs_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_cron_jobs_due_idx ON public.app_cron_jobs USING btree (next_run_at) WHERE enabled;

--
-- Name: app_cron_runs_actor_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_cron_runs_actor_idx ON public.app_cron_runs USING btree (actor_id) WHERE (actor_id IS NOT NULL);

--
-- Name: app_cron_runs_age_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_cron_runs_age_idx ON public.app_cron_runs USING btree (started_at);

--
-- Name: app_cron_runs_inflight_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_cron_runs_inflight_idx ON public.app_cron_runs USING btree (job_id) WHERE (status = 'running'::text);

--
-- Name: app_cron_runs_job_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_cron_runs_job_idx ON public.app_cron_runs USING btree (job_id, started_at DESC);

--
-- Name: app_cron_runs_retry_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_cron_runs_retry_due_idx ON public.app_cron_runs USING btree (retry_at) WHERE (retry_at IS NOT NULL);

--
-- Name: app_events_app_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_events_app_time_idx ON public.app_events USING btree (app_id, occurred_at DESC);

--
-- Name: app_events_open_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_events_open_idx ON public.app_events USING btree (app_id, kind, fingerprint) WHERE (resolved_at IS NULL);

--
-- Name: app_events_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_events_time_idx ON public.app_events USING btree (occurred_at DESC);

--
-- Name: app_integration_sends_app_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_integration_sends_app_time_idx ON public.app_integration_sends USING btree (app_id, created_at DESC);

--
-- Name: app_integration_sends_idem_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX app_integration_sends_idem_idx ON public.app_integration_sends USING btree (app_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);

--
-- Name: app_integration_sends_queued_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_integration_sends_queued_idx ON public.app_integration_sends USING btree (created_at) WHERE (status = 'queued'::text);

--
-- Name: app_integration_sends_recipients_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_integration_sends_recipients_idx ON public.app_integration_sends USING gin (recipients jsonb_path_ops);

--
-- Name: app_integration_sends_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_integration_sends_time_idx ON public.app_integration_sends USING btree (created_at DESC);

--
-- Name: app_integrations_attached_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_integrations_attached_by_idx ON public.app_integrations USING btree (attached_by) WHERE (attached_by IS NOT NULL);

--
-- Name: app_integrations_connection_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_integrations_connection_idx ON public.app_integrations USING btree (connection_id);

--
-- Name: app_platform_log_app_time_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_platform_log_app_time_idx ON public.app_platform_log USING btree (app_id, at DESC, seq DESC);

--
-- Name: app_reachability_suspended_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_reachability_suspended_idx ON public.app_reachability USING btree (app_id) WHERE (suspended_at IS NOT NULL);

--
-- Name: app_render_schedule_recipients_added_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_render_schedule_recipients_added_by_idx ON public.app_render_schedule_recipients USING btree (added_by) WHERE (added_by IS NOT NULL);

--
-- Name: app_render_schedule_recipients_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_render_schedule_recipients_user_idx ON public.app_render_schedule_recipients USING btree (user_id);

--
-- Name: app_render_schedules_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX app_render_schedules_app_idx ON public.app_render_schedules USING btree (app_id);

--
-- Name: app_render_schedules_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_render_schedules_due_idx ON public.app_render_schedules USING btree (next_run_at) WHERE enabled;

--
-- Name: app_tags_added_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_tags_added_by_idx ON public.app_tags USING btree (added_by) WHERE (added_by IS NOT NULL);

--
-- Name: app_tags_lookup_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_tags_lookup_idx ON public.app_tags USING btree (tag_id, value_slug);

--
-- Name: app_tokens_workload_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_tokens_workload_idx ON public.app_tokens USING btree (app_id, workload) WHERE (revoked_at IS NULL);

--
-- Name: apps_endpoint_key_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX apps_endpoint_key_idx ON public.apps USING btree (endpoint_host, port);

--
-- Name: apps_gallery_item_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_gallery_item_idx ON public.apps USING btree (gallery_item_id) WHERE (gallery_item_id <> ''::text);

--
-- Name: apps_git_connection_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_git_connection_idx ON public.apps USING btree (git_connection_id) WHERE (git_connection_id IS NOT NULL);

--
-- Name: apps_locked_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_locked_by_idx ON public.apps USING btree (locked_by) WHERE (locked_by IS NOT NULL);

--
-- Name: apps_locked_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_locked_idx ON public.apps USING btree (locked_at) WHERE (locked_at IS NOT NULL);

--
-- Name: apps_owner_group_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_owner_group_idx ON public.apps USING btree (owner_group_id);

--
-- Name: apps_owner_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX apps_owner_user_idx ON public.apps USING btree (owner_user_id);

--
-- Name: audit_log_actor_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_log_actor_idx ON public.audit_log USING btree (actor_type, actor_id);

--
-- Name: audit_log_occurred_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_log_occurred_idx ON public.audit_log USING btree (occurred_at);

--
-- Name: audit_log_target_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_log_target_idx ON public.audit_log USING btree (target_type, target_id);

--
-- Name: auth_providers_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX auth_providers_created_by_idx ON public.auth_providers USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: cli_device_codes_approved_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cli_device_codes_approved_user_idx ON public.cli_device_codes USING btree (approved_user_id) WHERE (approved_user_id IS NOT NULL);

--
-- Name: cli_device_codes_expires_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cli_device_codes_expires_idx ON public.cli_device_codes USING btree (expires_at);

--
-- Name: credentials_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX credentials_created_by_idx ON public.credentials USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: cron_run_logs_age_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_run_logs_age_idx ON public.cron_run_logs USING btree (at);

--
-- Name: cron_run_logs_run_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX cron_run_logs_run_idx ON public.cron_run_logs USING btree (run_id, id);

--
-- Name: dependency_waivers_advisory_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX dependency_waivers_advisory_idx ON public.dependency_waivers USING btree (advisory_id);

--
-- Name: dependency_waivers_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX dependency_waivers_app_idx ON public.dependency_waivers USING btree (app_id);

--
-- Name: dependency_waivers_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX dependency_waivers_created_by_idx ON public.dependency_waivers USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: deploy_keys_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deploy_keys_created_by_idx ON public.deploy_keys USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: deployment_dependencies_pkg_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deployment_dependencies_pkg_idx ON public.deployment_dependencies USING btree (ecosystem, package);

--
-- Name: deployments_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX deployments_app_idx ON public.deployments USING btree (app_id);

--
-- Name: encryption_data_keys_one_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX encryption_data_keys_one_active_idx ON public.encryption_data_keys USING btree (active) WHERE active;

--
-- Name: features_enabled_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX features_enabled_by_idx ON public.features USING btree (enabled_by) WHERE (enabled_by IS NOT NULL);

--
-- Name: git_connection_identities_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_connection_identities_user_idx ON public.git_connection_identities USING btree (user_id);

--
-- Name: git_connections_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX git_connections_created_by_idx ON public.git_connections USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: group_claim_mappings_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX group_claim_mappings_created_by_idx ON public.group_claim_mappings USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: group_claim_mappings_lookup_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX group_claim_mappings_lookup_idx ON public.group_claim_mappings USING btree (provider_id, claim_value);

--
-- Name: group_members_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX group_members_user_idx ON public.group_members USING btree (user_id);

--
-- Name: integration_connections_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX integration_connections_created_by_idx ON public.integration_connections USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: integration_connections_kind_name_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX integration_connections_kind_name_idx ON public.integration_connections USING btree (kind, lower(name));

--
-- Name: integration_placements_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX integration_placements_app_idx ON public.integration_placements USING btree (app_id, placed_at DESC);

--
-- Name: integration_placements_connection_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX integration_placements_connection_idx ON public.integration_placements USING btree (connection_id, placed_at DESC);

--
-- Name: job_definitions_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_definitions_due_idx ON public.job_definitions USING btree (next_run_at) WHERE (enabled AND (schedule IS NOT NULL));

--
-- Name: job_run_logs_run_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_run_logs_run_idx ON public.job_run_logs USING btree (run_id, seq);

--
-- Name: job_runs_active_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_active_idx ON public.job_runs USING btree (status) WHERE (status = ANY (ARRAY['claimed'::text, 'running'::text]));

--
-- Name: job_runs_actor_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_actor_idx ON public.job_runs USING btree (actor_id) WHERE (actor_id IS NOT NULL);

--
-- Name: job_runs_app_actor_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_app_actor_idx ON public.job_runs USING btree (app_id, actor_id, queued_at DESC) WHERE (actor_id IS NOT NULL);

--
-- Name: job_runs_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_app_idx ON public.job_runs USING btree (app_id, created_at DESC);

--
-- Name: job_runs_definition_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_definition_idx ON public.job_runs USING btree (definition_id, created_at DESC);

--
-- Name: job_runs_idem_live_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX job_runs_idem_live_idx ON public.job_runs USING btree (app_id, idempotency_key) WHERE ((idempotency_key IS NOT NULL) AND (status = ANY (ARRAY['pending'::text, 'claimed'::text, 'running'::text])));

--
-- Name: job_runs_pending_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_pending_idx ON public.job_runs USING btree (queued_at) WHERE (status = 'pending'::text);

--
-- Name: license_usage_samples_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX license_usage_samples_at_idx ON public.license_usage_samples USING btree (sampled_at DESC);

--
-- Name: object_grants_granted_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX object_grants_granted_by_idx ON public.object_grants USING btree (granted_by) WHERE (granted_by IS NOT NULL);

--
-- Name: object_grants_principal_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX object_grants_principal_idx ON public.object_grants USING btree (principal_type, principal_id);

--
-- Name: report_deliveries_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX report_deliveries_app_idx ON public.report_deliveries USING btree (app_id, created_at DESC);

--
-- Name: report_delivery_recipients_delivery_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX report_delivery_recipients_delivery_idx ON public.report_delivery_recipients USING btree (delivery_id);

--
-- Name: report_delivery_recipients_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX report_delivery_recipients_user_idx ON public.report_delivery_recipients USING btree (user_id) WHERE (user_id IS NOT NULL);

--
-- Name: settings_updated_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX settings_updated_by_idx ON public.settings USING btree (updated_by) WHERE (updated_by IS NOT NULL);

--
-- Name: storage_managers_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_managers_created_by_idx ON public.storage_managers USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: storage_managers_subject_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_managers_subject_idx ON public.storage_managers USING btree (subject_type, subject_id);

--
-- Name: storage_mappings_authority_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_mappings_authority_idx ON public.storage_mappings USING btree (authority);

--
-- Name: storage_mappings_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_mappings_created_by_idx ON public.storage_mappings USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: storage_resources_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX storage_resources_created_by_idx ON public.storage_resources USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: storage_resources_name_lower_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX storage_resources_name_lower_idx ON public.storage_resources USING btree (lower(name));

--
-- Name: store_grants_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX store_grants_created_by_idx ON public.store_grants USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: store_grants_subject_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX store_grants_subject_idx ON public.store_grants USING btree (subject_type, subject_id, folder);

--
-- Name: tags_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX tags_created_by_idx ON public.tags USING btree (created_by) WHERE (created_by IS NOT NULL);

--
-- Name: tags_name_lower_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX tags_name_lower_idx ON public.tags USING btree (lower(name));

--
-- Name: user_api_keys_user_last_used_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_api_keys_user_last_used_idx ON public.user_api_keys USING btree (user_id, last_used_at);

--
-- Name: user_identities_provider_login_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_identities_provider_login_idx ON public.user_identities USING btree (provider_id, login);

--
-- Name: user_identities_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX user_identities_user_idx ON public.user_identities USING btree (user_id);

--
-- Name: group_members group_members_cap_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER group_members_cap_insert BEFORE INSERT ON public.group_members FOR EACH ROW EXECUTE FUNCTION public.enforce_group_membership_cap();

--
-- Name: group_members group_members_cap_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER group_members_cap_update BEFORE UPDATE ON public.group_members FOR EACH ROW WHEN ((new.user_id IS DISTINCT FROM old.user_id)) EXECUTE FUNCTION public.enforce_group_membership_cap();

--
-- Name: storage_managers storage_managers_access_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER storage_managers_access_insert BEFORE INSERT ON public.storage_managers FOR EACH ROW EXECUTE FUNCTION public.enforce_storage_manager_access();

--
-- Name: storage_managers storage_managers_access_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER storage_managers_access_update BEFORE UPDATE ON public.storage_managers FOR EACH ROW EXECUTE FUNCTION public.enforce_storage_manager_access();

--
-- Name: storage_mappings storage_mappings_authority_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER storage_mappings_authority_insert BEFORE INSERT ON public.storage_mappings FOR EACH ROW EXECUTE FUNCTION public.enforce_storage_mapping_authority();

--
-- Name: storage_mappings storage_mappings_authority_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER storage_mappings_authority_update BEFORE UPDATE ON public.storage_mappings FOR EACH ROW EXECUTE FUNCTION public.enforce_storage_mapping_authority();

--
-- Name: store_grants store_grants_bucket_insert; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER store_grants_bucket_insert BEFORE INSERT ON public.store_grants FOR EACH ROW EXECUTE FUNCTION public.enforce_grant_has_bucket();

--
-- Name: store_grants store_grants_bucket_update; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER store_grants_bucket_update BEFORE UPDATE ON public.store_grants FOR EACH ROW WHEN ((new.resource_id IS DISTINCT FROM old.resource_id)) EXECUTE FUNCTION public.enforce_grant_has_bucket();

--
-- Name: access_requests access_requests_approver_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_requests
    ADD CONSTRAINT access_requests_approver_group_id_fkey FOREIGN KEY (approver_group_id) REFERENCES public.groups(id) ON DELETE SET NULL;

--
-- Name: access_requests access_requests_approver_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_requests
    ADD CONSTRAINT access_requests_approver_id_fkey FOREIGN KEY (approver_id) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: access_requests access_requests_object_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_requests
    ADD CONSTRAINT access_requests_object_id_fkey FOREIGN KEY (object_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: access_requests access_requests_requester_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.access_requests
    ADD CONSTRAINT access_requests_requester_id_fkey FOREIGN KEY (requester_id) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: advisory_affected advisory_affected_advisory_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.advisory_affected
    ADD CONSTRAINT advisory_affected_advisory_id_fkey FOREIGN KEY (advisory_id) REFERENCES public.advisories(id) ON DELETE CASCADE;

--
-- Name: allowed_git_sources allowed_git_sources_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.allowed_git_sources
    ADD CONSTRAINT allowed_git_sources_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: announcement_email_recipients announcement_email_recipients_announcement_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_email_recipients
    ADD CONSTRAINT announcement_email_recipients_announcement_id_fkey FOREIGN KEY (announcement_id) REFERENCES public.announcement_emails(id) ON DELETE CASCADE;

--
-- Name: announcement_email_recipients announcement_email_recipients_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_email_recipients
    ADD CONSTRAINT announcement_email_recipients_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: announcement_emails announcement_emails_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_emails
    ADD CONSTRAINT announcement_emails_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: announcement_emails announcement_emails_sent_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.announcement_emails
    ADD CONSTRAINT announcement_emails_sent_by_fkey FOREIGN KEY (sent_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: app_api_keys app_api_keys_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_api_keys
    ADD CONSTRAINT app_api_keys_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_api_keys app_api_keys_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_api_keys
    ADD CONSTRAINT app_api_keys_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: app_credentials app_credentials_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_credentials
    ADD CONSTRAINT app_credentials_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_credentials app_credentials_attached_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_credentials
    ADD CONSTRAINT app_credentials_attached_by_fkey FOREIGN KEY (attached_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: app_credentials app_credentials_credential_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_credentials
    ADD CONSTRAINT app_credentials_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES public.credentials(id) ON DELETE RESTRICT;

--
-- Name: app_cron_jobs app_cron_jobs_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_cron_jobs
    ADD CONSTRAINT app_cron_jobs_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_cron_runs app_cron_runs_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_cron_runs
    ADD CONSTRAINT app_cron_runs_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: app_cron_runs app_cron_runs_job_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_cron_runs
    ADD CONSTRAINT app_cron_runs_job_id_fkey FOREIGN KEY (job_id) REFERENCES public.app_cron_jobs(id) ON DELETE CASCADE;

--
-- Name: app_data app_data_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_data
    ADD CONSTRAINT app_data_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_events app_events_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_events
    ADD CONSTRAINT app_events_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_integration_sends app_integration_sends_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_integration_sends
    ADD CONSTRAINT app_integration_sends_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_integrations app_integrations_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_integrations
    ADD CONSTRAINT app_integrations_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_integrations app_integrations_attached_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_integrations
    ADD CONSTRAINT app_integrations_attached_by_fkey FOREIGN KEY (attached_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: app_integrations app_integrations_connection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_integrations
    ADD CONSTRAINT app_integrations_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES public.integration_connections(id) ON DELETE RESTRICT;

--
-- Name: app_platform_log app_platform_log_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_platform_log
    ADD CONSTRAINT app_platform_log_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_reachability app_reachability_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_reachability
    ADD CONSTRAINT app_reachability_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_render_schedule_recipients app_render_schedule_recipients_added_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_render_schedule_recipients
    ADD CONSTRAINT app_render_schedule_recipients_added_by_fkey FOREIGN KEY (added_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: app_render_schedule_recipients app_render_schedule_recipients_schedule_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_render_schedule_recipients
    ADD CONSTRAINT app_render_schedule_recipients_schedule_id_fkey FOREIGN KEY (schedule_id) REFERENCES public.app_render_schedules(id) ON DELETE CASCADE;

--
-- Name: app_render_schedule_recipients app_render_schedule_recipients_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_render_schedule_recipients
    ADD CONSTRAINT app_render_schedule_recipients_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

--
-- Name: app_render_schedules app_render_schedules_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_render_schedules
    ADD CONSTRAINT app_render_schedules_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_tags app_tags_added_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_tags
    ADD CONSTRAINT app_tags_added_by_fkey FOREIGN KEY (added_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: app_tags app_tags_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_tags
    ADD CONSTRAINT app_tags_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: app_tags app_tags_tag_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_tags
    ADD CONSTRAINT app_tags_tag_id_fkey FOREIGN KEY (tag_id) REFERENCES public.tags(id) ON DELETE CASCADE;

--
-- Name: app_tokens app_tokens_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_tokens
    ADD CONSTRAINT app_tokens_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: apps apps_git_connection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_git_connection_id_fkey FOREIGN KEY (git_connection_id) REFERENCES public.git_connections(id);

--
-- Name: apps apps_locked_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_locked_by_fkey FOREIGN KEY (locked_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: apps apps_owner_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_owner_group_id_fkey FOREIGN KEY (owner_group_id) REFERENCES public.groups(id);

--
-- Name: apps apps_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES public.users(id);

--
-- Name: auth_providers auth_providers_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_providers
    ADD CONSTRAINT auth_providers_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: cli_device_codes cli_device_codes_approved_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cli_device_codes
    ADD CONSTRAINT cli_device_codes_approved_user_id_fkey FOREIGN KEY (approved_user_id) REFERENCES public.users(id) ON DELETE CASCADE;

--
-- Name: credential_values credential_values_credential_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credential_values
    ADD CONSTRAINT credential_values_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES public.credentials(id) ON DELETE CASCADE;

--
-- Name: credentials credentials_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.credentials
    ADD CONSTRAINT credentials_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: cron_run_logs cron_run_logs_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.cron_run_logs
    ADD CONSTRAINT cron_run_logs_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.app_cron_runs(id) ON DELETE CASCADE;

--
-- Name: dependency_waivers dependency_waivers_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dependency_waivers
    ADD CONSTRAINT dependency_waivers_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: dependency_waivers dependency_waivers_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dependency_waivers
    ADD CONSTRAINT dependency_waivers_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: deploy_keys deploy_keys_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deploy_keys
    ADD CONSTRAINT deploy_keys_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: deployment_dependencies deployment_dependencies_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployment_dependencies
    ADD CONSTRAINT deployment_dependencies_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id) ON DELETE CASCADE;

--
-- Name: deployments deployments_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deployments
    ADD CONSTRAINT deployments_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: env_vars env_vars_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.env_vars
    ADD CONSTRAINT env_vars_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: features features_enabled_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.features
    ADD CONSTRAINT features_enabled_by_fkey FOREIGN KEY (enabled_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: git_connection_identities git_connection_identities_connection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_connection_identities
    ADD CONSTRAINT git_connection_identities_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES public.git_connections(id) ON DELETE CASCADE;

--
-- Name: git_connection_identities git_connection_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_connection_identities
    ADD CONSTRAINT git_connection_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

--
-- Name: git_connections git_connections_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.git_connections
    ADD CONSTRAINT git_connections_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: group_claim_mappings group_claim_mappings_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.group_claim_mappings
    ADD CONSTRAINT group_claim_mappings_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: group_claim_mappings group_claim_mappings_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.group_claim_mappings
    ADD CONSTRAINT group_claim_mappings_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;

--
-- Name: group_members group_members_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.group_members
    ADD CONSTRAINT group_members_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;

--
-- Name: group_members group_members_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.group_members
    ADD CONSTRAINT group_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

--
-- Name: integration_connections integration_connections_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.integration_connections
    ADD CONSTRAINT integration_connections_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: job_definitions job_definitions_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_definitions
    ADD CONSTRAINT job_definitions_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: job_run_logs job_run_logs_run_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_run_logs
    ADD CONSTRAINT job_run_logs_run_id_fkey FOREIGN KEY (run_id) REFERENCES public.job_runs(id) ON DELETE CASCADE;

--
-- Name: job_runs job_runs_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_runs
    ADD CONSTRAINT job_runs_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: job_runs job_runs_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_runs
    ADD CONSTRAINT job_runs_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: object_grants object_grants_granted_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.object_grants
    ADD CONSTRAINT object_grants_granted_by_fkey FOREIGN KEY (granted_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: object_grants object_grants_object_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.object_grants
    ADD CONSTRAINT object_grants_object_id_fkey FOREIGN KEY (object_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: report_deliveries report_deliveries_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.report_deliveries
    ADD CONSTRAINT report_deliveries_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: report_delivery_recipients report_delivery_recipients_delivery_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.report_delivery_recipients
    ADD CONSTRAINT report_delivery_recipients_delivery_id_fkey FOREIGN KEY (delivery_id) REFERENCES public.report_deliveries(id) ON DELETE CASCADE;

--
-- Name: report_delivery_recipients report_delivery_recipients_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.report_delivery_recipients
    ADD CONSTRAINT report_delivery_recipients_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: settings settings_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.settings
    ADD CONSTRAINT settings_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: storage_managers storage_managers_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_managers
    ADD CONSTRAINT storage_managers_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: storage_managers storage_managers_resource_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_managers
    ADD CONSTRAINT storage_managers_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES public.storage_resources(id) ON DELETE CASCADE;

--
-- Name: storage_mappings storage_mappings_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_mappings
    ADD CONSTRAINT storage_mappings_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;

--
-- Name: storage_mappings storage_mappings_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_mappings
    ADD CONSTRAINT storage_mappings_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: storage_mappings storage_mappings_resource_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_mappings
    ADD CONSTRAINT storage_mappings_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES public.storage_resources(id) ON DELETE CASCADE;

--
-- Name: storage_resources storage_resources_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_resources
    ADD CONSTRAINT storage_resources_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: store_grants store_grants_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.store_grants
    ADD CONSTRAINT store_grants_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: store_grants store_grants_resource_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.store_grants
    ADD CONSTRAINT store_grants_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES public.storage_resources(id) ON DELETE CASCADE;

--
-- Name: tags tags_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.tags
    ADD CONSTRAINT tags_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;

--
-- Name: user_api_keys user_api_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_api_keys
    ADD CONSTRAINT user_api_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

--
-- Name: user_identities user_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_identities
    ADD CONSTRAINT user_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

--
-- Name: user_passwords user_passwords_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.user_passwords
    ADD CONSTRAINT user_passwords_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;

--
-- PostgreSQL database dump complete
--


--
-- Seed data
--
-- The half a schema-only dump cannot carry, and the half a fresh install does
-- not work without. Six rows.

-- The one tag that ships by default. Its values are derived from apps.framework
-- at read time and are never stored in app_tags.
INSERT INTO tags (id, name, slug, kind, color, description)
VALUES ('tag-framework', 'Framework', 'framework', 'system', 'slate',
        'Detected automatically when an app is deployed.');

-- Local sign-in is a provider row like any other, so it inherits the whole
-- registry: it appears on the login page when enabled, disappears when not, an
-- admin sees it under Admin -> Authentication, and disabling it is audited by
-- the same code path that audits disabling Okta. kind='password' has no client
-- id, no secret and no redirect URI -- there is no third party in the flow --
-- and auto_provision stays FALSE, because a password provider that could create
-- accounts would be a public signup form.
INSERT INTO auth_providers (id, kind, display_name, enabled, client_id, default_role, auto_provision)
VALUES ('local', 'password', 'Local administrator', TRUE, '', 'viewer', FALSE);

-- The built-in local administrator: one door that does not require an identity
-- provider to already be configured. There is deliberately no
-- first-user-becomes-admin rule -- that is a race, not a bootstrap: on a
-- corporate IdP it means whichever employee loads the URL first owns the
-- install. The id is fixed rather than generated because the break-glass path
-- (`launchpad local-admin`) has to find this account on an install nobody can
-- log into, and a generated id would have to be looked up by a login an
-- administrator can rename.
INSERT INTO users (id, login, name, email, avatar_url, system_role)
VALUES ('builtin-admin', 'admin', 'Local administrator', 'admin@launchpad.local', '', 'system_admin');

-- The identity row costs nothing and buys consistency: the account page can say
-- how this user signs in, and the provider's user count is right.
INSERT INTO user_identities (provider_id, subject, user_id, email, login, name)
VALUES ('local', 'builtin-admin', 'builtin-admin', 'admin@launchpad.local', 'admin', 'Local administrator');

-- No password hash is seeded. It is written by the binary at startup instead:
-- hashing is a Go function with cost parameters that will change, and a literal
-- hash here would be a copy of it that cannot be kept in step. See
-- ensureBuiltinAdmin in main.go -- absent credential means "set the default".
-- The default is not a secret and is not treated as one: signing in with it
-- mints no session at all, only the chance to replace it.

-- A name for this install, minted here because nothing can be issued to
-- something that cannot be identified: a licence is attributed to it, and every
-- row a vendor ever holds about this install is keyed on it. Generated rather
-- than derived from the hostname or the cluster, so it does not change when
-- infrastructure moves. ON CONFLICT DO NOTHING because minting a second id
-- would silently orphan everything anybody holds about the first.
INSERT INTO settings (key, value, updated_at)
VALUES ('install.id', gen_random_uuid()::text, NOW())
ON CONFLICT (key) DO NOTHING;

-- The email connection exists on every install, because mail is configured on
-- its own settings page and its "connection" is the install's relay. What this
-- row decides is which apps may send through it. for_platform is FALSE and the
-- audience list empty because mail is not a Poster: the platform's own mail is
-- governed by email.notify_admin and email.notify_app, and a second copy here
-- would be a second thing to disagree.
INSERT INTO integration_connections
    (id, kind, name, scope_id, scope_label, config, credential_secret,
     default_destination_id, default_destination_label,
     for_platform, for_apps, platform_audiences, created_at, updated_at)
VALUES
    ('iconn_email', 'email', 'Email', '', '', '{}'::jsonb, '',
     '', '', FALSE, FALSE, '[]'::jsonb, NOW(), NOW());


-- +goose Down
--
-- A baseline's down is a teardown, not a downgrade: there is no earlier version
-- of this schema to return to, and nothing in this product ever runs it. This
-- product's answer to going back is to restore a backup. It is here because a
-- migration without a Down is one goose will not read, and because what it drops
-- is the honest statement of what the Up created.
DROP TABLE IF EXISTS public.access_requests CASCADE;
DROP TABLE IF EXISTS public.advisories CASCADE;
DROP TABLE IF EXISTS public.advisory_affected CASCADE;
DROP TABLE IF EXISTS public.advisory_feed_state CASCADE;
DROP TABLE IF EXISTS public.allowed_git_sources CASCADE;
DROP TABLE IF EXISTS public.announcement_email_recipients CASCADE;
DROP TABLE IF EXISTS public.announcement_emails CASCADE;
DROP TABLE IF EXISTS public.api_key_usage_daily CASCADE;
DROP TABLE IF EXISTS public.app_api_keys CASCADE;
DROP TABLE IF EXISTS public.app_capability_usage_daily CASCADE;
DROP TABLE IF EXISTS public.app_consumption_daily CASCADE;
DROP TABLE IF EXISTS public.app_consumption_intervals CASCADE;
DROP TABLE IF EXISTS public.app_credentials CASCADE;
DROP TABLE IF EXISTS public.app_cron_jobs CASCADE;
DROP TABLE IF EXISTS public.app_cron_runs CASCADE;
DROP TABLE IF EXISTS public.app_data CASCADE;
DROP TABLE IF EXISTS public.app_events CASCADE;
DROP TABLE IF EXISTS public.app_integration_sends CASCADE;
DROP TABLE IF EXISTS public.app_integrations CASCADE;
DROP TABLE IF EXISTS public.app_platform_log CASCADE;
DROP TABLE IF EXISTS public.app_reachability CASCADE;
DROP TABLE IF EXISTS public.app_render_schedule_recipients CASCADE;
DROP TABLE IF EXISTS public.app_render_schedules CASCADE;
DROP TABLE IF EXISTS public.app_tags CASCADE;
DROP TABLE IF EXISTS public.app_tokens CASCADE;
DROP TABLE IF EXISTS public.app_usage_daily CASCADE;
DROP TABLE IF EXISTS public.apps CASCADE;
DROP TABLE IF EXISTS public.audit_log CASCADE;
DROP TABLE IF EXISTS public.auth_providers CASCADE;
DROP TABLE IF EXISTS public.cli_device_codes CASCADE;
DROP TABLE IF EXISTS public.credential_values CASCADE;
DROP TABLE IF EXISTS public.credentials CASCADE;
DROP TABLE IF EXISTS public.cron_run_logs CASCADE;
DROP TABLE IF EXISTS public.dependency_waivers CASCADE;
DROP TABLE IF EXISTS public.deploy_keys CASCADE;
DROP TABLE IF EXISTS public.deployment_dependencies CASCADE;
DROP TABLE IF EXISTS public.deployments CASCADE;
DROP TABLE IF EXISTS public.encryption_data_keys CASCADE;
DROP TABLE IF EXISTS public.env_vars CASCADE;
DROP TABLE IF EXISTS public.features CASCADE;
DROP TABLE IF EXISTS public.gallery_catalog CASCADE;
DROP TABLE IF EXISTS public.git_connection_identities CASCADE;
DROP TABLE IF EXISTS public.git_connections CASCADE;
DROP TABLE IF EXISTS public.group_claim_mappings CASCADE;
DROP TABLE IF EXISTS public.group_members CASCADE;
DROP TABLE IF EXISTS public.groups CASCADE;
DROP TABLE IF EXISTS public.integration_connections CASCADE;
DROP TABLE IF EXISTS public.integration_placements CASCADE;
DROP TABLE IF EXISTS public.job_definitions CASCADE;
DROP TABLE IF EXISTS public.job_run_logs CASCADE;
DROP TABLE IF EXISTS public.job_runs CASCADE;
DROP TABLE IF EXISTS public.license_usage_samples CASCADE;
DROP TABLE IF EXISTS public.object_grants CASCADE;
DROP TABLE IF EXISTS public.report_deliveries CASCADE;
DROP TABLE IF EXISTS public.report_delivery_recipients CASCADE;
DROP TABLE IF EXISTS public.settings CASCADE;
DROP TABLE IF EXISTS public.storage_managers CASCADE;
DROP TABLE IF EXISTS public.storage_mappings CASCADE;
DROP TABLE IF EXISTS public.storage_resources CASCADE;
DROP TABLE IF EXISTS public.store_grants CASCADE;
DROP TABLE IF EXISTS public.tags CASCADE;
DROP TABLE IF EXISTS public.user_api_keys CASCADE;
DROP TABLE IF EXISTS public.user_identities CASCADE;
DROP TABLE IF EXISTS public.user_passwords CASCADE;
DROP TABLE IF EXISTS public.users CASCADE;

DROP FUNCTION IF EXISTS public.enforce_grant_has_bucket();
DROP FUNCTION IF EXISTS public.enforce_group_membership_cap();
DROP FUNCTION IF EXISTS public.enforce_storage_manager_access();
DROP FUNCTION IF EXISTS public.enforce_storage_mapping_authority();
