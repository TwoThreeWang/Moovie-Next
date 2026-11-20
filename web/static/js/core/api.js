// 和本站服务器通信的小工具，以及几个全站通用的界面函数。

export const csrfToken = () => document.querySelector('meta[name="csrf-token"]')?.content ?? '';

/** 当前登录用户的 ID（字符串）。未登录时为空字符串。本地数据都按用户分开存放。 */
export const userId = () => document.body?.dataset.user ?? '';

/** 本地存储用的 key：加上用户前缀，同一台设备上的不同账号互不影响。 */
export const userKey = (name) => `u${userId()}:${name}`;

/** 调用本站接口。json 非空时以 JSON 发送。 */
export function api(path, { method = 'GET', json, keepalive = false, headers = {}, signal, cache } = {}) {
  const init = { method, credentials: 'same-origin', keepalive, signal, cache, headers: { 'X-CSRF-Token': csrfToken(), ...headers } };
  if (json !== undefined) {
    init.body = JSON.stringify(json);
    init.headers['Content-Type'] = 'application/json';
  }
  return fetch(path, init);
}

let toastTimer;
/** 在页面底部显示一条短提示。 */
export function toast(message, duration = 2200) {
  const el = document.getElementById('toast');
  if (!el) return;
  el.textContent = message;
  el.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove('show'), duration);
}

/** 后台同步失败时保留可见提示，不用一闪而过的成功提示掩盖失败。 */
export function showSyncError(message) {
  let el = document.getElementById('account-sync-error');
  if (!el) {
    el = h('p', { id: 'account-sync-error', class: 'flash error', role: 'alert' });
    (document.querySelector('main') ?? document.body).prepend(el);
  }
  el.textContent = '账号同步失败：' + message;
}

/** 创建元素。文字一律走 textContent，资源站返回的内容不会被当成 HTML 解析。 */
export function h(tag, props = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === 'class') el.className = value;
    else if (key === 'text') el.textContent = value;
    else if (key === 'dataset') Object.assign(el.dataset, value);
    else if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
    else el.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child);
  }
  return el;
}

/** 图标（引用页面里的 SVG 符号）。 */
export function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'i');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#i-' + name);
  svg.append(use);
  return svg;
}

