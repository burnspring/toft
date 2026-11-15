-- +goose Up
--
-- Why a deploy failed, kept on the row rather than only on the wire.
--
-- **The frame was already the contract and it was lying by omission.** A
-- progress frame is a snapshot — every one states the deploy's status, phase
-- and step in full — precisely so that a client which reconnects loses nothing.
-- The failure message was the one thing that broke that promise: it was
-- published on the live terminal frame and nowhere else, so
-- `builder.SnapshotProgress`, which is what a reconnecting or late watcher
-- actually receives, reported a failed deploy with no reason at all. `lp deploy
-- --watch` on a build that had already failed printed the step and stopped.
-- Storing it is what makes the snapshot as complete as the live frame, which is
-- what the design said it was.
--
-- **Two columns and not one.** failure_message is the paragraph a person reads;
-- failure_reason is the key a program branches on
-- (`internal/deploysteps.Reason`). Deriving the second from the first would mean
-- matching on prose that ~30 call sites are free to reword, which is the
-- situation this replaces.
--
-- No CHECK on failure_reason, following deployments.dependency_verdict and
-- dependency_scans.advisories: it is a **vocabulary that will grow**, and a
-- CHECK would make adding a reason a migration on every install before the
-- build that writes it can run. The vocabulary is guarded in Go, where both
-- ends of the wire can see it, and an unknown key is required to render rather
-- than vanish.
--
-- Empty is the honest value for every row already written, and means
-- *unclassified* — never internal_error, which is a claim about the platform
-- that no historical row supports. DEFAULT '' NOT NULL, so there is nothing to
-- backfill and nothing that has to distinguish NULL from "".
--
-- The message is deliberately duplicated out of build_log rather than parsed
-- back from it. A build log is bytes an app's build produced and is subject to
-- retention; the reason a deploy was refused is state, and it must still answer
-- when the log it was also written to has been trimmed.
-- **No `--` inside a string literal here.** goose does not see the terminating
-- semicolon of a statement whose string contains one, so that statement silently
-- runs on into the next -- which Postgres executes anyway, with the full text
-- intact, which is why 00037 and 00041 have carried one for months and work.
-- When the affected statement is the *last* in the block there is nothing to run
-- on into, and goose fails the whole migration with `unexpected unfinished SQL
-- query` at **startup**. Verified by bisection against a real database on
-- 2026-09-02: the same comment passes with a statement after it and fails
-- without. `TestNoCommentMarkerInsideAStringLiteral` is the guard.
ALTER TABLE deployments ADD COLUMN failure_reason text NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN failure_message text NOT NULL DEFAULT '';

COMMENT ON COLUMN public.deployments.failure_reason IS 'Why a failed deploy failed, as a key a program branches on rather than a sentence; internal/deploysteps.Reason. No CHECK: a growing vocabulary, guarded in Go where both ends of the wire can see it, and an unknown key renders rather than vanishing. Empty means unclassified, which is every row written before this column and is never internal_error.';

COMMENT ON COLUMN public.deployments.failure_message IS 'The whole reason a deploy failed, in the words the build log got. Stored because a progress frame is a snapshot and SnapshotProgress (what a reconnecting watcher receives) could not previously say why. Duplicated out of build_log on purpose: the log is subject to retention and this must still answer afterwards.';

-- +goose Down
--
-- Both columns go, and with them the answer to "why did this fail" for every
-- deployment whose build log has since been trimmed. Nothing is re-derivable:
-- the reason was classified at the moment of failure by a builder that is no
-- longer running. A watcher on the build below this goes back to receiving a
-- terminal frame that says where a deploy died and not why.
ALTER TABLE deployments DROP COLUMN failure_reason;
ALTER TABLE deployments DROP COLUMN failure_message;
