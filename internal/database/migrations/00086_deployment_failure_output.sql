-- +goose Up
--
-- What a start that failed printed, line by line. The tail rode only in
-- failure_message's prose, so `toft logs` put it back with the failure's one
-- timestamp and stream stderr on every line, a print to stdout included
-- (BB-038, fourth 2026-11-03 black-box list).
ALTER TABLE deployments
    ADD COLUMN failure_output text NOT NULL DEFAULT '';
COMMENT ON COLUMN deployments.failure_output IS 'A JSON array of {at, stream, line}: the workload''s own output a readiness failure read before the workload was deleted, each line with its own time and stream. Empty when the failure carried none, and on every row written before the column.';

-- +goose Down
ALTER TABLE deployments DROP COLUMN failure_output;
