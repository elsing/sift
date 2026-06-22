// Core inbox: fetching/rendering mail, swipe gestures, pull-to-refresh, and switching
// between the inbox and a browsed folder (same list + gestures either way).
//
// ponytail: native touch events handle swipe + rubber-band; no gesture library needed.
// touch events (not Pointer Events) chosen deliberately: they stay locked to their original
// target for the whole gesture in iOS standalone/PWA mode, where Pointer Events + setPointerCapture
// have known quirks (gesture randomly reverts to native scroll mid-drag).
import { dryRunHeaders } from './util.js';
import { renderTagChips, setupFolderAutoTagSelect } from './tags.js';
import { moveRowToFolder } from './folders.js';

const EDGE_GUARD = 20;     // px from screen edge where swipe is ignored, avoids iOS back/app-switcher gesture
const COMMIT_RATIO = 0.3;  // fraction of row width dragged at release-time to commit; below this, releasing cancels
export const PAGE_SIZE = 30;

export let mails = [];
let hasMore = true;
let loadingMore = false;

// null when viewing the inbox; { accountId, path, name } when browsing a folder.
export let currentFolder = null;

// accountId -> trash folder path, fetched once and cached — lets a row in Trash offer
// a one-tap "Restore" instead of the full move-to-folder picker. Best-effort: an
// account whose trash folder hasn't been detected yet (no archive/delete done, no
// fresh-enough account-add) just never shows the button, rather than blocking on it.
let trashFolderByAccount = null;
async function loadTrashFolders() {
  if (trashFolderByAccount) return trashFolderByAccount;
  trashFolderByAccount = {};
  try {
    const res = await fetch('/api/accounts');
    if (res.ok) {
      for (const a of await res.json()) {
        if (a.trashFolder) trashFolderByAccount[a.id] = a.trashFolder;
      }
    }
  } catch {
    // best-effort — worst case the Restore button just doesn't show up this time
  }
  return trashFolderByAccount;
}

// null normally; { tag, mails } when "stepped into" a tag group from the inbox — same
// idea as currentFolder, just for a group instead of an IMAP folder.
let currentGroup = null;

// null means "all accounts" (the common case, and the only case with one account).
// Setting it restricts the inbox to a chosen subset — one account, or any mix.
let accountFilterIds = null;

// Called by the account-filter chip strip when the selection changes. Clears the
// list immediately (same pattern as switching folders) rather than leaving stale
// rows up while the new fetch is in flight.
export function applyAccountFilter(ids) {
  accountFilterIds = ids && ids.length ? ids : null;
  mails = [];
  renderLoadingSkeleton();
  fetchMails().then(render);
}
function accountsQueryParam(prefix) {
  return accountFilterIds ? `${prefix}accounts=${accountFilterIds.map(encodeURIComponent).join(',')}` : '';
}

// Swipe actions are config-driven so a future settings UI just needs to write new
// values to localStorage — no gesture/dispatch code to touch. 'move' is special: it
// doesn't commit immediately, it asks the injected onMoveRequested to open the folder picker.
const SWIPE_ACTIONS = {
  archive: { label: 'Archive', colorVar: '--archive' },
  delete:  { label: 'Delete',  colorVar: '--delete' },
  move:    { label: 'Move',    colorVar: '--move' },
  read:    { label: 'Mark read', colorVar: '--accent' },
  tag:     { label: 'Tag',    colorVar: '--accent' },
};

function getSwipeConfig() {
  return {
    left: localStorage.getItem('swipeLeft') || 'delete',
    right: localStorage.getItem('swipeRight') || 'move',
  };
}

// Wired up once by main.js. Keeps this module from importing folders.js/reader.js
// directly, which would otherwise create a circular module dependency (those modules
// import mail-list helpers from here).
let onMoveRequested = () => {};
let onRowTapped = () => {};
let onTagRequested = () => {};
export function setHandlers({ onMove, onTap, onTag }) {
  onMoveRequested = onMove;
  onRowTapped = onTap;
  onTagRequested = onTag;
}

// Folder mail content wasn't cached at all — every open/reopen of the same folder
// always blocked on a live IMAP fetch. Same cache-then-network pattern already used
// for the folder tree itself: show the last-known list instantly, then quietly
// refetch and replace it. force=true (pull-to-refresh) skips straight to network.
const folderMailCache = new Map(); // "accountId|path" -> mails array

// Keeps `mails` itself newest-first, not just the rendered display order — buildDisplayItems
// sorts its own derived list for display, but the underlying array was never sorted to
// match. loadMore's cursor (mails[mails.length - 1]) assumes the oldest-loaded mail is
// last in the array; that's true for the inbox (the server already returns it
// newest-first), but IMAP's folder fetch returns whatever sequence-number order it was
// asked for (oldest-first for "most recent N") — so without this, loadMore was reading
// the cursor off the *newest* mail in a folder, not the oldest, and pagination either
// looped on the same page or looked like it had reached the end when it hadn't.
function sortMailsNewestFirst() {
  mails = mails.slice().sort((a, b) => (a.date < b.date ? 1 : -1));
}

// A manual pull-to-refresh and the IDLE-driven SSE listener (setupLiveUpdates) can
// both end up fetching at nearly the same moment — a refresh you triggered yourself
// kicks off a real IMAP sync, and the IDLE watcher can independently notice new mail
// and broadcast right in the middle of it. Whichever of the two overlapping requests
// happens to resolve *second* would otherwise overwrite the other's already-painted,
// newer result with its own now-stale snapshot — which is exactly what looked like tag
// groups breaking apart and then flashing back: two real, correct renders, just out of
// order. This counter makes sure only the most recently-started fetch is ever allowed
// to actually paint.
let fetchGeneration = 0;

// A swipe commit (delete's flick, restore, move) animates a specific row entirely
// client-side — but render() rebuilds the *whole* list from scratch, and a live
// update arriving from the server mid-animation used to trigger exactly that rebuild,
// yanking the animating row out of the DOM before its own transition finished. That's
// what made the delete flourish inconsistent — sometimes cut off entirely, sometimes
// (if the rebuild landed between the flash and the slide) looking like it "hung".
// Swipe animations bump this counter for their duration; setupLiveUpdates' SSE
// handler waits for it to hit zero before rebuilding anything, so a local animation
// always gets to play out the same way regardless of what the server happens to be
// doing at that exact moment.
let activeRowAnimations = 0;
export function beginRowAnimation() { activeRowAnimations++; }
export function endRowAnimation() { activeRowAnimations = Math.max(0, activeRowAnimations - 1); }

