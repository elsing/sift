// Core inbox: fetching/rendering mail, swipe gestures, pull-to-refresh, and switching
// between the inbox and a browsed folder (same list + gestures either way).
//
// ponytail: native touch events handle swipe + rubber-band; no gesture library needed.
// touch events (not Pointer Events) chosen deliberately: they stay locked to their original
// target for the whole gesture in iOS standalone/PWA mode, where Pointer Events + setPointerCapture
// have known quirks (gesture randomly reverts to native scroll mid-drag).
import { dryRunHeaders } from './util.js';

const EDGE_GUARD = 20;     // px from screen edge where swipe is ignored, avoids iOS back/app-switcher gesture
const COMMIT_RATIO = 0.3;  // fraction of row width dragged at release-time to commit; below this, releasing cancels
export const PAGE_SIZE = 30;

export let mails = [];
let hasMore = true;
let loadingMore = false;
// nextOffset tracks how many rows we've fetched from the server, independent of mails.length:
// archiving/deleting (esp. under dry-run, which doesn't actually shrink the server-side list)
// shrinks the local array without moving the server cursor, so reusing mails.length as the
// offset re-fetches already-seen rows and shows duplicates.
let nextOffset = 0;

// null when viewing the inbox; { accountId, path, name } when browsing a folder.
export let currentFolder = null;

// null means "all accounts" (the common case, and the only case with one account).
// Setting it restricts the inbox to a chosen subset — one account, or any mix.
let accountFilterIds = null;

