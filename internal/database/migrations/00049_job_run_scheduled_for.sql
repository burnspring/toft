-- +goose Up
--
-- The tick a scheduled run belongs to (ADR §104). The claim selects a schedule
-- by its next_run_at and then advances it, so until now the instant a run was
-- *for* was thrown away the moment it was known: a run that waited an hour
-- behind its app's queue limit could not tell it was the 03:00 run, and a re-run
-- of Tuesday's work on Thursday had no way to say it was Tuesday's.
ALTER TABLE job_runs ADD COLUMN scheduled_for timestamptz;

COMMENT ON COLUMN public.job_runs.scheduled_for IS 'The schedule tick this run belongs to — the claimed next_run_at, never the dispatch time — or the instant a person named for it (the Run for dialog, or lp job run with a for flag). NULL on a run nobody gave one: a manual or app-started run is never assumed to be for now. A retry copies its original''s. Reaches the workload as LAUNCHPAD_SCHEDULED_FOR, in the schedule''s own zone.';

-- +goose Down
ALTER TABLE job_runs DROP COLUMN scheduled_for;
