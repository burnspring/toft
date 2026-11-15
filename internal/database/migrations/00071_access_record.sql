-- +goose Up
--
-- The access record: whose history an audit row belongs to, the person behind
-- a key's traffic, and the index the retention sweep cuts the rollup by.
--
-- audit_log.app_id is the app a row is part of the history of, which is not
-- always its target: a denied access request is a fact about a request and
-- part of an app's history. Empty, never null (the schema's own rules), and no
-- foreign key, for the table's own reason: the history of a deleted app is
-- still its history.
ALTER TABLE audit_log ADD COLUMN app_id text NOT NULL DEFAULT '';

-- The backfill reads the log and nothing else. A join to today's mappings or
-- grants would file a fact from last spring under whichever app a resource is
-- attached to now.
UPDATE audit_log SET app_id = target_id
 WHERE target_type = 'app' AND target_id <> '';
UPDATE audit_log SET app_id = actor_id
 WHERE app_id = '' AND actor_type = 'app' AND actor_id IS NOT NULL;
UPDATE audit_log SET app_id = after->>'object_id'
 WHERE app_id = '' AND action = 'access_request.create' AND target_type = 'object_access'
   AND COALESCE(after->>'object_id', '') <> '';
UPDATE audit_log d SET app_id = c.app_id
  FROM audit_log c
 WHERE d.app_id = '' AND d.action IN ('access_request.deny', 'access_request.withdraw')
   AND d.target_type = 'access_request'
   AND c.action = 'access_request.create' AND c.target_id = d.target_id AND c.app_id <> '';

-- The feed's keyset order, and only the rows that carry an app: the install's
-- own rows, sign-ins above all, are most of the table.
CREATE INDEX audit_log_app_idx ON audit_log (app_id, occurred_at DESC, id DESC) WHERE app_id <> '';

-- Where the attribution starts. Only on a log that already has rows, so a
-- fresh install carries no label about a past it does not have; inside the
-- record it qualifies, so retention removes it when every row it describes is
-- gone.
INSERT INTO audit_log (id, actor_type, action, target_type, target_id, occurred_at)
SELECT gen_random_uuid()::text, 'system', 'audit_log.app_scope_started', 'platform', 'audit_log', now()
 WHERE EXISTS (SELECT 1 FROM audit_log);

COMMENT ON COLUMN audit_log.app_id IS
    'The app this row is part of the history of, or empty for an install-wide act. Derived from an app target or an app actor where the row is not told otherwise; internal/database/audit_scope.go says which actions must carry one.';
COMMENT ON TABLE audit_log IS
    'Append-only audit trail, deliberately with no foreign keys. An audit record has to survive the deletion of its actor and its target, which is most of the point of keeping it, and a foreign key would prevent the deletion or erase the attribution. Its one deletion path is the access_record sweep, governed by access_record_retention_days (0 keeps everything), and every pass that removes rows writes audit_log.pruned.';

-- The person a key's traffic belonged to, kept on the row so it outlives the
-- key and the account: user_api_keys cascades away with its owner.
ALTER TABLE app_usage_daily ADD COLUMN key_owner_user_id text NOT NULL DEFAULT '';
UPDATE app_usage_daily u SET key_owner_user_id = k.user_id
  FROM user_api_keys k
 WHERE u.user_id = 'key:' || k.id;
UPDATE app_usage_daily u SET key_owner_user_id = a.target_id
  FROM audit_log a
 WHERE u.key_owner_user_id = '' AND u.user_id LIKE 'key:%'
   AND a.action = 'user.api_key_create' AND u.user_id = 'key:' || (a.after->>'key_id');
COMMENT ON COLUMN app_usage_daily.key_owner_user_id IS
    'For a key: row, the account the key belonged to on the day of the traffic; empty otherwise. No foreign key, so it outlives the person.';

-- The sweep cuts by day; the primary key leads with app_id.
CREATE INDEX app_usage_daily_day_idx ON app_usage_daily (day);

-- +goose Down
-- Destroys the attribution and the key owners; the rows themselves stay.
DROP INDEX IF EXISTS app_usage_daily_day_idx;
ALTER TABLE app_usage_daily DROP COLUMN key_owner_user_id;
DELETE FROM audit_log WHERE action = 'audit_log.app_scope_started';
DROP INDEX IF EXISTS audit_log_app_idx;
ALTER TABLE audit_log DROP COLUMN app_id;
COMMENT ON TABLE audit_log IS
    'Append-only audit trail; there is deliberately no deletion path, and deliberately no foreign keys. An audit record has to survive the deletion of its actor and its target — that is most of the point of keeping it — and a foreign key is precisely the thing that would prevent the deletion or erase the attribution.';
