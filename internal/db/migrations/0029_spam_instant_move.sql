-- The auto-created Spam tag now sets instant_move=true at creation (imageproxy.go's
-- getOrCreateSpamTag), but a Spam tag created before that change predates it and was
-- never retroactively touched — backfilling it once here so spam tagged before this
-- deploy also skips the regular auto_move_delay_days wait, same as one tagged after.
UPDATE tags SET instant_move = true WHERE name = 'Spam' AND instant_move = false;
