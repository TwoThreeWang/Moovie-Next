import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

// 在模拟环境里运行 sw.js：假的 caches、fetch 和事件。
function load({ online = true } = {}) {
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
    self, caches, URL, Response, Promise, addEventListener: self.addEventListener,
    fetch: async (request) => {
      network.push(typeof request === 'string' ? request : request.url);
      if (!online) throw new TypeError('offline');
      return new Response('network');
    },
  });
  vm.runInContext(fs.readFileSync(new URL('../static/sw.js', import.meta.url), 'utf8').replaceAll('__VERSION__', 'v1'), context);
  const dispatch = async (type, event) => {
    let waited, responded = null;
    listeners[type]({ ...event, waitUntil: (p) => { waited = p; }, respondWith: (p) => { responded = p; } });
    await waited;
    return responded ? await responded : null;
  };
  const request = (url, mode = 'no-cors') => ({ url, mode, method: 'GET' });
  return { dispatch, request, stores, network };
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
