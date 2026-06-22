-- Explicit, opt-in "don't suggest this tag for this sender again" — separate from a
-- plain dismiss, which only ever means "wrong for this one email" (see suppressedTags
-- in smarttags.go). This is the user's deliberate choice to go broader, not an
-- automatic consequence of dismissing enough times.
CREATE TABLE IF NOT EXISTS tag_sender_blocks (
    owner_subject TEXT NOT NULL,
    sender_email TEXT NOT NULL,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_subject, sender_email, tag_id)
);
