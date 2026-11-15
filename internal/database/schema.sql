-- The Toft schema at head, as pg_dump renders it.
--
-- Generated, committed and checked by TestSchemaSnapshotIsCurrent. Do not
-- edit by hand: change internal/database/migrations/ and regenerate with
--
--     scripts/schema-snapshot.sh
--
-- It exists to answer "what is the schema?" without migrating a database to
-- find out, and to make a migration that forgets to update it a test failure
-- rather than a discovery.

--
-- PostgreSQL database dump
--


-- Dumped from database version 17.10
-- Dumped by pg_dump version 17.10

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

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

CREATE FUNCTION public.enforce_grant_has_bucket() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    resource_kind    TEXT;
    resource_bucket  TEXT;
    resource_backing TEXT;
    resource_root    TEXT;
BEGIN
    SELECT kind, bucket, backing, storage_root
      INTO resource_kind, resource_bucket, resource_backing, resource_root
      FROM storage_resources WHERE id = NEW.resource_id;
    IF resource_kind IS NULL THEN
        RAISE EXCEPTION 'store_grants: resource % does not exist', NEW.resource_id;
    END IF;
    IF COALESCE(resource_bucket, '') = ''
       AND NOT (resource_backing = 'host' AND COALESCE(resource_root, '') <> '') THEN
        RAISE EXCEPTION 'store_grants: resource % names no bucket and no host root, so there is nothing to sign for — only a resource with objects or a host directory behind it takes grants', NEW.resource_id;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enforce_group_membership_cap(); Type: FUNCTION; Schema: public; Owner: -
--

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


--
-- Name: enforce_storage_manager_access(); Type: FUNCTION; Schema: public; Owner: -
--

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
    IF resource_kind = 'volume' AND NEW.max_access NOT IN ('read', 'full') THEN
        RAISE EXCEPTION 'storage_managers: resource % is a volume, which offers read and full — % is not one of them',
            NEW.resource_id, NEW.max_access;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enforce_storage_mapping_authority(); Type: FUNCTION; Schema: public; Owner: -