export async function fetchMails(force) {
  if (currentFolder) {
    const cacheKey = currentFolder.accountId + '|' + currentFolder.path;
    const cached = !force && folderMailCache.get(cacheKey);
    if (cached) {
      mails = cached;
      hasMore = mails.length === PAGE_SIZE; // matches loadMore's own heuristic below
      render();
    }
    const myGen = ++fetchGeneration;
    const res = await fetch(`/api/accounts/${currentFolder.accountId}/folder-mails?path=${encodeURIComponent(currentFolder.path)}`);
    if (!res.ok) {
      const msg = await res.text();
      if (cached) return; // keep showing what we had rather than blow it away on a failed refresh
      throw new Error(msg || 'failed to load folder');
    }
    const data = await res.json();
    if (myGen !== fetchGeneration) return; // a newer fetch started while this one was in flight
    mails = data;
    sortMailsNewestFirst();
    folderMailCache.set(cacheKey, mails);
    hasMore = mails.length === PAGE_SIZE;
    return;
  }
  const targetCount = Math.max(mails.length, PAGE_SIZE * 2);
  const myGen = ++fetchGeneration;
  const res = await fetch(`/api/mails?${accountsQueryParam('')}`);
  const data = await res.json();
  if (myGen !== fetchGeneration) return;
  mails = data;
  hasMore = mails.length === PAGE_SIZE;
  // Same reasoning as refreshMails below: a brand-new mail at the very top can share a
  // tag with something that's always just past page 1 — this isn't only a refresh
  // problem, the plain initial page load (main.js's startup fetchMails().then(render))
  // hits exactly the same one-page-deep gap every single time, not just after a pull
  // to refresh.
  await catchUpToDepth(targetCount);
}

// Loads additional pages (via the existing loadMore) until `mails` reaches at least
// targetCount or there's nothing more to load. Shared by fetchMails (initial load)
// and refreshMails (pull-to-refresh) — both reset/start from a single page and both
// need the same catch-up so tag grouping has enough loaded to actually find a group's
// other members instead of showing brand-new mail ungrouped until a manual scroll
// happens to trigger loadMore on its own.
async function catchUpToDepth(targetCount) {
  // hasMore alone isn't a safe loop condition — if a page comes back full of mail
  // already in `mails` (a duplicate page, a stuck cursor, a generation-race no-op),
  // the cursor never advances and the exact same request repeats forever. Bail the
  // moment a round trip doesn't actually grow the list, instead of spinning the tab
  // into the ground (this is what was crashing the browser).
  let lastLength = mails.length;
  while (mails.length < targetCount && hasMore) {
    // skipRender: each page here would otherwise call its own full render(), visibly
    // growing a tag group's count page by page (e.g. "7" then a beat later "18") as
    // catch-up runs — the caller renders once after this loop finishes instead.
    await loadMore(true);
    if (mails.length === lastLength) break;
    lastLength = mails.length;
  }
}

export async function refreshMails() {
  if (currentFolder) return fetchMails(true); // force network, skip the cached flash
  // Always catch up to at least 2 pages, not just whatever depth was there before —
  // a brand-new mail at the very top, sharing a tag with something that's always
  // just past page 1, would otherwise show up ungrouped every single refresh (not
  // just after scrolling first): the group's other member genuinely isn't loaded yet
  // at exactly one page deep, restored-previous-depth or not.
  const targetCount = Math.max(mails.length, PAGE_SIZE * 2);
  const myGen = ++fetchGeneration;
  const res = await fetch(`/api/mails/refresh?${accountsQueryParam('')}`, { method: 'POST' });
  const data = await res.json();
  if (myGen !== fetchGeneration) return;
  mails = data;
  hasMore = mails.length === PAGE_SIZE;
  // A refresh re-fetches just the first page — without this, scrolling further than
  // one page before refreshing silently lost everything past it, which (among other
  // things) could drop a tag group below its 2-member threshold and visibly break it
  // apart, only for it to reappear once loadMore brought the rest back on its own.
  // Restore at least that same depth here instead of waiting for that to happen by
  // accident.
  await catchUpToDepth(targetCount);
}

export async function loadMore(skipRender = false) {
  if (loadingMore || !hasMore || mails.length === 0) return;
  loadingMore = true;
  const myGen = ++fetchGeneration; // same race a concurrent refresh could otherwise cause — see fetchGeneration's own comment
  const oldest = mails[mails.length - 1];
  let more;
  if (currentFolder) {
    // IMAP has no date-cursor equivalent here — page by UID instead (also
    // monotonic, same idea): fetch the next batch with UID below the oldest one
    // already loaded.
    const params = `path=${encodeURIComponent(currentFolder.path)}&beforeUid=${oldest.uid || ''}`;
    const res = await fetch(`/api/accounts/${currentFolder.accountId}/folder-mails?${params}`);
    more = res.ok ? await res.json() : [];
  } else {
    // Paged by cursor ("older than the last mail you've already got"), not by row
    // count — an offset shifts under you the instant the IMAP IDLE watcher inserts
    // new mail mid-scroll, which silently repeated or skipped a row. A date+id
    // cursor doesn't care how many rows exist elsewhere, only where the last-seen
    // mail sits.
    const params = `before=${encodeURIComponent(oldest.date || '')}&beforeId=${encodeURIComponent(oldest.id)}${accountsQueryParam('&')}`;
    const res = await fetch(`/api/mails?${params}`);
    more = await res.json();
  }
  if (myGen !== fetchGeneration) { loadingMore = false; return; } // a refresh replaced `mails` while this page was in flight — concatenating onto it now would be wrong
  hasMore = more.length === PAGE_SIZE;
  const seenIds = new Set(mails.map((m) => m.id));
  mails = mails.concat(more.filter((m) => !seenIds.has(m.id)));
  if (currentFolder) {
    sortMailsNewestFirst(); // folder pages come back oldest-first; keep the invariant loadMore's cursor relies on
    folderMailCache.set(currentFolder.accountId + '|' + currentFolder.path, mails);
  }
  if (!skipRender) {
    // full re-render rather than appending incrementally: newly-loaded older mail can
    // join an existing tag group (growing its count) instead of always starting a new
    // one, since groups aren't a simple append-only list — preserve scroll position
    // since render() rebuilds the list from scratch.
    const scrollY = window.scrollY;
    render();
    window.scrollTo(0, scrollY);
  }
  loadingMore = false;
}

// Scrolling to the bottom with nothing more to load (backfill has reached the oldest
// message in the mailbox) used to just silently do nothing, which looked identical to
// "broken". Make that state visible instead.
function updateEndOfListFooter() {
  const list = document.getElementById('inbox');
  const existing = document.getElementById('endOfListFooter');
  if (existing) existing.remove();
  // A short inbox/folder that fits on screen with room to spare has nothing to
  // scroll to in the first place — stating "you've reached the end" right away,
  // with no scrolling having happened, read as presumptuous rather than informative.
  const scrollable = document.body.scrollHeight > window.innerHeight;
  if (!hasMore && mails.length > 0 && scrollable) {
    const li = document.createElement('li');
    li.id = 'endOfListFooter';
    li.className = 'end-of-list-footer';
    li.textContent = currentFolder ? "📭 You've reached the end of this folder" : "📭 You've reached the end of your inbox";
    list.appendChild(li);
  }
}

