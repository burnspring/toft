-- +goose Up
--
-- Three storage levels, one meaning each: read < write < full.
--
-- `overwrite` (Can replace) is merged into `full` (Read & write). What it
-- withheld was the removal of a name, not the destruction of its contents --
-- an app at `overwrite` could replace any object with zero bytes -- so it read
-- as a safety boundary and was not one. `write` now means create-only on every
-- path that offers it.
--
-- On a volume, a *mapping* (and a delegate's ceiling, which caps mappings) at
-- `write` mounted read-write: it is renamed `full`, and the mount does not
-- change. A volume *grant* at `write` is create-only through link(2) on the
-- host data plane and If-None-Match on S3 Files, which is already what `write`
-- means, so grants on a volume do not move.
--
-- Order matters. The ranks change before any row does; the CHECKs are dropped
-- before the updates and re-added after; ceilings move before mappings,
-- because the authority trigger fires on each mapping UPDATE and would refuse a
-- promoted mapping under an unpromoted ceiling. One audit row per changed row,
-- written before the update so it can still read the old level.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_storage_manager_access() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    resource_kind TEXT;
BEGIN
    SELECT kind INTO resource_kind FROM storage_resources WHERE id = NEW.resource_id;
    IF resource_kind IS NULL THEN
        RAISE EXCEPTION 'storage_managers: resource % does not exist', NEW.resource_id;
    END IF;
    IF resource_kind = 'volume' AND NEW.max_access NOT IN ('read', 'full') THEN
        RAISE EXCEPTION 'storage_managers: resource % is a volume, which offers read and full — % is not one of them',
            NEW.resource_id, NEW.max_access;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_storage_mapping_authority() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_subject_type TEXT;
    v_subject_id   TEXT;
    v_ceiling      TEXT;
    v_rank_of      INT;
    v_rank_max     INT;
BEGIN
    IF NEW.authority = 'admin' THEN
        RETURN NEW;
    END IF;
    v_subject_type := split_part(NEW.authority, ':', 1);
    v_subject_id   := substr(NEW.authority, length(v_subject_type) + 2);
    IF v_subject_type NOT IN ('user', 'group') OR v_subject_id = '' THEN
        RAISE EXCEPTION 'storage_mappings: authority % is not admin, user:<id> or group:<id>', NEW.authority;
    END IF;

    SELECT m.max_access INTO v_ceiling FROM storage_managers m
     WHERE m.resource_id = NEW.resource_id
       AND m.subject_type = v_subject_type
       AND m.subject_id = v_subject_id;
    IF v_ceiling IS NULL THEN
        RAISE EXCEPTION 'storage_mappings: % does not manage resource %', NEW.authority, NEW.resource_id;
    END IF;

    v_rank_of  := CASE NEW.access WHEN 'read' THEN 1 WHEN 'write' THEN 2 WHEN 'full' THEN 3 ELSE 0 END;
    v_rank_max := CASE v_ceiling  WHEN 'read' THEN 1 WHEN 'write' THEN 2 WHEN 'full' THEN 3 ELSE 0 END;
    IF v_rank_of > v_rank_max THEN
        RAISE EXCEPTION 'storage_mappings: % may attach resource % at % at most, not %',
            NEW.authority, NEW.resource_id, v_ceiling, NEW.access;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

ALTER TABLE storage_managers DROP CONSTRAINT storage_managers_access_check;
ALTER TABLE storage_mappings DROP CONSTRAINT storage_mappings_access_check;
ALTER TABLE store_grants DROP CONSTRAINT store_grants_access_check;

-- Ceilings: overwrite -> full everywhere; write -> full on a volume.
INSERT INTO audit_log (id, actor_type, action, target_type, target_id, before, after)
SELECT gen_random_uuid()::text, 'system', 'storage.level_promoted', 'storage_manager', m.id,
       jsonb_build_object('access', m.max_access), jsonb_build_object('access', 'full')
  FROM storage_managers m JOIN storage_resources r ON r.id = m.resource_id
 WHERE m.max_access = 'overwrite' OR (r.kind = 'volume' AND m.max_access = 'write');

UPDATE storage_managers m SET max_access = 'full'
  FROM storage_resources r
 WHERE r.id = m.resource_id
   AND (m.max_access = 'overwrite' OR (r.kind = 'volume' AND m.max_access = 'write'));

-- Mappings: the same two rules, each row part of its app's history.
INSERT INTO audit_log (id, actor_type, action, target_type, target_id, before, after, app_id)
SELECT gen_random_uuid()::text, 'system', 'storage.level_promoted', 'storage_mapping', m.id,
       jsonb_build_object('access', m.access), jsonb_build_object('access', 'full'), m.app_id
  FROM storage_mappings m JOIN storage_resources r ON r.id = m.resource_id
 WHERE m.access = 'overwrite' OR (r.kind = 'volume' AND m.access = 'write');

UPDATE storage_mappings m SET access = 'full'
  FROM storage_resources r
 WHERE r.id = m.resource_id
   AND (m.access = 'overwrite' OR (r.kind = 'volume' AND m.access = 'write'));

-- Grants: overwrite -> full only. A grant's `write` is create-only on every kind.
INSERT INTO audit_log (id, actor_type, action, target_type, target_id, before, after)
SELECT gen_random_uuid()::text, 'system', 'storage.level_promoted', 'store_grant', g.id,
       jsonb_build_object('access', g.access), jsonb_build_object('access', 'full')
  FROM store_grants g
 WHERE g.access = 'overwrite';

UPDATE store_grants SET access = 'full' WHERE access = 'overwrite';

ALTER TABLE storage_managers ADD CONSTRAINT storage_managers_access_check
    CHECK (max_access = ANY (ARRAY['read'::text, 'write'::text, 'full'::text]));
ALTER TABLE storage_mappings ADD CONSTRAINT storage_mappings_access_check
    CHECK (access = ANY (ARRAY['read'::text, 'write'::text, 'full'::text]));
ALTER TABLE store_grants ADD CONSTRAINT store_grants_access_check
    CHECK (access = ANY (ARRAY['read'::text, 'write'::text, 'full'::text]));

-- +goose Down
-- Written to be read; nothing runs a migration backwards. The widening from
-- overwrite cannot be told apart from a full that was always full except by the
-- storage.level_promoted rows, so a Down would restore the four tokens and
-- leave the data as it is.
ALTER TABLE store_grants DROP CONSTRAINT store_grants_access_check;
ALTER TABLE storage_mappings DROP CONSTRAINT storage_mappings_access_check;
ALTER TABLE storage_managers DROP CONSTRAINT storage_managers_access_check;
ALTER TABLE storage_managers ADD CONSTRAINT storage_managers_access_check
    CHECK (max_access = ANY (ARRAY['read'::text, 'write'::text, 'overwrite'::text, 'full'::text]));
ALTER TABLE storage_mappings ADD CONSTRAINT storage_mappings_access_check
    CHECK (access = ANY (ARRAY['read'::text, 'write'::text, 'overwrite'::text, 'full'::text]));
ALTER TABLE store_grants ADD CONSTRAINT store_grants_access_check
    CHECK (access = ANY (ARRAY['read'::text, 'write'::text, 'overwrite'::text, 'full'::text]));