--

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

    v_rank_of  := CASE NEW.access WHEN 'read' THEN 1 WHEN 'write' THEN 2 WHEN 'full' THEN 3 ELSE 0 END;
    v_rank_max := CASE v_ceiling  WHEN 'read' THEN 1 WHEN 'write' THEN 2 WHEN 'full' THEN 3 ELSE 0 END;
    IF v_rank_of > v_rank_max THEN
        RAISE EXCEPTION 'storage_mappings: % may attach resource % at % at most, not %',
            NEW.authority, NEW.resource_id, v_ceiling, NEW.access;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enforce_storage_mapping_partition(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_storage_mapping_partition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_partitioned boolean;
    v_other text;
BEGIN
    SELECT partitioned INTO v_partitioned FROM storage_resources
     WHERE id = NEW.resource_id FOR UPDATE;
    IF NOT COALESCE(v_partitioned, false) THEN
        RETURN NEW;
    END IF;
    IF NEW.folder = '' THEN
        RAISE EXCEPTION 'storage_mappings: resource % is partitioned, so a mapping names a folder', NEW.resource_id;
    END IF;
    SELECT o.folder INTO v_other FROM storage_mappings o
     WHERE o.resource_id = NEW.resource_id AND o.id <> NEW.id AND o.folder <> NEW.folder
       AND (starts_with(o.folder, NEW.folder || '/') OR starts_with(NEW.folder, o.folder || '/'))
     LIMIT 1;
    IF v_other IS NOT NULL THEN
        RAISE EXCEPTION 'storage_mappings: folder % overlaps folder %, which is already mapped on partitioned resource %',
            NEW.folder, v_other, NEW.resource_id;
    END IF;
    RETURN NEW;
END;
$$;


--
-- Name: enforce_storage_partitioned_fixed(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE FUNCTION public.enforce_storage_partitioned_fixed() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.partitioned IS DISTINCT FROM OLD.partitioned THEN
        RAISE EXCEPTION 'storage_resources: resource % is % and that is fixed at creation',
            OLD.id, CASE WHEN OLD.partitioned THEN 'partitioned' ELSE 'whole' END;
    END IF;
    RETURN NEW;
END;
$$;


SET default_tablespace = '';

SET default_table_access_method = heap;

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

COMMENT ON COLUMN public.advisories.snapshot_id IS 'Which feed snapshot brought this row in. Not a Toft reference: it names a fetch, and advisory_feed_state holds the current one per ecosystem.';


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

COMMENT ON COLUMN public.advisory_feed_state.snapshot_id IS 'The snapshot currently in force for this ecosystem. Not a Toft reference.';


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
    abandoned integer DEFAULT 0 NOT NULL,
    resend_of text
);


--
-- Name: TABLE announcement_emails; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.announcement_emails IS 'The announcement an administrator sends — the first mail in the product whose arrival is the point. Every other notification may vanish; a frozen row saying "sent to 214 people on the 3rd of March" has to be true. Two tables rather than a settings key for that reason.';


--
-- Name: COLUMN announcement_emails.resend_of; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.announcement_emails.resend_of IS 'The announcement this row resends to the people whose delivery failed or was abandoned. Set only on a row created already sending by the resend route; no foreign key, the original may be deleted.';


--
-- Name: api_key_usage_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_key_usage_daily (
    key_id text NOT NULL,
    key_kind text NOT NULL,
    day date NOT NULL,
    app_id text DEFAULT ''::text NOT NULL,
    status_class smallint NOT NULL,
    requests bigint DEFAULT 0 NOT NULL,
    CONSTRAINT api_key_usage_daily_key_kind_check CHECK ((key_kind = 'user'::text))
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
-- Name: app_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_accounts (
    id bigint NOT NULL,
    name text NOT NULL,
    number integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone
);


--
-- Name: TABLE app_accounts; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_accounts IS 'The uid of each auto-created per-app OS account, which is also its per-app group''s gid. The database is the source of the number; a container''s /etc/passwd and /etc/group are a cache of it, recreated at every start. Retired rather than deleted: the number is never reused.';


--
-- Name: COLUMN app_accounts.name; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_accounts.name IS 'The account and group name, toft-app-<slug> (runtime accountName). One live row per name.';


--
-- Name: COLUMN app_accounts.number; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_accounts.number IS 'The uid, and the per-app group''s gid. From APP_ACCOUNT_ID_RANGE (default 100000-1999999; rows from before the default widened hold 63000-64999), unique across live and retired rows.';


--
-- Name: COLUMN app_accounts.retired_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_accounts.retired_at IS 'When the app was deleted. A retired number is never allocated again.';


--
-- Name: app_accounts_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE public.app_accounts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: app_accounts_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.app_accounts_id_seq OWNED BY public.app_accounts.id;


--
-- Name: app_audience_daily; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_audience_daily (
    app_id text NOT NULL,
    day date NOT NULL,
    sketch bytea NOT NULL
);


--
-- Name: TABLE app_audience_daily; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_audience_daily IS 'Distinct anonymous browsers per app per UTC day, as a HyperLogLog sketch (precision 14, axiomhq binary encoding). Written only while audience_measurement_enabled is on; no foreign key to apps, for app_usage_daily''s reason. Swept by the access record''s retention.';


--
-- Name: COLUMN app_audience_daily.sketch; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_audience_daily.sketch IS 'axiomhq/hyperloglog MarshalBinary output. Names nobody: an estimate of how many visitor ids were added, never the ids.';


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
    has_gap boolean DEFAULT false NOT NULL,
    periods bigint,
    throttled_periods bigint,
    throttled_seconds double precision
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
    has_gap boolean DEFAULT false NOT NULL,
    periods bigint,
    throttled_periods bigint,
    throttled_seconds double precision
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
-- Name: COLUMN app_consumption_intervals.throttled_periods; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_consumption_intervals.throttled_periods IS 'Enforcement periods within the window in which the workload was held at its CPU limit; NULL where not measured.';


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
    placement_id text DEFAULT ''::text NOT NULL,
    day date GENERATED ALWAYS AS (((created_at AT TIME ZONE 'UTC'::text))::date) STORED
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
-- Name: COLUMN app_integration_sends.day; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_integration_sends.day IS 'The UTC date this send was created, generated from created_at. It exists so the idempotency index can be bounded by time: a partial index cannot compare to now(), and a key is single-use within a day and reusable after.';


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

COMMENT ON COLUMN public.app_render_schedule_recipients.schedule_id IS 'The schedule this list belongs to. Cascades on delete, deliberately: a list kept past its schedule would reappear if one were recreated, sending mail to people chosen for a different report. The row survives every change to the schedule and its zone; only removing [render] from toft.toml deletes it, and that deploy announces it (schedule_removed).';


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

COMMENT ON TABLE public.app_render_schedules IS 'At most one render schedule per app, written only by a deploy reconciling toft.toml [render]; enabled is the one field anything else writes. What it fires is a deploy. A table rather than a column on apps because the shared claim selects app_id by name and apps has none.';


--
-- Name: app_restart_schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_restart_schedules (
    id text NOT NULL,
    app_id text NOT NULL,
    name text DEFAULT 'Restart'::text NOT NULL,
    schedule text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    next_run_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_fired_at timestamp with time zone,
    last_outcome text DEFAULT ''::text NOT NULL,
    last_reason text DEFAULT ''::text NOT NULL,
    CONSTRAINT app_restart_schedules_last_outcome_check CHECK ((last_outcome = ANY (ARRAY[''::text, 'restarted'::text, 'skipped'::text, 'failed'::text])))
);


--
-- Name: TABLE app_restart_schedules; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.app_restart_schedules IS 'At most one restart schedule per app, written only by a deploy reconciling toft.toml [restart]. A firing restarts a running app and nothing else, and never resets the supervisor or the health sweep.';


--
-- Name: COLUMN app_restart_schedules.schedule; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_restart_schedules.schedule IS 'The cron expression with its zone joined in (CRON_TZ=…), so the shared claim sees one string.';


--
-- Name: COLUMN app_restart_schedules.enabled; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_restart_schedules.enabled IS 'False is a pause from the Settings tab, which lasts until the next deploy.';


--
-- Name: COLUMN app_restart_schedules.last_outcome; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_restart_schedules.last_outcome IS 'The last firing: restarted, skipped (declined, last_reason says why) or failed (last_reason is the start error). Empty before the first. Kept across deploys.';


--
-- Name: COLUMN app_restart_schedules.last_reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_restart_schedules.last_reason IS 'Why the last firing was skipped — sleeping, stopped, crashed, deploy_pending, locked, no_process — or the failed start''s sentence.';


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
    requests bigint DEFAULT 0 NOT NULL,
    key_owner_user_id text DEFAULT ''::text NOT NULL,
    page_loads bigint DEFAULT 0 NOT NULL
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
-- Name: COLUMN app_usage_daily.key_owner_user_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_usage_daily.key_owner_user_id IS 'For a key: row, the account the key belonged to on the day of the traffic; empty otherwise. No foreign key, so it outlives the person.';


--
-- Name: COLUMN app_usage_daily.page_loads; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.app_usage_daily.page_loads IS 'How many of requests were top-level document navigations (Sec-Fetch-Dest: document, or a GET preferring text/html with no Sec-Fetch-Dest). Framed loads never count: the SPA''s Preview tab is an iframe.';


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
    listable boolean DEFAULT true NOT NULL,
    locked_source text DEFAULT 'admin'::text NOT NULL,
    repo_subdir text DEFAULT ''::text NOT NULL,
    serving_since timestamp with time zone,
    status_changed_at timestamp with time zone DEFAULT now(),
    last_seen_tree text DEFAULT ''::text NOT NULL,
    cpu_limit_millicores integer DEFAULT 0 NOT NULL,
    hostname text,
    hostname_enabled boolean DEFAULT false NOT NULL,
    release_base_path text,
    garth_linked_at timestamp with time zone,
    status_before_build text DEFAULT ''::text NOT NULL,
    CONSTRAINT apps_execution_mode_check CHECK ((execution_mode = ANY (ARRAY['shared'::text, 'isolated'::text]))),
    CONSTRAINT apps_hostname_lower CHECK ((hostname = lower(hostname))),
    CONSTRAINT apps_hostname_needs_name CHECK (((NOT hostname_enabled) OR (hostname IS NOT NULL))),
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
-- Name: COLUMN apps.listable; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.listable IS 'Whether this app appears in the listing of somebody who holds nothing on it, while the install-wide list rung is on. Inert for public and authenticated apps, which are already listable to everyone who can open them. It subtracts from the install-wide rung and from nothing else, so a grant always outranks it. The owner writes it; the platform never does.';


--
-- Name: COLUMN apps.locked_source; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.locked_source IS 'Who placed the lock that is on this app: admin, or dependency for one the dependency policy placed automatically. It is what tells apart the three cases locked_by cannot: no lock, an administrator since deleted, and the platform itself. Inert while locked_at is NULL.';


--
-- Name: COLUMN apps.repo_subdir; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.repo_subdir IS 'The folder of repo_url the app is built from, relative and clean; empty is the repository root. Only the folder is placed in the release — the rest of the clone, .git included, is discarded.';


--
-- Name: COLUMN apps.serving_since; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.serving_since IS 'When current_release last changed (the flip of a deploy, rollback or promote). NULL: nothing serving, or not recorded before migration 00061.';


--
-- Name: COLUMN apps.status_changed_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.status_changed_at IS 'When status last took a different value. NULL: not recorded before migration 00061.';


--
-- Name: COLUMN apps.last_seen_tree; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.last_seen_tree IS 'git tree id of repo_subdir at the commit the last successful build cloned. Cleared with last_seen_sha; empty means the poller builds on any new commit.';


--
-- Name: COLUMN apps.cpu_limit_millicores; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.cpu_limit_millicores IS 'An administrator''s CPU limit for this app, in millicores. 0 means the install''s app_cpu_limit_millicores.';


--
-- Name: COLUMN apps.hostname; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.hostname IS 'A DNS name of the app''s own, set by an administrator; NULL when none. Unique whether or not it is served.';


--
-- Name: COLUMN apps.hostname_enabled; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.hostname_enabled IS 'Whether the hostname is served. A switch beside the name, so a name can be recorded before its DNS exists.';


--
-- Name: COLUMN apps.release_base_path; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.release_base_path IS 'The base path the serving release was built for: /apps/{slug}, or empty at the root of a hostname. NULL when nothing serves. Every reader of the serving path reads this, not the slug.';


--
-- Name: COLUMN apps.garth_linked_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.garth_linked_at IS 'When a deploy from a Garth session first linked this app; NULL when it was never deployed from Garth or was unlinked. Open in Garth is offered only while it is set.';


--
-- Name: COLUMN apps.status_before_build; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.apps.status_before_build IS 'The status the app held when a build last wrote building (MarkAppBuilding); kept while another build is running. Read by FailOrphanedDeployments at boot so a stopped or sleeping app stays so; empty before any build.';


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
    api_key_id text,
    app_id text DEFAULT ''::text NOT NULL
);


--
-- Name: TABLE audit_log; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.audit_log IS 'Append-only audit trail, deliberately with no foreign keys. An audit record has to survive the deletion of its actor and its target, which is most of the point of keeping it, and a foreign key would prevent the deletion or erase the attribution. Its one deletion path is the access_record sweep, governed by access_record_retention_days (0 keeps everything), and every pass that removes rows writes audit_log.pruned.';


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

COMMENT ON COLUMN public.audit_log.request_id IS 'The request that caused this, for correlation with the access log. Not a Toft reference.';


--
-- Name: COLUMN audit_log.api_key_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.api_key_id IS 'Which credential spoke, when it was not a browser. No foreign key — a revoked and deleted key must not erase the record of what it did.';


--
-- Name: COLUMN audit_log.app_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.audit_log.app_id IS 'The app this row is part of the history of, or empty for an install-wide act. Derived from an app target or an app actor where the row is not told otherwise; internal/database/audit_scope.go says which actions must carry one.';


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
    scim_last_call_at timestamp with time zone,
    permitted_login_groups text DEFAULT ''::text NOT NULL,
    allowed_emails text DEFAULT ''::text NOT NULL,
    groups_separator text DEFAULT ''::text NOT NULL
);


