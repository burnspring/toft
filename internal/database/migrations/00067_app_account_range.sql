-- +goose Up
--
-- The account range's default widened from 63000-64999 to 100000-1999999
-- (ADR §122): a number is never reused, and two thousand would not outlast a
-- week of an install that creates and deletes apps as the e2e suite does.
-- Numbers already given keep them; only new allocations come from the range.
COMMENT ON COLUMN app_accounts.number IS
    'The uid, and the per-app group''s gid. From APP_ACCOUNT_ID_RANGE (default 100000-1999999; rows from before the default widened hold 63000-64999), unique across live and retired rows.';

-- +goose Down
COMMENT ON COLUMN app_accounts.number IS
    'The uid, and the per-app group''s gid. From APP_ACCOUNT_ID_RANGE (default 63000-64999), unique across live and retired rows.';
