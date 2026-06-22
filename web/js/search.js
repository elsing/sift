import { currentFolder, openFolder } from './inbox.js';
import { getSelectedAccountIds } from './accountFilter.js';
import { openMailReaderById, setReaderBack } from './reader.js';
import { renderFolderTree, fetchFolders, cachedFolders, setLoading } from './folders.js';
import { fetchTags } from './tags.js';

let panel, input, scopeBtn, accountScopeEl, errorEl, results;
let progressWrap, progressFill, progressText, deepBtn, continueBtn;
let advancedToggle, advancedPanel, fromInput, sinceInput, beforeInput, folderPickerToggle, folderPicker, tagFilterEl;
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
  folderPicker = document.getElementById('searchFolderPicker');
  tagFilterEl = document.getElementById('searchTagFilter');

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
      return;
    }
    debounce = setTimeout(() => startSearch('light'), 400);
  };
  input.addEventListener('input', onFilterInput);
  fromInput.addEventListener('input', onFilterInput);
  sinceInput.addEventListener('change', onFilterInput);
  beforeInput.addEventListener('change', onFilterInput);
}

// A search needs free text, a sender filter, a date range, or some mix — not all of them.
function hasQuery() {
  return input.value.trim() !== '' || fromInput.value.trim() !== '' || sinceInput.value !== '' || beforeInput.value !== '';
}

// Folder picker only really makes sense scoped to one account (folder names are
// account-specific) — with several accounts selected, fall back to "all folders"
// rather than trying to merge unrelated folder trees into one picker.
async function openFolderPicker() {
  const showing = !folderPicker.classList.contains('hidden');
  if (showing) {
    folderPicker.classList.add('hidden');
    return;
  }

  const res = await fetch('/api/accounts');
  if (!res.ok) return;
  const accounts = await res.json();
  const selectedIds = getSelectedAccountIds();
  const inScope = selectedIds ? accounts.filter((a) => selectedIds.includes(a.id)) : accounts;
  if (inScope.length !== 1) {
    folderPicker.classList.remove('hidden');
    folderPicker.textContent = 'Pick a single account above (in the account filter) to choose specific folders.';
    return;
  }

  const accountId = inScope[0].id;
  folderPicker.classList.remove('hidden');
  const render = (folders) => {
    folderPicker.innerHTML = '';
    folderPicker.appendChild(renderFolderTree(folders, { checkSet: chosenFolders }));
  };
  const cached = cachedFolders(accountId);
  if (cached) render(cached);
  else setLoading(folderPicker, true);
  try {
    const folders = await fetchFolders(accountId);
    if (!folderPicker.classList.contains('hidden')) render(folders);
  } catch (err) {
    if (!cached) {
      folderPicker.textContent = '';
      errorEl.textContent = err.message;
    }
  }
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
  sinceInput.value = '';
  beforeInput.value = '';
  chosenFolders.clear();
  advancedPanel.classList.add('hidden');
  advancedToggle.textContent = 'Advanced filters ▾';
  folderPicker.classList.add('hidden');
  results.innerHTML = '';
  errorEl.textContent = '';
  progressWrap.classList.add('hidden');
  deepBtn.classList.add('hidden');
  continueBtn.classList.add('hidden');
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

  source.addEventListener('progress', (e) => {
    const { done, total } = JSON.parse(e.data);
    progressFill.style.width = (total ? Math.round((done / total) * 100) : 100) + '%';
    progressText.textContent = `Searched ${done} of ${total} folders · ${matches.length} match${matches.length === 1 ? '' : 'es'}`;
  });

  source.addEventListener('match', (e) => {
    matches.push(...JSON.parse(e.data));
    // results stream in whatever order each folder's IMAP round trip happens to
    // finish in — not date order — so re-sort before every render, not just at the end
    matches.sort((a, b) => (a.date < b.date ? 1 : -1));
    renderResults(matches);
  });

  source.addEventListener('complete', (e) => {
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
      progressWrap.classList.add('hidden');
    }
    // a quick header-only search came up short (or you just want to be thorough) —
    // offer the slower full-body search as a deliberate next step, not automatic
    if (mode === 'light') deepBtn.classList.remove('hidden');
    stopSearch();
  });

  source.onerror = () => {
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
        <div class="timestamp">${mail.time}</div>
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
}
