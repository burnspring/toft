-- +goose Up
--
-- What one person has been opening (Admin → People → a person's page) reads
-- app_usage_daily by user_id, which its primary key (app_id, day, user_id)
-- cannot serve: without this the read is a scan of every app's counters.
CREATE INDEX app_usage_daily_user_day_idx ON app_usage_daily (user_id, day);

-- +goose Down
DROP INDEX app_usage_daily_user_day_idx;
