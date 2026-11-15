-- +goose Up
--
-- A job that stops succeeding says so (ADR §105). A job that fails already
-- sends a notice; one that is paused and forgotten, refused by its app's queue
-- limit, or never placed sends nothing, and that silence is what this column is
-- for. NULL is "no expectation", which is every job written before it.
ALTER TABLE job_definitions ADD COLUMN expect_within_sec integer;
ALTER TABLE job_definitions ADD CONSTRAINT job_definitions_expect_within_check
    CHECK (expect_within_sec IS NULL OR expect_within_sec BETWEEN 3600 AND 2678400);

-- The floor of the overdue window: a job created or switched back on is not
-- overdue on arrival. Not updated_at, which every deploy's manifest reconcile
-- writes and would reset the window on each one.
ALTER TABLE job_definitions ADD COLUMN enabled_at timestamptz;
UPDATE job_definitions SET enabled_at = created_at;
ALTER TABLE job_definitions ALTER COLUMN enabled_at SET DEFAULT now();
ALTER TABLE job_definitions ALTER COLUMN enabled_at SET NOT NULL;

COMMENT ON COLUMN public.job_definitions.expect_within_sec IS 'How long this job may go without a successful run before job_run_overdue is raised, in seconds (1 hour to 31 days). NULL is no expectation. On a manifest row it is the repository''s, written by every deploy like schedule; the UI may set it only on a UI row.';
COMMENT ON COLUMN public.job_definitions.enabled_at IS 'When this definition was created or last switched from disabled to enabled — the floor of the overdue window, so a new or re-enabled job is not overdue on arrival. Never touched by a deploy that leaves enabled alone.';

-- +goose Down
ALTER TABLE job_definitions DROP CONSTRAINT job_definitions_expect_within_check;
ALTER TABLE job_definitions DROP COLUMN expect_within_sec;
ALTER TABLE job_definitions DROP COLUMN enabled_at;
