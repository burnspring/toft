-- +goose Up
--
-- A failed deploy's code beside its reason. failure_reason is the category a
-- program branches on (storage_refused, manifest_invalid); failure_code is
-- which refusal inside it (storage_unknown, storage_revoked_during_deploy,
-- removed_key) — the code `toft check` and an upload refusal already answer
-- with, so a deploy refused later, during the build, says the same thing.
ALTER TABLE deployments
    ADD COLUMN failure_code text NOT NULL DEFAULT '';
COMMENT ON COLUMN deployments.failure_code IS 'The refusal''s own code inside failure_reason, when the failure has one (storage_unknown under storage_refused, removed_key under manifest_invalid). Empty for a failure with no finer code, and on every row written before the column.';

-- +goose Down
ALTER TABLE deployments DROP COLUMN failure_code;
