import { applyAccountFilter } from './inbox.js';

// Multi-account filter chips: "All" plus one chip per account. Selection can be all,
// one, or any mix — persisted locally so it survives a reload. Hidden entirely for a
// single-account setup, where a filter has nothing to do.
const STORAGE_KEY = 'accountFilter';

let container;
let accounts = [];
let selected = loadSelected(); // null means "all"

// Read by search.js so a search respects the same account selection as the inbox.
export function getSelectedAccountIds() {
  return selected;
}

function loadSelected() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return raw ? JSON.parse(raw) : null;
  } catch {
    return null;
  }
}

function saveSelected() {
  if (selected) localStorage.setItem(STORAGE_KEY, JSON.stringify(selected));
  else localStorage.removeItem(STORAGE_KEY);
}

export async function setupAccountFilter() {
  container = document.getElementById('accountFilterBar');
  await refreshAccountFilter();
}

// Re-fetches the account list and re-renders the chips — call after accounts are
// added/removed (from accounts.js) so the strip doesn't go stale.
export async function refreshAccountFilter() {
  const res = await fetch('/api/accounts');
  if (!res.ok) return;
  accounts = await res.json();

  if (accounts.length < 2) {
    container.classList.add('hidden');
    if (selected) {
      selected = null;
      saveSelected();
      applyAccountFilter(null);
    }
    return;
  }

  if (selected) {
    const validIds = new Set(accounts.map((a) => a.id));
    const trimmed = selected.filter((id) => validIds.has(id));
    selected = trimmed.length ? trimmed : null;
  }

  container.classList.remove('hidden');
  render();
  applyAccountFilter(selected);
}

function render() {
  container.innerHTML = '';
  container.appendChild(makeChip('All', selected === null, () => {
    selected = null;
    saveSelected();
    render();
    applyAccountFilter(null);
  }));

  for (const a of accounts) {
    const active = selected === null || selected.includes(a.id);
    container.appendChild(makeChip(a.email.split('@')[0], active, () => {
      const current = selected === null ? accounts.map((x) => x.id) : selected.slice();
      const idx = current.indexOf(a.id);
      if (idx === -1) current.push(a.id);
      else current.splice(idx, 1);
      if (current.length === 0) return; // always keep at least one account selected
      selected = current.length === accounts.length ? null : current;
      saveSelected();
      render();
      applyAccountFilter(selected);
    }));
  }
}

function makeChip(label, active, onClick) {
  const btn = document.createElement('button');
  btn.className = 'account-chip' + (active ? ' active' : '');
  btn.textContent = label;
  btn.addEventListener('click', onClick);
  return btn;
}