// Today / This Week / Last Week / Older, based on calendar-day difference from now —
// "Older" only within the current year. Anything from a previous year gets that year
// as its own section instead of disappearing into one undifferentiated "Older" bucket
// (which a folder full of years of archived mail otherwise scrolls through with zero
// indication of which year you're looking at).
function dateSection(iso) {
  if (!iso) return 'Older';
  const d = new Date(iso);
  if (isNaN(d)) return 'Older';
  const now = new Date();
  const startOfDay = (date) => new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const daysAgo = Math.round((startOfDay(now) - startOfDay(d)) / 86400000);
  if (daysAgo <= 0) return 'Today';
  if (daysAgo <= 6) return 'This Week';
  if (daysAgo <= 13) return 'Last Week';
  if (d.getFullYear() === now.getFullYear()) return 'Older';
  return String(d.getFullYear());
}

// Tracks the last section header written, across both a full render() and incremental
// loadMore() appends, so headers only appear once per transition, not once per row.
let lastSection = null;

function appendDisplayItem(list, item) {
  const section = dateSection(item.date);
  if (section !== lastSection) {
    const header = document.createElement('li');
    header.className = 'date-section-header';
    header.textContent = section;
    list.appendChild(header);
    lastSection = section;
  }
  list.appendChild(item.type === 'group' ? renderTagGroupRow(item.tag, item.mails) : renderRow(item.mail));
}

// A handful of shimmering placeholder rows, not just an empty list — shown the moment
// a folder/inbox switch clears the old rows, replaced as soon as the real fetch lands.
export function renderLoadingSkeleton() {
  const list = document.getElementById('inbox');
  list.innerHTML = '';
  for (let i = 0; i < 8; i++) {
    const row = document.createElement('li');
    row.className = 'mail-skeleton-row';
    row.innerHTML = '<div class="skeleton-bar"></div><div class="skeleton-bar"></div>';
    list.appendChild(row);
  }
}

export function openFolder(accountId, path, name) {
  currentFolder = { accountId, path, name };
  lastRefreshAt = 0; // a different view now — don't carry over the inbox's cooldown
  document.getElementById('folderBannerName').textContent = name;
  document.getElementById('folderBanner').classList.remove('hidden');
  document.getElementById('accountsPanel').classList.add('hidden');
  setupFolderAutoTagSelect(
    document.getElementById('folderAutoTagSelect'),
    document.getElementById('applyTagToFolderBtn'),
    accountId, path,
  );
  window.scrollTo(0, 0);
  mails = [];
  renderLoadingSkeleton();
  // Best-effort and not awaited — if it resolves after the first render, re-render
  // once so a first-ever visit to Trash still gets its Restore buttons rather than
  // needing a second folder switch before they show up.
  loadTrashFolders().then(() => {
    if (currentFolder && currentFolder.accountId === accountId && currentFolder.path === path) render();
  });
  fetchMails().then(render).catch((err) => {
    console.error(err);
    const list = document.getElementById('inbox');
    list.innerHTML = '';
    const li = document.createElement('li');
    li.className = 'folder-empty-status';
    li.textContent = "Couldn't load this folder: " + err.message;
    list.appendChild(li);
  });
}

export function backToInbox() {
  currentFolder = null;
  lastRefreshAt = 0; // a different view now — don't carry over the folder's cooldown
  document.getElementById('folderBanner').classList.add('hidden');
  window.scrollTo(0, 0);
  mails = [];
  renderLoadingSkeleton();
  fetchMails().then(render).catch((err) => {
    console.error(err);
    const list = document.getElementById('inbox');
    list.innerHTML = '';
    const li = document.createElement('li');
    li.className = 'folder-empty-status';
    li.textContent = "Couldn't load the inbox: " + err.message;
    list.appendChild(li);
  });
}

export function setupFolderBanner() {
  document.getElementById('folderBannerBack').addEventListener('click', backToInbox);
}

// Live push: the server holds an SSE connection open and sends a "mail" event whenever
// an IMAP IDLE watcher sees new mail land. We just refresh the current view in response —
// browsers auto-reconnect EventSource on drop, so no retry/backoff logic needed here.
//
// ponytail: a reverse proxy/tunnel in front of the app can silently stall a long-lived
// connection without ever firing onerror, so SSE alone isn't fully trustworthy end-to-end.
// Backstop with a refresh whenever the tab/PWA regains focus — cheap, and catches anything
// SSE missed without needing real reconnect/backoff logic.
export function setupLiveUpdates() {
  const es = new EventSource('/api/events');
  es.addEventListener('mail', () => {
    // Force a real refetch in folder mode — the cache-then-network path would
    // otherwise flash the stale cached copy first, which is exactly what a live
    // update is trying to avoid showing.
    const run = () => fetchMails(!!currentFolder).then(render);
    // See activeRowAnimations' own comment — never rebuild the list out from under a
    // swipe animation that's still playing, just wait for it to finish first.
    const runWhenIdle = () => {
      if (activeRowAnimations > 0) { setTimeout(runWhenIdle, 100); return; }
      run();
    };
    runWhenIdle();
  });
  es.addEventListener('error', () => {
    console.warn('live updates: SSE connection error (browser will retry)', es.readyState);
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') fetchMails(!!currentFolder).then(render);
  });
}

export function setupInfiniteScroll() {
  let scheduled = false;
  window.addEventListener('scroll', () => {
    if (scheduled) return;
    scheduled = true;
    requestAnimationFrame(() => {
      scheduled = false;
      if (window.innerHeight + window.scrollY >= document.body.scrollHeight - 300) {
        loadMore();
      }
    });
  }, { passive: true });
}

// A genuinely empty folder/inbox used to just render a flat blank list (or, briefly,
// crash entirely — an empty IMAP folder's nil mail slice was encoding as JSON `null`,
// and folder-mode code unconditionally called .slice() on it). Worth celebrating
// instead of just stating: a few messages in rotation so it doesn't feel like the
// exact same screen every time you happen to clear something out.
const EMPTY_STATE_ICONS = ['🎉', '🌵', '🦗', '🍃', '🛸'];
const EMPTY_INBOX_MESSAGES = [
  "Inbox zero! Go touch some grass.",
  "Nothing here. Suspiciously peaceful.",
  "All caught up. Treat yourself.",
  "Empty! Even the tumbleweeds left.",
];
const EMPTY_FOLDER_MESSAGES = [
  "Nothing in here yet.",
  "Quiet in this folder.",
  "Empty — for now.",
];

