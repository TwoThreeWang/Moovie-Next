import test from 'node:test';
import assert from 'node:assert/strict';
import { normalizeTitle, splitSeason, chineseNumber, plainText } from '../static/js/lib/title.js';
import { parseEpisodeLabel, numberEpisodes } from '../static/js/lib/episode.js';
import { searchURL, checkURL, typeFromName, parsePlayLists, parseResponse, parseSubscription } from '../static/js/lib/cms.js';
import { scoreItem, bestMatch, findEpisode, searchKeyword, sameWork, incompleteKind, hiddenMatch } from '../static/js/lib/match.js';

test('片名规范化和服务器端一致', () => {
  assert.equal(normalizeTitle('雾港 第二季'), '雾港第二季');
  assert.equal(normalizeTitle('雾港：第二季'), '雾港第二季');
  assert.equal(normalizeTitle('  The Shawshank Redemption '), 'theshawshankredemption');
  assert.equal(normalizeTitle('ＡＢＣ１２３'), 'abc123');
  assert.equal(normalizeTitle('!!!'), '');
  assert.equal(normalizeTitle(null), '');
});

test('拆出季数', () => {
  assert.deepEqual(splitSeason('雾港'), { base: '雾港', season: 1 });
  assert.deepEqual(splitSeason('雾港 第二季'), { base: '雾港', season: 2 });
  assert.deepEqual(splitSeason('雾港第十二季'), { base: '雾港', season: 12 });
  assert.deepEqual(splitSeason('Dark Season 3'), { base: 'Dark', season: 3 });
  assert.deepEqual(splitSeason('风骚律师 S06'), { base: '风骚律师', season: 6 });
  assert.deepEqual(splitSeason('第二季'), { base: '第二季', season: 1 });
  assert.equal(chineseNumber('二十一'), 21);
  assert.equal(chineseNumber('甲'), 0);
});

test('资源站的文字先去掉 HTML', () => {
  assert.equal(plainText('<p>台风季的<b>港城</b></p>&nbsp;&amp; 旧案'), '台风季的 港城 & 旧案');
  assert.equal(plainText('<img src=x onerror=alert(1)>你好'), '你好');
  assert.equal(plainText(undefined), '');
});

test('识别各种写法的集数', () => {
  const cases = {
    '第05集': 5, '第5话': 5, '05': 5, '5': 5, 'EP05': 5, 'ep.12': 12, 'E3': 3, '第二十集': 20, '第十期': 10,
    '12集': 12, '第1集 超前点映': 1, 'HD': null, '正片': null, '国语版': null, '20261108期': null, '2026-11-08': null, '': null,
    '花絮': null, '１２': 12,
  };
  for (const [label, want] of Object.entries(cases)) assert.equal(parseEpisodeLabel(label), want, label);
});

test('给一条线路分配集数', () => {
  assert.deepEqual(numberEpisodes(['第01集', '第02集', '第03集']), [1, 2, 3]);
  // 个别识别不出来的按位置补上
  assert.deepEqual(numberEpisodes(['第01集', '彩蛋', '第03集']), [1, 2, 3]);
  // 重复的集名不能占同一个集数
  assert.deepEqual(numberEpisodes(['第01集', '第01集', '第02集']), [1, 2, 3]);
  // 大部分识别不出来：整条线路按顺序编号
  assert.deepEqual(numberEpisodes(['上', '中', '下', '第9集']), [1, 2, 3, 4]);
  assert.deepEqual(numberEpisodes(['20261101期', '20261108期']), [1, 2]);
  // 从中间开始更新的线路保持原来的集数
  assert.deepEqual(numberEpisodes(['第05集', '第06集']), [5, 6]);
});

test('搜索地址只查第一页，并保留接口原有的参数', () => {
  const url = new URL(searchURL('https://s.example/api.php/provide/vod/?token=abc', '雾港'));
  assert.equal(url.searchParams.get('ac'), 'detail');
  assert.equal(url.searchParams.get('wd'), '雾港');
  assert.equal(url.searchParams.get('pg'), '1');
  assert.equal(url.searchParams.get('token'), 'abc');
});

