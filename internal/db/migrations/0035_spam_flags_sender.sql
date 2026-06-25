-- Denormalized the same way tag_history already does (sender_email/sender_domain) and
-- for the same reason: scoring needs to survive the source mails row being
-- archived/deleted, not just exist while it's still cached. Lets a sender's own
-- historical auth-check pass rate be queried directly from spam_flags, without joining
-- back to mails (which a since-archived message would no longer have a row for).
ALTER TABLE spam_flags ADD COLUMN IF NOT EXISTS sender_email TEXT NOT NULL DEFAULT '';
ALTER TABLE spam_flags ADD COLUMN IF NOT EXISTS sender_domain TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS spam_flags_sender_idx ON spam_flags (owner_subject, sender_email);
