/* Doot service worker.
 *
 * Scope is deliberately narrow: precache the app shell so the PWA launches like
 * an app, and cache nothing else. A cached conversation or a cached run status
 * would be worse than no data, because it would show stale agent state that the
 * operator might act on.
 */

const VERSION = 'doot-shell-v1';

/* Static assets only. No HTML, no API responses. */
const SHELL = [
  '/static/css/app.css',
  '/static/js/app.js',
  '/static/js/htmx.min.js',
  '/static/icons/icon-192.png',
  '/static/icons/icon-512.png',
  '/manifest.webmanifest'
];

const OFFLINE_URL = '/offline';

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(VERSION).then(async (cache) => {
      /* Cache entries individually: one missing asset must not fail the whole
         install and leave the app with no worker at all. */
      await Promise.all(
        SHELL.map((url) =>
          cache.add(new Request(url, { cache: 'reload' })).catch((err) => {
            console.warn('sw: could not precache', url, err);
          })
        )
      );
      await cache.add(new Request(OFFLINE_URL, { cache: 'reload' })).catch(() => {});
    }).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(
        keys.filter((k) => k !== VERSION).map((k) => caches.delete(k))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const request = event.request;
  const url = new URL(request.url);

  /* Never touch anything that is not a plain GET on this origin. */
  if (request.method !== 'GET' || url.origin !== self.location.origin) return;

  /* Never cache live or authenticated data. The SSE stream in particular must
     reach the network untouched, or the whole live-update model breaks. */
  if (
    url.pathname === '/events' ||
    url.pathname.startsWith('/api/') ||
    url.pathname === '/healthz' ||
    url.pathname === '/login' ||
    url.pathname === '/sw.js'
  ) {
    return;
  }

  /* Navigations: network-first, falling back to the offline page. Never serve a
     cached HTML document. */
  if (request.mode === 'navigate') {
    event.respondWith(
      fetch(request).catch(() => caches.match(OFFLINE_URL).then(
        (cached) => cached || new Response(
          'Offline', { status: 503, headers: { 'Content-Type': 'text/plain' } }
        )
      ))
    );
    return;
  }

  /* Static assets: cache-first, since they are versioned by worker version. */
  if (url.pathname.startsWith('/static/') || url.pathname === '/manifest.webmanifest') {
    event.respondWith(
      caches.match(request).then((cached) => {
        if (cached) return cached;
        return fetch(request).then((response) => {
          if (response && response.ok) {
            const copy = response.clone();
            caches.open(VERSION).then((cache) => cache.put(request, copy));
          }
          return response;
        });
      })
    );
  }
});
