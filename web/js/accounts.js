import { dryRunHeaders } from './util.js';
import { renderFolderTree } from './folders.js';
import { openFolder, fetchMails, render } from './inbox.js';

export function setupAccountsPanel() {
  const panel = document.getElementById('accountsPanel');
  const list = document.getElementById('accountsList');
  const form = document.getElementById('addAccountForm');
  const errorEl = document.getElementById('addAccountError');

  async function loadAccounts() {
    const res = await fetch('/api/accounts');
    const accounts = await res.json();
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
        await fetch(`/api/accounts/${a.id}`, { method: 'DELETE', headers: dryRunHeaders() });
        loadAccounts();
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
      foldersBtn.addEventListener('click', async () => {
        const showing = !tree.classList.contains('hidden');
        if (showing) {
          tree.classList.add('hidden');
          return;
        }
        tree.classList.remove('hidden');
        tree.textContent = 'Loading…';
        const res = await fetch(`/api/accounts/${a.id}/folders`);
        if (!res.ok) {
          tree.textContent = await res.text();
          return;
        }
        const folders = await res.json();
        const expandedSet = new Set(a.expandedFolders || []);
        tree.innerHTML = '';
        tree.appendChild(renderFolderTree(folders, {
          onNavigate: (path, name) => openFolder(a.id, path, name),
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
    fetchMails().then(render); // pick up anything a sync brought in
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
    } finally {
      submitBtn.disabled = false;
      submitBtn.classList.remove('dot-loader');
      submitBtn.textContent = 'Add account';
    }
  });
}