test('只接受 HTTPS 地址，本机除外', () => {
  assert.equal(checkURL('https://s.example/a.m3u8').ok, true);
  assert.deepEqual(checkURL('http://s.example/a.m3u8'), { ok: false, reason: 'http', url: 'http://s.example/a.m3u8' });
  assert.equal(checkURL('http://localhost:5210/api').ok, true);
  assert.equal(checkURL('http://127.0.0.1:5210/api').ok, true);
  assert.equal(checkURL('javascript:alert(1)').ok, false);
  assert.equal(checkURL('data:text/html,x').ok, false);
  assert.equal(checkURL('').ok, false);
  assert.equal(checkURL('not a url').ok, false);
});

test('根据分类名判断类型', () => {
  assert.equal(typeFromName('动作片'), 'movie');
  assert.equal(typeFromName('国产剧'), 'series');
  assert.equal(typeFromName('日韩动漫'), 'series');
  assert.equal(typeFromName('大陆综艺'), 'series');
  assert.equal(typeFromName('其他'), '');
});

test('解析播放列表', () => {
  const lines = parsePlayLists('量子$$$非凡', [
    '第01集$https://a.example/1/index.m3u8#第02集$https://a.example/2/index.m3u8',
    '第01集$http://b.example/1.m3u8#第02集$javascript:alert(1)#第03集$https://b.example/share/abc',
  ].join('$$$'));
  assert.equal(lines.length, 2);
  assert.equal(lines[0].name, '量子');
  assert.deepEqual(lines[0].episodes.map((e) => [e.number, e.insecure]), [[1, false], [2, false]]);
  // HTTP 地址保留但标记出来；javascript: 和不是视频文件的网页地址直接丢弃
  assert.equal(lines[1].episodes.length, 1);
  assert.equal(lines[1].episodes[0].insecure, true);
  // 没有集名的电影
  const movie = parsePlayLists('', 'https://a.example/movie.m3u8');
  assert.equal(movie[0].name, '线路 1');
  assert.equal(movie[0].episodes[0].url, 'https://a.example/movie.m3u8');
  // 地址里带 $ 时以最后一个 $ 为界
  assert.equal(parsePlayLists('x', 'HD$中字$https://a.example/a.m3u8?t=1')[0].episodes[0].label, 'HD$中字');
});

const JSON_BODY = JSON.stringify({ code: 1, list: [
  { vod_id: 12, vod_name: '<b>雾港</b>第二季', vod_year: '2026', type_name: '国产剧', vod_douban_id: 36000001, vod_pic: 'https://img.example/p.jpg',
    vod_remarks: '更新至08集', vod_content: '<script>alert(1)</script>', vod_play_from: 'lz', vod_play_url: '第01集$https://a.example/1.m3u8#第02集$https://a.example/2.m3u8' },
  { vod_id: 13, vod_name: '没有播放地址', vod_play_from: 'lz', vod_play_url: '' },
  { vod_id: 14, vod_name: '', vod_play_url: 'https://a.example/x.m3u8' },
  { vod_id: 15, vod_name: '回声', vod_year: '未知', type_name: '剧情片', vod_douban_id: '0', vod_pic: 'javascript:alert(1)', vod_play_from: 'lz', vod_play_url: 'HD$https://a.example/m.m3u8' },
] });

test('解析 JSON 接口，坏数据被跳过', () => {
  const items = parseResponse(JSON_BODY);
  assert.equal(items.length, 2);
  assert.deepEqual({ ...items[0], lines: undefined }, {
    vodId: '12', name: '雾港 第二季', year: 2026, typeName: '国产剧', type: 'series', doubanId: '36000001',
    pic: 'https://img.example/p.jpg', remarks: '更新至08集', lines: undefined,
  });
  assert.equal(items[0].lines[0].episodes.length, 2);
  assert.equal(items[1].year, 0);
  assert.equal(items[1].doubanId, '');
  assert.equal(items[1].pic, '');
  assert.equal(items[1].type, 'movie');
});

test('解析 XML 接口', () => {
  const xml = `<?xml version="1.0" encoding="utf-8"?><rss version="5.1"><list page="1">
<video><last>2026-11-08</last><id>77</id><tid>13</tid><name><![CDATA[雾港]]></name><type>国产剧</type><pic>https://img.example/p.jpg</pic>
<year>2024</year><note><![CDATA[全12集]]></note>
<dl><dd flag="lzm3u8"><![CDATA[第01集$https://a.example/1.m3u8#第02集$https://a.example/2.m3u8]]></dd><dd flag="ff"><![CDATA[第01集$https://b.example/1.m3u8]]></dd></dl></video>
</list></rss>`;
  const items = parseResponse(xml);
  assert.equal(items.length, 1);
  assert.equal(items[0].name, '雾港');
  assert.equal(items[0].year, 2024);
  assert.deepEqual(items[0].lines.map((l) => [l.name, l.episodes.length]), [['lzm3u8', 2], ['ff', 1]]);
});

