-- +goose Up
--
-- RStudio's "Add account" dialog signs in to a Posit Connect server with a
-- token flow rather than a key: rsconnect makes an RSA key pair, registers the
-- public half, sends a person to a browser to approve it, and from then on
-- signs every request with the private half (ADR §102).
--
-- The approval is `lp login`'s, unchanged — a device code, the /cli page, a
-- browser session deciding — so cli_device_codes learns which client asked and
-- from where, which is what the approval screen has to say to be worth reading.
-- Both default to what every existing row was: an `lp` request from an address
-- nobody recorded.
ALTER TABLE cli_device_codes ADD COLUMN client text NOT NULL DEFAULT 'lp';
ALTER TABLE cli_device_codes ADD COLUMN requested_from text NOT NULL DEFAULT '';

COMMENT ON COLUMN public.cli_device_codes.client IS 'Which program asked: lp (lp login) or rsconnect (RStudio''s Add account dialog). Display only — it decides the approval screen''s wording and nothing about what is minted.';
COMMENT ON COLUMN public.cli_device_codes.requested_from IS 'The address the request arrived from, shown on the approval screen so a person can tell their own machine from somebody else''s. Empty on rows written before the column.';

-- What a signed request is checked against. The row binds a public key to the
-- personal key the approval created, so the credential that is authenticated,
-- disabled, expired, scoped, counted and revoked is an ordinary user_api_keys
-- row — this is a second proof of possession for that class, not a fourth
-- class. Until approved, device_code_hash names the pending request and
-- user_api_key_id is empty; the claim swaps one for the other.
CREATE TABLE connect_tokens (
    token_id         text PRIMARY KEY,
    public_key       bytea NOT NULL,
    device_code_hash text,
    user_api_key_id  text REFERENCES user_api_keys(id) ON DELETE CASCADE,
    created_at       timestamp with time zone NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX connect_tokens_user_api_key_id ON connect_tokens (user_api_key_id);

COMMENT ON TABLE public.connect_tokens IS 'rsconnect''s request-signing tokens (ADR §102): a public key, and the personal API key whose identity and lifecycle it carries. No secret rests here — the private key never leaves the publisher''s machine, and the personal key''s secret was discarded when it was created. Revoking the key deletes the row.';
COMMENT ON COLUMN public.connect_tokens.public_key IS 'The DER-encoded RSA public key rsconnect registered; every signed request is verified against it.';
COMMENT ON COLUMN public.connect_tokens.device_code_hash IS 'While pending, the cli_device_codes row a person approves. Empty once claimed.';

-- +goose Down
DROP TABLE connect_tokens;
ALTER TABLE cli_device_codes DROP COLUMN requested_from;
ALTER TABLE cli_device_codes DROP COLUMN client;
