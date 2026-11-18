// 设置新密码：用脚本提交，成功后用新密码重新加密钥匙圈（设备密钥和订阅地址），再进首页。
const form = document.querySelector('form[action="/auth/reset"]');

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
    form.submit(); // 退回普通提交
    return;
  }
  if (!response.ok) {
    let flash = form.querySelector('.flash.error');
    if (!flash) { flash = document.createElement('p'); flash.className = 'flash error'; flash.setAttribute('role', 'alert'); form.querySelector('h1').after(flash); }
    flash.textContent = result.error || '设置失败，请重试';
    button.disabled = false;
    return;
  }
  document.body.dataset.user = String(result.user_id);
  button.textContent = '正在同步播放源…';
  try {
    const { afterLogin } = await import('../core/keyring.js');
    const outcome = await Promise.race([afterLogin(password), new Promise((resolve) => setTimeout(() => resolve('timeout'), 12000))]);
    if (outcome === 'fresh') sessionStorage.setItem('mv:keyringFresh', '1');
  } catch { /* 下次登录时再同步 */ }
  location.href = result.redirect || '/';
});
