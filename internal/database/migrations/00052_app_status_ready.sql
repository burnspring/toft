-- +goose Up
--
-- `ready` is a batch app with a published release (ADR §107): built, live, and
-- serving nothing, because it has no service. A word of its own rather than
-- `running`, which every reader of this column takes to mean a process answers.
ALTER TABLE apps DROP CONSTRAINT apps_status_check;
ALTER TABLE apps ADD CONSTRAINT apps_status_check CHECK ((status = ANY (ARRAY['building'::text, 'running'::text, 'crashed'::text, 'stopped'::text, 'failed'::text, 'sleeping'::text, 'ready'::text]))) NOT VALID;

-- +goose Down
UPDATE apps SET status = 'stopped' WHERE status = 'ready';
ALTER TABLE apps DROP CONSTRAINT apps_status_check;
ALTER TABLE apps ADD CONSTRAINT apps_status_check CHECK ((status = ANY (ARRAY['building'::text, 'running'::text, 'crashed'::text, 'stopped'::text, 'failed'::text, 'sleeping'::text]))) NOT VALID;
