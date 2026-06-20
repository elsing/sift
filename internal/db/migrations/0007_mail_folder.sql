-- ID scheme changes from "<account>-<uid>" to "<account>|<folder>|<uid>" so UIDs from
-- different folders (only unique within their own mailbox) don't collide. mails is just
-- a disposable IMAP cache, not the source of truth, so clearing it is safe — the next
-- sync repopulates it under the new scheme.
DELETE FROM mails;
ALTER TABLE mails ADD COLUMN IF NOT EXISTS folder TEXT NOT NULL DEFAULT 'INBOX';
