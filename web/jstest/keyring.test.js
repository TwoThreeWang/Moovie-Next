import test from 'node:test';
import assert from 'node:assert/strict';
import * as idb from '../static/js/core/idb.js';
import * as sources from '../static/js/core/sources.js';
import * as keyring from '../static/js/core/keyring.js';
import { deriveWrapKey, unwrapKeyring, wrapKeyring, fromBase64, toBase64 } from '../static/js/lib/vault.js';

const password = 'old-password-123';
const first = 'https://moovie.test/first.json', second = 'https://moovie.test/second.json';
let remote, putMode, getFailure, putCount, releasePut, putStarted;
globalThis.document = { body: { dataset: { user: '1' } }, querySelector: () => ({ content: 'test-csrf' }) };
globalThis.location = new URL('https://moovie.test/settings');

async function body() {
  return unwrapKeyring(await deriveWrapKey(password, fromBase64(remote.salt), remote.iterations), fromBase64(remote.blob));
}
async function freshLogin() { await keyring.afterLogin(password); }
async function resetLocal() { await idb.clearPrefix('u1:'); }

test.beforeEach(async () => {
  await resetLocal();
  delete globalThis.isSecureContext;
  remote = null; putMode = null; getFailure = false; putCount = 0; releasePut = null; putStarted = false;
  globalThis.fetch = async (path, options = {}) => {
    const target = new URL(path, location.href);
    if (target.pathname === '/api/keyring') {
      assert.equal(options.cache, 'no-store');
      if (options.method === 'PUT') {
        putCount++;
        const payload = JSON.parse(options.body);
        assert.equal(typeof payload.blob, 'string');
        assert.ok(!JSON.stringify(payload).includes('moovie.test'), '只发送密文，不能上传明文订阅');
        if (putMode === 'hold') { putStarted = true; await new Promise(resolve => { releasePut = resolve; }); }
        if (putMode === 'network') throw new Error('offline');
        if (typeof putMode === 'number') return Response.json({ error: 'failure' }, { status: putMode });
        if (putMode === 'conflicts') {
          remote = { ...remote, version: remote.version + 1 };
          return Response.json({ current: remote }, { status: 409 });
        }
        if (payload.base !== (remote?.version ?? 0)) return Response.json({ current: remote }, { status: 409 });
        remote = { salt: payload.salt, iterations: payload.iterations, blob: payload.blob, version: (remote?.version ?? 0) + 1 };
        return Response.json({ version: remote.version });
      }
      if (getFailure) return Response.json({ error: 'failure' }, { status: 500 });
      return remote ? Response.json(remote) : Response.json({ error: 'none' }, { status: 404 });
    }
    if (target.pathname === '/first.json' || target.pathname === '/second.json') return Response.json({ sources: [{ key: target.pathname, name: '测试源', api: 'https://source.example/api.php/provide/vod/' }] });
    throw new Error('Unexpected request: ' + target.pathname);
  };
});

test('保存等服务器确认后才在本机生效，另一客户端能用密码解密同步', async () => {
  await freshLogin();
  await keyring.saveSubscriptionToAccount(first);
  putMode = 'hold';
  const saving = keyring.saveSubscriptionToAccount(second);
  for (let i = 0; i < 200 && !putStarted; i++) await new Promise(resolve => setTimeout(resolve, 2));
  assert.equal(putStarted, true);
  assert.equal((await sources.getSubscription()).url, first, '服务器未确认时原配置保持有效');
  assert.equal((await body()).sub, first);
  releasePut(); await saving; putMode = null;
  assert.equal((await sources.getSubscription()).url, second);
  assert.equal((await body()).sub, second);
  await resetLocal();
  assert.equal(await keyring.afterLogin(password), 'synced');
  assert.equal((await sources.getSubscription()).url, second);
});

test('HTTP 加密不可用时明确失败，不改本机配置，不上传', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const before = putCount;
  globalThis.isSecureContext = false;
  await assert.rejects(keyring.saveSubscriptionToAccount(second), /HTTPS/);
  await assert.rejects(keyring.preparePasswordChange('new-password', password), /HTTPS/);
  assert.equal((await sources.getSubscription()).url, first);
  assert.equal(putCount, before);
});

test('缺少本机同步密钥时明确要求重新登录', async () => {
  await sources.setSubscription(first);
  await assert.rejects(keyring.saveSubscriptionToAccount(second), /重新登录/);
  assert.equal((await sources.getSubscription()).url, first);
  assert.equal(putCount, 0);
});

test('上传的认证、校验、限流、服务器和网络失败均保留原配置并返回可读错误', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  for (const [mode, message] of [[401,/登录已失效/],[403,/请求校验失败/],[429,/太频繁/],[500,/服务器/],['network',/连接失败/]]) {
    putMode = mode;
    await assert.rejects(keyring.saveSubscriptionToAccount(second), message);
    assert.equal((await sources.getSubscription()).url, first);
    assert.equal((await body()).sub, first);
  }
});

