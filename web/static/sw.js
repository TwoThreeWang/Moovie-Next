// Service Worker：让"添加到桌面"后的启动更快，断网时给出提示页。
//
// 只做四件事，不碰任何接口和用户数据：
//   1. 带版本号的静态文件（/static/…?v=…）缓存起来，下次直接从本机读。
//   2. 页面导航走网络（借助 navigation preload 不额外等待），断网时显示离线页。
//   3. 新版本上线后清掉旧缓存。
//   4. 预取交接：给不支持 Speculation Rules 的浏览器（Safari / Firefox）用，见下面 handoff 部分。
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

// ---------- 预取交接 ----------
// 页面在手指按下 / 鼠标停留时发来 {type: 'prefetch', url}，这里先把那一页取回来放在内存里；
// 紧接着真的打开这一页时直接交出去，省掉一次等待网络的时间。为了不看到过期内容，规则很保守：
//   - 只放内存，最多 PREFETCH_TTL 毫秒，用一次就丢；任何一次页面导航（不论用没用上）都会清空全部。
//   - 页面发出任何非 GET 请求（标记看过、发短评、退出登录……）就清空全部，并作废正在取的；
//     写操作之后 WRITE_QUIET 毫秒内不预取，避免取回写入生效之前的页面。
//   - 只收 200、没跳转过、是 HTML、没写 no-store 的响应；刷新页面（cache 不是 default）不用。
//   - 导航时预取结果和正常走网络赛跑，谁先回来用谁，预取卡住不会拖慢打开。
// 出任何问题都退回原来的走网络。Service Worker 被系统回收时内存清空，也只是退回走网络。
const PREFETCH_TTL = 10000;
const WRITE_QUIET = 3000;
const PREFETCH_MAX = 4;
const NO_PREFETCH = /^\/(play|auth|admin|notifications)(\/|$)/;
const prefetched = new Map(); // 地址 → { at, response: Promise<Response|null> }
let lastWrite = 0;

function usable(response) {
  return response.status === 200 && !response.redirected
    && (response.headers.get('Content-Type') || '').startsWith('text/html')
    && !/no-store/i.test(response.headers.get('Cache-Control') || '');
}

function prefetch(href) {
  let url;
  try { url = new URL(href); } catch { return; }
  url.hash = '';
  if (url.origin !== self.location.origin || NO_PREFETCH.test(url.pathname)) return;
  if (Date.now() - lastWrite < WRITE_QUIET) return;
  const key = url.href;
  const existing = prefetched.get(key);
  if (existing && Date.now() - existing.at < PREFETCH_TTL) return;
  if (prefetched.size >= PREFETCH_MAX) prefetched.delete(prefetched.keys().next().value);
  const entry = { at: Date.now() };
  // 清空 Map 就等于作废：正在取的请求即使之后返回，也没有人再能拿到它
  entry.response = fetch(key, { credentials: 'same-origin' })
    .then((response) => (usable(response) ? response : null), () => null);
  prefetched.set(key, entry);
}

// 取出一份可以交给这次导航的预取结果；没有就返回 null。不论有没有，都清空全部。
async function takePrefetched(request) {
  const entry = request.cache === 'default' ? prefetched.get(request.url.split('#')[0]) : undefined;
  prefetched.clear();
  if (!entry || Date.now() - entry.at > PREFETCH_TTL) return null;
  const written = lastWrite;
  const response = await entry.response;
  // 等结果的这段时间里又有写操作：不用了
  return lastWrite === written ? response : null;
}

self.addEventListener('message', (event) => {
  if (event.data?.type === 'prefetch' && typeof event.data.url === 'string') prefetch(event.data.url);
});

self.addEventListener('fetch', (event) => {
  const request = event.request;
  if (request.method !== 'GET') {
    lastWrite = Date.now();
    prefetched.clear();
    return;
  }
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return; // 播放源、图片 CDN 等跨域请求不经手

  if (request.mode === 'navigate') {
    event.respondWith((async () => {
      try {
        const network = (async () => (await event.preloadResponse) || (await fetch(request)))();
        // 预取的结果和正常走网络赛跑：预取还没回来、卡住或者不能用，都不耽误正常加载
        const early = await Promise.race([takePrefetched(request).catch(() => null), network.then(() => null, () => null)]);
        if (early) {
          event.waitUntil(network.catch(() => {})); // 正常请求已经发出，让它安静地结束，不用它的结果
          return early;
        }
        return await network;
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