test('损坏或不认识的响应返回空数组', () => {
  for (const body of ['', '   ', '{"list": "oops"}', '{broken', '<html><body>502</body></html>', 'plain text', '[]', 'null']) {
    assert.deepEqual(parseResponse(body), [], JSON.stringify(body));
  }
  assert.deepEqual(parseResponse(undefined), []);
});

test('校验订阅文件', () => {
  const { sources, errors } = parseSubscription(JSON.stringify({ version: 1, sources: [
    { key: 'lz', name: '量子', api: 'https://lz.example/api.php/provide/vod/' },
    { name: '没有 key', api: 'https://ff.example/api.php/provide/vod' },
    { key: 'http', name: 'HTTP 源', api: 'http://plain.example/api' },
    { key: 'lz', name: '重复', api: 'https://dup.example/api' },
    { key: 'bad', name: '坏地址', api: 'javascript:alert(1)' },
  ] }));
  assert.deepEqual(sources.map((s) => s.key).slice(0, 1), ['lz']);
  assert.equal(sources.length, 3); // HTTP 接口也收，浏览器访问不了时走本站转发
  assert.match(sources[1].key, /^u[0-9a-f]{8}$/);
  assert.equal(sources[2].api, 'http://plain.example/api');
  assert.equal(errors.length, 2);
  assert.match(errors[0], /key 重复/);
  assert.match(errors[1], /接口地址不正确/);
  // 没有 key 的源：同一个接口地址每次得到同一个 key
  const again = parseSubscription(JSON.stringify([{ name: 'x', api: 'https://ff.example/api.php/provide/vod/' }]));
  assert.equal(again.sources[0].key, sources[1].key);
  assert.equal(parseSubscription('not json').errors.length, 1);
  assert.equal(parseSubscription('{"a":1}').errors.length, 1);
});

const MEDIA = { title: '雾港 第二季', original_title: 'Fog Harbor', aliases: ['雾港2'], year: 2026, type: 'tv', season: 2, douban_id: '36000001' };
const item = (extra) => ({ name: '', year: 0, type: '', doubanId: '', lines: [], ...extra });

test('匹配搜索结果', () => {
  assert.equal(scoreItem(MEDIA, item({ name: '完全不同', doubanId: '36000001' })), 100, '豆瓣 ID 相同直接匹配');
  assert.ok(scoreItem(MEDIA, item({ name: '雾港第二季', year: 2026, type: 'series' })) >= 90);
  assert.ok(scoreItem(MEDIA, item({ name: '雾港 第2季' })) >= 85, '季数写法不同也能对上');
  assert.ok(scoreItem(MEDIA, item({ name: '雾港2' })) >= 90, '别名');
  assert.equal(scoreItem(MEDIA, item({ name: '雾港' })), 0, '第一季不是第二季');
  assert.equal(scoreItem(MEDIA, item({ name: '雾港 第三季' })), 0);
  assert.equal(scoreItem(MEDIA, item({ name: '雾港第二季', type: 'movie' })), 0, '类型不符');
  assert.equal(scoreItem(MEDIA, item({ name: '雾港往事' })), 0);
  const movie = { title: '回声', year: 2024, type: 'movie', season: 1 };
  assert.ok(scoreItem(movie, item({ name: '回声', year: 2024 })) > scoreItem(movie, item({ name: '回声' })));
  assert.equal(scoreItem(movie, item({ name: '回声', year: 2010 })), 0, '同名但年份差很多是另一部');
  assert.ok(scoreItem(movie, item({ name: '回声', year: 2025 })) > 0, '年份允许差一年');

  const found = bestMatch(MEDIA, [item({ name: '雾港' }), item({ name: '雾港第二季', year: 2026 }), item({ name: '雾港2' })]);
  assert.equal(found.item.name, '雾港第二季');
  assert.equal(bestMatch(MEDIA, [item({ name: '雾港' })]), null);
  assert.equal(searchKeyword(MEDIA), '雾港');
});

