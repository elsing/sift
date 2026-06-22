import { dryRunHeaders } from './util.js';
import { removeMailById, openFolder, beginRowAnimation, endRowAnimation, render } from './inbox.js';
import { confirmModal, promptModal } from './confirmModal.js';

// Folder lists rarely change, so every sheet open here uses cache-then-network: show
// the last-known tree instantly (no spinner, no flash) while quietly refetching in the
// background. Persisted to localStorage (survives reloads, like tags.js's tag cache) —
// and throttled (FOLDER_CACHE_TTL) so a fresh-enough cache skips the network call
// entirely rather than re-running an IMAP LIST against the remote server every single
// time a folder picker opens, which adds up for something that almost never changes.
const FOLDER_CACHE_TTL = 5 * 60 * 1000;
const FOLDERS_STORAGE_KEY = 'sift_folders_cache';
const folderCache = new Map(); // accountId -> { folders, fetchedAt }
try {
  const stored = JSON.parse(localStorage.getItem(FOLDERS_STORAGE_KEY) || '{}');
  for (const [id, entry] of Object.entries(stored)) folderCache.set(id, entry);
} catch {}

function persistFolderCache() {
  try {
    localStorage.setItem(FOLDERS_STORAGE_KEY, JSON.stringify(Object.fromEntries(folderCache)));
  } catch {}
}

export async function fetchFolders(accountId, force) {
  const entry = folderCache.get(accountId);
  if (!force && entry && Date.now() - entry.fetchedAt < FOLDER_CACHE_TTL) {
    return entry.folders;
  }
  const res = await fetch(`/api/accounts/${accountId}/folders`);
  if (!res.ok) throw new Error(await res.text());
  const folders = await res.json();
  folderCache.set(accountId, { folders, fetchedAt: Date.now() });
  persistFolderCache();
  return folders;
}

export function cachedFolders(accountId) {
  const entry = folderCache.get(accountId);
  return entry && entry.folders;
}

// parentPath is "" for a root-level folder. The server joins parentPath+name using
// the account's real hierarchy delimiter — the client never has a reliable way to
// know what that delimiter is itself.
export async function createFolder(accountId, parentPath, name) {
  const res = await fetch(`/api/accounts/${accountId}/folders`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ parentPath, name }),
  });
  if (!res.ok) throw new Error(await res.text());
  folderCache.delete(accountId);
  persistFolderCache();
}

// Renames in place (same parent, new leaf name only) — moving to a different parent
// isn't exposed here.
export async function renameFolder(accountId, path, newName) {
  const res = await fetch(`/api/accounts/${accountId}/folders`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path, newName }),
  });
  if (!res.ok) throw new Error(await res.text());
  folderCache.delete(accountId);
  persistFolderCache();
}

export async function deleteFolderOnServer(accountId, path) {
  const res = await fetch(`/api/accounts/${accountId}/folders`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path }),
  });
  if (!res.ok) throw new Error(await res.text());
  folderCache.delete(accountId);
  persistFolderCache();
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
  const { onSelect, onNavigate, checkSet, onCheck, expandedSet, onToggle, manageActions } = opts;
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

    // manageActions: { onCreateChild, onRename, onDelete } — the "Manage folders"
    // settings panel's mode, distinct from every other use of this tree (pick a
    // destination, browse, multi-select) which never need to mutate the folder
    // structure itself.
    if (manageActions) {
      const actions = document.createElement('span');
      actions.className = 'folder-manage-actions';
      const addBtn = document.createElement('button');
      addBtn.type = 'button';
      addBtn.className = 'folder-manage-btn';
      addBtn.textContent = '+';
      addBtn.title = 'New subfolder';
      addBtn.addEventListener('click', (e) => { e.stopPropagation(); manageActions.onCreateChild(node.path); });
      const renameBtn = document.createElement('button');
      renameBtn.type = 'button';
      renameBtn.className = 'folder-manage-btn';
      renameBtn.textContent = '✎';
      renameBtn.title = 'Rename';
      renameBtn.addEventListener('click', (e) => { e.stopPropagation(); manageActions.onRename(node.path, node.name); });
      const deleteBtn = document.createElement('button');
      deleteBtn.type = 'button';
      deleteBtn.className = 'folder-manage-btn folder-manage-delete';
      deleteBtn.textContent = '🗑';
      deleteBtn.title = 'Delete';
      deleteBtn.addEventListener('click', (e) => { e.stopPropagation(); manageActions.onDelete(node.path, node.name); });
      actions.append(addBtn, renameBtn, deleteBtn);
      row.appendChild(actions);
    }

    li.appendChild(row);
    if (childrenEl) li.appendChild(childrenEl);
    ul.appendChild(li);
  }
  return ul;
}

