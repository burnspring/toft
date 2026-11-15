-- +goose Up
--
-- The dependency sweep gets a source it is allowed to write.
--
-- `app_events_source_check` has named seven subsystems since 00035, and the
-- dependency sweep has been writing an eighth -- `dependency` -- since it was
-- built. Every advisory it raised against a serving release was refused by the
-- constraint, and the refusal was invisible: events are recorded out of the
-- request path, so a failed insert is logged and swallowed rather than
-- surfaced. Nothing broke, nothing was slow, and the one feature that exists to
-- say "code you already shipped has been reclassified" said nothing at all.
--
-- **The source is added rather than the write reassigned**, which is the whole
-- decision here. The obvious cheap fix is to spell the sweep's events
-- `builder`, since that source already exists and the rows would then be
-- accepted -- but `source` is the actor label the admin feed renders as
-- `system:<source>`, and it would be a lie. Nothing was built. A finding on a
-- serving release is precisely the advisory that could *not* have been seen at
-- deploy time, and collapsing it into the builder's voice hides the only thing
-- that distinguishes it.
--
-- The literal is gone from the sweep in the same change: it now writes
-- database.EventSourceDependency, so the constants and this constraint are one
-- list rather than two that drifted. A ninth source is a migration above this
-- one, and it will fail loudly here rather than silently at 3am.
--
-- NOT VALID matches how 00035 wrote every one of these: existing rows are not
-- re-checked, new inserts are. There are no rows to re-check in any case --
-- nothing carrying this value was ever successfully written.
ALTER TABLE app_events DROP CONSTRAINT app_events_source_check;
ALTER TABLE app_events ADD CONSTRAINT app_events_source_check
    CHECK (source IN ('builder', 'autodeploy', 'health', 'cron', 'jobs', 'integrations', 'reachability', 'dependency')) NOT VALID;

-- +goose Down
--
-- Rows written by the sweep are deleted rather than left to fail the restored
-- constraint. They are machine observations that the next sweep re-raises
-- within six hours, so this loses nothing a person has to keep.
DELETE FROM app_events WHERE source = 'dependency';
ALTER TABLE app_events DROP CONSTRAINT app_events_source_check;
ALTER TABLE app_events ADD CONSTRAINT app_events_source_check
    CHECK (source IN ('builder', 'autodeploy', 'health', 'cron', 'jobs', 'integrations', 'reachability')) NOT VALID;
