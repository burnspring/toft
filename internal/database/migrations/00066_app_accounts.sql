-- +goose Up
--
-- The number behind each app's own OS account and its per-app group (ADR §122).
--
-- An auto-created account lived in a container's /etc/passwd and took whatever
-- uid `useradd --system` chose, so every restart recreated the accounts in the
-- order apps next started and the numbers on the disk — release trees, the
-- group-gated app directory, files in shared storage — came to belong to other
-- apps. The database is the source of the number now, as storage_gates is for
-- a gate's gid: allocated once, and every start makes the account at exactly it.
--
-- One number per account, used as its uid and as its per-app group's gid, so
-- `ls -ln` names the app the same way in both columns. A deleted app's row is
-- retired, never deleted and never reused: files written under its number can
-- outlive it in shared storage and in backups, and a number that is never
-- handed out again needs no condition on whether such files still exist.
CREATE TABLE app_accounts (
    id bigserial PRIMARY KEY,
    name text NOT NULL,
    number integer NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    retired_at timestamptz
);

CREATE UNIQUE INDEX app_accounts_live_name ON app_accounts (name) WHERE retired_at IS NULL;

COMMENT ON TABLE app_accounts IS
    'The uid of each auto-created per-app OS account, which is also its per-app group''s gid. The database is the source of the number; a container''s /etc/passwd and /etc/group are a cache of it, recreated at every start. Retired rather than deleted: the number is never reused.';
COMMENT ON COLUMN app_accounts.name IS
    'The account and group name, toft-app-<slug> (runtime accountName). One live row per name.';
COMMENT ON COLUMN app_accounts.number IS
    'The uid, and the per-app group''s gid. From APP_ACCOUNT_ID_RANGE (default 63000-64999), unique across live and retired rows.';
COMMENT ON COLUMN app_accounts.retired_at IS
    'When the app was deleted. A retired number is never allocated again.';

-- +goose Down
DROP TABLE app_accounts;
