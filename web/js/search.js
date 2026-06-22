import { currentFolder, openFolder } from './inbox.js';
import { getSelectedAccountIds } from './accountFilter.js';
import { openMailReaderById, setReaderBack } from './reader.js';
import { pickFolders } from './folders.js';
import { fetchTags } from './tags.js';

let panel, input, scopeBtn, accountScopeEl, errorEl, results;
let progressWrap, progressFill, progressText, deepBtn, continueBtn;
let advancedToggle, advancedPanel, fromInput, sinceInput, beforeInput, folderPickerToggle, tagFilterEl;
let searchAllFolders = true; // toggled off via scopeBtn to restrict to the folder you opened search from
let currentSource = null;
let matches = [];
let lastMode = 'light'; // so "Continue" resumes in the same mode the timed-out search used

// Which specific folders to search, chosen via the folder picker — empty means "use
// the normal all-folders/this-folder-only scope" (the picker is an override, not a
// second independent scope control).
const chosenFolders = new Set();

// Tags are local-only data IMAP can't search by — chosen here, they narrow whatever
// the text/sender search already found, rather than being a standalone "search by
// tag" (that would mean scanning the whole mailbox with no real query, which doesn't
// scale any better than the "deep, all folders" case already doesn't).
const chosenTags = new Set();

export function setupSearch() {
  panel = document.getElementById('searchPanel');
  input = document.getElementById('searchInput');
  scopeBtn = document.getElementById('searchFolderScope');
  accountScopeEl = document.getElementById('searchAccountScope');
  errorEl = document.getElementById('searchError');
  results = document.getElementById('searchResults');
  progressWrap = document.getElementById('searchProgress');
  progressFill = document.getElementById('searchProgressFill');
  progressText = document.getElementById('searchProgressText');
  deepBtn = document.getElementById('deepSearchBtn');
  continueBtn = document.getElementById('continueSearchBtn');
  advancedToggle = document.getElementById('advancedSearchToggle');
  advancedPanel = document.getElementById('advancedSearchPanel');
  fromInput = document.getElementById('searchFromInput');
  sinceInput = document.getElementById('searchSinceInput');
  beforeInput = document.getElementById('searchBeforeInput');
  folderPickerToggle = document.getElementById('searchFolderPickerToggle');
  tagFilterEl = document.getElementById('searchTagFilter');

  // Previous attempts resized #searchPanel itself (to visualViewport.height) — that's
  // what caused the worse regressions (a stuck-short panel, jank from toolbar-driven
  // resize events, a literal gap exposing the inbox underneath). The panel's own
  // size/position is never touched now, full stop — still plain `position: fixed;
  // inset: 0`, always fully covering the screen regardless of keyboard state.
  //
  // But that on its own left genuinely nothing to scroll for a short result list: the
  // panel's box never shrinks, so its scrollable content (input + filters + a couple of
  // results) can easily be shorter than that box even while the keyboard visually
  // covers the bottom third of it — scrollBy/scrollIntoView have no effect when there's
  // no overflow to scroll into in the first place. Padding the *results* element specif-
  // ically (never the panel) by the actual keyboard height — window.innerHeight minus
  // visualViewport.height, the one number iOS reports correctly — manufactures that
  // room without changing what the panel itself covers.
  if (window.visualViewport) {
    const syncKeyboardPadding = () => {
      if (document.activeElement !== input) {
        results.style.paddingBottom = '';
        return;
      }
      const keyboardHeight = Math.max(0, window.innerHeight - window.visualViewport.height);
      results.style.paddingBottom = keyboardHeight > 0 ? `${keyboardHeight}px` : '';
    };
    window.visualViewport.addEventListener('resize', syncKeyboardPadding);
    input.addEventListener('focus', syncKeyboardPadding);
    input.addEventListener('blur', syncKeyboardPadding);
  }

  document.getElementById('searchBtn').addEventListener('click', openSearch);
  document.getElementById('closeSearchBtn').addEventListener('click', closeSearch);
  document.getElementById('searchForm').addEventListener('submit', (e) => {
    e.preventDefault();
    startSearch('light');
  });
  deepBtn.addEventListener('click', () => startSearch('deep'));
  continueBtn.addEventListener('click', () => startSearch(lastMode, continueBtn.dataset.resume));

  scopeBtn.addEventListener('click', () => {
    searchAllFolders = !searchAllFolders;
    updateScopeLabel();
    if (hasQuery()) startSearch('light');
  });

  advancedToggle.addEventListener('click', () => {
    const showing = advancedPanel.classList.toggle('hidden') === false;
    advancedToggle.textContent = showing ? 'Advanced filters ▴' : 'Advanced filters ▾';
    if (showing) renderTagFilter();
  });

  folderPickerToggle.addEventListener('click', () => openFolderPicker());

  // The 1-month default (openSearch, below) needs an easy way back out to "no date
  // filter at all" — clearing two date fields by hand felt fiddlier than it should.
  document.getElementById('searchClearDatesBtn').addEventListener('click', () => {
    sinceInput.value = '';
    beforeInput.value = '';
    if (hasQuery()) startSearch('light');
  });

  // search-as-you-type, debounced — always "light" (headers only): cheap enough to
  // run on every pause, unlike a full body search across every folder
  let debounce;
  const onFilterInput = () => {
    clearTimeout(debounce);
    if (!hasQuery()) {
      stopSearch();
      results.innerHTML = '';
      errorEl.textContent = '';
      deepBtn.classList.add('hidden');
      continueBtn.classList.add('hidden');
      progressWrap.classList.add('hidden');
      tagFilterEl.classList.remove('collapsed');
      if (!advancedPanel.classList.contains('hidden')) renderTagFilter();
      return;
    }
    debounce = setTimeout(() => startSearch('light'), 700);
  };
  input.addEventListener('input', onFilterInput);
  fromInput.addEventListener('input', onFilterInput);
  sinceInput.addEventListener('change', onFilterInput);
  beforeInput.addEventListener('change', onFilterInput);
}

