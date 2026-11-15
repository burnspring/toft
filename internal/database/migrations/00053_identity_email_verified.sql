-- +goose Up
--
-- Whether the provider vouched for the address this identity last signed in
-- with. Verified-email linking attaches a new identity to an existing user, so
-- the existing side has to have proved the address too: otherwise a person who
-- asserts a colleague's address at a provider that does not verify it becomes
-- the account the colleague's first verified sign-in is linked to.
--
-- Rows that exist today keep counting as verified (the default while the column
-- is added) and are corrected by the identity's next sign-in; new rows are
-- unverified until a provider says otherwise.
ALTER TABLE user_identities ADD COLUMN email_verified boolean NOT NULL DEFAULT true;
ALTER TABLE user_identities ALTER COLUMN email_verified SET DEFAULT false;
COMMENT ON COLUMN user_identities.email_verified IS 'The provider said, at the last sign-in, that this identity''s email is verified. A user is a verified-email linking target only through an identity that is.';

-- +goose Down
ALTER TABLE user_identities DROP COLUMN email_verified;