--
-- Name: TABLE auth_providers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.auth_providers IS 'Admin-managed sign-in providers. Identity has its own table because a hardcoded provider makes the choice operator-only — a restart to change — and makes a second provider inexpressible.';


--
-- Name: COLUMN auth_providers.client_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.auth_providers.client_id IS 'The OAuth client id at the provider. Not a Toft reference.';


--
-- Name: COLUMN auth_providers.permitted_login_groups; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.auth_providers.permitted_login_groups IS 'Claim values -- not group ids -- of which a person must hold at least one to sign in through this provider. Empty means every group is permitted. Compared against the asserted claim before any mapping is consulted, because an authentication gate must not depend on an authorization table. A non-empty list refuses a sign-in that asserts no claim at all, and the local administrator is what makes failing closed survivable.';


--
-- Name: COLUMN auth_providers.allowed_emails; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.auth_providers.allowed_emails IS 'Individual addresses permitted to sign in, in union with allowed_domains and never in intersection: a person matching either list is permitted. Both empty means any address this provider authenticates.';


--
-- Name: COLUMN auth_providers.groups_separator; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.auth_providers.groups_separator IS 'The delimiter a group claim delivered as one string is split on. Empty means no split, which is what every provider read before this existed: a bare string is a list of one.';


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
    denied_at timestamp with time zone,
    client text DEFAULT 'lp'::text NOT NULL,
    requested_from text DEFAULT ''::text NOT NULL
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
-- Name: COLUMN cli_device_codes.client; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.cli_device_codes.client IS 'Which program asked: lp (lp login) or rsconnect (RStudio''s Add account dialog). Display only — it decides the approval screen''s wording and nothing about what is minted.';


--
-- Name: COLUMN cli_device_codes.requested_from; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.cli_device_codes.requested_from IS 'The address the request arrived from, shown on the approval screen so a person can tell their own machine from somebody else''s. Empty on rows written before the column.';


--
-- Name: connect_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.connect_tokens (
    token_id text NOT NULL,
    public_key bytea NOT NULL,
    device_code_hash text,
    user_api_key_id text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE connect_tokens; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.connect_tokens IS 'rsconnect''s request-signing tokens (ADR §102): a public key, and the personal API key whose identity and lifecycle it carries. No secret rests here — the private key never leaves the publisher''s machine, and the personal key''s secret was discarded when it was created. Revoking the key deletes the row.';


--
-- Name: COLUMN connect_tokens.public_key; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.connect_tokens.public_key IS 'The DER-encoded RSA public key rsconnect registered; every signed request is verified against it.';


--
-- Name: COLUMN connect_tokens.device_code_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.connect_tokens.device_code_hash IS 'While pending, the cli_device_codes row a person approves. Empty once claimed.';


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
-- Name: dependency_scans; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.dependency_scans (
    id text NOT NULL,
    app_id text NOT NULL,
    deployment_id text,
    trigger text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    scanned boolean DEFAULT false NOT NULL,
    feed_as_of timestamp with time zone,
    findings integer DEFAULT 0 NOT NULL,
    counted integer DEFAULT 0 NOT NULL,
    malware integer DEFAULT 0 NOT NULL,
    critical integer DEFAULT 0 NOT NULL,
    high integer DEFAULT 0 NOT NULL,
    moderate integer DEFAULT 0 NOT NULL,
    low integer DEFAULT 0 NOT NULL,
    unknown integer DEFAULT 0 NOT NULL,
    waived integer DEFAULT 0 NOT NULL,
    packages integer DEFAULT 0 NOT NULL,
    advisories text DEFAULT '[]'::text NOT NULL,
    CONSTRAINT dependency_scans_trigger_check CHECK ((trigger = ANY (ARRAY['deploy'::text, 'scheduled'::text, 'manual'::text])))
);


--
-- Name: TABLE dependency_scans; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.dependency_scans IS 'One row per app per pass of the dependency sweep: the observation, never the decision. A finding is derived against a corpus that is swapped wholesale, so a reading is only available at the moment it is taken, and this is where it is kept. deployments.dependency_verdict remains the release decision, and nothing derives one from the other.';


--
-- Name: COLUMN dependency_scans.trigger; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.dependency_scans.trigger IS 'Why this reading was taken: deploy, scheduled or manual. Three values and no fourth, because an automatic consequence needs an automatic trigger, and the lock decision reads this to know whether a person was the one asking.';


--
-- Name: COLUMN dependency_scans.scanned; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.dependency_scans.scanned IS 'Whether the release was covered, in the policy sense: an inventory in an ecosystem this build matches against. False is unknown and never renders as a clean bill of health.';


--
-- Name: COLUMN dependency_scans.advisories; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.dependency_scans.advisories IS 'The notable advisory ids as a JSON array, capped. No CHECK, following deployments.dependency_verdict: it is a shape, not a vocabulary. Counts alone cannot answer which advisory arrived on 3 August, and the full finding set thirty times over answers nothing extra.';


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
    may_attach boolean DEFAULT false NOT NULL,
    CONSTRAINT deploy_keys_may_attach_scope CHECK (((NOT may_attach) OR (scope = 'deploy'::text))),
    CONSTRAINT deploy_keys_scope_check CHECK ((scope = ANY (ARRAY['read'::text, 'deploy'::text])))
);


--
-- Name: TABLE deploy_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.deploy_keys IS 'The third key class, separated from the other two by who it belongs to: an app key is a grant on one app, a personal key is the person, and a deploy key belongs to nobody — an administrator mints it, possession is the authorization, and it is revoked explicitly or not at all.';


--
-- Name: COLUMN deploy_keys.key_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deploy_keys.key_hash IS 'A SHA-256 of the secret. The plaintext is shown once at creation and never stored.';


--
-- Name: COLUMN deploy_keys.may_attach; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deploy_keys.may_attach IS 'A deploy carrying this key may add storage declared in toft.toml, judged on the app owner''s delegation alone — the administrator who set it stands in for the person a change otherwise needs. Only a deploy-scoped key may carry it.';


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
    path text DEFAULT ''::text NOT NULL,
    source text DEFAULT 'resolved'::text NOT NULL
);


--
-- Name: TABLE deployment_dependencies; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.deployment_dependencies IS 'What a release actually installed. The one table that grows, and what makes everything else cheap: the sweep joins stored inventory against refreshed advisories, so watching costs no rebuild and touches no registry. Pruned through the deployments cascade — retention of the inventory is the release''s retention and deliberately not a knob of its own.';


--
-- Name: COLUMN deployment_dependencies.source; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployment_dependencies.source IS 'resolved (lockfile or manifest), installed (the build put it in the release library for the app), platform (Toft put it there), or runtime (found by the sweep after the deploy). Runtime rows are appended, never removed.';


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
    runtime_version text DEFAULT ''::text NOT NULL,
    failure_reason text DEFAULT ''::text NOT NULL,
    failure_message text DEFAULT ''::text NOT NULL,
    warnings text DEFAULT ''::text NOT NULL,
    source_deploy_id text DEFAULT ''::text NOT NULL,
    framework text DEFAULT ''::text NOT NULL,
    entrypoint text DEFAULT ''::text NOT NULL,
    start_cmd text DEFAULT ''::text NOT NULL,
    base_path text,
    deployed_by_type text DEFAULT ''::text NOT NULL,
    deployed_by_id text DEFAULT ''::text NOT NULL,
    failure_code text DEFAULT ''::text NOT NULL,
    failure_output text DEFAULT ''::text NOT NULL,
    restored_from text DEFAULT ''::text NOT NULL,
    build_log_times text DEFAULT ''::text NOT NULL,
    CONSTRAINT deployments_deployed_by_type_check CHECK ((deployed_by_type = ANY (ARRAY[''::text, 'user'::text, 'service_account'::text, 'deploy_key'::text])))
);


