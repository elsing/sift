-- Remote images in HTML mail (most commonly tracking pixels) are blocked by default
-- now — this is the explicit opt-in to load them automatically for a given sender
-- going forward, instead of needing "Show images" every single time.
CREATE TABLE IF NOT EXISTS trusted_senders (
    owner_subject TEXT NOT NULL,
    sender_email TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_subject, sender_email)
);
