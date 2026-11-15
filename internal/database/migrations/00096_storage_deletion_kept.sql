-- +goose Up
--
-- A finished delete of a cluster volume whose files were kept. The chart's EFS
-- class roots every access point at /lpv-<slug>, so a resource created again
-- under the slug would be handed the deleted one's files. The tombstone now
-- stays, marked kept, and holds the name until an administrator releases it.
ALTER TABLE storage_deletions
    ADD COLUMN kept boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN storage_deletions.kept IS 'The delete has finished and left a cluster volume''s directory on the file system (report.left_behind). The row then holds the slug until DELETE /admin/storage/deleting/{slug} releases it, because a volume created under the same name would be handed those files. The reconciler skips kept rows.';

-- +goose Down
ALTER TABLE storage_deletions DROP COLUMN kept;
