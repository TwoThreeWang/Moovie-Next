// 端到端加密：没有元数据的作品，观看历史在浏览器里加密后才上传。
// 服务器只看到一个不透明的记录 ID 和一段定长的密文。
import { normalizeTitle } from './title.js';

const encoder = new TextEncoder(), decoder = new TextDecoder();

export function toBase64(bytes) {
  let binary = '';
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary);
}

export function fromBase64(text) {
  const binary = atob(text);
  return Uint8Array.from(binary, (c) => c.charCodeAt(0));
}

export const toBase64URL = (bytes) => toBase64(bytes).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
export const fromBase64URL = (text) => fromBase64(text.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((text.length + 3) % 4));
export const toHex = (bytes) => Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');

/** 生成一个新的主密钥（32 个随机字节，base64url 文本）。 */
export function generateMasterKey() {
  return toBase64URL(crypto.getRandomValues(new Uint8Array(32)));
}

/** 检查文本是不是合法的主密钥。 */
export function validMasterKey(text) {
  try { return typeof text === 'string' && fromBase64URL(text).length === 32; } catch { return false; }
}

/**
 * 从主密钥派生出两把用途分开的密钥：
 * encKey 用来加密内容；ridKey 用来计算记录 ID。
 */
export async function deriveKeys(masterKey) {
  const raw = await crypto.subtle.importKey('raw', fromBase64URL(masterKey), 'HKDF', false, ['deriveKey']);
  const derive = (info, algorithm, usages) => crypto.subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(32), info: encoder.encode(info) }, raw, algorithm, false, usages);
  return {
    encKey: await derive('moovie/history/enc/v1', { name: 'AES-GCM', length: 256 }, ['encrypt', 'decrypt']),
    ridKey: await derive('moovie/history/rid/v1', { name: 'HMAC', hash: 'SHA-256', length: 256 }, ['sign']),
  };
}

/**
 * 计算记录 ID：HMAC(密钥, 规范化片名|年份|类型)。
 * 两台设备对同一部作品算出的 ID 相同；没有密钥的人（包括服务器）无法从 ID 反推片名。
 */
export async function recordId(ridKey, { title, year, type }) {
  const text = `${normalizeTitle(title)}|${year || 0}|${type || ''}`;
  return toHex(new Uint8Array(await crypto.subtle.sign('HMAC', ridKey, encoder.encode(text))));
}

const SIZES = [480, 960]; // 明文补齐到的长度，让密文长度不反映片名长短

/** 加密一条记录，返回 Uint8Array：版本(1) | IV(12) | 密文。 */
export async function seal(encKey, record) {
  const body = encoder.encode(JSON.stringify(record));
  const size = SIZES.find((s) => body.length + 2 <= s);
  if (!size) throw new Error('记录太大');
  const plain = new Uint8Array(size);
  plain[0] = body.length >> 8; plain[1] = body.length & 0xff;
  plain.set(body, 2);
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const cipher = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, encKey, plain));
  const out = new Uint8Array(13 + cipher.length);
  out[0] = 1; out.set(iv, 1); out.set(cipher, 13);
  return out;
}

/** 解密一条记录。密钥不对或数据损坏时抛错。 */
export async function open(encKey, bytes) {
  if (bytes[0] !== 1 || bytes.length < 13 + 16) throw new Error('不认识的记录格式');
  const plain = new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: bytes.subarray(1, 13) }, encKey, bytes.subarray(13)));
  const length = (plain[0] << 8) | plain[1];
  return JSON.parse(decoder.decode(plain.subarray(2, 2 + length)));
}

// ---------- 钥匙圈：用登录密码保护设备主密钥和订阅地址 ----------
// 服务器只拿到密文和随机盐。PBKDF2 的次数取 OWASP 建议的量级，手机上派生一次约半秒，只在登录、改密码时做。

export const KEYRING_ITERATIONS = 310000;

/** 提前检查加密能力，避免 HTTP 局域网环境在保存后才静默失败。 */
export function requireCrypto() {
  if (globalThis.isSecureContext === false || !globalThis.crypto?.subtle) {
    throw new Error('当前地址无法使用浏览器加密，不能保存到账号或同步。请使用 HTTPS；本机开发可使用 localhost。');
  }
}

/** 从登录密码派生出加密钥匙圈用的密钥（不可导出）。 */
export async function deriveWrapKey(password, salt, iterations = KEYRING_ITERATIONS) {
  requireCrypto();
  const base = await crypto.subtle.importKey('raw', encoder.encode(password), 'PBKDF2', false, ['deriveKey']);
  return crypto.subtle.deriveKey({ name: 'PBKDF2', hash: 'SHA-256', salt, iterations }, base,
    { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
}

/** 加密钥匙圈内容，返回 Uint8Array：IV(12) | 密文。 */
export async function wrapKeyring(wrapKey, payload) {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const sealed = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, wrapKey, encoder.encode(JSON.stringify(payload))));
  const out = new Uint8Array(12 + sealed.length);
  out.set(iv);
  out.set(sealed, 12);
  return out;
}

/** 解密钥匙圈。密码不对（密钥不对）时抛错。 */
export async function unwrapKeyring(wrapKey, bytes) {
  const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: bytes.slice(0, 12) }, wrapKey, bytes.slice(12));
  const payload = JSON.parse(new TextDecoder().decode(plain));
  if (!payload || typeof payload !== 'object' || (payload.key && !validMasterKey(payload.key))) throw new Error('钥匙圈内容不正确');
  return payload;
}
