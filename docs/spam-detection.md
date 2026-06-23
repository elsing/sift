# Spam detection

A heuristic spam engine, deliberately separate from Smart Tagging (`docs/smart-tagging.md`)
even though both share the same underlying `tag_history`/`message_tags`/`folder_tag_rules`
plumbing. The implementation lives in `internal/api/spam.go`, with a few touch points in
`imageproxy.go` (the live new-mail path) and `mails.go` (the reader's read-only score
readout).

## Why not just reuse the tagging-history scorer

Smart Tagging's scorer (`scoreTagsForMail`) answers "does this sender/domain/subject
match *your own* history of tagging decisions" — which is exactly backwards for spam: a
brand-new spammer has no history to match against, by definition. Spam detection needs a
scorer that works against a sender with **zero** prior history. `scoreSpam` is that
scorer — authentication checks, sender/reply-to mismatches, display-name spoofing, and
content heuristics, none of which depend on having seen this sender before. The tagging-
history mechanism still contributes, but only as a *secondary* reinforcing signal (a
sender already flagged spam before scores a little higher next time) — never the
foundation.

## The scoring signals (`scoreSpam`, `internal/api/spam.go`)

Each signal adds a weight to a running total, capped at 1.0. The full reason list is
stored alongside the score (`spam_flags.reasons`) and shown verbatim in the UI — nothing
is summarized away into a bare count.

**Authentication** (`Authentication-Results` header, parsed by `authResultVerdict`):
- SPF/DKIM absent (`none`): +0.15 each. DMARC absent: +0.3 — weighted higher than
  SPF/DKIM, since DMARC adoption among legitimate senders is high enough today that its
  absence is a meaningfully stronger tell on its own.
- Any of the three explicitly failing: +0.35 each.
- None of SPF/DKIM/DMARC passing at all: +0.3 combined, on top of the individual
  penalties above — legitimate mail today almost always passes at least one.

**Sender mismatches:**
- Reply-To domain ≠ From domain: +0.3, or only +0.1 if all three auth checks passed.
- Return-Path domain ≠ From domain: +0.2, or only +0.05 if all three auth checks passed.
- Weighted down when auth is clean because a domain mismatch alone is normal for
  legitimate bulk/transactional mail relayed through a third-party ESP (SES, SendGrid,
  etc. all rewrite Return-Path for their own bounce handling) — a properly authenticated
  sender using a relay is the expected case, not a red flag.

**Display-name spoofing**: From display name contains a well-known brand/bank word
(`spamBrandWords` — PayPal, Amazon, banks, etc.) while the actual sending domain doesn't
match that brand at all: +0.4.

**Content heuristics** (subject + text body, falling back to HTML stripped of tags):
- Static spam-phrase list (`spamPhrases`) — `min(0.5, 0.15 × matched-phrase-count)`, with
  every matched phrase named explicitly in the reason, not just a count.
- Subject mostly capital letters (>60% of letters, subject longer than 8 chars): +0.15.
- Link anchor text that itself looks like a domain, pointing somewhere else
  (`anchorMismatch`) — e.g. text says "paypal.com" but the link goes elsewhere: +0.3.
  Skips `mailto:`/non-`http(s)` links entirely — a "Contact us" link whose text is an
  org name and href is a `mailto:` address is not a spoofed destination.
- Link to a bare IP address literal: +0.25.
- Link through a known URL shortener domain (`shortenerLink`): +0.2 — a SpamAssassin
  `URI_SHORTENER`-style signal.
- `!!!`/`$$$`-style punctuation runs (`punctSpamRun`, 3+ repeats): +0.15.
- HTML-only with no plain-text alternative: +0.05 (weak alone — plenty of legitimate
  mail skips the text part too).
- Suspicious auto-generated-looking domain shape (`suspiciousDomainPattern` — a brand
  word jammed next to "secure"/"verify"/etc. with hyphens): +0.2.

**History reinforcement** (secondary): `max(senderRatio, domainRatio)` against the Spam
tag specifically (same `ratio` formula Smart Tagging uses), contributing `hist × 0.3` —
only kicks in above a small floor (0.1), and is zero for a sender with no prior history,
which is the entire gap this exists to cover for everyone else.

