// 钥匙圈同步：登录同一个账号的设备，自动拿到同一把主密钥和同一个订阅地址，不用扫码配对。
//
// 内容 { v, key, sub } 用登录密码派生的密钥加密后存到服务器（/api/keyring）。
// 服务器只有密文和随机盐；它不保存密码，解不开。
// 登录时（这时脚本手里有密码）解开钥匙圈；派生出来的密钥存在本机（不可导出），
// 之后改订阅地址时用它重新加密上传，不需要再输密码。
import * as idb from './idb.js';
import { api, userKey } from './api.js';
import { getMasterKey, setMasterKey, ensureMasterKey } from './keys.js';
import * as sources from './sources.js';
import { deriveWrapKey, wrapKeyring, unwrapKeyring, toBase64, fromBase64, KEYRING_ITERATIONS } from '../lib/vault.js';

const LOCAL = () => userKey('keyring');

async function fetchRemote() {
  const response = await api('/api/keyring');
  if (response.status === 404) return null;
  if (!response.ok) throw new Error('keyring');
  return response.json();
}

async function upload(local, payload, base) {
  const blob = await wrapKeyring(local.wrapKey, payload);
  const response = await api('/api/keyring', { method: 'PUT', json: { salt: local.salt, iterations: local.iterations, blob: toBase64(blob), base } });
  if (response.status === 409) return { conflict: (await response.json()).current ?? null };
  if (!response.ok) throw new Error('keyring');
  const { version } = await response.json();
  await idb.set('kv', LOCAL(), { ...local, version });
  return { version };
}

/** 这台设备现在的内容。 */
async function localPayload() {
  return { v: 1, key: await ensureMasterKey(), sub: (await sources.getSubscription())?.url ?? '' };
}

/** 用一个新密码（新的盐）把这台设备的内容加密上传，覆盖服务器上的。 */
async function rewrap(password, base) {
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const local = { wrapKey: await deriveWrapKey(password, salt, KEYRING_ITERATIONS), salt: toBase64(salt), iterations: KEYRING_ITERATIONS, version: base };
  const result = await upload(local, await localPayload(), base);
  if (result.conflict) return rewrap(password, result.conflict.version);
}

/**
 * 换成账号的主密钥。这台设备之前用的是另一把密钥时，把本机已有的观看记录用新密钥重新加密：
 * 记录 ID 也是从密钥算出来的，所以要换 ID，旧 ID 的密文删掉。
 */
async function adoptKey(key) {
  const current = await getMasterKey();
  if (!key || key === current) return;
  if (!current) { await setMasterKey(key); return; }
  const history = await import('./history.js');
  await history.syncPrivate().catch(() => {}); // 先用旧密钥把服务器上的拉下来
  const records = await history.listPrivate();
  await setMasterKey(key);
  for (const { rid, ...data } of records) {
    const next = await history.privateId({ title: data.title, year: data.year, type: data.type });
    if (next === rid) continue;
    await history.savePrivate(next, data).catch(() => {});
    await history.savePrivate(rid, null).catch(() => {});
  }
}

/** 换成账号的订阅地址。读取失败时也先记下地址，之后会自动重试。 */
async function adoptSubscription(url) {
  const current = (await sources.getSubscription())?.url ?? '';
  if (!url || url === current) return;
  try {
    await sources.setSubscription(url);
  } catch {
    await idb.set('kv', userKey('subscription'), { url, sources: [], errors: ['订阅暂时读不了，稍后自动重试'], fetchedAt: 0 });
  }
}

async function adopt(payload, local) {
  await adoptKey(payload.key);
  await adoptSubscription(payload.sub);
  // 账号里还没有订阅地址，这台设备有：补上去
  if (!payload.sub && (await sources.getSubscription())?.url) await upload(local, await localPayload(), local.version);
}

/**
 * 登录成功后调用（document.body.dataset.user 已经是这个用户）。返回发生了什么：
 * created：账号还没有钥匙圈，用这台设备的建了一个；synced：解开并同步了；
 * rewrapped：密码在别处改过，用这台设备的密钥按新密码重新加密了；fresh：密码改过且这台设备没有密钥，只能重新开始。
 */
export async function afterLogin(password) {
  const remote = await fetchRemote();
  if (!remote) { await rewrap(password, 0); return 'created'; }
  const salt = fromBase64(remote.salt);
  const wrapKey = await deriveWrapKey(password, salt, remote.iterations);
  let payload = null;
  try { payload = await unwrapKeyring(wrapKey, fromBase64(remote.blob)); } catch { /* 密码对不上：在别处重置过 */ }
  if (payload) {
    const local = { wrapKey, salt: remote.salt, iterations: remote.iterations, version: remote.version };
    await idb.set('kv', LOCAL(), local);
    await adopt(payload, local);
    return 'synced';
  }
  const hadKey = !!(await getMasterKey());
  await rewrap(password, remote.version);
  return hadKey ? 'rewrapped' : 'fresh';
}

/** 改了密码（这台设备上）：用新密码重新加密。 */
export async function passwordChanged(password) {
  const remote = await fetchRemote().catch(() => null);
  await rewrap(password, remote?.version ?? 0);
}

/** 这台设备改了订阅地址：加密上传。没有本地的钥匙圈密钥（这台设备登录早于这个功能）时什么也不做。 */
export async function pushKeyring() {
  const local = await idb.get('kv', LOCAL());
  if (!local?.wrapKey) return;
  let result = await upload(local, await localPayload(), local.version);
  if (result.conflict) {
    // 别的设备也改过：密钥以账号的为准，订阅地址以这次的修改为准
    try {
      const theirs = await unwrapKeyring(local.wrapKey, fromBase64(result.conflict.blob));
      await adoptKey(theirs.key);
    } catch { return; } // 密码在别处改过：下次登录时再同步
    result = await upload(local, await localPayload(), result.conflict.version);
  }
}

/** 打开页面时检查别的设备有没有改过（每 5 分钟最多一次）。 */
export async function syncKeyring() {
  const local = await idb.get('kv', LOCAL());
  if (!local?.wrapKey) return false;
  const checkedKey = userKey('keyringChecked');
  if (Date.now() - ((await idb.get('kv', checkedKey)) ?? 0) < 5 * 60 * 1000) return false;
  await idb.set('kv', checkedKey, Date.now());
  const remote = await fetchRemote().catch(() => null);
  if (!remote || remote.version === local.version || remote.salt !== local.salt) return false; // 盐变了说明密码改过，要重新登录
  const payload = await unwrapKeyring(local.wrapKey, fromBase64(remote.blob)).catch(() => null);
  if (!payload) return false;
  const next = { ...local, version: remote.version };
  await idb.set('kv', LOCAL(), next);
  await adopt(payload, next);
  return true;
}
