// ponytail: native touch events handle swipe + rubber-band; no gesture library needed for this scope.
// touch events (not Pointer Events) chosen deliberately: they stay locked to their original
// target for the whole gesture in iOS standalone/PWA mode, where Pointer Events + setPointerCapture
// have known quirks (gesture randomly reverts to native scroll mid-drag).

const EDGE_GUARD = 20;      // px from screen edge where swipe is ignored, avoids iOS back/app-switcher gesture
const COMMIT_RATIO = 0.3;   // fraction of row width dragged at release-time to commit; below this, releasing cancels

// Swipe actions are config-driven so a future settings UI just needs to write new
// values to localStorage — no gesture/dispatch code to touch. 'move' is special: it
// doesn't commit immediately, it opens the folder picker (see performSwipeAction).
const SWIPE_ACTIONS = {
  archive: { label: 'Archive', colorVar: '--archive' },
  delete:  { label: 'Delete',  colorVar: '--delete' },
  move:    { label: 'Move',    colorVar: '--archive' },
  read:    { label: 'Mark read', colorVar: '--accent' },
};

function getSwipeConfig() {
  return {
    left: localStorage.getItem('swipeLeft') || 'delete',
    right: localStorage.getItem('swipeRight') || 'move',
  };
}

function performSwipeAction(row, action, direction) {
  if (action === 'move') {
    if (!row.dataset.accountId) return; // mock mail has no real folders to move into
    openMoveModal(row, direction);
    return;
  }
  if (action === 'read') {
    row.classList.toggle('unread');
    fetch(`/api/mails/${row.dataset.id}/read`, { method: 'POST', headers: dryRunHeaders() });
    return;
  }
  commit(row, action, direction);
}

const PAGE_SIZE = 30;
let mails = [];
let hasMore = true;
let loadingMore = false;
// nextOffset tracks how many rows we've fetched from the server, independent of mails.length:
// archiving/deleting (esp. under dry-run, which doesn't actually shrink the server-side list)
// shrinks the local array without moving the server cursor, so reusing mails.length as the
// offset re-fetches already-seen rows and shows duplicates.
let nextOffset = 0;

function dryRunHeaders() {
  return localStorage.getItem('dryRun') === '1' ? { 'X-Dry-Run': '1' } : {};
}

async function fetchMails() {
  const res = await fetch('/api/mails');
  mails = await res.json();
  nextOffset = mails.length;
  hasMore = mails.length === PAGE_SIZE;
}

async function refreshMails() {
  const res = await fetch('/api/mails/refresh', { method: 'POST' });
  mails = await res.json();
  nextOffset = mails.length;
  hasMore = mails.length === PAGE_SIZE;
}

async function loadMore() {
  if (loadingMore || !hasMore) return;
  loadingMore = true;
  const res = await fetch(`/api/mails?offset=${nextOffset}`);
  const more = await res.json();
  nextOffset += more.length;
  hasMore = more.length === PAGE_SIZE;
  mails = mails.concat(more);
  const list = document.getElementById('inbox');
  for (const mail of more) list.appendChild(renderRow(mail));
  loadingMore = false;
}

function setupInfiniteScroll() {
  let scheduled = false;
  window.addEventListener('scroll', () => {
    if (scheduled) return;
    scheduled = true;
    requestAnimationFrame(() => {
      scheduled = false;
      if (window.innerHeight + window.scrollY >= document.body.scrollHeight - 300) {
        loadMore();
      }
    });
  }, { passive: true });
}

function render() {
  const list = document.getElementById('inbox');
  list.innerHTML = '';
  for (const mail of mails) list.appendChild(renderRow(mail));
}

function renderRow(mail) {
  const li = document.createElement('li');
  li.className = 'row' + (mail.unread ? ' unread' : '');
  li.dataset.id = mail.id;
  li.dataset.accountId = mail.accountId || '';
  const { left, right } = getSwipeConfig();
  const leftMeta = SWIPE_ACTIONS[left];
  const rightMeta = SWIPE_ACTIONS[right];
  li.innerHTML = `
    <div class="row-action right-swipe" style="background: var(${rightMeta.colorVar})">${rightMeta.label}</div>
    <div class="row-action left-swipe" style="background: var(${leftMeta.colorVar})">${leftMeta.label}</div>
    <div class="row-content">
      <div class="unread-dot"></div>
      <div class="row-text">
        <div class="sender">${mail.sender}</div>
        <div class="subject">${mail.subject}</div>
        <div class="snippet">${mail.snippet}</div>
      </div>
      <div class="timestamp">${mail.time}</div>
    </div>
  `;
  attachSwipe(li);
  return li;
}

