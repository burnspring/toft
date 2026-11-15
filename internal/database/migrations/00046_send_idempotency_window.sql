-- +goose Up
--
-- The idempotency index is bounded by time, the way the job-run index is
-- bounded by status.
--
-- `IdempotencyWindow` is 24 hours: a key like `weekly-report-2026-W32` is
-- remembered for a day and reusable after, so a repeat inside the window
-- answers with the original send and a repeat next week is a new send. The
-- pre-check and the recovery lookup both honoured that window; the unique
-- index did not. On day 8 the pre-check found nothing, the insert hit the
-- index, the recovery lookup — also window-bound — found nothing, and the app
-- received a 500 for a key it could not use again until retention deleted the
-- row. For a send that left a placement the row is kept on purpose, so the
-- key was burnt for good.
--
-- A partial index cannot compare to now(), so the bound is a fact of the row:
-- `day` is the UTC date the send was created, stored as a generated column,
-- and the index is unique on (app_id, idempotency_key, day). Within a day two
-- replays racing past the pre-check still collide — that is what the index is
-- for — and across days the key is free, which is what the constant promised.
-- The window the pre-check applies stays 24 hours; the index now permits
-- what the pre-check permits.
--
-- Not a status. Sends have terminal statuses the way runs do, but a failed
-- send freeing its key for an immediate retry with the same key is what the
-- client's own retry policy is for, and a sent one should hold its key until
-- the window the constant names.
ALTER TABLE app_integration_sends
    ADD COLUMN day date GENERATED ALWAYS AS ((created_at AT TIME ZONE 'UTC')::date) STORED;

COMMENT ON COLUMN public.app_integration_sends.day IS 'The UTC date this send was created, generated from created_at. It exists so the idempotency index can be bounded by time: a partial index cannot compare to now(), and a key is single-use within a day and reusable after.';

DROP INDEX app_integration_sends_idem_idx;
CREATE UNIQUE INDEX app_integration_sends_idem_idx
    ON app_integration_sends (app_id, idempotency_key, day)
    WHERE idempotency_key IS NOT NULL;

-- +goose Down
--
-- Back to the unbounded index. It can fail: two rows with the same key on
-- different days are exactly what the bounded index admitted, and the
-- unbounded one refuses. Delete the later of each such pair first.
DROP INDEX app_integration_sends_idem_idx;
CREATE UNIQUE INDEX app_integration_sends_idem_idx
    ON app_integration_sends (app_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
ALTER TABLE app_integration_sends DROP COLUMN day;
