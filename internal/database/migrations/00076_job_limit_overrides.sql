-- +goose Up
--
-- A job's limits become an administrator's: two nullable overrides, NULL being
-- the install's job default (job_memory_limit_mb, job_cpu_limit_millicores).
-- The deploy upsert never names them, so an override survives every redeploy.
--
-- The manifest stops declaring memory and cpu, so the two declared columns
-- go — read first, because an upgrade must change no job's memory. Each job's
-- effective memory today was: declared, else its app's override, else the
-- install's app default, capped at max_job_memory_mb. Written as an override
-- wherever it differs from the new 1024 MiB job default.
--
-- The install's app default may be the environment's (APP_MEMORY_LIMIT_MB),
-- which SQL cannot see. Those jobs get a negative sentinel, -<the ceiling
-- that applied>, which the binary resolves at its next start against the
-- environment (database.SeedJobMemoryOverrides) and which nothing else reads.
-- The declared cpu was parsed and never read, so no CPU override is seeded.
ALTER TABLE job_definitions
    ADD COLUMN override_memory_mb integer,
    ADD COLUMN override_cpu_millicores integer;

COMMENT ON COLUMN job_definitions.override_memory_mb IS 'An administrator''s memory limit for this job, in MiB. NULL is the install''s job_memory_limit_mb. Never written by a deploy.';
COMMENT ON COLUMN job_definitions.override_cpu_millicores IS 'An administrator''s CPU limit for this job, in millicores. NULL is the install''s job_cpu_limit_millicores. Never written by a deploy.';

WITH ceiling AS (
    SELECT COALESCE((SELECT NULLIF(value, '')::integer FROM settings
                     WHERE key = 'max_job_memory_mb' AND value ~ '^[0-9]+$'), 8192) AS mb
), install AS (
    SELECT (SELECT value::integer FROM settings
            WHERE key = 'app_memory_limit_mb' AND value ~ '^[0-9]+$' AND value::integer > 0) AS mb
), effective AS (
    SELECT j.id,
           CASE
               WHEN j.memory_mb > 0 THEN LEAST(j.memory_mb, c.mb)
               WHEN a.memory_limit_mb > 0 THEN LEAST(a.memory_limit_mb, c.mb)
               WHEN i.mb IS NOT NULL THEN LEAST(i.mb, c.mb)
               ELSE -c.mb
           END AS mb
    FROM job_definitions j
    JOIN apps a ON a.id = j.app_id
    CROSS JOIN ceiling c
    CROSS JOIN install i
)
UPDATE job_definitions j SET override_memory_mb = e.mb
FROM effective e
WHERE e.id = j.id AND e.mb <> 1024;

ALTER TABLE job_definitions DROP COLUMN memory_mb, DROP COLUMN cpu_millis;

DELETE FROM settings WHERE key = 'max_job_memory_mb';

-- +goose Down
ALTER TABLE job_definitions
    ADD COLUMN cpu_millis integer NOT NULL DEFAULT 500,
    ADD COLUMN memory_mb integer NOT NULL DEFAULT 512;
UPDATE job_definitions SET memory_mb = override_memory_mb WHERE override_memory_mb > 0;
ALTER TABLE job_definitions DROP COLUMN override_memory_mb, DROP COLUMN override_cpu_millicores;
