// Service Worker：让"添加到桌面"后的启动更快，断网时给出提示页。
//
// 只做三件事，不碰任何接口和用户数据：
//   1. 带版本号的静态文件（/static/…?v=…）缓存起来，下次直接从本机读。
//   2. 页面导航走网络（借助 navigation preload 不额外等待），断网时显示离线页。
//   3. 新版本上线后清掉旧缓存。
// 不缓存 HTML 页面，避免看到过期的内容；不缓存 /api/，也不经手任何播放源的请求（跨域请求一律不拦截）。
const VERSION = '__VERSION__';
const CACHE = 'moovie-' + VERSION;
const CORE = [
  '/offline',
  '/static/css/app.css?v=' + VERSION,
  '/static/css/site.css?v=' + VERSION,
  '/static/js/htmx.min.js?v=' + VERSION,
  '/static/img/apple-touch-icon.png',
];

self.addEventListener('install', (event) => {
  event.waitUntil(caches.open(CACHE).then((cache) => cache.addAll(CORE)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(names.filter((name) => name.startsWith('moovie-') && name !== CACHE).map((name) => caches.delete(name)));
    if (self.registration.navigationPreload) await self.registration.navigationPreload.enable();
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const request = event.request;
  if (request.method !== 'GET') return;
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return; // 播放源、图片 CDN 等跨域请求不经手

  if (request.mode === 'navigate') {
    event.respondWith((async () => {
      try {
        return (await event.preloadResponse) || (await fetch(request));
      } catch {
        return (await caches.match('/offline')) || Response.error();
      }
    })());
    return;
  }

  // 只缓存带版本号的静态文件：内容不会变，可以放心长期用。
  if (url.pathname.startsWith('/static/') && url.searchParams.has('v')) {
    event.respondWith((async () => {
      const cached = await caches.match(request);
      if (cached) return cached;
      const response = await fetch(request);
      if (response.ok) {
        const copy = response.clone();
        caches.open(CACHE).then((cache) => cache.put(request, copy));
      }
      return response;
    })());
  }
});
