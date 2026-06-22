import { dryRunHeaders } from './util.js';
import { removeMailById, openFolder } from './inbox.js';

// Folder lists rarely change, so every sheet open here uses cache-then-network:
// show the last-known tree instantly (no spinner, no flash) while quietly refetching
// in the background, swapping in fresh data only if it actually differs.
const folderCache = new Map(); // accountId -> folders array

export async function fetchFolders(accountId) {
  const res = await fetch(`/api/accounts/${accountId}/folders`);
  if (!res.ok) throw new Error(await res.text());
  const folders = await res.json();
  folderCache.set(accountId, folders);
  return folders;
}

export function cachedFolders(accountId) {
  return folderCache.get(accountId);
}

// A skeleton at roughly the height of a real folder tree, not "Loading…" text —
// swapping a couple of words for the actual tree was the "double bump": the sheet
// slides up to a tiny height, then immediately jumps taller once real content lands.
// Matching heights up front makes that swap barely noticeable instead of jarring.
const SKELETON_WIDTHS = [62, 78, 48, 70, 55, 82, 40, 65, 50, 74, 58, 45, 68, 80];

export function setLoading(el, isLoading) {
  if (!isLoading) {
    el.innerHTML = '';
    return;
  }
  el.innerHTML = '';
  const wrap = document.createElement('div');
  wrap.className = 'folder-skeleton';
  for (const width of SKELETON_WIDTHS) {
    const row = document.createElement('div');
    row.className = 'skeleton-row';
    const bar = document.createElement('div');
    bar.className = 'skeleton-bar';
    bar.style.width = width + '%';
    row.appendChild(bar);
    wrap.appendChild(row);
  }
  el.appendChild(wrap);
}

// Closing a bottom-sheet modal by tapping the dimmed backdrop, not just its X button —
// matches what every native sheet/picker does.
function closeOnBackdropTap(modal, close) {
  modal.addEventListener('click', (e) => {
    if (e.target === modal) close();
  });
}

// Renders a folder tree as nested <ul>s, in one of three modes:
// - onSelect: the whole row is a tap target (move-to-folder picker — any folder,
//   branch or leaf, is a valid destination).
// - onNavigate: the chevron toggles expand/collapse, the label opens that folder.
//   expandedSet/onToggle persist which folders are expanded (server-side, per
//   account — see handleSetExpandedFolders).
// - checkSet/onCheck: a checkbox per row, independent of expand/collapse — picking a
//   specific set of folders (e.g. the search filter's folder picker). checkSet holds
//   the currently-checked paths; checking a branch does not cascade to its children.
export function renderFolderTree(nodes, opts = {}) {
  const { onSelect, onNavigate, checkSet, onCheck, expandedSet, onToggle } = opts;
  const ul = document.createElement('ul');
  for (const node of nodes) {
    const hasChildren = node.children && node.children.length > 0;
    const li = document.createElement('li');
    const row = document.createElement('div');
    row.className = 'folder-row ' + (hasChildren ? 'has-children' : 'leaf');

    let childrenEl = null;
    if (hasChildren) {
      const toggle = document.createElement('button');
      toggle.className = 'folder-toggle';
      toggle.setAttribute('aria-label', 'Expand');
      row.appendChild(toggle);
      childrenEl = renderFolderTree(node.children, opts);
      const startExpanded = !!(expandedSet && expandedSet.has(node.path));
      if (!startExpanded) childrenEl.classList.add('hidden');
      toggle.classList.toggle('open', startExpanded);
      toggle.addEventListener('click', (e) => {
        e.stopPropagation();
        const collapsed = childrenEl.classList.toggle('hidden');
        toggle.classList.toggle('open', !collapsed);
        if (onToggle) onToggle(node.path, !collapsed);
      });
    } else {
      row.appendChild(document.createElement('span')).className = 'folder-toggle-spacer';
    }

    if (checkSet) {
      const checkbox = document.createElement('input');
      checkbox.type = 'checkbox';
      checkbox.className = 'folder-checkbox';
      checkbox.checked = checkSet.has(node.path);
      checkbox.addEventListener('click', (e) => e.stopPropagation());
      checkbox.addEventListener('change', () => {
        if (checkbox.checked) checkSet.add(node.path);
        else checkSet.delete(node.path);
        if (onCheck) onCheck(node.path, checkbox.checked);
      });
      row.appendChild(checkbox);
      row.classList.add('selectable');
      row.addEventListener('click', (e) => {
        e.stopPropagation();
        checkbox.checked = !checkbox.checked;
        checkbox.dispatchEvent(new Event('change'));
      });
    }

    const label = document.createElement('span');
    label.className = 'folder-label';
    label.textContent = node.name;
    row.appendChild(label);

    if (onSelect) {
      row.classList.add('selectable');
      row.addEventListener('click', (e) => {
        e.stopPropagation();
        onSelect(node.path);
      });
    } else if (onNavigate) {
      label.classList.add('navigable');
      label.addEventListener('click', (e) => {
        e.stopPropagation();
        onNavigate(node.path, node.name);
      });
    }

    li.appendChild(row);
    if (childrenEl) li.appendChild(childrenEl);
    ul.appendChild(li);
  }
  return ul;
}

