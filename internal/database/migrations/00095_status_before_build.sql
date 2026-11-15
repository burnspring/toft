-- +goose Up
--
-- The status an app held before a build wrote `building`. A failed build puts
-- it back from a local variable, which dies with the process; the boot sweep
-- that settles builds a restart interrupted read nothing and wrote `running`,
-- so an app stopped or asleep before its build came back started.
ALTER TABLE apps
    ADD COLUMN status_before_build text NOT NULL DEFAULT '';
COMMENT ON COLUMN apps.status_before_build IS 'The status the app held when a build last wrote building (MarkAppBuilding); kept while another build is running. Read by FailOrphanedDeployments at boot so a stopped or sleeping app stays so; empty before any build.';

-- +goose Down
ALTER TABLE apps DROP COLUMN status_before_build;
