-- Detected from BODYSTRUCTURE at sync time (cheap — no body fetch) so the inbox list
-- can show a paperclip without needing the much more expensive full-body fetch that
-- attachment listing itself uses.
ALTER TABLE mails ADD COLUMN IF NOT EXISTS has_attachments BOOLEAN NOT NULL DEFAULT false;
