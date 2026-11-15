-- +goose Up
--
-- A storage resource has one name, and it is the slug.
--
-- The name used to be a free-form label ("Sales datasets") with a slug derived
-- from it ("sales-datasets"), and the slug was what apps passed to
-- toft.storage(...) and people typed in toft store — a second name nobody was
-- shown as the thing to type. From here the name is validated at create
-- (database.ValidateStorageName) and is the slug, and the CHECK makes the two
-- columns unable to drift apart again.
--
-- An existing label is not thrown away: it moves to the front of the
-- description, which is where a phrase with spaces in it now belongs. A
-- resource's slug is never touched, because its live bindings and its apps'
-- code are spelled with it — so a slug created before the format existed (one
-- starting with a digit, say) is kept as it is.
UPDATE storage_resources
   SET description = CASE WHEN description = '' THEN name
                          ELSE name || E'\n\n' || description END,
       name = slug
 WHERE name <> slug;

ALTER TABLE storage_resources
    ADD CONSTRAINT storage_resources_name_is_slug CHECK (name = slug);

-- +goose Down
ALTER TABLE storage_resources DROP CONSTRAINT storage_resources_name_is_slug;
