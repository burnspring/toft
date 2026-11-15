-- +goose Up
--
-- Scheduled tasks are removed (ADR § One automation concept). A task was an
-- HTTP request into the app's web process on a schedule; everything it did is
-- now a job, declared in toft.toml, which runs as its own process, needs no
-- web process awake, and is not cut off by a request's limit. There are no
-- customers to migrate, so the tables go with their history rather than being
-- converted.
DROP TABLE cron_run_logs;
DROP TABLE app_cron_runs;
DROP TABLE app_cron_jobs;

DELETE FROM settings WHERE key IN (
    'max_scheduled_tasks_per_app',
    'scheduled_task_timeout_seconds',
    'scheduled_run_retention_days',
    'scheduled_run_result_bytes',
    'scheduled_log_retention_days'
);

-- +goose Down
-- Not reversible: the tables and their history are gone. Restore from a
-- backup taken before this migration to get them back.
SELECT 1;
