-- +goose Up
--
-- The folder of the repository an app is built from. Empty is the repository's
-- root, which is every app that existed before this column; a value is a clean
-- relative path the builder checks again before it clones. The gallery's
-- monorepo is why it exists: every item in it shares one repo_url, and the
-- folder is what tells them apart.
ALTER TABLE apps ADD COLUMN repo_subdir text NOT NULL DEFAULT '';
COMMENT ON COLUMN apps.repo_subdir IS 'The folder of repo_url the app is built from, relative and clean; empty is the repository root. Only the folder is placed in the release — the rest of the clone, .git included, is discarded.';

-- +goose Down
ALTER TABLE apps DROP COLUMN repo_subdir;
