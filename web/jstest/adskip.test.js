import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { analyze, isMaster, pickVariant, parseSegments, buildBlocks, canonicalText } from '../static/js/lib/m3u8.js';
import { parseList, lookup } from '../static/js/lib/fplist.js';

// 一份典型的带广告的播放清单：开头 4 段广告（另一个目录、另一把密钥、3 秒一片），
// 正片 10 秒一片，片中同一段广告再出现一次。
function playlist({ adAtStart = true, adInMiddle = true, body = 40 } = {}) {
  const out = ['#EXTM3U', '#EXT-X-VERSION:3', '#EXT-X-TARGETDURATION:10', '#EXT-X-PLAYLIST-TYPE:VOD'];
  const ad = () => {
    out.push('#EXT-X-DISCONTINUITY', '#EXT-X-KEY:METHOD=AES-128,URI="https://k.example/ad.key"');
    for (let i = 0; i < 4; i++) out.push(`#EXTINF:${i === 3 ? '2.040000' : '3.000000'},`, `/20260820/R7Q9mzPo/1000kb/hls/ad${i}.ts?sign=abc`);
    out.push('#EXT-X-DISCONTINUITY', '#EXT-X-KEY:METHOD=AES-128,URI="https://k.example/main.key"');
  };
  if (adAtStart) ad(); else out.push('#EXT-X-KEY:METHOD=AES-128,URI="https://k.example/main.key"');
  for (let i = 0; i < body; i++) {
    if (adInMiddle && i === body / 2) ad();
    out.push('#EXTINF:10.000000,', `seg${String(i).padStart(4, '0')}.ts`);
  }
  out.push('#EXT-X-ENDLIST');
  return out.join('\n');
}

const URL_A = 'https://cdn-a.example/20260806/DHhUeik0/2000kb/hls/index.m3u8?token=1';
const URL_B = 'https://cdn-b.example:8443/20260806/DHhUeik0/2000kb/hls/index.m3u8';

test('找出开头和片中的广告，同一段广告的指纹相同', async () => {
  const found = await analyze(playlist(), URL_A);
  assert.equal(found.length, 2);
  assert.equal(found[0].start, 0);
  assert.ok(Math.abs(found[0].dur - 11.04) < 0.001);
  assert.ok(Math.abs(found[1].start - (11.04 + 200)) < 0.001);
  assert.equal(found[0].hex, found[1].hex);
  assert.match(found[0].hex, /^[0-9a-f]{64}$/);
});

test('指纹不受域名、端口和查询参数影响', async () => {
  const a = await analyze(playlist(), URL_A), b = await analyze(playlist(), URL_B);
  assert.equal(a[0].hex, b[0].hex);
});

test('指纹的值固定不变（改动算法会让已有的指纹库失效）', async () => {
  const found = await analyze(playlist(), URL_A);
  const expected = '3000|/20260820/R7Q9mzPo/1000kb/hls/ad0.ts\n3000|/20260820/R7Q9mzPo/1000kb/hls/ad1.ts\n3000|/20260820/R7Q9mzPo/1000kb/hls/ad2.ts\n2040|/20260820/R7Q9mzPo/1000kb/hls/ad3.ts';
  const blocks = buildBlocks(parseSegments(playlist(), URL_A));
  assert.equal(canonicalText(blocks[0], URL_A), expected);
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(expected));
  assert.equal(found[0].hex, Buffer.from(digest).toString('hex'));
});

test('拿不准的时候不报告', async () => {
  assert.deepEqual(await analyze(playlist({ adAtStart: false, adInMiddle: false }), URL_A), [], '没有广告');
  assert.deepEqual(await analyze(playlist().replace('#EXT-X-ENDLIST', ''), URL_A), [], '直播流不处理');
  assert.deepEqual(await analyze('#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nlow.m3u8', URL_A), [], '主播放清单不处理');
  assert.deepEqual(await analyze('', URL_A), []);
  // 只有一个 discontinuity、其他特征都和正片一样的短片段（比如片头）：信号不够，不算广告
  const intro = ['#EXTM3U'];
  for (let i = 0; i < 3; i++) intro.push('#EXTINF:10,', `seg${i}.ts`);
  intro.push('#EXT-X-DISCONTINUITY');
  for (let i = 3; i < 40; i++) intro.push('#EXTINF:10,', `seg${i}.ts`);
  intro.push('#EXT-X-ENDLIST');
  assert.deepEqual(await analyze(intro.join('\n'), URL_A), []);
  // 两段差不多长：找不出正片主体，整条放弃
  const even = ['#EXTM3U'];
  for (let part = 0; part < 4; part++) {
    even.push('#EXT-X-DISCONTINUITY');
    for (let i = 0; i < 6; i++) even.push('#EXTINF:10,', `/p${part}/s${i}.ts`);
  }
  even.push('#EXT-X-ENDLIST');
  assert.deepEqual(await analyze(even.join('\n'), URL_A), []);
});

