// 识别手机系统和浏览器，决定"添加到桌面"教程该显示哪一份。纯函数，方便测试。

const IN_APP = [
  [/MicroMessenger/i, '微信'],
  [/\bQQ\//i, 'QQ'],
  [/Weibo/i, '微博'],
  [/DingTalk/i, '钉钉'],
  [/aweme|BytedanceWebview|ByteLocale/i, '抖音'],
  [/AlipayClient/i, '支付宝'],
  [/xhsdiscover/i, '小红书'],
];

const ANDROID_BROWSERS = [
  [/SamsungBrowser/i, '三星浏览器'],
  [/MiuiBrowser|XiaoMi\/MiuiBrowser/i, '小米浏览器'],
  [/HuaweiBrowser|HBPC/i, '华为浏览器'],
  [/HeyTapBrowser|OppoBrowser/i, 'OPPO 浏览器'],
  [/VivoBrowser/i, 'vivo 浏览器'],
  [/UCBrowser/i, 'UC 浏览器'],
  [/Quark/i, '夸克'],
  [/MQQBrowser/i, 'QQ 浏览器'],
  [/EdgA\//i, 'Edge'],
  [/Firefox/i, 'Firefox'],
];

/**
 * 返回 { os, browser, inApp, appName }。
 * os：ios / android / desktop。browser：safari / chrome / other。
 * inApp 为 true 表示在微信、QQ 这类 App 内置的浏览器里，那里没法添加到桌面。
 */
export function detectPlatform(userAgent, { maxTouchPoints = 0, platform = '' } = {}) {
  const ua = String(userAgent ?? '');
  // iPad 默认用桌面版 UA，只能靠触点数区分
  const ios = /iPhone|iPad|iPod/i.test(ua) || (/Macintosh/i.test(ua) && maxTouchPoints > 1) || (platform === 'MacIntel' && maxTouchPoints > 1);
  const android = /Android/i.test(ua);
  const os = ios ? 'ios' : android ? 'android' : 'desktop';
  for (const [pattern, name] of IN_APP) {
    if (pattern.test(ua)) return { os, browser: 'other', inApp: true, appName: name };
  }
  if (ios) {
    const browser = /CriOS|FxiOS|EdgiOS|OPiOS|MQQBrowser|UCBrowser|Quark/i.test(ua) ? 'other' : 'safari';
    return { os, browser, inApp: false, appName: browser === 'safari' ? 'Safari' : '' };
  }
  if (android) {
    for (const [pattern, name] of ANDROID_BROWSERS) {
      if (pattern.test(ua)) return { os, browser: 'other', inApp: false, appName: name };
    }
    return { os, browser: /Chrome\//i.test(ua) ? 'chrome' : 'other', inApp: false, appName: /Chrome\//i.test(ua) ? 'Chrome' : '' };
  }
  const browser = /Edg\//i.test(ua) || /Chrome\//i.test(ua) ? 'chrome' : /Safari/i.test(ua) ? 'safari' : 'other';
  return { os, browser, inApp: false, appName: '' };
}

/** 现在是不是已经从桌面图标打开（全屏模式）。 */
export function isStandalone(win = globalThis) {
  return Boolean(win.matchMedia?.('(display-mode: standalone)').matches || win.navigator?.standalone === true);
}
