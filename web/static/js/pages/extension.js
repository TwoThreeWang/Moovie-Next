// 浏览器插件的说明页和连接页。插件装了的话，它在本站页面上会写 <html data-moovie-ext="版本号">。

const installed = () => document.documentElement.dataset.moovieExt || '';

// 版本号比较：a 新返回正数
function compare(a, b) {
  const left = a.split('.').map((x) => parseInt(x, 10) || 0), right = b.split('.').map((x) => parseInt(x, 10) || 0);
  for (let i = 0; i < Math.max(left.length, right.length); i++) {
    const diff = (left[i] || 0) - (right[i] || 0);
    if (diff) return diff;
  }
  return 0;
}

// 说明页：按浏览器显示对应的安装步骤，告诉你这个浏览器装没装、是不是最新版
const page = document.querySelector('[data-ext-version]');
if (page) {
  const tabs = document.querySelectorAll('[data-guide-tab]'), guides = document.querySelectorAll('[data-guide]');
  const show = (name) => {
    for (const tab of tabs) tab.setAttribute('aria-current', String(tab.dataset.guideTab === name));
    for (const guide of guides) guide.hidden = guide.dataset.guide !== name;
  };
  for (const tab of tabs) tab.addEventListener('click', (ev) => { ev.preventDefault(); show(tab.dataset.guideTab); });
  const ua = navigator.userAgent;
  show(/Edg\//.test(ua) ? 'edge' : /Chrome\//.test(ua) && !/OPR\/|QQBrowser|360SE|MetaSr/.test(ua) ? 'chrome' : 'other');

  const detect = document.getElementById('ext-detect');
  const latest = page.dataset.extVersion;
  // 插件的脚本在页面开始加载时就写好了标记，这里稍等一下再看，免得极少数情况下还没写上
  setTimeout(() => {
    const version = installed();
    if (!version) return;
    detect.hidden = false;
    detect.textContent = compare(latest, version) > 0
      ? `这个浏览器装的插件是 ${version}，有新版本 ${latest}，按下面「怎么更新」的步骤更新。`
      : `这个浏览器已经装了最新版插件（${version}）。`;
  }, 300);
}

// 连接页：没装插件时提示先装；凭证交出去之后，插件会自己改页面上的提示
const missing = document.getElementById('ext-missing') || document.getElementById('ext-not-installed');
if (missing) {
  setTimeout(() => {
    if (installed()) return;
    missing.hidden = false;
    const status = document.getElementById('ext-connect-status');
    if (status) status.hidden = true;
  }, 1500);
}
