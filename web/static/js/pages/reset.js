// 设置新密码：用脚本提交，成功后用新密码重新加密钥匙圈（设备密钥和订阅地址），再进首页。
import { showFormError, syncThenEnter } from '../core/signin.js';

const form = document.querySelector('form[action="/auth/reset"]');

const showError = (message) => showFormError(form, message);

form?.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const button = form.querySelector('button');
  const password = form.querySelector('[name="password"]').value;
  button.disabled = true;
  let response, result;
  try {
    response = await fetch(form.action, { method: 'POST', body: new FormData(form), credentials: 'same-origin', headers: { Accept: 'application/json' } });
    result = await response.json();
  } catch {
    button.disabled = false;
    showError('未能确认密码设置结果，请检查网络，必要时尝试使用新密码登录。');
    return;
  }
  if (!response.ok) {
    showError(result.error || '设置失败，请重试');
    button.disabled = false;
    return;
  }
  document.body.dataset.user = String(result.user_id);
  await syncThenEnter({ form, button, password, redirect: result.redirect, failedPrefix: '密码已重置并登录，但账号同步未完成：' });
});
