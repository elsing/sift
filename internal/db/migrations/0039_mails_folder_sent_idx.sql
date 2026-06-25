-- list() (mails.go) queries WHERE folder = 'INBOX' ORDER BY sent_at DESC, id DESC with
-- no account_id filter. The existing mails_account_folder_sent_idx has account_id as its
-- leading column, so Postgres falls back to a full table scan for the cross-account case.
CREATE INDEX IF NOT EXISTS mails_folder_sent_idx ON mails (folder, sent_at DESC NULLS LAST, id DESC);
