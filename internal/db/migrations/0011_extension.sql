-- 浏览器插件（extension/ 目录，见 docs/extension.md）。

-- 插件的连接凭证。插件不用网站的登录 Cookie，在"连接插件"页面拿到一个随机凭证，服务器只存它的哈希。
-- session_epoch 是签发时账号的密码版本：改密码、被封禁后旧凭证一起失效，和登录凭证一样。
CREATE TABLE extension_tokens (
    id            bigserial PRIMARY KEY,
    user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    bytea NOT NULL UNIQUE,
    session_epoch integer NOT NULL,
    user_agent    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX extension_tokens_user_idx ON extension_tokens (user_id);

-- 插件最近一次完整同步完豆瓣标记（没有失败的条目）的时间。最近三天同步过的，服务器每天的自动同步就跳过他。
ALTER TABLE users ADD COLUMN douban_ext_synced_at timestamptz;

-- 谁用插件补全过哪部作品的豆瓣资料：主页显示贡献数；有人上传假资料时，可以按人找出他碰过的作品。
CREATE TABLE media_contributions (
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    media_id   bigint NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, media_id)
);
CREATE INDEX media_contributions_media_idx ON media_contributions (media_id);

-- 用户在本站标记的想看、看过，等插件取走写回豆瓣（双向同步，插件里默认关闭）。只记连接过插件的用户，保留 30 天。
CREATE TABLE douban_outbox (
    id         bigserial PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    douban_id  text NOT NULL,
    status     text NOT NULL CHECK (status IN ('wish', 'watched')),
    rating     smallint NOT NULL DEFAULT 0 CHECK (rating BETWEEN 0 AND 5),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX douban_outbox_user_idx ON douban_outbox (user_id, id);
CREATE INDEX douban_outbox_created_idx ON douban_outbox (created_at);
