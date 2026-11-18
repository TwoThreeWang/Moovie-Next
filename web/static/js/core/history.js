// 观看历史的同步。
//
// 有元数据的作品：进度按"作品 ID + 季 + 集"明文上报，不含任何源信息。
// 没有元数据的作品：记录在浏览器里加密后才上传，服务器只看到密文。
import * as idb from './idb.js';
import { api, userKey } from './api.js';
import { keys } from './keys.js';
import { seal, open, recordId, toBase64, fromBase64 } from '../lib/vault.js';

// ---------- 明文进度 ----------

/** 上报一条进度。失败时存在本地，下次再补。 */
export async function reportPosition(position, { keepalive = false } = {}) {
  const item = { ...position, at: Date.now() };
  try {
    const response = await api('/api/positions', { method: 'POST', json: { items: [item] }, keepalive });
    if (!response.ok && response.status >= 500) throw new Error('server');
  } catch {
    const pending = (await idb.get('kv', userKey('pendingPositions'))) ?? [];
    const key = (p) => `${p.media_id}:${p.season}:${p.episode}`;
    await idb.set('kv', userKey('pendingPositions'), [...pending.filter((p) => key(p) !== key(item)), item].slice(-100));
  }
}

/** 把之前没发出去的进度补发。 */
export async function flushPositions() {
  const pending = await idb.get('kv', userKey('pendingPositions'));
  if (!pending?.length) return;
  try {
    const response = await api('/api/positions', { method: 'POST', json: { items: pending } });
    if (response.ok || response.status < 500) await idb.del('kv', userKey('pendingPositions'));
  } catch { /* 下次再试 */ }
}

// ---------- 加密历史 ----------
// 本地每条记录：{ rid, data, version, dirty }。data 为 null 表示已删除。

const privateKey = (rid) => userKey('p:' + rid);

/** 计算一部作品的记录 ID。 */
export async function privateId(info) {
  const { ridKey } = await keys();
  return recordId(ridKey, info);
}

/** 读取一条本地记录的内容；没有或已删除时返回 null。 */
export async function getPrivate(rid) {
  return (await idb.get('private', privateKey(rid)))?.data ?? null;
}

/** 列出本地的加密历史，最近看的在前。 */
export async function listPrivate() {
  const records = await idb.entries('private', userKey('p:'));
  return records.map((r) => r.value).filter((r) => r.data).map((r) => ({ rid: r.rid, ...r.data })).sort((a, b) => b.at - a.at);
}

/** 保存一条记录（data 为 null 表示删除），并尝试上传。 */
export async function savePrivate(rid, data, { keepalive = false } = {}) {
  const existing = await idb.get('private', privateKey(rid));
  const record = { rid, data, version: existing?.version ?? 0, dirty: true };
  await idb.set('private', privateKey(rid), record);
  await push(record, { keepalive });
}

async function push(record, { keepalive = false, retry = true } = {}) {
  const { encKey } = await keys();
  let blob = null;
  if (record.data) blob = toBase64(await seal(encKey, record.data));
  let response;
  try {
    response = await api('/api/private-history/' + record.rid, { method: 'PUT', json: { blob, base: record.version }, keepalive });
  } catch { return; } // 离线：保持 dirty，下次同步再传
  if (response.ok) {
    const { version } = await response.json();
    await idb.set('private', privateKey(record.rid), { ...record, version, dirty: false });
    return;
  }
  if (response.status !== 409) return;
  // 另一台设备改过这条记录：比较两边的播放时间，保留较新的。
  const { current } = await response.json();
  const remote = current?.blob ? await open(encKey, fromBase64(current.blob)).catch(() => null) : null;
  const remoteAt = remote?.at ?? 0, localAt = record.data?.at ?? Date.now();
  if (current && remoteAt > localAt) {
    await idb.set('private', privateKey(record.rid), { rid: record.rid, data: remote, version: current.version, dirty: false });
  } else if (retry) {
    await push({ ...record, version: current?.version ?? 0 }, { keepalive, retry: false });
  }
}

/** 和服务器同步加密历史：先拉取别的设备的改动，再上传本地没传的。返回是否有变化。 */
export async function syncPrivate() {
  const { encKey } = await keys();
  let changed = false;
  let since = (await idb.get('kv', userKey('privateVersion'))) ?? 0;
  for (let page = 0; page < 10; page++) {
    let payload;
    try {
      const response = await api('/api/private-history?since=' + since);
      if (!response.ok) break;
      payload = await response.json();
    } catch { break; }
    for (const item of payload.items) {
      const local = await idb.get('private', privateKey(item.rid));
      if (local && local.version >= item.version) continue;
      let data = null;
      if (item.blob) {
        try { data = await open(encKey, fromBase64(item.blob)); } catch { continue; } // 用别的密钥加密的记录：读不了，跳过
      }
      if (local?.dirty && (local.data?.at ?? 0) >= (data?.at ?? 0)) {
        await idb.set('private', privateKey(item.rid), { ...local, version: item.version }); // 本地更新，稍后上传
      } else {
        await idb.set('private', privateKey(item.rid), { rid: item.rid, data, version: item.version, dirty: false });
        changed = true;
      }
    }
    since = payload.version;
    if (payload.items.length < 500) break;
  }
  await idb.set('kv', userKey('privateVersion'), since);
  for (const { value } of await idb.entries('private', userKey('p:'))) if (value.dirty) await push(value);
  return changed;
}

/**
 * 作品库后来收录了这部作品：把加密记录转成普通的进度记录，然后删除加密记录。
 * 每天最多检查一次。请求里只有片名、年份和类型。
 */
export async function promotePrivate() {
  const last = (await idb.get('kv', userKey('promotedAt'))) ?? 0;
  if (Date.now() - last < 24 * 3600 * 1000) return 0;
  const records = (await listPrivate()).slice(0, 40);
  if (!records.length) return 0;
  await idb.set('kv', userKey('promotedAt'), Date.now());
  let response;
  try {
    response = await api('/api/media/resolve', { method: 'POST', json: { items: records.map((r) => ({ title: r.title, year: r.year, type: r.type === 'movie' ? 'movie' : r.type ? 'tv' : '' })) } });
    if (!response.ok) return 0;
  } catch { return 0; }
  const { results } = await response.json();
  let promoted = 0;
  for (const [index, result] of results.entries()) {
    if (result.status !== 'matched') continue;
    const record = records[index], media = result.media;
    const item = { media_id: media.id, season: media.season || 1, episode: record.ep || 1, position: record.pos || 0, duration: record.dur || 0, completed: !!record.done, at: record.at };
    try {
      const saved = await api('/api/positions', { method: 'POST', json: { items: [item] } });
      if (!saved.ok) continue;
    } catch { continue; }
    await savePrivate(record.rid, null);
    promoted++;
  }
  return promoted;
}
