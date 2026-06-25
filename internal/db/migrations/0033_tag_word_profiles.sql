-- body_tokens parallels subject_tokens — populated only where a body was already
-- fetched for scoring (spam scan, live image-prefetch scoring, tag scan once it also
-- fetches bodies); nil for decisions with no body in hand (manual, folder_rule).
ALTER TABLE tag_history ADD COLUMN IF NOT EXISTS body_tokens TEXT[];

-- One row per tag: its aggregate word→weight "signature", built from body_tokens
-- across that tag's own applied history. Rebuilt wholesale on demand (see
-- rebuildWordProfile, smarttags.go), not kept incrementally up to date — words is a
-- map[string]float64 as JSON rather than a token-per-row table, since the whole profile
-- is always read/written together, never queried by individual word.
CREATE TABLE IF NOT EXISTS tag_word_profiles (
    tag_id TEXT PRIMARY KEY REFERENCES tags(id) ON DELETE CASCADE,
    words JSONB NOT NULL,
    built_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-owner choice of weighting formula — defaults to plain frequency; 'distinctive'
-- opts into the heavier TF-IDF-style cross-tag computation (rebuildWordProfile).
ALTER TABLE owner_settings ADD COLUMN IF NOT EXISTS word_profile_weighting TEXT NOT NULL DEFAULT 'plain'
    CHECK (word_profile_weighting IN ('plain', 'distinctive'));
