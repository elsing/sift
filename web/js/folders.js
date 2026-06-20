import { dryRunHeaders } from './util.js';
import { removeMailById } from './inbox.js';

// Renders a folder tree as nested <ul>s, in one of two modes:
// - onSelect: the whole row is a tap target (move-to-folder picker — any folder,
//   branch or leaf, is a valid destination).
// - onNavigate: the chevron toggles expand/collapse, the label opens that folder
//   (Accounts panel's folder browser). expandedSet/onToggle persist which folders
//   are expanded (server-side, per account — see handleSetExpandedFolders).
export function renderFolderTree(nodes, opts = {}) {
  const { onSelect, onNavigate, expandedSet, onToggle } = opts;
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

let moveModal, moveModalTree, moveModalError;

export function setupMoveModal() {
  moveModal = document.getElementById('moveModal');
  moveModalTree = document.getElementById('moveModalTree');
  moveModalError = document.getElementById('moveModalError');
  document.getElementById('moveModalClose').addEventListener('click', closeMoveModal);
}

function closeMoveModal() {
  moveModal.classList.add('hidden');
  moveModalTree.innerHTML = '';
  moveModalError.textContent = '';
}

export async function openMoveModal(row, direction = 'left') {
  row.dataset.moveDirection = direction;
  moveModal.classList.remove('hidden');
  moveModalError.textContent = '';
  moveModalTree.textContent = 'Loading…';
  const res = await fetch(`/api/accounts/${row.dataset.accountId}/folders`);
  if (!res.ok) {
    moveModalTree.textContent = '';
    moveModalError.textContent = await res.text();
    return;
  }
  const folders = await res.json();
  moveModalTree.innerHTML = '';
  moveModalTree.appendChild(renderFolderTree(folders, { onSelect: (path) => moveRowToFolder(row, path) }));
}

function moveRowToFolder(row, path) {
  closeMoveModal();
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
