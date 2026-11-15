-- +goose Up
--
-- Storage declared in toft.toml, and default attach rights (the
-- manifest-storage PRD). Nothing reads these until the builder and the
-- defaults routes land; every default below is what an existing row already
-- means.

-- Which half of the product wrote a mapping. The manifest owns what it
-- declares and only that: a deploy reconciles `manifest` rows and never
-- touches a `console` one, even one identical to an entry.
ALTER TABLE storage_mappings
    ADD COLUMN source text NOT NULL DEFAULT 'console'
        CHECK (source IN ('console', 'manifest'));
COMMENT ON COLUMN storage_mappings.source IS 'console: attached by a person on the console or the API, and kept until a person detaches it. manifest: declared in the serving release''s toft.toml [[storage]], stamped to the app owner''s delegation, written only at a deploy''s flip and removed when the entry goes.';

-- How a delegation came to exist. A default is materialised when the resource
-- is created, and from then on is an ordinary delegation; the origin is what
-- lets removing a default revoke exactly its own rows and nothing an
-- administrator chose per resource.
ALTER TABLE storage_managers
    ADD COLUMN origin text NOT NULL DEFAULT 'admin'
        CHECK (origin IN ('admin', 'default'));
COMMENT ON COLUMN storage_managers.origin IS 'admin: written on the resource by an administrator. default: written from storage_default_managers when the resource was created, or when the default was added with apply_existing. Editing the ceiling keeps it; it is provenance, not a kind.';

-- Who deployed. triggered_by is the category and stays; this is the principal,
-- which the manifest's check judges a storage change against.
ALTER TABLE deployments
    ADD COLUMN deployed_by_type text NOT NULL DEFAULT ''
        CHECK (deployed_by_type IN ('', 'user', 'service_account', 'deploy_key')),
    ADD COLUMN deployed_by_id text NOT NULL DEFAULT '';
COMMENT ON COLUMN deployments.deployed_by_type IS 'The principal that asked for this deployment: user, service_account or deploy_key; empty when no principal did (auto, render, move, and every row before 00082).';
COMMENT ON COLUMN deployments.deployed_by_id IS 'The id of deployed_by_type''s principal — for a service account, its owning user, whose delegations it is judged through. No foreign key: it is polymorphic, and the deployment row outlives the credential, as the audit log does.';

-- A deploy key an administrator has let add storage on the app owner's right.
ALTER TABLE deploy_keys
    ADD COLUMN may_attach boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT deploy_keys_may_attach_scope CHECK (NOT may_attach OR scope = 'deploy');
COMMENT ON COLUMN deploy_keys.may_attach IS 'A deploy carrying this key may add storage declared in toft.toml, judged on the app owner''s delegation alone — the administrator who set it stands in for the person a change otherwise needs. Only a deploy-scoped key may carry it.';

-- The install's default delegations: every resource created afterwards is
-- delegated to each subject here, as one ordinary storage_managers row.
CREATE TABLE storage_default_managers (
    id text PRIMARY KEY,
    subject_type text NOT NULL CHECK (subject_type IN ('user', 'group')),
    user_id text REFERENCES users(id) ON DELETE CASCADE,
    group_id text REFERENCES groups(id) ON DELETE CASCADE,
    max_access text NOT NULL CHECK (max_access IN ('read', 'full')),
    created_by text REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT storage_default_managers_subject CHECK (
        (subject_type = 'user' AND user_id IS NOT NULL AND group_id IS NULL) OR
        (subject_type = 'group' AND group_id IS NOT NULL AND user_id IS NULL))
);
CREATE UNIQUE INDEX storage_default_managers_user_idx ON storage_default_managers (user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX storage_default_managers_group_idx ON storage_default_managers (group_id) WHERE group_id IS NOT NULL;
COMMENT ON TABLE storage_default_managers IS 'Who every new storage resource is delegated to when it is created. Materialised, never consulted at attach time: a resource''s create writes one storage_managers row per entry (origin default), which is then revocable on that resource alone. A table rather than a settings key because each entry carries provenance and a subject that can be deleted.';
COMMENT ON COLUMN storage_default_managers.max_access IS 'read or full, the two levels every kind''s ladder has. The ceiling of each delegation it writes, which confers the data plane at that level as any delegation does.';
COMMENT ON COLUMN storage_default_managers.user_id IS 'The subject when subject_type is user. A foreign key, unlike storage_managers.subject_id, so a deleted subject takes its default with it.';
COMMENT ON COLUMN storage_default_managers.group_id IS 'The subject when subject_type is group; cascades as user_id does.';

-- +goose Down
DROP TABLE storage_default_managers;
ALTER TABLE deploy_keys DROP CONSTRAINT deploy_keys_may_attach_scope, DROP COLUMN may_attach;
ALTER TABLE deployments DROP COLUMN deployed_by_type, DROP COLUMN deployed_by_id;
ALTER TABLE storage_managers DROP COLUMN origin;
ALTER TABLE storage_mappings DROP COLUMN source;