// A search needs free text, a sender filter, a date range, or a tag — not all of them.
// A tag alone is enough: it's local data, not an IMAP query, so it doesn't need pairing
// with text to be a real search.
function hasQuery() {
  return input.value.trim() !== '' || fromInput.value.trim() !== '' || sinceInput.value !== '' || beforeInput.value !== '' || chosenTags.size > 0;
}

// Uses the same shared move-to-folder modal every other folder picker in the app
// already uses (checkbox mode, account-picked-first if there's more than one) —
// the old version rendered its own inline tree directly in the search panel, which
// is exactly what made the panel feel cramped/overlapping.
function openFolderPicker() {
  pickFolders('Search these folders', chosenFolders, () => {
    folderPickerToggle.textContent = chosenFolders.size > 0
      ? `${chosenFolders.size} folder${chosenFolders.size === 1 ? '' : 's'} chosen`
      : 'Choose folders…';
    if (hasQuery()) startSearch('light');
  });
}

async function renderTagFilter() {
  let tags;
  try {
    tags = await fetchTags();
  } catch {
    return;
  }
  tagFilterEl.innerHTML = '';
  if (tags.length === 0) return;
  // Once a search is actually running, the full tag list is just noise next to the
  // results — only the ones you picked still matter, capped well under what the
  // collapsed strip could ever show anyway.
  if (tagFilterEl.classList.contains('collapsed')) {
    tags = tags.filter((t) => chosenTags.has(t.id)).slice(0, 8);
  }
  for (const tag of tags) {
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'account-chip' + (chosenTags.has(tag.id) ? ' active' : '');
    chip.style.borderColor = tag.color;
    if (chosenTags.has(tag.id)) chip.style.background = tag.color;
    chip.textContent = tag.name;
    chip.addEventListener('click', () => {
      if (chosenTags.has(tag.id)) chosenTags.delete(tag.id);
      else chosenTags.add(tag.id);
      renderTagFilter();
    });
    tagFilterEl.appendChild(chip);
  }
}

function openSearch() {
  panel.classList.remove('hidden');
  chosenTags.clear();
  // default to "this folder" if search was opened while browsing one — almost always
  // what you mean by searching from there, with the all-folders toggle one tap away
  searchAllFolders = !currentFolder;
  if (currentFolder) {
    scopeBtn.classList.remove('hidden');
    updateScopeLabel();
  } else {
    scopeBtn.classList.add('hidden');
  }
  input.value = '';
  fromInput.value = '';
  // A blank date range looked like something had gone wrong rather than "no filter
  // set yet" — defaulting to the last month gives it an obviously-intentional
  // starting point instead, one tap away from being changed or cleared entirely.
  const oneMonthAgo = new Date();
  oneMonthAgo.setMonth(oneMonthAgo.getMonth() - 1);
  sinceInput.value = oneMonthAgo.toISOString().slice(0, 10);
  beforeInput.value = '';
  chosenFolders.clear();
  folderPickerToggle.textContent = 'Choose folders…';
  advancedPanel.classList.add('hidden');
  advancedToggle.textContent = 'Advanced filters ▾';
  results.innerHTML = '';
  errorEl.textContent = '';
  progressWrap.classList.add('hidden');
  deepBtn.classList.add('hidden');
  continueBtn.classList.add('hidden');
  tagFilterEl.classList.remove('collapsed');
  input.focus();
  updateAccountScopeLabel();
}

