// Smart spam's own panel — deliberately separate from Smart Tagging (smarttags.js)
// even though both ultimately read/write the same tag_history table: spam is its own
// concept the user thinks about independently (mode, scan, suggestions), not a
// sub-feature of regular tagging. Always one tag ("Spam"), so unlike smarttags.js's
// suggestions list this never needs to group rows by tag. The shared full-auto
// undo/activity view lives in autoTagActivity.js (toggle between tagging/spam there).
import { fetchTagHistory, acceptSuggestion, dismissSuggestion } from './tags.js';
import { openMailReaderById, setReaderBack } from './reader.js';
import { withBusyButton } from './util.js';
import { confirmModal } from './confirmModal.js';
import { pickFolders } from './folders.js';

const MODES = [
  { id: 'review', name: 'Review', icon: '👀' },
  { id: 'full_auto', name: 'Full-auto', icon: '⚡' },
];

const SCAN_SCOPES = [
  { id: 'all', name: 'All folders', icon: '🗂' },
  { id: 'inbox', name: 'Inbox only', icon: '📥' },
  { id: 'folders', name: 'Choose folders', icon: '📂' },
];
let scanScope = 'all';
const chosenScanFolders = new Set(); // only used when scanScope === 'folders'
let scanFoldersAccountId = null;

let modeOptions, errorEl, suggestionsList;
let scanScopeOptions, scanAccountPicker, scanProgress, scanProgressFill, scanProgressText, scanResults;
let currentSettings = { spamMode: 'review' };
let pendingSuggestions = [];
let activeScanSource = null;

async function fetchSettings() {
  const res = await fetch('/api/owner-settings');
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

// Shares /api/owner-settings with Smart Tagging's own mode/delay fields — has to send
// the whole object back, not just spamMode, or it'd clobber the others.
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
    btn.className = 'theme-option' + (currentSettings.spamMode === m.id ? ' selected' : '');
    btn.innerHTML = `<span class="theme-option-icon">${m.icon}</span><span>${m.name}</span>`;
    btn.addEventListener('click', async () => {
      for (const b of modeOptions.children) b.disabled = true;
      errorEl.textContent = '';
      try {
        await saveSettings({ spamMode: m.id });
        renderModeOptions();
      } catch (err) {
        errorEl.textContent = err.message;
        for (const b of modeOptions.children) b.disabled = false;
      }
    });
    modeOptions.appendChild(btn);
  }
}

function renderScanScopeOptions() {
  scanScopeOptions.innerHTML = '';
  for (const s of SCAN_SCOPES) {
    const btn = document.createElement('button');
    btn.className = 'theme-option' + (scanScope === s.id ? ' selected' : '');
    btn.innerHTML = `<span class="theme-option-icon">${s.icon}</span><span>${s.name}</span>`;
    btn.addEventListener('click', () => {
      scanScope = s.id;
      renderScanScopeOptions();
      // "Choose folders" needs to know which folders before "Scan for spam" makes any
      // sense — ask right away via the shared move-to-folder modal (checkbox mode)
      // rather than waiting for that button press, same as the tag scanner.
      if (s.id === 'folders') {
        chosenScanFolders.clear();
        scanFoldersAccountId = null;
        pickFolders('Folders to scan for spam', chosenScanFolders, (accountId) => {
          scanFoldersAccountId = accountId;
        });
      } else {
        chosenScanFolders.clear();
        scanFoldersAccountId = null;
      }
    });
    scanScopeOptions.appendChild(btn);
  }
}

function openMailFromPanel(mailId) {
  const panel = document.getElementById('smartSpamPanel');
  panel.classList.add('hidden');
  openMailReaderById(mailId);
  setReaderBack(() => panel.classList.remove('hidden'));
}

function renderSuggestionRow(entry) {
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
    dismiss.disabled = true;
    try {
      await withBusyButton(accept, 'Accepting…', () => acceptSuggestion(entry.id));
    } catch (err) {
      errorEl.textContent = err.message;
      dismiss.disabled = false;
      return;
    }
    renderSuggestions();
  });
  const dismiss = document.createElement('button');
  dismiss.className = 'dismiss-btn';
  dismiss.textContent = 'Dismiss';
  dismiss.addEventListener('click', async () => {
    accept.disabled = true;
    try {
      await withBusyButton(dismiss, 'Dismissing…', () => dismissSuggestion(entry.id));
    } catch (err) {
      errorEl.textContent = err.message;
      accept.disabled = false;
      return;
    }
    renderSuggestions();
  });
  actions.append(accept, dismiss);
  top.append(name, actions);
  const meta = document.createElement('div');
  meta.className = 'smart-history-meta';
  meta.textContent = `${entry.senderEmail}${entry.subject ? ' — ' + entry.subject : ''}${entry.folder ? ' (' + entry.folder + ')' : ''}`;
  li.append(top, meta);
  if (entry.mailId) {
    meta.classList.add('openable');
    meta.addEventListener('click', () => openMailFromPanel(entry.mailId));
  }
  return li;
}