function rubberBand(dx, elasticPoint) {
  // full-speed up to elasticPoint, diminishing returns past it
  const abs = Math.abs(dx);
  if (abs <= elasticPoint) return dx;
  const over = abs - elasticPoint;
  const damped = elasticPoint + over / (1 + over / elasticPoint);
  return dx > 0 ? damped : -damped;
}

function attachSwipe(row) {
  const content = row.querySelector('.row-content');
  const rightSwipeBox = row.querySelector('.row-action.right-swipe'); // revealed dragging right (dx > 0)
  const leftSwipeBox = row.querySelector('.row-action.left-swipe');   // revealed dragging left (dx < 0)
  let startX = 0, startY = 0, dx = 0, dragging = false, horizontal = false, rafScheduled = false;

  function paint() {
    rafScheduled = false;
    content.style.transform = `translateX(${dx}px)`;
    rightSwipeBox.style.width = `${Math.max(dx, 0)}px`;
    leftSwipeBox.style.width = `${Math.max(-dx, 0)}px`;
  }

  function schedulePaint() {
    if (rafScheduled) return;
    rafScheduled = true;
    requestAnimationFrame(paint);
  }

  content.addEventListener('touchstart', (e) => {
    const t = e.touches[0];
    if (t.clientX < EDGE_GUARD || t.clientX > window.innerWidth - EDGE_GUARD) return;
    startX = t.clientX;
    startY = t.clientY;
    dragging = true;
    horizontal = false;
    content.style.transition = 'none';
    rightSwipeBox.style.transition = leftSwipeBox.style.transition = 'none';
  }, { passive: true });

  content.addEventListener('touchmove', (e) => {
    if (!dragging) return;
    const t = e.touches[0];
    const moveX = t.clientX - startX;
    const moveY = t.clientY - startY;
    if (!horizontal) {
      if (Math.abs(moveX) < 8 && Math.abs(moveY) < 8) {
        e.preventDefault(); // hold off native scroll until direction is decided
        return;
      }
      horizontal = Math.abs(moveX) > Math.abs(moveY);
      if (!horizontal) { dragging = false; return; }
    }
    e.preventDefault(); // claim the gesture so the page doesn't scroll mid-swipe
    const max = row.clientWidth;
    dx = Math.max(-max, Math.min(max, moveX)); // track the finger 1:1, clamped so it can't fly off past the row
    schedulePaint();
  }, { passive: false });

  function endDrag() {
    if (!dragging) return;
    dragging = false;
    content.style.transition = 'transform 0.28s cubic-bezier(.2,.8,.2,1)';
    rightSwipeBox.style.transition = leftSwipeBox.style.transition = 'width 0.28s cubic-bezier(.2,.8,.2,1)';
    const threshold = row.clientWidth * COMMIT_RATIO;
    const { left, right } = getSwipeConfig();
    if (dx > threshold) { dx = 0; paint(); performSwipeAction(row, right, 'right'); return; }
    if (dx < -threshold) { dx = 0; paint(); performSwipeAction(row, left, 'left'); return; }
    dx = 0;
    paint();
  }

  content.addEventListener('touchend', endDrag);
  content.addEventListener('touchcancel', endDrag);

  content.addEventListener('click', () => {
    if (Math.abs(dx) > 4) return; // suppress tap right after a drag
    row.classList.toggle('unread');
    fetch(`/api/mails/${row.dataset.id}/read`, { method: 'POST', headers: dryRunHeaders() });
  });
}

function commit(row, action, direction) {
  row.classList.add('removing');
  const content = row.querySelector('.row-content');
  content.style.transform = `translateX(${direction === 'right' ? '100%' : '-100%'})`;
  fetch(`/api/mails/${row.dataset.id}/${action}`, { method: 'POST', headers: dryRunHeaders() })
    .then((res) => {
      if (!res.ok) return res.text().then((msg) => Promise.reject(new Error(msg || `${action} failed`)));
      setTimeout(() => {
        mails = mails.filter((m) => m.id !== row.dataset.id);
        row.remove();
      }, 280);
    })
    .catch((err) => {
      // the server didn't actually do it (e.g. no Trash/Archive folder found) — snap back
      // instead of pretending it worked, so a failed action doesn't quietly vanish then
      // reappear confusingly on the next sync.
      console.error(err);
      row.classList.remove('removing');
      content.style.transition = 'transform 0.28s cubic-bezier(.2,.8,.2,1)';
      content.style.transform = 'translateX(0)';
      setTimeout(() => { content.style.transition = ''; }, 280);
    });
}

