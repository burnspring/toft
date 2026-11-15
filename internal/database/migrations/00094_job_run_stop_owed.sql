-- +goose Up
--
-- A stop the platform owes a job run's workload. Every path that ends a run
-- makes the row terminal first and then asks the backend to stop it; a stop
-- that failed was only logged, and every later control skips terminal rows, so
-- the workload ran on with the app's environment and mounts. Set with the
-- terminal write when the run has a workload, cleared by the jobs sweep once
-- the backend says the workload is gone.
ALTER TABLE job_runs
    ADD COLUMN stop_owed_at timestamptz;
COMMENT ON COLUMN job_runs.stop_owed_at IS 'When the platform began owing this terminal run a stop at its backend (timeout, cancel, lock, an orphaned start); NULL once the backend reports the workload gone, or when nothing was owed.';
CREATE INDEX job_runs_stop_owed_idx ON job_runs (stop_owed_at) WHERE stop_owed_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS job_runs_stop_owed_idx;
ALTER TABLE job_runs DROP COLUMN stop_owed_at;
