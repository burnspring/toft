-- +goose Up
--
-- App keys (`toft_…`) are removed (docs/prds/concept-scanner-account-no-app-keys.md).
-- Machine access to an app is an account the identity provider issued, granted
-- on that app, with its personal key; scanning is the marked scanner account
-- (00064). Existing keys are deleted, not migrated: there is nothing they could
-- become, and a presented one is refused at both doors with a sentence that
-- says so.
--
-- Their traffic rows go with them. The audit rows that recorded their
-- creation, renames and revocations stay — history is not rewritten because
-- the mechanism it describes is gone. app_usage_daily keeps its `key:` rows for
-- the same reason; the read that named them simply no longer finds a name.
DROP TABLE app_api_keys;

DELETE FROM api_key_usage_daily WHERE key_kind = 'app';
ALTER TABLE api_key_usage_daily DROP CONSTRAINT api_key_usage_daily_key_kind_check;
ALTER TABLE api_key_usage_daily ADD CONSTRAINT api_key_usage_daily_key_kind_check
    CHECK (key_kind = 'user');

-- Three comments pointed at the dropped table.
COMMENT ON COLUMN deploy_keys.key_hash IS
    'A SHA-256 of the secret. The plaintext is shown once at creation and never stored.';
COMMENT ON COLUMN user_api_keys.key_hash IS
    'A SHA-256 of the secret. The plaintext is shown once at creation and never stored.';
COMMENT ON TABLE user_api_keys IS
    'Personal API keys: a long-lived credential that authenticates its owner to the platform API and to the apps gateway. user_id is ON DELETE CASCADE — a personal key is its account and must not outlive it.';

-- +goose Down
-- Written to be read: the keys and their traffic are not recoverable.
ALTER TABLE api_key_usage_daily DROP CONSTRAINT api_key_usage_daily_key_kind_check;
ALTER TABLE api_key_usage_daily ADD CONSTRAINT api_key_usage_daily_key_kind_check
    CHECK (key_kind = ANY (ARRAY['app'::text, 'user'::text]));
CREATE TABLE app_api_keys (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name text NOT NULL,
    key_hash text NOT NULL UNIQUE,
    created_by text REFERENCES users(id) ON DELETE SET NULL,
    key_prefix text DEFAULT '' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    expires_at timestamptz,
    disabled_at timestamptz,
    last_used_at timestamptz,
    caller_role text,
    UNIQUE (app_id, name)
);
