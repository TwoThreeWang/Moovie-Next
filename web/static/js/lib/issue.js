// 综艺按"期"选集。资源站的综艺集名非常乱，同一部节目各站写法都不一样：
//   "20261010第1期上"、"加更第1期"、"第1期还有加更"、"第20261010期"（只有日期）、"20261010上"（日期 + 上下）……
// 一部节目里还常有好几条各自编号的版本：正片、加更版、沉浸版、旅行日记、福利篇，每条都有自己的"第 N 期"，
// 另外夹着纯享、彩蛋、回顾特辑这些片段。按顺序编号的话，同一个"第 3 集"在各个源里是不同的内容。
//
// 这里把每个集名识别成"第几期、哪个版本、哪一段"，编码成一个整数当集号，各个源按这个集号对齐：
//   集号 = 期号 × 100 + 版本 × 10 + 段落。先导片是第 0 期。
//   版本：0 正片，1 加更，2 第二个加更（还有加更、特别加更），3 沉浸版，4 日记，5 福利篇，6 会员版。
//   段落：1 整期，2 上，3 中，4 下。
//   例如 302 是第 3 期上，211 是第 2 期加更，344 是第 3 期日记下，2 是先导片上。
// 集号末两位是 00（比如 300）表示"第 3 期，哪一段都行"：详情页还不知道源里有哪几段时用它链到这一期，播放页再换成实际的段落。
import { chineseNumber } from './title.js';
import { parseEpisodeLabel } from './episode.js';

const WHOLE = 1;
const HALVES = { 上: 2, 中: 3, 下: 4 };
const HALF_NAMES = { 2: '上', 3: '中', 4: '下' };

// 版本：识别用的写法和显示的名字。顺序要紧：先认"还有加更"，再认"加更"。
const VARIANTS = [
  { code: 2, name: '加更 2', pattern: /还有加更|特别加更|二次加更|加更\s*[2二]|加更版\s*[2二]/ },
  { code: 1, name: '加更', pattern: /(?:游戏)?加更版?/ },
  { code: 3, name: '沉浸版', pattern: /沉浸版?/ },
  { code: 4, name: '日记', pattern: /(?:旅行)?日记/ },
  { code: 5, name: '福利篇', pattern: /福利篇?/ },
  { code: 6, name: '会员版', pattern: /会员版|会员加长版|加长版|plus版?/i },
];
const VARIANT_NAMES = Object.fromEntries(VARIANTS.map((v) => [v.code, v.name]));

// 不是一期节目的片段：纯享、彩蛋、抢鲜看、回顾特辑、花絮、直拍、宣传片……它们是正片的剪辑或周边，不进选集。
const EXTRA = /纯享|彩蛋|抢鲜|超前|回顾|特辑|花絮|预告|片花|直播|直拍|竖屏|总宣|宣传|专访|片段|精彩|看点|幕后|独家|联动|\bmv\b|reaction|\bcut\b/i;

export const encodeIssue = (issue, variant, half) => issue * 100 + variant * 10 + half;
export const decodeIssue = (episode) => ({ issue: Math.floor(episode / 100), variant: Math.floor((episode % 100) / 10), half: episode % 10 });

/** 选集按钮上的文字（不带期号）："上"、"加更"、"日记 下"；正片整期是"正片"。 */
export function partName(episode) {
  const { variant, half } = decodeIssue(episode);
  const words = [VARIANT_NAMES[variant], HALF_NAMES[half]].filter(Boolean);
  return words.length ? words.join(' ') : '正片';
}

/** 集号对应的完整文字："第 3 期 下"、"第 2 期 加更"、"先导片 上"、"第 5 期"。 */
export function issueLabel(episode) {
  const { issue, variant, half } = decodeIssue(episode);
  const head = issue === 0 ? '先导片' : `第 ${issue} 期`;
  return variant === 0 && half === WHOLE ? head : `${head} ${partName(episode)}`;
}

// 集名开头（或"第……期"里）的日期：20261010、261010、2026-10-10。返回 "20261010"，不是日期返回 ''。
function takeDate(text) {
  const match = /^(?:第\s*)?((?:20)?\d{2})[-./]?(\d{2})[-./]?(\d{2})(?!\d)/.exec(text);
  if (!match) return { date: '', rest: text };
  const year = match[1].length === 2 ? '20' + match[1] : match[1];
  const month = +match[2], day = +match[3];
  if (month < 1 || month > 12 || day < 1 || day > 31) return { date: '', rest: text };
  // "第20261010期"：日期占了期号的位置，去掉后面的"期"
  const rest = text.slice(match[0].length).replace(/^\s*期/, '');
  return { date: year + match[2] + match[3], rest };
}

