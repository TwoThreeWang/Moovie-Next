import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { sha256, leadingZeroBits, solved, search, parseToken } from '../static/js/lib/pow.js';

const words = (buffer) => Array.from({ length: 8 }, (_, i) => buffer.readUInt32BE(i * 4));

test('SHA-256 和标准实现一致', () => {
  const lengths = [0, 1, 3, 55, 56, 57, 63, 64, 65, 119, 120, 200, 1000];
  for (const length of lengths) {
    const input = randomBytes(length);
    assert.deepEqual(sha256(new Uint8Array(input)), words(createHash('sha256').update(input).digest()), `长度 ${length}`);
  }
  assert.deepEqual(sha256(new TextEncoder().encode('注册:挑战')), words(createHash('sha256').update('注册:挑战').digest()));
});

test('开头的 0 比特数', () => {
  assert.equal(leadingZeroBits([0x80000000]), 0);
  assert.equal(leadingZeroBits([0x00010000]), 15);
  assert.equal(leadingZeroBits([0, 0x0fffffff]), 36);
});

test('求解的答案能通过标准 SHA-256 的校验', () => {
  const challenge = 'register.1700000000000.12.00112233445566778899aabb';
  const nonce = search(challenge, 12);
  assert.match(nonce, /^\d+$/);
  assert.equal(solved(challenge, nonce, 12), true);
  const digest = createHash('sha256').update(challenge + ':' + nonce).digest();
  assert.equal(digest[0], 0);
  assert.equal(digest[1] >> 4, 0);
  // 比它小的数都不是答案（逐个递增，找到的是第一个）
  for (let n = 0; n < Number(nonce); n += Math.max(1, Math.floor(Number(nonce) / 50))) {
    assert.equal(solved(challenge, String(n), 12), false);
  }
  // 和服务器端测试里的同一组数据
  assert.equal(nonce, '451');
  assert.equal(search(challenge, 12, 0, 100), null, '限定次数内找不到时返回 null');
});

test('从挑战里读出难度', () => {
  assert.deepEqual(parseToken('login.1700000000000.20.abcd.sig'), { challenge: 'login.1700000000000.20.abcd', bits: 20 });
  assert.equal(parseToken('bad'), null);
  assert.equal(parseToken('a.b.99.c.d'), null);
  assert.equal(parseToken(undefined), null);
});
