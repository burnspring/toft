-- +goose Up
--
-- The product was renamed Toft, a clean break taken while no install was in
-- the field. Two things in the database said the old name, and both are
-- corrected here rather than in the frozen baseline, which is never edited:
--
-- 1. Sixteen comments, re-issued with the new name. A comment is the
--    schema's own documentation, and one naming a product that no longer
--    exists is read as a description of this one.
-- 2. The built-in administrator's address, which the baseline seeded as
--    admin@launchpad.local and the binary now creates as admin@toft.local.
--    Only the seeded value moves: an administrator who changed the address
--    keeps theirs.
COMMENT ON COLUMN advisories.snapshot_id IS 'Which feed snapshot brought this row in. Not a Toft reference: it names a fetch, and advisory_feed_state holds the current one per ecosystem.';
COMMENT ON COLUMN advisory_feed_state.snapshot_id IS 'The snapshot currently in force for this ecosystem. Not a Toft reference.';
COMMENT ON COLUMN audit_log.request_id IS 'The request that caused this, for correlation with the access log. Not a Toft reference.';
COMMENT ON COLUMN auth_providers.client_id IS 'The OAuth client id at the provider. Not a Toft reference.';
COMMENT ON COLUMN git_connections.github_app_id IS 'The GitHub App''s own identifier. Renamed from app_id, which said something false: app_id means a Toft app in twenty-one other tables in this schema.';
COMMENT ON COLUMN git_connections.installation_id IS 'The git host''s own installation identifier. Not a Toft reference.';
COMMENT ON COLUMN git_connections.client_id IS 'The OAuth client id at the git host. Not a Toft reference.';
COMMENT ON COLUMN integration_connections.scope_id IS 'The far side''s own id for the scope this connection is welded to. Not a Toft reference, and a new scope is a new connection rather than an edit to this.';
COMMENT ON COLUMN integration_connections.default_destination_id IS 'The far side''s own id, not a Toft reference.';
COMMENT ON COLUMN integration_placements.destination_id IS 'The far side''s own id for the folder or channel — not a Toft reference.';
COMMENT ON COLUMN integration_placements.item_id IS 'The far side''s own id for what was placed — not a Toft reference.';
COMMENT ON COLUMN job_runs.external_id IS 'The backend''s own handle for the workload — a pod name, a task ARN. Not a Toft reference.';
COMMENT ON COLUMN job_runs.scheduled_for IS 'The schedule tick this run belongs to — the claimed next_run_at, never the dispatch time — or the instant a person named for it (the Run for dialog, or toft job run with a for flag). NULL on a run nobody gave one: a manual or app-started run is never assumed to be for now. A retry copies its original''s. Reaches the workload as TOFT_SCHEDULED_FOR, in the schedule''s own zone.';
COMMENT ON TABLE storage_resources IS 'Durable storage for apps: admin-provisioned resources, mapped onto apps at absolute paths and delivered to app code as a filesystem path and nothing else. Two kinds, one admin surface — a volume is a real POSIX filesystem, a store is an object-store prefix. The durable resource always pre-exists; Toft creates only the objects that bind it to a workload.';
COMMENT ON COLUMN user_identities.external_id IS 'The provider''s own id for the account, not a Toft reference.';

UPDATE users SET email = 'admin@toft.local'
 WHERE id = 'builtin-admin' AND email = 'admin@launchpad.local';
UPDATE user_identities SET email = 'admin@toft.local'
 WHERE provider_id = 'local' AND user_id = 'builtin-admin' AND email = 'admin@launchpad.local';

-- +goose Down
-- Written to be read, never run (docs/dev-reference.md § Backend Conventions):
-- the comments would be re-issued with the old name, and the two addresses
-- moved back where they still read admin@toft.local.
UPDATE users SET email = 'admin@launchpad.local'
 WHERE id = 'builtin-admin' AND email = 'admin@toft.local';
UPDATE user_identities SET email = 'admin@launchpad.local'
 WHERE provider_id = 'local' AND user_id = 'builtin-admin' AND email = 'admin@toft.local';
