-- +goose Up
--
-- Places an administrator pasted onto a connection and the far side verified:
-- for the Teams bot, the teams and chats the Toft app was installed into. The
-- bot has no way to be told where it lives without an inbound endpoint, and
-- finding out by looking needs tenant-wide Graph reads, so the place list is
-- pasted (ADR §72). Channels are not stored: they are
-- asked of the far side when the destinations menu opens.
CREATE TABLE integration_places (
    id              text PRIMARY KEY,
    connection_id   text NOT NULL REFERENCES integration_connections(id) ON DELETE CASCADE,
    place_kind      text NOT NULL CHECK (place_kind IN ('team', 'chat')),
    conversation_id text NOT NULL,
    name            text NOT NULL,
    added_by        text REFERENCES users(id) ON DELETE SET NULL,
    added_at        timestamp with time zone NOT NULL DEFAULT now(),
    UNIQUE (connection_id, conversation_id)
);
COMMENT ON TABLE integration_places IS
    'A place on the far side an administrator pasted onto a connection and Toft verified the connection can reach — a Teams team or chat the bot was installed in. Deleted with its connection; refused while an attachment or the connection''s default destination points into it.';
COMMENT ON COLUMN integration_places.conversation_id IS
    'The far side''s own id for the place (a Teams thread id, 19:…@thread.tacv2 or 19:…@thread.v2). Not a Toft reference.';
COMMENT ON COLUMN integration_places.name IS
    'The team''s name as the far side reported it at paste time, or the name an administrator gave a chat, which the far side does not report.';

-- +goose Down
DROP TABLE integration_places;
