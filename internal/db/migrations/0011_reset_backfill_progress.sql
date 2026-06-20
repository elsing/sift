-- oldest_synced_uid could get stuck "exhausted" (1) from before the mails cache was
-- last cleared, permanently blocking backfill even after the cache was rebuilt from
-- scratch and had plenty more real history to pull. One-time reset so backfill runs
-- again; see the self-heal fix in handleList for why this can't recur.
UPDATE accounts SET oldest_synced_uid = NULL;
