-- +goose Up
--
-- A host run's bootstrap credential is accepted once (ADR §106). A pod proves
-- itself with a token the cluster issued and bound to the pod; a process on the
-- platform's own machine has nothing of the kind, so the platform writes a
-- short-lived credential into the run's directory, and this column is what makes
-- it single-use: anything else on the machine that read the file after the
-- runner did opens nothing.
ALTER TABLE job_runs ADD COLUMN bootstrapped_at timestamptz;

COMMENT ON COLUMN public.job_runs.bootstrapped_at IS 'When a host run''s runner presented its bootstrap credential — set once, by an UPDATE that matches only a NULL, so a second presentation is refused. NULL for every pod run, which proves itself through the cluster instead.';

-- +goose Down
ALTER TABLE job_runs DROP COLUMN bootstrapped_at;
