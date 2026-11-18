import test from 'node:test';
import assert from 'node:assert/strict';
import { detectPlatform, isStandalone } from '../static/js/lib/platform.js';

const UA = {
  iphoneSafari: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1',
  iphoneChrome: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/131.0.6778.73 Mobile/15E148 Safari/604.1',
  iphoneWeChat: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MicroMessenger/8.0.54(0x18003637) NetType/WIFI Language/zh_CN',
  ipad: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15',
  androidChrome: 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36',
  androidHuawei: 'Mozilla/5.0 (Linux; Android 12; NOH-AN00; HMSCore 6.13.0.302) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.5735.196 HuaweiBrowser/14.0.6.300 Mobile Safari/537.36',
  androidQQApp: 'Mozilla/5.0 (Linux; Android 13; V2219A) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/109.0 Mobile Safari/537.36 V1_AND_SQ_9.0.0_4660_YYB_D QQ/9.0.0.14110 NetType/WIFI',
  androidQQBrowser: 'Mozilla/5.0 (Linux; U; Android 13; zh-cn; V2219A) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/109.0 MQQBrowser/14.2 Mobile Safari/537.36',
  macChrome: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
  winEdge: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0',
};

test('识别手机系统和浏览器', () => {
  assert.deepEqual(detectPlatform(UA.iphoneSafari), { os: 'ios', browser: 'safari', inApp: false, appName: 'Safari' });
  assert.equal(detectPlatform(UA.iphoneChrome).browser, 'other');
  assert.deepEqual(detectPlatform(UA.iphoneWeChat), { os: 'ios', browser: 'other', inApp: true, appName: '微信' });
  // iPad 默认用桌面版 UA：只有触点数能区分；Mac 没有触屏
  assert.equal(detectPlatform(UA.ipad, { maxTouchPoints: 5 }).os, 'ios');
  assert.equal(detectPlatform(UA.ipad, { maxTouchPoints: 0 }).os, 'desktop');
  assert.deepEqual(detectPlatform(UA.androidChrome), { os: 'android', browser: 'chrome', inApp: false, appName: 'Chrome' });
  assert.equal(detectPlatform(UA.androidHuawei).appName, '华为浏览器');
  assert.equal(detectPlatform(UA.androidHuawei).browser, 'other', '国产浏览器 UA 里也带 Chrome，不能当成 Chrome');
  assert.equal(detectPlatform(UA.androidQQApp).inApp, true, 'QQ 里打开的');
  assert.equal(detectPlatform(UA.androidQQBrowser).inApp, false, 'QQ 浏览器不是 App 内置浏览器');
  assert.equal(detectPlatform(UA.macChrome).browser, 'chrome');
  assert.equal(detectPlatform(UA.winEdge).browser, 'chrome');
  assert.equal(detectPlatform('').os, 'desktop');
});

test('判断是不是从桌面图标打开', () => {
  assert.equal(isStandalone({ matchMedia: () => ({ matches: true }) }), true);
  assert.equal(isStandalone({ matchMedia: () => ({ matches: false }), navigator: { standalone: true } }), true, 'iOS 老版本用 navigator.standalone');
  assert.equal(isStandalone({ matchMedia: () => ({ matches: false }), navigator: {} }), false);
  assert.equal(isStandalone({}), false);
});
