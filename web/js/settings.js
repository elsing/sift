import { fetchTags, createTag, updateTag, deleteTag } from './tags.js';

// Same palette the server cycles through for newly-created tags (tagPalette in
// internal/api/tags.go) — kept in sync manually since there's no endpoint for "list
// of colors", just for "list of tags".
const TAG_COLORS = ['#ff3d81', '#00d9a3', '#9b5de5', '#ff9f1c', '#3a86ff', '#f72585', '#06d6a0'];

const THEMES = [
  { id: 'auto', name: 'Auto', icon: 'A' },
  { id: 'light', name: 'Light', icon: '☀' },
  { id: 'dark', name: 'Dark', icon: '☾' },
];

export function setupThemeOptions() {
  const container = document.getElementById('themeOptions');

  function apply(theme) {
    if (theme === 'auto') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
    for (const btn of container.children) {
      btn.classList.toggle('selected', btn.dataset.theme === theme);
    }
  }

  for (const t of THEMES) {
    const btn = document.createElement('button');
    btn.className = 'theme-option';
    btn.dataset.theme = t.id;
    btn.innerHTML = `<span class="theme-option-icon">${t.icon}</span><span>${t.name}</span>`;
    btn.addEventListener('click', () => {
      localStorage.setItem('theme', t.id);
      apply(t.id);
    });
    container.appendChild(btn);
  }

  apply(localStorage.getItem('theme') || 'auto');
}

// "funky" (the default) has no data-palette attribute — it's what :root/[data-theme]
// already define in style.css. The hex values here are just for painting the swatch
// previews; they must match style.css's [data-palette="..."] blocks.
const PALETTES = [
  { id: 'funky', name: 'Funky', colors: ['#ff3d81', '#00d9a3', '#ff4d4d', '#9b5de5'] },
  { id: 'warm', name: 'Warm', colors: ['#c9763e', '#7a8f4e', '#b8593f', '#a87b4f'] },
  { id: 'ocean', name: 'Ocean', colors: ['#00b4d8', '#2dd4bf', '#fb7185', '#6366f1'] },
  { id: 'sunset', name: 'Sunset', colors: ['#ff6b35', '#f7b801', '#d72638', '#6a0572'] },
  { id: 'candy', name: 'Candy', colors: ['#ff6fae', '#4ecdc4', '#ff6b6b', '#c77dff'] },
];

export function setupPaletteSwatches() {
  const container = document.getElementById('paletteSwatches');

  function apply(palette) {
    if (palette === 'funky') delete document.documentElement.dataset.palette;
    else document.documentElement.dataset.palette = palette;
    for (const btn of container.children) {
      btn.classList.toggle('selected', btn.dataset.palette === palette);
    }
  }

  for (const p of PALETTES) {
    const btn = document.createElement('button');
    btn.className = 'palette-swatch';
    btn.dataset.palette = p.id;
    btn.innerHTML = `
      <span class="palette-swatch-dots">${p.colors.map((c) => `<span style="background:${c}"></span>`).join('')}</span>
      <span class="palette-swatch-name">${p.name}</span>
    `;
    btn.addEventListener('click', () => {
      localStorage.setItem('palette', p.id);
      apply(p.id);
    });
    container.appendChild(btn);
  }

  apply(localStorage.getItem('palette') || 'funky');
}

// Swipe actions were config-driven (localStorage) from the start, but there was never
// actually a UI to change them — this is that missing piece.
const SWIPE_ACTIONS = {
  archive: { label: 'Archive', icon: '📥' },
  delete: { label: 'Delete', icon: '🗑' },
  move: { label: 'Move', icon: '➡️' },
  read: { label: 'Mark read', icon: '✓' },
  tag: { label: 'Tag', icon: '🏷' },
};

export function setupSwipeOptions() {
  setupSwipeSide('swipeLeftOptions', 'swipeLeft', 'delete');
  setupSwipeSide('swipeRightOptions', 'swipeRight', 'move');
}

