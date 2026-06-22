// Settings surface for the smart-tagging subsystem: the global mode (full-auto vs
// review), the auto-move delay, the live pending-suggestions queue, and a read-only
// history/audit list — see docs/smart-tagging.md for how the scoring underneath this
// actually works.
import { fetchTagHistory, acceptSuggestion, dismissSuggestion, createTag, setFolderTagRule } from './tags.js';
import { openMailReaderById, setReaderBack } from './reader.js';

const MODES = [
  { id: 'review', name: 'Review', icon: '👀' },
  { id: 'full_auto', name: 'Full-auto', icon: '⚡' },
];

let modeOptions, delayInput, errorEl, suggestionsList, historyList, historySearch;
let scanBtn, scanAccountPicker, scanProgress, scanProgressFill, scanProgressText, scanResults;
let currentSettings = { autoTagMode: 'review', autoMoveDelayDays: 3 };

async function fetchSettings() {
  const res = await fetch('/api/owner-settings');
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

async function saveSettings(changes) {
  const next = { ...currentSettings, ...changes };
  const res = await fetch('/api/owner-settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(next),
  });
  if (!res.ok) throw new Error(await res.text());
  currentSettings = next;
}

function renderModeOptions() {
  modeOptions.innerHTML = '';
  for (const m of MODES) {
    const btn = document.createElement('button');
    btn.className = 'theme-option' + (currentSettings.autoTagMode === m.id ? ' selected' : '');
    btn.innerHTML = `<span class="theme-option-icon">${m.icon}</span><span>${m.name}</span>`;
    btn.addEventListener('click', async () => {
      errorEl.textContent = '';
      try {
        await saveSettings({ autoTagMode: m.id });
        renderModeOptions();
      } catch (err) {
        errorEl.textContent = err.message;
      }
    });
    modeOptions.appendChild(btn);
  }
}

