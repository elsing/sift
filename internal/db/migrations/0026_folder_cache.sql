CREATE TABLE IF NOT EXISTS folder_cache (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    names      TEXT[] NOT NULL,
    delim      TEXT NOT NULL DEFAULT '',
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
