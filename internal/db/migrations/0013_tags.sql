ALTER TABLE mails ADD COLUMN IF NOT EXISTS message_id TEXT;

CREATE TABLE IF NOT EXISTS tags (
    id TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL,
    name TEXT NOT NULL,
    color TEXT NOT NULL DEFAULT '#9b5de5',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_subject, name)
);

-- Keyed by the message's real Message-ID header, not mails.id: mails.id is an
-- ephemeral cache key (account|folder|uid) that changes whenever a message moves
-- folders, and rows get deleted/re-upserted constantly as the cache is rebuilt. A tag
-- needs to survive both.
CREATE TABLE IF NOT EXISTS message_tags (
    message_id TEXT NOT NULL,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, tag_id)
);

-- Moving a message into folder X auto-applies tag Y.
CREATE TABLE IF NOT EXISTS folder_tag_rules (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    folder TEXT NOT NULL,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (account_id, folder)
);
