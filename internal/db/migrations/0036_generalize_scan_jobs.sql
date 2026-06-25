-- scan_jobs started out scoped to one account (a folder scan), but the same
-- persistence — survives a reload, reports real progress instead of nothing, doesn't
-- die when the connection watching it drops — belongs to every long-running action in
-- the app, including owner-wide ones with no single account to scope to (a spam
-- cleanup across every account, say). account_id becomes optional for those; kind
-- widens to name the new job types.
ALTER TABLE scan_jobs ALTER COLUMN account_id DROP NOT NULL;
ALTER TABLE scan_jobs DROP CONSTRAINT IF EXISTS scan_jobs_kind_check;
ALTER TABLE scan_jobs ADD CONSTRAINT scan_jobs_kind_check CHECK (kind IN (
    'tags', 'spam', 'cleanup-unconfirmed-spam', 'restore-stranded-spam',
    'cleanup-duplicate-folders', 'image-backfill', 'apply-tag-folder',
    'move-misplaced-mail'
));
