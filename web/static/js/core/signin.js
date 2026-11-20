// 登录、重置密码页共用：显示表单错误；登录成功后趁密码还在内存里同步账号配置，再进入网站。

/** 在表单标题下显示一条错误。 */
export function showFormError(form, message) {
  let flash = form.querySelector('.flash.error');
  if (!flash) {
    flash = document.createElement('p');
    flash.className = 'flash error';
    flash.setAttribute('role', 'alert');
    form.querySelector('h1')?.after(flash);
  }
  flash.textContent = message;
}

/**
 * 同步账号配置后跳转。同步失败不影响已经完成的登录：错误留在页面上，
 * 提交按钮变成“进入网站”，由用户决定什么时候离开。
 */
export async function syncThenEnter({ form, button, password, redirect, failedPrefix }) {
  const target = redirect || '/';
  if (button) button.textContent = '正在同步播放源…';
  try {
    const { afterLogin, LOCAL_ONLY_MESSAGE } = await import('./keyring.js');
    if (await afterLogin(password) === 'local-only') throw new Error(LOCAL_ONLY_MESSAGE);
  } catch (error) {
    showFormError(form, failedPrefix + (error.message || '请稍后重试。'));
    form.querySelector('[name="password"]').value = '';
    if (button) {
      button.disabled = false;
      button.type = 'button';
      button.textContent = '进入网站';
      button.addEventListener('click', () => { location.href = target; }, { once: true });
    }
    return;
  }
  location.href = target;
}
