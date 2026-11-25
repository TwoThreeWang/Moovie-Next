-- douban_listed_at：最近一次从豆瓣的列表（热门榜、用户的标记列表）里顺手拿到这部作品评分的时间。
-- 近期上映的电影原本每周重抓一次详情，主要就是为了更新评分；评分已经从列表里拿到了，详情可以隔得更久再抓。
ALTER TABLE media ADD COLUMN douban_listed_at timestamptz;
