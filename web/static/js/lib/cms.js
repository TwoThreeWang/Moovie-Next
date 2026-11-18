// 解析苹果 CMS 格式的资源站接口。
//
// 资源站返回的所有内容都是不可信数据：文字先去掉 HTML，地址只接受 http(s)。
// 这里只做解析，不发请求，方便测试。
import { plainText } from './title.js';
import { numberEpisodes } from './episode.js';

const MAX_ITEMS = 40;
const MAX_EPISODES = 3000;

/** 拼出搜索接口的地址。只查第一页：很短的关键词可能匹配到非常多的结果。 */
export function searchURL(api, keyword) {
  const url = new URL(api);
  url.searchParams.set('ac', 'detail');
  url.searchParams.set('wd', keyword);
  url.searchParams.set('pg', '1');
  return url.toString();
}

/**
 * 检查一个地址能不能从 HTTPS 页面访问。
 * 返回 { ok, url, reason }。HTTP 地址只有本机（开发用）才允许。
 */
export function checkURL(raw, base) {
  let url;
  try { url = new URL(String(raw).trim(), base); } catch { return { ok: false, reason: 'invalid' }; }
  if (url.protocol === 'https:') return { ok: true, url: url.toString() };
  if (url.protocol === 'http:') {
    if (url.hostname === 'localhost' || url.hostname === '127.0.0.1') return { ok: true, url: url.toString() };
    return { ok: false, reason: 'http', url: url.toString() };
  }
  return { ok: false, reason: 'invalid' };
}

/** 根据资源站的分类名判断是电影还是分集的。判断不出来返回空字符串。 */
export function typeFromName(typeName) {
  const name = String(typeName ?? '').trim();
  if (/纪录/.test(name)) return ''; // 纪录片既有电影也有剧集，不猜
  if (/片$|电影/.test(name)) return 'movie'; // "剧情片""动作片"是电影分类，先于下面的"剧"判断
  if (/剧|动漫|动画|番|综艺|连续/.test(name)) return 'series';
  return '';
}

