// TVBox 配置的兼容导入。
//
// 只取 sites 里 type 0（XML）和 type 1（JSON）的站点：它们就是苹果 CMS 接口，转换成订阅文件的
// { key, name, api } 之后交给 parseSubscription 照常校验。其余内容一律不用：
// - type 3 等爬虫站点要运行 jar / js / py 爬虫，等于在本站执行第三方代码，网页里也跑不了；
// - parses 多用于解析正版平台的 VIP 链接；lives、spider 等和点播搜索无关。
// 加密或藏在图片里的配置、多仓配置暂不支持，给出明确提示。
//
// 要删除这个功能时一起删：这个文件、cms.js 里的 import 和 parseSubscription 调用 readTVBox 的那一段、
// web/jstest/tvbox.test.js、设置页「订阅文件的格式」里关于 TVBox 的那句说明。
// 已经填了 TVBox 地址的用户会读取失败（已同步的设备继续用旧的源），删除时把 cms.js 里
// "订阅文件里没有 sources 列表" 的提示改成 "已不再支持 TVBox 订阅，请换成普通订阅地址"。

/**
 * 识别并转换 TVBox 配置。不是 TVBox 配置时返回 null，交给原来的流程处理。
 * 返回 { error } 或 { sources, notes }：notes 是导入时跳过了什么，显示在源列表下面。
 * data 是严格 JSON 解析的结果（解析失败为 undefined），base 是订阅地址，用来解析相对路径。
 */
export function readTVBox(text, data, base, limit) {
  if (data === undefined) {
    data = parseLoose(text);
    if (!isTVBox(data)) return looksEncoded(text) ? { error: '订阅内容经过加密或编码（常见于 TVBox 加密配置），暂不支持，请换一个未加密的配置地址。' } : null;
  }
  if (!isTVBox(data)) return null;

  const repos = Array.isArray(data.urls) ? data.urls.map((r) => [r?.name, r?.url])
    : Array.isArray(data.storeHouse) ? data.storeHouse.map((r) => [r?.sourceName, r?.sourceUrl]) : [];
  if (!Array.isArray(data.sites)) {
    const first = repos.map(([name, url]) => [String(name ?? '').trim(), resolve(url, base)]).find(([, url]) => /^https?:/.test(url));
    return { error: 'TVBox 多仓配置暂不支持，请改填其中一个仓库的配置地址' + (first ? `，例如「${first[0] || '仓库'}」：${first[1]}` : '。') };
  }

  const sources = [], seen = new Set();
  let spider = 0, unsearchable = 0;
  for (const site of data.sites) {
    const type = Number(site?.type);
    if (type !== 0 && type !== 1) { spider++; continue; }
    if (String(site.searchable ?? '1') === '0') { unsearchable++; continue; }
    const api = resolve(site.api, base);
    const same = api.replace(/\/+$/, '').toLowerCase();
    if (seen.has(same)) continue; // 同一个接口在配置里出现多次，只留第一个
    seen.add(same);
    sources.push({ key: site.key, name: site.name, api });
  }

  const notes = [];
  if (!sources.length) notes.push(`这份 TVBox 配置里没有网页能用的站点：只支持苹果 CMS 类型（type 0 / 1）的站点${spider ? `，其余 ${spider} 个需要 TVBox 专用爬虫` : ''}。`);
  else if (spider) notes.push(`TVBox 配置里另有 ${spider} 个站点需要 TVBox 专用爬虫，网页里无法使用`);
  if (unsearchable) notes.push(`TVBox 配置里有 ${unsearchable} 个站点不支持搜索`);
  if (sources.length > limit) notes.push(`TVBox 配置里可用的站点超过 ${limit} 个，只导入前 ${limit} 个`);
  return { sources, notes };
}

function isTVBox(data) {
  return !!data && typeof data === 'object' && !Array.isArray(data) && !('sources' in data) &&
    (Array.isArray(data.sites) || Array.isArray(data.urls) || Array.isArray(data.storeHouse));
}

/** 相对路径按订阅地址解析；解析不了的原样返回，由 parseSubscription 报"接口地址不正确"。 */
function resolve(raw, base) {
  const value = String(raw ?? '').trim();
  try { return new URL(value, base).href; } catch { return value; }
}

/** 加密（十六进制）、Base64 编码，或藏在图片末尾（"**" 后面跟 Base64）的配置。 */
function looksEncoded(text) {
  const body = String(text ?? '').trim();
  if (/\*\*[A-Za-z0-9+/=]{40,}$/.test(body)) return true;
  return body.length >= 64 && (/^[0-9a-f]+$/i.test(body) || /^[A-Za-z0-9+/=_\-\s]+$/.test(body));
}

/**
 * TVBox 配置常常不是严格的 JSON：带 // 和 /* *\/ 注释、末尾多余的逗号、开头的 BOM。
 * 去掉这些再解析，字符串里的内容（比如地址里的 //）保持不变。解析失败返回 undefined。
 */
export function parseLoose(text) {
  const source = String(text ?? '').replace(/^﻿/, '');
  let out = '';
  for (let i = 0; i < source.length; i++) {
    const ch = source[i];
    if (ch === '"') {
      let end = i + 1;
      while (end < source.length && source[end] !== '"') end += source[end] === '\\' ? 2 : 1;
      out += source.slice(i, end + 1);
      i = end;
    } else if (ch === '/' && source[i + 1] === '/') {
      while (i < source.length && source[i] !== '\n') i++;
      out += '\n';
    } else if (ch === '/' && source[i + 1] === '*') {
      const end = source.indexOf('*/', i + 2);
      i = end < 0 ? source.length : end + 1;
    } else if (ch === '}' || ch === ']') {
      out = out.replace(/,\s*$/, '') + ch; // 末尾多余的逗号（中间隔着注释也算）
    } else {
      out += ch;
    }
  }
  try { return JSON.parse(out); } catch { return undefined; }
}
