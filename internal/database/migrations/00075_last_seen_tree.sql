-- +goose Up
--
-- The folder a monorepo app is built from, as git's tree id at the commit its
-- last successful build cloned. The poller compares it with the folder's tree
-- at a new tip and, when they are equal, claims the commit without building:
-- a push elsewhere in the repository changed nothing this app is made of.
-- Empty for an app at the repository root, and for every app until its next
-- build; empty means the poller builds, as it always did.
ALTER TABLE apps ADD COLUMN last_seen_tree text NOT NULL DEFAULT '';

COMMENT ON COLUMN apps.last_seen_tree IS 'git tree id of repo_subdir at the commit the last successful build cloned. Cleared with last_seen_sha; empty means the poller builds on any new commit.';

-- +goose Down
ALTER TABLE apps DROP COLUMN last_seen_tree;
