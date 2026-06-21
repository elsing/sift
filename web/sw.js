// Minimal service worker: exists only to receive push events while the app isn't
// open, and show a notification. No caching/offline support — that's a separate
// concern this app doesn't need yet.
self.addEventListener('push', (event) => {
  const data = event.data ? event.data.json() : { title: 'New mail', body: '' };
  event.waitUntil(self.registration.showNotification(data.title, {
    body: data.body,
    icon: '/icon-180.png',
    data: { mailId: data.mailId || null },
  }));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const mailId = event.notification.data && event.notification.data.mailId;
  const url = mailId ? `/?openMail=${encodeURIComponent(mailId)}` : '/';
  event.waitUntil((async () => {
    // If the app is already running (even backgrounded), openWindow() alone can just
    // refocus it without navigating — explicitly navigate the existing window first,
    // and only fall back to opening a new one if there isn't one.
    const existing = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const client of existing) {
      if ('navigate' in client) {
        await client.navigate(url);
        return client.focus();
      }
    }
    return self.clients.openWindow(url);
  })());
});
