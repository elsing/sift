-- Widen tag_history.source to allow the spam engine's own audit trail through the
-- same recordTagHistory path everything else already uses.
ALTER TABLE tag_history DROP CONSTRAINT IF EXISTS tag_history_source_check;
ALTER TABLE tag_history ADD CONSTRAINT tag_history_source_check
    CHECK (source IN ('manual', 'folder_rule', 'smart_auto', 'smart_suggested', 'scan_inferred', 'spam_engine'));

-- Mirrors archive_folder/trash_folder — best-effort, detected the same way.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS junk_folder TEXT;

-- review (not full_auto) is the safer default here for the opposite reason regular
-- tagging defaults to review: a spam false-positive burying real mail is worse than a
-- missed spam-suggestion sitting in a review queue.
ALTER TABLE owner_settings ADD COLUMN IF NOT EXISTS spam_mode TEXT NOT NULL DEFAULT 'review' CHECK (spam_mode IN ('full_auto', 'review'));

-- The "why flagged" note the user explicitly asked to see — tag_history's own score
-- column has no room for a human-readable reason list.
CREATE TABLE IF NOT EXISTS spam_flags (
    id TEXT PRIMARY KEY,
    message_id TEXT NOT NULL,
    owner_subject TEXT NOT NULL,
    score REAL NOT NULL,
    reasons TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS spam_flags_message_idx ON spam_flags (message_id);
