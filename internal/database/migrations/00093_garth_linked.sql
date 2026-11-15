-- +goose Up
--
-- An app deployed from a Garth session (ADR §158). Set by the deploy routes
-- when the request arrives through Garth's proxy, cleared by "Unlink from
-- Garth" on the app page; "Open in Garth" is offered only while it is set.
ALTER TABLE apps
    ADD COLUMN garth_linked_at timestamptz;
COMMENT ON COLUMN apps.garth_linked_at IS 'When a deploy from a Garth session first linked this app; NULL when it was never deployed from Garth or was unlinked. Open in Garth is offered only while it is set.';

-- +goose Down
ALTER TABLE apps DROP COLUMN garth_linked_at;
