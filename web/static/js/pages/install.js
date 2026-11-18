// 教程页：按设备显示对应的步骤，安卓上能一键安装时显示按钮。
import { detectPlatform, isStandalone } from '../lib/platform.js';
import * as install from '../core/install.js';

const info = detectPlatform(navigator.userAgent, { maxTouchPoints: navigator.maxTouchPoints, platform: navigator.platform });
const tabs = document.querySelectorAll('[data-guide-tab]'), guides = document.querySelectorAll('[data-guide]');

function show(name) {
  for (const tab of tabs) tab.setAttribute('aria-current', String(tab.dataset.guideTab === name));
  for (const guide of guides) guide.hidden = guide.dataset.guide !== name;
}
for (const tab of tabs) tab.addEventListener('click', (ev) => { ev.preventDefault(); show(tab.dataset.guideTab); });

let current = info.inApp ? 'inapp' : info.os === 'ios' ? 'ios' : info.os === 'android' ? 'android' : 'desktop';
show(current);

const detected = document.getElementById('detected');
if (detected) {
  const device = { ios: 'iPhone / iPad', android: '安卓手机', desktop: '电脑' }[info.os];
  detected.textContent = info.inApp
    ? `你现在是在${info.appName}里打开的本站，需要先换到浏览器里。`
    : `检测到你在用 ${device}${info.appName ? ` 上的 ${info.appName}` : ''}，下面是对应的步骤。`;
}
if (info.os === 'ios' && !info.inApp && info.browser !== 'safari') {
  document.getElementById('ios-other')?.removeAttribute('hidden');
}

// 安卓 Chrome、电脑 Chrome / Edge：能一键安装就显示按钮
const buttons = document.querySelectorAll('[data-install]');
const sync = (available) => { for (const button of buttons) button.hidden = !available || isStandalone(); };
sync(install.canPrompt());
install.onPromptChange(sync);
for (const button of buttons) {
  button.addEventListener('click', async () => {
    if (await install.prompt()) {
      document.getElementById('install-result')?.removeAttribute('hidden');
    }
  });
}
