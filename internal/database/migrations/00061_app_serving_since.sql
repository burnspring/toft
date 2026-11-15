-- +goose Up
--
-- Two clocks `toft status` and the app pages had no honest source for. The
-- serving deployment's finished_at is when it was *built*: after a rollback it
-- is the original build's time, and after a stop it still sat beside the word
-- `stopped`.
--
-- serving_since is when current_release last changed — the flip — written by
-- UpdateApp and NULL when nothing is serving. status_changed_at is when status
-- last took a different value, written by UpdateAppStatus and the build sweep.
-- Rows that predate this migration have neither (NULL is "not recorded", never
-- a time); new rows are born with status_changed_at = their insert.
ALTER TABLE apps ADD COLUMN serving_since timestamptz;
ALTER TABLE apps ADD COLUMN status_changed_at timestamptz;
ALTER TABLE apps ALTER COLUMN status_changed_at SET DEFAULT now();

COMMENT ON COLUMN apps.serving_since IS
    'When current_release last changed (the flip of a deploy, rollback or promote). NULL: nothing serving, or not recorded before migration 00061.';
COMMENT ON COLUMN apps.status_changed_at IS
    'When status last took a different value. NULL: not recorded before migration 00061.';

-- +goose Down
ALTER TABLE apps DROP COLUMN status_changed_at;
ALTER TABLE apps DROP COLUMN serving_since;
