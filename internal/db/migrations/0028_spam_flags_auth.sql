-- The reader's bottom-of-mail diagnostic readout reads stored spam_flags instead of
-- recomputing scoreSpam live on every single mail open (that was the bug — opening
-- mail shouldn't itself run the scorer; only an explicit scan, or the new-mail sync
-- path, should). SPF/DKIM/DMARC need their own columns so that stored row carries the
-- same detail the reader used to compute on the fly.
ALTER TABLE spam_flags ADD COLUMN IF NOT EXISTS spf TEXT NOT NULL DEFAULT 'none';
ALTER TABLE spam_flags ADD COLUMN IF NOT EXISTS dkim TEXT NOT NULL DEFAULT 'none';
ALTER TABLE spam_flags ADD COLUMN IF NOT EXISTS dmarc TEXT NOT NULL DEFAULT 'none';
