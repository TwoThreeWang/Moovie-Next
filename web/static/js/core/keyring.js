// 账号同步：候选订阅先加密并等待服务器确认，再在本机生效。
import * as idb from './idb.js';
import { api, userKey } from './api.js';
import { getMasterKey, setMasterKey, ensureMasterKey } from './keys.js';
import * as sources from './sources.js';
import { requireCrypto, deriveWrapKey, wrapKeyring, unwrapKeyring, toBase64, fromBase64, KEYRING_ITERATIONS } from '../lib/vault.js';

const LOCAL = () => userKey('keyring');

/** 本机订阅还只在这台设备上（旧版本保存的），需要手动保存到账号。 */
export const LOCAL_ONLY_MESSAGE = '本机订阅尚未保存到账号，原配置已保留。请在设置页点击“保存并检查”。';
let pending = Promise.resolve();
const serial = (work) => {
  const result = pending.then(work);
  pending = result.catch(() => {});
  return result;
};

async function request(options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 10000);
  try {
    const response = await api('/api/keyring', { ...options, signal: controller.signal, cache: 'no-store' });
    const data = await response.json().catch(() => null);
    if (controller.signal.aborted) throw new Error('timeout');
    return { ok: response.ok, status: response.status, json: async () => {
      if (!data) throw new Error('账号同步响应异常，请稍后重试。');
      return data;
    } };
  } catch {
    throw new Error(controller.signal.aborted ? '账号同步请求超时，未能确认结果，请重试。' : '账号同步连接失败，请检查网络后重试。');
  } finally { clearTimeout(timer); }
}

async function remoteError(response) {
  const messages = {
    401: '登录已失效，请重新登录后再保存。',
    403: '请求校验失败，请刷新页面后重试。',
    429: '保存太频繁，请稍后重试。',
  };
  const message = messages[response.status] ?? (response.status >= 500 ? '服务器暂时无法保存账号配置，请稍后重试。' : '账号配置保存失败，请刷新页面后重试。');
  throw new Error(message);
}

async function fetchRemote() {
  const response = await request();
  if (response.status === 404) return null;
  if (!response.ok) return remoteError(response);
  return response.json();
}

// 这里只确认服务器写入；本地版本在配置全部落地后才更新。
async function upload(local, payload, base) {
  const blob = await wrapKeyring(local.wrapKey, payload);
  const response = await request({ method: 'PUT', json: { salt: local.salt, iterations: local.iterations, blob: toBase64(blob), base } });
  if (response.status === 409) {
    const current = (await response.json()).current;
    if (!current) throw new Error('账号配置发生变化，请重新登录后重试。');
    return { conflict: current };
  }
  if (!response.ok) return remoteError(response);
  const { version } = await response.json();
  if (!Number.isSafeInteger(version) || version <= base) throw new Error('服务器没有返回有效的保存确认，请重试。');
  return { version };
}

async function localPayload() {
  return { v: 1, key: await ensureMasterKey(), sub: (await sources.getSubscription())?.url ?? '' };
}

// 临时：从旧版迁移过来的账号第一次登录时，自动填入示例订阅（见 internal/app/example_subscription.go）。
// 查询失败就当没有，不影响登录。
async function pendingExampleSubscription() {
  try {
    const response = await api('/api/example-subscription', { cache: 'no-store' });
    if (!response.ok) return '';
    const { url } = await response.json();
    if (typeof url !== 'string') return '';
    if (url.startsWith('https://')) return url;
    return url.startsWith('/') ? location.origin + url : '';
  } catch { return ''; }
}

async function newWrapping(password) {
  requireCrypto();
  const salt = crypto.getRandomValues(new Uint8Array(16));
  return { wrapKey: await deriveWrapKey(password, salt, KEYRING_ITERATIONS), salt: toBase64(salt), iterations: KEYRING_ITERATIONS };
}

async function adoptKey(key) {
  const current = await getMasterKey();
  if (!key || key === current) return;
  if (!current) { await setMasterKey(key); return; }
  const history = await import('./history.js');
  await history.syncPrivate().catch(() => {});
  const records = await history.listPrivate();
  await setMasterKey(key);
  for (const { rid, ...data } of records) {
    const next = await history.privateId({ title: data.title, year: data.year, type: data.type });
    if (next === rid) continue;
    await history.savePrivate(next, data).catch(() => {});
    await history.savePrivate(rid, null).catch(() => {});
  }
}

async function adoptSubscription(url, explicit) {
  const current = (await sources.getSubscription())?.url ?? '';
  if (url === current) return;
  // 旧版本可能只保存过本地订阅；远端从未配置的空值不能抹掉这份数据。
  if (!url && !explicit && current) return true;
  if (!url) { await sources.clearSubscription(); return; }
  try {
    await sources.setSubscription(url);
  } catch {
    // 账号地址已经同步；源站暂时打不开时也保存地址，并在设置页显示读取失败。
    await sources.saveSubscription({ url, sources: [], errors: ['订阅地址已同步，但暂时读取失败，请点击“重新读取”。'], fetchedAt: 0 });
  }
}

async function apply(payload, local, candidate) {
  await adoptKey(payload.key);
  let keptLocal = false;
  if (candidate === null) await sources.clearSubscription();
  else if (candidate) await sources.saveSubscription(candidate);
  else keptLocal = !!(await adoptSubscription(payload.sub ?? '', payload.subSet));
  await idb.set('kv', LOCAL(), { ...local, subscriptionPending: keptLocal });
  return keptLocal;
}

async function applySaved(payload, local, candidate) {
  try { return await apply(payload, local, candidate); }
  catch {
    throw new Error('账号已保存，但本机保存失败。请刷新页面重试，或重新登录恢复账号配置。');
  }
}