let sheetModal, sheetBody, sheetError, sheetTitle, sheetSearch, sheetConfirm;

export function setupFolderSheet() {
  sheetModal = document.getElementById('folderSheetModal');
  sheetBody = document.getElementById('folderSheetBody');
  sheetError = document.getElementById('folderSheetError');
  sheetTitle = document.getElementById('folderSheetTitle');
  sheetSearch = document.getElementById('folderSheetSearch');
  sheetConfirm = document.getElementById('folderSheetConfirm');
  document.getElementById('folderSheetClose').addEventListener('click', closeFolderSheet);
  closeOnBackdropTap(sheetModal, closeFolderSheet);
  sheetSearch.addEventListener('input', () => renderCurrentTree());
}

function closeFolderSheet() {
  sheetModal.classList.add('hidden');
  sheetBody.innerHTML = '';
  sheetError.textContent = '';
  sheetSearch.value = '';
  sheetConfirm.classList.add('hidden');
  sheetConfirm.onclick = null;
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
// passed straight to renderFolderTree (onSelect, or onNavigate+expandedSet+onToggle, or
// checkSet+onCheck for multi-select). confirm, if given, shows a footer button — used by
// checkSet mode, where there's no per-row action to close the sheet on.
function showFolderSheet(accountId, title, treeOpts, confirm) {
  sheetModal.classList.remove('hidden');
  sheetTitle.textContent = title;
  sheetError.textContent = '';
  sheetSearch.value = '';
  if (confirm) {
    sheetConfirm.textContent = confirm.label;
    sheetConfirm.classList.remove('hidden');
    sheetConfirm.onclick = () => {
      confirm.onConfirm();
      closeFolderSheet();
    };
  } else {
    sheetConfirm.classList.add('hidden');
    sheetConfirm.onclick = null;
  }

  const render = (folders) => {
    lastFolders = folders;
    lastTreeOpts = treeOpts;
    renderCurrentTree();
  };

  const cached = cachedFolders(accountId);
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
        // The sheet's already closed by this point and onMoved() (which would
        // otherwise close the reader as its own success signal) never runs — without
        // this, a failed move here looked exactly like a successful one, nothing on
        // screen ever said otherwise.
        console.error(err);
        alert(`Couldn't move that mail: ${err.message}`);
      }
    },
    onToggle: (path, isExpanded) => {
      if (isExpanded) expandedSet.add(path);
      else expandedSet.delete(path);
      persistExpandedFolders(accountId, expandedSet);
    },
  });
}

// Generic "pick a folder" — account selection first if there's more than one, then the
// usual folder tree, calling onSelect(accountId, path). Used by anything that needs a
// destination folder but isn't moving a specific mail (e.g. assigning a tag a folder
// to auto-move into, from the tag's own side rather than the folder banner).
export async function pickFolder(title, onSelect) {
  sheetModal.classList.remove('hidden');
  sheetTitle.textContent = title;
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
    sheetError.textContent = 'Add an account first (Settings > Accounts).';
    return;
  }

  const showPicker = async (accountId) => {
    const expandedSet = await loadExpandedFolders(accountId);
    showFolderSheet(accountId, title, {
      expandedSet,
      onSelect: (path) => {
        closeFolderSheet();
        onSelect(accountId, path);
      },
      onToggle: (path, isExpanded) => {
        if (isExpanded) expandedSet.add(path);
        else expandedSet.delete(path);
        persistExpandedFolders(accountId, expandedSet);
      },
    });
  };

  if (accounts.length === 1) {
    showPicker(accounts[0].id);
    return;
  }
  sheetBody.classList.remove('dot-loader');
  sheetBody.innerHTML = '';
  for (const a of accounts) {
    const btn = document.createElement('button');
    btn.className = 'settings-row-btn';
    btn.textContent = a.email;
    btn.addEventListener('click', () => showPicker(a.id));
    sheetBody.appendChild(btn);
  }
}