// Called by the account-filter chip strip when the selection changes. Clears the
// list immediately (same pattern as switching folders) rather than leaving stale
// rows up while the new fetch is in flight.
export function applyAccountFilter(ids) {
  accountFilterIds = ids && ids.length ? ids : null;
  mails = [];
  render();
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
export function setHandlers({ onMove, onTap }) {
  onMoveRequested = onMove;
  onRowTapped = onTap;
}

export async function fetchMails() {
  if (currentFolder) {
    const res = await fetch(`/api/accounts/${currentFolder.accountId}/folder-mails?path=${encodeURIComponent(currentFolder.path)}`);
    if (!res.ok) {
      const msg = await res.text();
      throw new Error(msg || 'failed to load folder');
    }
    mails = await res.json();
    nextOffset = mails.length;
    hasMore = false; // ponytail: folder view doesn't paginate yet, just the most recent page
    return;
  }
  const res = await fetch(`/api/mails?${accountsQueryParam('')}`);
  mails = await res.json();
  nextOffset = mails.length;
  hasMore = mails.length === PAGE_SIZE;
}

export async function refreshMails() {
  if (currentFolder) return fetchMails(); // folder view: a refresh just re-fetches live from IMAP
  const res = await fetch(`/api/mails/refresh?${accountsQueryParam('')}`, { method: 'POST' });
  mails = await res.json();
  nextOffset = mails.length;
  hasMore = mails.length === PAGE_SIZE;
}

export async function loadMore() {
  if (loadingMore || !hasMore || currentFolder) return;
  loadingMore = true;
  const res = await fetch(`/api/mails?offset=${nextOffset}${accountsQueryParam('&')}`);
  const more = await res.json();
  nextOffset += more.length;
  hasMore = more.length === PAGE_SIZE;
  mails = mails.concat(more);
  const list = document.getElementById('inbox');
  for (const mail of more) appendMailRow(list, mail);
  updateEndOfListFooter();
  loadingMore = false;
}

// Scrolling to the bottom with nothing more to load (backfill has reached the oldest
// message in the mailbox) used to just silently do nothing, which looked identical to
// "broken". Make that state visible instead.
function updateEndOfListFooter() {
  const list = document.getElementById('inbox');
  const existing = document.getElementById('endOfListFooter');
  if (existing) existing.remove();
  if (!hasMore && !currentFolder && mails.length > 0) {
    const li = document.createElement('li');
    li.id = 'endOfListFooter';
    li.className = 'end-of-list-footer';
    li.textContent = "📭 You've reached the end of your inbox";
    list.appendChild(li);
  }
}

// Today / This Week / Last Week / Older, based on calendar-day difference from now.
function dateSection(iso) {
  if (!iso) return 'Older';
  const d = new Date(iso);
  if (isNaN(d)) return 'Older';
  const startOfDay = (date) => new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const daysAgo = Math.round((startOfDay(new Date()) - startOfDay(d)) / 86400000);
  if (daysAgo <= 0) return 'Today';
  if (daysAgo <= 6) return 'This Week';
  if (daysAgo <= 13) return 'Last Week';
  return 'Older';
}

// Tracks the last section header written, across both a full render() and incremental
// loadMore() appends, so headers only appear once per transition, not once per row.
let lastSection = null;

function appendMailRow(list, mail) {
  const section = dateSection(mail.date);
  if (section !== lastSection) {
    const header = document.createElement('li');
    header.className = 'date-section-header';
    header.textContent = section;
    list.appendChild(header);
    lastSection = section;
  }
  list.appendChild(renderRow(mail));
}

export function openFolder(accountId, path, name) {
  currentFolder = { accountId, path, name };
  document.getElementById('folderBannerName').textContent = name;
  document.getElementById('folderBanner').classList.remove('hidden');
  document.getElementById('accountsPanel').classList.add('hidden');
  window.scrollTo(0, 0);
  mails = [];
  render(); // clear the old (inbox) rows immediately, don't wait on the fetch to land
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
  document.getElementById('folderBanner').classList.add('hidden');
  window.scrollTo(0, 0);
  mails = [];
  render(); // clear the folder's rows immediately, same as entering a folder does
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
    if (!currentFolder) fetchMails().then(render);
  });
  es.addEventListener('error', () => {
    console.warn('live updates: SSE connection error (browser will retry)', es.readyState);
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && !currentFolder) fetchMails().then(render);
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

export function render() {
  const list = document.getElementById('inbox');
  list.innerHTML = '';
  lastSection = null;
  for (const mail of mails) appendMailRow(list, mail);
  updateEndOfListFooter();
}

export function removeMailById(id) {
  mails = mails.filter((m) => m.id !== id);
}

export function getMailById(id) {
  return mails.find((m) => m.id === id);
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
    <div class="row-content">
      <div class="unread-dot"></div>
      <div class="row-text">
        <div class="sender">${mail.sender}</div>
        <div class="subject">${mail.subject}</div>
        <div class="snippet">${mail.snippet}</div>
      </div>
      <div class="timestamp">${mail.time}</div>
    </div>
  `;
  attachSwipe(li);
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
  commit(row, action, direction);
}

function attachSwipe(row) {
  const content = row.querySelector('.row-content');
  const rightSwipeBox = row.querySelector('.row-action.right-swipe'); // revealed dragging right (dx > 0)
  const leftSwipeBox = row.querySelector('.row-action.left-swipe');   // revealed dragging left (dx < 0)
  let startX = 0, startY = 0, dx = 0, dragging = false, horizontal = false, rafScheduled = false;

  function paint() {
    rafScheduled = false;
    content.style.transform = `translateX(${dx}px)`;
    rightSwipeBox.style.width = `${Math.max(dx, 0)}px`;
    leftSwipeBox.style.width = `${Math.max(-dx, 0)}px`;
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

  function endDrag() {
    if (!dragging) return;
    dragging = false;
    content.style.transition = 'transform 0.28s cubic-bezier(.34,1.56,.64,1)';
    rightSwipeBox.style.transition = leftSwipeBox.style.transition = 'width 0.28s cubic-bezier(.34,1.56,.64,1)';
    const threshold = row.clientWidth * COMMIT_RATIO;
    const { left, right } = getSwipeConfig();
    if (dx > threshold) { dx = 0; paint(); performSwipeAction(row, right, 'right'); return; }
    if (dx < -threshold) { dx = 0; paint(); performSwipeAction(row, left, 'left'); return; }
    dx = 0;
    paint();
  }

  content.addEventListener('touchend', endDrag);
  content.addEventListener('touchcancel', endDrag);

  content.addEventListener('click', () => {
    if (Math.abs(dx) > 4) return; // suppress tap right after a drag
    onRowTapped(row);
  });
}

function commit(row, action, direction) {
  row.classList.add('removing');
  const content = row.querySelector('.row-content');
  content.style.transform = `translateX(${direction === 'right' ? '100%' : '-100%'})`;
  fetch(`/api/mails/${row.dataset.id}/${action}`, { method: 'POST', headers: dryRunHeaders() })
    .then((res) => {
      if (!res.ok) return res.text().then((msg) => Promise.reject(new Error(msg || `${action} failed`)));
      setTimeout(() => {
        removeMailById(row.dataset.id);
        row.remove();
      }, 280);
    })
    .catch((err) => {
      // the server didn't actually do it (e.g. no Trash/Archive folder found) — snap back
      // instead of pretending it worked, so a failed action doesn't quietly vanish then
      // reappear confusingly on the next sync.
      console.error(err);
      row.classList.remove('removing');
      content.style.transition = 'transform 0.28s cubic-bezier(.34,1.56,.64,1)';
      content.style.transform = 'translateX(0)';
      setTimeout(() => { content.style.transition = ''; }, 280);
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

// Pull-to-refresh listens on `document` regardless of what's currently visible, so it
// needs to explicitly stand aside whenever a full-screen overlay (mail reader, accounts
// panel, move modal) is open — otherwise scrolling inside that overlay's own content
// can get mistaken for a pull-down gesture on the inbox underneath it.
function overlayOpen() {
  return ['mailReaderPanel', 'accountsPanel', 'moveModal'].some((id) => {
    const el = document.getElementById(id);
    return el && !el.classList.contains('hidden');
  });
}

export function setupPullToRefresh() {
  const indicator = document.getElementById('pullIndicator');
  const icon = document.getElementById('pullIcon');
  const text = document.getElementById('pullText');
  const list = document.getElementById('inbox');
  const THRESHOLD = 70;
  let startX = 0, startY = 0, pulling = false, decided = false, claimed = false, refreshing = false, pull = 0, rafScheduled = false;
  let lastRefreshAt = 0;

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
        }, 1000);
        pull = 0;
        setTimeout(() => { list.style.transition = ''; }, 280);
        return;
      }

      icon.classList.add('spin');
      icon.textContent = '📨';
      text.textContent = REFRESH_QUIPS[Math.floor(Math.random() * REFRESH_QUIPS.length)];
      // keep the quip on screen for a beat even if the fetch resolves instantly —
      // otherwise it flashes by unread on a fast connection.
      const minDelay = new Promise((resolve) => setTimeout(resolve, 900));
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
