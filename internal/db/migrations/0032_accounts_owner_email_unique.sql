-- Nothing previously stopped two rows for the same (owner_subject, email) — every
-- insert path (manual add, Google OAuth, backup import) does an app-level "check by
-- email, then insert" with no DB-level backstop, which is race-prone (two concurrent
-- requests can both pass the check before either commits). No duplicates exist today,
-- but the project's had enough duplicate-row incidents elsewhere (tag_history,
-- mails-cache ghosts) that this is worth closing now rather than after it happens.
ALTER TABLE accounts ADD CONSTRAINT accounts_owner_subject_email_key UNIQUE (owner_subject, email);