const SEPARATORS = /[\s·・:：\-_|/,，、]+/g;

/**
 * 识别一个综艺集名。返回 { issue, variant, half, date, other } 或 null（片段、识别不出来）。
 * issue 为 null 表示集名里没有期号（只有日期）；variant、half 为 null 表示集名里没写，要靠别的源推断。
 * other 表示集名里还有认不出来的字（嘉宾名之类）。
 */
export function parseIssueLabel(label) {
  let text = String(label ?? '').normalize('NFKC').replace(/[​-‍﻿]/g, '').replace(/[()（）【】\[\]《》]/g, ' ').trim();
  if (!text || EXTRA.test(text)) return null;
  const { date, rest } = takeDate(text);
  text = rest.replace(SEPARATORS, ' ').trim();

  let issue = null;
  const numbered = /第\s*([0-9]{1,4}|[一二三四五六七八九十百零两]{1,5})\s*期/.exec(text);
  if (numbered) {
    issue = chineseNumber(numbered[1]);
    // 有的站会重复写："第5期第5期下"
    text = text.replaceAll(numbered[0], ' ');
  } else if (/先导/.test(text)) {
    issue = 0;
    text = text.replace(/先导(?:片|篇)?/, ' ');
  }

  let variant = null;
  for (const candidate of VARIANTS) {
    if (candidate.pattern.test(text)) {
      variant = candidate.code;
      text = text.replace(candidate.pattern, ' ');
      break;
    }
  }

  // 有的站把综艺也写成"第1集""01"，电影天堂一天两期时写"20261022-01"
  if (issue === null && /[0-9一二三四五六七八九十]/.test(text)) {
    const number = parseEpisodeLabel(text);
    if (number !== null) {
      issue = number;
      text = text.replace(/第?\s*[0-9一二三四五六七八九十百零两]+\s*[集话話回]?/, ' ');
    }
  }
  if (issue === null && !date) return null;

  text = text.replace(SEPARATORS, '');
  let half = null, other = false;
  const halfMatch = /^([上中下])(?:篇|集|部)?$/.exec(text);
  if (halfMatch) half = HALVES[halfMatch[1]];
  else if (text) other = true;
  // 只有日期、后面又是认不出来的字（"第20261010期捣蛋""20260702地球团集结前"）：多半是片段
  if (issue === null && other) return null;
  // 写了上下但没写版本，就是正片
  if (variant === null && half !== null) variant = 0;
  if (issue !== null) {
    variant ??= 0;
    half ??= WHOLE;
  }
  return { issue, variant, half, date, other };
}

function vote(table, key, value) {
  let counts = table.get(key);
  if (!counts) table.set(key, counts = new Map());
  counts.set(value, (counts.get(value) ?? 0) + 1);
}

// 票数最多的；平票时取小的（上比下早，正片比加更早）。
function winner(counts, accept = () => true) {
  let best = null, most = 0;
  for (const [value, count] of counts ?? []) {
    if (!accept(value)) continue;
    if (count > most || (count === most && value < best)) { best = value; most = count; }
  }
  return best;
}

// 前后相邻的日期："20261011" → ["20261010", "20261012"]。
function neighborDates(date) {
  const day = Date.UTC(+date.slice(0, 4), +date.slice(4, 6) - 1, +date.slice(6, 8));
  return [-1, 1].map((offset) => new Date(day + offset * 86400000).toISOString().slice(0, 10).replaceAll('-', ''));
}