// Mirrors smarttags.js's expandedTagGroups — same "stays expanded once you've opened
// it" behavior, just keyed by sender instead of tag (spam suggestions are always the
// one Spam tag, so grouping by tag would just be one giant group; sender is the axis
// that actually splits a pile of suggestions into something scannable — bulk spam
// runs routinely from the same sender/domain repeatedly).
const expandedSenderGroups = new Set();

function renderSuggestionGroup(senderEmail, entries) {
  const li = document.createElement('li');
  li.className = 'suggestion-group';

  const header = document.createElement('div');
  header.className = 'smart-history-row-top suggestion-group-header';
  const name = document.createElement('span');
  name.textContent = `${senderEmail || '(unknown sender)'} (${entries.length})`;

  const actions = document.createElement('span');
  actions.className = 'smart-suggestion-actions';
  const acceptGroup = document.createElement('button');
  acceptGroup.className = 'accept-btn';
  acceptGroup.textContent = 'Accept all';
  acceptGroup.addEventListener('click', async () => {
    dismissGroup.disabled = true;
    try {
      await withBusyButton(acceptGroup, '…', () => Promise.all(entries.map((e) => acceptSuggestion(e.id))));
    } catch (err) {
      errorEl.textContent = err.message;
      dismissGroup.disabled = false;
      return;
    }
    renderSuggestions();
  });
  const dismissGroup = document.createElement('button');
  dismissGroup.className = 'dismiss-btn';
  dismissGroup.textContent = 'Dismiss all';
  dismissGroup.addEventListener('click', async () => {
    if (entries.length > 1 && !(await confirmModal(`Dismiss all ${entries.length} suggestions from ${senderEmail}?`))) return;
    acceptGroup.disabled = true;
    try {
      await withBusyButton(dismissGroup, '…', () => Promise.all(entries.map((e) => dismissSuggestion(e.id))));
    } catch (err) {
      errorEl.textContent = err.message;
      acceptGroup.disabled = false;
      return;
    }
    renderSuggestions();
  });
  actions.append(acceptGroup, dismissGroup);
  header.append(name, actions);
  li.appendChild(header);

  const list = document.createElement('ul');
  list.className = 'accounts-list suggestion-group-rows';
  for (const entry of entries) list.appendChild(renderSuggestionRow(entry));

  // Same collapse-once-it's-actually-a-pile threshold as smarttags.js's tag groups.
  const collapsible = entries.length > 3;
  const startExpanded = expandedSenderGroups.has(senderEmail);
  if (collapsible && !startExpanded) list.classList.add('hidden');
  const toggle = document.createElement('button');
  toggle.type = 'button';
  toggle.className = 'suggestion-group-toggle';
  toggle.textContent = collapsible ? (startExpanded ? 'Hide ▴' : `Show ${entries.length} ▾`) : '';
  toggle.classList.toggle('hidden', !collapsible);
  toggle.addEventListener('click', () => {
    const showing = list.classList.toggle('hidden') === false;
    toggle.textContent = showing ? 'Hide ▴' : `Show ${entries.length} ▾`;
    if (showing) expandedSenderGroups.add(senderEmail);
    else expandedSenderGroups.delete(senderEmail);
  });
  li.append(toggle, list);
  return li;
}

async function renderSuggestions() {
  const acceptAllBtn = document.getElementById('acceptAllSpamBtn');
  const dismissAllBtn = document.getElementById('dismissAllSpamBtn');
  try {
    pendingSuggestions = await fetchTagHistory('suggested', null, 'spam_engine');
  } catch (err) {
    errorEl.textContent = err.message;
    return;
  }
  suggestionsList.innerHTML = '';
  acceptAllBtn.classList.toggle('hidden', pendingSuggestions.length < 2);
  dismissAllBtn.classList.toggle('hidden', pendingSuggestions.length < 2);
  if (pendingSuggestions.length === 0) {
    suggestionsList.innerHTML = '<li class="folder-empty-status">No pending suggestions.</li>';
    return;
  }
  const bySender = new Map(); // senderEmail -> entries[]
  for (const entry of pendingSuggestions) {
    const key = entry.senderEmail || '';
    if (!bySender.has(key)) bySender.set(key, []);
    bySender.get(key).push(entry);
  }
  for (const [senderEmail, entries] of bySender) {
    suggestionsList.appendChild(renderSuggestionGroup(senderEmail, entries));
  }
}

