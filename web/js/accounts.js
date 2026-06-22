import { dryRunHeaders, withBusyButton } from './util.js';
import { confirmModal } from './confirmModal.js';
import { renderFolderTree, fetchFolders, cachedFolders, setLoading } from './folders.js';
import { openFolder, fetchMails, render } from './inbox.js';
import { refreshAccountFilter } from './accountFilter.js';

export function setupAccountsPanel() {
  const panel = document.getElementById('accountsPanel');
  const list = document.getElementById('accountsList');
  const form = document.getElementById('addAccountForm');
  const errorEl = document.getElementById('addAccountError');

  async function loadAccounts() {
    // A failed fetch/parse here used to throw with nothing catching it — the list
    // just silently stayed however it was (often empty, on the very first open of a
    // session), which read as "my account isn't showing" with zero indication
    // anything had actually gone wrong.
    let accounts;
    try {
      const res = await fetch('/api/accounts');
      if (!res.ok) throw new Error(await res.text());
      accounts = await res.json();
    } catch (err) {
      list.innerHTML = `<li class="folder-empty-status">Couldn't load accounts: ${err.message}</li>`;
      return;
    }
    list.innerHTML = '';
    for (const a of accounts) {
      const li = document.createElement('li');
      const row = document.createElement('div');
      row.className = 'account-row';
      const label = document.createElement('span');
      label.textContent = `${a.email} (${a.host})`;
      const foldersBtn = document.createElement('button');
      foldersBtn.textContent = 'Folders';
      const removeBtn = document.createElement('button');
      removeBtn.textContent = 'Remove';
      removeBtn.addEventListener('click', async () => {
        const ok = await confirmModal(
          `Remove ${a.email}? This deletes its cached mail and settings from Sift — your real mailbox on the server is untouched, but you'll need to re-add it (and re-enter its password) to use it here again.`,
          { confirmLabel: 'Remove it', danger: true },
        );
        if (!ok) return;
        await withBusyButton(removeBtn, 'Removing…', () =>
          fetch(`/api/accounts/${a.id}`, { method: 'DELETE', headers: dryRunHeaders() })
        );
        loadAccounts();
        refreshAccountFilter();
      });
      const btnGroup = document.createElement('span');
      btnGroup.append(foldersBtn, removeBtn);
      row.append(label, btnGroup);
      li.appendChild(row);
      if (a.lastSyncError) {
        const err = document.createElement('p');
        err.className = 'account-sync-error';
        err.textContent = `Sync failed: ${a.lastSyncError}`;
        li.appendChild(err);
      }
      const tree = document.createElement('div');
      tree.className = 'folder-tree hidden';
      li.appendChild(tree);
      const expandedSet = new Set(a.expandedFolders || []);
      const renderTree = (folders) => {
        tree.innerHTML = '';
        tree.appendChild(renderFolderTree(folders, {
          onNavigate: (path, name) => {
            panel.classList.add('hidden');
            openFolder(a.id, path, name);
          },
          expandedSet,
          onToggle: (path, isExpanded) => {
            if (isExpanded) expandedSet.add(path);
            else expandedSet.delete(path);
            fetch(`/api/accounts/${a.id}/expanded-folders`, {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ paths: Array.from(expandedSet) }),
            });
          },
        }));
      };
      foldersBtn.addEventListener('click', async () => {
        const showing = !tree.classList.contains('hidden');
        if (showing) {
          tree.classList.add('hidden');
          return;
        }
        tree.classList.remove('hidden');

        const cached = cachedFolders(a.id);
        if (cached) renderTree(cached);
        else setLoading(tree, true);

        try {
          const folders = await fetchFolders(a.id);
          if (!tree.classList.contains('hidden')) renderTree(folders);
        } catch (err) {
          if (!cached) {
            tree.classList.remove('dot-loader');
            tree.textContent = err.message;
          }
        }
      });
      list.appendChild(li);
    }
  }

  document.getElementById('accountsBtn').addEventListener('click', () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    panel.classList.remove('hidden');
    loadAccounts();
  });
  document.getElementById('closeAccountsBtn').addEventListener('click', () => {
    panel.classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
    fetchMails().then(render); // pick up anything a sync brought in, even though the inbox isn't what's showing right now
  });

  document.getElementById('showAddAccountBtn').addEventListener('click', () => {
    form.classList.remove('hidden');
  });

  const submitBtn = form.querySelector('button[type="submit"]');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    errorEl.textContent = '';
    const body = Object.fromEntries(new FormData(form));
    body.port = Number(body.port);
    submitBtn.disabled = true;
    submitBtn.classList.add('dot-loader');
    submitBtn.textContent = 'Connecting';
    try {
      const res = await fetch('/api/accounts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        errorEl.textContent = await res.text();
        return;
      }
      form.reset();
      form.port.value = '993';
      form.classList.add('hidden');
      loadAccounts();
      refreshAccountFilter();
    } finally {
      submitBtn.disabled = false;
      submitBtn.classList.remove('dot-loader');
      submitBtn.textContent = 'Add account';
    }
  });
}
