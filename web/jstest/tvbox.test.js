// TVBox 配置的兼容导入。删除这个功能时连同 web/static/js/lib/tvbox.js 一起删掉。
import test from 'node:test';
import assert from 'node:assert/strict';
import { parseSubscription } from '../static/js/lib/cms.js';
import { parseLoose } from '../static/js/lib/tvbox.js';

const BASE = 'https://box.example/tv/config.json';

test('TVBox 配置只导入苹果 CMS 站点', () => {
  const config = `﻿{
    // 这是注释，地址里的 // 不能被当成注释
    "spider": "https://box.example/spider.jar;md5;abc",
    "sites": [
      { "key": "lz", "name": "🎬量子┃资源", "type": 1, "api": "https://lz.example/api.php/provide/vod/", "searchable": 1 },
      { "key": "xml", "name": "XML 站", "type": 0, "api": "http://xml.example/api.php/provide/vod/at/xml/" },
      { "key": "rel", "name": "相对路径", "type": 1, "api": "./cms/api.php" },
      { "key": "dup", "name": "重复接口", "type": 1, "api": "https://lz.example/api.php/provide/vod" },
      { "key": "csp_Douban", "name": "爬虫", "type": 3, "api": "csp_Douban" },
      { "key": "drpy", "name": "JS 爬虫", "type": 3, "api": "./drpy.js", "ext": "./x.js" },
      { "key": "nosearch", "name": "不能搜", "type": 1, "api": "https://ns.example/api", "searchable": 0 },
      /* 块注释 */
    ],
    "parses": [{ "name": "解析", "type": 0, "url": "https://jx.example/?url=" }],
    "lives": [{ "name": "直播", "url": "https://live.example/tv.txt" }],
  }`;
  const { sources, errors } = parseSubscription(config, BASE);
  assert.deepEqual(sources.map((s) => s.key), ['lz', 'xml', 'rel']);
  assert.equal(sources[0].name, '🎬量子┃资源');
  assert.equal(sources[1].api, 'http://xml.example/api.php/provide/vod/at/xml/'); // HTTP 也收，走本站转发
  assert.equal(sources[2].api, 'https://box.example/tv/cms/api.php');
  assert.deepEqual(errors, ['TVBox 配置里另有 2 个站点需要 TVBox 专用爬虫，网页里无法使用', 'TVBox 配置里有 1 个站点不支持搜索']);
});

test('TVBox 配置里的 key 不合规时按接口地址生成', () => {
  const { sources } = parseSubscription(JSON.stringify({ sites: [{ key: '量子', name: '量子', type: 1, api: 'https://lz.example/api.php/provide/vod/' }] }));
  const plain = parseSubscription(JSON.stringify([{ name: '量子', api: 'https://lz.example/api.php/provide/vod/' }]));
  assert.equal(sources[0].key, plain.sources[0].key);
});

test('TVBox 配置里没有可用站点时说明原因', () => {
  const { sources, errors } = parseSubscription(JSON.stringify({ sites: [{ key: 'a', type: 3, api: 'csp_A' }, { key: 'b', type: 3, api: 'csp_B' }] }), BASE);
  assert.equal(sources.length, 0);
  assert.match(errors[0], /没有网页能用的站点.*其余 2 个需要 TVBox 专用爬虫/);
  // 相对路径没有订阅地址可参照时，按"接口地址不正确"跳过
  const relative = parseSubscription(JSON.stringify({ sites: [{ key: 'r', name: '相对', type: 1, api: './api.php' }] }));
  assert.equal(relative.sources.length, 0);
  assert.match(relative.errors.at(-1), /相对：接口地址不正确/);
});

test('TVBox 可用站点超过上限时提示', () => {
  const sites = Array.from({ length: 65 }, (_, i) => ({ key: 's' + i, name: '站' + i, type: 1, api: `https://s${i}.example/api` }));
  const { sources, errors } = parseSubscription(JSON.stringify({ sites }));
  assert.equal(sources.length, 60);
  assert.deepEqual(errors, ['TVBox 配置里可用的站点超过 60 个，只导入前 60 个']);
});

test('TVBox 多仓配置提示改填单个仓库', () => {
  const { sources, errors } = parseSubscription(JSON.stringify({ urls: [{ name: '饭盒', url: './a.json' }, { name: '二号', url: 'https://b.example/b.json' }] }), BASE);
  assert.equal(sources.length, 0);
  assert.match(errors[0], /多仓配置暂不支持.*「饭盒」：https:\/\/box\.example\/tv\/a\.json/);
  assert.match(parseSubscription(JSON.stringify({ storeHouse: [{ sourceName: '仓', sourceUrl: 'https://c.example/c.json' }] })).errors[0], /「仓」：https:\/\/c\.example\/c\.json/);
});

test('加密或编码的配置给出明确提示', () => {
  const hex = '2423' + 'ab12'.repeat(40);
  assert.match(parseSubscription(hex).errors[0], /加密或编码/);
  assert.match(parseSubscription(btoa(JSON.stringify({ sites: [{ key: "a", type: 1, api: "https://a.example/api" }] }))).errors[0], /加密或编码/);
  assert.match(parseSubscription('ÿØJFIF...**' + 'QUJD'.repeat(20)).errors[0], /加密或编码/);
});

test('不影响原来的订阅格式', () => {
  assert.deepEqual(parseSubscription('not json').errors, ['订阅文件不是合法的 JSON']);
  assert.deepEqual(parseSubscription('{"a":1}').errors, ['订阅文件里没有 sources 列表']);
  // 原格式里写了注释：和以前一样按不合法的 JSON 处理，不会走宽松解析
  assert.deepEqual(parseSubscription('{ "sources": [] // 注释\n}').errors, ['订阅文件不是合法的 JSON']);
  // 同时有 sources 和 sites 时按原格式处理
  const both = parseSubscription(JSON.stringify({ sources: [{ key: 'a', name: 'A', api: 'https://a.example/api' }], sites: [{ type: 1, api: 'https://b.example/api' }] }));
  assert.deepEqual(both.sources.map((s) => s.key), ['a']);
  assert.deepEqual(both.errors, []);
});

test('宽松解析只去掉注释和多余逗号', () => {
  assert.deepEqual(parseLoose('{"u":"https://a.example//x/*y*/", "s":"a\\"//b", /* c */ "n":[1,2,],}'),
    { u: 'https://a.example//x/*y*/', s: 'a"//b', n: [1, 2] });
  assert.equal(parseLoose('{ broken'), undefined);
});