--
-- Name: TABLE deployments; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.deployments IS 'One build of one app. Three phases — upload, scan, deploy — and the phase is derived from the timestamps rather than stored.';


--
-- Name: COLUMN deployments.triggered_by; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.triggered_by IS 'Not a person. This is the category of trigger — manual, auto, rollback, render, move — and the CHECK beside it is the whole vocabulary. Named _by for historical reasons and deliberately not renamed: the name is on the wire in models.go JSON tags and the SPA reads it.';


--
-- Name: COLUMN deployments.execution_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.execution_mode IS 'The effective mode this release ran under. Constrained here as well as on apps, because the invariant governs all three spellings of it — shared or isolated, never a backend name.';


--
-- Name: COLUMN deployments.dependency_verdict; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.dependency_verdict IS 'The policy verdict as a JSON document, not a vocabulary — no CHECK. A restart is not a deploy: this is the release''s verdict, not today''s.';


--
-- Name: COLUMN deployments.failure_reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.failure_reason IS 'Why a failed deploy failed, as a key a program branches on rather than a sentence; internal/deploysteps.Reason. No CHECK: a growing vocabulary, guarded in Go where both ends of the wire can see it, and an unknown key renders rather than vanishing. Empty means unclassified, which is every row written before this column and is never internal_error.';


--
-- Name: COLUMN deployments.failure_message; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.failure_message IS 'The whole reason a deploy failed, in the words the build log got. Stored because a progress frame is a snapshot and SnapshotProgress (what a reconnecting watcher receives) could not previously say why. Duplicated out of build_log on purpose: the log is subject to retention and this must still answer afterwards.';


--
-- Name: COLUMN deployments.warnings; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.warnings IS 'What this deploy noticed and carried on past, as a JSON array of sentences; rendered into the warnings field of the deploy result object. On the row rather than only on a live progress frame, for the reason failure_reason is: a frame is a snapshot and SnapshotProgress is what a reconnecting watcher receives. Empty means none, which is every row written before this column.';


--
-- Name: COLUMN deployments.source_deploy_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.source_deploy_id IS 'For a rollback, the deployment whose release it restores, recorded when the rollback is queued so a bulk release deletion cannot remove it while the rollback waits for the worker. Empty for anything that is not a rollback.';


--
-- Name: COLUMN deployments.framework; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.framework IS 'What this build detected, so a workload of this release is told what to run even after a later build rewrote the app row. Empty for a deployment made before migration 00073; readers fall back to the app row.';


--
-- Name: COLUMN deployments.base_path; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.base_path IS 'The base path this release was built for; NULL on rows from before the column.';


--
-- Name: COLUMN deployments.deployed_by_type; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.deployed_by_type IS 'The principal that asked for this deployment: user, service_account or deploy_key; empty when no principal did (auto, render, move, and every row before 00082).';


--
-- Name: COLUMN deployments.deployed_by_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.deployed_by_id IS 'The id of deployed_by_type''s principal — for a service account, its owning user, whose delegations it is judged through. No foreign key: it is polymorphic, and the deployment row outlives the credential, as the audit log does.';


--
-- Name: COLUMN deployments.failure_code; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.failure_code IS 'The refusal''s own code inside failure_reason, when the failure has one (storage_unknown under storage_refused, removed_key under manifest_invalid). Empty for a failure with no finer code, and on every row written before the column.';


--
-- Name: COLUMN deployments.failure_output; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.failure_output IS 'A JSON array of {at, stream, line}: the workload''s own output a readiness failure read before the workload was deleted, each line with its own time and stream. Empty when the failure carried none, and on every row written before the column.';


--
-- Name: COLUMN deployments.restored_from; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.restored_from IS 'The deployment a rollback restored, set when the rollback is queued and never cleared — unlike source_deploy_id, which a rollback rebuilt for a new base path gives up. Empty on a row that is not a rollback.';


--
-- Name: COLUMN deployments.build_log_times; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.deployments.build_log_times IS 'v1:<start unix ms>:<offset ms>,… — one offset per line of build_log, from the top. Empty for a build that recorded none; shorter than the log when lines were appended after the build (the restart sweep).';


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
-- Name: exchanged_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.exchanged_keys (
    id text NOT NULL,
    user_id text NOT NULL,
    issuer_id text NOT NULL,
    key_hash text NOT NULL,
    key_prefix text NOT NULL,
    token_subject text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    last_used_at timestamp with time zone,
    last_used_ip text DEFAULT ''::text NOT NULL
);


--
-- Name: TABLE exchanged_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.exchanged_keys IS 'Short-lived deploy-scoped keys (toftx_) minted by exchanging a trusted issuer''s identity token. A person''s, like user_api_keys, but never long-lived and never wider than the deploy surface. Rows are swept a week after they expire; audit_log.api_key_id keeps naming them.';


--
-- Name: COLUMN exchanged_keys.key_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.exchanged_keys.key_hash IS 'A SHA-256 of the secret — see user_api_keys.key_hash.';


--
-- Name: COLUMN exchanged_keys.token_subject; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.exchanged_keys.token_subject IS 'The claim value the issuer named the person by, kept so a review can join a key to the issuer''s own log.';


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

COMMENT ON COLUMN public.git_connections.github_app_id IS 'The GitHub App''s own identifier. Renamed from app_id, which said something false: app_id means a Toft app in twenty-one other tables in this schema.';


--
-- Name: COLUMN git_connections.installation_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.git_connections.installation_id IS 'The git host''s own installation identifier. Not a Toft reference.';


--
-- Name: COLUMN git_connections.client_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.git_connections.client_id IS 'The OAuth client id at the git host. Not a Toft reference.';


--
-- Name: goose_db_version; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.goose_db_version (
    id integer NOT NULL,
    version_id bigint NOT NULL,
    is_applied boolean NOT NULL,
    tstamp timestamp without time zone DEFAULT now() NOT NULL
);


--
-- Name: goose_db_version_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.goose_db_version ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.goose_db_version_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


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
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    issue_tokens boolean DEFAULT false NOT NULL
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

COMMENT ON COLUMN public.integration_connections.scope_id IS 'The far side''s own id for the scope this connection is welded to. Not a Toft reference, and a new scope is a new connection rather than an edit to this.';


--
-- Name: COLUMN integration_connections.default_destination_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_connections.default_destination_id IS 'The far side''s own id, not a Toft reference.';


--
-- Name: COLUMN integration_connections.description; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_connections.description IS 'Free text an administrator wrote about this connection — what it is for and who owns the far side. Optional, editable, at most 1000 characters.';


--
-- Name: COLUMN integration_connections.issue_tokens; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_connections.issue_tokens IS 'Apps attached to this connection may get its short-lived access token (file kinds only). Off by default; a token reaches the connection''s whole grant, not the folder chosen per app.';


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

COMMENT ON COLUMN public.integration_placements.destination_id IS 'The far side''s own id for the folder or channel — not a Toft reference.';


--
-- Name: COLUMN integration_placements.item_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_placements.item_id IS 'The far side''s own id for what was placed — not a Toft reference.';


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
-- Name: integration_places; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.integration_places (
    id text NOT NULL,
    connection_id text NOT NULL,
    place_kind text NOT NULL,
    conversation_id text NOT NULL,
    name text NOT NULL,
    added_by text,
    added_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT integration_places_place_kind_check CHECK ((place_kind = ANY (ARRAY['team'::text, 'chat'::text])))
);


--
-- Name: TABLE integration_places; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.integration_places IS 'A place on the far side an administrator pasted onto a connection and Toft verified the connection can reach — a Teams team or chat the bot was installed in. Deleted with its connection; refused while an attachment or the connection''s default destination points into it.';


--
-- Name: COLUMN integration_places.conversation_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_places.conversation_id IS 'The far side''s own id for the place (a Teams thread id, 19:…@thread.tacv2 or 19:…@thread.v2). Not a Toft reference.';