test('在线路里找某一集', () => {
  const line = { episodes: [{ number: 5, url: 'e5' }, { number: 6, url: 'e6' }] };
  assert.equal(findEpisode(line, 6, false).url, 'e6');
  assert.equal(findEpisode(line, 1, false), null);
  assert.equal(findEpisode(line, 99, true).url, 'e5', '电影直接取第一个');
  assert.equal(findEpisode({ episodes: [] }, 1, true), null);
});

test('换源找同一部：片名要完全一样，年份相同才算确定', () => {
  const media = { title: '雾港 第二季', year: 2026, type: 'series' };
  const items = [
    { name: '雾港', year: 2026, type: 'series', vodId: 'a' },          // 片名不同（少了季）
    { name: '雾港·第二季', year: 2024, type: 'series', vodId: 'b' },    // 年份不同
    { name: '雾港第二季', year: 0, type: '', vodId: 'c' },              // 没写年份：不确定
    { name: '雾港 第二季', year: 2026, type: 'movie', vodId: 'd' },     // 类型冲突
    { name: '雾港（第二季）', year: 2026, type: 'series', vodId: 'e' }, // 确定
  ];
  assert.deepEqual(sameWork(media, items), { item: items[4], sure: true });
  assert.deepEqual(sameWork(media, items.slice(0, 4)), { item: items[2], sure: false });
  assert.equal(sameWork(media, items.slice(0, 2)), null);
  assert.equal(sameWork({ title: '回声', year: 0, type: 'movie' }, [{ name: '回声', year: 2025, type: 'movie' }]).sure, false, '自己没年份时也不确定');
  assert.equal(sameWork({ title: '', year: 0 }, items), null);
});

test('预告片、花絮和枪版不算正片', () => {
  // 资源站常给预告片填上正片的豆瓣 ID：豆瓣 ID 相同也不能选它
  const trailer = item({ name: '雾港 第二季（预告片）', doubanId: '36000001', remarks: '预告片' });
  const feature = item({ name: '雾港 第二季', doubanId: '36000001', remarks: '更新至12集' });
  assert.equal(scoreItem(MEDIA, trailer), 0);
  assert.equal(bestMatch(MEDIA, [trailer, feature]).item, feature, '同一个源里正片和预告片都有时选正片');
  assert.equal(bestMatch(MEDIA, [trailer]), null, '只有预告片算没找到');
  assert.equal(hiddenMatch(MEDIA, [trailer]), 'trailer', '但能说明是因为只有预告片');
  assert.equal(hiddenMatch(MEDIA, [item({ name: '别的剧', remarks: '预告片' })]), '', '不是这部作品的预告片不算');

  const kinds = {
    'remarks:TC中字': 'cam', 'remarks:HDTC': 'cam', 'remarks:HD-TS 国语': 'cam', 'remarks:枪版': 'cam', 'name:回声 抢先版': 'cam', 'remarks:CAM': 'cam',
    'remarks:花絮': 'trailer', 'typeName:预告片': 'trailer', 'remarks:片花': 'trailer', 'remarks:Trailer': 'trailer',
    'remarks:HD中字': '', 'remarks:HDTV': '', 'remarks:更新至12集': '', 'name:TCL 电视': '', 'name:Totsuki': '', 'remarks:4K': '', 'typeName:国产剧': '',
  };
  for (const [field, want] of Object.entries(kinds)) {
    const [key, value] = field.split(':');
    assert.equal(incompleteKind(item({ name: '回声', [key]: value })), want, field);
  }
  const movie = { title: '回声', year: 2024, type: 'movie' };
  assert.equal(sameWork(movie, [item({ name: '回声', year: 2024, remarks: 'TC' })]), null, '没有元数据的作品换源时也跳过枪版');
});

test('正片里夹带的预告、花絮不进选集', () => {
  const lines = parsePlayLists('量子$$$预告', [
    '预告片$https://a.example/t.m3u8#第1集$https://a.example/1.m3u8#第2集$https://a.example/2.m3u8#花絮1$https://a.example/x.m3u8',
    '预告$https://a.example/t2.m3u8',
  ].join('$$$'));
  assert.equal(lines.length, 1, '整条都是预告的线路去掉');
  assert.deepEqual(lines[0].episodes.map((e) => [e.label, e.number]), [['第1集', 1], ['第2集', 2]], '预告、花絮不占集数');
  assert.equal(parsePlayLists('', '预告片$https://a.example/t.m3u8').length, 0, '只有预告的电影没有可播的线路');
});
