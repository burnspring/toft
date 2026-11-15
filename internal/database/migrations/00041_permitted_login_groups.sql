-- +goose Up
--
-- Who may authenticate through a provider, asked of the directory rather than
-- of the address.
--
-- Until now the only gate on a sign-in was `allowed_domains`, and on the
-- provider this product is most often pointed at -- one corporate Entra or Okta
-- tenant -- every employee shares the domain, so that gate is the empty gate.
-- The install's real answer to "only the data team may sign in" was to turn
-- auto-provisioning off and approve each person by hand, which is a queue and
-- not a policy: it scales with headcount and it says nothing about the person
-- whose role changed last week.
--
-- Three columns, all `text NOT NULL DEFAULT ''`, so this is a no-op for every
-- existing row and needs no backfill.
--
-- **permitted_login_groups holds claim values, not group ids.** It is compared
-- against what the provider asserted, before any mapping is consulted. It is an
-- *authentication* gate and must not depend on an authorization table; a value
-- with no group_claim_mappings row still has to be usable, or an administrator
-- has to create a group they do not want in order to gate on it; and the gate's
-- failure mode stays independent of the reconciler's. Empty means every group
-- is permitted, so an upgrade writes nothing and a provider created before this
-- existed reads the same as one nobody has configured -- groups_claim's rule,
-- for groups_claim's reason.
--
-- **An absent claim refuses.** Not stored here, but this is where it is
-- written down: when the list is non-empty and the sign-in asserts no group
-- claim at all, the sign-in is refused. Admitting on absence would make the
-- column decorative -- a dropped scope, an Entra overage, an Okta app
-- configured to omit the claim, and the install is silently open to everybody,
-- which is the opposite of what the administrator who filled this in asked for.
-- The cost is real and is stated rather than softened: a provider that stops
-- asserting groups locks out every one of its users at once. What makes that
-- survivable is the invariant that there is always exactly one way in that
-- needs no provider -- the built-in local administrator is reachable when every
-- provider is refusing, which is precisely the situation it exists for.
--
-- **allowed_emails is a union with allowed_domains, never an intersection.** A
-- person is permitted if they match either list. Both empty is unchanged
-- behaviour. It is here because "the domain, plus two contractors" is the
-- request that arrives immediately after a domain gate is switched on.
--
-- **groups_separator is a bug fix wearing a column.** A claim delivered as one
-- delimited string has always been read as a single group whose name contains
-- the delimiter, which matches nothing. Harmless while the only consumer of a
-- claim was membership, which grants; not harmless the moment a consumer
-- refuses. Empty preserves today's behaviour exactly.
ALTER TABLE auth_providers ADD COLUMN permitted_login_groups text NOT NULL DEFAULT '';
ALTER TABLE auth_providers ADD COLUMN allowed_emails text NOT NULL DEFAULT '';
ALTER TABLE auth_providers ADD COLUMN groups_separator text NOT NULL DEFAULT '';

COMMENT ON COLUMN auth_providers.permitted_login_groups IS 'Claim values -- not group ids -- of which a person must hold at least one to sign in through this provider. Empty means every group is permitted. Compared against the asserted claim before any mapping is consulted, because an authentication gate must not depend on an authorization table. A non-empty list refuses a sign-in that asserts no claim at all, and the local administrator is what makes failing closed survivable.';

COMMENT ON COLUMN auth_providers.allowed_emails IS 'Individual addresses permitted to sign in, in union with allowed_domains and never in intersection: a person matching either list is permitted. Both empty means any address this provider authenticates.';

COMMENT ON COLUMN auth_providers.groups_separator IS 'The delimiter a group claim delivered as one string is split on. Empty means no split, which is what every provider read before this existed: a bare string is a list of one.';

-- +goose Down
--
-- The columns go, and what is lost is the policy itself: down from here is a
-- build whose only gate is the email domain, so an install that had narrowed
-- sign-in to one directory group would find it open to the whole tenant again.
-- That is a disclosure rather than a breakage, it has nowhere to be kept, and
-- it is said plainly rather than softened -- an operator going back should
-- expect to re-narrow on the way forward.
ALTER TABLE auth_providers DROP COLUMN permitted_login_groups;
ALTER TABLE auth_providers DROP COLUMN allowed_emails;
ALTER TABLE auth_providers DROP COLUMN groups_separator;