--
-- Name: COLUMN integration_places.name; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.integration_places.name IS 'The team''s name as the far side reported it at paste time, or the name an administrator gave a chat, which the far side does not report.';


--
-- Name: job_definitions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.job_definitions (
    id text NOT NULL,
    app_id text NOT NULL,
    name text NOT NULL,
    command text NOT NULL,
    schedule text,
    params_spec jsonb DEFAULT '{}'::jsonb NOT NULL,
    timeout_sec integer DEFAULT 3600 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    next_run_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    expect_within_sec integer,
    enabled_at timestamp with time zone DEFAULT now() NOT NULL,
    args jsonb,
    overlap text DEFAULT 'queue'::text NOT NULL,
    notify text DEFAULT 'none'::text NOT NULL,
    override_memory_mb integer,
    override_cpu_millicores integer,
    CONSTRAINT job_definitions_expect_within_check CHECK (((expect_within_sec IS NULL) OR ((expect_within_sec >= 3600) AND (expect_within_sec <= 2678400)))),
    CONSTRAINT job_definitions_notify_check CHECK ((notify = ANY (ARRAY['none'::text, 'failures'::text, 'always'::text]))),
    CONSTRAINT job_definitions_overlap_check CHECK ((overlap = ANY (ARRAY['skip'::text, 'queue'::text])))
);


--
-- Name: TABLE job_definitions; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.job_definitions IS 'One job system, multiple triggers: cron scheduling, the on-demand API and app-initiated enqueues are all producers into this one durable queue. There is no separate cron subsystem — app_cron_jobs is the HTTP-trigger feature, which is a different thing. This is the codebase''s first durable queue, and it has to be: a run that vanishes with a platform restart is a job that silently never happened.';


--
-- Name: COLUMN job_definitions.enabled; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.enabled IS 'False while paused from the Automation tab. A deploy re-applies toft.toml and resumes it.';


--
-- Name: COLUMN job_definitions.expect_within_sec; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.expect_within_sec IS 'How long this job may go without a successful run before job_run_overdue is raised, in seconds (1 hour to 31 days). NULL is no expectation. On a manifest row it is the repository''s, written by every deploy like schedule; the UI may set it only on a UI row.';


--
-- Name: COLUMN job_definitions.enabled_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.enabled_at IS 'When this definition was created or last switched from disabled to enabled — the floor of the overdue window, so a new or re-enabled job is not overdue on arrival. Never touched by a deploy that leaves enabled alone.';


--
-- Name: COLUMN job_definitions.args; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.args IS 'The argv of a single built binary, instead of command. NULL when command is used.';


--
-- Name: COLUMN job_definitions.overlap; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.overlap IS 'A scheduled firing while the last run is still going: skip records a skipped run, queue waits.';


--
-- Name: COLUMN job_definitions.notify; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.notify IS 'Who hears about a run''s outcome: none, failures (one message per failed run) or always.';


--
-- Name: COLUMN job_definitions.override_memory_mb; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.override_memory_mb IS 'An administrator''s memory limit for this job, in MiB. NULL is the install''s job_memory_limit_mb. Never written by a deploy.';


--
-- Name: COLUMN job_definitions.override_cpu_millicores; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_definitions.override_cpu_millicores IS 'An administrator''s CPU limit for this job, in millicores. NULL is the install''s job_cpu_limit_millicores. Never written by a deploy.';


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
    status text DEFAULT 'queued'::text NOT NULL,
    started_by text DEFAULT 'ui'::text NOT NULL,
    actor_id text,
    params jsonb DEFAULT '{}'::jsonb NOT NULL,
    execution_mode text DEFAULT 'isolated'::text NOT NULL,
    external_id text DEFAULT ''::text NOT NULL,
    release_ref text DEFAULT ''::text NOT NULL,
    claimed_by text DEFAULT ''::text NOT NULL,
    claimed_at timestamp with time zone,
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
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    scheduled_for timestamp with time zone,
    bootstrapped_at timestamp with time zone,
    rerun_of text,
    progress_message text DEFAULT ''::text NOT NULL,
    peak_memory_mb integer,
    cpu_throttled_seconds double precision,
    scheduled_for_offset text DEFAULT ''::text NOT NULL,
    stop_owed_at timestamp with time zone,
    CONSTRAINT job_runs_started_by_check CHECK ((started_by = ANY (ARRAY['app'::text, 'viewer'::text, 'cli'::text, 'schedule'::text, 'ui'::text]))),
    CONSTRAINT job_runs_status_check CHECK ((status = ANY (ARRAY['queued'::text, 'claimed'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'timed_out'::text, 'out_of_memory'::text, 'cancelled'::text, 'skipped'::text])))
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
-- Name: COLUMN job_runs.status; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.status IS 'queued, claimed (a node is starting it), running; then exactly one of succeeded, failed, timed_out, out_of_memory, cancelled — or skipped, a scheduled firing the overlap policy declined.';


--
-- Name: COLUMN job_runs.started_by; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.started_by IS 'app (the app as itself), viewer (the app for a signed-in person, actor_id set), cli (a key or the CLI), schedule, ui (Run now).';


--
-- Name: COLUMN job_runs.execution_mode; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.execution_mode IS 'Constrained for deployments.execution_mode''s reason.';


--
-- Name: COLUMN job_runs.external_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.external_id IS 'The backend''s own handle for the workload — a pod name, a task ARN. Not a Toft reference.';


--
-- Name: COLUMN job_runs.claimed_by; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.claimed_by IS 'Not a person. A worker''s identity, written by whichever node won the claim. Named _by for historical reasons and deliberately not renamed, for the reason deployments.triggered_by gives.';


--
-- Name: COLUMN job_runs.failure_reason; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.failure_reason IS 'A sentence, not a vocabulary — no CHECK. failure_stage is the constrained half.';


--
-- Name: COLUMN job_runs.scheduled_for; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.scheduled_for IS 'The schedule tick this run belongs to — the claimed next_run_at, never the dispatch time — or the instant a person named for it (the Run for dialog, or toft job run with a for flag). NULL on a run nobody gave one: a manual or app-started run is never assumed to be for now. A retry copies its original''s. Reaches the workload as TOFT_SCHEDULED_FOR, in the schedule''s own zone.';


--
-- Name: COLUMN job_runs.bootstrapped_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.bootstrapped_at IS 'When a host run''s runner presented its bootstrap credential — set once, by an UPDATE that matches only a NULL, so a second presentation is refused. NULL for every pod run, which proves itself through the cluster instead.';


--
-- Name: COLUMN job_runs.rerun_of; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.rerun_of IS 'The run this one deliberately repeats (toft job rerun, Run again). Nothing re-runs automatically.';


--
-- Name: COLUMN job_runs.progress_message; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.progress_message IS 'The short message beside progress, as the job last reported it.';


--
-- Name: COLUMN job_runs.peak_memory_mb; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.peak_memory_mb IS 'The most memory the run was seen to use. NULL when never reported.';


--
-- Name: COLUMN job_runs.cpu_throttled_seconds; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.cpu_throttled_seconds IS 'Seconds the run was held at its CPU limit, raised as the runner reports; NULL where not measured.';


--
-- Name: COLUMN job_runs.scheduled_for_offset; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.scheduled_for_offset IS 'The UTC offset scheduled_for was named in, as RFC 3339 writes it (-05:00, +05:30), when a person named the instant with a non-UTC offset. Empty for a schedule tick (which reads in its schedule''s zone), for a UTC instant, for no instant, and on every row written before the column.';