// Like pickFolder, but multi-select via checkboxes instead of "tap one row and close
// immediately" — account selection first if there's more than one, then a folder tree
// with checkboxes and a "Done" button. checkSet is mutated directly as boxes are
// (un)checked; onConfirm(accountId) fires when "Done" is pressed.
export async function pickFolders(title, checkSet, onConfirm) {
  sheetModal.classList.remove('hidden');
  sheetTitle.textContent = title;
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
    sheetError.textContent = 'Add an account first (Settings > Accounts).';
    return;
  }

  const showPicker = async (accountId) => {
    const expandedSet = await loadExpandedFolders(accountId);
    showFolderSheet(accountId, title, {
      checkSet,
      expandedSet,
      onToggle: (path, isExpanded) => {
        if (isExpanded) expandedSet.add(path);
        else expandedSet.delete(path);
        persistExpandedFolders(accountId, expandedSet);
      },
    }, { label: 'Done', onConfirm: () => onConfirm(accountId) });
  };

  if (accounts.length === 1) {
    showPicker(accounts[0].id);
    return;
  }
  sheetBody.classList.remove('dot-loader');
  sheetBody.innerHTML = '';
  for (const a of accounts) {
    const btn = document.createElement('button');
    btn.className = 'settings-row-btn';
    btn.textContent = a.email;
    btn.addEventListener('click', () => showPicker(a.id));
    sheetBody.appendChild(btn);
  }
}

