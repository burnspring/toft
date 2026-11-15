-- +goose Up
--
-- An administrator's own words about a connection: what it is for and who on
-- the far side answers for it. Optional and editable, unlike the scope. Teams
-- is why it exists — a Workflows URL names neither its channel nor the person
-- who owns the flow, and that person is the boundary (ADR §72).
ALTER TABLE integration_connections ADD COLUMN description TEXT NOT NULL DEFAULT '';
COMMENT ON COLUMN integration_connections.description IS
    'Free text an administrator wrote about this connection — what it is for and who owns the far side. Optional, editable, at most 1000 characters.';

-- +goose Down
ALTER TABLE integration_connections DROP COLUMN description;
