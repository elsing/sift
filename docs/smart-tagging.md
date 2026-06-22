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
`maxSuggestionsPerMail = 3`.

Mail with no message ID, no sender, or no tags configured yet short-circuits before
any of this runs. A scan also never re-evaluates the same `Message-ID` twice in one
pass — duplicate physical copies of the same mail (different IMAP UIDs, same
Message-ID) used to each log their own redundant suggestion for the identical
(message, tag) pair, which is what made a single dismissed suggestion look like it
"came back" on the next scan. And mail that's already tagged with *something* skips
scoring entirely on a scan — the scan never removes a tag it disagrees with, so
re-scoring already-sorted mail can only ever add one more suggestion, never fix a
wrong one, which isn't worth several DB round trips per mail on every run.

## Dismiss vs. block — two different verdicts

Dismissing a suggestion (`status = 'dismissed'`) is purely "this tag is wrong for
*this one email*" — `suppressedTags` checks it by exact `message_id`, never by sender.
An earlier version suppressed by sender after enough dismissals; that turned out to
silence legitimate matches too readily (a shared ESP/large-provider domain bleeding
across unrelated senders), and wasn't actually what dismissing one email was supposed
to mean.

The broader, sender-wide version is a separate, explicit, opt-in action —
**"Don't suggest this tag from this sender again,"** shown next to each pending
suggestion. It writes to `tag_sender_blocks` (migration `0017`), a dedicated table
of `(owner_subject, sender_email, tag_id)`, checked by `blockedSenderTags`. Blocking
also clears every other still-pending suggestion of that same tag from that same
sender — otherwise you'd block it and immediately see more of the exact same thing.

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

Deliberately only two modes, no in-between "semi-auto" — full-auto's whole bargain is
"confident matches apply with no review step," and the thing that bargain actually
needs is an easy way to audit and reverse what it did, not a third mode to reason
about. That's **Auto-tag activity** (Settings → Auto-tag activity, split out from the
Smart Tagging panel itself since that panel was already getting long): every
`smart_auto`/`applied` row, newest first, each with a one-tap **Undo**. Undo deletes
the real `message_tags` row and marks that `tag_history` row `dismissed` — the same
status a manual dismiss leaves, so undoing a wrong auto-tag also teaches the scorer
not to repeat it via `suppressedTags`, not just a one-time fix.

## Per-tag notifications

Each tag has a `notify` column (migration `0018`, default `true`). A tag you trust
enough to auto-sort mail into — newsletters, receipts — is exactly the kind of mail
that doesn't need a push every time. `notifyNewMail` (`realtime.go`) checks
`allTagsMuted` before sending: a mail is only silenced if it has at least one tag *and*
every one of its tags has `notify = false`. One still-loud tag wins over any number of
muted ones on the same mail. A mail with no tags at all is never muted by this — it's
about tags you've explicitly silenced, not a default-quiet state.

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
- **Folder concentration (unknown)** — a ruleless folder with ≥ 3 mail that didn't match
  any existing tag history surfaces a "create a tag for this?" candidate, one per
  *folder* (not per sender — a folder is almost always one sorting decision regardless
  of who sent any given mail inside it, so "12 from sender A, 8 from sender B, all in
  Volunteering" is one real signal, not several near-duplicate suggestions fragmented
  by source address). Mail older than 2 years doesn't count toward this — a sender's
  filing pattern from years ago isn't a useful predictor of where new mail belongs
  today, and it was the single biggest source of noise here. Never auto-applied in
  either mode — naming a brand-new tag needs a person, not a score.

"All folders" scope excludes the account's Trash folder entirely — it's mostly
junk/deleted mail by nature, and scoring against it taught the scorer patterns nobody
actually wanted learned. (Picking Trash explicitly via "Choose folders" still scans it
— that's a deliberate override.) INBOX is also never eligible as a new-tag candidate
in any scope: "this mail is in my inbox" is structural, not something the user sorted
by hand. An earlier bug let exactly that happen — accepting an "INBOX" candidate
created a tag named "INBOX" with a folder rule mapping INBOX back to itself, which
went on to cause real duplicate mail (see the same-folder move guard below).

Scan scope is one of: every folder, just the inbox, or an explicitly hand-picked set
(via the folder-picker modal). The scan streams progress over SSE (same pattern as
`/api/search`) since it's a multi-folder, multi-IMAP-round-trip operation that can
take a while on a large mailbox — including a distinct "scoring N mail" phase once
every folder's been fetched, since that step alone can take a noticeable while on a
large mailbox with nothing else to show progress.

## Auto-move by tag — the opportunistic timer

Once a tag is known to map to a folder (`folder_tag_rules`, the same table the
folder→tag rule already uses — and now, since multiple accounts can each have their
own destination for the same tag, scoped per-account throughout), mail carrying that
tag that's sat in the inbox past `owner_settings.auto_move_delay_days` (default 3)
gets moved there automatically. This reads `folder_tag_rules` in the *other*
direction — no new mapping concept to learn.

The delay is measured from `mails.sent_at` (how long the mail has actually sat in the
inbox), not from `tag_history.created_at` (when it got tagged) — those are very
different clocks for mail tagged well after it arrived. A bulk scan or "accept all"
over a backlog tags everything *today*, regardless of how old the underlying mail is;
measuring from the tag event meant a multi-day delay could never fire on backlog mail
at all, since it was always "tagged just now" by the time anyone checked.

**This is not a real timer.** There's no cron/background-job infrastructure in this
app — only the IMAP IDLE event loop. `autoMoveTaggedMail` is called opportunistically
from `syncAccount` (every IDLE wake, manual pull-to-refresh, account sync), from
accepting a suggestion, and from manual tagging — plus on demand via "Move tagged mail
now" in Smart Tagging settings, which shows exactly what it moved (sender, subject,
tag, destination), not just a bare count. In practice that's frequent enough to feel
timely, but the actual move happens "the next time something causes a sync for that
account after the delay has elapsed," not at the exact moment the delay expires.

Because it's called from so many uncoordinated places, `autoMoveTaggedMail` is guarded
by `Store.autoMoveMu` so only one run executes at a time — two overlapping runs could
otherwise both select the same candidate mail, and whichever lost the race found it
already moved by the time it tried, undercounting (this is what made the manual
button occasionally report "nothing to move" while a concurrent sync was moving
something at that exact moment).

If a tag maps to more than one folder for an account, auto-move skips it rather than
guessing which one — and it never targets INBOX as a destination, full stop. Moving
mail to the folder it's already sitting in isn't a no-op on real IMAP servers: it's
typically implemented as copy-then-delete, so it silently *duplicates* the message
every time it runs. A bogus folder rule mapping a tag back to its own source folder
(the same default-naming bug behind the INBOX-candidate issue above) caused exactly
this once, in production, producing several genuinely duplicate copies of the same
mail on the server. `moveMailToFolder` now no-ops on a same-folder move regardless of
caller or reason, as a second, unconditional layer of protection on top of the
INBOX-destination check.

## Verifying this is working

- `tag_history` directly: `SELECT source, status, count(*) FROM tag_history GROUP BY 1, 2;`
- An account's first-ever sync should produce zero smart-tagging activity (the
  `last_seen_uid` watermark is `NULL` until the first sync sets it, so nothing counts
  as "new" yet) — this is intentional, not a bug, and avoids flooding suggestions
  against a whole pre-existing inbox the moment an account is added.
