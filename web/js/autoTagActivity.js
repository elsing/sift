// "Auto-tag activity" — the audit/undo surface for full-auto mode. Smart Tagging's
// own settings panel covers mode/delay/scan/suggestions and was already getting
// large; this is split out on purpose so the one thing full-auto mode actually needs
// (an easy way to see and reverse what it did on its own) has a home that isn't
// buried at the bottom of an already-long page.
import { fetchTagHistory, undoTagHistory } from './tags.js';
import { openMailReaderById, setReaderBack } from './reader.js';
import { withBusyButton } from './util.js';

let listEl, errorEl;

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
  undoBtn.textContent = 'Undo';
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

async function render() {
  errorEl.textContent = '';
  listEl.innerHTML = '<li class="folder-empty-status dot-loader">Loading</li>';
  let entries;
  try {
    entries = await fetchTagHistory('applied', null, 'smart_auto');
  } catch (err) {
    listEl.innerHTML = '';
    errorEl.textContent = err.message;
    return;
  }
  listEl.innerHTML = '';
  if (entries.length === 0) {
    listEl.innerHTML = '<li class="folder-empty-status">Nothing auto-tagged yet.</li>';
    return;
  }
  for (const entry of entries) listEl.appendChild(renderRow(entry));
}

export function setupAutoTagActivity() {
  listEl = document.getElementById('autoTagActivityList');
  errorEl = document.getElementById('autoTagActivityError');
  const panel = document.getElementById('autoTagActivityPanel');

  document.getElementById('openAutoTagActivityBtn').addEventListener('click', () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    panel.classList.remove('hidden');
    render();
  });
  const closePanel = () => {
    panel.classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeAutoTagActivityBtn').addEventListener('click', closePanel);
  document.getElementById('closeAutoTagActivityTopBtn').addEventListener('click', closePanel);
}
