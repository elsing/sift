// Browser push notifications (banner/lock-screen, even with the app closed). Separate
// from the SSE live-refresh in inbox.js, which only updates the list while the app is
// open in the foreground. iOS requires the subscribe step to run from a real user
// gesture (a button click), so this can't just auto-run on page load.
function urlBase64ToUint8Array(base64) {
  const padded = base64.padEnd(base64.length + (4 - (base64.length % 4)) % 4, '=');
  const raw = atob(padded.replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

export function setupPushNotifications() {
  const btn = document.getElementById('enablePushBtn');
  const errorEl = document.getElementById('pushError');
  if (!btn) return;

  if (!('serviceWorker' in navigator) || !('PushManager' in window) || !('Notification' in window)) {
    btn.disabled = true;
    btn.textContent = 'Notifications not supported';
    errorEl.textContent = 'This browser/install doesn\'t support push. On iPhone, open this app in Safari, ' +
      'add it to your Home Screen, then launch it from that icon (not from a regular browser tab or another browser).';
    return;
  }

  navigator.serviceWorker.register('/sw.js').catch((err) => {
    errorEl.textContent = 'Couldn\'t register the service worker: ' + err.message;
  });

  async function refreshLabel() {
    const reg = await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.getSubscription();
    btn.textContent = sub ? 'Notifications: On (tap to turn off)' : 'Enable notifications';
  }
  refreshLabel();

  let busy = false;
  btn.addEventListener('click', async () => {
    // iOS Safari in standalone PWA mode can dispatch a click twice for one tap (a known
    // "ghost click" quirk) — without this guard that fired two subscribe/unsubscribe
    // requests per tap.
    if (busy) return;
    busy = true;
    errorEl.textContent = '';
    try {
      const reg = await navigator.serviceWorker.ready;
      const existing = await reg.pushManager.getSubscription();

      if (existing) {
        await fetch('/api/push/unsubscribe', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ endpoint: existing.endpoint }),
        });
        await existing.unsubscribe();
        errorEl.textContent = 'Notifications turned off.';
        refreshLabel();
        return;
      }

      const permission = await Notification.requestPermission();
      if (permission !== 'granted') {
        errorEl.textContent = permission === 'denied'
          ? 'Notifications are blocked for this app. Re-enable them in your device/browser settings.'
          : 'Permission was dismissed without choosing — try again.';
        return;
      }

      const { publicKey } = await fetch('/api/push/public-key').then((r) => r.json());
      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(publicKey),
      });
      const res = await fetch('/api/push/subscribe', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(sub.toJSON()),
      });
      if (!res.ok) throw new Error(await res.text());
      errorEl.textContent = 'Notifications on — you should see a test notification any second.';
      refreshLabel();
    } catch (err) {
      console.error(err);
      errorEl.textContent = 'Couldn\'t change notification settings: ' + err.message;
    } finally {
      busy = false;
    }
  });
}
