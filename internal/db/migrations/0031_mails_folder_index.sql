-- cachedFolderMails (accounts.go) — the now-default, DB-backed folder listing path —
-- filters by (account_id, folder) and orders by (sent_at, id) on every single call.
-- With no index here this was a full sequential scan + sort of the entire mails table
-- every time, not noticeable yet at a few thousand rows but exactly the kind of thing
-- that gets slower as the periodic full-folder sync (watchFolderMailSync) keeps
-- filling more folders in. The DESC NULLS LAST matches the query's own
-- coalesce(sent_at, '-infinity') ordering closely enough for Postgres to use this for
-- both the initial page and the cursor-paged continuation.
CREATE INDEX IF NOT EXISTS mails_account_folder_sent_idx
    ON mails (account_id, folder, sent_at DESC NULLS LAST, id DESC);
