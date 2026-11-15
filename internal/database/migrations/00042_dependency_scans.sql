-- +goose Up
--
-- The sweep says it ran, and the platform's own lock gets somewhere to sign its
-- name.
--
-- **Why a table rather than a query.** A dependency finding is *derived*:
-- FindingsFor joins deployment_dependencies against advisory_affected at read
-- time, and ReplaceSnapshot swaps an ecosystem's whole corpus in one
-- transaction. An advisory that is withdrawn upstream, or whose affected range
-- is narrowed, takes its finding with it -- and no query run tomorrow can
-- reproduce what a tab said today. History is written at the moment of the
-- reading or it is not available at all, which is the fact that makes this a
-- table and not a view.
--
-- **A row is written whether or not anything changed.** The question a history
-- answers is *when did we last look, and what did we see*, and a table that
-- only recorded changes could not tell "checked, unchanged for thirty days"
-- from "not checked for thirty days". That is `not recorded is not unknown`
-- applied to a new object rather than a new rule.
--
-- **It is a summary plus the notable advisory ids, never a copy of the
-- findings.** Storing the full finding set thirty times over would duplicate
-- the inventory join for a question nobody asks; storing counts alone would
-- make the one question a history is *for* -- "which advisory arrived on 3
-- August, and is it the one still open" -- unanswerable. The ids are the
-- smallest thing that keeps it answerable, and they are a JSON array with no
-- CHECK, following deployments.dependency_verdict: a shape, not a vocabulary.
--
-- Retention is a **count** per app, pruned inline in the same transaction as
-- the write. Bounded by construction, no second timer, and nothing to drift.
-- app_event_retention_days is deliberately not reused: an event is a problem
-- that opened and closed, a scan is an observation, and folding them into one
-- dial would make "keep six months of scans" depend on how often the install
-- sweeps.
CREATE TABLE dependency_scans (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    -- The release the reading was taken against. Nullable and ON DELETE SET
    -- NULL for app_events.deployment_id's reason: a release can be pruned
    -- while the observation of it is still the answer to "what did we see in
    -- August".
    deployment_id text REFERENCES deployments(id) ON DELETE SET NULL,
    trigger text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    -- Coverage, per the policy's own three rungs: recorded is not scanned, and
    -- matched is not covered. False here is *unknown*, and no surface may
    -- render it as "no findings".
    scanned boolean DEFAULT false NOT NULL,
    -- The corpus age the reading was taken against. Every number this feature
    -- shows is scoped to a feed and its age, and a reading with no feed behind
    -- it is a reading against nothing.
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
    CONSTRAINT dependency_scans_trigger_check
        CHECK ((trigger = ANY (ARRAY['deploy'::text, 'scheduled'::text, 'manual'::text])))
);

CREATE INDEX dependency_scans_app_idx ON dependency_scans (app_id, occurred_at DESC);

COMMENT ON TABLE dependency_scans IS 'One row per app per pass of the dependency sweep: the observation, never the decision. A finding is derived against a corpus that is swapped wholesale, so a reading is only available at the moment it is taken, and this is where it is kept. deployments.dependency_verdict remains the release decision, and nothing derives one from the other.';

COMMENT ON COLUMN dependency_scans.trigger IS 'Why this reading was taken: deploy, scheduled or manual. Three values and no fourth, because an automatic consequence needs an automatic trigger, and the lock decision reads this to know whether a person was the one asking.';

COMMENT ON COLUMN dependency_scans.scanned IS 'Whether the release was covered, in the policy sense: an inventory in an ecosystem this build matches against. False is unknown and never renders as a clean bill of health.';

COMMENT ON COLUMN dependency_scans.advisories IS 'The notable advisory ids as a JSON array, capped. No CHECK, following deployments.dependency_verdict: it is a shape, not a vocabulary. Counts alone cannot answer which advisory arrived on 3 August, and the full finding set thirty times over answers nothing extra.';

-- **The platform's lock needs an attributable actor, and locked_by cannot be
-- one.** It is a foreign key into users with ON DELETE SET NULL, and NULL
-- already carries two meanings -- no lock, and a lock whose administrator has
-- since been deleted. A lock the platform placed has no user id to store,
-- cannot invent one, and must not take NULL as a third meaning.
--
-- A synthetic users row was the obvious alternative and is worse in a way that
-- spreads: a row that can never sign in is one every people listing, every seat
-- count, every directory sweep, every group membership query and every "deleted
-- administrator" branch has to learn to exclude, forever, to avoid one
-- migration.
--
-- DEFAULT 'admin' and NOT NULL, so this is a no-op for every existing row --
-- which is exactly what every existing row means -- and needs no backfill.
ALTER TABLE apps ADD COLUMN locked_source text NOT NULL DEFAULT 'admin';

COMMENT ON COLUMN apps.locked_source IS 'Who placed the lock that is on this app: admin, or dependency for one the dependency policy placed automatically. It is what tells apart the three cases locked_by cannot: no lock, an administrator since deleted, and the platform itself. Inert while locked_at is NULL.';

-- +goose Down
--
-- The table goes and the history with it, which is the point of the feature and
-- has nowhere else to be kept: a reading is only available at the moment it is
-- taken, so what is dropped here cannot be re-derived on the way back up. An
-- operator going back should expect the install's answer to "when was this last
-- checked" to start again from the next pass.
--
-- The column goes too, and with it the distinction between a lock an
-- administrator placed and one the platform did. Every surviving lock reads as
-- an administrator's, which is what the build below this understood a lock to
-- be. Nothing is unlocked by going back: no verdict withdraws anything.
DROP TABLE dependency_scans;
ALTER TABLE apps DROP COLUMN locked_source;
