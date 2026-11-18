import test from 'node:test';
import assert from 'node:assert/strict';
import * as idb from '../static/js/core/idb.js';

// Node 里没有 IndexedDB，走内存退路；逻辑相同。
test('注销后只清掉这个账号在本机的数据', async () => {
  await idb.set('kv', 'u1:subscription', 'a');
  await idb.set('private', 'u1:p:x', { rid: 'x' });
  await idb.set('kv', 'u12:subscription', 'b');
  await idb.set('kv', 'u2:subscription', 'c');
  await idb.clearPrefix('u1:');
  assert.equal(await idb.get('kv', 'u1:subscription'), undefined);
  assert.equal(await idb.get('private', 'u1:p:x'), undefined);
  assert.equal(await idb.get('kv', 'u12:subscription'), 'b', '前缀要带冒号，不能误删 u12');
  assert.equal(await idb.get('kv', 'u2:subscription'), 'c');
});
