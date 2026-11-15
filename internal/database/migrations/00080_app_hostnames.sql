-- +goose Up
--
-- An app on a hostname of its own, and the base path a release was built for.
--
-- `hostname` is the name an administrator recorded for the app and
-- `hostname_enabled` whether it is served; the two are separate because the
-- name usually exists before its DNS record, and backing an app out should not
-- cost retyping it. The unique index covers disabled names too, so switching
-- one on can never collide.
--
-- `deployments.base_path` is the path a release was built for, and
-- `apps.release_base_path` the serving release's, written in the same
-- statement as `current_release`. Serving reads these, never the app's current
-- slug: a release built under one address and served under another is how a
-- renamed app came to answer 404. The backfill uses the current slug, which is
-- wrong only for releases built before a rename — the apps that were already
-- broken, and that their next move rebuilds.
ALTER TABLE apps ADD COLUMN hostname text;
ALTER TABLE apps ADD COLUMN hostname_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE apps ADD COLUMN release_base_path text;
CREATE UNIQUE INDEX apps_hostname_key ON apps (hostname) WHERE hostname IS NOT NULL;
ALTER TABLE apps ADD CONSTRAINT apps_hostname_lower CHECK (hostname = lower(hostname));
ALTER TABLE apps ADD CONSTRAINT apps_hostname_needs_name CHECK (NOT hostname_enabled OR hostname IS NOT NULL);

ALTER TABLE deployments ADD COLUMN base_path text;

-- A move: a rebuild of the code already running, at a new base path.
ALTER TABLE deployments DROP CONSTRAINT deployments_triggered_by_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_triggered_by_check
    CHECK (triggered_by = ANY (ARRAY['manual', 'auto', 'rollback', 'render', 'move'])) NOT VALID;
COMMENT ON COLUMN deployments.triggered_by IS 'Not a person. This is the category of trigger — manual, auto, rollback, render, move — and the CHECK beside it is the whole vocabulary. Named _by for historical reasons and deliberately not renamed: the name is on the wire in models.go JSON tags and the SPA reads it.';

UPDATE apps SET release_base_path = '/apps/' || slug WHERE current_release <> '';

COMMENT ON COLUMN apps.hostname IS 'A DNS name of the app''s own, set by an administrator; NULL when none. Unique whether or not it is served.';
COMMENT ON COLUMN apps.hostname_enabled IS 'Whether the hostname is served. A switch beside the name, so a name can be recorded before its DNS exists.';
COMMENT ON COLUMN apps.release_base_path IS 'The base path the serving release was built for: /apps/{slug}, or empty at the root of a hostname. NULL when nothing serves. Every reader of the serving path reads this, not the slug.';
COMMENT ON COLUMN deployments.base_path IS 'The base path this release was built for; NULL on rows from before the column.';

-- +goose Down
ALTER TABLE deployments DROP CONSTRAINT deployments_triggered_by_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_triggered_by_check
    CHECK (triggered_by = ANY (ARRAY['manual', 'auto', 'rollback', 'render'])) NOT VALID;
ALTER TABLE deployments DROP COLUMN base_path;
ALTER TABLE apps DROP CONSTRAINT apps_hostname_needs_name;
ALTER TABLE apps DROP CONSTRAINT apps_hostname_lower;
DROP INDEX apps_hostname_key;
ALTER TABLE apps DROP COLUMN release_base_path;
ALTER TABLE apps DROP COLUMN hostname_enabled;
ALTER TABLE apps DROP COLUMN hostname;
