-- +goose Up
--
-- The scanner account: an ordinary account an administrator has marked as the
-- one an AppSec tool signs in as (docs/prds/concept-scanner-account-no-app-keys.md).
-- The mark does two things and no third — `X-Toft-Scan` is believed from it at
-- the gateway, and it may read /admin/estate/targets — and it never grants: no
-- authorization path reads it.
--
-- On the account rather than a list in `settings`, so it dies with the account
-- and is seen beside the person it describes. A time and who, like a lock, so
-- "since when has this account's scan traffic been uncounted" needs no audit
-- query.
ALTER TABLE users ADD COLUMN scanner_since timestamptz;
ALTER TABLE users ADD COLUMN scanner_set_by text REFERENCES users(id) ON DELETE SET NULL;

COMMENT ON COLUMN users.scanner_since IS
    'When an administrator marked this account as the scanner. NULL: not a scanner. Believed for X-Toft-Scan and admitted to the estate read; never read by an authorization path.';
COMMENT ON COLUMN users.scanner_set_by IS
    'The administrator who set the scanner mark. NULL once that account is deleted.';

-- +goose Down
ALTER TABLE users DROP COLUMN scanner_set_by;
ALTER TABLE users DROP COLUMN scanner_since;