function renderScanSummary(summary) {
  scanResults.innerHTML = '';
  const stats = document.createElement('p');
  stats.className = 'smart-history-meta';
  stats.textContent = `Tagged ${summary.applied}, suggested ${summary.suggested}, learned from ${summary.trained} already-sorted mail.`;
  scanResults.appendChild(stats);
}

function runScan(accountId) {
  scanResults.innerHTML = '';
  scanProgress.classList.remove('hidden');
  scanProgressFill.style.width = '0%';
  scanProgressText.textContent = 'Counting emails…';

  const params = new URLSearchParams();
  if (scanScope === 'inbox') params.set('scope', 'inbox');
  else if (scanScope === 'folders') {
    for (const f of chosenScanFolders) params.append('folders', f);
  }
  const qs = params.toString();
  const source = new EventSource(`/api/accounts/${accountId}/scan-spam${qs ? '?' + qs : ''}`);
  activeScanSource = source;
  source.addEventListener('progress', (e) => {
    const { done, total } = JSON.parse(e.data);
    scanProgressFill.style.width = (total ? Math.round((done / total) * 100) : 100) + '%';
    scanProgressText.textContent = `Scanned ${done} of ${total} email${total === 1 ? '' : 's'}…`;
  });
  source.addEventListener('complete', (e) => {
    const summary = JSON.parse(e.data);
    scanProgress.classList.add('hidden');
    source.close();
    activeScanSource = null;
    renderScanSummary(summary);
    renderSuggestions();
  });
  source.addEventListener('error', () => {
    // Fires both for a real failure and for EventSource's own auto-retry attempts —
    // by the time this runs the source is already in a terminal state either way.
    scanProgress.classList.add('hidden');
    source.close();
    if (activeScanSource === source) {
      errorEl.textContent = "Scan didn't complete — try again.";
    }
    activeScanSource = null;
  });
}

async function setupScan() {
  // "Choose folders" already picked both the account and the folders up front, when
  // that scope option was clicked — nothing left to ask here.
  if (scanScope === 'folders') {
    if (!scanFoldersAccountId || chosenScanFolders.size === 0) {
      errorEl.textContent = 'Pick at least one folder first (tap "Choose folders" again).';
      return;
    }
    runScan(scanFoldersAccountId);
    return;
  }

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

export function setupSmartSpamPanel() {
  modeOptions = document.getElementById('spamModeOptions');
  errorEl = document.getElementById('smartSpamError');
  suggestionsList = document.getElementById('smartSpamSuggestions');
  scanScopeOptions = document.getElementById('spamScanScopeOptions');
  scanAccountPicker = document.getElementById('spamScanAccountPicker');
  scanProgress = document.getElementById('spamBootstrapProgress');
  scanProgressFill = document.getElementById('spamBootstrapProgressFill');
  scanProgressText = document.getElementById('spamBootstrapProgressText');
  scanResults = document.getElementById('spamScanResults');
  renderScanScopeOptions();

  document.getElementById('scanForSpamBtn').addEventListener('click', () => {
    errorEl.textContent = '';
    setupScan();
  });

  document.getElementById('cancelSpamScanBtn').addEventListener('click', () => {
    if (activeScanSource) {
      activeScanSource.close(); // also tears down the server side — handleScanSpam checks ctx.Err() between folders
      activeScanSource = null;
    }
    scanProgress.classList.add('hidden');
    scanProgressText.textContent = '';
  });

  document.getElementById('acceptAllSpamBtn').addEventListener('click', async (e) => {
    e.target.disabled = true;
    try {
      await Promise.all(pendingSuggestions.map((s) => acceptSuggestion(s.id)));
    } catch (err) {
      errorEl.textContent = err.message;
    }
    e.target.disabled = false;
    renderSuggestions();
  });

  document.getElementById('dismissAllSpamBtn').addEventListener('click', async (e) => {
    if (!(await confirmModal(`Dismiss all ${pendingSuggestions.length} pending suggestions?`))) return;
    e.target.disabled = true;
    try {
      await Promise.all(pendingSuggestions.map((s) => dismissSuggestion(s.id)));
    } catch (err) {
      errorEl.textContent = err.message;
    }
    e.target.disabled = false;
    renderSuggestions();
  });

  document.getElementById('openSmartSpamBtn').addEventListener('click', async () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    document.getElementById('smartSpamPanel').classList.remove('hidden');
    errorEl.textContent = '';
    try {
      currentSettings = await fetchSettings();
    } catch (err) {
      errorEl.textContent = err.message;
    }
    renderModeOptions();
    renderSuggestions();
  });

  const closePanel = () => {
    document.getElementById('smartSpamPanel').classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeSmartSpamBtn').addEventListener('click', closePanel);
  document.getElementById('closeSmartSpamTopBtn').addEventListener('click', closePanel);
}
