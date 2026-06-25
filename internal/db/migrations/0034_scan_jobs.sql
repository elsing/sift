-- Persists scan progress server-side so a reloaded client (or a second tab) can pick
-- up exactly where a "Scan for tags"/"Scan for spam" run actually is, rather than the
-- progress view just vanishing — the scan itself already keeps running either way
-- (see runScanJob, smarttags.go: it's driven by a server-lifetime context now, not the
-- HTTP request that started it), this is what lets the UI find it again.
CREATE TABLE IF NOT EXISTS scan_jobs (
    id TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('tags', 'spam')),
    status TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'error', 'cancelled', 'interrupted')),
    done INT NOT NULL DEFAULT 0,
    total INT NOT NULL DEFAULT 0,
    summary JSONB,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- "is there already a running job for this account+kind" (started) and "is there a
-- running job at all for this owner+kind" (panel-open auto-resume) are the only two
-- lookups this table ever serves.
CREATE INDEX IF NOT EXISTS scan_jobs_account_kind_status_idx ON scan_jobs (account_id, kind, status);
CREATE INDEX IF NOT EXISTS scan_jobs_owner_kind_status_idx ON scan_jobs (owner_subject, kind, status);