const REFRESH_QUIPS = [
  'Summoning your mail…', 'Asking the mail gods…', 'Herding electrons…',
  'Bribing the IMAP server…', 'Untangling the tubes…', 'Waking up the postman…',
];

function setupPullToRefresh() {
  const indicator = document.getElementById('pullIndicator');
  const icon = document.getElementById('pullIcon');
  const text = document.getElementById('pullText');
  const list = document.getElementById('inbox');
  const THRESHOLD = 70;
  let startX = 0, startY = 0, pulling = false, decided = false, claimed = false, refreshing = false, pull = 0, rafScheduled = false;

  function paint() {
    rafScheduled = false;
    list.style.transform = `translateY(${Math.min(pull, 100)}px)`;
    indicator.style.height = `${Math.min(pull, 60)}px`;
    const past = pull > THRESHOLD;
    icon.classList.toggle('tilt', past);
    icon.textContent = past ? '📬' : '📨';
    text.textContent = past ? 'Release to summon mail' : 'Pull to refresh';
  }

  function schedulePaint() {
    if (rafScheduled) return;
    rafScheduled = true;
    requestAnimationFrame(paint);
  }

  document.addEventListener('touchstart', (e) => {
    if (refreshing || document.scrollingElement.scrollTop > 0) { pulling = false; return; }
    startX = e.touches[0].clientX;
    startY = e.touches[0].clientY;
    pulling = true;
    decided = false;
    claimed = false;
  }, { passive: true });

  document.addEventListener('touchmove', (e) => {
    if (!pulling || refreshing) return;
    const dx = e.touches[0].clientX - startX;
    const dy = e.touches[0].clientY - startY;
    if (!decided) {
      if (Math.abs(dx) < 8 && Math.abs(dy) < 8) return;
      decided = true;
      // a row swipe in progress will have dx dominant — bail out and let it handle the gesture
      if (Math.abs(dx) >= Math.abs(dy) || dy <= 0) { pulling = false; return; }
    }
    if (dy <= 0) return;
    claimed = true;
    e.preventDefault(); // claim it before the browser starts its own scroll/bounce
    pull = rubberBand(dy, 70);
    schedulePaint();
  }, { passive: false });

  function endPull() {
    if (!pulling) return;
    pulling = false;
    if (!claimed || refreshing) return;
    list.style.transition = 'transform 0.28s cubic-bezier(.2,.8,.2,1)';
    list.style.transform = 'translateY(0)';
    if (pull > THRESHOLD) {
      refreshing = true;
      indicator.classList.add('refreshing');
      icon.classList.remove('tilt');
      icon.classList.add('spin');
      icon.textContent = '📨';
      text.textContent = REFRESH_QUIPS[Math.floor(Math.random() * REFRESH_QUIPS.length)];
      refreshMails().then(() => {
        render();
        indicator.classList.remove('refreshing');
        icon.classList.remove('spin');
        indicator.style.height = '0';
        refreshing = false;
      });
    } else {
      indicator.style.height = '0';
    }
    pull = 0;
    setTimeout(() => { list.style.transition = ''; }, 280);
  }

  document.addEventListener('touchend', endPull);
  document.addEventListener('touchcancel', endPull);
}