// Exported for the Trash folder's "Restore" button (folders.js's own row decoration
// in inbox.js) — same animate-then-move-then-remove path the folder-picker uses,
// just invoked directly with a known destination instead of via the picker.
export function moveRowToFolder(row, path) {
  closeFolderSheet();
  beginRowAnimation(); // held until this finishes — see its own comment in inbox.js
  row.classList.add('removing');
  const content = row.querySelector('.row-content');
  // Both transform and opacity, not transform alone — overriding `transition` with
  // just one silently drops the other (inline style replaces the whole property), and
  // .removing's own opacity fade is what was missing, same issue commit() in inbox.js
  // had: content vanishing instantly then sliding away already invisible.
  content.style.transition = 'transform 0.28s cubic-bezier(.34,1.56,.64,1), opacity 0.28s ease';
  content.style.transform = `translateX(${row.dataset.moveDirection === 'right' ? '100%' : '-100%'})`;
  // Removal timer runs on its own fixed schedule, not nested inside the fetch's
  // .then() — otherwise the row's actual disappearance happens after (network
  // latency + 280ms), not just 280ms, leaving it visibly stuck mid-animation on a
  // slow connection. See the matching comment on commit() in inbox.js.
  let settled = false;
  setTimeout(() => {
    if (settled) return;
    settled = true;
    row.remove();
    endRowAnimation();
  }, 280);
  fetch(`/api/mails/${row.dataset.id}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...dryRunHeaders() },
    body: JSON.stringify({ folder: path }),
  })
    .then((res) => {
      if (!res.ok) return res.text().then((msg) => Promise.reject(new Error(msg || 'move failed')));
      removeMailById(row.dataset.id);
    })
    .catch((err) => {
      console.error(err);
      if (settled) {
        render(); // already removed from the DOM; data model still has it, so this restores it correctly
        return;
      }
      settled = true;
      row.classList.remove('removing');
      content.style.transform = 'translateX(0)';
      setTimeout(() => { content.style.transition = ''; endRowAnimation(); }, 280);
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

// Settings > Manage folders: real IMAP folder create/rename/delete, not just
// browsing — the Folder Browser and every picker elsewhere in the app only ever read
// the folder list, this is the one place that can actually change it.
let folderManagerAccountId = null;

export function setupFolderManager() {
  const panel = document.getElementById('folderManagerPanel');
  const accountPicker = document.getElementById('folderManagerAccountPicker');
  const newRootBtn = document.getElementById('folderManagerNewRootBtn');
  const tree = document.getElementById('folderManagerTree');
  const errorEl = document.getElementById('folderManagerError');

  const render = async () => {
    errorEl.textContent = '';
    if (!folderManagerAccountId) return;
    setLoading(tree, true);
    try {
      const folders = await fetchFolders(folderManagerAccountId);
      // INBOX always exists and can't be renamed/deleted on any IMAP server worth
      // talking to — offering rename/delete buttons for it here would just be a
      // guaranteed-to-fail action sitting in the list for no reason.
      const manageable = folders.filter((f) => f.path !== 'INBOX');
      tree.innerHTML = '';
      tree.appendChild(renderFolderTree(manageable, {
        manageActions: {
          onCreateChild: async (parentPath) => {
            const name = await promptModal('New subfolder name:');
            if (!name) return;
            errorEl.textContent = '';
            try {
              await createFolder(folderManagerAccountId, parentPath, name);
              render();
            } catch (err) {
              errorEl.textContent = err.message;
            }
          },
          onRename: async (path, currentName) => {
            const newName = await promptModal(`Rename "${currentName}" to:`, { defaultValue: currentName });
            if (!newName || newName === currentName) return;
            errorEl.textContent = '';
            try {
              await renameFolder(folderManagerAccountId, path, newName);
              render();
            } catch (err) {
              errorEl.textContent = err.message;
            }
          },
          onDelete: async (path, name) => {
            const ok = await confirmModal(
              `Delete "${name}"? This removes it (and everything in it) from the server.`,
              { confirmLabel: 'Delete it', danger: true },
            );
            if (!ok) return;
            errorEl.textContent = '';
            try {
              await deleteFolderOnServer(folderManagerAccountId, path);
              render();
            } catch (err) {
              errorEl.textContent = err.message;
            }
          },
        },
      }));
    } catch (err) {
      tree.innerHTML = '';
      errorEl.textContent = err.message;
    }
  };

  newRootBtn.addEventListener('click', async () => {
    const name = await promptModal('New folder name:');
    if (!name) return;
    errorEl.textContent = '';
    try {
      await createFolder(folderManagerAccountId, '', name);
      render();
    } catch (err) {
      errorEl.textContent = err.message;
    }
  });

  const closePanel = () => {
    panel.classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeFolderManagerBtn').addEventListener('click', closePanel);
  document.getElementById('closeFolderManagerTopBtn').addEventListener('click', closePanel);

  document.getElementById('openFolderManagerBtn').addEventListener('click', async () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    panel.classList.remove('hidden');
    accountPicker.innerHTML = '';
    newRootBtn.classList.add('hidden');
    errorEl.textContent = '';
    // Nothing shown at all between opening the panel and the accounts fetch
    // resolving (and then, for one account, the folders fetch after that) — both
    // network round trips, both with a real gap a skeleton should fill.
    setLoading(tree, true);

    const res = await fetch('/api/accounts');
    if (!res.ok) { errorEl.textContent = await res.text(); return; }
    const accounts = await res.json();
    if (accounts.length === 0) {
      tree.innerHTML = '';
      errorEl.textContent = 'Add an account first (Settings > Accounts).';
      return;
    }
    newRootBtn.classList.remove('hidden');
    if (accounts.length === 1) {
      folderManagerAccountId = accounts[0].id;
      render();
      return;
    }
    tree.innerHTML = ''; // waiting on an account pick now, not actively loading anything
    for (const a of accounts) {
      const btn = document.createElement('button');
      btn.className = 'settings-row-btn';
      btn.textContent = a.email;
      btn.addEventListener('click', () => {
        folderManagerAccountId = a.id;
        render();
      });
      accountPicker.appendChild(btn);
    }
  });
}