--
-- Name: COLUMN job_runs.stop_owed_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.job_runs.stop_owed_at IS 'When the platform began owing this terminal run a stop at its backend (timeout, cancel, lock, an orphaned start); NULL once the backend reports the workload gone, or when nothing was owed.';


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
    CONSTRAINT object_grants_principal_type_check CHECK ((principal_type = ANY (ARRAY['user'::text, 'group'::text, 'service_account'::text])))
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
-- Name: COLUMN object_grants.role; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.object_grants.role IS 'A rung of the object-role ladder, ordered owner > editor > viewer > list. The word is stored and the ordering is not: internal/auth ranks it, and ranks list strictly below every way of reading, which is what makes a list grant put the app in your listings without letting you open it.';


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
-- Name: storage_claims; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_claims (
    resource_id text NOT NULL,
    claim text NOT NULL,
    folder text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: TABLE storage_claims; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_claims IS 'Every volume claim a storage resource was ever given, recorded before the claim exists, so the resource delete can destroy exactly those volumes and no other.';


--
-- Name: storage_default_managers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_default_managers (
    id text NOT NULL,
    subject_type text NOT NULL,
    user_id text,
    group_id text,
    max_access text NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_default_managers_max_access_check CHECK ((max_access = ANY (ARRAY['read'::text, 'full'::text]))),
    CONSTRAINT storage_default_managers_subject CHECK ((((subject_type = 'user'::text) AND (user_id IS NOT NULL) AND (group_id IS NULL)) OR ((subject_type = 'group'::text) AND (group_id IS NOT NULL) AND (user_id IS NULL)))),
    CONSTRAINT storage_default_managers_subject_type_check CHECK ((subject_type = ANY (ARRAY['user'::text, 'group'::text])))
);


--
-- Name: TABLE storage_default_managers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_default_managers IS 'Who every new storage resource is delegated to when it is created. Materialised, never consulted at attach time: a resource''s create writes one storage_managers row per entry (origin default), which is then revocable on that resource alone. A table rather than a settings key because each entry carries provenance and a subject that can be deleted.';


--
-- Name: COLUMN storage_default_managers.user_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_default_managers.user_id IS 'The subject when subject_type is user. A foreign key, unlike storage_managers.subject_id, so a deleted subject takes its default with it.';


--
-- Name: COLUMN storage_default_managers.group_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_default_managers.group_id IS 'The subject when subject_type is group; cascades as user_id does.';


--
-- Name: COLUMN storage_default_managers.max_access; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_default_managers.max_access IS 'read or full, the two levels every kind''s ladder has. The ceiling of each delegation it writes, which confers the data plane at that level as any delegation does.';


--
-- Name: storage_deletions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_deletions (
    slug text NOT NULL,
    resource_id text NOT NULL,
    kind text NOT NULL,
    backing text NOT NULL,
    storage_class text DEFAULT ''::text NOT NULL,
    storage_root text DEFAULT ''::text NOT NULL,
    claims jsonb DEFAULT '[]'::jsonb NOT NULL,
    volumes jsonb DEFAULT '[]'::jsonb NOT NULL,
    report jsonb,
    predates_claims boolean DEFAULT false NOT NULL,
    actor_id text,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    kept boolean DEFAULT false NOT NULL
);


--
-- Name: TABLE storage_deletions; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_deletions IS 'Storage resources deleted in Toft whose volumes the cluster has not finished deleting. Holds the slug until it has; report is NULL until the destroy has been dispatched.';


--
-- Name: COLUMN storage_deletions.claims; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_deletions.claims IS 'The resource''s storage_claims, moved here by the delete''s transaction.';


--
-- Name: COLUMN storage_deletions.volumes; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_deletions.volumes IS 'The PersistentVolumes the destroy flipped to Delete, which must be gone before the slug is free.';


--
-- Name: COLUMN storage_deletions.kept; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_deletions.kept IS 'The delete has finished and left a cluster volume''s directory on the file system (report.left_behind). The row then holds the slug until DELETE /admin/storage/deleting/{slug} releases it, because a volume created under the same name would be handed those files. The reconciler skips kept rows.';


--
-- Name: storage_drives; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_drives (
    key text NOT NULL,
    label text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    updated_by text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_drives_description_check CHECK ((length(description) <= 2000)),
    CONSTRAINT storage_drives_key_check CHECK ((key ~ '^(root|class):.+$'::text)),
    CONSTRAINT storage_drives_label_check CHECK (((length(label) >= 1) AND (length(label) <= 60)))
);


--
-- Name: TABLE storage_drives; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_drives IS 'What an administrator calls each storage drive — a host root (root:<name>) or a cluster class (class:<name>). The key is the operator''s and is what resources record; a label reaches only the admin pages.';


--
-- Name: storage_gates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.storage_gates (
    group_name text NOT NULL,
    resource_id text NOT NULL,
    folder text DEFAULT ''::text NOT NULL,
    gid integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone
);


--
-- Name: TABLE storage_gates; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_gates IS 'The host system group gating one storage directory under a shared storage root, and its gid. The database is the source of the number; the host''s /etc/group is a cache of it, recreated at every start. Never deleted: a retired gate keeps its number because the files on the disk still carry it.';


--
-- Name: COLUMN storage_gates.folder; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_gates.folder IS 'Empty is the whole resource''s gate.';


--
-- Name: COLUMN storage_gates.retired_at; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_gates.retired_at IS 'Set when the resource holding the gate is deleted. The number stays allocated.';


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
    origin text DEFAULT 'admin'::text NOT NULL,
    CONSTRAINT storage_managers_access_check CHECK ((max_access = ANY (ARRAY['read'::text, 'write'::text, 'full'::text]))),
    CONSTRAINT storage_managers_origin_check CHECK ((origin = ANY (ARRAY['admin'::text, 'default'::text]))),
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
-- Name: COLUMN storage_managers.origin; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_managers.origin IS 'admin: written on the resource by an administrator. default: written from storage_default_managers when the resource was created, or when the default was added with apply_existing. Editing the ceiling keeps it; it is provenance, not a kind.';


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
    source text DEFAULT 'console'::text NOT NULL,
    CONSTRAINT storage_mappings_access_check CHECK ((access = ANY (ARRAY['read'::text, 'write'::text, 'full'::text]))),
    CONSTRAINT storage_mappings_source_check CHECK ((source = ANY (ARRAY['console'::text, 'manifest'::text])))
);


--
-- Name: TABLE storage_mappings; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_mappings IS 'Where a resource appears on one app''s filesystem, and at what level. A mapping is an app''s and a grant is a person''s; neither implies the other. Stamped with the authority that wrote it, so revoking that authority collects exactly those.';


--
-- Name: COLUMN storage_mappings.source; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_mappings.source IS 'console: attached by a person on the console or the API, and kept until a person detaches it. manifest: declared in the serving release''s toft.toml [[storage]], stamped to the app owner''s delegation, written only at a deploy''s flip and removed when the entry goes.';


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
    partitioned boolean DEFAULT false NOT NULL,
    CONSTRAINT storage_resources_backing_check CHECK ((((kind = 'volume'::text) AND (backing = ANY (ARRAY['efs'::text, 's3files'::text, 'host'::text, 'azurefiles'::text]))) OR ((kind = 'store'::text) AND (backing = ANY (ARRAY['mountpoint'::text, 'blob'::text]))))),
    CONSTRAINT storage_resources_kind_check CHECK ((kind = ANY (ARRAY['volume'::text, 'store'::text]))),
    CONSTRAINT storage_resources_name_is_slug CHECK ((name = slug)),
    CONSTRAINT storage_resources_root_check CHECK ((((storage_root = ''::text) AND (backing <> 'host'::text)) OR ((storage_root <> ''::text) AND (kind = 'volume'::text))))
);


--
-- Name: TABLE storage_resources; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.storage_resources IS 'Durable storage for apps: admin-provisioned resources, mapped onto apps at absolute paths and delivered to app code as a filesystem path and nothing else. Two kinds, one admin surface — a volume is a real POSIX filesystem, a store is an object-store prefix. The durable resource always pre-exists; Toft creates only the objects that bind it to a workload.';


