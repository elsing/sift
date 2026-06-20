CREATE TABLE IF NOT EXISTS accounts (
    id            TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL,
    email         TEXT NOT NULL,
    host          TEXT NOT NULL,
    port          INTEGER NOT NULL,
    username      TEXT NOT NULL,
    password_enc  BYTEA NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS mails (
    id         TEXT PRIMARY KEY,
    sender     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    snippet    TEXT NOT NULL,
    time       TEXT NOT NULL,
    unread     BOOLEAN NOT NULL
);
