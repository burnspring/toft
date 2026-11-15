-- +goose Up
--
-- The object-role ladder gains a rung below every way of reading.
--
-- `list` means the app appears in your listings and you may not open it. It is
-- the state an internal platform has always had and could never express: an app
-- exists, you have heard of it, and you do not have access yet. Until now the
-- only two representations were invisible and fully readable, so every real ask
-- for access happened in chat and left a grant row whose reason nobody wrote
-- down.
--
-- **The word is stored; the ordering is not.** internal/auth ranks the ladder
-- in Go and ranks `list` strictly between `none` and `anonymous`, which is what
-- makes it unreadable — there is no branch anywhere that says so, only one
-- integer below the read threshold. This constraint's job is narrower: to admit
-- the fourth word, and to keep admitting exactly four.
--
-- The one thing that had to happen in the same change as this line is in the
-- SQL that reads the column. Every grant clause in appsVisibilityPredicate used
-- to mean read access whatever the role was, so the moment this constraint
-- accepted `list`, a list grant would have conferred exactly the access it
-- exists to withhold. Those clauses subtract the rung now, and listable adds it
-- back as a disjunct of its own.
--
-- NOT VALID, as 00035 and 00038 wrote every one of these: existing rows are not
-- re-checked and new inserts are. Widening a check can strand no existing row
-- in any case -- every value the old constraint admitted, this one admits.
ALTER TABLE object_grants DROP CONSTRAINT object_grants_role_check;
ALTER TABLE object_grants ADD CONSTRAINT object_grants_role_check
    CHECK (role IN ('owner', 'editor', 'viewer', 'list')) NOT VALID;

COMMENT ON COLUMN object_grants.role IS 'A rung of the object-role ladder, ordered owner > editor > viewer > list. The word is stored and the ordering is not: internal/auth ranks it, and ranks list strictly below every way of reading, which is what makes a list grant put the app in your listings without letting you open it.';

-- +goose Down
--
-- The rung's rows are deleted rather than left to fail the restored constraint.
-- A grant is a row until somebody removes it, so this is the one destructive
-- down migration in the set that removes something a person wrote on purpose --
-- but the alternative is a database that will not accept a write to any grant
-- row, and there is no third option: the column has no way to say "this used to
-- be a list grant".
--
-- Nothing is lost that the owner cannot see is gone. Going down from here means
-- going back to a build with no fourth rung, where the grant would have had no
-- effect on any listing anyway.
DELETE FROM object_grants WHERE role = 'list';
ALTER TABLE object_grants DROP CONSTRAINT object_grants_role_check;
ALTER TABLE object_grants ADD CONSTRAINT object_grants_role_check
    CHECK (role IN ('owner', 'editor', 'viewer')) NOT VALID;
