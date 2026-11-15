-- +goose Up
--
-- A resend of an announcement to the people its run did not reach (ADR §79):
-- its own frozen row, pointing at the one it resends. No foreign key, the way
-- cloned_from has none — the original may be deleted, and "a resend of
-- something that is gone" is still true. Unique, because an announcement is
-- resent once; what that resend in turn failed to reach is resent from it.
ALTER TABLE announcement_emails
    ADD COLUMN resend_of text;
COMMENT ON COLUMN announcement_emails.resend_of IS 'The announcement this row resends to the people whose delivery failed or was abandoned. Set only on a row created already sending by the resend route; no foreign key, the original may be deleted.';
CREATE UNIQUE INDEX announcement_emails_resend_of_idx
    ON announcement_emails (resend_of) WHERE resend_of IS NOT NULL;

-- +goose Down
DROP INDEX announcement_emails_resend_of_idx;
ALTER TABLE announcement_emails DROP COLUMN resend_of;
