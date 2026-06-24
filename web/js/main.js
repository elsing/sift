import { fetchMails, render, renderLoadingSkeleton, setupPullToRefresh, setupInfiniteScroll, setupFolderBanner, setupTagGroupBanner, setHandlers, setupLiveUpdates, setupOverlayScrollLock, setupTapTopToScroll } from './inbox.js';
import { setupFolderSheet, openMoveModal, openFolderBrowser, setupFolderManager } from './folders.js';
import { setupConfirmModal } from './confirmModal.js';
import { setupAutoTagActivity } from './autoTagActivity.js';
import { setupMailReader, setupTagSheet, openMailReader, openMailReaderById, openTagSheetForRow } from './reader.js';
import { setupAccountsPanel } from './accounts.js';
import { setupThemeOptions, setupDryRunToggle, setupDataTransfer, setupPaletteSwatches, setupSettingsPanel, setupSwipeOptions, setupTagManager } from './settings.js';
import { setupPushNotifications } from './push.js';
import { setupAccountFilter } from './accountFilter.js';
import { setupSearch } from './search.js';
import { setupSmartTaggingPanel } from './smarttags.js';
import { setupSmartSpamPanel } from './spam.js';

// One throwing block used to take down every single line after it in this file — it's
// all synchronous top-level code, so an exception anywhere stops the whole script
// right there (confirmed: the topbar/CSS still rendered since that's not JS-driven,
// but literally nothing else — buttons, the inbox list, every panel — ever got wired
// up). Wrapping only the named setup*() calls below wasn't enough; several other
// blocks in this file (the ResizeObserver setup, the visualViewport listener, the
// drag-to-dismiss IIFE) ran *unguarded*, any one of them throwing was just as fatal.
// Every top-level block in this file goes through this now, no exceptions.
function safe(name, fn) {
  try {
    fn();
  } catch (err) {
    console.error(`setup failed: ${name}`, err);
  }
}

safe('handlers', () => {
  setHandlers({ onMove: openMoveModal, onTap: openMailReader, onTag: openTagSheetForRow });
  setupConfirmModal();
});

safe('stickyHeaderResize', () => {
  // .sticky-header is position: fixed (style.css) so it never scrolls away — body
  // needs equivalent top padding so content doesn't start out hidden underneath it.
  // Its height varies (dry-run banner, folder banner, account filter bar each show/
  // hide on their own), so this stays in sync via ResizeObserver rather than a
  // one-off measurement.
  const stickyHeader = document.querySelector('.sticky-header');
  if (!stickyHeader) return;
  const syncHeaderHeight = () => { document.body.style.paddingTop = `${stickyHeader.offsetHeight}px`; };
  new ResizeObserver(syncHeaderHeight).observe(stickyHeader);
  syncHeaderHeight();
});

safe('modalKeyboardPadding', () => {
  // Every bottom-sheet modal (.move-modal-content — the folder picker, the tag-apply
  // sheet, both built on the same shared markup) has the same problem search ran
  // into: iOS Safari never shrinks the layout viewport for the on-screen keyboard, so
  // a text input near the top can still end up with no visible room below the
  // keyboard to type into. Fixed there with padding (not resizing the sheet itself —
  // resizing is what caused real regressions: a stuck-short panel, a literal gap
  // exposing content behind it). Generalized here via focus delegation instead of
  // wiring each sheet by hand, so it covers every current .move-modal and any future
  // one automatically.
  if (!window.visualViewport) return;
  const syncModalKeyboardPadding = () => {
    const active = document.activeElement;
    const content = active && active.closest && active.closest('.move-modal-content');
    document.querySelectorAll('.move-modal-content').forEach((el) => {
      if (el !== content) el.style.paddingBottom = '';
    });
    if (!content) return;
    const keyboardHeight = Math.max(0, window.innerHeight - window.visualViewport.height);
    content.style.paddingBottom = keyboardHeight > 0 ? `${keyboardHeight}px` : '';
  };
  window.visualViewport.addEventListener('resize', syncModalKeyboardPadding);
  document.addEventListener('focusin', (e) => {
    syncModalKeyboardPadding();
    // The input itself is usually near the top of the sheet already, but "usually
    // already visible" isn't "definitely" — same one-shot precise scroll search uses,
    // run the instant the input is focused (a real user gesture, so none of blur()'s
    // iOS gesture restrictions apply here) rather than waiting for content to change.
    const content = e.target.closest && e.target.closest('.move-modal-content');
    if (!content) return;
    requestAnimationFrame(() => {
      const overflow = e.target.getBoundingClientRect().bottom - window.visualViewport.height;
      if (overflow > 0) content.scrollBy({ top: overflow + 16, behavior: 'smooth' });
    });
  });
  document.addEventListener('focusout', syncModalKeyboardPadding);
});

renderLoadingSkeleton();
fetchMails().then(render).catch((err) => console.error('initial fetchMails failed', err));

