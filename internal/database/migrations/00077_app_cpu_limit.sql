-- +goose Up
--
-- One app's own CPU limit, set by an administrator beside its memory limit. In
-- millicores: an amount of CPU time, 1000 being one core's worth spread across
-- any of the machine's cores. 0 means the install's limit, never "uncapped".
ALTER TABLE apps ADD COLUMN cpu_limit_millicores integer NOT NULL DEFAULT 0;

COMMENT ON COLUMN apps.cpu_limit_millicores IS 'An administrator''s CPU limit for this app, in millicores. 0 means the install''s app_cpu_limit_millicores.';

-- +goose Down
ALTER TABLE apps DROP COLUMN cpu_limit_millicores;