/** 解析播放列表字段："第1集$url#第2集$url$$$第1集$url2…" 和对应的线路名 "线路A$$$线路B"。 */
export function parsePlayLists(playFrom, playURL) {
  const names = String(playFrom ?? '').split('$$$');
  const lines = [];
  String(playURL ?? '').split('$$$').forEach((block, index) => {
    const episodes = [];
    for (const part of block.split('#')) {
      if (episodes.length >= MAX_EPISODES) break;
      const cut = part.lastIndexOf('$');
      const label = plainText(cut >= 0 ? part.slice(0, cut) : '');
      const raw = (cut >= 0 ? part.slice(cut + 1) : part).trim();
      if (!raw) continue;
      const checked = checkURL(raw);
      if (!checked.ok && checked.reason !== 'http') continue; // javascript: 之类的直接丢弃
      // 只保留能直接播放的地址；"解析页"之类的网页地址播放器放不了。
      if (!/\.(m3u8|mp4)(\?|#|$)/i.test(checked.url)) continue;
      episodes.push({ label: label || String(episodes.length + 1), url: checked.url, insecure: !checked.ok });
    }
    if (!episodes.length) return;
    const numbers = numberEpisodes(episodes.map((e) => e.label));
    episodes.forEach((episode, i) => { episode.number = numbers[i]; });
    lines.push({ name: plainText(names[index]) || `线路 ${index + 1}`, episodes });
  });
  return lines;
}

function normalizeItem(raw) {
  const name = plainText(raw.name);
  const lines = parsePlayLists(raw.playFrom, raw.playURL);
  if (!name || !lines.length) return null;
  const year = parseInt(String(raw.year ?? '').slice(0, 4), 10);
  const doubanId = String(raw.doubanId ?? '').trim();
  const pic = checkURL(String(raw.pic ?? ''));
  return {
    vodId: String(raw.id ?? ''),
    name,
    year: year > 1880 && year < 2100 ? year : 0,
    typeName: plainText(raw.typeName),
    type: typeFromName(raw.typeName),
    doubanId: /^\d{6,9}$/.test(doubanId) ? doubanId : '',
    pic: pic.ok ? pic.url : '',
    remarks: plainText(raw.remarks).slice(0, 40),
    lines,
  };
}

function parseJSON(data) {
  const list = Array.isArray(data?.list) ? data.list : [];
  return list.slice(0, MAX_ITEMS).map((v) => normalizeItem({
    id: v.vod_id, name: v.vod_name, year: v.vod_year, typeName: v.type_name, doubanId: v.vod_douban_id,
    pic: v.vod_pic, remarks: v.vod_remarks, playFrom: v.vod_play_from, playURL: v.vod_play_url,
  })).filter(Boolean);
}

// XML 接口的结构很固定，用正则提取就够了，而且在浏览器和 node 里都能跑。
function xmlText(block, tag) {
  const match = new RegExp(`<${tag}[^>]*>([\\s\\S]*?)</${tag}>`, 'i').exec(block);
  if (!match) return '';
  return match[1].replace(/^\s*<!\[CDATA\[([\s\S]*?)\]\]>\s*$/, '$1').trim();
}

function parseXML(text) {
  const items = [];
  const videos = text.match(/<video>[\s\S]*?<\/video>/gi) ?? [];
  for (const block of videos.slice(0, MAX_ITEMS)) {
    const names = [], urls = [];
    for (const dd of block.matchAll(/<dd[^>]*flag="([^"]*)"[^>]*>([\s\S]*?)<\/dd>/gi)) {
      names.push(dd[1]);
      urls.push(dd[2].replace(/^\s*<!\[CDATA\[([\s\S]*?)\]\]>\s*$/, '$1').trim());
    }
    const item = normalizeItem({
      id: xmlText(block, 'id'), name: xmlText(block, 'name'), year: xmlText(block, 'year'), typeName: xmlText(block, 'type'),
      doubanId: '', pic: xmlText(block, 'pic'), remarks: xmlText(block, 'note'),
      playFrom: names.join('$$$'), playURL: urls.join('$$$'),
    });
    if (item) items.push(item);
  }
  return items;
}

/** 解析接口返回的文本（JSON 或 XML）。格式不认识或内容损坏时返回空数组，不抛错。 */
export function parseResponse(text) {
  const body = String(text ?? '').trim();
  if (!body) return [];
  if (body[0] === '{' || body[0] === '[') {
    try { return parseJSON(JSON.parse(body)); } catch { return []; }
  }
  if (body[0] === '<') {
    try { return parseXML(body); } catch { return []; }
  }
  return [];
}

/**
 * 校验订阅文件的内容，返回 { sources, errors }。
 * 每个源需要稳定的 key；没有 key 的用接口地址生成一个。
 */
export function parseSubscription(text) {
  let data;
  try { data = JSON.parse(text); } catch { return { sources: [], errors: ['订阅文件不是合法的 JSON'] }; }
  const list = Array.isArray(data) ? data : data?.sources;
  if (!Array.isArray(list)) return { sources: [], errors: ['订阅文件里没有 sources 列表'] };
  const sources = [], errors = [], seen = new Set();
  for (const entry of list.slice(0, 60)) {
    const name = plainText(entry?.name).slice(0, 30) || '未命名';
    const checked = checkURL(entry?.api ?? '');
    if (!checked.url) { // HTTP 接口也收：查询由本站转发
      errors.push(`${name}：接口地址不正确`);
      continue;
    }
    let key = String(entry?.key ?? '').trim().slice(0, 40);
    if (!/^[\w.\-]+$/.test(key)) key = 'u' + hash32(checked.url.replace(/\/+$/, '').toLowerCase());
    if (seen.has(key)) { errors.push(`${name}：key 重复`); continue; }
    seen.add(key);
    sources.push({ key, name, api: checked.url });
  }
  return { sources, errors };
}

/** 简单的 32 位字符串哈希（FNV-1a），只用来给没有 key 的源生成一个稳定的标识。 */
export function hash32(text) {
  let hash = 0x811c9dc5;
  for (let i = 0; i < text.length; i++) {
    hash ^= text.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(16).padStart(8, '0');
}
