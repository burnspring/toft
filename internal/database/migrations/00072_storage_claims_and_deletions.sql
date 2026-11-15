-- +goose Up
--
-- Deleting a volume on a cluster deletes its files (ADR §12a), which needs two
-- things the schema did not have.
--
-- storage_claims is every volume claim a resource was ever given, written
-- before the claim is created. The delete runs only once every mapping is gone,
-- so it cannot ask the mappings which folders existed, and a name pattern
-- cannot answer either: lpv-a- followed by eight hex characters is also the
-- root claim of the valid slug a-deadbeef. The list is the specification.
CREATE TABLE storage_claims (
    resource_id text NOT NULL REFERENCES storage_resources(id) ON DELETE CASCADE,
    claim       text NOT NULL,
    folder      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resource_id, claim)
);

COMMENT ON TABLE storage_claims IS 'Every volume claim a storage resource was ever given, recorded before the claim exists, so the resource delete can destroy exactly those volumes and no other.';

-- What can be named is named: each volume's root claim, and one claim per
-- folder it is mapped at today. Folder claims the sweep released before this
-- migration are not on the cluster's live list and are not invented here; a
-- delete of such a resource says so.
INSERT INTO storage_claims (resource_id, claim, folder)
SELECT id, claim_name, '' FROM storage_resources WHERE kind = 'volume'
ON CONFLICT DO NOTHING;

INSERT INTO storage_claims (resource_id, claim, folder)
SELECT DISTINCT r.id,
       'lpv-' || r.slug || '-' || left(encode(sha256(convert_to(m.folder, 'UTF8')), 'hex'), 8),
       m.folder
  FROM storage_mappings m
  JOIN storage_resources r ON r.id = m.resource_id
 WHERE r.kind = 'volume' AND m.folder <> ''
ON CONFLICT DO NOTHING;

-- storage_deletions is a delete the cluster has not finished. The resource row
-- goes in the delete's own transaction, as it always did, so grants, mappings
-- and managers cascade at once; this row holds the slug until every volume the
-- delete flipped is gone, because a slug used again provisions onto the same
-- directory and the late DeleteVolume would empty the new resource.
--
-- A table of its own rather than a state on storage_resources: every reader of
-- that table would otherwise have to learn to skip it.
CREATE TABLE storage_deletions (
    slug        text PRIMARY KEY,
    resource_id text NOT NULL,
    kind        text NOT NULL,
    backing     text NOT NULL,
    storage_class text NOT NULL DEFAULT '',
    storage_root  text NOT NULL DEFAULT '',
    claims      jsonb NOT NULL DEFAULT '[]',
    volumes     jsonb NOT NULL DEFAULT '[]',
    report      jsonb,
    -- Created before storage_claims existed, so folders the sweep released
    -- back then may still be on the file system with nothing naming them.
    predates_claims boolean NOT NULL DEFAULT false,
    actor_id    text REFERENCES users(id) ON DELETE SET NULL,
    started_at  timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE storage_deletions IS 'Storage resources deleted in Toft whose volumes the cluster has not finished deleting. Holds the slug until it has; report is NULL until the destroy has been dispatched.';
COMMENT ON COLUMN storage_deletions.claims IS 'The resource''s storage_claims, moved here by the delete''s transaction.';
COMMENT ON COLUMN storage_deletions.volumes IS 'The PersistentVolumes the destroy flipped to Delete, which must be gone before the slug is free.';

-- +goose Down
DROP TABLE storage_deletions;
DROP TABLE storage_claims;
