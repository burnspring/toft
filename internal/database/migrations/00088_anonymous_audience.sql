-- +goose Up
--
-- The anonymous audience (ADR §150). page_loads is a second unit of the counter
-- the install already keeps — top-level document navigations, the figure a
-- person reading an app produces once per page however chatty its framework is.
ALTER TABLE app_usage_daily
    ADD COLUMN page_loads bigint NOT NULL DEFAULT 0;
COMMENT ON COLUMN app_usage_daily.page_loads IS 'How many of requests were top-level document navigations (Sec-Fetch-Dest: document, or a GET preferring text/html with no Sec-Fetch-Dest). Framed loads never count: the SPA''s Preview tab is an iframe.';

-- One HyperLogLog sketch per app per UTC day of the anonymous browsers whose
-- visitor cookie came back. Unenumerable by construction: it estimates how
-- many, and cannot say which. Merged under a row lock by every replica's flush.
CREATE TABLE app_audience_daily (
    app_id text  NOT NULL,
    day    date  NOT NULL,
    sketch bytea NOT NULL,
    PRIMARY KEY (app_id, day)
);
COMMENT ON TABLE app_audience_daily IS 'Distinct anonymous browsers per app per UTC day, as a HyperLogLog sketch (precision 14, axiomhq binary encoding). Written only while audience_measurement_enabled is on; no foreign key to apps, for app_usage_daily''s reason. Swept by the access record''s retention.';
COMMENT ON COLUMN app_audience_daily.sketch IS 'axiomhq/hyperloglog MarshalBinary output. Names nobody: an estimate of how many visitor ids were added, never the ids.';

-- +goose Down
DROP TABLE app_audience_daily;
ALTER TABLE app_usage_daily DROP COLUMN page_loads;