function renderEmptyState(list) {
  const li = document.createElement('li');
  li.className = 'empty-state';
  const icon = document.createElement('div');
  icon.className = 'empty-state-icon';
  icon.textContent = EMPTY_STATE_ICONS[Math.floor(Math.random() * EMPTY_STATE_ICONS.length)];
  const text = document.createElement('div');
  text.className = 'empty-state-text';
  const messages = currentFolder ? EMPTY_FOLDER_MESSAGES : EMPTY_INBOX_MESSAGES;
  text.textContent = messages[Math.floor(Math.random() * messages.length)];
  li.append(icon, text);
  list.appendChild(li);
}

export function render() {
  const list = document.getElementById('inbox');
  list.innerHTML = '';
  lastSection = null;
  const items = buildDisplayItems(mails);
  if (items.length === 0) {
    renderEmptyState(list);
    return;
  }
  for (const item of items) appendDisplayItem(list, item);
  updateEndOfListFooter();
}

// Any mail sharing a tag with at least one other mail gets squashed into one group
// row instead of cluttering the list individually. A group sorts by its most recent
// member's date, same as it would if shown individually. Stepping into a group
// (currentGroup set) shows just its mails as a flat list — no further grouping, same
// idea as a folder itself.
//
// Grouping is skipped while browsing a folder, though — folders are routinely mapped
// to a single tag (folder_tag_rules), so most/all of a folder's mail tends to share
// that one tag, and the "group" would just be the entire folder collapsed into one
// row. Sorting still applies the same way either way; only grouping is inbox-only.
function buildDisplayItems(mailList) {
  if (currentFolder) {
    return mailList
      .map((mail) => ({ type: 'mail', mail, date: mail.date }))
      .sort((a, b) => (a.date < b.date ? 1 : -1));
  }
  if (currentGroup) return currentGroup.mails.map((mail) => ({ type: 'mail', mail, date: mail.date }));

  // A mail with several tags joins whichever of its own tags has the most mail
  // overall, not just whichever was applied first — tally every tag's total count
  // up front so that choice can actually be made.
  const tagCounts = new Map();
  for (const mail of mailList) {
    for (const t of mail.tags || []) tagCounts.set(t.id, (tagCounts.get(t.id) || 0) + 1);
  }

  const byTag = new Map(); // tagId -> { tag, mails: [] }
  for (const mail of mailList) {
    const tag = primaryTagFor(mail, tagCounts);
    if (!tag) continue;
    if (!byTag.has(tag.id)) byTag.set(tag.id, { tag, mails: [] });
    byTag.get(tag.id).mails.push(mail);
  }

  const grouped = new Set(); // mail ids absorbed into a group, not shown individually
  const items = [];
  for (const { tag, mails: tagMails } of byTag.values()) {
    // Two mails sharing a tag isn't enough to be worth collapsing into a group row —
    // it saves barely any space and just hides two individually-readable subjects
    // behind a generic "Tag (2)" row. Three+ is where grouping actually starts paying
    // for itself.
    if (tagMails.length < 3) continue;
    for (const m of tagMails) grouped.add(m.id);
    const date = tagMails.reduce((max, m) => (m.date > max ? m.date : max), tagMails[0].date);
    items.push({ type: 'group', tag, mails: tagMails, date });
  }
  for (const mail of mailList) {
    if (!grouped.has(mail.id)) items.push({ type: 'mail', mail, date: mail.date });
  }

  items.sort((a, b) => (a.date < b.date ? 1 : -1)); // newest first
  return items;
}

// Picks the tag (among a mail's own tags) with the most mail overall — joining the
// biggest group it's eligible for, rather than an arbitrary "first applied" one.
function primaryTagFor(mail, tagCounts) {
  if (!mail.tags || mail.tags.length === 0) return null;
  return mail.tags.reduce((best, t) => ((tagCounts.get(t.id) || 0) > (tagCounts.get(best.id) || 0) ? t : best));
}

function renderTagGroupRow(tag, groupMails) {
  const allRead = groupMails.every((m) => !m.unread);
  const li = document.createElement('li');
  li.className = 'row tag-group-row' + (allRead ? '' : ' unread');
  li.dataset.tagId = tag.id;
  // newest few subjects, comma-joined and clipped with an ellipsis to whatever
  // actually fits — not a fixed count, the CSS just clips the overflow
  const preview = groupMails
    .slice()
    .sort((a, b) => (a.date < b.date ? 1 : -1))
    .slice(0, 6)
    .map((m) => m.subject)
    .join(' • ');
  const content = document.createElement('div');
  content.className = 'row-content tag-group-content';
  content.innerHTML = `
    <div class="unread-dot"></div>
    <div class="tag-group-text">
      <span class="tag-group-badge" style="background:${tag.color}">${tag.name} (${groupMails.length})</span>
      <div class="tag-group-preview">${preview}</div>
    </div>
    <span class="tag-group-chevron">›</span>
  `;
  li.appendChild(content);
  content.addEventListener('click', () => openTagGroup(tag, groupMails));
  return li;
}

// Where the inbox was scrolled to right before stepping into a group, so coming back
// can return you there instead of always dumping you back at the top.
let savedInboxScrollY = 0;

// Stepping into a group works the same way openFolder does for a real IMAP folder:
// a banner with a way back, the list showing just that group's mails.
function openTagGroup(tag, groupMails) {
  savedInboxScrollY = window.scrollY;
  currentGroup = { tag, mails: groupMails };
  document.getElementById('tagGroupBannerName').textContent = tag.name;
  document.getElementById('tagGroupBanner').classList.remove('hidden');
  window.scrollTo(0, 0);
  render();
}

function closeTagGroup() {
  currentGroup = null;
  document.getElementById('tagGroupBanner').classList.add('hidden');
  render();
  window.scrollTo(0, savedInboxScrollY);
}

export function setupTagGroupBanner() {
  document.getElementById('tagGroupBannerBack').addEventListener('click', closeTagGroup);
}

export function removeMailById(id) {
  mails = mails.filter((m) => m.id !== id);
}

export function getMailById(id) {
  return mails.find((m) => m.id === id);
}

// Tag membership can change which group a mail belongs to (or create/dissolve one
// entirely), so this just re-renders rather than trying to patch one row's chips in
// place — simpler, and groups aren't a small patch away from a flat per-row update.
export function updateMailTags(id, tags) {
  const mail = mails.find((m) => m.id === id);
  if (mail) mail.tags = tags;
  if (!currentFolder) render();
}

