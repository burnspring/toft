-- +goose Up
--
-- The offset a person named a run's instant in. scheduled_for is timestamptz,
-- which keeps the instant and drops the zone, so a run started for
-- 2026-11-03T09:00:00-05:00 reached the job as 14:00:00Z and was read back in
-- UTC (BB-045, fourth 2026-11-03 black-box list). The offset beside it lets
-- TOFT_SCHEDULED_FOR and every read of the run say the text as given.
ALTER TABLE job_runs
    ADD COLUMN scheduled_for_offset text NOT NULL DEFAULT '';
COMMENT ON COLUMN job_runs.scheduled_for_offset IS 'The UTC offset scheduled_for was named in, as RFC 3339 writes it (-05:00, +05:30), when a person named the instant with a non-UTC offset. Empty for a schedule tick (which reads in its schedule''s zone), for a UTC instant, for no instant, and on every row written before the column.';

-- +goose Down
ALTER TABLE job_runs DROP COLUMN scheduled_for_offset;
