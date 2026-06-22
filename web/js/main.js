import { fetchMails, render, renderLoadingSkeleton, setupPullToRefresh, setupInfiniteScroll, setupFolderBanner, setupTagGroupBanner, setHandlers, setupLiveUpdates, setupOverlayScrollLock, setupTapTopToScroll } from './inbox.js';
import { setupFolderSheet, openMoveModal, openFolderBrowser, setupFolderManager } from './folders.js';
import { setupConfirmModal } from './confirmModal.js';
import { setupAutoTagActivity } from './autoTagActivity.js';
import { setupMailReader, setupTagSheet, openMailReader, openMailReaderById, openTagSheetForRow } from './reader.js';
import { setupAccountsPanel } from './accounts.js';
import { setupThemeOptions, setupDryRunToggle, setupPaletteSwatches, setupSettingsPanel, setupSwipeOptions, setupTagManager } from './settings.js';
import { setupPushNotifications } from './push.js';
import { setupAccountFilter } from './accountFilter.js';
import { setupSearch } from './search.js';
import { setupSmartTaggingPanel } from './smarttags.js';

setHandlers({ onMove: openMoveModal, onTap: openMailReader, onTag: openTagSheetForRow });
setupConfirmModal();

renderLoadingSkeleton();
fetchMails().then(render);
setupPullToRefresh();
setupInfiniteScroll();
setupFolderSheet();
setupFolderBanner();
setupTagGroupBanner();
setupOverlayScrollLock();
setupTapTopToScroll();
setupMailReader();
setupTagSheet();
setupThemeOptions();
setupPaletteSwatches();
setupSettingsPanel();
setupSwipeOptions();
setupTagManager();
setupDryRunToggle();
setupAccountsPanel();
setupLiveUpdates();
setupPushNotifications();
setupAccountFilter();
setupSearch();
setupSmartTaggingPanel();
setupFolderManager();
setupAutoTagActivity();

document.getElementById('foldersBtn').addEventListener('click', openFolderBrowser);

document.getElementById('logoutBtn').addEventListener('click', () => {
  fetch('/auth/logout', { method: 'POST' }).then(() => location.reload());
});

// Deep-link from a push notification: the service worker opens/focuses the app at
// /?openMail=<id>, and the page loads straight into that specific email.
const openMailId = new URLSearchParams(location.search).get('openMail');
if (openMailId) {
  history.replaceState({}, '', location.pathname);
  openMailReaderById(openMailId);
}
