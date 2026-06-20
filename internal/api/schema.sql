CREATE TABLE IF NOT EXISTS accounts (
    id            TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL,
    email         TEXT NOT NULL,
    host          TEXT NOT NULL,
    port          INTEGER NOT NULL,
    username      TEXT NOT NULL,
    password_enc  BYTEA NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_sync_error TEXT
);

CREATE TABLE IF NOT EXISTS mails (
    id         TEXT PRIMARY KEY,
    account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE,
    sender     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    snippet    TEXT NOT NULL,
    time       TEXT NOT NULL,
    unread     BOOLEAN NOT NULL
);

-- ponytail: no migration framework; ALTER ... IF NOT EXISTS covers columns added after initial deploy.
ALTER TABLE mails ADD COLUMN IF NOT EXISTS account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS last_sync_error TEXT;