## Thresholds

Separate constants from Smart Tagging's (`spamAutoApplyScore = 0.75`,
`spamSuggestScore = 0.4`) — kept distinct on purpose even though the numbers match,
because a spam false positive burying real mail is a worse failure mode than a
mis-suggested regular tag, and the two systems' weights are tuned independently.

**The mode gate is always `mode == "full_auto" && score >= spamAutoApplyScore`** (an AND,
never an OR). This shipped backwards twice during development — `score >= threshold ||
mode == "full_auto"` let a high-confidence score silently auto-apply even in Review mode.
If "applied without confirming" is ever reported again, check this exact condition
first, in both `imageproxy.go` (live path) and `spam.go` (scan path).

## Where scoring actually runs

Three places, and *only* three — opening a mail never triggers scoring:

1. **`prefetchMailImages` (`imageproxy.go`)** — every newly-synced mail gets scored once,
   piggybacking on the body fetch that already happens there for image prefetching (no
   second IMAP round trip).
2. **`scanAccountForSpam` (`spam.go`)** — the manual "Scan for spam" action in the Smart
   Spam panel, scoped like Smart Tagging's own scan (all folders / inbox only / a
   hand-picked set via the shared folder-sheet modal).
3. Nowhere else. `handleMailBody` (`mails.go`) — what the reader calls when you open a
   mail — only ever *reads* the last stored `spam_flags` row for that message; it never
   calls `scoreSpam` itself. A mail that's never been touched by sync or a scan shows
   "Not yet scanned" rather than scoring on the spot.

Every scored mail gets a `spam_flags` row regardless of whether it clears the suggest
threshold — a low-scoring mail still has something to show in the reader's bottom-of-mail
readout, not just the ones that got flagged.

## Junk/Trash during a scan: evidence, not a bypass

Mail already sitting in Junk or Trash during a scan still gets fetched and actually
scored (real SPF/DKIM/DMARC, real content checks) — it does **not** get blindly marked
spam without going through the engine. Being in Junk/Trash just forces the *effective*
score up to at least `spamAutoApplyScore` (treated as strong evidence, since the server
or a past manual move already sorted it there) before the same suggest/apply-by-mode
gate above runs. In Review mode that still means a suggestion, not a silent apply — this
was a real bug during development (Junk-folder mail bypassing the mode gate entirely)
and is now deliberately *not* a special case.

## Images: never auto-load, never cache, regardless of the global setting

Two independent rules, both keyed off whether the *current* message carries the Spam tag
(checked fresh per-request, not cached):

- `handleMailBody` sets `isSpam` on the response; `reader.js` checks it ahead of the
  global "auto-load images" setting and ahead of a normally-trusted sender — spam always
  falls through to the "tap to load" prompt.
- If the user taps to load anyway, the request still goes through the image proxy
  (`handleImageProxy`, `imageproxy.go`) — never a raw direct fetch — but
  `cachedOrFetchImage`'s `cacheable` parameter is `false` for spam, so the fetched bytes
  are never written to `image_cache`. The check is by message ID (`mid` query param) at
  request time, so removing the Spam tag ("Not spam") immediately restores normal
  auto-load and caching with no migration or backfill needed.

## Junk folder detection and auto-move

`detectSpecialUseFolders` (`internal/api/imap.go`) detects Junk the same way it already
detected Archive/Trash — the `\Junk` RFC 6154 attribute, falling back to fuzzy name
matching ("junk", "spam", "bulk"). Cached in `accounts.junk_folder`, lazily detected and
backfilled via `resolveFolders`, same pattern as the existing archive/trash columns.

The Spam tag is created with `instant_move = true` (`getOrCreateSpamTag`,
`imageproxy.go`) — it skips the normal `auto_move_delay_days` wait entirely, since spam
should move promptly once decided, not sit in the inbox for days. `ensureSpamFolderRule`
creates the Spam→Junk `folder_tag_rules` mapping **proactively**, the moment the tag
exists — not just inside the auto-apply branch. That distinction matters: in Review mode
nothing is ever auto-applied, so if the rule were only created on auto-apply, accepting a
suggestion by hand later would find no destination folder to move to at all. This was a
real gap during development.

