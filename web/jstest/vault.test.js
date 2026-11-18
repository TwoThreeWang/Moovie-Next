import test from 'node:test';
import assert from 'node:assert/strict';
import {
  generateMasterKey, validMasterKey, deriveKeys, recordId, seal, open,
  toBase64, fromBase64, toBase64URL, fromBase64URL,
} from '../static/js/lib/vault.js';

const RECORD = { title: '某部没有元数据的片子', year: 2025, type: 'series', source: 'lz', vod_id: '123', ep: 5, pos: 1312.5, dur: 2700, at: 1762843200000 };

test('主密钥的生成和校验', () => {
  const key = generateMasterKey();
  assert.match(key, /^[A-Za-z0-9_-]{43}$/);
  assert.equal(validMasterKey(key), true);
  assert.notEqual(generateMasterKey(), key);
  for (const bad of ['', 'short', 'A'.repeat(42), '!'.repeat(43), null, undefined, 123]) assert.equal(validMasterKey(bad), false);
});

test('base64 编解码', () => {
  const bytes = Uint8Array.from({ length: 256 }, (_, i) => i);
  assert.deepEqual(fromBase64(toBase64(bytes)), bytes);
  for (const length of [0, 1, 2, 3, 31, 32, 33]) {
    const sample = crypto.getRandomValues(new Uint8Array(length));
    assert.deepEqual(fromBase64URL(toBase64URL(sample)), sample);
  }
});

test('加密后能解密回来，密文长度固定', async () => {
  const { encKey } = await deriveKeys(generateMasterKey());
  const sealed = await seal(encKey, RECORD);
  assert.equal(sealed.length, 13 + 480 + 16);
  assert.ok(sealed.length <= 1024, '不能超过服务器的上限');
  assert.deepEqual(await open(encKey, sealed), RECORD);
  // 片名长短不影响密文长度
  const short = await seal(encKey, { ...RECORD, title: '短' });
  assert.equal(short.length, sealed.length);
  // 同样的内容每次加密结果不同（随机 IV）
  assert.notDeepEqual(await seal(encKey, RECORD), sealed);
  // 密文里看不到片名
  assert.equal(Buffer.from(sealed).includes(Buffer.from('没有元数据')), false);
  // 内容较长时用下一档长度，仍在上限内
  const long = await seal(encKey, { ...RECORD, title: '长'.repeat(180) });
  assert.equal(long.length, 13 + 960 + 16);
  await assert.rejects(seal(encKey, { ...RECORD, title: '长'.repeat(400) }), /太大/);
});

test('密钥不对或数据被改过时解密失败', async () => {
  const a = await deriveKeys(generateMasterKey()), b = await deriveKeys(generateMasterKey());
  const sealed = await seal(a.encKey, RECORD);
  await assert.rejects(open(b.encKey, sealed));
  const tampered = sealed.slice(); tampered[40] ^= 1;
  await assert.rejects(open(a.encKey, tampered));
  await assert.rejects(open(a.encKey, new Uint8Array(10)));
});

test('记录 ID：同一把密钥下稳定，换密钥就不同', async () => {
  const master = generateMasterKey();
  const first = await deriveKeys(master), again = await deriveKeys(master), other = await deriveKeys(generateMasterKey());
  const id = await recordId(first.ridKey, RECORD);
  assert.match(id, /^[0-9a-f]{64}$/);
  assert.equal(await recordId(again.ridKey, RECORD), id, '另一台设备用同一把密钥算出同一个 ID');
  assert.equal(await recordId(first.ridKey, { title: '某部没有元数据的 片子！', year: 2025, type: 'series' }), id, '片名写法不同但规范化后相同');
  assert.notEqual(await recordId(first.ridKey, { ...RECORD, year: 2024 }), id);
  assert.notEqual(await recordId(other.ridKey, RECORD), id, '没有密钥就算不出 ID，服务器无法用片名库反推');
});

test('钥匙圈：同一个密码能解开，换了密码解不开', async () => {
  const { deriveWrapKey, wrapKeyring, unwrapKeyring, generateMasterKey } = await import('../static/js/lib/vault.js');
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const key = generateMasterKey();
  const sealed = await wrapKeyring(await deriveWrapKey('correct horse', salt, 100000), { v: 1, key, sub: 'https://example.com/sub.json' });
  assert.ok(!new TextDecoder().decode(sealed).includes('example.com'), '密文里不能出现订阅地址');
  const opened = await unwrapKeyring(await deriveWrapKey('correct horse', salt, 100000), sealed);
  assert.deepEqual(opened, { v: 1, key, sub: 'https://example.com/sub.json' });
  await assert.rejects(unwrapKeyring(await deriveWrapKey('wrong password', salt, 100000), sealed));
  const otherSalt = crypto.getRandomValues(new Uint8Array(16));
  await assert.rejects(unwrapKeyring(await deriveWrapKey('correct horse', otherSalt, 100000), sealed), '盐不同也解不开');
});
