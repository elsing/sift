ALTER TABLE owner_settings ADD COLUMN IF NOT EXISTS image_cache_retention_days INT NOT NULL DEFAULT 90;
