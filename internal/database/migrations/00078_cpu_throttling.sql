-- +goose Up
--
-- CPU throttling, measured beside CPU: how many enforcement periods a
-- workload ran in, in how many it was held at its CPU limit, and for how long
-- — window deltas, never raw counters, as cpu_seconds is. NULL where the
-- workload's cgroup keeps no such counters (no cgroup of its own, or no cpu
-- controller): absent, never zero. app_cpu_throttled reads the last hour of
-- intervals.
ALTER TABLE app_consumption_intervals
    ADD COLUMN periods bigint,
    ADD COLUMN throttled_periods bigint,
    ADD COLUMN throttled_seconds double precision;

ALTER TABLE app_consumption_daily
    ADD COLUMN periods bigint,
    ADD COLUMN throttled_periods bigint,
    ADD COLUMN throttled_seconds double precision;

-- A job run's own figure, beside its peak memory: the seconds it spent held
-- at its CPU limit. NULL where it was not measured.
ALTER TABLE job_runs ADD COLUMN cpu_throttled_seconds double precision;

COMMENT ON COLUMN app_consumption_intervals.throttled_periods IS 'Enforcement periods within the window in which the workload was held at its CPU limit; NULL where not measured.';
COMMENT ON COLUMN job_runs.cpu_throttled_seconds IS 'Seconds the run was held at its CPU limit, raised as the runner reports; NULL where not measured.';

-- +goose Down
ALTER TABLE job_runs DROP COLUMN cpu_throttled_seconds;
ALTER TABLE app_consumption_daily DROP COLUMN periods, DROP COLUMN throttled_periods, DROP COLUMN throttled_seconds;
ALTER TABLE app_consumption_intervals DROP COLUMN periods, DROP COLUMN throttled_periods, DROP COLUMN throttled_seconds;
