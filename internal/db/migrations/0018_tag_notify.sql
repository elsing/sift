-- Per-tag notification toggle — a tag the user trusts to auto-sort mail (e.g.
-- newsletters, receipts) is exactly the kind of mail that doesn't need a push
-- notification every time, so this needs to be checkable before a push fires, not
-- just at display time.
ALTER TABLE tags ADD COLUMN IF NOT EXISTS notify BOOLEAN NOT NULL DEFAULT true;