// Renders a folder tree as nested <ul>s, in one of two modes:
// - onSelect: the whole row is a tap target (move-to-folder picker — any folder,
//   branch or leaf, is a valid destination).
// - onNavigate: the chevron toggles expand/collapse, the label opens that folder
//   (Accounts panel's folder browser).
function renderFolderTree(nodes, { onSelect, onNavigate } = {}) {
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
      childrenEl = renderFolderTree(node.children, { onSelect, onNavigate });
      childrenEl.classList.add('hidden');
      toggle.addEventListener('click', (e) => {
        e.stopPropagation();
        const collapsed = childrenEl.classList.toggle('hidden');
        toggle.classList.toggle('open', !collapsed);
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

function setupMoveModal() {
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

async function openMoveModal(row, direction = 'left') {
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
  content.style.transition = 'transform 0.28s cubic-bezier(.2,.8,.2,1)';
  content.style.transform = `translateX(${row.dataset.moveDirection === 'right' ? '100%' : '-100%'})`;
  fetch(`/api/mails/${row.dataset.id}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...dryRunHeaders() },
    body: JSON.stringify({ folder: path }),
  })
    .then((res) => {
      if (!res.ok) return res.text().then((msg) => Promise.reject(new Error(msg || 'move failed')));
      setTimeout(() => {
        mails = mails.filter((m) => m.id !== row.dataset.id);
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

let folderViewPanel, folderViewList, folderViewTitle;

function setupFolderView() {
  folderViewPanel = document.getElementById('folderViewPanel');
  folderViewList = document.getElementById('folderViewList');
  folderViewTitle = document.getElementById('folderViewTitle');
  document.getElementById('folderViewBack').addEventListener('click', () => {
    folderViewPanel.classList.add('hidden');
  });
}

async function openFolderView(accountId, path, name) {
  folderViewPanel.classList.remove('hidden');
  folderViewTitle.textContent = name;
  folderViewList.innerHTML = '<li class="folder-view-status">Loading…</li>';
  const res = await fetch(`/api/accounts/${accountId}/folder-mails?path=${encodeURIComponent(path)}`);
  if (!res.ok) {
    folderViewList.innerHTML = '';
    const li = document.createElement('li');
    li.className = 'folder-view-status';
    li.textContent = await res.text();
    folderViewList.appendChild(li);
    return;
  }
  const folderMails = await res.json();
  folderViewList.innerHTML = '';
  if (folderMails.length === 0) {
    const li = document.createElement('li');
    li.className = 'folder-view-status';
    li.textContent = 'No mail in this folder.';
    folderViewList.appendChild(li);
    return;
  }
  for (const m of folderMails) {
    const li = document.createElement('li');
    li.className = 'row folder-view-row' + (m.unread ? ' unread' : '');
    li.innerHTML = `
      <div class="row-content">
        <div class="unread-dot"></div>
        <div class="row-text">
          <div class="sender">${m.sender}</div>
          <div class="subject">${m.subject}</div>
        </div>
        <div class="timestamp">${m.time}</div>
      </div>
    `;
    folderViewList.appendChild(li);
  }
}

fetchMails().then(render);
setupPullToRefresh();
setupInfiniteScroll();
setupMoveModal();
setupFolderView();

document.getElementById('logoutBtn').addEventListener('click', () => {
  fetch('/auth/logout', { method: 'POST' }).then(() => location.reload());
});

setupThemeToggle();
setupDryRunToggle();
setupAccountsPanel();

function setupThemeToggle() {
  const order = ['auto', 'light', 'dark'];
  const icons = { auto: 'A', light: '☀', dark: '☾' };
  const btn = document.getElementById('themeBtn');
  function apply(theme) {
    if (theme === 'auto') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
    btn.textContent = icons[theme];
    btn.title = 'Theme: ' + theme;
  }
  apply(localStorage.getItem('theme') || 'auto');
  btn.addEventListener('click', () => {
    const current = localStorage.getItem('theme') || 'auto';
    const next = order[(order.indexOf(current) + 1) % order.length];
    localStorage.setItem('theme', next);
    apply(next);
  });
}

function setupDryRunToggle() {
  const btn = document.getElementById('dryRunBtn');
  const banner = document.getElementById('dryRunBanner');
  function apply(on) {
    btn.classList.toggle('active', on);
    btn.title = on ? 'Dry run: on (no changes are saved)' : 'Dry run: off';
    banner.classList.toggle('visible', on);
  }
  apply(localStorage.getItem('dryRun') === '1');
  btn.addEventListener('click', () => {
    const on = localStorage.getItem('dryRun') !== '1';
    localStorage.setItem('dryRun', on ? '1' : '0');
    apply(on);
  });
}

function setupAccountsPanel() {
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
        tree.innerHTML = '';
        tree.appendChild(renderFolderTree(folders, {
          onNavigate: (path, name) => openFolderView(a.id, path, name),
        }));
      });
      list.appendChild(li);
    }
  }

  document.getElementById('accountsBtn').addEventListener('click', () => {
    panel.classList.remove('hidden');
    loadAccounts();
  });
  document.getElementById('closeAccountsBtn').addEventListener('click', () => {
    panel.classList.add('hidden');
    fetchMails().then(render); // pick up anything a sync brought in
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
      loadAccounts();
    } finally {
      submitBtn.disabled = false;
      submitBtn.classList.remove('dot-loader');
      submitBtn.textContent = 'Add account';
    }
  });
}