function renderRow(mail) {
  const li = document.createElement('li');
  li.className = 'row' + (mail.unread ? ' unread' : '');
  li.dataset.id = mail.id;
  li.dataset.accountId = mail.accountId || '';
  const { left, right } = getSwipeConfig();
  const leftMeta = SWIPE_ACTIONS[left];
  const rightMeta = SWIPE_ACTIONS[right];
  li.innerHTML = `
    <div class="row-action right-swipe" style="background: var(${rightMeta.colorVar})">${rightMeta.label}</div>
    <div class="row-action left-swipe" style="background: var(${leftMeta.colorVar})">${leftMeta.label}</div>
    <div class="row-burst"></div>
    <div class="row-content">
      <div class="unread-dot"></div>
      <div class="row-text">
        <div class="sender">${mail.sender}</div>
        <div class="subject">${mail.subject}</div>
        <div class="snippet">${mail.snippet}</div>
      </div>
      <div class="timestamp">${mail.hasAttachments ? '<span class="attachment-clip" title="Has attachments">📎</span>' : ''}${mail.time}</div>
    </div>
  `;
  const chips = renderTagChips(mail.tags);
  if (chips) li.querySelector('.row-text').appendChild(chips);
  attachSwipe(li);

  // Browsing Trash specifically gets a direct one-tap Restore instead of needing the
  // full move-to-folder picker just to put something back exactly where it came from.
  if (currentFolder && trashFolderByAccount && trashFolderByAccount[currentFolder.accountId] === currentFolder.path) {
    const restoreBtn = document.createElement('button');
    restoreBtn.type = 'button';
    restoreBtn.className = 'restore-btn';
    restoreBtn.textContent = '↩️ Restore';
    restoreBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      li.dataset.moveDirection = 'right';
      moveRowToFolder(li, 'INBOX');
    });
    li.querySelector('.row-content').appendChild(restoreBtn);
  }
  return li;
}

function rubberBand(dx, elasticPoint) {
  // full-speed up to elasticPoint, diminishing returns past it
  const abs = Math.abs(dx);
  if (abs <= elasticPoint) return dx;
  const over = abs - elasticPoint;
  const damped = elasticPoint + over / (1 + over / elasticPoint);
  return dx > 0 ? damped : -damped;
}

function performSwipeAction(row, action, direction) {
  if (action === 'move') {
    if (!row.dataset.accountId) return; // mock mail has no real folders to move into
    onMoveRequested(row, direction);
    return;
  }
  if (action === 'read') {
    row.classList.toggle('unread');
    fetch(`/api/mails/${row.dataset.id}/read`, { method: 'POST', headers: dryRunHeaders() });
    return;
  }
  if (action === 'tag') {
    onTagRequested(row);
    return;
  }
  commit(row, action, direction);
}

// Capped px/frame the *rendered* position can move, independent of how fast the
// finger itself is actually moving. Without this, a fast flick delivers touchmove
// events with big jumps in moveX between them, and painting that raw value directly
// made the revealed action box and the row content visibly jump/snap between frames
// instead of sliding — "doesn't render very well" on a quick swipe specifically,
// where a slow deliberate drag (lots of small touchmove deltas) never showed it.
const MAX_RENDER_SPEED = 45;

function attachSwipe(row) {
  const content = row.querySelector('.row-content');
  const rightSwipeBox = row.querySelector('.row-action.right-swipe'); // revealed dragging right (dx > 0)
  const leftSwipeBox = row.querySelector('.row-action.left-swipe');   // revealed dragging left (dx < 0)
  // dx is the real, immediate finger position — used for the commit-threshold check,
  // which has to reflect where the finger actually is, not the eased catch-up below.
  // renderedDx is what's actually painted, easing toward dx at a capped rate so the
  // visuals stay smooth even when dx itself is jumping a lot between events.
  let startX = 0, startY = 0, dx = 0, renderedDx = 0, dragging = false, horizontal = false, rafScheduled = false;

  function paint() {
    rafScheduled = false;
    const diff = dx - renderedDx;
    const step = Math.max(-MAX_RENDER_SPEED, Math.min(MAX_RENDER_SPEED, diff));
    renderedDx += step;
    content.style.transform = `translateX(${renderedDx}px)`;
    rightSwipeBox.style.width = `${Math.max(renderedDx, 0)}px`;
    leftSwipeBox.style.width = `${Math.max(-renderedDx, 0)}px`;
    // Still catching up to a fast-moving target — keep stepping toward it every
    // frame even without a fresh touchmove, instead of stalling until the next one.
    if (renderedDx !== dx) schedulePaint();
  }

  function schedulePaint() {
    if (rafScheduled) return;
    rafScheduled = true;
    requestAnimationFrame(paint);
  }

  content.addEventListener('touchstart', (e) => {
    const t = e.touches[0];
    if (t.clientX < EDGE_GUARD || t.clientX > window.innerWidth - EDGE_GUARD) return;
    startX = t.clientX;
    startY = t.clientY;
    dragging = true;
    horizontal = false;
    content.style.transition = 'none';
    rightSwipeBox.style.transition = leftSwipeBox.style.transition = 'none';
  }, { passive: true });

  content.addEventListener('touchmove', (e) => {
    if (!dragging) return;
    const t = e.touches[0];
    const moveX = t.clientX - startX;
    const moveY = t.clientY - startY;
    if (!horizontal) {
      if (Math.abs(moveX) < 8 && Math.abs(moveY) < 8) {
        // Don't preventDefault here — CSS touch-action: pan-y already lets native
        // vertical scroll start immediately if that's what this turns out to be.
        // Calling preventDefault during this ambiguous window (even briefly) is what
        // made vertical scrolling feel like you had to "catch it right": iOS can
        // suppress the rest of a scroll gesture once you've preventDefault()'d any
        // part of it, even if you stop calling it once direction is known.
        return;
      }
      horizontal = Math.abs(moveX) > Math.abs(moveY);
      if (!horizontal) { dragging = false; return; }
    }
    e.preventDefault(); // confirmed horizontal — claim the gesture so the page doesn't scroll mid-swipe
    const max = row.clientWidth;
    dx = Math.max(-max, Math.min(max, moveX)); // track the finger 1:1, clamped so it can't fly off past the row
    schedulePaint();
  }, { passive: false });

  // Committing snaps straight to "fully revealed" (whichever action box fills the
  // whole row) rather than collapsing back to 0 first — the old code reset dx to 0
  // and repainted right before sliding the content away, which shrank the colored
  // action box to nothing at the exact moment it should have been showing "this is
  // what just happened". Now it flashes full, holds for a beat, then commit() slides
  // the content off over it.
  // reveal: how far (as a fraction of row width) content itself moves during the
  // flash. The "fling" variant needs to stay partway visible here (.row clips
  // overflow — going all the way to 100% put content fully outside that clip,
  // already invisible before its own fling could ever play). "Blowup" wants the
  // opposite: content fully out of the way at 100%, since its payoff is a separate
  // burst centered in the row, not content's own motion.
  function snapToFullReveal(sign, reveal = 0.8) {
    dragging = false;
    const max = row.clientWidth;
    dx = renderedDx = sign * max * reveal;
    content.style.transition = 'transform 0.12s ease-out';
    rightSwipeBox.style.transition = leftSwipeBox.style.transition = 'width 0.12s ease-out';
    content.style.transform = `translateX(${renderedDx}px)`;
    // The action box still fills the *entire* row width, independent of how far
    // content itself moved — this is what actually reads as "the bar shows full".
    rightSwipeBox.style.width = `${sign > 0 ? max : 0}px`;
    leftSwipeBox.style.width = `${sign < 0 ? max : 0}px`;
  }

  // Only archive/delete actually remove the row (via commit() below) — the flash-then-
  // slide-away choreography only makes sense for those. Move/tag/read either open a
  // picker or toggle something in place, and the row needs to be sitting back at
  // center for that, the same as a swipe that never crossed the threshold at all.
  function removesRow(action) {
    return action === 'archive' || action === 'delete';
  }

  // Picked here (before the flash) rather than inside commit() — the flash itself
  // needs to behave differently per variant (see snapToFullReveal's own comment), so
  // the choice has to be made before that runs, not after. Stashed on the row so
  // commit() (a separate function, called via performSwipeAction) can read it back.
  function pickDeleteVariant(action) {
    if (action !== 'delete' || localStorage.getItem('funDeleteAnimation') === 'off') {
      delete row.dataset.deleteVariant;
      return 0.8;
    }
    const variant = DELETE_ANIM_VARIANTS[Math.floor(Math.random() * DELETE_ANIM_VARIANTS.length)];
    row.dataset.deleteVariant = variant;
    return variant === 'shatter' ? 1 : 0.8;
  }

  function settleToCenter() {
    dragging = false;
    content.style.transition = 'transform 0.28s cubic-bezier(.34,1.56,.64,1)';
    rightSwipeBox.style.transition = leftSwipeBox.style.transition = 'width 0.28s cubic-bezier(.34,1.56,.64,1)';
    dx = renderedDx = 0;
    paint();
  }

  function endDrag() {
    if (!dragging) return;
    const threshold = row.clientWidth * COMMIT_RATIO;
    const { left, right } = getSwipeConfig();
    if (dx > threshold) {
      if (removesRow(right)) {
        beginRowAnimation(); // held until commit() actually finishes — see its own end calls
        snapToFullReveal(1, pickDeleteVariant(right));
        // Long enough to register as "the box flashed" without dragging the whole
        // gesture out — 220ms felt a beat slower than it needed to be.
        setTimeout(() => performSwipeAction(row, right, 'right'), 170);
      } else {
        settleToCenter();
        performSwipeAction(row, right, 'right');
      }
      return;
    }
    if (dx < -threshold) {
      if (removesRow(left)) {
        beginRowAnimation();
        snapToFullReveal(-1, pickDeleteVariant(left));
        setTimeout(() => performSwipeAction(row, left, 'left'), 170);
      } else {
        settleToCenter();
        performSwipeAction(row, left, 'left');
      }
      return;
    }
    settleToCenter();
  }

  content.addEventListener('touchend', endDrag);
  content.addEventListener('touchcancel', endDrag);

  content.addEventListener('click', () => {
    if (Math.abs(dx) > 4) return; // suppress tap right after a drag
    onRowTapped(row);
  });
}