safe('setupPullToRefresh', setupPullToRefresh);
safe('setupInfiniteScroll', setupInfiniteScroll);
safe('setupFolderSheet', setupFolderSheet);
safe('setupFolderBanner', setupFolderBanner);
safe('setupTagGroupBanner', setupTagGroupBanner);
safe('setupOverlayScrollLock', setupOverlayScrollLock);
safe('setupTapTopToScroll', setupTapTopToScroll);
safe('setupMailReader', setupMailReader);
safe('setupTagSheet', setupTagSheet);
safe('setupThemeOptions', setupThemeOptions);
safe('setupPaletteSwatches', setupPaletteSwatches);
safe('setupSettingsPanel', setupSettingsPanel);
safe('setupSwipeOptions', setupSwipeOptions);
safe('setupTagManager', setupTagManager);
safe('setupDryRunToggle', setupDryRunToggle);
safe('setupDataTransfer', setupDataTransfer);
safe('setupAccountsPanel', setupAccountsPanel);
safe('setupLiveUpdates', setupLiveUpdates);
safe('setupPushNotifications', setupPushNotifications);
safe('setupAccountFilter', setupAccountFilter);
safe('setupSearch', setupSearch);
safe('setupSmartTaggingPanel', setupSmartTaggingPanel);
safe('setupSmartSpamPanel', setupSmartSpamPanel);
safe('setupFolderManager', setupFolderManager);
safe('setupAutoTagActivity', setupAutoTagActivity);

safe('topbarButtons', () => {
  document.getElementById('foldersBtn').addEventListener('click', openFolderBrowser);
  document.getElementById('logoutBtn').addEventListener('click', () => {
    fetch('/auth/logout', { method: 'POST' }).then(() => location.reload());
  });
});

safe('pushDeepLink', () => {
  // Deep-link from a push notification: the service worker opens/focuses the app at
  // /?openMail=<id>, and the page loads straight into that specific email.
  const openMailId = new URLSearchParams(location.search).get('openMail');
  if (openMailId) {
    history.replaceState({}, '', location.pathname);
    openMailReaderById(openMailId);
  }
});

safe('dragToDismiss', () => {
  // Drag-to-dismiss for every bottom sheet — the same gesture every native iOS/Android
  // sheet already trains people to expect. Two previous attempts (a small grabber
  // handle, then widening detection to the header) both kept falling through to native
  // scroll — relying on "which specific sibling/child did the touch land on" is fragile.
  // .move-modal-drag-catcher (style.css) is the fix: one explicit, absolutely-positioned
  // layer with nothing else competing for that hit-test, sized to exclude the close
  // button's column so it stays normally tappable. Closes by clicking the sheet's own
  // existing close button rather than duplicating whatever cleanup its handler does.
  let drag = null;
  document.addEventListener('touchstart', (e) => {
    const catcher = e.target.closest('.move-modal-drag-catcher');
    if (!catcher) return;
    const content = catcher.closest('.move-modal-content');
    if (!content) return;
    drag = { content, startY: e.touches[0].clientY, startTime: Date.now() };
    content.style.transition = 'none';
  }, { passive: true });

  document.addEventListener('touchmove', (e) => {
    if (!drag) return;
    // Without this, .move-modal-content's own overflow-y:auto scroll (the grabber is
    // a child of it) fights the drag the whole time — the transform and the native
    // scroll both trying to respond to the same finger movement is exactly what read
    // as "thinks I'm scrolling" / getting stuck partway and snapping back. Needs
    // { passive: false } below for preventDefault to actually do anything here.
    e.preventDefault();
    const dy = Math.max(0, e.touches[0].clientY - drag.startY);
    drag.dy = dy;
    drag.content.style.transform = `translateY(${dy}px)`;
  }, { passive: false });

  const endDrag = () => {
    if (!drag) return;
    const { content, dy = 0, startTime } = drag;
    const velocity = dy / Math.max(1, Date.now() - startTime); // px/ms
    content.style.transition = '';
    content.style.transform = '';
    if (dy > content.offsetHeight * 0.3 || velocity > 0.8) {
      content.closest('.move-modal').querySelector('[id$="Close"]')?.click();
    }
    drag = null;
  };
  document.addEventListener('touchend', endDrag);
  // Without this, an interrupted touch (a system gesture taking over, multi-touch,
  // anything that fires touchcancel instead of touchend) left `drag` stuck non-null
  // forever — every touchmove anywhere on the page afterward, including the tiny
  // unavoidable movement inside an ordinary tap, kept calling preventDefault() above,
  // which is exactly the kind of thing that can silently swallow a tap's resulting
  // click entirely. This is almost certainly what broke "tapping a folder does
  // nothing" after the grabber was added — nothing folder-specific about it, any tap
  // anywhere following one interrupted drag would have been affected the same way.
  document.addEventListener('touchcancel', endDrag);
});