function closeSearch() {
  stopSearch();
  panel.classList.add('hidden');
}

function stopSearch() {
  if (currentSource) {
    currentSource.close();
    currentSource = null;
  }
}

function updateScopeLabel() {
  scopeBtn.textContent = searchAllFolders ? 'Searching: all folders' : `Searching: "${currentFolder.name}" only`;
}

// Visible confirmation of which account(s) a search will run against — easy to miss
// otherwise, since the account filter chips themselves are hidden for a single-account
// setup and aren't shown again inside this panel.
async function updateAccountScopeLabel() {
  const res = await fetch('/api/accounts');
  if (!res.ok) return;
  const accounts = await res.json();
  const selectedIds = getSelectedAccountIds();
  if (accounts.length <= 1 || !selectedIds) {
    accountScopeEl.textContent = accounts.length === 1
      ? `Searching: ${accounts[0].email}`
      : `Searching: all ${accounts.length} accounts`;
    return;
  }
  const selected = accounts.filter((a) => selectedIds.includes(a.id));
  accountScopeEl.textContent = selected.length === 1
    ? `Searching: ${selected[0].email}`
    : `Searching: ${selected.length} of ${accounts.length} accounts`;
}

// Progress and results both stream in over SSE rather than one blocking response —
// a mailbox can have 100+ folders, so this is what lets the UI show real progress
// ("searched 42 of 137 folders") instead of either a frozen spinner or a long wait
// that occasionally times out and reports nothing.
//
// resumeToken, when set, names exactly the (account, folder) pairs a previous,
// timed-out run of this same search never got to (see the "complete" handler below) —
// existing matches/results are kept and added to, not wiped and restarted.
function startSearch(mode, resumeToken) {
  const q = input.value.trim();
  const from = fromInput.value.trim();
  const since = sinceInput.value;
  const before = beforeInput.value;
  if (!q && !from && !since && !before) return;
  stopSearch();
  lastMode = mode;
  errorEl.textContent = '';
  continueBtn.classList.add('hidden');
  if (!resumeToken) {
    matches = [];
    results.innerHTML = '';
  }
  // Tag filter starts fully open (room to pick tags before you've typed anything) and
  // shrinks down once an actual search is running — no point spending vertical space
  // on the full picker once results are what you're looking at, just the tags you
  // actually filtered by.
  tagFilterEl.classList.add('collapsed');
  renderTagFilter();
  progressWrap.classList.remove('hidden');
  progressFill.style.width = resumeToken ? progressFill.style.width : '0%';
  progressText.textContent = resumeToken
    ? 'Continuing…'
    : (mode === 'deep' ? 'Deep searching…' : 'Searching…');
  deepBtn.classList.add('hidden');

  const params = new URLSearchParams({ mode });
  if (q) params.set('q', q);
  if (from) params.set('from', from);
  if (since) params.set('since', since);
  if (before) params.set('before', before);
  const accountIds = getSelectedAccountIds();
  if (accountIds) params.set('accounts', accountIds.join(','));
  // an explicit folder picker selection overrides the simpler all-folders/this-folder
  // scope toggle — they both express "which folders", just at different granularity
  if (chosenFolders.size > 0) {
    for (const f of chosenFolders) params.append('folders', f);
  } else if (!searchAllFolders && currentFolder) {
    params.set('folder', currentFolder.path);
  }
  for (const tagId of chosenTags) params.append('tags', tagId);
  if (resumeToken) params.set('resume', resumeToken);

  const source = new EventSource(`/api/search?${params}`);
  currentSource = source;

  // stopSearch() closes the previous EventSource before this one is created, but
  // close() doesn't retroactively cancel an event that's already been queued as a
  // task by the time it's called — typing fast enough to start a new search while an
  // old one's "complete" event is already in flight could let that stale event fire
  // anyway, briefly stomping fresh results with the old (often-empty) search's
  // verdict right after they'd just rendered. Every handler below checks it's still
  // the current search before acting, the same guard inbox.js's fetchGeneration uses.
  const isStale = () => source !== currentSource;

  source.addEventListener('progress', (e) => {
    if (isStale()) return;
    const { done, total } = JSON.parse(e.data);
    progressFill.style.width = (total ? Math.round((done / total) * 100) : 100) + '%';
    progressText.textContent = `Searched ${done} of ${total} folders · ${matches.length} match${matches.length === 1 ? '' : 'es'}`;
  });

  source.addEventListener('match', (e) => {
    if (isStale()) return;
    matches.push(...JSON.parse(e.data));
    // results stream in whatever order each folder's IMAP round trip happens to
    // finish in — not date order — so re-sort before every render, not just at the end
    matches.sort((a, b) => (a.date < b.date ? 1 : -1));
    renderResults(matches);
  });

  source.addEventListener('complete', (e) => {
    if (isStale()) return;
    const { done, total, timedOut, resume } = JSON.parse(e.data);
    if (matches.length === 0) {
      results.innerHTML = '<li class="folder-empty-status">No matches.</li>';
    }
    if (timedOut) {
      // ran out of time before covering every folder — say so plainly, rather than
      // just stopping one short of the total and looking identical to "finished",
      // and offer to pick up exactly where it left off
      progressFill.style.width = (total ? Math.round((done / total) * 100) : 100) + '%';
      progressText.textContent = `Stopped after ${done} of ${total} folders (timed out) · ${matches.length} match${matches.length === 1 ? '' : 'es'}`;
      if (resume) {
        continueBtn.textContent = `Continue searching (${total - done} folders left)`;
        continueBtn.dataset.resume = resume;
        continueBtn.classList.remove('hidden');
      }
    } else {
      // The very last "progress" event (done === total) and this "complete" event
      // arrive close enough together that the bar jumped straight from e.g. "41 of
      // 42" to hidden — the 100%/"42 of 42" frame got superseded before it ever
      // painted, which read as "it always stops one short of the max" even though
      // every folder genuinely was searched. Show the finished state explicitly,
      // then hide it a beat later instead of immediately.
      progressFill.style.width = '100%';
      progressText.textContent = `Searched ${total} of ${total} folder${total === 1 ? '' : 's'} · ${matches.length} match${matches.length === 1 ? '' : 'es'}`;
      setTimeout(() => progressWrap.classList.add('hidden'), 500);
    }
    // a quick header-only search came up short (or you just want to be thorough) —
    // offer the slower full-body search as a deliberate next step, not automatic
    if (mode === 'light') deepBtn.classList.remove('hidden');
    stopSearch();
  });

  source.onerror = () => {
    if (isStale()) return;
    if (matches.length === 0) errorEl.textContent = "Couldn't complete the search.";
    stopSearch();
    progressWrap.classList.add('hidden');
  };
}

