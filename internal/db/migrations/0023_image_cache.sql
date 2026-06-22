CREATE TABLE IF NOT EXISTS image_cache (
    url_hash TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    bytes BYTEA NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
