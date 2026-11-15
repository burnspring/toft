-- +goose Up
--
-- Where an inventory row came from. `resolved` is every row written before
-- this and every non-R ecosystem: read off a lockfile or a manifest before
-- the build. The other three are R's, read off the installed library:
-- `installed` the build placed on the app's behalf, `platform` Toft placed
-- whatever the app said (renv, the SDK and its dependencies), and `runtime`
-- the sweep found on disk that the deploy did not — a package the app
-- installed while it ran. The sweep appends runtime rows and never deletes one.
ALTER TABLE deployment_dependencies
    ADD COLUMN source text NOT NULL DEFAULT 'resolved';

COMMENT ON COLUMN deployment_dependencies.source IS 'resolved (lockfile or manifest), installed (the build put it in the release library for the app), platform (Toft put it there), or runtime (found by the sweep after the deploy). Runtime rows are appended, never removed.';

-- +goose Down
ALTER TABLE deployment_dependencies DROP COLUMN source;