--
-- Name: COLUMN storage_resources.partitioned; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.storage_resources.partitioned IS 'Mapped only by folder, folders never overlap, and on a host root each folder is its own gate. Fixed at creation.';


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
    CONSTRAINT store_grants_access_check CHECK ((access = ANY (ARRAY['read'::text, 'write'::text, 'full'::text]))),
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
-- Name: trusted_issuers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.trusted_issuers (
    id text NOT NULL,
    display_name text NOT NULL,
    issuer text NOT NULL,
    audience text NOT NULL,
    subject_claim text DEFAULT 'sub'::text NOT NULL,
    provider_id text DEFAULT ''::text NOT NULL,
    key_lifetime_minutes integer DEFAULT 480 NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_by text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT trusted_issuers_key_lifetime_check CHECK (((key_lifetime_minutes >= 60) AND (key_lifetime_minutes <= 1440))),
    CONSTRAINT trusted_issuers_subject_claim_check CHECK ((subject_claim = ANY (ARRAY['sub'::text, 'email'::text])))
);


--
-- Name: TABLE trusted_issuers; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.trusted_issuers IS 'Issuers an administrator trusts to vouch for a person: an identity token from one, carrying its audience, is exchanged for a short-lived deploy-scoped key (toftx_) for the account it names. Nothing is trusted by default.';


--
-- Name: COLUMN trusted_issuers.subject_claim; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.trusted_issuers.subject_claim IS 'How the token names its person: sub, looked up as a user_identities subject on provider_id; or email, matched against exactly one account and only when email_verified is true.';


--
-- Name: COLUMN trusted_issuers.provider_id; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.trusted_issuers.provider_id IS 'The sign-in provider whose identities a sub names. No foreign key, for user_identities.provider_id''s reason: a vanished provider leaves the issuer inert and visible.';


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
    last_used_ip text DEFAULT ''::text NOT NULL,
    scope text DEFAULT 'full'::text NOT NULL,
    CONSTRAINT user_api_keys_scope_check CHECK ((scope = ANY (ARRAY['read'::text, 'full'::text])))
);


--
-- Name: TABLE user_api_keys; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON TABLE public.user_api_keys IS 'Personal API keys: a long-lived credential that authenticates its owner to the platform API and to the apps gateway. user_id is ON DELETE CASCADE — a personal key is its account and must not outlive it.';


--
-- Name: COLUMN user_api_keys.key_hash; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_api_keys.key_hash IS 'A SHA-256 of the secret. The plaintext is shown once at creation and never stored.';


--
-- Name: COLUMN user_api_keys.scope; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_api_keys.scope IS 'read or full: which routes this key may call, never which objects it may reach. A read key is refused every non-GET on the allow-list and reaches every GET on it; a full key is the allow-list alone, which is what every key minted before this column had. Chosen at mint and never edited, so nothing writes it after the insert. DEFAULT full is what made the upgrade silent; an unrecognised value resolves to read in Go, because this is an allow-list that fails closed.';


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
    external_id text DEFAULT ''::text NOT NULL,
    email_verified boolean DEFAULT false NOT NULL
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

COMMENT ON COLUMN public.user_identities.external_id IS 'The provider''s own id for the account, not a Toft reference.';


--
-- Name: COLUMN user_identities.email_verified; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.user_identities.email_verified IS 'The provider said, at the last sign-in, that this identity''s email is verified. A user is a verified-email linking target only through an identity that is.';


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
    pending_activation_at timestamp with time zone,
    scanner_since timestamp with time zone,
    scanner_set_by text,
    time_zone text DEFAULT ''::text NOT NULL
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
-- Name: COLUMN users.scanner_since; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.scanner_since IS 'When an administrator marked this account as the scanner. NULL: not a scanner. Believed for X-Toft-Scan and admitted to the estate read; never read by an authorization path.';


--
-- Name: COLUMN users.scanner_set_by; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.scanner_set_by IS 'The administrator who set the scanner mark. NULL once that account is deleted.';


--
-- Name: COLUMN users.time_zone; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.users.time_zone IS 'Display only. Empty follows the install''s display_time_zone; "browser" is this person''s browser zone whatever the install says; otherwise "UTC" or an IANA name. Nothing is stored or computed in it.';


--
-- Name: advisory_affected id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.advisory_affected ALTER COLUMN id SET DEFAULT nextval('public.advisory_affected_id_seq'::regclass);


--
-- Name: app_accounts id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_accounts ALTER COLUMN id SET DEFAULT nextval('public.app_accounts_id_seq'::regclass);


--
-- Name: app_consumption_intervals id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_consumption_intervals ALTER COLUMN id SET DEFAULT nextval('public.app_consumption_intervals_id_seq'::regclass);


--
-- Name: app_platform_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_platform_log ALTER COLUMN id SET DEFAULT nextval('public.app_platform_log_id_seq'::regclass);


--
-- Name: job_run_logs id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_run_logs ALTER COLUMN id SET DEFAULT nextval('public.job_run_logs_id_seq'::regclass);


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
-- Name: api_key_usage_daily api_key_usage_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_key_usage_daily
    ADD CONSTRAINT api_key_usage_daily_pkey PRIMARY KEY (key_id, day, app_id, status_class);


--
-- Name: app_accounts app_accounts_number_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_accounts
    ADD CONSTRAINT app_accounts_number_key UNIQUE (number);


--
-- Name: app_accounts app_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_accounts
    ADD CONSTRAINT app_accounts_pkey PRIMARY KEY (id);


--
-- Name: app_audience_daily app_audience_daily_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_audience_daily
    ADD CONSTRAINT app_audience_daily_pkey PRIMARY KEY (app_id, day);


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
    ADD CONSTRAINT app_events_source_check CHECK ((source = ANY (ARRAY['builder'::text, 'autodeploy'::text, 'health'::text, 'cron'::text, 'jobs'::text, 'integrations'::text, 'dependency'::text]))) NOT VALID;


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
-- Name: app_restart_schedules app_restart_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_restart_schedules
    ADD CONSTRAINT app_restart_schedules_pkey PRIMARY KEY (id);


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
    ADD CONSTRAINT apps_status_check CHECK ((status = ANY (ARRAY['building'::text, 'running'::text, 'crashed'::text, 'stopped'::text, 'failed'::text, 'sleeping'::text, 'ready'::text]))) NOT VALID;


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
-- Name: connect_tokens connect_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.connect_tokens
    ADD CONSTRAINT connect_tokens_pkey PRIMARY KEY (token_id);


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
-- Name: dependency_scans dependency_scans_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dependency_scans
    ADD CONSTRAINT dependency_scans_pkey PRIMARY KEY (id);


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
    ADD CONSTRAINT deployments_triggered_by_check CHECK ((triggered_by = ANY (ARRAY['manual'::text, 'auto'::text, 'rollback'::text, 'render'::text, 'move'::text]))) NOT VALID;


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
-- Name: exchanged_keys exchanged_keys_key_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.exchanged_keys
    ADD CONSTRAINT exchanged_keys_key_hash_key UNIQUE (key_hash);


--
-- Name: exchanged_keys exchanged_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.exchanged_keys
    ADD CONSTRAINT exchanged_keys_pkey PRIMARY KEY (id);


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
-- Name: goose_db_version goose_db_version_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.goose_db_version
    ADD CONSTRAINT goose_db_version_pkey PRIMARY KEY (id);


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
-- Name: integration_places integration_places_connection_id_conversation_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.integration_places
    ADD CONSTRAINT integration_places_connection_id_conversation_id_key UNIQUE (connection_id, conversation_id);


--
-- Name: integration_places integration_places_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.integration_places
    ADD CONSTRAINT integration_places_pkey PRIMARY KEY (id);


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
-- Name: object_grants object_grants_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.object_grants
    ADD CONSTRAINT object_grants_pkey PRIMARY KEY (object_id, principal_type, principal_id);


--
-- Name: object_grants object_grants_role_check; Type: CHECK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.object_grants
    ADD CONSTRAINT object_grants_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'editor'::text, 'viewer'::text, 'list'::text]))) NOT VALID;


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
-- Name: storage_claims storage_claims_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_claims
    ADD CONSTRAINT storage_claims_pkey PRIMARY KEY (resource_id, claim);


--
-- Name: storage_default_managers storage_default_managers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_default_managers
    ADD CONSTRAINT storage_default_managers_pkey PRIMARY KEY (id);


