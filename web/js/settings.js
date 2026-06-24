import { fetchTags, createTag, updateTag, deleteTag, fetchTagDestinations, setTagDestination, deleteTagDestination } from './tags.js';
import { pickFolder, setLoading } from './folders.js';
import { withBusyButton } from './util.js';
import { confirmModal } from './confirmModal.js';
import { render as renderInbox } from './inbox.js';

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
  setupFunDeleteToggle();
  setupAutoLoadImagesToggle();
  setupImageCacheSettings();
}

function setupFunDeleteToggle() {
  const toggle = document.getElementById('funDeleteToggle');
  toggle.checked = localStorage.getItem('funDeleteAnimation') !== 'off'; // on by default
  toggle.addEventListener('change', () => {
    localStorage.setItem('funDeleteAnimation', toggle.checked ? 'on' : 'off');
  });
}

// On by default (like OWA/Gmail) — every image still goes through the server-side
// proxy (reader.js) and is sniffed/verified server-side (imageproxy.go rejects
// anything that doesn't genuinely sniff as a raster image, and SVG outright), so
// auto-showing doesn't mean trusting the sender's link directly, just that you don't
// need to tap "Show images" each time. Still off-by-request, not off-by-default,
// since the proxy already does the actual safety work.
function setupAutoLoadImagesToggle() {
  const toggle = document.getElementById('autoLoadImagesToggle');
  toggle.checked = localStorage.getItem('autoLoadImages') !== '0';
  toggle.addEventListener('change', () => {
    localStorage.setItem('autoLoadImages', toggle.checked ? '1' : '0');
  });
}

// Server-side (image_cache table is global, not localStorage) — shares /api/owner-settings
// with the Smart Tagging panel's autoTagMode/autoMoveDelayDays, so saving here has to
// send the whole object back, not just this one field, or it'd clobber the others.
async function setupImageCacheSettings() {
  const input = document.getElementById('imageCacheRetentionInput');
  const backfillBtn = document.getElementById('backfillImageCacheBtn');
  const statusEl = document.getElementById('imageBackfillStatus');
  const progressWrap = document.getElementById('imageCacheProgress');
  const progressFill = document.getElementById('imageCacheProgressFill');
  const progressText = document.getElementById('imageCacheProgressText');

  // A one-time bootstrap, not a routine action — ordinary new mail already gets its
  // images prefetched automatically going forward (syncAccount). Showing when it last
  // ran (instead of just always offering a bare "Download now" button) is the nudge
  // away from re-running it "just in case" every time this panel's opened.
  const renderStatus = () => {
    statusEl.textContent = settings.imageBackfillCompletedAt
      ? `Last ran ${new Date(settings.imageBackfillCompletedAt).toLocaleString()}`
      : "Hasn't been run yet";
  };

  let settings = { autoTagMode: 'review', autoMoveDelayDays: 3, imageCacheRetentionDays: 90, imageBackfillCompletedAt: null };
  try {
    const res = await fetch('/api/owner-settings');
    if (res.ok) settings = await res.json();
  } catch {
    // best-effort — input just shows the 90-day default until the next successful load
  }
  input.value = settings.imageCacheRetentionDays;
  renderStatus();

  input.addEventListener('change', async () => {
    const days = Math.min(90, Math.max(1, parseInt(input.value, 10) || 90)); // server clamps too — this just avoids the round trip showing you a value it's about to reject
    input.value = days;
    settings = { ...settings, imageCacheRetentionDays: days };
    try {
      await fetch('/api/owner-settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(settings),
      });
    } catch {
      // best-effort — worst case the next backfill/cleanup still uses the old value
    }
  });

  backfillBtn.addEventListener('click', () => {
    backfillBtn.disabled = true;
    progressWrap.classList.remove('hidden');
    progressFill.style.width = '0%';
    progressText.textContent = 'Starting…';
    const source = new EventSource('/api/image-cache/backfill');
    const finish = () => {
      source.close();
      backfillBtn.disabled = false;
    };
    source.addEventListener('progress', (e) => {
      const { done, total } = JSON.parse(e.data);
      progressFill.style.width = (total ? Math.round((done / total) * 100) : 100) + '%';
      progressText.textContent = `Scanned ${done} of ${total} folders…`;
    });
    source.addEventListener('complete', (e) => {
      const { imagesCached } = JSON.parse(e.data);
      progressFill.style.width = '100%';
      progressText.textContent = `Done — ${imagesCached} image${imagesCached === 1 ? '' : 's'} cached.`;
      settings = { ...settings, imageBackfillCompletedAt: new Date().toISOString() };
      renderStatus();
      finish();
    });
    // EventSource fires the generic 'error' event both for a real failure and for the
    // browser's own auto-retry attempts — by the time this runs the source is already
    // closed either way (we just closed it ourselves on 'complete', or it died on its
    // own), so there's nothing left worth distinguishing (same reasoning as the Smart
    // Tagging scan's own SSE error handling).
    source.addEventListener('error', () => {
      if (source.readyState === EventSource.CLOSED) {
        progressText.textContent = "Didn't finish — try again.";
      }
      finish();
    });
  });
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
      // Each row's swipe label/color was baked in at render time — stayed wrong until
      // the next unrelated re-render (or a full reload) happened to rebuild it, even
      // though the swipe gesture itself already used the new setting immediately.
      renderInbox();
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
    // Closing a settings sub-panel should land back on Settings, not skip all the way
    // out to the inbox underneath — every "open" handler hides Settings to show the
    // sub-panel, so "close" needs to be the exact reverse, not just hide itself.
    panel.classList.remove('hidden');
    document.getElementById('personalisationPanel').classList.add('hidden');
  });
}

