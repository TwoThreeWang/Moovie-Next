// 播放源引擎：订阅管理、查询、缓存。
//
// 订阅文件和搜索、详情的查询一律由本站服务器转发（/api/relay）：大多数资源站没有给网页跨域许可，
// 浏览器直接访问会被拦下。只有本站自己的地址（比如示例订阅）和本机地址（开发用的模拟源）由浏览器直接访问。
// 播放链接不经过这里，由播放器直接加载。返回的内容一律当作不可信数据。
import * as idb from './idb.js';
import { userKey } from './api.js';
import { checkURL, searchURL, parseResponse, parseSubscription } from '../lib/cms.js';
import { bestMatch, hiddenMatch, searchKeyword } from '../lib/match.js';

const QUERY_TIMEOUT = 8000; // 转发要多走一段路，给够时间
const SUBSCRIPTION_TTL = 6 * 3600 * 1000;
const RESULT_FRESH = 10 * 60 * 1000;   // 10 分钟内的结果直接用
const RESULT_KEEP = 24 * 3600 * 1000;  // 更旧的先显示，同时在后台刷新

const FETCH_OPTIONS = { credentials: 'omit', referrerPolicy: 'no-referrer', mode: 'cors' };

/** 读取一个地址的文本：本站和本机的地址直接访问，其他的走本站转发。 */
function fetchText(url, { signal, timeout = QUERY_TIMEOUT } = {}) {
  const target = new URL(url, location.href);
  const local = target.origin === location.origin || target.hostname === 'localhost' || target.hostname === '127.0.0.1';
  return local ? direct(target.href, { signal, timeout }) : relay(target.href, { signal, timeout });
}

/** 由本站服务器转发。要登录；对方超时、连不上、返回错误，分别对应 timeout / network / http。 */
async function relay(url, { signal, timeout }) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(new DOMException('timeout', 'TimeoutError')), timeout);
  signal?.addEventListener('abort', () => controller.abort(signal.reason), { once: true });
  try {
    const response = await fetch('/api/relay?' + new URLSearchParams({ url }), { credentials: 'same-origin', signal: controller.signal });
    if (response.ok) return await response.text();
    const code = response.status === 504 ? 'timeout' : response.status === 502 || response.status === 401 || response.status === 403 ? 'network' : 'http';
    throw Object.assign(new Error('HTTP ' + response.status), { code });
  } catch (error) {
    if (error.code) throw error;
    const timedOut = controller.signal.aborted && controller.signal.reason?.name === 'TimeoutError';
    throw Object.assign(new Error(timedOut ? '超时' : '无法连接'), { code: timedOut ? 'timeout' : 'network' });
  } finally {
    clearTimeout(timer);
  }
}

async function direct(url, { signal, timeout }) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(new DOMException('timeout', 'TimeoutError')), timeout);
  signal?.addEventListener('abort', () => controller.abort(signal.reason), { once: true });
  try {
    const response = await fetch(url, { ...FETCH_OPTIONS, signal: controller.signal });
    if (!response.ok) throw Object.assign(new Error('HTTP ' + response.status), { code: 'http' });
    return await response.text();
  } catch (error) {
    if (error.code) throw error;
    const timedOut = controller.signal.aborted && controller.signal.reason?.name === 'TimeoutError';
    throw Object.assign(new Error(timedOut ? '超时' : '无法连接'), { code: timedOut ? 'timeout' : 'network' });
  } finally {
    clearTimeout(timer);
  }
}

// ---------- 订阅 ----------

/** 读取本地保存的订阅：{ url, sources, errors, fetchedAt } 或 null。 */
export const getSubscription = () => idb.get('kv', userKey('subscription'));

/** 当前可用的源，按最近的表现排序（成功率高、速度快的在前）。 */
export async function getSources() {
  const subscription = await getSubscription();
  if (!subscription?.sources?.length) return [];
  const health = (await idb.get('kv', userKey('health'))) ?? {};
  const score = (source) => {
    const stats = health[source.key];
    if (!stats || stats.ok + stats.fail === 0) return 0.5;
    return stats.ok / (stats.ok + stats.fail) - Math.min(stats.ms, 4000) / 40000;
  };
  return [...subscription.sources].sort((a, b) => score(b) - score(a));
}

/**
 * 保存订阅地址并读取一次。返回订阅状态；地址不对或读取失败时抛错（error.message 可以直接给用户看）。
 */
export async function setSubscription(url) {
  return saveSubscription(await readSubscription(url));
}

/** 读取并校验候选订阅，不改动本机已经生效的配置。 */
export async function readSubscription(url) {
  const checked = checkURL(url, location.href);
  if (!checked.url) throw new Error('订阅地址不正确');
  return readCheckedSubscription(checked.url);
}

async function readCheckedSubscription(url) {
  let text;
  try {
    text = await fetchText(url, { timeout: 8000 });
  } catch (error) {
    throw new Error(error.code === 'http' ? `读取订阅失败（${error.message}）` : error.code === 'timeout' ? '读取订阅失败：对方响应太慢' : '读取订阅失败：地址打不开');
  }
  const { sources, errors } = parseSubscription(text, url);
  if (!sources.length) throw new Error(errors[0] ?? '订阅里没有可用的源');
  return { url, sources, errors, fetchedAt: Date.now() };
}

/** 账号保存确认后，或同步账号配置时，才让候选配置在本机生效。 */
export async function saveSubscription(subscription) {
  const previous = await getSubscription();
  await idb.set('kv', userKey('subscription'), subscription);
  globalThis.dispatchEvent?.(new CustomEvent('moovie:subscription-change'));
  const { sources } = subscription;
  if (previous) await dropRemovedSources(previous.sources, sources);
  return subscription;
}

