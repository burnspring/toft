-- +goose Up
--
-- A resource under a host root has a data plane: the platform carries its
-- bytes over self-signed URLs, because a disk mounted on the platform's own
-- host has no other server that could enforce a signature.
--
-- The grant floor used to read "names a bucket". It now reads "names a bucket,
-- or lives under a host root" — the sentence StorageResource.HasDataPlane says
-- in Go and the reach query says in SQL. An EFS resource on a cluster still
-- takes no grant: the platform's pod does not mount it, so there is nobody to
-- carry the bytes.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_grant_has_bucket() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    resource_kind    TEXT;
    resource_bucket  TEXT;
    resource_backing TEXT;
    resource_root    TEXT;
BEGIN
    SELECT kind, bucket, backing, storage_root
      INTO resource_kind, resource_bucket, resource_backing, resource_root
      FROM storage_resources WHERE id = NEW.resource_id;
    IF resource_kind IS NULL THEN
        RAISE EXCEPTION 'store_grants: resource % does not exist', NEW.resource_id;
    END IF;
    IF COALESCE(resource_bucket, '') = ''
       AND NOT (resource_backing = 'host' AND COALESCE(resource_root, '') <> '') THEN
        RAISE EXCEPTION 'store_grants: resource % names no bucket and no host root, so there is nothing to sign for — only a resource with objects or a host directory behind it takes grants', NEW.resource_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_grant_has_bucket() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    resource_kind   TEXT;
    resource_bucket TEXT;
BEGIN
    SELECT kind, bucket INTO resource_kind, resource_bucket
      FROM storage_resources WHERE id = NEW.resource_id;
    IF resource_kind IS NULL THEN
        RAISE EXCEPTION 'store_grants: resource % does not exist', NEW.resource_id;
    END IF;
    IF COALESCE(resource_bucket, '') = '' THEN
        RAISE EXCEPTION 'store_grants: resource % names no bucket, so there is nothing to sign for — only a resource with objects behind it takes grants', NEW.resource_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
