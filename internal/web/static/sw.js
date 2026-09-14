// A deliberately empty service worker.
//
// It exists because browsers require one with a fetch handler before they will
// offer to install an app. This product is online-only by design, so the
// handler passes everything through to the network rather than caching: a
// stale schedule is worse than no schedule, and cache invalidation is a class
// of bug nobody here wants to debug remotely.
self.addEventListener('install', () => self.skipWaiting());

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('fetch', () => {
  // No respondWith: the browser handles the request normally.
});