function setupSwipeSide(containerId, storageKey, fallback) {
  const container = document.getElementById(containerId);

  function apply(action) {
    for (const btn of container.children) {
      btn.classList.toggle('selected', btn.dataset.action === action);
    }
  }

  for (const [id, { label, icon }] of Object.entries(SWIPE_ACTIONS)) {
    const btn = document.createElement('button');
    btn.className = 'theme-option';
    btn.dataset.action = id;
    btn.innerHTML = `<span class="theme-option-icon">${icon}</span><span>${label}</span>`;
    btn.addEventListener('click', () => {
      localStorage.setItem(storageKey, id);
      apply(id);
    });
    container.appendChild(btn);
  }

  apply(localStorage.getItem(storageKey) || fallback);
}

export function setupSettingsPanel() {
  const panel = document.getElementById('settingsPanel');
  document.getElementById('settingsBtn').addEventListener('click', () => {
    panel.classList.remove('hidden');
  });
  document.getElementById('closeSettingsBtn').addEventListener('click', () => {
    panel.classList.add('hidden');
  });

  document.getElementById('openPersonalisationBtn').addEventListener('click', () => {
    panel.classList.add('hidden');
    document.getElementById('personalisationPanel').classList.remove('hidden');
  });
  document.getElementById('closePersonalisationBtn').addEventListener('click', () => {
    document.getElementById('personalisationPanel').classList.add('hidden');
  });
}

export function setupTagManager() {
  const list = document.getElementById('tagManagerList');
  const form = document.getElementById('tagManagerForm');
  const input = document.getElementById('tagManagerInput');
  const errorEl = document.getElementById('tagManagerError');

  async function render() {
    const tags = await fetchTags(true);
    list.innerHTML = '';
    if (tags.length === 0) {
      list.innerHTML = '<li class="folder-empty-status">No tags yet — add one below.</li>';
      return;
    }
    for (const tag of tags) {
      const li = document.createElement('li');
      const row = document.createElement('div');
      row.className = 'account-row';

      const dot = document.createElement('span');
      dot.className = 'tag-sheet-dot';
      dot.style.display = 'inline-block';
      dot.style.marginRight = '6px';
      dot.style.background = tag.color;

      const nameInput = document.createElement('input');
      nameInput.type = 'text';
      nameInput.className = 'tag-manager-name-input';
      nameInput.value = tag.name;
      nameInput.addEventListener('change', async () => {
        const name = nameInput.value.trim();
        if (!name || name === tag.name) { nameInput.value = tag.name; return; }
        errorEl.textContent = '';
        try {
          await updateTag(tag.id, { name });
          render();
        } catch (err) {
          errorEl.textContent = err.message;
          nameInput.value = tag.name;
        }
      });

      const removeBtn = document.createElement('button');
      removeBtn.textContent = 'Delete';
      removeBtn.addEventListener('click', async () => {
        await deleteTag(tag.id);
        render();
      });
      row.append(dot, nameInput, removeBtn);
      li.appendChild(row);

      const swatches = document.createElement('div');
      swatches.className = 'tag-manager-swatches';
      for (const color of TAG_COLORS) {
        const sw = document.createElement('button');
        sw.type = 'button';
        sw.className = 'tag-manager-swatch' + (color === tag.color ? ' selected' : '');
        sw.style.background = color;
        sw.addEventListener('click', async () => {
          await updateTag(tag.id, { color });
          render();
        });
        swatches.appendChild(sw);
      }
      li.appendChild(swatches);

      list.appendChild(li);
    }
  }

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const name = input.value.trim();
    if (!name) return;
    errorEl.textContent = '';
    try {
      await createTag(name);
      input.value = '';
      render();
    } catch (err) {
      errorEl.textContent = err.message;
    }
  });

  document.getElementById('openTagsBtn').addEventListener('click', () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    document.getElementById('tagsPanel').classList.remove('hidden');
    render();
  });
  document.getElementById('closeTagsBtn').addEventListener('click', () => {
    document.getElementById('tagsPanel').classList.add('hidden');
  });
}

export function setupDryRunToggle() {
  const btn = document.getElementById('dryRunBtn');
  const banner = document.getElementById('dryRunBanner');
  function apply(on) {
    btn.classList.toggle('active', on);
    btn.textContent = 'Dry run mode: ' + (on ? 'On' : 'Off');
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