test('连续版本冲突不会误报保存成功，也不会无限重试', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const before = putCount;
  putMode = 'conflicts';
  await assert.rejects(keyring.saveSubscriptionToAccount(second), /本次未保存/);
  assert.equal(putCount - before, 3);
  assert.equal((await sources.getSubscription()).url, first);
});

test('清除也等待账号确认，远端空订阅能清除其他设备的旧配置', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const oldLocal = await idb.get('kv', 'u1:keyring');
  await keyring.clearSubscriptionFromAccount();
  assert.equal((await body()).sub, '');
  assert.equal(await sources.getSubscription(), undefined);
  // 模拟另一台尚未拉取清除结果的设备。
  await idb.set('kv', 'u1:keyring', oldLocal);
  await sources.setSubscription(first);
  assert.equal(await keyring.syncKeyring(), true);
  assert.equal(await sources.getSubscription(), undefined);
  assert.equal((await body()).sub, '', '不能把其他设备的旧订阅反向补回账号');
});

test('后台同步失败不提前记录已检查，立即重试可以恢复', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const local = await idb.get('kv', 'u1:keyring');
  const payload = await body(); payload.sub = second;
  remote = { ...remote, blob: toBase64(await wrapKeyring(local.wrapKey, payload)), version: remote.version + 1 };
  getFailure = true;
  await assert.rejects(keyring.syncKeyring(), /服务器/);
  assert.equal(await idb.get('kv', 'u1:keyringChecked'), undefined);
  assert.equal((await idb.get('kv', 'u1:keyring')).version, local.version);
  getFailure = false;
  assert.equal(await keyring.syncKeyring(), true);
  assert.equal((await sources.getSubscription()).url, second);
});

test('密码解不开云端数据时，新设备不覆盖原账号配置', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const before = remote.blob, beforeCount = putCount;
  await resetLocal();
  await assert.rejects(keyring.afterLogin('different-password'), /原设备/);
  assert.equal(remote.blob, before);
  assert.equal(putCount, beforeCount);
});

test('改密码先准备密文不上传，服务器确认后才切换本机密钥，新设备能用新密码同步', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const oldLocal = await idb.get('kv', 'u1:keyring'), oldRemote = { ...remote }, count = putCount;
  const prepared = await keyring.preparePasswordChange('new-password-456', password);
  assert.equal(putCount, count);
  assert.equal((await idb.get('kv', 'u1:keyring')).salt, oldLocal.salt);
  assert.equal(remote.blob, oldRemote.blob);
  const nextKey = await deriveWrapKey('new-password-456', fromBase64(prepared.wire.salt), prepared.wire.iterations);
  assert.equal((await unwrapKeyring(nextKey, fromBase64(prepared.wire.blob))).sub, first);
  // 模拟后端事务已同时提交新密码和新密文。
  remote = { ...prepared.wire, version: remote.version + 1 };
  await keyring.finishPasswordChange(prepared, remote.version);
  assert.equal((await idb.get('kv', 'u1:keyring')).salt, remote.salt);
  await resetLocal();
  await keyring.afterLogin('new-password-456');
  assert.equal((await sources.getSubscription()).url, first);
});

test('密码变化导致的保存冲突明确提示重新登录，不覆盖对方更新', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const prepared = await keyring.preparePasswordChange('new-password-456', password);
  remote = { ...prepared.wire, version: remote.version + 1 };
  await assert.rejects(keyring.saveSubscriptionToAccount(second), /密码已在其他设备修改/);
  assert.equal((await sources.getSubscription()).url, first);
});

test('旧版本只保存到本机的订阅不被远端未配置的空值抹掉，并要求显式保存', async () => {
  await freshLogin();
  await sources.setSubscription(first);
  assert.equal((await body()).sub, '');
  assert.equal(await keyring.afterLogin(password), 'local-only');
  assert.equal((await sources.getSubscription()).url, first);
  await assert.rejects(keyring.syncKeyring(), /尚未保存到账号/);
  await keyring.saveSubscriptionToAccount(first);
  assert.equal((await body()).sub, first);
  assert.equal((await idb.get('kv', 'u1:keyring')).subscriptionPending, false);
});

test('原设备保留旧包装密钥时能恢复密码重置后的配置，不丢弃订阅', async () => {
  await freshLogin(); await keyring.saveSubscriptionToAccount(first);
  const result = await keyring.afterLogin('reset-password-789');
  assert.equal(result, 'rewrapped');
  const nextKey = await deriveWrapKey('reset-password-789', fromBase64(remote.salt), remote.iterations);
  assert.equal((await unwrapKeyring(nextKey, fromBase64(remote.blob))).sub, first);
});
