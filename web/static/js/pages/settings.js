// 设置页：订阅先加密保存到账号，收到确认后在本机生效。
import { h, toast } from '../core/api.js';
import * as idb from '../core/idb.js';
import { userKey } from '../core/api.js';
import * as sources from '../core/sources.js';

const $ = (id) => document.getElementById(id);
const form = $('sub-form'), input = $('sub-url'), status = $('sub-status'), table = $('src-table');
let saving = false;
function busy(value) {
  saving = value;
  input.disabled = value;
  for (const button of form.querySelectorAll('button')) button.disabled = value;
}
function failure(error) {
  status.textContent = error.message || '保存失败，请稍后重试。';
  status.classList.add('error');
  status.setAttribute('role', 'alert');
}
globalThis.addEventListener?.('moovie:subscription-change', () => { if (!saving) render().catch(failure); });

function timeAgo(at) {
  const minutes = Math.floor((Date.now() - at) / 60000);
  if (minutes < 1) return '刚刚';
  if (minutes < 60) return `${minutes} 分钟前`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)} 小时前`;
  return `${Math.floor(minutes / 1440)} 天前`;
}

// 源很多时只显示前 10 个，其余点"展开全部"再看（和详情页的播放源列表一样），设置页不会被拉得很长。
// 展开后重新读取订阅也保持展开。
const LIMIT = 10;
let expanded = false;
const toggle = $('src-more');
toggle?.addEventListener('click', () => {
  expanded = true;
  for (const row of table.children) row.hidden = false;
  toggle.hidden = true;
});

async function render() {
  const subscription = await sources.getSubscription();
  table.replaceChildren();
  if (toggle) toggle.hidden = true;
  if (!subscription) {
    status.textContent = '还没有设置。没有播放源时，想看、看过、片单等功能照常可用，只是不能播放。';
    return;
  }
  input.value = subscription.url;
  status.textContent = `共 ${subscription.sources.length} 个源，${timeAgo(subscription.fetchedAt)}读取。`;
  const health = (await idb.get('kv', userKey('health'))) ?? {};
  subscription.sources.forEach((source, index) => {
    const stats = health[source.key];
    let state = '还没用过';
    if (stats && stats.ok + stats.fail > 0) state = stats.ok ? `成功 ${stats.ok} 次，失败 ${stats.fail} 次，平均 ${stats.ms}ms` : `最近 ${stats.fail} 次都失败了`;
    const row = h('li', {}, h('b', { text: source.name }), h('span', { class: 'dim', text: new URL(source.api).hostname }),
      h('span', { class: 'state', dataset: { key: source.key }, text: state }));
    row.hidden = !expanded && index >= LIMIT;
    table.append(row);
  });
  // 跳过的源总是显示：订阅文件哪里写错了，要让人看得到
  for (const error of subscription.errors ?? []) table.append(h('li', { class: 'error' }, h('span', { text: '已跳过 · ' + error })));
  if (toggle && !expanded && subscription.sources.length > LIMIT) {
    toggle.textContent = `展开全部 ${subscription.sources.length} 个源`;
    toggle.hidden = false;
  }
}

// 保存后顺便测一下每个源能不能连上。
async function probe() {
  const list = await sources.getSources();
  await Promise.all(list.map(async (source) => {
    const cell = table.querySelector(`.state[data-key="${CSS.escape(source.key)}"]`);
    if (cell) cell.textContent = '正在测试…';
    try {
      const { ms } = await sources.querySource(source, '的');
      if (cell) { cell.textContent = `可用 · ${ms}ms`; cell.classList.add('ok'); }
    } catch (error) {
      const text = { timeout: '超时', network: '连不上', http: '接口返回了错误' }[error.code] ?? '出错';
      if (cell) { cell.textContent = text; cell.classList.add('error'); }
    }
  }));
}

form.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  if (saving) return;
  const url = input.value.trim();
  if (!url) { failure(new Error('请输入订阅地址；如需删除，请点击“清除”。')); return; }
  busy(true);
  status.classList.remove('error');
  status.setAttribute('role', 'status');
  status.textContent = '正在校验订阅并加密保存到账号…';
  try {
    const { saveSubscriptionToAccount } = await import('../core/keyring.js');
    await saveSubscriptionToAccount(url);
    await render();
    $('account-sync-error')?.remove();
    status.textContent += ' 已保存到账号。';
    toast('订阅已保存到账号');
    probe();
  } catch (error) {
    failure(error);
  } finally { busy(false); }
});
input.addEventListener('input', () => status.classList.remove('error'));

// 临时：示例订阅（见 internal/app/example_subscription.go）。填进输入框，照常保存并检查。
$('sub-example')?.addEventListener('click', (ev) => {
  input.value = new URL(ev.currentTarget.dataset.url, location.href).href;
  form.requestSubmit();
});

$('sub-refresh').addEventListener('click', async () => {
  status.textContent = '正在重新读取…';
  try {
    if (!(await sources.refreshSubscription({ force: true }))) { status.textContent = '还没有设置订阅地址。'; return; }
    await render();
    probe();
  } catch (error) {
    failure(error);
  }
});

$('sub-clear').addEventListener('click', async () => {
  if (saving) return;
  if (!confirm('清除订阅地址？登录这个账号的其他设备也会一起清除。')) return;
  busy(true);
  status.classList.remove('error');
  status.textContent = '正在从账号清除订阅…';
  try {
    const { clearSubscriptionFromAccount } = await import('../core/keyring.js');
    await clearSubscriptionFromAccount();
    $('account-sync-error')?.remove();
    input.value = '';
    await render();
    toast('账号订阅已清除');
  } catch (error) { failure(error); }
  finally { busy(false); }
});

render().catch(failure);

// ---------- 账号同步 ----------
// 先准备新密文，再与密码一起提交；服务器在一个事务里更新，成功后更新本机密钥。
document.querySelector('form[action="/settings/password"]')?.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const formEl = ev.currentTarget, button = formEl.querySelector('button');
  if (button.disabled) return;
  const password = formEl.querySelector('[name="password"]').value;
  const current = formEl.querySelector('[name="current"]').value;
  let notice = formEl.querySelector('[data-password-status]');
  if (!notice) { notice = h('p', { class: 'hint', role: 'status', 'data-password-status': '' }); formEl.append(notice); }
  notice.classList.remove('error');
  notice.setAttribute('role', 'status');
  notice.textContent = '正在准备加密数据…';
  button.disabled = true;
  try {
    const { preparePasswordChange, finishPasswordChange } = await import('../core/keyring.js');
    const prepared = await preparePasswordChange(password, current);
    const data = new FormData(formEl);
    data.set('keyring', JSON.stringify(prepared.wire));
    notice.textContent = '正在修改密码并保存账号配置…';
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);
    let response, result;
    try {
      response = await fetch(formEl.action, { method: 'POST', body: data, credentials: 'same-origin', headers: { Accept: 'application/json' }, signal: controller.signal });
      result = await response.json();
    } catch { throw new Error('未能确认修改结果，请检查网络，必要时使用新密码重新登录确认。'); }
    finally { clearTimeout(timer); }
    if (!response.ok) throw new Error(result.error || '密码修改失败，请稍后重试。');
    await finishPasswordChange(prepared, result.keyring_version);
    location.href = result.redirect;
  } catch (error) {
    notice.textContent = error.message || '修改失败，请稍后重试。';
    notice.classList.add('error');
    notice.setAttribute('role', 'alert');
  } finally { button.disabled = false; }
});

// ---------- 导出数据 ----------
// 服务器只有加密历史的密文；导出时由这台设备解密后附进文件。出错时退回服务器的原始文件（链接本身就能下载）。
document.querySelector('[data-export]')?.addEventListener('click', async (ev) => {
  ev.preventDefault();
  const link = ev.currentTarget;
  link.setAttribute('aria-busy', 'true');
  try {
    const { syncPrivate, listPrivate } = await import('../core/history.js');
    await syncPrivate().catch(() => {});
    const [response, records] = await Promise.all([fetch(link.href, { credentials: 'same-origin' }), listPrivate()]);
    if (!response.ok) throw new Error('export');
    const data = await response.json();
    data.private_history = records.map((r) => ({
      title: r.title, year: r.year || undefined, type: r.type, episode: r.ep, position_seconds: r.pos, duration_seconds: r.dur,
      completed: !!r.done, watched_at: new Date(r.at).toISOString(),
    }));
    const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }));
    const a = h('a', { href: url, download: `moovie-export-${new Date().toISOString().slice(0, 10).replaceAll('-', '')}.json` });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  } catch {
    location.href = link.href;
  } finally {
    link.removeAttribute('aria-busy');
  }
});

// 自定义头像：输入框里只留最后一个字符（一个 emoji 可能由多个码点组成，按字形切），并选中它。
const customAvatar = $('avatar-custom'), customRadio = $('avatar-custom-radio');
customAvatar?.addEventListener('input', () => {
  const parts = [...new Intl.Segmenter().segment(customAvatar.value.trim())];
  const last = parts.at(-1)?.segment ?? '';
  customAvatar.value = last;
  customRadio.value = last;
  customRadio.checked = !!last;
});
customAvatar?.addEventListener('focus', () => { if (customAvatar.value) customRadio.checked = true; });

// 装了浏览器插件（插件在本站页面上会写 <html data-moovie-ext>）但还没连接账号时，提示去插件里连接。
if (document.documentElement?.dataset?.moovieExt) {
  for (const hint of document.querySelectorAll('[data-ext-installed]')) hint.hidden = false;
}
