-- +goose Up
--
-- When each line of a build log was written (ADR §155), beside the text rather
-- than in it, so every reader of build_log reads exactly what it did before.
ALTER TABLE deployments
    ADD COLUMN build_log_times text NOT NULL DEFAULT '';
COMMENT ON COLUMN deployments.build_log_times IS 'v1:<start unix ms>:<offset ms>,… — one offset per line of build_log, from the top. Empty for a build that recorded none; shorter than the log when lines were appended after the build (the restart sweep).';

-- +goose Down
ALTER TABLE deployments DROP COLUMN build_log_times;
