// "Auto activity" — the audit/undo surface for full-auto mode, covering both regular
// smart-tagging and the spam engine (toggled between, not merged — they're different
// concepts with different tag sets, just the same "full-auto did this, here's undo"
// shape). Split out on its own panel so this one thing full-auto mode actually needs
// doesn't get buried at the bottom of the Smart Tagging/Smart Spam settings pages.
import { fetchTagHistory, undoTagHistory, undoTagHistories } from './tags.js';
import { openMailReaderById, setReaderBack } from './reader.js';
import { withBusyButton } from './util.js';

const SOURCES = [
  { id: 'smart_auto', name: 'Tagging', icon: '🏷️' },
  { id: 'spam_engine', name: 'Spam', icon: '🚫' },
];
let activeSource = 'smart_auto';

let listEl, errorEl, sourceOptions;

function timeAgo(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '';
  const mins = Math.round((Date.now() - d.getTime()) / 60000);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

function renderRow(entry) {
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
  const undoBtn = document.createElement('button');
  undoBtn.className = 'dismiss-btn';
  undoBtn.textContent = activeSource === 'spam_engine' ? 'Not spam' : 'Undo';
  undoBtn.addEventListener('click', async () => {
    errorEl.textContent = '';
    try {
      await withBusyButton(undoBtn, 'Undoing…', () => undoTagHistory(entry.id));
    } catch (err) {
      errorEl.textContent = err.message;
      return;
    }
    render();
  });
  top.append(name, undoBtn);
  const meta = document.createElement('div');
  meta.className = 'smart-history-meta';
  meta.textContent = `${entry.senderEmail}${entry.subject ? ' — ' + entry.subject : ''}${entry.folder ? ' (' + entry.folder + ')' : ''} · ${timeAgo(entry.createdAt)}`;
  li.append(top, meta);
  if (entry.mailId) {
    meta.classList.add('openable');
    meta.addEventListener('click', () => {
      setReaderBack(() => document.getElementById('autoTagActivityPanel').classList.remove('hidden'));
      document.getElementById('autoTagActivityPanel').classList.add('hidden');
      openMailReaderById(entry.mailId);
    });
  }
  return li;
}

// Mirrors spam.js's expandedSenderGroups/renderSuggestionGroup — same "group by
// sender, collapse once it's a real pile" treatment, just for the applied/Undo side
// instead of the suggested/Accept side. Tagging activity stays flat (kept to what was
// actually asked for): grouping by sender doesn't carry the same "bulk spam run from
// one sender" rationale for regular tags, which are usually one-off per sender anyway.
const expandedSenderGroups = new Set();

function renderGroup(senderEmail, entries) {
  const li = document.createElement('li');
  li.className = 'suggestion-group';

  const header = document.createElement('div');
  header.className = 'smart-history-row-top suggestion-group-header';
  const name = document.createElement('span');
  name.textContent = `${senderEmail || '(unknown sender)'} (${entries.length})`;

  const undoGroup = document.createElement('button');
  undoGroup.className = 'dismiss-btn';
  undoGroup.textContent = 'Undo all';
  undoGroup.addEventListener('click', async () => {
    errorEl.textContent = '';
    try {
      await withBusyButton(undoGroup, '…', () => undoTagHistories(entries.map((e) => e.id)));
    } catch (err) {
      errorEl.textContent = err.message;
      return;
    }
    render();
  });
  header.append(name, undoGroup);
  li.appendChild(header);

  const list = document.createElement('ul');
  list.className = 'accounts-list suggestion-group-rows';
  for (const entry of entries) list.appendChild(renderRow(entry));

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

function renderSourceOptions() {
  sourceOptions.innerHTML = '';
  for (const s of SOURCES) {
    const btn = document.createElement('button');
    btn.className = 'theme-option' + (activeSource === s.id ? ' selected' : '');
    btn.innerHTML = `<span class="theme-option-icon">${s.icon}</span><span>${s.name}</span>`;
    btn.addEventListener('click', () => {
      if (activeSource === s.id) return;
      activeSource = s.id;
      renderSourceOptions();
      render();
    });
    sourceOptions.appendChild(btn);
  }
}

async function render() {
  errorEl.textContent = '';
  listEl.innerHTML = '<li class="folder-empty-status dot-loader">Loading</li>';
  let entries;
  try {
    entries = await fetchTagHistory('applied', null, activeSource);
  } catch (err) {
    listEl.innerHTML = '';
    errorEl.textContent = err.message;
    return;
  }
  listEl.innerHTML = '';
  if (entries.length === 0) {
    listEl.innerHTML = `<li class="folder-empty-status">Nothing ${activeSource === 'spam_engine' ? 'flagged' : 'auto-tagged'} yet.</li>`;
    return;
  }
  if (activeSource !== 'spam_engine') {
    for (const entry of entries) listEl.appendChild(renderRow(entry));
    return;
  }
  const bySender = new Map(); // senderEmail -> entries[]
  for (const entry of entries) {
    const key = entry.senderEmail || '';
    if (!bySender.has(key)) bySender.set(key, []);
    bySender.get(key).push(entry);
  }
  for (const [senderEmail, group] of bySender) {
    listEl.appendChild(renderGroup(senderEmail, group));
  }
}

export function setupAutoTagActivity() {
  listEl = document.getElementById('autoTagActivityList');
  errorEl = document.getElementById('autoTagActivityError');
  sourceOptions = document.getElementById('autoActivitySourceOptions');
  const panel = document.getElementById('autoTagActivityPanel');

  document.getElementById('openAutoTagActivityBtn').addEventListener('click', () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    panel.classList.remove('hidden');
    renderSourceOptions();
    render();
  });
  const closePanel = () => {
    panel.classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeAutoTagActivityBtn').addEventListener('click', closePanel);
  document.getElementById('closeAutoTagActivityTopBtn').addEventListener('click', closePanel);
}
