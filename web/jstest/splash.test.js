import test from 'node:test';
import assert from 'node:assert/strict';
import { splashLayout } from '../static/js/core/splash.js';

test('启动图排版：图标、站名、标语从上到下不重叠，整体在屏幕里水平居中', () => {
  for (const [width, height] of [[375, 667], [393, 852], [440, 956], [820, 1180], [1180, 820], [1032, 1376]]) {
    const { logo, name, slogan } = splashLayout(width, height);
    assert.ok(logo.y > 0, `${width}x${height} 图标在屏幕内`);
    assert.ok(Math.abs(logo.x * 2 + logo.size - width) <= 1, `${width}x${height} 图标水平居中`);
    assert.ok(name.y >= logo.y + logo.size, `${width}x${height} 站名在图标下面`);
    assert.ok(slogan.y >= name.y + name.size, `${width}x${height} 标语在站名下面`);
    assert.ok(slogan.y + slogan.size < height * 0.6, `${width}x${height} 整体偏上`);
  }
});

test('启动图排版：图标大小有上下限', () => {
  assert.equal(splashLayout(320, 568).logo.size, 88);
  assert.equal(splashLayout(1032, 1376).logo.size, 120);
});
