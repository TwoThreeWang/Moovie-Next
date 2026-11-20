// 登录、注册页：页面一打开就在后台做人机校验，用户填表的时候通常已经算完了。
// 提交时如果还没算完，就等它算完再提交；服务器要求打开页面 2 秒后才能提交，这里也拦一下。
import { parseToken, search } from '../lib/pow.js';
import { showFormError, syncThenEnter } from '../core/signin.js';

const form = document.querySelector('form[data-pow]');
const tokenInput = form?.querySelector('[name="pow_token"]');
const nonceInput = form?.querySelector('[name="pow_nonce"]');

function solve(token) {
  return new Promise((resolve) => {
    try {
      const worker = new Worker(new URL('./pow-worker.js', import.meta.url), { type: 'module' });
      worker.addEventListener('message', (event) => { worker.terminate(); resolve(event.data?.nonce ?? null); });
      worker.addEventListener('error', () => { worker.terminate(); resolve(fallback(token)); });
      worker.postMessage({ token });
    } catch {
      resolve(fallback(token)); // 不支持模块 Worker 的老浏览器：在主线程算
    }
  });
}

function fallback(token) {
  const parsed = parseToken(token);
  return parsed ? search(parsed.challenge, parsed.bits) : null;
}

const showError = (message) => showFormError(form, message);

/**
 * 登录：用脚本提交，成功后趁密码还在内存里同步钥匙圈（设备密钥和订阅地址），再跳转。
 * 这样换设备登录同一个账号，播放源和观看记录就都在，不用扫码配对。
 */
async function login(button, label) {
  const password = form.querySelector('[name="password"]').value;
  let response;
  try {
    response = await fetch(form.action, { method: 'POST', body: new FormData(form), credentials: 'same-origin', headers: { Accept: 'application/json' } });
  } catch {
    showError('网络不通，请稍后再试');
    return false;
  }
  const result = await response.json().catch(() => ({ error: '登录失败，请重试' }));
  if (!response.ok) {
    showError(result.error || '登录失败，请重试');
    if (result.pow) { tokenInput.value = result.pow; nonceInput.value = ''; answer = start(result.pow); openedAt = performance.now(); }
    return false;
  }
  document.body.dataset.user = String(result.user_id);
  await syncThenEnter({ form, button, password, redirect: result.redirect, failedPrefix: '已登录，但账号同步未完成：' });
  return true;
}

let answer, openedAt = performance.now();
const start = (token) => solve(token).then((nonce) => { if (nonce) nonceInput.value = nonce; return nonce; });

if (form && tokenInput && nonceInput) {
  answer = start(tokenInput.value);
  let submitting = false;
  form.addEventListener('submit', async (event) => {
    if (submitting) return;
    const isLogin = form.hasAttribute('data-login');
    if (!isLogin && nonceInput.value && performance.now() - openedAt > 2100) return;
    event.preventDefault();
    const button = form.querySelector('button:not([type="button"])');
    const label = button?.textContent;
    if (button) { button.disabled = true; button.textContent = '正在进行安全校验…'; }
    await answer;
    const wait = 2100 - (performance.now() - openedAt);
    if (wait > 0) await new Promise((resolve) => setTimeout(resolve, wait));
    if (isLogin) {
      if (button) button.textContent = '正在登录…';
      if (!(await login(button, label)) && button) { button.disabled = false; button.textContent = label; }
      return;
    }
    if (button) { button.disabled = false; button.textContent = label; }
    submitting = true;
    form.requestSubmit();
  });
}
