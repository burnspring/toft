-- +goose Up
--
-- Jobs as Toft's one automation concept (ADR § One automation concept).
--
-- Definitions come only from toft.toml: the UI and the API operate jobs and
-- never define them, so `source` and the UI-made rows go. Nothing re-runs by
-- itself, so `max_attempts` and the retry chain on runs go. The platform does
-- not gate a viewer's start (the app's own button does), so `viewer_runnable`
-- goes. Parameter *values* on a definition were the UI's; a declaration's
-- defaults live in `params_spec`, so `params` goes too.
--
-- New on a definition: `args` (a single binary's argv, instead of a shell
-- command), `overlap` (what a scheduled firing does when the last run is still
-- going) and `notify` (who hears about a run's outcome).
DELETE FROM job_definitions WHERE source = 'ui';
ALTER TABLE job_definitions DROP CONSTRAINT job_definitions_source_check;
ALTER TABLE job_definitions DROP COLUMN source;
ALTER TABLE job_definitions DROP COLUMN viewer_runnable;
ALTER TABLE job_definitions DROP COLUMN max_attempts;
ALTER TABLE job_definitions DROP COLUMN params;
ALTER TABLE job_definitions ADD COLUMN args jsonb;
ALTER TABLE job_definitions ADD COLUMN overlap text NOT NULL DEFAULT 'queue';
ALTER TABLE job_definitions ADD COLUMN notify text NOT NULL DEFAULT 'none';
ALTER TABLE job_definitions ADD CONSTRAINT job_definitions_overlap_check
    CHECK (overlap IN ('skip', 'queue'));
ALTER TABLE job_definitions ADD CONSTRAINT job_definitions_notify_check
    CHECK (notify IN ('none', 'failures', 'always'));
COMMENT ON COLUMN job_definitions.args IS
    'The argv of a single built binary, instead of command. NULL when command is used.';
COMMENT ON COLUMN job_definitions.overlap IS
    'A scheduled firing while the last run is still going: skip records a skipped run, queue waits.';
COMMENT ON COLUMN job_definitions.notify IS
    'Who hears about a run''s outcome: none, failures (one message per failed run) or always.';
COMMENT ON COLUMN job_definitions.enabled IS
    'False while paused from the Automation tab. A deploy re-applies toft.toml and resumes it.';

-- Runs. One run ends exactly once and nothing re-runs it: the retry chain goes,
-- and a deliberate re-run points at the run it repeats instead.
DROP INDEX job_runs_pending_idx;
DROP INDEX job_runs_idem_live_idx;
DROP INDEX job_runs_active_idx;
ALTER TABLE job_runs DROP CONSTRAINT job_runs_status_check;
ALTER TABLE job_runs DROP CONSTRAINT job_runs_trigger_check;

UPDATE job_runs SET status = 'queued' WHERE status = 'pending';
UPDATE job_runs SET status = 'out_of_memory' WHERE status = 'resource_exceeded';
UPDATE job_runs SET status = 'failed', failure_stage = CASE WHEN failure_stage = '' THEN 'platform' ELSE failure_stage END
    WHERE status = 'infrastructure_error';

ALTER TABLE job_runs RENAME COLUMN trigger TO started_by;
UPDATE job_runs SET started_by = CASE
    WHEN started_by = 'cron' THEN 'schedule'
    WHEN started_by = 'manual' THEN 'ui'
    WHEN started_by = 'api' THEN 'cli'
    WHEN started_by = 'app' AND actor_id IS NOT NULL THEN 'viewer'
    ELSE 'app' END;
ALTER TABLE job_runs ALTER COLUMN started_by SET DEFAULT 'ui';
ALTER TABLE job_runs ALTER COLUMN status SET DEFAULT 'queued';

ALTER TABLE job_runs DROP COLUMN attempt;
ALTER TABLE job_runs DROP COLUMN retry_of;
ALTER TABLE job_runs DROP COLUMN not_before;
ALTER TABLE job_runs DROP COLUMN result;
ALTER TABLE job_runs ADD COLUMN rerun_of text REFERENCES job_runs(id) ON DELETE SET NULL;
ALTER TABLE job_runs ADD COLUMN progress_message text NOT NULL DEFAULT '';
ALTER TABLE job_runs ADD COLUMN peak_memory_mb integer;

ALTER TABLE job_runs ADD CONSTRAINT job_runs_status_check CHECK (status IN (
    'queued', 'claimed', 'running',
    'succeeded', 'failed', 'timed_out', 'out_of_memory', 'cancelled', 'skipped'));
ALTER TABLE job_runs ADD CONSTRAINT job_runs_started_by_check
    CHECK (started_by IN ('app', 'viewer', 'cli', 'schedule', 'ui'));

CREATE INDEX job_runs_queued_idx ON job_runs (queued_at) WHERE status = 'queued';
CREATE INDEX job_runs_active_idx ON job_runs (status) WHERE status IN ('claimed', 'running');
CREATE INDEX job_runs_idem_idx ON job_runs (app_id, idempotency_key, queued_at DESC)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX job_runs_rerun_of_idx ON job_runs (rerun_of) WHERE rerun_of IS NOT NULL;

COMMENT ON COLUMN job_runs.status IS
    'queued, claimed (a node is starting it), running; then exactly one of succeeded, failed, timed_out, out_of_memory, cancelled — or skipped, a scheduled firing the overlap policy declined.';
COMMENT ON COLUMN job_runs.started_by IS
    'app (the app as itself), viewer (the app for a signed-in person, actor_id set), cli (a key or the CLI), schedule, ui (Run now).';
COMMENT ON COLUMN job_runs.rerun_of IS
    'The run this one deliberately repeats (toft job rerun, Run again). Nothing re-runs automatically.';
COMMENT ON COLUMN job_runs.progress_message IS
    'The short message beside progress, as the job last reported it.';
COMMENT ON COLUMN job_runs.peak_memory_mb IS
    'The most memory the run was seen to use. NULL when never reported.';

DELETE FROM settings WHERE key = 'max_run_retries';

-- +goose Down
-- Not reversible: UI-made definitions, the retry chain and stored results are
-- gone. Restore from a backup taken before this migration to get them back.
SELECT 1;
