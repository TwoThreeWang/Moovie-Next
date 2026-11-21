import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

// 在模拟环境里运行 sw.js：假的 caches、fetch 和事件。
function load({ online = true, respond } = {}) {
  const clock = { now: 1_000_000 };
  const stores = new Map();
  const network = [];
  const listeners = {};
  const cacheFor = (name) => {
    if (!stores.has(name)) stores.set(name, new Map());
    const store = stores.get(name);
    return {
      addAll: async (urls) => urls.forEach((url) => store.set(url, new Response('cached ' + url))),
      put: async (request, response) => store.set(typeof request === 'string' ? request : new URL(request.url).pathname + new URL(request.url).search, response),
      keys: async () => [...store.keys()],
    };
  };
  const caches = {
    open: async (name) => cacheFor(name),
    keys: async () => [...stores.keys()],
    delete: async (name) => stores.delete(name),
    match: async (request) => {
      const key = typeof request === 'string' ? request : new URL(request.url).pathname + new URL(request.url).search;
      for (const store of stores.values()) if (store.has(key)) return store.get(key).clone();
      return undefined;
    },
  };
  const self = {
    location: new URL('https://moovie.example/sw.js'),
    addEventListener: (type, fn) => { listeners[type] = fn; },
    skipWaiting: async () => {},
    clients: { claim: async () => {} },
    registration: { navigationPreload: { enable: async () => {} } },
  };
  const context = vm.createContext({
    self, caches, URL, Response, Promise, setTimeout, addEventListener: self.addEventListener,
    Date: { now: () => clock.now },
    fetch: async (request, init) => {
      const url = typeof request === 'string' ? request : request.url;
      network.push(url);
      if (!online) throw new TypeError('offline');
      return respond ? respond(url, init, network.length) : new Response('network');
    },
  });
  vm.runInContext(fs.readFileSync(new URL('../static/sw.js', import.meta.url), 'utf8').replaceAll('__VERSION__', 'v1'), context);
  const dispatch = async (type, event) => {
    let waited, responded = null;
    const waits = [];
    listeners[type]({ ...event, waitUntil: (p) => { waits.push(p); waited = p; }, respondWith: (p) => { responded = p; } });
    await waited;
    return responded ? await responded : null;
  };
  const request = (url, mode = 'no-cors', cache = 'default') => ({ url, mode, method: 'GET', cache });
  return { dispatch, request, stores, network, clock };
}

test('安装时缓存离线页和核心文件；激活时清掉旧版本的缓存', async () => {
  const sw = load();
  sw.stores.set('moovie-old', new Map([['x', new Response('old')]]));
  sw.stores.set('other-app', new Map());
  await sw.dispatch('install', {});
  await sw.dispatch('activate', {});
  assert.deepEqual([...sw.stores.keys()].sort(), ['moovie-v1', 'other-app']);
  assert.ok(sw.stores.get('moovie-v1').has('/offline'));
  assert.ok(sw.stores.get('moovie-v1').has('/static/css/app.css?v=v1'));
});

test('页面导航走网络，断网时显示离线页', async () => {
  const online = load();
  await online.dispatch('install', {});
  const page = await online.dispatch('fetch', { request: online.request('https://moovie.example/m/1', 'navigate') });
  assert.equal(await page.text(), 'network');

  const offline = load({ online: false });
  await offline.dispatch('install', {});
  const fallback = await offline.dispatch('fetch', { request: offline.request('https://moovie.example/m/1', 'navigate') });
  assert.equal(await fallback.text(), 'cached /offline');
});

test('只缓存带版本号的静态文件', async () => {
  const sw = load();
  await sw.dispatch('install', {});
  const first = await sw.dispatch('fetch', { request: sw.request('https://moovie.example/static/js/app.js?v=v1') });
  assert.equal(await first.text(), 'network');
  await new Promise((r) => setTimeout(r, 10));
  const before = sw.network.length;
  const second = await sw.dispatch('fetch', { request: sw.request('https://moovie.example/static/js/app.js?v=v1') });
  assert.equal(await second.text(), 'network', '第二次从缓存读');
  assert.equal(sw.network.length, before, '第二次不再请求网络');
  // 不带版本号的、接口、跨域的请求一律不拦截
  for (const url of ['https://moovie.example/static/js/app.js', 'https://moovie.example/api/positions', 'https://source.example/api.php/provide/vod/', 'https://cdn.example/a.m3u8']) {
    assert.equal(await sw.dispatch('fetch', { request: sw.request(url) }), null, url);
  }
  // 只拦截 GET
  assert.equal(await sw.dispatch('fetch', { request: { url: 'https://moovie.example/static/x.js?v=1', mode: 'no-cors', method: 'POST' } }), null);
});