// 只有日期的集名：在别的源同一天的集里找版本、段落都对得上的。段落对不上时，只借期号。
// 各站标的日期常差一天（同一个"第1期下"，有的标 10 号，有的标 11 号），同一天找不到时，版本和段落都写明了的再看前后一天。
function inferFromDate(item, byDate) {
  const counts = byDate.get(item.date);
  const fits = (code) => {
    const known = decodeIssue(code);
    return (item.variant === null || known.variant === item.variant) && (item.half === null || known.half === item.half);
  };
  const exact = winner(counts, fits);
  if (exact !== null) return exact;
  if (!counts && item.variant !== null && item.half !== null) {
    for (const date of neighborDates(item.date)) {
      const near = winner(byDate.get(date), fits);
      if (near !== null) return near;
    }
  }
  if (item.variant === null) return null;
  // 一天播了两期整期（别的源写"第1期""第2期"），这个源却写成"上""下"：上是当天的第一期，下是最后一期
  if (item.half !== null && item.half !== WHOLE) {
    const sameDay = [...new Set([...(counts?.keys() ?? [])]
      .map(decodeIssue).filter((known) => known.variant === item.variant && known.half === WHOLE).map((known) => known.issue))].sort((a, b) => a - b);
    if (sameDay.length >= 2) {
      const issue = item.half === HALVES.上 ? sameDay[0] : item.half === HALVES.下 ? sameDay.at(-1) : sameDay[1];
      return encodeIssue(issue, item.variant, WHOLE);
    }
  }
  const sameVariant = winner(counts, (code) => decodeIssue(code).variant === item.variant);
  return sameVariant === null ? null : encodeIssue(decodeIssue(sameVariant).issue, item.variant, item.half ?? WHOLE);
}

/**
 * 给所有源的所有线路分配综艺集号：每一集写上 episode.issue（识别不出来时为 null）。
 * lines 是各个源的线路合在一起的数组，每条线路 { episodes: [{ label }] }。
 *
 * 只有日期的集名（"第20261010期"、"20261010上"）靠其他源对齐：别的源里同一天写的是"第1期上"，那它就是第 1 期上。
 * 所有源都没有期号时，按日期先后编期号（每天一期的节目就是这样）。
 * 同一条线路里重复的集号只保留第一个。
 */
export function assignIssues(lines) {
  const parsed = lines.map((line) => line.episodes.map((episode) => parseIssueLabel(episode.label)));
  const byDate = new Map();
  for (const list of parsed) {
    for (const item of list) {
      if (item?.date && item.issue !== null && !item.other) vote(byDate, item.date, encodeIssue(item.issue, item.variant, item.half));
    }
  }
  // 兜底：没有任何期号时，按日期排出期号
  let dateRank = null;
  if (!parsed.some((list) => list.some((item) => item && item.issue !== null))) {
    const dates = [...new Set(parsed.flat().filter((item) => item?.date).map((item) => item.date))].sort();
    dateRank = new Map(dates.map((date, index) => [date, index + 1]));
  }

  lines.forEach((line, lineIndex) => {
    const used = new Set();
    line.episodes.forEach((episode, index) => {
      const item = parsed[lineIndex][index];
      let code = null;
      if (item && item.issue !== null) code = encodeIssue(item.issue, item.variant, item.half);
      else if (item && dateRank) code = encodeIssue(dateRank.get(item.date), item.variant ?? 0, item.half ?? WHOLE);
      else if (item) code = inferFromDate(item, byDate);
      if (code !== null && used.has(code)) code = null;
      if (code !== null) used.add(code);
      episode.issue = code;
    });
  });
}

/**
 * 把集号按期分组：[{ issue, parts: [集号…] }]，期和段落都从小到大。
 * flat 为 true 表示每一期都只有正片整期，选集可以像剧集一样排成一排数字。
 */
export function groupIssues(codes) {
  const groups = new Map();
  for (const code of [...new Set(codes)].sort((a, b) => a - b)) {
    const { issue } = decodeIssue(code);
    if (!groups.has(issue)) groups.set(issue, []);
    groups.get(issue).push(code);
  }
  const list = [...groups].map(([issue, parts]) => ({ issue, parts }));
  return { list, flat: list.every((group) => group.parts.length === 1 && group.parts[0] % 100 === WHOLE) };
}

/** "第 3 期，哪一段都行"（末两位是 00）换成实际播放的段落：先找正片，没有再用这一期的第一段。 */
export function resolveIssue(episode, available) {
  if (episode % 100 !== 0) return episode;
  const { issue } = decodeIssue(episode);
  const parts = available.filter((code) => decodeIssue(code).issue === issue).sort((a, b) => a - b);
  return parts.find((code) => decodeIssue(code).variant === 0) ?? parts[0] ?? episode;
}

/** 源里最新的一期正片的期号，没有返回 0。 */
export function latestIssue(codes) {
  return codes.reduce((latest, code) => {
    const { issue, variant } = decodeIssue(code);
    return variant === 0 ? Math.max(latest, issue) : latest;
  }, 0);
}
