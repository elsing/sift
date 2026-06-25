// Smart spam's own panel — deliberately separate from Smart Tagging (smarttags.js)
// even though both ultimately read/write the same tag_history table: spam is its own
// concept the user thinks about independently (mode, scan, suggestions), not a
// sub-feature of regular tagging. Always one tag ("Spam"), so unlike smarttags.js's
// suggestions list this never needs to group rows by tag. The shared full-auto
// undo/activity view lives in autoTagActivity.js (toggle between tagging/spam there).
import { fetchTagHistory, acceptSuggestion, dismissSuggestion, clearSuggestion, acceptSuggestions, dismissSuggestions, clearSuggestions } from './tags.js';
import { openMailReaderById, setReaderBack } from './reader.js';
import { withBusyButton } from './util.js';
import { confirmModal } from './confirmModal.js';
import { pickFolders } from './folders.js';

const MODES = [
  { id: 'review', name: 'Review', icon: '👀' },
  { id: 'full_auto', name: 'Full-auto', icon: '⚡' },
];

// Mirrors smarttags.js's own WORD_PROFILE_WEIGHTINGS — same setting, same two options,
// just rendered into this panel's own button group too (it's a shared owner_settings
// field, not a separate spam-only one).
const WORD_PROFILE_WEIGHTINGS = [
  { id: 'plain', name: 'Plain', icon: '📊' },
  { id: 'distinctive', name: 'Distinctive', icon: '🎯' },
];

const SCAN_SCOPES = [
  { id: 'all', name: 'All folders', icon: '🗂' },
  { id: 'inbox', name: 'Inbox only', icon: '📥' },
  { id: 'folders', name: 'Choose folders', icon: '📂' },
];
let scanScope = 'all';
const chosenScanFolders = new Set(); // only used when scanScope === 'folders'
let scanFoldersAccountId = null;

let modeOptions, wordWeightingOptions, errorEl, suggestionsList;
let scanScopeOptions, scanChosenFoldersSummary, scanAccountPicker, scanProgress, scanProgressFill, scanProgressText, scanResults;
let currentSettings = { spamMode: 'review', wordProfileWeighting: 'plain' };
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

function renderWordWeightingOptions() {
  wordWeightingOptions.innerHTML = '';
  for (const w of WORD_PROFILE_WEIGHTINGS) {
    const btn = document.createElement('button');
    btn.className = 'theme-option' + (currentSettings.wordProfileWeighting === w.id ? ' selected' : '');
    btn.innerHTML = `<span class="theme-option-icon">${w.icon}</span><span>${w.name}</span>`;
    btn.addEventListener('click', async () => {
      for (const b of wordWeightingOptions.children) b.disabled = true;
      errorEl.textContent = '';
      try {
        await saveSettings({ wordProfileWeighting: w.id });
        renderWordWeightingOptions();
      } catch (err) {
        errorEl.textContent = err.message;
        for (const b of wordWeightingOptions.children) b.disabled = false;
      }
    });
    wordWeightingOptions.appendChild(btn);
  }
}