async function loadSubscription(url) { return saveSubscription(await readCheckedSubscription(url)); }

/** 订阅里删掉的源：清掉指向它的"上次用的源"。 */
async function dropRemovedSources(before, after) {
  const alive = new Set(after.map((s) => s.key));
  if (before.every((s) => alive.has(s.key))) return;
  for (const { key, value } of await idb.entries('kv', userKey('last:'))) {
    if (!alive.has(value?.source)) await idb.del('kv', key);
  }
}

/** 订阅超过 6 小时没更新时重新读取。失败时继续用旧的。 */
export async function refreshSubscription({ force = false } = {}) {
  const subscription = await getSubscription();
  if (!subscription) return null;
  if (!force && Date.now() - subscription.fetchedAt < SUBSCRIPTION_TTL) return subscription;
  try { return await loadSubscription(subscription.url); } catch (error) {
    if (force) throw error;
    return subscription;
  }
}

/** 清除订阅和所有相关的本地缓存。 */
export async function clearSubscription() {
  await idb.del('kv', userKey('subscription'));
  await idb.del('kv', userKey('health'));
  for (const prefix of [userKey('last:')]) for (const { key } of await idb.entries('kv', prefix)) await idb.del('kv', key);
  for (const prefix of ['m:', 'm2:']) for (const { key } of await idb.entries('cache', userKey(prefix))) await idb.del('cache', key);
  globalThis.dispatchEvent?.(new CustomEvent('moovie:subscription-change'));
}

// ---------- 查询 ----------

async function recordHealth(key, ok, ms) {
  const health = (await idb.get('kv', userKey('health'))) ?? {};
  const stats = health[key] ?? { ok: 0, fail: 0, ms: 0 };
  if (ok) { stats.ms = stats.ok ? Math.round(stats.ms * 0.7 + ms * 0.3) : ms; stats.ok++; } else stats.fail++;
  // 只看最近的表现：次数多了就整体减半
  if (stats.ok + stats.fail > 40) { stats.ok = Math.ceil(stats.ok / 2); stats.fail = Math.floor(stats.fail / 2); }
  health[key] = stats;
  await idb.set('kv', userKey('health'), health);
}

/** 在一个源里搜索。返回 { items, ms }；失败时抛出带 code 的错误（timeout / network / http）。 */
export async function querySource(source, keyword, { signal } = {}) {
  const started = performance.now();
  try {
    const text = await fetchText(searchURL(source.api, keyword), { signal });
    const ms = Math.round(performance.now() - started);
    recordHealth(source.key, true, ms);
    return { items: parseResponse(text), ms };
  } catch (error) {
    if (!signal?.aborted) recordHealth(source.key, false, 0);
    throw error;
  }
}

/**
 * 在所有源里搜索一个关键词。每个源一有结果就调用 onResult({ source, items, ms, error })，不等其他源。
 */
export async function searchAll(keyword, onResult, { signal } = {}) {
  const sources = await getSources();
  await Promise.all(sources.map(async (source) => {
    try {
      const { items, ms } = await querySource(source, keyword, { signal });
      onResult({ source, items, ms });
    } catch (error) {
      if (!signal?.aborted) onResult({ source, items: [], error });
    }
  }));
  return sources.length;
}

const ERROR_TEXT = { timeout: '超时', network: '无法连接', http: '接口出错' };

/**
 * 为一部作品找可播放的源。
 * 返回 { at, entries: [{ key, name, status, ms, item, message }] }，status 取 ok / none / error。
 * 有缓存时先通过 onUpdate 给出缓存，缓存不够新时再在后台刷新并再次调用 onUpdate。
 */
export async function findForMedia(media, { onUpdate, force = false, signal } = {}) {
  // 键名带版本：匹配规则变了（比如开始排除预告片、枪版）就换一个，旧结果不再拿来用。
  const cacheKey = userKey('m2:' + media.id);
  const cached = await idb.get('cache', cacheKey);
  if (cached && !force && Date.now() - cached.at < RESULT_KEEP) {
    onUpdate?.(cached, { cached: true, done: Date.now() - cached.at < RESULT_FRESH });
    if (Date.now() - cached.at < RESULT_FRESH) return cached;
  }
  const sources = await getSources();
  const result = { at: Date.now(), entries: [] };
  if (!sources.length) {
    onUpdate?.(result, { done: true });
    return result;
  }
  const keyword = searchKeyword(media);
  await Promise.all(sources.map(async (source) => {
    const entry = { key: source.key, name: source.name, status: 'none', ms: 0, item: null, message: '', hidden: '' };
    try {
      const { items, ms } = await querySource(source, keyword, { signal });
      entry.ms = ms;
      const match = bestMatch(media, items);
      if (match) { entry.status = 'ok'; entry.item = match.item; }
      else entry.hidden = hiddenMatch(media, items); // 只有预告片或枪版：不能播，但告诉用户为什么
    } catch (error) {
      entry.status = 'error';
      entry.message = ERROR_TEXT[error.code] ?? '出错';
    }
    result.entries.push(entry);
    result.entries.sort((a, b) => (a.status === 'ok' ? 0 : 1) - (b.status === 'ok' ? 0 : 1) || a.ms - b.ms);
    onUpdate?.(result, { done: result.entries.length === sources.length });
  }));
  if (!signal?.aborted) await idb.set('cache', cacheKey, result);
  return result;
}

// ---------- 上次用的源 ----------

/** 读取某部作品上次成功播放时用的源和线路。 */
export const getLast = (mediaKey) => idb.get('kv', userKey('last:' + mediaKey));

/** 记住这次成功播放用的源和线路。只存在本机。 */
export const setLast = (mediaKey, value) => idb.set('kv', userKey('last:' + mediaKey), value);
