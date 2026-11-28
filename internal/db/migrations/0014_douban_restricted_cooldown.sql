-- douban_restricted_at / douban_restricted_until / douban_restricted_strikes：
-- 豆瓣返回 403 need_permission（code 1000）的条目冷却期。
-- 首次先冷却 1 小时观察；确认同类接口期间正常成功过再按 1 天 → 7 天 → 30 天递增。
ALTER TABLE media
  ADD COLUMN douban_restricted_at timestamptz,
  ADD COLUMN douban_restricted_until timestamptz,
  ADD COLUMN douban_restricted_strikes integer NOT NULL DEFAULT 0;
