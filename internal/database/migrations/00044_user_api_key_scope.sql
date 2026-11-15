-- +goose Up
--
-- A personal key gets a blast radius.
--
-- **What this column decides is not "which apps" but "which routes", and the
-- difference is the whole design.** A personal key authenticates its owner and
-- carries no claims of its own (ADR §23); role, groups and grants are resolved
-- live on every request. Nothing here touches that. A `read` key still reaches
-- exactly the objects its owner reaches — it simply cannot use a method that
-- changes one. That is why `auth.Authorize` never reads this and the route
-- guard in internal/api is the only thing that does, exactly as
-- deploy_keys.scope works one class over.
--
-- **Two values and not three.** The write-shaped middle of the range is already
-- a credential class of its own: ADR §51 built the deploy key for the pipeline
-- case and argued that a pipeline authenticating as a person is the failure
-- that class exists to prevent. A `deploy` scope here would answer that
-- question a second time and worse, with a word that made the wrong credential
-- look sanctioned.
--
-- **DEFAULT 'full' answers "what were the keys that already existed", and the
-- answer has to be every one of them.** No script breaks on upgrade, nothing
-- needs re-minting, and ClaimDeviceCode — which names its columns and therefore
-- never learns this feature exists — mints the right thing without changing.
-- The code's fallback for an unrecognised value answers a different question
-- and resolves the other way, to `read`: an allow-list that fails closed must
-- not let a word nobody has implemented become the widest credential on the
-- install. Both defaults are correct and they point in opposite directions,
-- which is worth the sentence.
--
-- **The scope is chosen at mint and never edited.** Nothing in this package
-- writes the column after the insert and no route carries it on a PATCH. A
-- scope that can be widened in place is a label rather than a boundary; moving
-- a key between scopes means minting the other and revoking this one. Carried
-- verbatim from the deploy class, because two credential classes disagreeing
-- about what a scope *is* would be worse than either rule.
ALTER TABLE user_api_keys
    ADD COLUMN scope text NOT NULL DEFAULT 'full'
    CONSTRAINT user_api_keys_scope_check CHECK (scope IN ('read', 'full'));

COMMENT ON COLUMN public.user_api_keys.scope IS 'read or full: which routes this key may call, never which objects it may reach. A read key is refused every non-GET on the allow-list and reaches every GET on it; a full key is the allow-list alone, which is what every key minted before this column had. Chosen at mint and never edited, so nothing writes it after the insert. DEFAULT full is what made the upgrade silent; an unrecognised value resolves to read in Go, because this is an allow-list that fails closed.';

-- +goose Down
--
-- Every read-scoped key on the install silently becomes a full one. There is no
-- narrower place to put them: the column is the only record that somebody chose
-- a bound, so dropping it widens those credentials rather than revoking them,
-- and nothing observable says so afterwards. An install rolling back past this
-- should revoke its read-scoped keys first.
ALTER TABLE user_api_keys DROP COLUMN scope;
