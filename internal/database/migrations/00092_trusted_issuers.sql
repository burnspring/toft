-- +goose Up
--
-- A workbench session's identity, exchanged for a short-lived key (ADR §157).
--
-- trusted_issuers is what an administrator names: an OIDC issuer and the
-- audience its tokens must carry, and how the person a token names is found.
-- Nothing is trusted by default, so an install with no row here exchanges
-- nothing.
--
-- exchanged_keys is the credential an exchange mints: `toftx_`, a person's,
-- deploy-scoped and expiring on its own. user_id cascades for user_api_keys'
-- reason — the key *is* the person. issuer_id cascades too: deleting an issuer
-- is withdrawing its word, and a key it vouched for must not outlive that.
CREATE TABLE trusted_issuers (
    id                   text PRIMARY KEY,
    display_name         text NOT NULL,
    issuer               text NOT NULL,
    audience             text NOT NULL,
    subject_claim        text NOT NULL DEFAULT 'sub'
        CONSTRAINT trusted_issuers_subject_claim_check CHECK (subject_claim IN ('sub', 'email')),
    provider_id          text NOT NULL DEFAULT '',
    key_lifetime_minutes integer NOT NULL DEFAULT 480
        CONSTRAINT trusted_issuers_key_lifetime_check CHECK (key_lifetime_minutes BETWEEN 60 AND 1440),
    enabled              boolean NOT NULL DEFAULT true,
    created_by           text REFERENCES users(id) ON DELETE SET NULL,
    created_at           timestamp with time zone NOT NULL DEFAULT now(),
    updated_at           timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT trusted_issuers_issuer_audience_key UNIQUE (issuer, audience)
);
COMMENT ON TABLE trusted_issuers IS 'Issuers an administrator trusts to vouch for a person: an identity token from one, carrying its audience, is exchanged for a short-lived deploy-scoped key (toftx_) for the account it names. Nothing is trusted by default.';
COMMENT ON COLUMN trusted_issuers.subject_claim IS 'How the token names its person: sub, looked up as a user_identities subject on provider_id; or email, matched against exactly one account and only when email_verified is true.';
COMMENT ON COLUMN trusted_issuers.provider_id IS 'The sign-in provider whose identities a sub names. No foreign key, for user_identities.provider_id''s reason: a vanished provider leaves the issuer inert and visible.';

CREATE TABLE exchanged_keys (
    id            text PRIMARY KEY,
    user_id       text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issuer_id     text NOT NULL REFERENCES trusted_issuers(id) ON DELETE CASCADE,
    key_hash      text NOT NULL CONSTRAINT exchanged_keys_key_hash_key UNIQUE,
    key_prefix    text NOT NULL,
    token_subject text NOT NULL DEFAULT '',
    created_at    timestamp with time zone NOT NULL DEFAULT now(),
    expires_at    timestamp with time zone NOT NULL,
    revoked_at    timestamp with time zone,
    last_used_at  timestamp with time zone,
    last_used_ip  text NOT NULL DEFAULT ''
);
COMMENT ON TABLE exchanged_keys IS 'Short-lived deploy-scoped keys (toftx_) minted by exchanging a trusted issuer''s identity token. A person''s, like user_api_keys, but never long-lived and never wider than the deploy surface. Rows are swept a week after they expire; audit_log.api_key_id keeps naming them.';
COMMENT ON COLUMN exchanged_keys.key_hash IS 'A SHA-256 of the secret — see user_api_keys.key_hash.';
COMMENT ON COLUMN exchanged_keys.token_subject IS 'The claim value the issuer named the person by, kept so a review can join a key to the issuer''s own log.';
CREATE INDEX exchanged_keys_user_id_idx ON exchanged_keys (user_id);
CREATE INDEX exchanged_keys_expires_at_idx ON exchanged_keys (expires_at);

-- +goose Down
DROP TABLE exchanged_keys;
DROP TABLE trusted_issuers;