let sheetModal, sheetBody, sheetError, sheetTitle, sheetSearch;

export function setupFolderSheet() {
  sheetModal = document.getElementById('folderSheetModal');
  sheetBody = document.getElementById('folderSheetBody');
  sheetError = document.getElementById('folderSheetError');
  sheetTitle = document.getElementById('folderSheetTitle');
  sheetSearch = document.getElementById('folderSheetSearch');
  document.getElementById('folderSheetClose').addEventListener('click', closeFolderSheet);
  closeOnBackdropTap(sheetModal, closeFolderSheet);
  sheetSearch.addEventListener('input', () => renderCurrentTree());
}

function closeFolderSheet() {
  sheetModal.classList.add('hidden');
  sheetBody.innerHTML = '';
  sheetError.textContent = '';
  sheetSearch.value = '';
}

// Keeps only nodes whose name matches (case-insensitive substring), plus any ancestor
// needed to reach a match — a folder with no matching name but a matching descendant
// still has to show up, just to contain it.
function filterTree(nodes, term) {
  const result = [];
  for (const node of nodes) {
    const selfMatch = node.name.toLowerCase().includes(term);
    const filteredChildren = node.children ? filterTree(node.children, term) : [];
    if (selfMatch || filteredChildren.length > 0) {
      result.push({ ...node, children: filteredChildren });
    }
  }
  return result;
}

// Every remaining branch in a filtered tree should render expanded — there's no point
// filtering down to a match and then still requiring a manual tap to reveal it.
function collectBranchPaths(nodes, set) {
  for (const node of nodes) {
    if (node.children && node.children.length > 0) {
      set.add(node.path);
      collectBranchPaths(node.children, set);
    }
  }
}

let lastFolders = null, lastTreeOpts = null;

function renderCurrentTree() {
  if (!lastFolders) return;
  const term = sheetSearch.value.trim().toLowerCase();
  sheetBody.innerHTML = '';
  if (!term) {
    sheetBody.appendChild(renderFolderTree(lastFolders, lastTreeOpts));
    return;
  }
  const filtered = filterTree(lastFolders, term);
  const expandedSet = new Set();
  collectBranchPaths(filtered, expandedSet);
  sheetBody.appendChild(renderFolderTree(filtered, { ...lastTreeOpts, expandedSet }));
}

// Shows one account's folder tree in the shared sheet, cache-then-network. treeOpts is
// passed straight to renderFolderTree (onSelect, or onNavigate+expandedSet+onToggle).
function showFolderSheet(accountId, title, treeOpts) {
  sheetModal.classList.remove('hidden');
  sheetTitle.textContent = title;
  sheetError.textContent = '';
  sheetSearch.value = '';

  const render = (folders) => {
    lastFolders = folders;
    lastTreeOpts = treeOpts;
    renderCurrentTree();
  };

  const cached = folderCache.get(accountId);
  if (cached) render(cached);
  else setLoading(sheetBody, true);

  fetchFolders(accountId)
    .then((folders) => {
      // still showing this same account's tree (not closed/switched while the fetch ran)
      if (!sheetModal.classList.contains('hidden') && sheetTitle.textContent === title) render(folders);
    })
    .catch((err) => {
      if (!cached) {
        sheetBody.textContent = '';
        sheetError.textContent = err.message;
      }
    });
}

// Match whatever's expanded in the Folder Browser for this account — without this,
// every picker opened fully collapsed regardless of where you'd actually navigated to
// elsewhere, which felt like it had no memory of its own.
async function loadExpandedFolders(accountId) {
  let expandedSet = new Set();
  try {
    const res = await fetch('/api/accounts');
    if (res.ok) {
      const accounts = await res.json();
      const account = accounts.find((a) => a.id === accountId);
      if (account) expandedSet = new Set(account.expandedFolders || []);
    }
  } catch {
    // best-effort — worst case the tree just opens collapsed
  }
  return expandedSet;
}

