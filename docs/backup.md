# Backup / restore

A full export/import of everything needed for a copy-paste deployment or disaster
recovery — not just tags. The implementation lives in `internal/api/datatransfer.go`,
behind its own "Backup" menu in Settings (deliberately not folded into Advanced, and
deliberately not named after tags/spam specifically — it covers the whole account).

## What's in the bundle

- **Connected accounts** — host/port/username, the IMAP password or OAuth refresh
  token (decrypted on export, re-encrypted with whatever `ENCRYPTION_KEY` the
  *importing* deployment has — this is what makes the bundle portable across
  installs), and expanded-folder UI state. On import, an account is matched by email;
  a genuinely new one is added only after a live IMAP login test passes.
- **Tags** — name, color, notify, instant-move.
- **Per-mail tag assignments** (`message_tags`), matched by Message-ID, not the
  ephemeral mail cache id.
- **Folder→tag rules**, matched by account email + folder path.
- **Trusted senders** and **tag sender-blocks**.
- **`tag_history`** — not a log. This is the actual learned state
  `senderRatio`/`domainRatio`/`subjectRatio` (see `docs/smart-tagging.md`) score
  against. Skipping it would mean a restored install starts smart tagging completely
  cold even though every tag and rule it depends on came back intact.
- **`spam_flags`** — past spam-scoring results (score, reasons, SPF/DKIM/DMARC). Purely
  diagnostic (the reader's "why flagged" readout) — `scoreSpam` itself is recomputed
  fresh from headers every time and never reads this table, but it's still real
  user-facing history worth restoring.
- **Owner settings** — auto-tag mode, spam mode, auto-move delay, image cache
  retention.
- **Browser-only personalisation** (`localPreferences`) — theme, palette, swipe-left/
  right actions, fun-delete-animation, auto-load-images. These never touch the server
  otherwise (plain `localStorage`), so the client gathers them itself and sends them
  in the export request body; import hands them back in its response for the client
  to write back to `localStorage` (then reloads, since theme/palette only apply at
  render time).

Deliberately excluded: push subscriptions (browser-specific, can't be replayed on
another device), and the cached archive/trash/junk folder names (auto-detected fresh
on first use, not worth carrying stale).

## Why export is POST, not GET

The client has to hand the server its `localPreferences` for the server to fold into
the bundle before it's (optionally) encrypted — there's no other way to get
browser-only state into a server-generated file.

## Encryption

The bundle contains plaintext IMAP passwords/OAuth tokens — necessary for a true DR
restore, no way around it. An optional passphrase (`X-Backup-Password` header) makes
the file itself opaque: the server derives a key via `crypto/pbkdf2` (stdlib, Go 1.26;
600k iterations, OWASP's 2023 minimum for PBKDF2-HMAC-SHA256) and AES-GCM-encrypts the
whole JSON bundle. No password given exports as plain JSON, same as before — the UI
warns about this either way. A wrong or missing password on import is rejected with a
clear error (AES-GCM's own auth tag, not a parse failure) — never silently produces
garbage data.

## Two scanning gotchas worth knowing if you touch this file again

Both `tag_history.subject_tokens` and (originally) `spam_flags.reasons` are Postgres
`TEXT[]` columns. pgx's `database/sql` stdlib adapter can't `Scan` an array column
directly into a Go `[]string` — `spam_flags.reasons` was migrated to plain `TEXT` for
exactly this reason (migration `0030_spam_flags_reasons_text.sql`). `subject_tokens`
wasn't, so the export query pulls it via `array_to_string(..., E'\n')` and splits it
back apart in Go (tokens are always lowercased, punctuation-stripped words —
`tokenizeSubject`, `smarttags.go` — so a literal newline can never appear in one).

The reverse direction has its own trap: a `nil` Go `[]string` (the zero value when a
mail had no subject tokens at all) binds as SQL `NULL`, not `'{}'` — and
`subject_tokens` is `NOT NULL`. Import coerces `nil` to `[]string{}` before the insert.

## No de-duplication on `tag_history` / `spam_flags`

Every other table in the bundle upserts (`ON CONFLICT DO NOTHING`/`DO UPDATE`) because
it has a real natural key. `tag_history` and `spam_flags` don't — each row is its own
freestanding event with a random id, so re-importing the same backup twice doubles
both tables. That's the accepted tradeoff for a restore-from-scratch flow; it's not
meant to be replayed repeatedly against a live system.