/** 登录成功后解密并落地账号配置，任何失败都交给界面明确提示。 */
export const afterLogin = (password) => serial(async () => {
  requireCrypto();
  let remote = await fetchRemote();
  if (!remote) {
    const local = await newWrapping(password), payload = await localPayload();
    if (!payload.sub) payload.sub = await pendingExampleSubscription();
    const result = await upload(local, payload, 0);
    if (!result.conflict) {
      await applySaved(payload, { ...local, version: result.version });
      return 'created';
    }
    remote = result.conflict;
  }
  const wrapKey = await deriveWrapKey(password, fromBase64(remote.salt), remote.iterations);
  let payload;
  try { payload = await unwrapKeyring(wrapKey, fromBase64(remote.blob)); }
  catch {
    // 密码重置后，只允许持有旧密钥的原设备恢复，不能用新设备的空配置覆盖账号数据。
    const cached = await idb.get('kv', LOCAL());
    if (!cached?.wrapKey || cached.salt !== remote.salt) throw new Error('当前密码无法解开账号同步数据。请在原设备用新密码登录恢复，账号原有数据未被覆盖。');
    try { payload = await unwrapKeyring(cached.wrapKey, fromBase64(remote.blob)); }
    catch { throw new Error('账号同步数据解密失败，请保留原设备的数据并稍后重试。'); }
    const local = await newWrapping(password);
    const result = await upload(local, payload, remote.version);
    if (result.conflict) throw new Error('恢复期间账号配置发生变化，请重新登录后重试。');
    const keptLocal = await applySaved(payload, { ...local, version: result.version });
    return keptLocal ? 'local-only' : 'rewrapped';
  }
  const keptLocal = await apply(payload, { wrapKey, salt: remote.salt, iterations: remote.iterations, version: remote.version });
  return keptLocal ? 'local-only' : 'synced';
});

async function saveCandidate(candidate) {
  requireCrypto();
  let local = await idb.get('kv', LOCAL());
  if (!local?.wrapKey) throw new Error('此设备尚未完成账号同步，请退出后重新登录，再保存订阅。');
  const payload = await localPayload();
  payload.sub = candidate?.url ?? '';
  payload.subSet = true;
  for (let attempt = 0; attempt < 3; attempt++) {
    const result = await upload(local, payload, local.version);
    if (!result.conflict) {
      await applySaved(payload, { ...local, version: result.version }, candidate);
      return;
    }
    const current = result.conflict;
    if (current.salt !== local.salt) throw new Error('密码已在其他设备修改，请重新登录后再保存。');
    let theirs;
    try { theirs = await unwrapKeyring(local.wrapKey, fromBase64(current.blob)); }
    catch { throw new Error('账号同步密钥已变化，请重新登录后再保存。'); }
    payload.key = theirs.key;
    local = { ...local, version: current.version };
  }
  throw new Error('其他设备正在更新账号配置，本次未保存，请稍后重试。');
}

/** 校验候选订阅 → 加密上传 → 服务端确认 → 本机生效。失败保留原配置。 */
export const saveSubscriptionToAccount = (url) => serial(async () => {
  requireCrypto();
  const candidate = await sources.readSubscription(url);
  await saveCandidate(candidate);
  return candidate;
});

export const clearSubscriptionFromAccount = () => serial(() => saveCandidate(null));

/** 在服务器改密码之前准备新的密文；提交时与密码一起在事务内更新。 */
export const preparePasswordChange = (password, currentPassword) => serial(async () => {
  requireCrypto();
  const remote = await fetchRemote();
  let payload;
  if (remote) {
    try {
      const currentKey = await deriveWrapKey(currentPassword, fromBase64(remote.salt), remote.iterations);
      payload = await unwrapKeyring(currentKey, fromBase64(remote.blob));
    } catch { throw new Error('当前密码无法解开账号同步数据，密码尚未修改。请确认密码，或在原设备恢复后重试。'); }
  } else payload = await localPayload();
  const local = await newWrapping(password);
  const blob = await wrapKeyring(local.wrapKey, payload);
  return { local, payload, wire: { salt: local.salt, iterations: local.iterations, blob: toBase64(blob), base: remote?.version ?? 0 } };
});

export const finishPasswordChange = (prepared, version) => serial(async () => {
  if (!Number.isSafeInteger(version) || version <= prepared.wire.base) throw new Error('密码修改结果异常，请使用新密码重新登录确认。');
  try { await apply(prepared.payload, { ...prepared.local, version }); }
  catch { throw new Error('密码和账号配置已修改，但本机保存失败。请使用新密码重新登录恢复。'); }
});

/** 仅在配置落地成功后标记检查时间和版本；失败可以立即重试。 */
export const syncKeyring = () => serial(async () => {
  const local = await idb.get('kv', LOCAL());
  if (!local?.wrapKey) return false;
  requireCrypto();
  const checkedKey = userKey('keyringChecked');
  if (Date.now() - ((await idb.get('kv', checkedKey)) ?? 0) < 5 * 60 * 1000) return false;
  const remote = await fetchRemote();
  if (!remote) throw new Error('账号同步数据暂时不可用，请重新登录后重试。');
  if (remote.salt !== local.salt) throw new Error('密码已在其他设备修改，请重新登录以恢复账号同步。');
  if (remote.version !== local.version) {
    let payload;
    try { payload = await unwrapKeyring(local.wrapKey, fromBase64(remote.blob)); }
    catch { throw new Error('账号同步数据解密失败，请重新登录后重试。'); }
    if (await apply(payload, { ...local, version: remote.version })) throw new Error(LOCAL_ONLY_MESSAGE);
  }
  else if (local.subscriptionPending) throw new Error(LOCAL_ONLY_MESSAGE);
  await idb.set('kv', checkedKey, Date.now());
  return remote.version !== local.version;
});
