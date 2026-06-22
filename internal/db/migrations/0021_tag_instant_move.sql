-- Per-tag override of the global auto-move delay — a tag like "Receipts" you always
-- want filed away right now, not after the usual few days everything else waits.
-- Off by default: the global delay (owner_settings.auto_move_delay_days) still governs
-- every tag unless this is explicitly turned on for it.
ALTER TABLE tags ADD COLUMN IF NOT EXISTS instant_move BOOLEAN NOT NULL DEFAULT false;
