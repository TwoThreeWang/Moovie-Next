// 在资源站的搜索结果里，找出哪些条目是我们要的那部作品。
import { normalizeTitle, splitSeason } from './title.js';

// 不完整的版本：预告片、花絮这类，和影院偷拍的枪版（TC、TS、CAM、抢先版）。资源站常把它们标在片名、备注或分类里，
// 还经常填上正片的豆瓣 ID，所以不能只靠片名排除。
// 英文缩写只按完整的词匹配（前后不是字母），免得误伤 HDTV、"tsm3u8" 或片名里碰巧带这几个字母的。
const TRAILER = /预告|片花|花絮|trailer/i;
const CAM = /枪版|抢先版|(?<![a-z])(?:hd)?(?:tc|ts|cam)(?![a-z])/i;

/**
 * 这条搜索结果是不是不完整的版本：'trailer'（预告片、花絮）、'cam'（枪版）或 ''（正常）。
 * 只看片名、备注、分类；正片里夹带的预告线路、预告单集在解析播放列表时就去掉了（见 cms.js）。
 */
export function incompleteKind(item) {
  const fields = [item.name, item.remarks, item.typeName].filter(Boolean);
  if (fields.some((text) => TRAILER.test(text))) return 'trailer';
  if (fields.some((text) => CAM.test(text))) return 'cam';
  return '';
}

/** 播放列表里的一集或一条线路是不是预告、花絮（有的资源站把它们夹在正片里）。 */
export function isExtra(label) {
  return TRAILER.test(String(label ?? ''));
}

/**
 * 给一条搜索结果打分。0 表示不是这部作品。预告片、枪版一律算 0。
 * media: { title, original_title, aliases, year, type, season, douban_id }
 */
export function scoreItem(media, item) {
  return incompleteKind(item) ? 0 : rawScore(media, item);
}

/**
 * 如果这个源里唯一像是这部作品的，只有预告片或枪版，返回 'trailer' 或 'cam'，用来告诉用户"有，但隐藏了"。
 */
export function hiddenMatch(media, items) {
  for (const item of items) {
    const kind = incompleteKind(item);
    if (kind && rawScore(media, item) > 59) return kind;
  }
  return '';
}

function rawScore(media, item) {
  if (media.douban_id && item.doubanId && item.doubanId === media.douban_id) return 100;
  const series = media.type && media.type !== 'movie';
  if (item.type && media.type && (item.type === 'movie') !== (media.type === 'movie')) return 0;

  const itemSplit = splitSeason(item.name);
  const itemFull = normalizeTitle(item.name);
  const itemBase = normalizeTitle(itemSplit.base);
  const names = [media.title, media.original_title, ...(media.aliases ?? [])].filter(Boolean);
  let score = 0;
  for (const name of names) {
    const split = splitSeason(name);
    const full = normalizeTitle(name), base = normalizeTitle(split.base);
    if (!full) continue;
    if (itemFull === full) { score = Math.max(score, 90); continue; }
    // 片名去掉季数后相同，再看季数是否对得上。
    if (series && base && itemBase === base) {
      const wanted = media.season || split.season || 1;
      if (itemSplit.season === wanted) score = Math.max(score, 85);
    }
  }
  if (!score) return 0;
  if (media.year && item.year) {
    // 同名作品靠年份区分。分集作品的后几季年份和资源站标的可能差一点，放宽到 1 年。
    if (Math.abs(media.year - item.year) > 1) return series && score >= 85 && media.season > 1 ? score - 25 : 0;
    score += 5;
  }
  return score;
}

/** 从一个源的搜索结果里挑出最匹配的一条；都不匹配时返回 null。 */
export function bestMatch(media, items) {
  let best = null, bestScore = 59;
  for (const item of items) {
    const score = scoreItem(media, item);
    if (score > bestScore) { best = item; bestScore = score; }
  }
  return best ? { item: best, score: bestScore } : null;
}

/** 在一条线路里找到某一集。电影直接取第一个。 */
export function findEpisode(line, episode, isMovie) {
  if (!line?.episodes?.length) return null;
  if (isMovie) return line.episodes[0];
  return line.episodes.find((e) => e.number === episode) ?? null;
}

/** 搜索用的关键词：去掉"第二季"这类后缀，资源站按包含关系搜索，这样各季都能搜到。 */
export function searchKeyword(media) {
  return splitSeason(media.title).base || media.title;
}

/**
 * 没有元数据的作品，原来的源找不到时，在其他源里找同一部。比 bestMatch 严格：
 * 片名规范化后必须完全相同，类型不能冲突（电影 / 分集）。
 * 两边都写了年份且相同 → sure: true，可以自动换过去；年份不同 → 不是同一部；
 * 有一边没写年份 → sure: false，只列出来让用户自己确认。
 */
export function sameWork(media, items) {
  const title = normalizeTitle(media.title);
  if (!title) return null;
  let unsure = null;
  for (const item of items) {
    if (incompleteKind(item)) continue;
    if (normalizeTitle(item.name) !== title) continue;
    if (item.type && media.type && (item.type === 'movie') !== (media.type === 'movie')) continue;
    if (media.year && item.year) {
      if (+media.year === +item.year) return { item, sure: true };
      continue;
    }
    unsure ??= { item, sure: false };
  }
  return unsure;
}
