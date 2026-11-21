-- 发现页的排序："最新"按上映日期，"最热"按近期热度。
--
-- release_date：电影是 TMDB 的上映日期；剧集、综艺、动漫每一季单独一行，是这一季的首播日期。
--   TMDB 上没有日期的作品留空，排序时退回按年份。
-- release_checked：已经向 TMDB 查过日期（查到没查到都算），后台补日期时不再重复请求。
-- popularity_tmdb：TMDB 的 popularity，反映最近一段时间的关注度。每天从 TMDB 公开的导出文件更新一次。
ALTER TABLE media
    ADD COLUMN release_date    date,
    ADD COLUMN release_checked boolean NOT NULL DEFAULT false,
    ADD COLUMN popularity_tmdb real    NOT NULL DEFAULT 0;

-- 已经有分集播出日期的剧集，直接取这一季第一集的日期，不用再去问 TMDB。
UPDATE media m SET release_date = u.first_aired, release_checked = true
FROM (SELECT media_id, season, min(air_date) AS first_aired FROM media_units WHERE air_date IS NOT NULL GROUP BY media_id, season) u
WHERE u.media_id = m.id AND u.season = m.season AND m.media_type IN ('tv', 'show', 'anime');

-- 后台补日期：只挑有 TMDB 条目、还没查过的长剧。
CREATE INDEX media_release_pending_idx ON media (id) WHERE media_type <> 'short' AND tmdb_id <> '' AND NOT release_checked;