// "Choose folders" only ever highlighted the scope button itself — nothing showed
// which folders had actually been picked, so re-opening the picker (or just trying to
// remember) was the only way to check before hitting Scan.
function renderChosenFoldersSummary() {
  if (scanScope !== 'folders' || chosenScanFolders.size === 0) {
    scanChosenFoldersSummary.textContent = '';
    return;
  }
  scanChosenFoldersSummary.textContent = `Scanning: ${[...chosenScanFolders].join(', ')}`;
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
          renderChosenFoldersSummary();
        });
      } else {
        chosenScanFolders.clear();
        scanFoldersAccountId = null;
      }
      renderChosenFoldersSummary();
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
    resolveSuggestion(entry.id);
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
    resolveSuggestion(entry.id);
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
  } else {
    // No local cache row for this message (common right after a backup import, or any
    // time the background folder sync hasn't reached it yet) — tapping used to do
    // nothing at all, with zero indication why, which read as "this is just broken".
    meta.classList.add('openable');
    meta.title = "This mail isn't cached locally yet — open it from its folder, or run a scan/search to refresh the cache.";
    meta.addEventListener('click', () => {
      errorEl.textContent = "Can't open this one yet — it's not cached locally. Try finding it in its folder, or run Scan for spam again.";
    });
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
      await withBusyButton(acceptGroup, '…', () => acceptSuggestions(entries.map((e) => e.id)));
    } catch (err) {
      errorEl.textContent = err.message;
      dismissGroup.disabled = false;
      return;
    }
    resolveSuggestions(entries.map((e) => e.id));
  });
  const dismissGroup = document.createElement('button');
  dismissGroup.className = 'dismiss-btn';
  dismissGroup.textContent = 'Dismiss all';
  dismissGroup.addEventListener('click', async () => {
    if (entries.length > 1 && !(await confirmModal(`Dismiss all ${entries.length} suggestions from ${senderEmail}?`))) return;
    acceptGroup.disabled = true;
    try {
      await withBusyButton(dismissGroup, '…', () => dismissSuggestions(entries.map((e) => e.id)));
    } catch (err) {
      errorEl.textContent = err.message;
      acceptGroup.disabled = false;
      return;
    }
    resolveSuggestions(entries.map((e) => e.id));
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

// loadSuggestions does the real fetch — only needed when the list might have changed
// server-side (panel open, a scan completing). resolveSuggestion(s) handle the far
// more common single accept/dismiss case by updating the already-loaded cache
// directly — no round trip for what's otherwise a full (possibly large, since there's
// no hard cap on pending suggestions) refetch just to drop one row.
async function loadSuggestions() {
  // Same loading placeholder reader.js/autoTagActivity.js already use elsewhere — this
  // fetch can take a real moment (no hard cap on how many pending suggestions there
  // can be), and an empty list with nothing happening looked indistinguishable from
  // "there's nothing to show" while it was still loading.
  suggestionsList.innerHTML = '<li class="folder-empty-status dot-loader">Loading</li>';
  try {
    pendingSuggestions = await fetchTagHistory('suggested', null, 'spam_engine');
  } catch (err) {
    errorEl.textContent = err.message;
    suggestionsList.innerHTML = ''; // don't leave the loading placeholder stuck forever on a failed fetch
    return;
  }
  renderSuggestionsFromCache();
}

function resolveSuggestion(id) {
  pendingSuggestions = pendingSuggestions.filter((e) => e.id !== id);
  renderSuggestionsFromCache();
}

function resolveSuggestions(ids) {
  const idSet = new Set(ids);
  pendingSuggestions = pendingSuggestions.filter((e) => !idSet.has(e.id));
  renderSuggestionsFromCache();
}

function renderSuggestionsFromCache() {
  const acceptAllBtn = document.getElementById('acceptAllSpamBtn');
  const dismissAllBtn = document.getElementById('dismissAllSpamBtn');
  const clearAllBtn = document.getElementById('clearAllSpamBtn');
  suggestionsList.innerHTML = '';
  acceptAllBtn.classList.toggle('hidden', pendingSuggestions.length < 2);
  dismissAllBtn.classList.toggle('hidden', pendingSuggestions.length < 2);
  clearAllBtn.classList.toggle('hidden', pendingSuggestions.length < 2);
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

// activeScanAccountId is what Cancel posts against — the scan now runs server-side
// independent of this connection (see handleScanSpam's own comment, spam.go), so
// cancelling needs the account id, not just something to close client-side.
let activeScanAccountId = null;

// resumeRunningScan checks for a scan_jobs row already running for this owner (e.g.
// one a previous page load started, then reloaded away from) and reattaches to it.
// runOwnerJob drives any of the owner-wide job endpoints (handleOwnerJobSSE,
// scanjobs.go) — restore-stranded, cleanup-unconfirmed, and whatever else adopts the
// same pattern. One EventSource, real progress into statusEl, the same
// CONNECTING-vs-CLOSED reconnect distinction runScan's own 'error' handler uses (a
// dropped connection isn't the job failing — it keeps running server-side regardless,
// see handleOwnerJobSSE's own comment), and resolves/rejects once it actually finishes.
function runOwnerJob(url, statusEl) {
  return new Promise((resolve, reject) => {
    statusEl.textContent = 'Starting…';
    const source = new EventSource(url);
    source.addEventListener('progress', (e) => {
      const { done, total } = JSON.parse(e.data);
      statusEl.textContent = total ? `Processing ${done} of ${total}…` : 'Processing…';
    });
    source.addEventListener('complete', (e) => {
      source.close();
      resolve(JSON.parse(e.data));
    });
    source.addEventListener('cancelled', () => {
      source.close();
      reject(new Error('Cancelled.'));
    });
    source.addEventListener('error', () => {
      if (source.readyState === EventSource.CONNECTING) {
        statusEl.textContent = 'Reconnecting…';
        return;
      }
      source.close();
      reject(new Error("Didn't complete — try again."));
    });
  });
}

// Reattaches to either owner-wide job if one's already running (e.g. a previous page
// load started it, then reloaded away) — same idea as resumeRunningScan below, just
// for these two instead of the account-scoped folder scans.
async function resumeOwnerJobs() {
  const restoreStatus = document.getElementById('restoreStrandedSpamStatus');
  const cleanupStatus = document.getElementById('cleanupUnconfirmedSpamStatus');
  for (const [kind, url, statusEl] of [
    ['restore-stranded-spam', '/api/spam/restore-stranded', restoreStatus],
    ['cleanup-unconfirmed-spam', '/api/spam/cleanup-unconfirmed', cleanupStatus],
  ]) {
    try {
      const res = await fetch(`/api/scan-jobs/running?kind=${kind}`);
      if (res.status !== 200) continue;
      runOwnerJob(url, statusEl).then(
        () => { statusEl.textContent = 'Done.'; },
        () => {},
      );
    } catch {
      // best-effort — worst case the user just doesn't see a run they could've reattached to
    }
  }
}

async function resumeRunningScan() {
  try {
    const res = await fetch('/api/scan-jobs/running?kind=spam');
    if (res.status !== 200) return;
    const { accountId } = await res.json();
    runScan(accountId);
  } catch {
    // best-effort — worst case the user just doesn't see a scan they could've reattached to
  }
}

function runScan(accountId) {
  scanResults.innerHTML = '';
  scanProgress.classList.remove('hidden');
  scanProgressFill.style.width = '0%';
  scanProgressText.textContent = 'Counting emails…';
  activeScanAccountId = accountId;

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
    activeScanAccountId = null;
    renderScanSummary(summary);
    loadSuggestions(); // a scan can genuinely add new ones — this one does need the real fetch
  });
  source.addEventListener('cancelled', () => {
    scanProgress.classList.add('hidden');
    source.close();
    activeScanSource = null;
    activeScanAccountId = null;
  });
  source.addEventListener('error', () => {
    // EventSource fires 'error' both for a real, final failure and for its own
    // automatic reconnect attempts (a dropped connection through a VPN/proxy hop is
    // common, and not the same thing as the scan itself failing — the scan runs
    // server-side on its own lifetime now, independent of this one connection, see
    // handleScanSpam's own comment). readyState distinguishes the two: CONNECTING
    // means the browser is about to retry on its own, in which case the right move is
    // to just wait — reconnecting picks the job back up exactly where it left off,
    // not from scratch. Only CLOSED means it's actually given up.
    if (source.readyState === EventSource.CONNECTING) {
      scanProgressText.textContent = 'Reconnecting…';
      return;
    }
    scanProgress.classList.add('hidden');
    source.close();
    if (activeScanSource === source) {
      errorEl.textContent = "Scan didn't complete — try again.";
    }
    activeScanSource = null;
    activeScanAccountId = null;
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
  wordWeightingOptions = document.getElementById('spamWordProfileWeightingOptions');
  errorEl = document.getElementById('smartSpamError');
  suggestionsList = document.getElementById('smartSpamSuggestions');
  scanScopeOptions = document.getElementById('spamScanScopeOptions');
  scanChosenFoldersSummary = document.getElementById('spamScanChosenFoldersSummary');
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

  const restoreBtn = document.getElementById('restoreStrandedSpamBtn');
  const restoreStatus = document.getElementById('restoreStrandedSpamStatus');
  restoreBtn.addEventListener('click', () =>
    withBusyButton(restoreBtn, 'Restoring…', async () => {
      errorEl.textContent = '';
      try {
        const { restored } = await runOwnerJob('/api/spam/restore-stranded', restoreStatus);
        restoreStatus.textContent = restored > 0
          ? `Restored ${restored} mail${restored === 1 ? '' : 's'} to the inbox.`
          : 'Nothing to restore — no past dismissals are still stuck in Junk.';
      } catch (err) {
        errorEl.textContent = err.message;
        restoreStatus.textContent = '';
      }
    })
  );

  const cleanupBtn = document.getElementById('cleanupUnconfirmedSpamBtn');
  const cleanupStatus = document.getElementById('cleanupUnconfirmedSpamStatus');
  cleanupBtn.addEventListener('click', () =>
    withBusyButton(cleanupBtn, 'Checking…', async () => {
      errorEl.textContent = '';
      try {
        const { untagged } = await runOwnerJob('/api/spam/cleanup-unconfirmed', cleanupStatus);
        cleanupStatus.textContent = untagged > 0
          ? `Untagged ${untagged} mail${untagged === 1 ? '' : 's'} nothing had actually decided was spam.`
          : 'Nothing to undo — every current Spam tag is backed by a real decision.';
      } catch (err) {
        errorEl.textContent = err.message;
        cleanupStatus.textContent = '';
      }
    })
  );

  document.getElementById('cancelSpamScanBtn').addEventListener('click', () => {
    // The scan now runs on a server-lifetime context, independent of this connection
    // (see handleScanSpam's own comment, spam.go) — closing the EventSource alone no
    // longer stops it, so this has to actually ask the server to.
    if (activeScanAccountId) {
      fetch(`/api/accounts/${activeScanAccountId}/scan-spam/cancel`, { method: 'POST' });
    }
    if (activeScanSource) {
      activeScanSource.close();
      activeScanSource = null;
    }
    activeScanAccountId = null;
    scanProgress.classList.add('hidden');
    scanProgressText.textContent = '';
  });

  document.getElementById('acceptAllSpamBtn').addEventListener('click', async (e) => {
    e.target.disabled = true;
    const ids = pendingSuggestions.map((s) => s.id);
    try {
      await acceptSuggestions(ids);
    } catch (err) {
      errorEl.textContent = err.message;
    }
    e.target.disabled = false;
    resolveSuggestions(ids);
  });

  document.getElementById('dismissAllSpamBtn').addEventListener('click', async (e) => {
    if (!(await confirmModal(`Dismiss all ${pendingSuggestions.length} pending suggestions?`))) return;
    e.target.disabled = true;
    const ids = pendingSuggestions.map((s) => s.id);
    try {
      await dismissSuggestions(ids);
    } catch (err) {
      errorEl.textContent = err.message;
    }
    e.target.disabled = false;
    resolveSuggestions(ids);
  });

  document.getElementById('clearAllSpamBtn').addEventListener('click', async (e) => {
    if (!(await confirmModal(`Clear all ${pendingSuggestions.length} pending suggestions? This isn't a verdict either way — any of them can resurface on a future scan.`))) return;
    e.target.disabled = true;
    const ids = pendingSuggestions.map((s) => s.id);
    try {
      await clearSuggestions(ids);
    } catch (err) {
      errorEl.textContent = err.message;
    }
    e.target.disabled = false;
    resolveSuggestions(ids);
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
    renderWordWeightingOptions();
    loadSuggestions();
    if (!activeScanSource) resumeRunningScan(); // pick back up a scan a previous page load started and left running
    resumeOwnerJobs(); // same, for restore-stranded/cleanup-unconfirmed
  });

  const closePanel = () => {
    document.getElementById('smartSpamPanel').classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeSmartSpamBtn').addEventListener('click', closePanel);
  document.getElementById('closeSmartSpamTopBtn').addEventListener('click', closePanel);
}