test('主播放清单：取码率最高的一路', () => {
  const master = '#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlow/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2800000\n\nhigh/index.m3u8\n';
  assert.equal(isMaster(master), true);
  assert.equal(pickVariant(master, 'https://cdn.example/v/master.m3u8'), 'https://cdn.example/v/high/index.m3u8');
  assert.equal(isMaster(playlist()), false);
});

// 和旧播放器逐一对比。旧文件在 new/ 目录之外，只读取、不修改；文件不在时跳过这一项。
test('和旧播放器算出的指纹完全相同', async (t) => {
  const legacyPath = new URL('../../../web/static/js/player.js', import.meta.url);
  if (!fs.existsSync(legacyPath)) return t.skip('旧播放器文件不存在');
  const source = fs.readFileSync(legacyPath, 'utf8');
  const start = source.indexOf('function adSkipResolvePath'), end = source.indexOf('function adSkipMatchAPI');
  assert.ok(start > 0 && end > start, '没有在旧文件里找到广告分析的代码');
  const context = vm.createContext({ URL, TextEncoder, Promise, crypto, window: { crypto }, Math, parseFloat });
  vm.runInContext(source.slice(start, end), context);
  const samples = [
    [playlist(), URL_A], [playlist({ adInMiddle: false }), URL_A], [playlist({ adAtStart: false }), URL_B],
    [playlist({ body: 400 }), URL_A], [playlist({ adAtStart: false, adInMiddle: false }), URL_A],
  ];
  for (const [text, url] of samples) {
    const legacy = context.adSkipAnalyze(text, url);
    const legacyCandidates = legacy ? await context.adSkipFingerprints(legacy.candidates, url) : [];
    const current = await analyze(text, url);
    assert.deepEqual(current.map((c) => [c.hex, c.start, c.end, c.score]), legacyCandidates.map((c) => [c.hex, c.block.start, c.block.end, c.score]));
  }
});

// ---------- 指纹列表 ----------

function encodeList(confirmed, rejected) {
  const sorted = (list) => list.map((hex) => Buffer.from(hex.slice(0, 32), 'hex')).sort(Buffer.compare);
  const header = Buffer.alloc(16);
  header.write('MVFP'); header[4] = 1; header[5] = 16;
  header.writeUInt32BE(confirmed.length, 8); header.writeUInt32BE(rejected.length, 12);
  return new Uint8Array(Buffer.concat([header, ...sorted(confirmed), ...sorted(rejected)]));
}

const fp = (byte) => byte.toString(16).padStart(2, '0').repeat(32);

test('在本地查询指纹列表', () => {
  const confirmed = Array.from({ length: 200 }, (_, i) => fp(i)), rejected = [fp(250), fp(240)];
  const list = parseList(encodeList(confirmed, rejected));
  assert.equal(list.confirmedCount, 200);
  for (const hex of [fp(0), fp(73), fp(199)]) assert.equal(lookup(list, hex), 'confirmed');
  assert.equal(lookup(list, fp(240)), 'rejected');
  assert.equal(lookup(list, fp(250)), 'rejected');
  assert.equal(lookup(list, fp(230)), 'unknown');
  // 前 16 字节相同就算命中，后 16 字节不参与比较
  assert.equal(lookup(list, fp(73).slice(0, 32) + 'ff'.repeat(16)), 'confirmed');
  assert.equal(lookup(list, 'xyz'), 'unknown');
  assert.equal(lookup(null, fp(1)), 'unknown');
  // 空列表
  const empty = parseList(encodeList([], []));
  assert.equal(lookup(empty, fp(1)), 'unknown');
});

test('格式不对的列表被拒绝', () => {
  const good = encodeList([fp(1)], []);
  assert.equal(parseList(new Uint8Array(4)), null);
  assert.equal(parseList(good.slice(0, good.length - 1)), null, '长度和数量对不上');
  const wrongMagic = good.slice(); wrongMagic[0] = 0x58;
  assert.equal(parseList(wrongMagic), null);
  const wrongVersion = good.slice(); wrongVersion[4] = 2;
  assert.equal(parseList(wrongVersion), null);
  assert.ok(parseList(good.buffer), '也接受 ArrayBuffer');
});
