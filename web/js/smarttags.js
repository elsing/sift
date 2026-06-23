// Settings surface for the smart-tagging subsystem: the global mode (full-auto vs
// review), the auto-move delay, the live pending-suggestions queue, and a read-only
// history/audit list — see docs/smart-tagging.md for how the scoring underneath this
// actually works.
import { fetchTagHistory, acceptSuggestion, dismissSuggestion, blockSenderForSuggestion, createTag, setFolderTagRule, fetchMisplacedMail, moveMail, getMailTags, setMailTags } from './tags.js';
import { openMailReaderById, setReaderBack } from './reader.js';
import { withBusyButton } from './util.js';
import { pickFolders } from './folders.js';
import { confirmModal } from './confirmModal.js';

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
let scanFoldersAccountId = null; // which account chosenScanFolders belongs to

let modeOptions, delayInput, errorEl, suggestionsList, historyList, historySearch, misplacedList;
let scanBtn, scanScopeOptions, scanAccountPicker, scanProgress, scanProgressFill, scanProgressText, scanResults;
let runAutoMoveBtn, autoMoveResults;
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
      // Not withBusyButton here — these buttons hold inner <span> icon/label markup,
      // and that helper's textContent swap would flatten it permanently on a failed
      // save (the success path rebuilds fresh buttons via renderModeOptions anyway).
      // Disabling everyone in the group during the request is enough signal here.
      for (const b of modeOptions.children) b.disabled = true;
      errorEl.textContent = '';
      try {
        await saveSettings({ autoTagMode: m.id });
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
      // "Choose folders" needs to know which folders before "Scan for tags" makes any
      // sense — ask right away via the shared move-to-folder modal (checkbox mode)
      // rather than waiting for that button press.
      if (s.id === 'folders') {
        chosenScanFolders.clear();
        scanFoldersAccountId = null;
        pickFolders('Folders to scan', chosenScanFolders, (accountId) => {
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
    dismiss.disabled = true; // both buttons act on the same row — neither should be tappable mid-request
    try {
      await withBusyButton(accept, 'Accepting…', () => acceptSuggestion(entry.id));
    } catch (err) {
      errorEl.textContent = err.message;
      dismiss.disabled = false;
      return;
    }
    onResolved();
    renderMisplaced(); // accepting can tag mail that isn't in inbox — autoMoveTaggedMail won't touch that, so it shows up here instead
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
    onResolved();
  });
  actions.append(accept, dismiss);
  top.append(name, actions);
  const meta = document.createElement('div');
  meta.className = 'smart-history-meta';
  meta.textContent = `${entry.senderEmail}${entry.subject ? ' — ' + entry.subject : ''}${entry.folder ? ' (' + entry.folder + ')' : ''}`;
  li.append(top, meta);

  // Separate, smaller, and off to the side from Accept/Dismiss on purpose — dismiss
  // alone is purely "wrong for this email"; going further and blocking the whole
  // sender is a bigger, less-common call that should take a deliberate second tap,
  // not happen as a side effect of the everyday dismiss button.
  if (entry.senderEmail) {
    const blockRow = document.createElement('div');
    blockRow.className = 'smart-history-meta';
    const blockBtn = document.createElement('button');
    blockBtn.type = 'button';
    blockBtn.className = 'suggestion-group-toggle';
    blockBtn.textContent = `Don't suggest "${entry.tagName}" from ${entry.senderEmail} again`;
    blockBtn.addEventListener('click', async () => {
      try {
        await withBusyButton(blockBtn, '…', () => blockSenderForSuggestion(entry.id));
      } catch (err) {
        errorEl.textContent = err.message;
        return;
      }
      // withBusyButton already restores the button's own text by the time this runs,
      // so without an explicit confirmation here, the row just silently vanishes into
      // a full list re-render — which reads as "nothing happened" rather than as the
      // action having actually gone through.
      blockBtn.disabled = true;
      blockBtn.textContent = `Won't suggest "${entry.tagName}" from ${entry.senderEmail} again ✓`;
      setTimeout(onResolved, 900);
    });
    blockRow.appendChild(blockBtn);
    li.appendChild(blockRow);
  }
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

// Tracked at module scope so "Accept all" (a scan can easily leave dozens of these,
// and accepting them one at a time was the only option) doesn't need its own fetch.
let pendingSuggestions = [];
// Persists across renderSuggestions() calls (e.g. after accepting one row) — without
// this, expanding a noisy tag's group just to work through it, then accepting one,
// re-collapsed the group out from under you on every single accept.
const expandedTagGroups = new Set();

async function renderSuggestions() {
  const acceptAllBtn = document.getElementById('acceptAllSuggestionsBtn');
  const dismissAllBtn = document.getElementById('dismissAllSuggestionsBtn');
  try {
    // Spam gets its own dedicated queue (spam.js's Smart Spam panel) — excluded here
    // so it isn't suggested twice in two different places.
    pendingSuggestions = (await fetchTagHistory('suggested')).filter((e) => e.source !== 'spam_engine');
  } catch (err) {
    errorEl.textContent = err.message;
    return;
  }
  suggestionsList.innerHTML = '';
  acceptAllBtn.classList.toggle('hidden', pendingSuggestions.length < 2); // not worth a bulk button for just one
  dismissAllBtn.classList.toggle('hidden', pendingSuggestions.length < 2);
  if (pendingSuggestions.length === 0) {
    suggestionsList.innerHTML = '<li class="folder-empty-status">No pending suggestions.</li>';
    return;
  }
  // Grouped by tag, not one flat list — a scan can easily leave dozens of suggestions
  // for one sender/pattern, which buried every other tag's suggestions underneath it.
  // Each group gets its own Accept-all/Dismiss-all pair and collapses by default once
  // it's long enough to be the actual problem, so a single noisy tag doesn't have to
  // be scrolled past (or waded through one row at a time) to get at the rest.
  const byTag = new Map(); // tagId -> { tagName, tagColor, entries: [] }
  for (const entry of pendingSuggestions) {
    if (!byTag.has(entry.tagId)) byTag.set(entry.tagId, { tagName: entry.tagName, tagColor: entry.tagColor, entries: [] });
    byTag.get(entry.tagId).entries.push(entry);
  }
  for (const [tagId, { tagName, tagColor, entries }] of byTag) {
    suggestionsList.appendChild(renderSuggestionGroup(tagId, tagName, tagColor, entries));
  }
}

function renderSuggestionGroup(tagId, tagName, tagColor, entries) {
  const li = document.createElement('li');
  li.className = 'suggestion-group';

  const header = document.createElement('div');
  header.className = 'smart-history-row-top suggestion-group-header';
  const dot = document.createElement('span');
  dot.className = 'tag-sheet-dot';
  dot.style.display = 'inline-block';
  dot.style.marginRight = '4px';
  dot.style.background = tagColor;
  const name = document.createElement('span');
  name.append(dot, document.createTextNode(`${tagName} (${entries.length})`));

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
    renderMisplaced();
  });
  const dismissGroup = document.createElement('button');
  dismissGroup.className = 'dismiss-btn';
  dismissGroup.textContent = 'Dismiss all';
  dismissGroup.addEventListener('click', async () => {
    if (entries.length > 1 && !(await confirmModal(`Dismiss all ${entries.length} "${tagName}" suggestions?`))) return;
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
  for (const entry of entries) {
    list.appendChild(renderSuggestionRow(entry, renderSuggestions));
  }

  // Collapsed by default once a tag's pile is the actual problem (more than 3) — the
  // group header with its own bulk actions is still right there either way, so
  // collapsing costs nothing if you did want to look through them individually. Once
  // you've explicitly expanded a tag's group, that choice sticks across re-renders
  // (e.g. accepting one row in it) instead of snapping back to collapsed every time.
  const collapsible = entries.length > 3;
  const startExpanded = expandedTagGroups.has(tagId);
  if (collapsible && !startExpanded) list.classList.add('hidden');
  const toggle = document.createElement('button');
  toggle.type = 'button';
  toggle.className = 'suggestion-group-toggle';
  toggle.textContent = collapsible ? (startExpanded ? 'Hide ▴' : `Show ${entries.length} ▾`) : '';
  toggle.classList.toggle('hidden', !collapsible);
  toggle.addEventListener('click', () => {
    const showing = list.classList.toggle('hidden') === false;
    toggle.textContent = showing ? 'Hide ▴' : `Show ${entries.length} ▾`;
    if (showing) expandedTagGroups.add(tagId);
    else expandedTagGroups.delete(tagId);
  });
  li.append(toggle, list);
  return li;
}

// Tracked at module scope for the same "Move all" reason pendingSuggestions is.
let misplacedMail = [];

function renderMisplacedRow(entry, onResolved) {
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
  const moveBtn = document.createElement('button');
  moveBtn.className = 'create-tag-btn'; // self-contained accent-bordered pill — see its definition for why this isn't relying on an ancestor class for base styling
  moveBtn.textContent = `Move to ${entry.destinationFolder}`;
  moveBtn.addEventListener('click', async () => {
    try {
      await withBusyButton(moveBtn, 'Moving…', () => moveMail(entry.mailId, entry.destinationFolder));
      onResolved();
    } catch (err) {
      errorEl.textContent = err.message;
    }
  });
  // The other way to resolve a mismatch — this mail just shouldn't have this tag at
  // all, rather than belonging in its destination folder. Misplaced mail is a live
  // query against current state (no stored suggestion row to dismiss), so removing
  // the tag is what actually makes the mismatch go away for good, not just hide it
  // from this one render.
  const rejectBtn = document.createElement('button');
  rejectBtn.className = 'dismiss-btn';
  rejectBtn.textContent = 'Remove tag';
  rejectBtn.addEventListener('click', async () => {
    moveBtn.disabled = true;
    try {
      await withBusyButton(rejectBtn, 'Removing…', async () => {
        const tags = await getMailTags(entry.mailId);
        await setMailTags(entry.mailId, tags.filter((t) => t.id !== entry.tagId).map((t) => t.id));
      });
      onResolved();
    } catch (err) {
      errorEl.textContent = err.message;
      moveBtn.disabled = false;
    }
  });
  top.append(name, moveBtn, rejectBtn);
  const meta = document.createElement('div');
  meta.className = 'smart-history-meta';
  meta.textContent = `${entry.sender}${entry.subject ? ' — ' + entry.subject : ''} (currently in ${entry.currentFolder})`;
  li.append(top, meta);
  return li;
}

async function renderMisplaced() {
  const relocateAllBtn = document.getElementById('relocateAllBtn');
  try {
    misplacedMail = await fetchMisplacedMail();
  } catch (err) {
    errorEl.textContent = err.message;
    return;
  }
  misplacedList.innerHTML = '';
  relocateAllBtn.classList.toggle('hidden', misplacedMail.length < 2);
  if (misplacedMail.length === 0) {
    misplacedList.innerHTML = '<li class="folder-empty-status">Nothing misplaced right now.</li>';
    return;
  }
  for (const entry of misplacedMail) {
    misplacedList.appendChild(renderMisplacedRow(entry, renderMisplaced));
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
// Tracked at module scope so the Cancel button (wired in setupSmartTaggingPanel) can
// reach whatever scan is currently running.
let activeScanSource = null;

function runScan(accountId) {
  scanResults.innerHTML = '';
  scanProgress.classList.remove('hidden');
  scanProgressFill.style.width = '0%';
  scanProgressText.textContent = 'Listing folders…';

  const params = new URLSearchParams();
  if (scanScope === 'inbox') params.set('scope', 'inbox');
  else if (scanScope === 'folders') {
    for (const f of chosenScanFolders) params.append('folders', f);
  }
  const qs = params.toString();
  const source = new EventSource(`/api/accounts/${accountId}/scan-tags${qs ? '?' + qs : ''}`);
  activeScanSource = source;
  source.addEventListener('progress', (e) => {
    const { done, total } = JSON.parse(e.data);
    if (done < 0) {
      // done = -1 is the server's signal it's moved on to scoring everything it found
      // against tag history — a separate, can-take-a-while phase with no per-item
      // progress of its own, but still needs *something* on screen so it doesn't look
      // stuck at 100%.
      scanProgressFill.style.width = '100%';
      scanProgressText.textContent = `Scoring ${total} mail${total === 1 ? '' : 's'}…`;
      return;
    }
    scanProgressFill.style.width = (total ? Math.round((done / total) * 100) : 100) + '%';
    scanProgressText.textContent = `Scanned ${done} of ${total} folders`;
  });
  source.addEventListener('complete', (e) => {
    const summary = JSON.parse(e.data);
    scanProgress.classList.add('hidden');
    source.close();
    activeScanSource = null;
    renderScanSummary(summary, accountId);
    renderMisplaced();
    renderSuggestions();
    loadHistory();
  });
  source.addEventListener('error', () => {
    // EventSource fires 'error' on the browser's own auto-retry attempts too, not just
    // a hard failure — but closing it ourselves (Cancel, or here) means readyState is
    // already CLOSED by the time this runs, so there's nothing left to distinguish; a
    // closed source's error event is the end of the line either way.
    scanProgress.classList.add('hidden');
    source.close();
    if (activeScanSource === source) {
      errorEl.textContent = "Scan didn't complete — try again.";
    }
    activeScanSource = null;
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
    // Stacked label-then-button, not a side-by-side row — a long sender address plus
    // a multi-segment folder path routinely overflowed an .account-row's single line,
    // forcing sideways scrolling to even see the button.
    const row = document.createElement('div');
    row.className = 'scan-candidate-row';
    const label = document.createElement('span');
    label.textContent = `${candidate.count} from ${candidate.senderOrDomain} are filed in "${candidate.folder}"`;

    // Editable, not just an auto-derived name applied silently — the folder's last
    // path segment is a reasonable starting guess, but "create tag" used to just go
    // ahead and use it with no way to see or change it first.
    const nameInput = document.createElement('input');
    nameInput.type = 'text';
    nameInput.className = 'scan-candidate-name-input';
    nameInput.value = candidate.folder.split('/').pop();
    nameInput.setAttribute('aria-label', 'Tag name');

    const createBtn = document.createElement('button');
    createBtn.className = 'create-tag-btn';
    createBtn.textContent = 'Create tag';
    createBtn.addEventListener('click', async () => {
      const name = nameInput.value.trim();
      if (!name) {
        nameInput.focus();
        return;
      }
      let tag;
      try {
        await withBusyButton(createBtn, 'Creating…', async () => {
          tag = await createTag(name);
          await setFolderTagRule(accountId, candidate.folder, tag.id);
        });
      } catch (err) {
        errorEl.textContent = err.message;
        return;
      }
      // Replaces the row instead of just removing it — "it disappeared" isn't
      // confirmation of what actually happened, naming the tag and the folder link
      // explicitly is.
      row.innerHTML = '';
      row.className = 'scan-candidate-row-done';
      const dot = document.createElement('span');
      dot.className = 'tag-sheet-dot';
      dot.style.display = 'inline-block';
      dot.style.marginRight = '6px';
      dot.style.background = tag.color;
      const confirmation = document.createElement('span');
      confirmation.append(dot, document.createTextNode(`Created "${tag.name}" — mail moved here auto-tags, mail tagged "${tag.name}" auto-moves to "${candidate.folder}"`));
      row.appendChild(confirmation);
    });
    row.append(label, nameInput, createBtn);
    scanResults.appendChild(row);
  }
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

export function setupSmartTaggingPanel() {
  modeOptions = document.getElementById('autoTagModeOptions');
  delayInput = document.getElementById('autoMoveDelayInput');
  errorEl = document.getElementById('smartTaggingSettingsError');
  suggestionsList = document.getElementById('smartTaggingSuggestions');
  historyList = document.getElementById('smartTaggingHistory');
  misplacedList = document.getElementById('misplacedMailList');
  scanBtn = document.getElementById('scanForTagsBtn');
  scanScopeOptions = document.getElementById('scanScopeOptions');
  scanAccountPicker = document.getElementById('scanAccountPicker');
  scanProgress = document.getElementById('scanProgress');
  scanProgressFill = document.getElementById('scanProgressFill');
  scanProgressText = document.getElementById('scanProgressText');
  scanResults = document.getElementById('scanResults');
  runAutoMoveBtn = document.getElementById('runAutoMoveBtn');
  autoMoveResults = document.getElementById('autoMoveResults');
  renderScanScopeOptions();
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

  document.getElementById('cancelScanBtn').addEventListener('click', () => {
    if (activeScanSource) {
      activeScanSource.close(); // also tears down the server side — handleScanTags checks ctx.Err() between folders
      activeScanSource = null;
    }
    scanProgress.classList.add('hidden');
    scanProgressText.textContent = '';
  });

  document.getElementById('acceptAllSuggestionsBtn').addEventListener('click', async (e) => {
    e.target.disabled = true;
    try {
      await Promise.all(pendingSuggestions.map((s) => acceptSuggestion(s.id)));
    } catch (err) {
      errorEl.textContent = err.message;
    }
    e.target.disabled = false;
    renderSuggestions();
    renderMisplaced(); // accepting can tag mail that isn't in inbox — autoMoveTaggedMail won't touch that, so it shows up here instead
  });

  document.getElementById('dismissAllSuggestionsBtn').addEventListener('click', async (e) => {
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

  // autoMoveTaggedMail normally only runs opportunistically, piggybacking on a sync —
  // this triggers it on demand (still respecting the configured delay, it just
  // doesn't wait for the next sync to check) so it's not invisible/unverifiable.
  runAutoMoveBtn.addEventListener('click', async () => {
    errorEl.textContent = '';
    runAutoMoveBtn.disabled = true;
    runAutoMoveBtn.textContent = 'Moving…';
    try {
      const res = await fetch('/api/auto-move/run', { method: 'POST' });
      if (!res.ok) throw new Error(await res.text());
      const { moved, details } = await res.json();
      runAutoMoveBtn.textContent = moved > 0 ? `Moved ${moved}` : 'Nothing to move yet';
      setTimeout(() => { runAutoMoveBtn.textContent = 'Move tagged mail now'; }, 2000);
      // A bare count doesn't say what just happened — list what actually moved so it's
      // not "the button said 37, no idea what" the next time it's pressed.
      autoMoveResults.innerHTML = '';
      for (const d of details || []) {
        const li = document.createElement('li');
        li.className = 'smart-history-meta';
        li.textContent = `${d.sender}${d.subject ? ' — ' + d.subject : ''} → "${d.tag}" (${d.folder})`;
        autoMoveResults.appendChild(li);
      }
    } catch (err) {
      errorEl.textContent = err.message;
      runAutoMoveBtn.textContent = 'Move tagged mail now';
    }
    runAutoMoveBtn.disabled = false;
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
    renderMisplaced();
    renderSuggestions();
    // History stays collapsed until expanded (see historyToggleBtn) — no point
    // fetching/rendering a list nobody's asked to see yet.
  });

  document.getElementById('relocateAllBtn').addEventListener('click', async (e) => {
    try {
      await withBusyButton(e.target, 'Moving…', () =>
        Promise.all(misplacedMail.map((m) => moveMail(m.mailId, m.destinationFolder))));
    } catch (err) {
      errorEl.textContent = err.message;
    }
    renderMisplaced();
  });

  const closePanel = () => {
    document.getElementById('smartTaggingPanel').classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeSmartTaggingBtn').addEventListener('click', closePanel);
  document.getElementById('closeSmartTaggingTopBtn').addEventListener('click', closePanel);
}
