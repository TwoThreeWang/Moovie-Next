// 设置页：订阅地址管理和设备配对。这些内容只保存在这台设备上。
import { h, toast } from '../core/api.js';
import * as idb from '../core/idb.js';
import { userKey } from '../core/api.js';
import * as sources from '../core/sources.js';

const $ = (id) => document.getElementById(id);
const form = $('sub-form'), input = $('sub-url'), status = $('sub-status'), table = $('src-table');

function timeAgo(at) {
  const minutes = Math.floor((Date.now() - at) / 60000);
  if (minutes < 1) return '刚刚';
  if (minutes < 60) return `${minutes} 分钟前`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)} 小时前`;
  return `${Math.floor(minutes / 1440)} 天前`;
}

async function render() {
  const subscription = await sources.getSubscription();
  table.replaceChildren();
  if (!subscription) {
    status.textContent = '还没有设置。没有播放源时，想看、看过、片单等功能照常可用，只是不能播放。';
    return;
  }
  input.value = subscription.url;
  status.textContent = `共 ${subscription.sources.length} 个源，${timeAgo(subscription.fetchedAt)}读取。`;
  const health = (await idb.get('kv', userKey('health'))) ?? {};
  for (const source of subscription.sources) {
    const stats = health[source.key];
    let state = '还没用过';
    if (stats && stats.ok + stats.fail > 0) state = stats.ok ? `成功 ${stats.ok} 次，失败 ${stats.fail} 次，平均 ${stats.ms}ms` : `最近 ${stats.fail} 次都失败了`;
    table.append(h('li', {}, h('b', { text: source.name }), h('span', { class: 'dim', text: new URL(source.api).hostname }),
      h('span', { class: 'state', dataset: { key: source.key }, text: state })));
  }
  for (const error of subscription.errors ?? []) table.append(h('li', { class: 'error' }, h('span', { text: '已跳过 · ' + error })));
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
  const url = input.value.trim();
  if (!url) return;
  status.textContent = '正在读取订阅…';
  try {
    await sources.setSubscription(url);
    await render();
    toast('订阅已保存，登录这个账号的其他设备会自动同步');
    sync();
    probe();
  } catch (error) {
    status.textContent = error.message;
    status.classList.add('error');
  }
});
input.addEventListener('input', () => status.classList.remove('error'));

// 临时：示例订阅（见 internal/app/example_subscription.go）。填进输入框，照常保存并检查。
$('sub-example')?.addEventListener('click', (ev) => {
  input.value = location.origin + ev.currentTarget.dataset.url;
  form.requestSubmit();
});

$('sub-refresh').addEventListener('click', async () => {
  status.textContent = '正在重新读取…';
  try {
    if (!(await sources.refreshSubscription({ force: true }))) { status.textContent = '还没有设置订阅地址。'; return; }
    await render();
    probe();
  } catch (error) {
    status.textContent = error.message;
  }
});

$('sub-clear').addEventListener('click', async () => {
  if (!confirm('清除订阅地址？登录这个账号的其他设备也会一起清除。')) return;
  await sources.clearSubscription();
  input.value = '';
  await render();
  toast('已清除');
  sync();
});

render();

// ---------- 账号同步 ----------
// 订阅地址改了就加密上传，登录同一个账号的其他设备会自动拿到（见 core/keyring.js）。
const sync = () => import('../core/keyring.js').then((m) => m.pushKeyring()).catch(() => {});

// 改密码：先让服务器改，成功后用新密码重新加密钥匙圈，再回到设置页。
document.querySelector('form[action="/settings/password"]')?.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const formEl = ev.currentTarget, password = formEl.querySelector('[name="password"]').value;
  let response;
  try {
    response = await fetch(formEl.action, { method: 'POST', body: new FormData(formEl), credentials: 'same-origin' });
  } catch { toast('网络不通，请稍后再试'); return; }
  if (response.url.includes('saved=')) {
    try { const { passwordChanged } = await import('../core/keyring.js'); await passwordChanged(password); } catch { /* 下次登录时会重新加密 */ }
  }
  location.href = response.url;
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