// A couple of different ways to send delete's flourish off, picked at random each
// time so it's not the exact same motion every single time — see the matching
// @keyframes in style.css for what each one actually does. 'fling' is a flick+spin
// away with a little anticipation squash first; 'shatter' is content vanishing fast
// followed by a handful of small shards punching outward from the row's center, each
// on its own randomized angle (spawnShards, below) — not one static glyph. Archive
// never uses these; it's not the action anyone asked to feel more enjoyable doing.
const DELETE_ANIM_VARIANTS = ['fling', 'shatter'];
const DELETE_ANIM_MS = 380;

const SHATTER_SLIDE_MS = 110; // content's own quick exit, before the shards punch out
const SHATTER_BURST_MS = 340; // matches row-shard-pop's duration in style.css
const SHARD_COUNT = 7;
const SHARD_COLORS = ['--delete', '--accent', '--move', '--archive'];

// Each shard gets its own random direction/distance/spin via CSS custom properties —
// row-shard-pop (style.css) just animates toward whatever --dx/--dy/--rot it's given,
// so every burst looks a little different instead of one fixed, identical pop.
function spawnShards(container) {
  container.innerHTML = '';
  for (let i = 0; i < SHARD_COUNT; i++) {
    const angle = (Math.PI * 2 * i) / SHARD_COUNT + (Math.random() - 0.5) * 0.6;
    const distance = 36 + Math.random() * 28; // bigger spread — too subtle to read clearly on an actual phone screen at the old 26-48px
    const shard = document.createElement('span');
    shard.className = 'row-burst-shard';
    shard.style.background = `var(${SHARD_COLORS[i % SHARD_COLORS.length]})`;
    shard.style.setProperty('--dx', `${Math.cos(angle) * distance}px`);
    shard.style.setProperty('--dy', `${Math.sin(angle) * distance}px`);
    shard.style.setProperty('--rot', `${(Math.random() - 0.5) * 360}deg`);
    container.appendChild(shard);
  }
}