function renderResults(mails) {
  results.innerHTML = '';
  for (const mail of mails) {
    const li = document.createElement('li');
    li.className = 'row search-result-row' + (mail.unread ? ' unread' : '');
    li.innerHTML = `
      <div class="row-content">
        <div class="unread-dot"></div>
        <div class="row-text">
          <div class="sender">${mail.sender}</div>
          <div class="subject">${mail.subject}</div>
          <div class="snippet">${mail.folder || ''}</div>
        </div>
        <div class="timestamp">${mail.hasAttachments ? '<span class="attachment-clip" title="Has attachments">📎</span>' : ''}${mail.time}</div>
      </div>
    `;
    li.addEventListener('click', () => {
      closeSearch(); // hides the panel only — doesn't clear the results list underneath
      // land in the mail's actual folder (not a reader floating with no context) so
      // swipe actions behave the same as if you'd browsed there normally
      if (mail.accountId && mail.folder) openFolder(mail.accountId, mail.folder, mail.folder);
      openMailReaderById(mail.id);
      // Back goes to the search results you came from, not the folder it landed you in
      setReaderBack(() => panel.classList.remove('hidden'));
    });
    results.appendChild(li);
  }
  // scrollIntoView checks visibility against the panel's own (unshrunk, full-layout-
  // viewport) bounds — it has no idea the keyboard is covering the bottom portion of
  // that box, so it considers a result "in view" even when it's visually hidden behind
  // the keyboard. window.visualViewport.height is the one number that's actually
  // correct here (the genuinely visible height above the keyboard), so this computes
  // the exact scroll distance by hand instead of asking the browser to guess.
  if (mails.length > 0 && document.activeElement === input && window.visualViewport) {
    requestAnimationFrame(() => {
      const last = results.lastElementChild;
      if (!last) return;
      const overflow = last.getBoundingClientRect().bottom - window.visualViewport.height;
      if (overflow > 0) {
        panel.scrollBy({ top: overflow + 16, behavior: 'smooth' });
      }
    });
  }
}
