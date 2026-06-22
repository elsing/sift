ALTER TABLE owner_settings ADD COLUMN IF NOT EXISTS image_backfill_completed_at TIMESTAMPTZ;
