-- handleListTagHistory's ORDER BY (smarttags.go) does a correlated subquery against
-- mails.message_id once per tag_history row to sort suggestions by the mail's actual
-- date — with no index here, each of those was a full sequential scan of the whole
-- mails table. Harmless at small scale, catastrophic once the suggested-row cap was
-- removed: 3000+ rows × a multi-thousand-row table scan each measured at 7.6s for one
-- request. Partial (only non-null message_id has any use as a join/lookup key).
CREATE INDEX IF NOT EXISTS mails_message_id_idx ON mails (message_id) WHERE message_id IS NOT NULL;
