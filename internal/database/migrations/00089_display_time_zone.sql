-- +goose Up
--
-- A person's choice of the zone the SPA shows exact times in (ADR §154). The
-- install's default is a settings row (display_time_zone) and needs no DDL.
ALTER TABLE users
    ADD COLUMN time_zone text NOT NULL DEFAULT '';
COMMENT ON COLUMN users.time_zone IS 'Display only. Empty follows the install''s display_time_zone; "browser" is this person''s browser zone whatever the install says; otherwise "UTC" or an IANA name. Nothing is stored or computed in it.';

-- +goose Down
ALTER TABLE users DROP COLUMN time_zone;
