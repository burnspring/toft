-- +goose Up
--
-- The system group behind each host-root storage resource, with its number.
--
-- A gate is a group the platform creates on the host to gate one directory under
-- an operator's storage root. The host's /etc/group is not where its identity
-- lives: on the ECS task and the compose container it dies with the container,
-- and the numbers recorded on the file system do not. A group recreated by
-- whichever groupadd ran first would come back at a different gid, and a
-- directory still carrying the old number would belong to whatever gate was
-- created into it. So the number is allocated here, once, and every start
-- recreates the group at it.
--
-- Keyed by the group's name rather than the resource's id, and deliberately
-- without a foreign key: deleting a resource keeps its directory (the reclaim
-- policy is Retain), so it keeps the number the files there carry. retired_at
-- marks a gate no resource holds; a resource created later onto the same
-- directory — the same slug — takes the gate back rather than a fresh number.
CREATE TABLE storage_gates (
    group_name  text NOT NULL PRIMARY KEY,
    resource_id text NOT NULL,
    folder      text NOT NULL DEFAULT '',
    gid         integer NOT NULL UNIQUE,
    created_at  timestamp with time zone DEFAULT now() NOT NULL,
    retired_at  timestamp with time zone
);
COMMENT ON TABLE storage_gates IS 'The host system group gating one storage directory under a shared storage root, and its gid. The database is the source of the number; the host''s /etc/group is a cache of it, recreated at every start. Never deleted: a retired gate keeps its number because the files on the disk still carry it.';
COMMENT ON COLUMN storage_gates.folder IS 'Empty is the whole resource''s gate.';
COMMENT ON COLUMN storage_gates.retired_at IS 'Set when the resource holding the gate is deleted. The number stays allocated.';

CREATE UNIQUE INDEX storage_gates_live_key ON storage_gates (resource_id, folder) WHERE retired_at IS NULL;

-- Every host-root resource that exists gets its gate, numbered from the default
-- range in creation order.
INSERT INTO storage_gates (group_name, resource_id, folder, gid)
SELECT 'toft-store-' || slug, id, '', 61999 + row_number() OVER (ORDER BY created_at, id)
  FROM storage_resources
 WHERE storage_root <> '';

-- +goose Down
DROP TABLE storage_gates;