function timeAgo(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '';
  const mins = Math.round((Date.now() - d.getTime()) / 60000);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

// Same two-line layout as a history row (tag name + colored dot on top, sender/subject
// below) so it's unambiguous which tag each Accept/Dismiss pair belongs to — the
// previous single-line version crammed a long "sender: subject" string right up
// against two unlabeled glyph buttons, which read as unrelated to the row.
function renderSuggestionRow(entry, onResolved) {
  const li = document.createElement('li');
  li.className = 'smart-history-row';
  const top = document.createElement('div');
  top.className = 'smart-history-row-top';
  const dot = document.createElement('span');
  dot.className = 'tag-sheet-dot';
  dot.style.display = 'inline-block';
  dot.style.marginRight = '4px';
  dot.style.background = entry.tagColor;
  const name = document.createElement('span');
  name.append(dot, document.createTextNode(entry.tagName));
  const actions = document.createElement('span');
  actions.className = 'smart-suggestion-actions';
  const accept = document.createElement('button');
  accept.className = 'accept-btn';
  accept.textContent = 'Accept';
  accept.addEventListener('click', async () => {
    await acceptSuggestion(entry.id);
    onResolved();
  });
  const dismiss = document.createElement('button');
  dismiss.className = 'dismiss-btn';
  dismiss.textContent = 'Dismiss';
  dismiss.addEventListener('click', async () => {
    await dismissSuggestion(entry.id);
    onResolved();
  });
  actions.append(accept, dismiss);
  top.append(name, actions);
  const meta = document.createElement('div');
  meta.className = 'smart-history-meta';
  meta.textContent = `${entry.senderEmail}${entry.subject ? ' — ' + entry.subject : ''}${entry.folder ? ' (' + entry.folder + ')' : ''}`;
  li.append(top, meta);
  makeRowOpenable(li, meta, entry);
  return li;
}

// Tapping a row opens the actual mail — same reader, same back-button behavior as
// everywhere else (it returns here, not to the inbox) — so a suggestion can actually
// be read and confirmed, not just accepted/dismissed blind. mailId is only present if
// that mail's still in the local cache; if it's been archived/deleted/moved since, the
// row just isn't clickable rather than opening to an error.
function makeRowOpenable(li, clickTarget, entry) {
  if (!entry.mailId) return;
  clickTarget.classList.add('openable');
  clickTarget.addEventListener('click', () => {
    const panel = document.getElementById('smartTaggingPanel');
    panel.classList.add('hidden');
    // openMailReaderById resets onBack to null as its very first line — setReaderBack
    // has to come after calling it, not before, or this callback gets wiped out
    // immediately and Back falls through to its default (the inbox).
    openMailReaderById(entry.mailId);
    setReaderBack(() => panel.classList.remove('hidden'));
  });
}

async function renderSuggestions() {
  let entries;
  try {
    entries = await fetchTagHistory('suggested');
  } catch (err) {
    errorEl.textContent = err.message;
    return;
  }
  suggestionsList.innerHTML = '';
  if (entries.length === 0) {
    suggestionsList.innerHTML = '<li class="folder-empty-status">No pending suggestions.</li>';
    return;
  }
  for (const entry of entries) {
    suggestionsList.appendChild(renderSuggestionRow(entry, renderSuggestions));
  }
}

const SOURCE_LABELS = {
  manual: 'Manual', folder_rule: 'Folder rule', smart_auto: 'Auto-tagged',
  smart_suggested: 'Suggested', scan_inferred: 'Scan',
};

// Fetched once per panel-open/expand, then filtered client-side as you type — the
// list only gets longer over time, and re-fetching on every keystroke for a plain
// substring filter is pointless round-tripping.
let historyEntries = [];

async function loadHistory() {
  try {
    const entries = await fetchTagHistory();
    // Still-pending suggestions already have their own section above with Accept/
    // Dismiss actions — showing them here too was a flat-out duplicate of that list.
    historyEntries = entries.filter((e) => e.status !== 'suggested');
  } catch (err) {
    errorEl.textContent = err.message;
    historyEntries = [];
  }
  renderHistory();
}

function renderHistory() {
  const term = historySearch.value.trim().toLowerCase();
  const entries = term
    ? historyEntries.filter((e) =>
        e.tagName.toLowerCase().includes(term) ||
        e.senderEmail.toLowerCase().includes(term) ||
        (e.subject && e.subject.toLowerCase().includes(term)) ||
        (e.folder && e.folder.toLowerCase().includes(term)))
    : historyEntries;

  historyList.innerHTML = '';
  if (entries.length === 0) {
    historyList.innerHTML = `<li class="folder-empty-status">${term ? 'No matches.' : 'Nothing logged yet.'}</li>`;
    return;
  }
  for (const entry of entries) {
    const li = document.createElement('li');
    li.className = 'smart-history-row';
    const top = document.createElement('div');
    top.className = 'smart-history-row-top';
    const dot = document.createElement('span');
    dot.className = 'tag-sheet-dot';
    dot.style.display = 'inline-block';
    dot.style.marginRight = '4px';
    dot.style.background = entry.tagColor;
    const name = document.createElement('span');
    name.append(dot, document.createTextNode(entry.tagName));
    const badge = document.createElement('span');
    badge.className = 'smart-history-badge';
    badge.textContent = `${SOURCE_LABELS[entry.source] || entry.source} · ${entry.status}`;
    top.append(name, badge);
    const meta = document.createElement('div');
    meta.className = 'smart-history-meta';
    meta.textContent = `${entry.senderEmail}${entry.subject ? ' — ' + entry.subject : ''}${entry.folder ? ' (' + entry.folder + ')' : ''} · ${timeAgo(entry.createdAt)}`;
    li.append(top, meta);
    makeRowOpenable(li, meta, entry);
    historyList.appendChild(li);
  }
}

// Runs the scan against one account, streaming progress over SSE, then renders a
// results summary — applied/suggested counts, plus any brand-new tag candidates
// surfaced from folders the user has already hand-sorted mail into (one tap to create
// the tag and the folder rule that goes with it, rather than two separate trips
// through Settings).
function runScan(accountId) {
  scanResults.innerHTML = '';
  scanProgress.classList.remove('hidden');
  scanProgressFill.style.width = '0%';
  scanProgressText.textContent = 'Listing folders…';

  const source = new EventSource(`/api/accounts/${accountId}/scan-tags`);
  source.addEventListener('progress', (e) => {
    const { done, total } = JSON.parse(e.data);
    scanProgressFill.style.width = (total ? Math.round((done / total) * 100) : 100) + '%';
    scanProgressText.textContent = `Scanned ${done} of ${total} folders`;
  });
  source.addEventListener('complete', (e) => {
    const summary = JSON.parse(e.data);
    scanProgress.classList.add('hidden');
    source.close();
    renderScanSummary(summary, accountId);
    renderSuggestions();
    loadHistory();
  });
  source.addEventListener('error', () => {
    scanProgress.classList.add('hidden');
    source.close();
    errorEl.textContent = "Scan didn't complete — try again.";
  });
}

function renderScanSummary(summary, accountId) {
  scanResults.innerHTML = '';
  const stats = document.createElement('p');
  stats.className = 'smart-history-meta';
  stats.textContent = `Applied ${summary.applied}, suggested ${summary.suggested}.`;
  scanResults.appendChild(stats);
  if (!summary.newTagCandidates || summary.newTagCandidates.length === 0) return;

  for (const candidate of summary.newTagCandidates) {
    const row = document.createElement('div');
    row.className = 'account-row';
    const label = document.createElement('span');
    label.textContent = `${candidate.count} from ${candidate.senderOrDomain} are filed in "${candidate.folder}"`;
    const createBtn = document.createElement('button');
    createBtn.className = 'create-tag-btn';
    createBtn.textContent = 'Create tag';
    createBtn.addEventListener('click', async () => {
      createBtn.disabled = true;
      try {
        const name = candidate.folder.split('/').pop();
        const tag = await createTag(name);
        await setFolderTagRule(accountId, candidate.folder, tag.id);
        row.remove();
      } catch (err) {
        errorEl.textContent = err.message;
        createBtn.disabled = false;
      }
    });
    row.append(label, createBtn);
    scanResults.appendChild(row);
  }
}

async function setupScan() {
  const res = await fetch('/api/accounts');
  if (!res.ok) return;
  const accounts = await res.json();
  scanAccountPicker.innerHTML = '';
  if (accounts.length === 0) {
    errorEl.textContent = 'Add an account first.';
    return;
  }
  if (accounts.length === 1) {
    runScan(accounts[0].id);
    return;
  }
  for (const a of accounts) {
    const btn = document.createElement('button');
    btn.className = 'settings-row-btn';
    btn.textContent = a.email;
    btn.addEventListener('click', () => {
      scanAccountPicker.innerHTML = '';
      runScan(a.id);
    });
    scanAccountPicker.appendChild(btn);
  }
}

export function setupSmartTaggingPanel() {
  modeOptions = document.getElementById('autoTagModeOptions');
  delayInput = document.getElementById('autoMoveDelayInput');
  errorEl = document.getElementById('smartTaggingSettingsError');
  suggestionsList = document.getElementById('smartTaggingSuggestions');
  historyList = document.getElementById('smartTaggingHistory');
  scanBtn = document.getElementById('scanForTagsBtn');
  scanAccountPicker = document.getElementById('scanAccountPicker');
  scanProgress = document.getElementById('scanProgress');
  scanProgressFill = document.getElementById('scanProgressFill');
  scanProgressText = document.getElementById('scanProgressText');
  scanResults = document.getElementById('scanResults');
  const historyToggleBtn = document.getElementById('historyToggleBtn');
  const historySection = document.getElementById('historySection');
  historySearch = document.getElementById('smartHistorySearch');

  let historyLoaded = false;
  historyToggleBtn.addEventListener('click', () => {
    const showing = historySection.classList.toggle('hidden') === false;
    historyToggleBtn.textContent = showing ? 'History ▴' : 'History ▾';
    if (showing && !historyLoaded) {
      historyLoaded = true;
      loadHistory();
    }
  });
  historySearch.addEventListener('input', renderHistory);

  scanBtn.addEventListener('click', () => {
    errorEl.textContent = '';
    setupScan();
  });

  delayInput.addEventListener('change', async () => {
    const days = parseInt(delayInput.value, 10);
    if (isNaN(days) || days < 0) {
      delayInput.value = currentSettings.autoMoveDelayDays;
      return;
    }
    errorEl.textContent = '';
    try {
      await saveSettings({ autoMoveDelayDays: days });
    } catch (err) {
      errorEl.textContent = err.message;
      delayInput.value = currentSettings.autoMoveDelayDays;
    }
  });

  document.getElementById('openSmartTaggingBtn').addEventListener('click', async () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    document.getElementById('smartTaggingPanel').classList.remove('hidden');
    errorEl.textContent = '';
    try {
      currentSettings = await fetchSettings();
    } catch (err) {
      errorEl.textContent = err.message;
    }
    delayInput.value = currentSettings.autoMoveDelayDays;
    renderModeOptions();
    renderSuggestions();
    // History stays collapsed until expanded (see historyToggleBtn) — no point
    // fetching/rendering a list nobody's asked to see yet.
  });
  const closePanel = () => document.getElementById('smartTaggingPanel').classList.add('hidden');
  document.getElementById('closeSmartTaggingBtn').addEventListener('click', closePanel);
  document.getElementById('closeSmartTaggingTopBtn').addEventListener('click', closePanel);
}
