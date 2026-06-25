-- Same class of gap as mails.message_id (migration 0037), caught by the same audit:
-- every "has this exact (message, tag) pair already been decided" check — run on
-- essentially every message during every scan and every live sync
-- (alreadySuggested/alreadyDecided/suppressedTags, spam.go and smarttags.go) — filters
-- by message_id with no index covering it. Postgres falls back to an existing index on
-- the wrong leading column (sender_domain/sender_email) and filters the rest, which
-- still means scanning every row for whatever tag is being checked rather than a
-- direct lookup — fine today, the same kind of thing that was "fine" for mails.message_id
-- right up until it wasn't.
CREATE INDEX IF NOT EXISTS tag_history_message_tag_idx ON tag_history (message_id, tag_id);

-- scoreTagsForMail's "has this sender ever been seen at all" check (smarttags.go) —
-- run on every single message during every tag scan and every live new-mail
-- evaluation — filters by owner_subject AND (sender_email OR sender_domain).
-- Confirmed via EXPLAIN: a full sequential scan, since neither existing index
-- (sender_email, tag_id) nor (sender_domain, tag_id) has owner_subject as a column,
-- and an OR across two different columns can't use either as a single ordered lookup
-- anyway. Two narrower indexes let Postgres do a BitmapOr of two real index scans
-- instead of reading the whole table on every single message scored.
CREATE INDEX IF NOT EXISTS tag_history_owner_sender_idx ON tag_history (owner_subject, sender_email);
CREATE INDEX IF NOT EXISTS tag_history_owner_domain_idx ON tag_history (owner_subject, sender_domain);
