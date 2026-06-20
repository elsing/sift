CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    subject    TEXT NOT NULL,
    email      TEXT NOT NULL,
    name       TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
