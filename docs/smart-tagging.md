# Smart tagging

Sift's manual tagging system (tags, the reader's tag picker, folder→tag rules) is fully
described by the code itself — `internal/api/tags.go`, `internal/db/migrations/0013_tags.sql`.
This doc covers the layer built on top of it: scoring, suggesting, auto-applying, a
one-off scan of existing mail, and auto-moving tagged mail into a folder after a delay.
The implementation lives in `internal/api/smarttags.go`.

## Why a separate `tag_history` table

`mails` is an ephemeral IMAP cache — rows get deleted on archive/delete/move.
`message_tags` survives that (it's keyed on Message-ID, not the ephemeral cache id),
but the sender/subject info needed to *score* future mail does not survive once a
tagged mail's cache row is evicted. `tag_history` (migration `0015_tag_history.sql`) is
a durable, denormalized record of every tag decision — independent of the cache — and
it doubles as the user-facing audit/history log.

## Pooled across accounts, not per-account

`tags` itself has no `account_id` — it's an `owner_subject`-level abstraction that sits
above individual IMAP accounts. `tag_history` follows the same rule: every scoring and
suppression query is scoped by `owner_subject`, never `account_id`. The same sender
emailing two of your accounts shares one tagging history rather than starting cold in
each. `tag_history.account_id` is kept on each row purely for reference (e.g. the audit
view can show which account a decision came from) — it's never part of a `WHERE`.

## The scoring formula

For a candidate `(sender or domain, tag)` pair:

```
ratio = appliedCount / (appliedCount + dismissedCount + K)   // K = 3
```

This single formula *is* the sample-size confidence check. A single applied/0
dismissed match scores 0.25 — nowhere near either threshold. Eight applied/0 dismissed
scores ~0.73 — close to, but still under, the auto-apply bar. It takes 9 consistent
applies with zero dismissals to cross 0.75. That's deliberate: the bar to *auto-apply*
a tag without asking is meant to be genuinely hard to clear by accident.

The same `ratio` function backs three signals:
- **Sender** — exact `sender_email` match.
- **Domain** — `sender_domain` match (the part after `@`).
- **Subject** — not exact-match (subjects are rarely literally identical except thread
  digests). Subjects are tokenized (lowercased, punctuation stripped, stopwords and
  short tokens dropped) and compared via Jaccard overlap against every past tagged
  subject for that tag; an overlap > 0.3 counts as a "match" and feeds the same ratio.

Combined: `score = 0.6×sender + 0.25×domain + 0.15×subject`. Two guards against the
obvious false-positive trap:
- If sender-history alone already clears the auto-apply bar, domain/subject aren't
  computed at all (cheap path, and there's no need).
- If a sender has **zero** history — by exact address or domain — the subject signal
  alone can never suggest a tag. A single keyword match on its own is not evidence.

Constants (`smarttags.go`): `autoApplyScore = 0.75`, `suggestScore = 0.4`,
`maxSuggestionsPerMail = 3`, `dismissSuppressCount = 2`.

## `source` and `status` vocabulary

`tag_history.source` records *how* a decision was made:

| source | meaning |
|---|---|
| `manual` | You applied it via the tag picker or swipe-to-tag. |
| `folder_rule` | The existing folder→tag rule fired on a move. |
| `smart_auto` | The live scorer auto-applied it (full-auto mode, score ≥ 0.75). |
| `smart_suggested` | The live scorer queued it for review. |
| `scan_inferred` | The bootstrap scan (see below) found it. |

`status` records *where it stands*: `applied` (a real `message_tags` row exists),
`suggested` (pending, nothing committed yet), or `dismissed` (rejected — and counted
toward `dismissSuppressCount` so the same sender+tag pair eventually stops being
re-suggested).

Manually removing a tag that was auto-applied or suggested logs a `dismissed` row too
(see `recordManualTagChange` in `tags.go`) — without that, a correction would teach the
scorer nothing, and it would just reapply the same tag next time.

## The two modes

`owner_settings.auto_tag_mode` is global, not per-account:
- **`review`** — every match ≥ `suggestScore` is queued for review. Nothing gets
  auto-applied, regardless of how high it scores.
- **`full_auto`** — matches ≥ `autoApplyScore` auto-apply; everything else in
  `[suggestScore, autoApplyScore)` is still queued for review. "Full-auto" means *not
  having to babysit the confident matches* — it doesn't mean every plausible match gets
  silently applied.

## Scanning existing mail (the bootstrap)

New accounts — or accounts that have been hand-sorted into folders for years — start
with a mailbox full of signal the live scorer never sees, because it only evaluates
*new* mail going forward. "Scan for tags" (Settings → Smart tagging) reads every folder
in one account and applies three signals:

- **Existing tag history** — mail in a ruleless folder is scored exactly like live new
  mail (same `scoreTagsForMail`, same two-tier mode behavior).
- **Folder concentration (known)** — mail sitting in a folder that already has a
  `folder_tag_rules` entry is strong retroactive evidence for that sender↔tag pairing.
  Logged directly as `folder_rule`/`applied` — this is what gives the live scorer real
  density immediately, rather than only whatever happens to still be cache-resident.
- **Folder concentration (unknown)** — for senders with *no* tag history at all,
  concentrated in one ruleless folder (≥ 3 mail, ≥ 70% of that sender's mail in this
  scan sitting in that one folder), the scan surfaces a "create a tag for this?"
  candidate. This is never auto-applied in either mode — naming a brand-new tag needs a
  person, not a score.

The scan streams progress over SSE (same pattern as `/api/search`) since it's a
multi-folder, multi-IMAP-round-trip operation that can take a while on a large mailbox.

## Auto-move by tag — the opportunistic timer

Once a tag is known to map to a folder (`folder_tag_rules`, the same table the
folder→tag rule already uses), mail carrying that tag that's sat in the inbox past
`owner_settings.auto_move_delay_days` (default 3) gets moved there automatically. This
reads `folder_tag_rules` in the *other* direction — no new mapping concept to learn.

**This is not a real timer.** There's no cron/background-job infrastructure in this
app — only the IMAP IDLE event loop. `autoMoveTaggedMail` is called opportunistically
from `syncAccount`, which fires on every IDLE wake, manual pull-to-refresh, and account
sync. In practice that's frequent enough to feel timely, but the actual move happens
"the next time something causes a sync for that account after the delay has elapsed,"
not at the exact moment the delay expires. If a tag maps to more than one folder for
an account, auto-move skips it rather than guessing which one.

## Verifying this is working

- `tag_history` directly: `SELECT source, status, count(*) FROM tag_history GROUP BY 1, 2;`
- An account's first-ever sync should produce zero smart-tagging activity (the
  `last_seen_uid` watermark is `NULL` until the first sync sets it, so nothing counts
  as "new" yet) — this is intentional, not a bug, and avoids flooding suggestions
  against a whole pre-existing inbox the moment an account is added.
