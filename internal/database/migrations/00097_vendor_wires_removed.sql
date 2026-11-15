-- +goose Up
--
-- The license check-in and the reachability probe are deleted from the
-- product, not kept dormant (ADR §32, *Toft is free*). Their state goes with
-- them: the one table each owned, the settings rows they wrote, and the app
-- events the probe raised. There are no installs to preserve it for.
--
-- `install.id` stays. It names the install on the compliance report and in a
-- support conversation, and it was never the license's.
DROP TABLE IF EXISTS app_reachability;
DROP TABLE IF EXISTS license_usage_samples;
DELETE FROM settings WHERE key LIKE 'license.%' OR key LIKE 'reachability.%';
DELETE FROM app_events WHERE source = 'reachability' OR kind = 'public_access_suspended';
ALTER TABLE app_events DROP CONSTRAINT app_events_source_check;
ALTER TABLE app_events ADD CONSTRAINT app_events_source_check
    CHECK (source IN ('builder', 'autodeploy', 'health', 'cron', 'jobs', 'integrations', 'dependency')) NOT VALID;

-- +goose Down
--
-- Nothing runs this. The tables are not recreated: the code that read them is
-- gone, and a schema holding them would be state with no reader.
ALTER TABLE app_events DROP CONSTRAINT app_events_source_check;
ALTER TABLE app_events ADD CONSTRAINT app_events_source_check
    CHECK (source IN ('builder', 'autodeploy', 'health', 'cron', 'jobs', 'integrations', 'reachability', 'dependency')) NOT VALID;
