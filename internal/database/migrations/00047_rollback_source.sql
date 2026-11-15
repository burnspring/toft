-- +goose Up
--
-- The release a rollback restores, on the row from the moment it is queued.
--
-- A rollback answers 202 and queues a pending deployment row; the release it
-- is returning to rode only on the in-memory build job. "Delete old releases"
-- pressed between the queue and the worker found that release unprotected —
-- `protects` guards in-progress rows, the live release and the current row,
-- and the source of a queued rollback was none of those — and deleted it, and
-- the rollback then failed with "that release no longer exists (pruned)".
-- The row carries the source now, and `protects` reads it.
--
-- Empty for every row that is not a rollback, and for every rollback queued
-- before this column, which is the same "none recorded" the other text
-- columns on this table mean by an empty string.
ALTER TABLE deployments ADD COLUMN source_deploy_id text NOT NULL DEFAULT '';

COMMENT ON COLUMN public.deployments.source_deploy_id IS 'For a rollback, the deployment whose release it restores, recorded when the rollback is queued so a bulk release deletion cannot remove it while the rollback waits for the worker. Empty for anything that is not a rollback.';

-- +goose Down
ALTER TABLE deployments DROP COLUMN source_deploy_id;
