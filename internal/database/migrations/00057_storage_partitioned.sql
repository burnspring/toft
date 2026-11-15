-- +goose Up
--
-- A partitioned resource: one directory handed out by folder, each folder its
-- own gate.
--
-- A resource is either whole — mapped whole or by folder, one gate, every app on
-- it trusting the others — or partitioned, mapped only by folder. The two cannot
-- be mixed on one resource because a Linux directory has one owning group: the
-- top level would have to be writable by a whole-resource app and traverse-only
-- for a folder app, which is an ACL. Fixed at creation, because re-gating a tree
-- that already holds files has no safe partial state.
ALTER TABLE storage_resources ADD COLUMN partitioned boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN storage_resources.partitioned IS 'Mapped only by folder, folders never overlap, and on a host root each folder is its own gate. Fixed at creation.';

-- The flag never moves. The API refuses it by name; this is the floor under that.
-- +goose StatementBegin
CREATE FUNCTION enforce_storage_partitioned_fixed() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.partitioned IS DISTINCT FROM OLD.partitioned THEN
        RAISE EXCEPTION 'storage_resources: resource % is % and that is fixed at creation',
            OLD.id, CASE WHEN OLD.partitioned THEN 'partitioned' ELSE 'whole' END;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER storage_resources_partitioned_fixed BEFORE UPDATE ON storage_resources
    FOR EACH ROW EXECUTE FUNCTION enforce_storage_partitioned_fixed();

-- A mapping on a partitioned resource names a folder, and no folder mapped there
-- contains another. Equal folders are how two apps share one. The match is on a
-- separator, never a string prefix — `report` does not contain `reports` — the
-- rule store_grants.go's coversPath states for grants. starts_with rather than
-- LIKE, because a folder may contain `_` and `%`.
--
-- The resource row is locked first, so two writes on one resource are
-- serialized here: otherwise `a` and `a/b` written at once would each pass,
-- each having looked before the other committed.
-- +goose StatementBegin
CREATE FUNCTION enforce_storage_mapping_partition() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    v_partitioned boolean;
    v_other text;
BEGIN
    SELECT partitioned INTO v_partitioned FROM storage_resources
     WHERE id = NEW.resource_id FOR UPDATE;
    IF NOT COALESCE(v_partitioned, false) THEN
        RETURN NEW;
    END IF;
    IF NEW.folder = '' THEN
        RAISE EXCEPTION 'storage_mappings: resource % is partitioned, so a mapping names a folder', NEW.resource_id;
    END IF;
    SELECT o.folder INTO v_other FROM storage_mappings o
     WHERE o.resource_id = NEW.resource_id AND o.id <> NEW.id AND o.folder <> NEW.folder
       AND (starts_with(o.folder, NEW.folder || '/') OR starts_with(NEW.folder, o.folder || '/'))
     LIMIT 1;
    IF v_other IS NOT NULL THEN
        RAISE EXCEPTION 'storage_mappings: folder % overlaps folder %, which is already mapped on partitioned resource %',
            NEW.folder, v_other, NEW.resource_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER storage_mappings_partition BEFORE INSERT OR UPDATE ON storage_mappings
    FOR EACH ROW EXECUTE FUNCTION enforce_storage_mapping_partition();

-- +goose Down
DROP TRIGGER storage_mappings_partition ON storage_mappings;
DROP FUNCTION enforce_storage_mapping_partition();
DROP TRIGGER storage_resources_partitioned_fixed ON storage_resources;
DROP FUNCTION enforce_storage_partitioned_fixed();
ALTER TABLE storage_resources DROP COLUMN partitioned;
