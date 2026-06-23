# Folder browsing: DB-backed, not live IMAP

Browsing a folder (anything other than the inbox) is backed by the database by default
— **no IMAP round trip on a normal page load.** This wasn't always true; it's documented
here because the wrong version (live-fetch-on-every-load) is an easy regression to
reintroduce by accident, and it shipped that way for a while before being caught.

## The three layers

1. **Client-side cache** (`folderMailCache`, `web/js/inbox.js`) — an in-memory `Map`,
   persisted to `sessionStorage` (`sift_folder_mail_cache`, capped at
   `FOLDER_MAIL_CACHE_LIMIT = 10` folders). Shown instantly on open, before any network
   call. Survives a reload within the same browser session.
2. **Server-side DB cache** — the `mails` table itself. `handleFolderMails`
   (`internal/api/accounts.go`) serves from here by default via `cachedFolderMails`,
   cursor-paginated the same way the inbox list already is (see below). This is the
   layer that makes "no IMAP round trip" actually true, not just deferred — without it,
   every client cache miss (or every revisit past the client cache's lifetime) would
   still hit IMAP live.
3. **IMAP itself** — only reached two ways: the periodic background sync
   (`watchFolderMailSync`, below), or an explicit `live=1` request parameter, sent only
   by the real pull-to-refresh gesture (`refreshMails()` in `inbox.js`).

## `watchFolderMailSync` — the piece that makes layer 2 actually fresh

`syncAccount`'s IMAP IDLE watch only ever covers INBOX. Before this existed, every other
folder's DB copy only ever got populated reactively — whenever a user happened to browse
it live, or one of the scan features (Smart Tagging scan, spam scan, image backfill)
incidentally walked it. A folder nobody had opened in a while, or ever, just sat stale or
empty in the DB forever.

`watchFolderMailSync` (`internal/api/accounts.go`) is a real periodic ticker — every
`folderCheckInterval()` (same 2-hour interval `watchFolderChanges` uses for folder
*structure*, env-overridable via `FOLDER_CHECK_INTERVAL_HOURS`), and once immediately on
startup — that walks every folder of every account via the existing `walkAccountFolders`
helper (the same one Smart Tagging's scan and the image-cache backfill share) and upserts
whatever it finds, up to `folderMailSyncLimit = 200` mails per folder per cycle. Started
alongside the other background watchers in `realtime.go`'s `StartWatching`.

## The explicit "go live" override

`handleFolderMails` checks `live=1` and, if set, does the old live-IMAP-fetch-and-upsert
path instead of reading the DB. The **only** caller that sets it is `refreshMails()` —
the actual pull-to-refresh gesture. Automatic refresh triggers (the SSE "mail" push,
`visibilitychange` regaining focus) call `fetchMails(force)` *without* `live` — they skip
the stale in-memory client cache and re-read the DB, but still never touch IMAP. If a
"folder loading is slow again" report comes up, check whether something started passing
`live` where it shouldn't, or whether a caller bypassed `handleFolderMails` entirely.

The "Manage folders" screen and the topbar folder-structure browser have their own,
separate `force=1` exception for folder *structure* (`handleListFolders`) — opening that
screen is the one place actively editing folders where genuinely current state matters
more than cache freshness. That's a different cache from this doc (folder *names*, not
folder *mail content*) — see `fetchFolders`/`folderCache` in `web/js/folders.js`.

## Pagination: date+id cursor, never IMAP UID

`loadMore` (`inbox.js`) and `cachedFolderMails` (`accounts.go`) page folders the same way
the inbox already pages — a `(sent_at, id) < (cursor_date, cursor_id)` tuple comparison,
immune to a new mail landing mid-scroll shifting every row's offset.

This *replaced* IMAP-UID-based paging (`beforeUid`), which is still what the `live=1`
path uses internally against IMAP directly (`fetchFolderMailPage`, `imap.go`) — there's
no date-cursor equivalent for a live IMAP fetch, only for the DB. UID-based paging has a
real correctness bug: **UID order only tracks Date order for mail delivered straight into
a folder.** Mail that arrived by being *moved* — auto-move, a manual move, a
`folder_tag_rules` move-in — gets a fresh UID at move time, completely unrelated to its
own Date header. Junk is the worst case, being mostly moved-in mail: paging "before UID
X" against it produced exactly the bug reported as "emails skip months" / mail missing
further down the list — older-by-date mail with a *higher* UID (because it was moved in
recently) than the page boundary just never got reached.

`fetchFolderMailPage`'s live path mitigates this (it still has to page by UID against
IMAP) by over-fetching a wider window than the page size —
`folderPageOverfetchFactor = 4`× the limit, capped at `folderPageOverfetchCap = 400` — and
sorting by date before slicing to the actual page. Not a perfect fix for a pathological
amount of out-of-order moved mail in one folder, but correct for any realistic case, and
documented as a known, bounded tradeoff rather than an adaptive widen-until-correct loop.

## Restoring scroll depth without blocking the first paint

`catchUpToDepth` (`inbox.js`) re-pages a freshly-fetched single page back up to however
deep a folder was previously scrolled, so revisiting a folder doesn't visibly "forget"
everything past page 1. Two things about this worth knowing:

- It used to block the *first* render until the entire catch-up chain finished — meaning
  a deeply-scrolled folder showed nothing but a loading skeleton for several sequential
  round trips. `fetchMails` now renders immediately after the first page lands, and lets
  catch-up continue (and re-render again once done) in the background.
- `persistFolderMailCache()` (the `sessionStorage` write) does a full `JSON.stringify` of
  *every* cached folder, not just the one being updated. It used to run after every
  single page during catch-up — real, measurable CPU cost on a phone, stacked on top of
  the round trips themselves, for no benefit (only the final state matters for reuse
  later). `loadMore`'s `skipRender` parameter (true only when called from
  `catchUpToDepth`'s own loop, never from real user-driven scrolling) now also gates this
  — it persists once, at the end of the whole chain, not once per page.

## Known gotcha: invalid UTF-8 breaks caching for a whole folder, silently

Some real-world senders emit raw 8-bit header bytes (e.g. a sender display name with a
Latin-1/Windows-1252 byte, no RFC 2047 encoded-word at all) that `go-imap` passes through
as-is. Postgres rejects invalid UTF-8 outright — not mangled text, the entire `INSERT`
errors, and that mail (and silently, every other mail batched in the same upsert) never
gets cached at all. `mailsFromFetch` (`imap.go`) sanitizes Sender/SenderEmail/Subject via
`toValidUTF8` (`strings.ToValidUTF8`) before they ever reach the DB layer. This was found
via `watchFolderMailSync` touching far more real historical mail than anything had
before — if a similarly-shaped Postgres encoding error shows up again, suspect the same
class of cause (raw IMAP envelope text) before anything else.
