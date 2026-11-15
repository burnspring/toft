-- +goose Up
--
-- Whether apps attached to this connection may be handed the connection's own
-- short-lived access token (POST /api/v1/app/tokens/{kind}). Off for every
-- existing connection: attaching meant "upload into one folder", and a token
-- reaches whatever the connection's grant reaches.
ALTER TABLE integration_connections ADD COLUMN issue_tokens BOOLEAN NOT NULL DEFAULT false;
COMMENT ON COLUMN integration_connections.issue_tokens IS
    'Apps attached to this connection may get its short-lived access token (file kinds only). Off by default; a token reaches the connection''s whole grant, not the folder chosen per app.';

-- +goose Down
ALTER TABLE integration_connections DROP COLUMN issue_tokens;