function commit(row, action, direction) {
  row.classList.add('removing');
  const content = row.querySelector('.row-content');
  const sign = direction === 'right' ? 1 : -1;
  const variant = row.dataset.deleteVariant; // set by pickDeleteVariant, before the flash — 'fling', 'shatter', or absent
  delete row.dataset.deleteVariant;
  let removeDelay;
  if (variant === 'fling') {
    content.style.transition = 'none'; // the keyframes animation owns the motion now, not a plain transition
    content.style.animation = `delete-${variant}-${direction} ${DELETE_ANIM_MS}ms ease-in forwards`;
    removeDelay = DELETE_ANIM_MS;
  } else if (variant === 'shatter') {
    // Content was already snapped fully out of view by the flash (snapToFullReveal's
    // reveal=1 for this variant) — finish that exit fast and plainly, then let the
    // shard burst (centered in the row, independent of content's position) be the payoff.
    content.style.transition = `transform ${SHATTER_SLIDE_MS}ms ease-in, opacity ${SHATTER_SLIDE_MS}ms ease-in`;
    content.style.transform = `translateX(${sign * 100}%)`;
    // .row clips overflow for the swipe-reveal mechanic, but the gesture's long over
    // by this point (committed, finger lifted) — nothing left needs that clip, and a
    // shard burst sized to read clearly on a real phone screen can reasonably want to
    // travel past the row's own edges. Reportedly invisible on an actual iPhone before
    // this; clipping the whole effect is the most likely reason why.
    row.style.overflow = 'visible';
    const burst = row.querySelector('.row-burst');
    spawnShards(burst);
    setTimeout(() => burst.classList.add('bursting'), SHATTER_SLIDE_MS);
    removeDelay = SHATTER_SLIDE_MS + SHATTER_BURST_MS;
  } else {
    // Toggle off, or an action other than delete (archive) — same plain slide either way.
    // Opacity has to stay listed here too: overriding `transition` with transform
    // alone dropped the .removing class's own opacity fade entirely (inline style
    // replaces the whole property, not just the part mentioned), so content vanished
    // instantly and *then* slid away while already invisible.
    content.style.transition = 'transform 0.32s cubic-bezier(.34,1.56,.64,1), opacity 0.32s ease';
    content.style.transform = `translateX(${sign * 130}%)`;
    removeDelay = 320;
  }
  // The removal timer used to live inside the fetch's .then() — meaning the row
  // actually disappeared after (network latency + removeDelay), not just removeDelay.
  // On a fast connection that's invisible; on a slow one the animation finishes,
  // visibly leaving the action box just sitting there fully revealed with nothing
  // left to show, until the response *eventually* came back. Decoupled here so the
  // row's removal always happens at exactly removeDelay after the swipe committed —
  // genuinely consistent, not "usually fine".
  let settled = false;
  setTimeout(() => {
    if (settled) return;
    settled = true;
    row.remove();
    endRowAnimation();
  }, removeDelay);
  fetch(`/api/mails/${row.dataset.id}/${action}`, { method: 'POST', headers: dryRunHeaders() })
    .then((res) => {
      if (!res.ok) return res.text().then((msg) => Promise.reject(new Error(msg || `${action} failed`)));
      removeMailById(row.dataset.id); // data-model update only — the row's removal from the DOM is already on its own fixed timer above
    })
    .catch((err) => {
      // the server didn't actually do it (e.g. no Trash/Archive folder found) — restore
      // instead of pretending it worked, so a failed action doesn't quietly vanish then
      // reappear confusingly on the next sync.
      console.error(err);
      if (settled) {
        // The removal timer already fired and the row's gone from the DOM — too late
        // for a snap-back animation, but the mail was never actually removed from the
        // data model (removeMailById only runs on success, above), so a plain
        // re-render brings it back correctly rather than leaving it silently missing.
        render();
        return;
      }
      settled = true;
      row.classList.remove('removing');
      content.style.animation = ''; // clear out of a 'fling' keyframes run, if that's what was active
      row.querySelector('.row-burst').classList.remove('bursting'); // and a 'shatter' burst, if that was instead
      content.style.transition = 'transform 0.28s cubic-bezier(.34,1.56,.64,1)';
      content.style.transform = 'translateX(0) rotate(0) scale(1)'; // undo delete's flick rotate/scale too, not just the slide
      setTimeout(() => { content.style.transition = ''; endRowAnimation(); }, 280);
    });
}

const REFRESH_QUIPS = [
  'Summoning your mail…', 'Asking the mail gods…', 'Herding electrons…',
  'Bribing the IMAP server…', 'Untangling the tubes…', 'Waking up the postman…',
];

// Refreshing repeatedly within this window just shows a quip instead of hitting the
// IMAP server again — both kinder to it and funnier than nothing happening.
const REFRESH_COOLDOWN_MS = 8000;
const COOLDOWN_QUIPS = [
  'Whoa, give me a sec! 😅', 'Easy there, tiger 🐯', 'Patience, young padawan…',
  'Still catching my breath…', 'The mail gods need a minute…',
];

// Has to be kept in sync with every overlay panel/sheet in the app — `moveModal` was
// a stale id left over from before it got folded into folderSheetModal, silently
// making the old version of this check always false for that case (and every panel
// added since never got added at all).
const OVERLAY_IDS = [
  'mailReaderPanel', 'accountsPanel', 'settingsPanel', 'personalisationPanel',
  'tagsPanel', 'smartTaggingPanel', 'searchPanel', 'folderSheetModal', 'tagSheetModal',
];

function openOverlays() {
  return OVERLAY_IDS.map((id) => document.getElementById(id)).filter((el) => el && !el.classList.contains('hidden'));
}

// Pull-to-refresh listens on `document` regardless of what's currently visible, so it
// needs to explicitly stand aside whenever a full-screen overlay is open — otherwise
// scrolling/dragging inside that overlay's own content gets mistaken for a pull-down
// (or just plain scroll) gesture on the inbox underneath it.
function overlayOpen() {
  return openOverlays().length > 0;
}

// CSS overscroll-behavior on the overlays' own scroll containers should stop scroll
// chaining into the inbox behind them, but that didn't fully hold up in practice —
// this is the blunt, reliable version: while any overlay is open, swallow any
// touchmove that isn't happening inside one of them, full stop.
//
// The bottom-sheet ones (.move-modal) are a full-screen backdrop with the actual
// content sitting at the bottom — the dimmed area above that content is still part
// of the SAME element, so checking "is the touch inside the overlay element" was true
// there too, and a touch starting on that backdrop never got prevented, leaving the
// browser free to fall back to its default behavior: scrolling the actual page
// underneath. What needs to stay scrollable is specifically .move-modal-content (or,
// for the other panels, the panel itself), not the backdrop around it.
function scrollableAreaFor(el) {
  return el.querySelector('.move-modal-content') || el.querySelector('.mail-reader-scroll') || el;
}

// iOS's native "tap the status bar to scroll to top" only ever reaches the document's
// own scroll — every full-screen overlay panel (Settings, Accounts, Search, the mail
// reader...) is its own separate position:fixed/overflow-y:auto scroll context that
// gesture can't touch at all, which is exactly why it works on the plain inbox but not
// once you're inside one of these. Replicate it generically rather than wiring up a
// one-off per panel: a tap right at the very top edge — the safe-area padding strip,
// before any real content starts — scrolls whichever overlay is currently open back to
// its own top.
const TOP_TAP_ZONE_PX = 44;

export function setupTapTopToScroll() {
  document.addEventListener('click', (e) => {
    if (e.clientY > TOP_TAP_ZONE_PX || e.target.closest('button')) return;
    const overlays = openOverlays();
    if (overlays.length === 0) return; // plain inbox/folder/group view — the OS gesture already covers this
    const topmost = overlays[overlays.length - 1]; // bottom-sheet modals are listed last, and only ever open on top of a panel
    scrollableAreaFor(topmost).scrollTo({ top: 0, behavior: 'smooth' });
  });
}

