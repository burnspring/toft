-- +goose Up
--
-- A drive is a declared place volumes live: a host root (SHARED_STORAGE_ROOTS)
-- or a cluster storage class. Its key — `root:<name>` or `class:<name>` — is
-- the operator's and is what resources record (storage_root, storage_class).
-- What people read is a label, and labels live here, because the declaration
-- is infrastructure and what an organization calls a drive is not.
--
-- A row is optional: an unlabelled drive reads as its name. Nothing outside the
-- admin pages reads a label; TOFT_STORAGE carries resource names and paths.
CREATE TABLE storage_drives (
    key         text PRIMARY KEY CHECK (key ~ '^(root|class):.+$'),
    label       text NOT NULL CHECK (length(label) BETWEEN 1 AND 60),
    description text NOT NULL DEFAULT '' CHECK (length(description) <= 2000),
    updated_by  text REFERENCES users(id) ON DELETE SET NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX storage_drives_label_lower ON storage_drives (lower(label));

COMMENT ON TABLE storage_drives IS
    'What an administrator calls each storage drive — a host root (root:<name>) or a cluster class (class:<name>). The key is the operator''s and is what resources record; a label reaches only the admin pages.';

-- +goose Down
DROP TABLE storage_drives;
