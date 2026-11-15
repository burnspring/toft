-- +goose Up
--
-- What a build detected, recorded on the deployment it belongs to.
--
-- The app row's framework, entrypoint and start_cmd are written at detect,
-- minutes before the new release serves, and a failed build never reverts
-- them. A workload of the serving release that bootstraps meanwhile -- a pod
-- restarted during a build, or after a failed one -- was told the *new*
-- release's entrypoint. The bootstrap now reads the workload's own deployment,
-- and the app row only for a deployment made before these columns existed,
-- which is what the empty default means.
ALTER TABLE deployments ADD COLUMN framework text NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN entrypoint text NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN start_cmd text NOT NULL DEFAULT '';

COMMENT ON COLUMN deployments.framework IS 'What this build detected, so a workload of this release is told what to run even after a later build rewrote the app row. Empty for a deployment made before migration 00073; readers fall back to the app row.';

-- +goose Down
ALTER TABLE deployments DROP COLUMN start_cmd;
ALTER TABLE deployments DROP COLUMN entrypoint;
ALTER TABLE deployments DROP COLUMN framework;
