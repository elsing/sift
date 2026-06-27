-- Queue for IMAP operations (move, delete, archive, flag changes) so HTTP handlers
-- can return immediately after updating the local DB, with the actual IMAP round-trip
-- happening in a background worker. Survives server restarts: pending rows are picked
-- up again on next start (all IMAP ops here are idempotent or fail-safe on retry).
CREATE TABLE IF NOT EXISTS imap_jobs (
    id          TEXT        PRIMARY KEY,
    account_id  TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL CHECK (kind IN ('move', 'archive', 'delete', 'set-read', 'set-unread')),
    payload     JSONB       NOT NULL DEFAULT '{}',
    status      TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'error')),
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Worker scans for pending jobs in insertion order.
CREATE INDEX IF NOT EXISTS imap_jobs_status_created_idx ON imap_jobs (status, created_at);
