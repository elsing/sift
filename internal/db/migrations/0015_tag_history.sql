ALTER TABLE accounts ADD COLUMN IF NOT EXISTS last_seen_uid BIGINT;

-- Durable, denormalized record of every tag decision (manual, folder-rule, or smart
-- auto/suggested/scan-inferred) — independent of the mails cache so sender/subject
-- scoring survives archive/delete/move evicting the source mails row. Doubles as the
-- user-facing audit/history log. Scoped by owner_subject, not account_id: tags are an
-- abstraction above individual IMAP accounts (the tags table itself has no account_id
-- at all), so history pools across every account a user has rather than fragmenting.
-- account_id is kept for reference/debugging only, never used in a scoring WHERE.
CREATE TABLE IF NOT EXISTS tag_history (
    id TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL,
    account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE,
    message_id TEXT NOT NULL,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    sender_email TEXT NOT NULL DEFAULT '',
    sender_domain TEXT NOT NULL DEFAULT '',
    subject_tokens TEXT[] NOT NULL DEFAULT '{}',
    source TEXT NOT NULL CHECK (source IN ('manual', 'folder_rule', 'smart_auto', 'smart_suggested', 'scan_inferred')),
    status TEXT NOT NULL CHECK (status IN ('applied', 'suggested', 'dismissed')),
    score REAL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS tag_history_sender_tag_idx ON tag_history (sender_email, tag_id);
CREATE INDEX IF NOT EXISTS tag_history_domain_tag_idx ON tag_history (sender_domain, tag_id);
CREATE INDEX IF NOT EXISTS tag_history_owner_status_idx ON tag_history (owner_subject, status, created_at DESC);

-- Seed from whatever's still resident in the mails cache today, so scoring isn't
-- completely cold on day one. Deterministic id makes this idempotent.
INSERT INTO tag_history (id, owner_subject, account_id, message_id, tag_id, sender_email, sender_domain, source, status, created_at)
SELECT encode(sha256((mt.message_id || ':' || mt.tag_id)::bytea), 'hex'),
       a.owner_subject, m.account_id, mt.message_id, mt.tag_id,
       coalesce(m.sender_email, ''), coalesce(split_part(m.sender_email, '@', 2), ''),
       'manual', 'applied', now()
FROM message_tags mt
JOIN mails m ON m.message_id = mt.message_id
JOIN accounts a ON a.id = m.account_id
WHERE mt.message_id IS NOT NULL AND mt.message_id != ''
ON CONFLICT (id) DO NOTHING;

-- One row per logged-in owner: the global auto-tagging mode and auto-move delay.
CREATE TABLE IF NOT EXISTS owner_settings (
    owner_subject TEXT PRIMARY KEY,
    auto_tag_mode TEXT NOT NULL DEFAULT 'review' CHECK (auto_tag_mode IN ('full_auto', 'review')),
    auto_move_delay_days INT NOT NULL DEFAULT 3
);
