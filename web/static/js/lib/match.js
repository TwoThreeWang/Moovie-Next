// 在资源站的搜索结果里，找出哪些条目是我们要的那部作品。
import { normalizeTitle, splitSeason } from './title.js';

/**
 * 给一条搜索结果打分。0 表示不是这部作品。
 * media: { title, original_title, aliases, year, type, season, douban_id }
 */
export function scoreItem(media, item) {
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
