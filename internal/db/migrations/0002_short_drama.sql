-- 短剧：资料来自红果短剧官网（hongguoduanju.com），和电影、剧集放在同一张 media 表里，
-- 这样想看、播放进度、片单、短评这些挂在 media.id 上的功能短剧都能直接用。
--
-- 短剧会比其他作品多得多（几十万部），所以凡是要扫 media 的查询都按类型分开走索引：
-- 长剧的后台巡检、站点地图只看 media_long_idx 覆盖的行；短剧频道按热度、按上架顺序各有一个索引。
ALTER TABLE media DROP CONSTRAINT IF EXISTS media_media_type_check;
ALTER TABLE media ADD CONSTRAINT media_media_type_check CHECK (media_type IN ('movie', 'tv', 'show', 'anime', 'short'));

ALTER TABLE media
    ADD COLUMN hongguo_id     text   NOT NULL DEFAULT '',  -- 红果的 series_id
    ADD COLUMN rating_hongguo real   NOT NULL DEFAULT 0,   -- 红果评分，10 分制
    ADD COLUMN heat           bigint NOT NULL DEFAULT 0;   -- 红果热度，短剧频道按它排序

CREATE UNIQUE INDEX media_hongguo_id_key ON media (hongguo_id) WHERE hongguo_id <> '';

-- 长剧（电影、剧集、综艺、动漫）：查询里写了 media_type <> 'short' 时只读这部分行，不碰几十万部短剧。
CREATE INDEX media_long_idx ON media (id) WHERE media_type <> 'short';

-- 短剧频道：按热度排序，或者按上架顺序（红果的 series_id 随时间递增，位数相同，按文本排就是按时间排）。
CREATE INDEX media_short_heat_idx ON media (heat DESC, id DESC) WHERE media_type = 'short' AND NOT hidden;

-- 短剧的后台刷新：只有没同步过的和连载中的要再抓，已完结的不在这个索引里。
CREATE INDEX media_short_sync_idx ON media (metadata_synced_at NULLS FIRST, id)
    WHERE media_type = 'short' AND series_status <> 'ended';

-- 按题材筛选（genres @> ARRAY[...]）和相似推荐（genres && ...）。
CREATE INDEX media_genres_idx ON media USING gin (genres);
