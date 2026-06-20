// ponytail: native touch events handle swipe + rubber-band; no gesture library needed for this scope.
// touch events (not Pointer Events) chosen deliberately: they stay locked to their original
// target for the whole gesture in iOS standalone/PWA mode, where Pointer Events + setPointerCapture
// have known quirks (gesture randomly reverts to native scroll mid-drag).

const EDGE_GUARD = 20;      // px from screen edge where swipe is ignored, avoids iOS back/app-switcher gesture
const COMMIT_RATIO = 0.3;   // fraction of row width dragged at release-time to commit; below this, releasing cancels

const PAGE_SIZE = 30;
let mails = [];
let hasMore = true;
let loadingMore = false;

function dryRunHeaders() {
  return localStorage.getItem('dryRun') === '1' ? { 'X-Dry-Run': '1' } : {};
}

async function fetchMails() {
  const res = await fetch('/api/mails');
  mails = await res.json();
  hasMore = mails.length === PAGE_SIZE;
}

async function refreshMails() {
  const res = await fetch('/api/mails/refresh', { method: 'POST' });
  mails = await res.json();
  hasMore = mails.length === PAGE_SIZE;
}

async function loadMore() {
  if (loadingMore || !hasMore) return;
  loadingMore = true;
  const res = await fetch(`/api/mails?offset=${mails.length}`);
  const more = await res.json();
  hasMore = more.length === PAGE_SIZE;
  mails = mails.concat(more);
  const list = document.getElementById('inbox');
  for (const mail of more) list.appendChild(renderRow(mail));
  loadingMore = false;
}

function setupInfiniteScroll() {
  window.addEventListener('scroll', () => {
    if (window.innerHeight + window.scrollY >= document.body.scrollHeight - 300) {
      loadMore();
    }
  });
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
  li.innerHTML = `
    <div class="row-action archive">Archive</div>
    <div class="row-action delete">Delete</div>
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
  const archiveBox = row.querySelector('.row-action.archive');
  const deleteBox = row.querySelector('.row-action.delete');
  let startX = 0, startY = 0, dx = 0, dragging = false, horizontal = false, rafScheduled = false;

  function paint() {
    rafScheduled = false;
    content.style.transform = `translateX(${dx}px)`;
    archiveBox.style.width = `${Math.max(dx, 0)}px`;
    deleteBox.style.width = `${Math.max(-dx, 0)}px`;
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
    archiveBox.style.transition = deleteBox.style.transition = 'none';
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
    archiveBox.style.transition = deleteBox.style.transition = 'width 0.28s cubic-bezier(.2,.8,.2,1)';
    const threshold = row.clientWidth * COMMIT_RATIO;
    if (dx > threshold) return commit(row, 'archive');
    if (dx < -threshold) return commit(row, 'delete');
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

function commit(row, action) {
  row.classList.add('removing');
  const content = row.querySelector('.row-content');
  content.style.transform = `translateX(${action === 'archive' ? '100%' : '-100%'})`;
  fetch(`/api/mails/${row.dataset.id}/${action}`, { method: 'POST', headers: dryRunHeaders() });
  setTimeout(() => {
    mails = mails.filter((m) => m.id !== row.dataset.id);
    row.remove();
  }, 280);
}

function setupPullToRefresh() {
  const indicator = document.getElementById('pullIndicator');
  const list = document.getElementById('inbox');
  const THRESHOLD = 70;
  let startX = 0, startY = 0, pulling = false, decided = false, claimed = false, refreshing = false, pull = 0, rafScheduled = false;

  function paint() {
    rafScheduled = false;
    list.style.transform = `translateY(${Math.min(pull, 100)}px)`;
    indicator.style.height = `${Math.min(pull, 60)}px`;
    indicator.textContent = pull > THRESHOLD ? '↑ release to refresh' : '↓ pull to refresh';
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
      indicator.textContent = 'refreshing…';
      refreshMails().then(() => {
        render();
        indicator.classList.remove('refreshing');
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

fetchMails().then(render);
setupPullToRefresh();
setupInfiniteScroll();

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
        tree.appendChild(renderFolderTree(folders));
      });
      list.appendChild(li);
    }
  }

  function renderFolderTree(nodes) {
    const ul = document.createElement('ul');
    for (const node of nodes) {
      const li = document.createElement('li');
      li.textContent = node.name;
      if (node.children && node.children.length) {
        li.appendChild(renderFolderTree(node.children));
      }
      ul.appendChild(li);
    }
    return ul;
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