// ---------- 预取交接 ----------
const ORIGIN = 'https://moovie.example';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// 预取（init 带 credentials）立刻回来，导航本身的请求慢 50ms，好看出用的是哪一份
function page(body, { status = 200, type = 'text/html; charset=utf-8', cache, redirected = false, delay = 0, hang = false } = {}) {
  return async () => {
    if (hang) return new Promise(() => {});
    if (delay) await sleep(delay);
    const headers = { 'Content-Type': type };
    if (cache) headers['Cache-Control'] = cache;
    const response = new Response(body, { status, headers });
    if (redirected) Object.defineProperty(response, 'redirected', { value: true });
    return response;
  };
}
function handoff(prefetchResponse = page('prefetched')) {
  return load({ respond: (url, init) => (init?.credentials ? prefetchResponse() : page('network', { delay: 50 })()) });
}
const nav = (sw, path, cache) => sw.dispatch('fetch', { request: sw.request(ORIGIN + path, 'navigate', cache) });
const ask = (sw, path) => sw.dispatch('message', { data: { type: 'prefetch', url: ORIGIN + path } });

test('预取交接：按下链接时取回的页面，打开时直接交出，只用一次', async () => {
  const sw = handoff();
  await ask(sw, '/m/1#cast');
  assert.deepEqual(sw.network, [ORIGIN + '/m/1'], '去掉 # 之后取一次');
  await ask(sw, '/m/1');
  assert.equal(sw.network.length, 1, '同一页有效期内不重复取');
  assert.equal(await (await nav(sw, '/m/1')).text(), 'prefetched');
  assert.equal(await (await nav(sw, '/m/1')).text(), 'network', '第二次打开走网络');
});

test('预取交接：任何一次导航都清空，别的页面的预取不会留到之后', async () => {
  const sw = handoff();
  await ask(sw, '/m/1');
  await ask(sw, '/m/2');
  assert.equal(await (await nav(sw, '/m/2')).text(), 'prefetched');
  assert.equal(await (await nav(sw, '/m/1')).text(), 'network');
});

test('预取交接：写操作清空预取，之后一小段时间内不再预取', async () => {
  const sw = handoff();
  await ask(sw, '/m/1');
  await sw.dispatch('fetch', { request: { url: ORIGIN + '/api/positions', mode: 'cors', method: 'POST' } });
  assert.equal(await (await nav(sw, '/m/1')).text(), 'network');

  const before = sw.network.length;
  await ask(sw, '/m/3');
  assert.equal(sw.network.length, before, '刚写过，不预取');
  sw.clock.now += 3001;
  await ask(sw, '/m/3');
  assert.equal(sw.network.length, before + 1, '过了安静期可以预取');
});

test('预取交接：预取还没回来时发生写操作，结果不用', async () => {
  const sw = handoff(page('prefetched', { delay: 20 }));
  await ask(sw, '/m/1');
  const opening = nav(sw, '/m/1');
  await sw.dispatch('fetch', { request: { url: ORIGIN + '/collections/x', mode: 'navigate', method: 'POST' } });
  assert.equal(await (await opening).text(), 'network');
});

test('预取交接：过期、刷新、不能用的响应都走网络', async () => {
  const expired = handoff();
  await ask(expired, '/m/1');
  expired.clock.now += 10001;
  assert.equal(await (await nav(expired, '/m/1')).text(), 'network', '过期');

  const reload = handoff();
  await ask(reload, '/m/1');
  assert.equal(await (await nav(reload, '/m/1', 'no-cache')).text(), 'network', '刷新');

  for (const [name, options] of [['404', { status: 404 }], ['跳转过', { redirected: true }], ['no-store', { cache: 'private, no-store' }], ['不是 HTML', { type: 'application/json' }]]) {
    const sw = handoff(page('prefetched', options));
    await ask(sw, '/m/1');
    assert.equal(await (await nav(sw, '/m/1')).text(), 'network', name);
  }
});

test('预取交接：预取卡住不拖慢打开', async () => {
  const sw = handoff(page('prefetched', { hang: true }));
  await ask(sw, '/m/1');
  const started = Date.now();
  assert.equal(await (await nav(sw, '/m/1')).text(), 'network');
  assert.ok(Date.now() - started < 500);
});

test('预取交接：播放、登录、后台、通知和跨域的地址不预取', async () => {
  const sw = handoff();
  for (const path of ['/play/1', '/play', '/auth/logout', '/admin', '/admin/users', '/notifications']) await ask(sw, path);
  await sw.dispatch('message', { data: { type: 'prefetch', url: 'https://other.example/m/1' } });
  await sw.dispatch('message', { data: { type: 'prefetch', url: 'not a url' } });
  await sw.dispatch('message', { data: { type: 'other', url: ORIGIN + '/m/1' } });
  assert.deepEqual(sw.network, []);
  await ask(sw, '/players');
  assert.equal(sw.network.length, 1, '只排除整段路径，不误伤 /players');
});
