-- +goose Up
--
-- The release a rollback restored, kept for good. source_deploy_id says the
-- row owns no tree and is cleared when a rollback across base paths is
-- rebuilt into a tree of its own, so the rebuilt row could not say what it
-- restored: a client anchoring the next rollback on it went forward to the
-- newest release instead (BB-005, 2026-11-04 black-box list).
ALTER TABLE deployments
    ADD COLUMN restored_from text NOT NULL DEFAULT '';
COMMENT ON COLUMN deployments.restored_from IS 'The deployment a rollback restored, set when the rollback is queued and never cleared — unlike source_deploy_id, which a rollback rebuilt for a new base path gives up. Empty on a row that is not a rollback.';
UPDATE deployments SET restored_from = source_deploy_id WHERE source_deploy_id <> '';

-- +goose Down
ALTER TABLE deployments DROP COLUMN restored_from;