function persistExpandedFolders(accountId, expandedSet) {
  fetch(`/api/accounts/${accountId}/expanded-folders`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ paths: Array.from(expandedSet) }),
  });
}

export async function openMoveModal(row, direction = 'left') {
  row.dataset.moveDirection = direction;
  const accountId = row.dataset.accountId;
  const expandedSet = await loadExpandedFolders(accountId);

  showFolderSheet(accountId, 'Move to…', {
    onSelect: (path) => moveRowToFolder(row, path),
    expandedSet,
    onToggle: (path, isExpanded) => {
      if (isExpanded) expandedSet.add(path);
      else expandedSet.delete(path);
      persistExpandedFolders(accountId, expandedSet);
    },
  });
}

// Same picker, but for the mail reader's Move button — there's no swipeable row to
// animate (the mail might not even be in the currently-loaded list, e.g. opened via
// search or a deep link), so this just calls the move API directly and lets the
// caller decide what happens next.
export async function openMoveModalForMail(accountId, mailId, onMoved) {
  const expandedSet = await loadExpandedFolders(accountId);

  showFolderSheet(accountId, 'Move to…', {
    expandedSet,
    onSelect: async (path) => {
      closeFolderSheet();
      try {
        const res = await fetch(`/api/mails/${mailId}/move`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', ...dryRunHeaders() },
          body: JSON.stringify({ folder: path }),
        });
        if (!res.ok) throw new Error(await res.text());
        onMoved();
      } catch (err) {
        console.error(err);
      }
    },
    onToggle: (path, isExpanded) => {
      if (isExpanded) expandedSet.add(path);
      else expandedSet.delete(path);
      persistExpandedFolders(accountId, expandedSet);
    },
  });
}

function moveRowToFolder(row, path) {
  closeFolderSheet();
  row.classList.add('removing');
  const content = row.querySelector('.row-content');
  content.style.transition = 'transform 0.28s cubic-bezier(.34,1.56,.64,1)';
  content.style.transform = `translateX(${row.dataset.moveDirection === 'right' ? '100%' : '-100%'})`;
  fetch(`/api/mails/${row.dataset.id}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...dryRunHeaders() },
    body: JSON.stringify({ folder: path }),
  })
    .then((res) => {
      if (!res.ok) return res.text().then((msg) => Promise.reject(new Error(msg || 'move failed')));
      setTimeout(() => {
        removeMailById(row.dataset.id);
        row.remove();
      }, 280);
    })
    .catch((err) => {
      console.error(err);
      row.classList.remove('removing');
      content.style.transform = 'translateX(0)';
      setTimeout(() => { content.style.transition = ''; }, 280);
    });
}

// The topbar Folders shortcut: jumps straight to a folder tree (same sheet as
// move-to-folder) instead of going through the full Accounts panel. With one account
// it skips straight to that account's tree; with several, it asks which one first.
export async function openFolderBrowser() {
  sheetModal.classList.remove('hidden');
  sheetTitle.textContent = 'Folders';
  sheetError.textContent = '';
  setLoading(sheetBody, true);

  const res = await fetch('/api/accounts');
  if (!res.ok) {
    sheetBody.textContent = '';
    sheetError.textContent = await res.text();
    return;
  }
  const accounts = await res.json();
  if (accounts.length === 0) {
    sheetBody.textContent = '';
    sheetError.textContent = 'Add an account first (Settings > Accounts) to browse folders.';
    return;
  }
  if (accounts.length === 1) {
    showAccountFolders(accounts[0]);
    return;
  }

  sheetBody.classList.remove('dot-loader');
  sheetBody.innerHTML = '';
  for (const a of accounts) {
    const btn = document.createElement('button');
    btn.className = 'settings-row-btn';
    btn.textContent = a.email;
    btn.addEventListener('click', () => showAccountFolders(a));
    sheetBody.appendChild(btn);
  }
}

function showAccountFolders(account) {
  const expandedSet = new Set(account.expandedFolders || []);
  showFolderSheet(account.id, account.email, {
    expandedSet,
    onNavigate: (path, name) => {
      closeFolderSheet();
      openFolder(account.id, path, name);
    },
    onToggle: (path, isExpanded) => {
      if (isExpanded) expandedSet.add(path);
      else expandedSet.delete(path);
      fetch(`/api/accounts/${account.id}/expanded-folders`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ paths: Array.from(expandedSet) }),
      });
    },
  });
}
