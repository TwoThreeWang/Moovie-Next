// 主密钥的本地保存。密钥只存在这台设备上，永远不发给服务器。
import * as idb from './idb.js';
import { userKey } from './api.js';
import { generateMasterKey, validMasterKey, deriveKeys } from '../lib/vault.js';

/** 读取主密钥；没有时返回空字符串。 */
export async function getMasterKey() {
  const key = await idb.get('kv', userKey('masterKey'));
  return validMasterKey(key) ? key : '';
}

/** 保存主密钥（登录时从账号的钥匙圈里解出来的）。 */
export async function setMasterKey(key) {
  if (!validMasterKey(key)) throw new Error('密钥格式不正确');
  await idb.set('kv', userKey('masterKey'), key);
  derived = null;
}

/** 读取主密钥；没有就新建一把。 */
export async function ensureMasterKey() {
  let key = await getMasterKey();
  if (!key) {
    key = generateMasterKey();
    await idb.set('kv', userKey('masterKey'), key);
  }
  return key;
}

let derived = null;
/** 返回派生出的加密密钥和记录 ID 密钥。 */
export async function keys() {
  derived ??= deriveKeys(await ensureMasterKey());
  return derived;
}