Auto-move itself reuses Smart Tagging's `autoMoveTaggedMail` unchanged — no
spam-specific move code. See `docs/smart-tagging.md` for how that mechanism works,
including `watchAutoMove` (`smarttags.go`), the periodic ticker (every
`AUTO_MOVE_CHECK_INTERVAL_MINUTES`, default 10 min, also fires once immediately on
startup) that exists specifically so an `instant_move` tag like Spam doesn't have to wait
for unrelated IMAP/sync activity to get swept — it's no longer purely opportunistic for
that case.

## UI: a separate menu, deliberately

Smart Spam (`web/js/spam.js`, the "🚫 Smart spam" Settings entry) is its own panel, not a
tab inside Smart Tagging — explicit product decision: spam is its own concept the user
thinks about independently, even though it shares `tag_history` plumbing under the hood.
It has its own mode toggle (review/full_auto, `owner_settings.spam_mode`), its own scan
scope picker (mirrors Smart Tagging's: all folders / inbox only / choose folders, via the
same shared folder-sheet modal), and its own suggestions queue — **grouped by sender**,
not by tag (there's only ever one tag, Spam, so grouping by tag would just be one giant
group; sender is the axis that actually splits a bulk-spam-run-from-one-sender pile into
something scannable).

**Auto activity** (Settings → Auto activity, `web/js/autoTagActivity.js`) is the shared
full-auto audit/undo surface for *both* systems — a Tagging/Spam toggle switches which
`tag_history` source it reads, rather than two separate panels. The spam side of that
view is also grouped by sender, same reasoning as the suggestions queue. Undo there reads
"Not spam" instead of "Undo" when viewing the spam source.

## Diagnostics: the bottom-of-mail readout

Every opened mail shows a small, always-present (not a toggle, not a button) section at
the bottom of the reader: the stored score, SPF/DKIM/DMARC verdicts, and the full reason
list — or "Not yet scanned" if `spam_flags` has no row for it yet. This is read-only by
design (see "where scoring actually runs" above) — opening the diagnostic view can never
itself change what's tagged or suggested. There's no separate UI flow to "go check a
specific email" — you reach it by opening the mail normally via the inbox or a folder,
the same as any other mail.

## Tables

- `spam_flags` (migration `0027`, `0028`, `0030`) — one row per scoring event:
  `message_id`, `owner_subject`, `score`, `reasons` (plain newline-joined `TEXT`, **not**
  a Postgres array — see the gotcha below), `spf`/`dkim`/`dmarc` verdicts, `created_at`.
  The reader reads the most recent row per message; nothing is ever updated in place.
- `tag_history.source` includes `'spam_engine'` alongside Smart Tagging's existing
  sources (migration `0027`).
- `owner_settings.spam_mode` (migration `0027`) — `review` (default) or `full_auto`,
  independent of `auto_tag_mode`.
- `accounts.junk_folder` (migration `0027`) — mirrors `archive_folder`/`trash_folder`.

**Gotcha worth keeping in mind**: `spam_flags.reasons` was originally a `TEXT[]`. The
pgx stdlib `database/sql` driver returns array columns as their raw string literal by
default rather than decoding into a Go `[]string` — every read silently failed
(`unsupported Scan, storing driver.Value type string into type *[]string`), which is
exactly why mail kept showing "Not yet scanned" despite scans completing successfully.
Migration `0030` converts it to plain `TEXT`, newline-joined/split in Go. Prefer plain
`TEXT` over a Postgres array column for anything read back through this driver unless
there's a strong reason not to.

## The Spam tag can't be deleted

`handleDeleteTag` (`tags.go`) explicitly blocks deleting a tag named "Spam" — it's
load-bearing for image gating, the folder move rule, and the Smart Spam panel's own
queries, all of which key off a tag literally named "Spam". Renaming is unrestricted if
the wording isn't wanted; deleting isn't.