--
-- Name: storage_deletions storage_deletions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_deletions
    ADD CONSTRAINT storage_deletions_pkey PRIMARY KEY (slug);


--
-- Name: storage_drives storage_drives_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_drives
    ADD CONSTRAINT storage_drives_pkey PRIMARY KEY (key);


--
-- Name: storage_gates storage_gates_gid_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_gates
    ADD CONSTRAINT storage_gates_gid_key UNIQUE (gid);


--
-- Name: storage_gates storage_gates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_gates
    ADD CONSTRAINT storage_gates_pkey PRIMARY KEY (group_name);


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
-- Name: trusted_issuers trusted_issuers_issuer_audience_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trusted_issuers
    ADD CONSTRAINT trusted_issuers_issuer_audience_key UNIQUE (issuer, audience);


--
-- Name: trusted_issuers trusted_issuers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trusted_issuers
    ADD CONSTRAINT trusted_issuers_pkey PRIMARY KEY (id);


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
-- Name: announcement_emails_resend_of_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX announcement_emails_resend_of_idx ON public.announcement_emails USING btree (resend_of) WHERE (resend_of IS NOT NULL);


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
-- Name: app_accounts_live_name; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX app_accounts_live_name ON public.app_accounts USING btree (name) WHERE (retired_at IS NULL);


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

CREATE UNIQUE INDEX app_integration_sends_idem_idx ON public.app_integration_sends USING btree (app_id, idempotency_key, day) WHERE (idempotency_key IS NOT NULL);


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
-- Name: app_restart_schedules_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX app_restart_schedules_app_idx ON public.app_restart_schedules USING btree (app_id);


--
-- Name: app_restart_schedules_due_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_restart_schedules_due_idx ON public.app_restart_schedules USING btree (next_run_at) WHERE enabled;


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
-- Name: app_usage_daily_day_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_usage_daily_day_idx ON public.app_usage_daily USING btree (day);


--
-- Name: app_usage_daily_user_day_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_usage_daily_user_day_idx ON public.app_usage_daily USING btree (user_id, day);


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
-- Name: apps_hostname_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX apps_hostname_key ON public.apps USING btree (hostname) WHERE (hostname IS NOT NULL);


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
-- Name: audit_log_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_log_app_idx ON public.audit_log USING btree (app_id, occurred_at DESC, id DESC) WHERE (app_id <> ''::text);


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
-- Name: connect_tokens_user_api_key_id; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX connect_tokens_user_api_key_id ON public.connect_tokens USING btree (user_api_key_id);


--
-- Name: credentials_created_by_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX credentials_created_by_idx ON public.credentials USING btree (created_by) WHERE (created_by IS NOT NULL);


--
-- Name: dependency_scans_app_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX dependency_scans_app_idx ON public.dependency_scans USING btree (app_id, occurred_at DESC);


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
-- Name: exchanged_keys_expires_at_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX exchanged_keys_expires_at_idx ON public.exchanged_keys USING btree (expires_at);


--
-- Name: exchanged_keys_user_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX exchanged_keys_user_id_idx ON public.exchanged_keys USING btree (user_id);


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
-- Name: job_runs_idem_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_idem_idx ON public.job_runs USING btree (app_id, idempotency_key, queued_at DESC) WHERE (idempotency_key IS NOT NULL);


--
-- Name: job_runs_queued_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_queued_idx ON public.job_runs USING btree (queued_at) WHERE (status = 'queued'::text);


--
-- Name: job_runs_rerun_of_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_rerun_of_idx ON public.job_runs USING btree (rerun_of) WHERE (rerun_of IS NOT NULL);


--
-- Name: job_runs_stop_owed_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX job_runs_stop_owed_idx ON public.job_runs USING btree (stop_owed_at) WHERE (stop_owed_at IS NOT NULL);


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
-- Name: storage_default_managers_group_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX storage_default_managers_group_idx ON public.storage_default_managers USING btree (group_id) WHERE (group_id IS NOT NULL);


--
-- Name: storage_default_managers_user_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX storage_default_managers_user_idx ON public.storage_default_managers USING btree (user_id) WHERE (user_id IS NOT NULL);


--
-- Name: storage_drives_label_lower; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX storage_drives_label_lower ON public.storage_drives USING btree (lower(label));


--
-- Name: storage_gates_live_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX storage_gates_live_key ON public.storage_gates USING btree (resource_id, folder) WHERE (retired_at IS NULL);


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
-- Name: storage_mappings storage_mappings_partition; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER storage_mappings_partition BEFORE INSERT OR UPDATE ON public.storage_mappings FOR EACH ROW EXECUTE FUNCTION public.enforce_storage_mapping_partition();


--
-- Name: storage_resources storage_resources_partitioned_fixed; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER storage_resources_partitioned_fixed BEFORE UPDATE ON public.storage_resources FOR EACH ROW EXECUTE FUNCTION public.enforce_storage_partitioned_fixed();


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
-- Name: app_restart_schedules app_restart_schedules_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_restart_schedules
    ADD CONSTRAINT app_restart_schedules_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;


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
-- Name: connect_tokens connect_tokens_user_api_key_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.connect_tokens
    ADD CONSTRAINT connect_tokens_user_api_key_id_fkey FOREIGN KEY (user_api_key_id) REFERENCES public.user_api_keys(id) ON DELETE CASCADE;


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
-- Name: dependency_scans dependency_scans_app_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dependency_scans
    ADD CONSTRAINT dependency_scans_app_id_fkey FOREIGN KEY (app_id) REFERENCES public.apps(id) ON DELETE CASCADE;


--
-- Name: dependency_scans dependency_scans_deployment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.dependency_scans
    ADD CONSTRAINT dependency_scans_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES public.deployments(id) ON DELETE SET NULL;


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
-- Name: exchanged_keys exchanged_keys_issuer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.exchanged_keys
    ADD CONSTRAINT exchanged_keys_issuer_id_fkey FOREIGN KEY (issuer_id) REFERENCES public.trusted_issuers(id) ON DELETE CASCADE;


--
-- Name: exchanged_keys exchanged_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.exchanged_keys
    ADD CONSTRAINT exchanged_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


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
-- Name: integration_places integration_places_added_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.integration_places
    ADD CONSTRAINT integration_places_added_by_fkey FOREIGN KEY (added_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: integration_places integration_places_connection_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.integration_places
    ADD CONSTRAINT integration_places_connection_id_fkey FOREIGN KEY (connection_id) REFERENCES public.integration_connections(id) ON DELETE CASCADE;


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
-- Name: job_runs job_runs_rerun_of_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_runs
    ADD CONSTRAINT job_runs_rerun_of_fkey FOREIGN KEY (rerun_of) REFERENCES public.job_runs(id) ON DELETE SET NULL;


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
-- Name: storage_claims storage_claims_resource_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_claims
    ADD CONSTRAINT storage_claims_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES public.storage_resources(id) ON DELETE CASCADE;


--
-- Name: storage_default_managers storage_default_managers_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_default_managers
    ADD CONSTRAINT storage_default_managers_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: storage_default_managers storage_default_managers_group_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_default_managers
    ADD CONSTRAINT storage_default_managers_group_id_fkey FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;


--
-- Name: storage_default_managers storage_default_managers_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_default_managers
    ADD CONSTRAINT storage_default_managers_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: storage_deletions storage_deletions_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_deletions
    ADD CONSTRAINT storage_deletions_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- Name: storage_drives storage_drives_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.storage_drives
    ADD CONSTRAINT storage_drives_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id) ON DELETE SET NULL;


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
-- Name: trusted_issuers trusted_issuers_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.trusted_issuers
    ADD CONSTRAINT trusted_issuers_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id) ON DELETE SET NULL;


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
-- Name: users users_scanner_set_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_scanner_set_by_fkey FOREIGN KEY (scanner_set_by) REFERENCES public.users(id) ON DELETE SET NULL;


--
-- PostgreSQL database dump complete
--


