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

export function setupSettingsPanel() {
  const panel = document.getElementById('settingsPanel');
  document.getElementById('settingsBtn').addEventListener('click', () => {
    panel.classList.remove('hidden');
  });
  document.getElementById('closeSettingsBtn').addEventListener('click', () => {
    panel.classList.add('hidden');
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
