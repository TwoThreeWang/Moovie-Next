-- 自托管的广告，在管理后台设置。slot 是广告位：目前只有 sidebar（播放页弹幕下方、详情页信息下方），
-- 同一个广告位可以有多条，按 position 从小到大竖着往下排，每条高 60px。
-- kind 为 image 时显示 image_url 指向的图片（text 作为图片的说明文字），为 text 时显示 text。
-- link 为空时只展示、不能点。
CREATE TABLE ads (
    id         bigserial PRIMARY KEY,
    slot       text NOT NULL DEFAULT 'sidebar',
    position   integer NOT NULL DEFAULT 0,
    enabled    boolean NOT NULL DEFAULT true,
    kind       text NOT NULL DEFAULT 'text' CHECK (kind IN ('image', 'text')),
    image_url  text NOT NULL DEFAULT '',
    text       text NOT NULL DEFAULT '',
    link       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ads_slot_idx ON ads (slot, position, id);
