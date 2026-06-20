import { fetchMails, render, setupPullToRefresh, setupInfiniteScroll, setupFolderBanner, setHandlers } from './inbox.js';
import { setupMoveModal, openMoveModal } from './folders.js';
import { setupMailReader, openMailReader } from './reader.js';
import { setupAccountsPanel, openAccountsPanel } from './accounts.js';
import { setupThemeOptions, setupDryRunToggle, setupPaletteSwatches, setupSettingsPanel } from './settings.js';

setHandlers({ onMove: openMoveModal, onTap: openMailReader });

fetchMails().then(render);
setupPullToRefresh();
setupInfiniteScroll();
setupMoveModal();
setupFolderBanner();
setupMailReader();
setupThemeOptions();
setupPaletteSwatches();
setupSettingsPanel();
setupDryRunToggle();
setupAccountsPanel();

document.getElementById('foldersBtn').addEventListener('click', openAccountsPanel);

document.getElementById('logoutBtn').addEventListener('click', () => {
  fetch('/auth/logout', { method: 'POST' }).then(() => location.reload());
});
