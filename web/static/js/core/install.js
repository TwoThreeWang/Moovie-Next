// 安卓 Chrome 等浏览器的"一键安装"。浏览器认为网站可以安装时会发 beforeinstallprompt 事件，
// 先把它存起来，等用户点按钮时再弹出系统的安装框。
let deferred = null;
const listeners = new Set();

addEventListener('beforeinstallprompt', (event) => {
  event.preventDefault(); // 不让浏览器自己弹小横幅，由我们在合适的时候提示
  deferred = event;
  for (const listener of listeners) listener(true);
});
addEventListener('appinstalled', () => {
  deferred = null;
  for (const listener of listeners) listener(false);
  try { localStorage.setItem('mv:installed', '1'); } catch { /* 记不住就算了 */ }
});

/** 现在能不能弹出系统的安装框。 */
export const canPrompt = () => deferred !== null;

/** 能弹安装框的状态变化时通知（比如事件来得比页面脚本晚）。 */
export function onPromptChange(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** 弹出安装框。返回 true 表示用户点了安装。 */
export async function prompt() {
  if (!deferred) return false;
  const event = deferred;
  deferred = null;
  event.prompt();
  const { outcome } = await event.userChoice;
  for (const listener of listeners) listener(false);
  return outcome === 'accepted';
}
