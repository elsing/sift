-- One-time cleanup: recordTagHistory (internal/api/smarttags.go) now auto-resolves any
-- pending 'suggested' row whenever the same (message_id, tag_id) pair gets applied
-- through any path — but that fix only takes effect going forward. This catches
-- suggestions that went stale before it existed (e.g. a tag applied via "Apply to all"
-- in a folder, while the live scorer's earlier suggestion for that same mail+tag sat
-- untouched).
UPDATE tag_history AS suggestion
SET status = 'applied', resolved_at = now()
WHERE suggestion.status = 'suggested'
  AND EXISTS (
    SELECT 1 FROM tag_history AS applied
    WHERE applied.message_id = suggestion.message_id
      AND applied.tag_id = suggestion.tag_id
      AND applied.status = 'applied'
  );
