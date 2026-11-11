/* Atelier service worker. Served from /sw.js (scope "/") by internal/web/pwa.go,
   which replaces __ATELIER_VERSION__ with the build version.

   - App shell and /static/ files: cache first, refreshed in the background,
     in a cache named after the build version (old versions are deleted).
   - Pages (HTML): network first. Atelier marks its pages no-store, so they
     are never kept; when the server can't be reached the offline page shows.
   - Never cached, never even looked at: anything that isn't a same-origin GET,
     requests with an Authorization header, the APIs (/api/, /mcp), git and LFS
     (/owner/repo.git/…), package registries (/npm/, /v2/, /go/, /packages/,
     /osv), sign-in, raw files, downloads and Actions logs, and any response marked
     Cache-Control: no-store or private. */
'use strict';

const VERSION = '__ATELIER_VERSION__';
const PREFIX = 'atelier-';
const STATIC_CACHE = PREFIX + 'static-' + VERSION;
const PAGE_CACHE = PREFIX + 'pages-' + VERSION;
const OFFLINE_URL = '/static/offline.html';

const SHELL = [
  OFFLINE_URL,
  '/static/atelier.css',
  '/static/mobile.css',
  '/static/atelier.js',
  '/static/pwa.js',
  '/static/icon.svg',
  '/static/icons/icon-192.png',
  '/manifest.webmanifest',
];

// Paths that must always go straight to the network.
const NEVER_CACHE = [
  /^\/api(\/|$)/,
  /^\/mcp(\/|$)/,
  /^\/npm(\/|$)/,
  /^\/v2(\/|$)/,
  /^\/go(\/|$)/,
  /^\/packages(\/|$)/,
  /^\/osv(\/|$)/,
  /^\/auth(\/|$)/,
  /^\/signin(\/|$)/,
  /^\/signout(\/|$)/,
  /^\/setup(\/|$)/,
  /^\/healthz$/,
  /^\/sw\.js$/,
  /^\/notifications\/count$/,
  /^\/[^/]+\/[^/]+\.git(\/|$)/i,
  /\/info\/refs$/,
  /\/git-(upload|receive)-pack$/,
  /\/info\/lfs(\/|$)/,
  /^\/[^/]+\/[^/]+\/raw\//,
  /^\/[^/]+\/[^/]+\/archive\//,
  /^\/[^/]+\/[^/]+\/releases\/download\//,
  /^\/[^/]+\/[^/]+\/actions\/runs\/\d+\/(.+\/)?logs(\/|$)/,
];

function neverCache(url, request) {
  if (request.method !== 'GET') return true;
  if (url.origin !== self.location.origin) return true;
  if (request.headers.has('Authorization')) return true;
  return NEVER_CACHE.some((re) => re.test(url.pathname));
}

// storable reports whether a response may be kept: a plain same-origin 200
// that the server didn't mark no-store or private.
function storable(response) {
  if (!response || response.status !== 200 || response.type !== 'basic') return false;
  const cc = (response.headers.get('Cache-Control') || '').toLowerCase();
  return !/(^|[,\s])(no-store|private)([,\s=]|$)/.test(cc);
}

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE)
      .then((cache) => cache.addAll(SHELL))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys
        .filter((k) => k.startsWith(PREFIX) && k !== STATIC_CACHE && k !== PAGE_CACHE)
        .map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

function isPage(request) {
  return request.mode === 'navigate' ||
    (request.headers.get('Accept') || '').includes('text/html');
}

async function networkFirst(request) {
  try {
    const response = await fetch(request);
    if (storable(response)) {
      const copy = response.clone();
      caches.open(PAGE_CACHE).then((c) => c.put(request, copy)).catch(() => {});
    }
    return response;
  } catch (err) {
    const cached = await caches.match(request, { cacheName: PAGE_CACHE });
    if (cached) return cached;
    const offline = await caches.match(OFFLINE_URL);
    if (offline) return offline;
    return new Response('Atelier is offline.', { status: 503, headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
  }
}

async function cacheFirst(request) {
  const cache = await caches.open(STATIC_CACHE);
  const cached = await cache.match(request);
  const refresh = fetch(request).then((response) => {
    if (storable(response)) cache.put(request, response.clone()).catch(() => {});
    return response;
  });
  if (cached) {
    refresh.catch(() => {});
    return cached;
  }
  return refresh;
}

self.addEventListener('fetch', (event) => {
  const request = event.request;
  const url = new URL(request.url);
  if (neverCache(url, request)) return; // the browser handles it as if there were no worker
  if (isPage(request)) {
    event.respondWith(networkFirst(request));
    return;
  }
  if (url.pathname.startsWith('/static/') || url.pathname === '/manifest.webmanifest') {
    event.respondWith(cacheFirst(request));
  }
});

// Web Push (needs HTTPS and a push sender on the server, see pwa.go). The
// payload is {"title", "body", "url"}; an empty push opens the inbox.
self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (e) { data = {}; }
  const title = data.title || 'Atelier';
  const url = typeof data.url === 'string' && data.url.startsWith('/') && !data.url.startsWith('//') ? data.url : '/notifications';
  event.waitUntil(self.registration.showNotification(title, {
    body: data.body || 'Something needs you.',
    icon: '/static/icons/icon-192.png',
    badge: '/static/icons/maskable-192.png',
    data: { url },
    tag: data.tag || url,
  }));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || '/notifications';
  event.waitUntil(self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
    for (const c of list) {
      if (new URL(c.url).origin === self.location.origin && 'focus' in c) {
        c.navigate(url);
        return c.focus();
      }
    }
    return self.clients.openWindow(url);
  }));
});