export function setupTagManager() {
  const list = document.getElementById('tagManagerList');
  const form = document.getElementById('tagManagerForm');
  const input = document.getElementById('tagManagerInput');
  const errorEl = document.getElementById('tagManagerError');

  async function render() {
    // fetchTags() alone (not force) actually benefits from tags.js's own localStorage
    // cache — forcing it here defeated that for no reason, since create/update/delete
    // already invalidate it correctly on their own. destinations/accounts still aren't
    // cached, so this can still take a moment — a skeleton instead of a frozen list
    // is the fix for that part.
    setLoading(list, true);
    const [tags, destinations, accountsRes] = await Promise.all([
      fetchTags(),
      fetchTagDestinations().catch(() => []),
      fetch('/api/accounts').then((r) => (r.ok ? r.json() : [])).catch(() => []),
    ]);
    const accountCount = accountsRes.length;
    list.innerHTML = '';
    if (tags.length === 0) {
      list.innerHTML = '<li class="folder-empty-status">No tags yet — add one below.</li>';
      return;
    }
    for (const tag of tags) {
      const tagDestinations = destinations.filter((d) => d.tagId === tag.id);
      const li = document.createElement('li');
      li.className = 'tag-manager-card';
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
        nameInput.disabled = true;
        try {
          await updateTag(tag.id, { name });
          render();
        } catch (err) {
          errorEl.textContent = err.message;
          nameInput.value = tag.name;
          nameInput.disabled = false;
        }
      });

      const removeBtn = document.createElement('button');
      removeBtn.textContent = 'Delete';
      removeBtn.addEventListener('click', async () => {
        const ok = await confirmModal(
          `Delete the "${tag.name}" tag? This removes it from every email it's on — the emails themselves are untouched.`,
          { confirmLabel: 'Delete it', danger: true },
        );
        if (!ok) return;
        errorEl.textContent = '';
        try {
          await withBusyButton(removeBtn, 'Deleting…', () => deleteTag(tag.id));
        } catch (err) {
          // Surface the failure instead of silently leaving the (still-present) tag
          // looking deleted until the panel was closed and reopened — render() below
          // always re-syncs with the server's actual state either way.
          errorEl.textContent = err.message;
        }
        render();
      });
      row.append(dot, nameInput, removeBtn);
      li.appendChild(row);

      const swatches = document.createElement('div');
      swatches.className = 'tag-manager-swatches';
      // Sharing the swatches row instead of its own full-width row — a checkbox +
      // short label doesn't need a row to itself, and it cut one of several already
      // stacked rows per tag that were making this panel feel cluttered.
      const notifyWrap = document.createElement('label');
      notifyWrap.className = 'tag-manager-notify';
      const notifyToggle = document.createElement('input');
      notifyToggle.type = 'checkbox';
      notifyToggle.checked = tag.notify;
      notifyToggle.title = 'Notify on new mail with this tag';
      notifyToggle.addEventListener('change', async () => {
        const notify = notifyToggle.checked;
        notifyToggle.disabled = true;
        try {
          await updateTag(tag.id, { notify });
        } catch (err) {
          errorEl.textContent = err.message;
          notifyToggle.checked = !notify;
        }
        notifyToggle.disabled = false;
      });
      notifyWrap.append(notifyToggle, document.createTextNode(' Notify'));
      swatches.appendChild(notifyWrap);

      // Off by default — the global auto-move delay (Smart tagging settings) governs
      // every tag unless this is explicitly turned on, for the rare tag (receipts,
      // say) you always want filed away right now instead of waiting like everything else.
      const instantWrap = document.createElement('label');
      instantWrap.className = 'tag-manager-notify';
      const instantToggle = document.createElement('input');
      instantToggle.type = 'checkbox';
      instantToggle.checked = tag.instantMove;
      instantToggle.title = 'Skip the auto-move delay for this tag — move it the moment it\'s tagged';
      instantToggle.addEventListener('change', async () => {
        const instantMove = instantToggle.checked;
        instantToggle.disabled = true;
        try {
          await updateTag(tag.id, { instantMove });
        } catch (err) {
          errorEl.textContent = err.message;
          instantToggle.checked = !instantMove;
        }
        instantToggle.disabled = false;
      });
      instantWrap.append(instantToggle, document.createTextNode(' Move instantly'));
      swatches.appendChild(instantWrap);
      for (const color of TAG_COLORS) {
        const sw = document.createElement('button');
        sw.type = 'button';
        sw.className = 'tag-manager-swatch' + (color === tag.color ? ' selected' : '');
        sw.style.background = color;
        sw.addEventListener('click', async () => {
          // No text-swap here (a colored circle has no room for a label) — disabling
          // the whole row is enough; render() rebuilds everything fresh on success and
          // a failure needs every swatch re-enabled, not just the one tapped.
          for (const el of swatches.children) el.disabled = true;
          try {
            await updateTag(tag.id, { color });
            render();
          } catch (err) {
            errorEl.textContent = err.message;
            for (const el of swatches.children) el.disabled = false;
          }
        });
        swatches.appendChild(sw);
      }
      li.appendChild(swatches);

      // Tag -> folder: where mail carrying this tag should auto-move to once it's
      // old enough (Smart tagging settings controls the delay). Same folder_tag_rules
      // row the folder banner's "auto-tag" dropdown writes, just assigned from here
      // instead of needing to go browse to that folder first. A tag can have a
      // separate destination per account — this used to only ever show/edit
      // whichever destination happened to come back first, silently hiding (and
      // making unclearable from here) any other account's mapping for the same tag.
      // The account is always named, even with just one — "Move to: Folder" alone
      // read as if no account were involved at all.
      for (const destination of tagDestinations) {
        const destRow = document.createElement('div');
        destRow.className = 'account-row';
        const destLabel = document.createElement('span');
        destLabel.className = 'tag-manager-destination-label';
        destLabel.textContent = `Move to: ${destination.folder} (${destination.accountEmail})`;
        const destBtn = document.createElement('button');
        destBtn.className = 'tag-manager-dest-btn';
        destBtn.textContent = 'Change';
        destBtn.addEventListener('click', () => {
          pickFolder(`Move "${tag.name}" mail to… (${destination.accountEmail})`, async (accountId, path) => {
            errorEl.textContent = '';
            try {
              await setTagDestination(accountId, tag.id, path);
              render();
            } catch (err) {
              errorEl.textContent = err.message;
            }
          });
        });
        const clearBtn = document.createElement('button');
        clearBtn.className = 'tag-manager-clear-destination';
        clearBtn.textContent = 'Clear';
        clearBtn.addEventListener('click', async () => {
          const ok = await confirmModal(`Stop auto-moving "${tag.name}" mail to "${destination.folder}" (${destination.accountEmail})?`);
          if (!ok) return;
          errorEl.textContent = '';
          try {
            await withBusyButton(clearBtn, 'Clearing…', () => deleteTagDestination(destination.accountId, tag.id));
            render();
          } catch (err) {
            errorEl.textContent = err.message;
          }
        });
        destRow.append(destLabel, destBtn, clearBtn);
        li.appendChild(destRow);
      }

      // A second (or third...) destination only makes sense with a second account to
      // assign it to — with just one account, "Set destination" above already covers
      // the only mapping that could ever exist, so offering "add another" was a dead
      // end that just added clutter.
      if (accountCount < 2 && tagDestinations.length > 0) {
        list.appendChild(li);
        continue;
      }
      const addRow = document.createElement('div');
      addRow.className = 'account-row';
      const addBtn = document.createElement('button');
      addBtn.className = 'tag-manager-dest-btn';
      addBtn.textContent = tagDestinations.length === 0 ? 'Set destination' : 'Add destination for another account';
      addBtn.addEventListener('click', () => {
        pickFolder(`Move "${tag.name}" mail to…`, async (accountId, path) => {
          errorEl.textContent = '';
          try {
            await setTagDestination(accountId, tag.id, path);
            render();
          } catch (err) {
            errorEl.textContent = err.message;
          }
        });
      });
      addRow.appendChild(addBtn);
      li.appendChild(addRow);

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
  const closeTagsPanel = () => {
    document.getElementById('tagsPanel').classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeTagsBtn').addEventListener('click', closeTagsPanel);
  document.getElementById('closeTagsTopBtn').addEventListener('click', closeTagsPanel);
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

// localStorage-only personalisation — never reaches the server otherwise, so a
// backup has to gather and restore these itself alongside the server-side bundle.
const BACKUP_LOCAL_KEYS = ['theme', 'palette', 'swipeLeft', 'swipeRight', 'funDeleteAnimation', 'autoLoadImages'];

function gatherLocalPreferences() {
  const prefs = {};
  for (const key of BACKUP_LOCAL_KEYS) {
    const value = localStorage.getItem(key);
    if (value !== null) prefs[key] = value;
  }
  return prefs;
}

function applyLocalPreferences(prefs) {
  for (const [key, value] of Object.entries(prefs || {})) {
    if (BACKUP_LOCAL_KEYS.includes(key)) localStorage.setItem(key, value);
  }
}

export function setupDataTransfer() {
  document.getElementById('openBackupBtn').addEventListener('click', () => {
    document.getElementById('settingsPanel').classList.add('hidden');
    document.getElementById('backupPanel').classList.remove('hidden');
  });
  const closeBackupPanel = () => {
    document.getElementById('backupPanel').classList.add('hidden');
    document.getElementById('settingsPanel').classList.remove('hidden');
  };
  document.getElementById('closeBackupBtn').addEventListener('click', closeBackupPanel);
  document.getElementById('closeBackupTopBtn').addEventListener('click', closeBackupPanel);

  const exportBtn = document.getElementById('exportDataBtn');
  const importBtn = document.getElementById('importDataBtn');
  const fileInput = document.getElementById('importDataFile');
  const errorEl = document.getElementById('dataTransferError');
  const passwordInput = document.getElementById('backupPasswordInput');
  const passwordHeaders = () => {
    const password = passwordInput.value;
    return password ? { 'X-Backup-Password': password } : {};
  };

  // Same four checkboxes drive both directions — "what gets exported" and "what gets
  // overwritten on import" are the same question asked at the other end, so one
  // shared control surface instead of two separate sets the user has to keep in sync.
  const INCLUDE_CHECKBOXES = {
    accounts: 'backupIncludeAccounts',
    tags: 'backupIncludeTags',
    history: 'backupIncludeHistory',
    settings: 'backupIncludeSettings',
  };
  const gatherInclude = () => {
    const include = {};
    for (const [key, id] of Object.entries(INCLUDE_CHECKBOXES)) {
      include[key] = document.getElementById(id).checked;
    }
    return include;
  };

  exportBtn.addEventListener('click', () =>
    withBusyButton(exportBtn, 'Exporting…', async () => {
      errorEl.textContent = ''; // clear up front, not just on success — a retry that fails the same way otherwise never visibly resets
      const include = gatherInclude();
      const res = await fetch('/api/backup/export', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', ...passwordHeaders() },
        body: JSON.stringify({ include, localPreferences: include.settings ? gatherLocalPreferences() : {} }),
      });
      if (!res.ok) {
        errorEl.textContent = 'Export failed.';
        return;
      }
      const url = URL.createObjectURL(await res.blob());
      const a = document.createElement('a');
      a.href = url;
      a.download = 'sift-backup.json';
      a.click();
      URL.revokeObjectURL(url);
      errorEl.textContent = '';
    })
  );

  importBtn.addEventListener('click', () => fileInput.click());
  fileInput.addEventListener('change', async () => {
    const file = fileInput.files[0];
    fileInput.value = '';
    if (!file) return;
    // Import merges, it never deletes — existing tags/rules/history aren't removed
    // just because the backup doesn't mention them. But a tag/folder-rule/setting
    // that DOES exist in both gets overwritten with the backup's version, which is
    // surprising enough (and not undoable from here) to confirm before it happens.
    const ok = await confirmModal(
      "Import this backup? Existing tags, folder rules, and settings that also exist in the file will be overwritten with the file's version. Nothing already here gets deleted, and tagging/spam history only ever adds."
    );
    if (!ok) return;
    await withBusyButton(importBtn, 'Importing…', async () => {
      errorEl.textContent = ''; // clear up front, not just on success — a retry that fails the same way otherwise never visibly resets
      // "none" when every box is unchecked, not an empty string — an empty header
      // value is indistinguishable from the header being absent entirely, which the
      // server treats as "no filter, include everything" (so older/plain callers
      // still get full-bundle behavior). "none" matches no real category, so the
      // server's include-flags all end up false, same as actually unchecking them.
      const checked = Object.entries(gatherInclude()).filter(([, v]) => v).map(([k]) => k);
      const include = checked.length ? checked.join(',') : 'none';
      const res = await fetch('/api/backup/import', {
        method: 'POST',
        body: await file.text(),
        headers: { 'Content-Type': 'application/json', 'X-Backup-Include': include, ...passwordHeaders() },
      });
      if (!res.ok) {
        errorEl.textContent = await res.text() || 'Import failed — make sure this is a Sift backup file.';
        return;
      }
      const { localPreferences } = await res.json();
      if (localPreferences) applyLocalPreferences(localPreferences);
      location.reload(); // theme/palette/swipe settings only take effect on the next render pass
    });
  });
}
