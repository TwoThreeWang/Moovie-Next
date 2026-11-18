// 从资源站的集名里识别集数。各个站的写法很乱："第05集"、"05"、"EP5"、"第五话"、"20261108期"……
import { chineseNumber } from './title.js';

const PATTERNS = [
  /第\s*([0-9]{1,4})\s*[集话話期回]/,
  /第\s*([一二三四五六七八九十百零两]{1,5})\s*[集话話期回]/,
  /\bep?\.?\s*([0-9]{1,4})\b/i,
  /^\s*([0-9]{1,4})\s*$/,
  /^\s*([0-9]{1,4})\s*(?:集|话|話)?\s*(?:[^0-9].*)?$/,
];

/** 识别单个集名里的集数，识别不出来返回 null。 */
export function parseEpisodeLabel(label) {
  const text = String(label ?? '').normalize('NFKC').trim();
  if (!text) return null;
  // 综艺常用日期当集名（20261108、2026-11-08），这不是集数。
  if (/^\d{6,8}/.test(text) || /^\d{4}[-./]\d{1,2}[-./]\d{1,2}/.test(text)) return null;
  for (const pattern of PATTERNS) {
    const match = pattern.exec(text);
    if (!match) continue;
    const number = chineseNumber(match[1]);
    if (number >= 1 && number <= 5000) return number;
  }
  return null;
}

/**
 * 给一条线路的所有集分配集数。
 * 能识别出来、并且不重复的集名用识别结果；否则按顺序编号。
 * 如果一半以上识别不出来，说明这条线路的集名不可信，全部按顺序编号。
 */
export function numberEpisodes(labels) {
  const parsed = labels.map(parseEpisodeLabel);
  const recognized = parsed.filter((n) => n !== null).length;
  if (recognized * 2 < labels.length) return labels.map((_, index) => index + 1);
  const used = new Set();
  return parsed.map((number, index) => {
    let value = number;
    if (value === null || used.has(value)) {
      value = index + 1;
      while (used.has(value)) value += 1;
    }
    used.add(value);
    return value;
  });
}
