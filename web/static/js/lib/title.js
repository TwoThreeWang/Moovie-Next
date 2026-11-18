// 片名处理。规则和服务器端 internal/media/title.go 保持一致，这样两边算出的结果相同。

/** 把片名规范化成只含字母和数字的小写形式，用于比较。 */
export function normalizeTitle(value) {
  return String(value ?? '').normalize('NFKC').toLowerCase().replace(/[^\p{L}\p{N}\p{M}]/gu, '');
}

const DIGITS = { 零: 0, 一: 1, 二: 2, 两: 2, 三: 3, 四: 4, 五: 5, 六: 6, 七: 7, 八: 8, 九: 9 };

/** 把 "12"、"二"、"十二"、"二十一" 转成整数，无法识别时返回 0。 */
export function chineseNumber(text) {
  if (/^\d+$/.test(text)) return parseInt(text, 10);
  let total = 0, current = 0;
  for (const ch of text) {
    if (ch === '百') { total += (current || 1) * 100; current = 0; }
    else if (ch === '十') { total += (current || 1) * 10; current = 0; }
    else if (ch in DIGITS) current = DIGITS[ch];
    else return 0;
  }
  return total + current;
}

const SEASON_PATTERNS = [
  /第\s*([0-9一二三四五六七八九十百零两]+)\s*[季部]/,
  /\bseason\s*([0-9]+)\b/i,
  /\bs([0-9]{1,2})\b/i,
];

/** 从片名里拆出季数。"雾港 第二季" → { base: "雾港", season: 2 }；没有季数标记时 season 为 1。 */
export function splitSeason(title) {
  title = String(title ?? '');
  for (const pattern of SEASON_PATTERNS) {
    const match = pattern.exec(title);
    if (!match) continue;
    const season = chineseNumber(match[1]);
    if (season < 1 || season > 99) continue;
    const base = (title.slice(0, match.index) + ' ' + title.slice(match.index + match[0].length)).trim().replace(/[\s:：\-—·]+$/, '');
    if (!base) return { base: title.trim(), season: 1 };
    return { base, season };
  }
  return { base: title.trim(), season: 1 };
}

/** 去掉文本里的 HTML 标签并还原常见的实体。资源站返回的文字一律先过这一步。 */
export function plainText(value) {
  return String(value ?? '')
    .replace(/<[^>]*>/g, ' ')
    .replace(/&nbsp;/g, ' ').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#0?39;/g, "'").replace(/&amp;/g, '&')
    .replace(/\s+/g, ' ').trim();
}
