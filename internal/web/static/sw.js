// The service worker.
//
// It exists for two reasons: browsers require one with a fetch handler before
// they will offer to install an app, and push notifications are delivered
// through it while the app is closed.
//
// This product is online-only by design, so the fetch handler passes
// everything through to the network. A stale schedule is worse than no
// schedule, and cache invalidation is a class of bug nobody wants to debug
// remotely from a glazier's phone.
self.addEventListener('install', () => self.skipWaiting());

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('fetch', () => {
  // No respondWith: the browser handles the request normally.
});

self.addEventListener('push', (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch (e) {
    // A malformed payload should still surface something rather than nothing.
  }

  const title = data.title || 'Your brief';
  event.waitUntil(
    self.registration.showNotification(title, {
      body: data.body || '',
      icon: '/static/icon-192.png',
      badge: '/static/icon-192.png',
      // Tagging by day means a rewritten brief replaces the morning's
      // notification instead of stacking a second one on the lock screen.
      tag: data.tag || 'brief',
      renotify: false,
      data: { url: data.url || '/' },
    })
  );
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const target = (event.notification.data && event.notification.data.url) || '/';

  // Focus the app if it is already open rather than opening a second window.
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((windows) => {
      for (const client of windows) {
        if ('focus' in client) {
          client.navigate(target);
          return client.focus();
        }
      }
      return self.clients.openWindow(target);
    })
  );
});
