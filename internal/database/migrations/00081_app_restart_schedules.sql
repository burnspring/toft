-- +goose Up
--
-- An app's scheduled restart, declared as `[restart]` in its toft.toml.
--
-- At most one per app, and a table rather than a column on apps for the
-- re-render's reason: the shared claim selects app_id by name and apps has
-- none, so a column would fork the claim. The seven columns the claim reads,
-- plus the last firing's outcome — three columns rather than a history table,
-- because the restarts that happened are audit rows and a history of declines
-- would be the non-decisions the audit log keeps out.
CREATE TABLE app_restart_schedules (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name text NOT NULL DEFAULT 'Restart',
    schedule text NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    next_run_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    updated_at timestamp with time zone NOT NULL DEFAULT now(),
    last_fired_at timestamp with time zone,
    last_outcome text NOT NULL DEFAULT '' CHECK (last_outcome IN ('', 'restarted', 'skipped', 'failed')),
    last_reason text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX app_restart_schedules_app_idx ON app_restart_schedules (app_id);
CREATE INDEX app_restart_schedules_due_idx ON app_restart_schedules (next_run_at) WHERE enabled;

COMMENT ON TABLE app_restart_schedules IS 'At most one restart schedule per app, written only by a deploy reconciling toft.toml [restart]. A firing restarts a running app and nothing else, and never resets the supervisor or the health sweep.';
COMMENT ON COLUMN app_restart_schedules.schedule IS 'The cron expression with its zone joined in (CRON_TZ=…), so the shared claim sees one string.';
COMMENT ON COLUMN app_restart_schedules.enabled IS 'False is a pause from the Settings tab, which lasts until the next deploy.';
COMMENT ON COLUMN app_restart_schedules.last_outcome IS 'The last firing: restarted, skipped (declined, last_reason says why) or failed (last_reason is the start error). Empty before the first. Kept across deploys.';
COMMENT ON COLUMN app_restart_schedules.last_reason IS 'Why the last firing was skipped — sleeping, stopped, crashed, deploy_pending, locked, no_process — or the failed start''s sentence.';

-- The re-render's schedule is now the file's too, so the person who deletes it
-- is the one who removed `[render]` in a commit, and no UI is there to warn.
COMMENT ON COLUMN app_render_schedule_recipients.schedule_id IS 'The schedule this list belongs to. Cascades on delete, deliberately: a list kept past its schedule would reappear if one were recreated, sending mail to people chosen for a different report. The row survives every change to the schedule and its zone; only removing [render] from toft.toml deletes it, and that deploy announces it (schedule_removed).';
COMMENT ON TABLE app_render_schedules IS 'At most one render schedule per app, written only by a deploy reconciling toft.toml [render]; enabled is the one field anything else writes. What it fires is a deploy. A table rather than a column on apps because the shared claim selects app_id by name and apps has none.';

-- +goose Down
COMMENT ON TABLE app_render_schedules IS 'At most one render schedule per app, and what it fires is a deploy. A table rather than a column on apps because the shared claim selects app_id by name and apps has none.';
COMMENT ON COLUMN app_render_schedule_recipients.schedule_id IS 'The schedule this list belongs to. Cascades on delete, deliberately, and the UI says so before it deletes: a list kept past its schedule would reappear if one were recreated, which is a worse surprise than losing an afternoon of picking people.';
DROP TABLE app_restart_schedules;
