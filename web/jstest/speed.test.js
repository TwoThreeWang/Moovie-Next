import test from 'node:test';
import assert from 'node:assert/strict';
import { speedRatio, SpeedMeter, fasterLine, formatSpeed } from '../static/js/lib/speed.js';

test('倍速：2 MB 的 10 秒视频用 5 秒下完是 2 倍速', () => {
  assert.equal(speedRatio(2e6, 5000, 10), 2);
  assert.equal(speedRatio(0, 5000, 10), 0);
  assert.equal(speedRatio(2e6, 0, 10), 0);
});

test('至少 3 个分片才判断线路慢不慢', () => {
  const meter = new SpeedMeter();
  meter.add(1e6, 9000, 10); // 1.1 倍速
  meter.add(1e6, 9000, 10);
  assert.equal(meter.isSlow(), false, '样本不够时不下结论');
  meter.add(1e6, 9000, 10);
  assert.equal(meter.isSlow(), true);
  assert.equal(meter.isSlow(0.75), false, '慢放时，同一条线路有足够余量');
  assert.ok(Math.abs(meter.value - 1.11) < 0.01);
});

test('只换到明显更快的线路', () => {
  assert.deepEqual(fasterLine(0.9, [{ index: 1, ratio: 1.2 }, { index: 2, ratio: 3 }, { index: 3, ratio: 2 }]), { index: 2, ratio: 3 });
  assert.equal(fasterLine(1.2, [{ index: 1, ratio: 1.6 }]), null, '快不到当前的 1.5 倍不换');
  assert.equal(fasterLine(0.5, [{ index: 1, ratio: 1.4 }]), null, '自己也不到 1.5 倍速的不换');
  assert.equal(fasterLine(1, []), null);
});

test('高倍速播放时，测速和换线都考虑更快的缓冲消耗', () => {
  const meter = new SpeedMeter();
  for (let i = 0; i < 3; i++) meter.add(1e6, 5000, 10);
  assert.equal(meter.isSlow(), false);
  assert.equal(meter.isSlow(2), true);
  const probes = [{ index: 1, ratio: 2.5 }, { index: 2, ratio: 3.2 }];
  assert.equal(fasterLine(1.5, probes.slice(0, 1), 2), null);
  assert.deepEqual(fasterLine(1.5, probes, 2), probes[1]);
});

test('速度文字', () => {
  assert.equal(formatSpeed(1.234e6), '1.2 MB/s');
  assert.equal(formatSpeed(640e3), '640 KB/s');
  assert.equal(formatSpeed(0), '');
});
