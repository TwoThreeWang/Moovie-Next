import test from 'node:test';
import assert from 'node:assert/strict';
import { castMode, castErrorText } from '../static/js/lib/cast.js';

const UA = {
  androidChrome: 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36',
  iphoneSafari: 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1',
  macSafari: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15',
  macChrome: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
  androidFirefox: 'Mozilla/5.0 (Android 14; Mobile; rv:132.0) Gecko/132.0 Firefox/132.0',
};
const remote = { prompt() {}, state: 'disconnected' };
const airplay = () => {};

test('安卓有 Remote Playback API 时显示投屏按钮', () => {
  assert.equal(castMode({ remote }, UA.androidChrome), 'remote');
});

test('苹果 Safari 只显示 AirPlay（它的 video.remote 背后也是 AirPlay）', () => {
  assert.equal(castMode({ remote, webkitShowPlaybackTargetPicker: airplay }, UA.iphoneSafari), 'airplay');
  assert.equal(castMode({ remote, webkitShowPlaybackTargetPicker: airplay }, UA.macSafari), 'airplay');
});

test('桌面 Chrome 保持原样，不显示投屏按钮', () => {
  assert.equal(castMode({ remote }, UA.macChrome), null);
});

test('浏览器没有 Remote Playback API 时不显示', () => {
  assert.equal(castMode({}, UA.androidFirefox), null);
  assert.equal(castMode({ remote: {} }, UA.androidChrome), null);
  assert.equal(castMode(null, UA.androidChrome), null);
});

test('投屏失败的提示', () => {
  const err = (name) => Object.assign(new Error(name), { name });
  assert.equal(castErrorText(err('NotAllowedError')), '', '用户自己关掉设备列表，不用提示');
  assert.match(castErrorText(err('NotSupportedError')), /Chrome/);
  assert.match(castErrorText(err('NotFoundError')), /Chrome/);
  assert.equal(castErrorText(err('OperationError')), '');
  assert.ok(castErrorText(err('AbortError')));
});