// Belt: block touchmove outside the overlay's own scrollable area, same as before.
// Suspenders: iOS momentum scrolling, once already in flight (e.g. you flick the
// inbox, then immediately tap a swipe action before it settles), is a compositor-level
// animation — it doesn't originate from a new touchmove at all, so no amount of
// preventDefault() on this listener can touch it. The only thing that reliably kills
// already-in-flight momentum is yanking the body out of the document flow.
let savedScrollY = 0;

function lockBody() {
  savedScrollY = window.scrollY;
  document.body.style.position = 'fixed';
  document.body.style.top = `-${savedScrollY}px`;
  document.body.style.width = '100%';
  document.body.classList.add('scroll-locked'); // see style.css: keeps .sticky-header from jumping
}

function unlockBody() {
  document.body.style.position = '';
  document.body.style.top = '';
  document.body.style.width = '';
  document.body.classList.remove('scroll-locked');
  window.scrollTo(0, savedScrollY);
}

// Only the bottom-sheet modals actually need the body lock: they're a dimmed backdrop
// with content sitting on top, so the inbox behind is still visible and its momentum
// scroll bleeding through is something to guard against. The full-screen panels
// (reader, settings, accounts...) are opaque and cover the entire viewport themselves —
// nothing's visible behind them to bleed through, so there's nothing to gain from
// locking body for as long as one's open. There's a real cost to doing it anyway: those
// panels are themselves `position: fixed`, and toggling body's own position between
// static and fixed while one is open is exactly the kind of nested-fixed-context change
// iOS Safari has long-standing rendering bugs with — which is what caused the reader's
// own sticky header to visibly glitch while scrolling.
const BODY_LOCK_IDS = ['folderSheetModal', 'tagSheetModal'];

function bodyLockNeeded() {
  return BODY_LOCK_IDS.some((id) => {
    const el = document.getElementById(id);
    return el && !el.classList.contains('hidden');
  });
}

export function setupOverlayScrollLock() {
  document.addEventListener('touchmove', (e) => {
    const overlays = openOverlays();
    if (overlays.length === 0) return;
    if (overlays.some((el) => scrollableAreaFor(el).contains(e.target))) return;
    e.preventDefault();
  }, { passive: false });

  let locked = false;
  const observer = new MutationObserver(() => {
    const shouldLock = bodyLockNeeded();
    if (shouldLock === locked) return;
    locked = shouldLock;
    if (locked) lockBody();
    else unlockBody();
  });
  for (const id of BODY_LOCK_IDS) {
    const el = document.getElementById(id);
    if (el) observer.observe(el, { attributes: true, attributeFilter: ['class'] });
  }
}

// Module-scoped (not inside setupPullToRefresh's closure) so switching context —
// entering or leaving a folder — can reset it: the cooldown exists to stop spamming
// the *same* refresh, not to silently block the first refresh after you've switched
// to looking at something else entirely, which looked just like "refresh is broken."
let lastRefreshAt = 0;

export function setupPullToRefresh() {
  const indicator = document.getElementById('pullIndicator');
  const icon = document.getElementById('pullIcon');
  const text = document.getElementById('pullText');
  const list = document.getElementById('inbox');
  const THRESHOLD = 70;
  let startX = 0, startY = 0, pulling = false, decided = false, claimed = false, refreshing = false, pull = 0, rafScheduled = false;

  function paint() {
    rafScheduled = false;
    list.style.transform = `translateY(${Math.min(pull, 100)}px)`;
    indicator.style.height = `${Math.min(pull, 60)}px`;
    const past = pull > THRESHOLD;
    icon.classList.toggle('tilt', past);
    icon.textContent = past ? '📬' : '📨';
    text.textContent = past ? 'Release to summon mail' : 'Pull to refresh';
  }

  function schedulePaint() {
    if (rafScheduled) return;
    rafScheduled = true;
    requestAnimationFrame(paint);
  }

  document.addEventListener('touchstart', (e) => {
    if (refreshing || overlayOpen() || document.scrollingElement.scrollTop > 0) { pulling = false; return; }
    startX = e.touches[0].clientX;
    startY = e.touches[0].clientY;
    pulling = true;
    decided = false;
    claimed = false;
  }, { passive: true });

  document.addEventListener('touchmove', (e) => {
    if (!pulling || refreshing) return;
    const dx = e.touches[0].clientX - startX;
    const dy = e.touches[0].clientY - startY;
    if (!decided) {
      if (Math.abs(dx) < 8 && Math.abs(dy) < 8) return;
      decided = true;
      // a row swipe in progress will have dx dominant — bail out and let it handle the gesture
      if (Math.abs(dx) >= Math.abs(dy) || dy <= 0) { pulling = false; return; }
    }
    if (dy <= 0) return;
    claimed = true;
    e.preventDefault(); // claim it before the browser starts its own scroll/bounce
    pull = rubberBand(dy, 70);
    schedulePaint();
  }, { passive: false });

  function endPull() {
    if (!pulling) return;
    pulling = false;
    if (!claimed || refreshing) return;
    list.style.transition = 'transform 0.28s cubic-bezier(.34,1.56,.64,1)';
    list.style.transform = 'translateY(0)';
    if (pull > THRESHOLD) {
      refreshing = true;
      indicator.classList.add('refreshing');
      icon.classList.remove('tilt');

      if (lastRefreshAt && Date.now() - lastRefreshAt < REFRESH_COOLDOWN_MS) {
        // refreshed too recently — show a quip and bail out instead of hammering IMAP
        icon.textContent = '🙃';
        text.textContent = COOLDOWN_QUIPS[Math.floor(Math.random() * COOLDOWN_QUIPS.length)];
        setTimeout(() => {
          indicator.classList.remove('refreshing');
          indicator.style.height = '0';
          refreshing = false;
        }, 1400);
        pull = 0;
        setTimeout(() => { list.style.transition = ''; }, 280);
        return;
      }

      icon.classList.add('spin');
      icon.textContent = '📨';
      text.textContent = REFRESH_QUIPS[Math.floor(Math.random() * REFRESH_QUIPS.length)];
      // keep the quip on screen for a beat even if the fetch resolves instantly —
      // otherwise it flashes by unread on a fast connection. 900ms wasn't quite enough
      // to actually read the punchline; 1400ms is.
      const minDelay = new Promise((resolve) => setTimeout(resolve, 1400));
      Promise.all([refreshMails(), minDelay]).then(() => {
        lastRefreshAt = Date.now();
        render();
        indicator.classList.remove('refreshing');
        icon.classList.remove('spin');
        indicator.style.height = '0';
        refreshing = false;
      });
    } else {
      indicator.style.height = '0';
    }
    pull = 0;
    setTimeout(() => { list.style.transition = ''; }, 280);
  }

  document.addEventListener('touchend', endPull);
  document.addEventListener('touchcancel', endPull);
}
