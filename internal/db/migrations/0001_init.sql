-- 影牛的表结构。
--
-- 原则：每张表、每一列都必须是"别处推不出来、又确实有人读"的数据。
-- 能从别的表算出来的（月度报告、推荐结果、相关片单），算出来就好，不存；
-- 只写不读的（抓取记录、字段来源、生成向量用的文本），不存。
-- 约定：这里没有任何和播放源有关的表或列；服务器不保存、也不访问资源站。

-- ---------- 用户 ----------
CREATE TABLE users (
    id                 bigserial PRIMARY KEY,
    email              text NOT NULL UNIQUE,
    username           text NOT NULL UNIQUE,
    password_hash      text NOT NULL,
    role               text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    avatar             text NOT NULL DEFAULT '🎬',
    is_public          boolean NOT NULL DEFAULT false,
    douban_user_id     text NOT NULL DEFAULT '',
    ad_skip_enabled    boolean NOT NULL DEFAULT true,
    -- 改密码、被封禁时加一，旧的登录凭证随之失效
    session_epoch      integer NOT NULL DEFAULT 0,
    -- 账号限制：mute 不能发短评、回复和弹幕；ban 不能登录。restricted_until 为空表示永久
    restriction        text NOT NULL DEFAULT '' CHECK (restriction IN ('', 'mute', 'ban')),
    restricted_until   timestamptz,
    restriction_reason text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX users_created_idx ON users (created_at);

CREATE TABLE user_follows (
    follower_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    followee_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (follower_id, followee_id),
    CHECK (follower_id <> followee_id)
);
CREATE INDEX user_follows_followee_idx ON user_follows (followee_id);

-- 账号的钥匙圈：设备主密钥和订阅地址，在浏览器里用登录密码派生的密钥加密后上传。
-- 服务器只有密文和随机盐；不保存密码，所以解不开。登录同一个账号的设备据此自动同步。
CREATE TABLE keyrings (
    user_id    bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    salt       bytea NOT NULL CHECK (octet_length(salt) = 16),
    iterations integer NOT NULL CHECK (iterations BETWEEN 100000 AND 5000000),
    blob       bytea NOT NULL CHECK (octet_length(blob) BETWEEN 29 AND 4096),
    version    bigint NOT NULL DEFAULT 1,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- ---------- 作品资料 ----------
-- 一行是一部作品。豆瓣把每一季当成独立条目，所以一季就是一行，season 记录它是第几季。
-- 各来源的 ID 直接是列：来源只有这几个，而且只按作品读写，不按外部 ID 反查。
-- TMDB、IMDb 里一部剧的所有季共用一个 ID，所以它们不唯一；豆瓣 ID 唯一。
CREATE TABLE media (
    id                 bigserial PRIMARY KEY,
    media_type         text NOT NULL DEFAULT 'movie' CHECK (media_type IN ('movie', 'tv', 'show', 'anime')),
    douban_id          text NOT NULL DEFAULT '',
    tmdb_id            text NOT NULL DEFAULT '',   -- 带类型前缀，如 "movie/603"、"tv/1399"
    imdb_id            text NOT NULL DEFAULT '',
    wikidata_id        text NOT NULL DEFAULT '',
    title              text NOT NULL DEFAULT '',
    original_title     text NOT NULL DEFAULT '',
    -- 去掉"第二季"这类后缀后的规范化片名，用来找同系列的其他季
    series_key         text NOT NULL DEFAULT '',
    year               integer NOT NULL DEFAULT 0,
    season             integer NOT NULL DEFAULT 1,
    episode_count      integer NOT NULL DEFAULT 0,
    poster             text NOT NULL DEFAULT '',
    backdrops          text[] NOT NULL DEFAULT '{}',
    summary            text NOT NULL DEFAULT '',
    genres             text[] NOT NULL DEFAULT '{}',
    countries          text[] NOT NULL DEFAULT '{}',
    directors          text[] NOT NULL DEFAULT '{}',
    actors             text[] NOT NULL DEFAULT '{}',
    duration_minutes   integer NOT NULL DEFAULT 0,
    rating_douban      real NOT NULL DEFAULT 0,
    rating_tmdb        real NOT NULL DEFAULT 0,
    votes_tmdb         integer NOT NULL DEFAULT 0,
    series_status      text NOT NULL DEFAULT '',
    hidden             boolean NOT NULL DEFAULT false,
    metadata_status    text NOT NULL DEFAULT 'partial' CHECK (metadata_status IN ('partial', 'complete', 'not_found')),
    -- 最近一次从豆瓣同步成功（或确认豆瓣没有这个条目）的时间
    metadata_synced_at timestamptz,
    -- 最近一次从 Wikidata、TMDB 补充资料成功的时间。早于 metadata_synced_at 表示这一轮还没补
    enriched_at        timestamptz,
    -- AI 根据资料写的一段语义描述，向量就是用它算的。semantic_hash 是写它时所用资料的哈希：
    -- 资料没变就不再调用 AI；描述变了，向量随之清空重算
    semantic_text      text NOT NULL DEFAULT '',
    semantic_hash      text NOT NULL DEFAULT '',
    embedding          vector(768),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX media_douban_id_key ON media (douban_id) WHERE douban_id <> '';
CREATE INDEX media_series_key_idx ON media (series_key) WHERE series_key <> '';
CREATE INDEX media_type_rating_idx ON media (media_type, rating_douban DESC) WHERE NOT hidden;
CREATE INDEX media_embedding_idx ON media USING hnsw (embedding vector_cosine_ops);

-- 别名：片名、原名、又名，用来匹配资源站的片名和搜索
CREATE TABLE media_aliases (
    media_id   bigint NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    alias      text NOT NULL,
    normalized text NOT NULL,
    PRIMARY KEY (media_id, normalized)
);
CREATE INDEX media_aliases_normalized_idx ON media_aliases (normalized);
CREATE INDEX media_aliases_trgm_idx ON media_aliases USING gin (normalized gin_trgm_ops);

-- 分集信息，只用于展示（集名、播出日期、时长）和"今日更新"。播放进度不依赖这张表。
CREATE TABLE media_units (
    media_id        bigint NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    season          integer NOT NULL,
    episode         integer NOT NULL,
    title           text NOT NULL DEFAULT '',
    air_date        date,
    runtime_minutes integer NOT NULL DEFAULT 0,
    PRIMARY KEY (media_id, season, episode)
);
CREATE INDEX media_units_air_date_idx ON media_units (air_date);

-- 热门榜单（来自豆瓣，每 6 小时整批替换）
CREATE TABLE chart_items (
    kind     text NOT NULL,
    rank     integer NOT NULL,
    media_id bigint NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    PRIMARY KEY (kind, rank)
);

-- ---------- 想看 / 看过 / 短评 ----------
CREATE TABLE user_movies (
    id         bigserial PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    media_id   bigint NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    status     text NOT NULL CHECK (status IN ('wish', 'watched')),
    rating     smallint NOT NULL DEFAULT 0 CHECK (rating BETWEEN 0 AND 5),
    comment    text NOT NULL DEFAULT '' CHECK (char_length(comment) <= 2000),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, media_id)
);
CREATE INDEX user_movies_user_idx ON user_movies (user_id, status, updated_at DESC);
CREATE INDEX user_movies_media_idx ON user_movies (media_id, updated_at DESC) WHERE comment <> '';
CREATE INDEX user_movies_created_idx ON user_movies (created_at);

CREATE TABLE comment_likes (
    user_movie_id bigint NOT NULL REFERENCES user_movies(id) ON DELETE CASCADE,
    user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_movie_id, user_id)
);

CREATE TABLE comment_replies (
    id            bigserial PRIMARY KEY,
    user_movie_id bigint NOT NULL REFERENCES user_movies(id) ON DELETE CASCADE,
    user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content       text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 300),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX comment_replies_comment_idx ON comment_replies (user_movie_id, id);
CREATE INDEX comment_replies_created_idx ON comment_replies (created_at);

-- ---------- 片单 ----------
-- 相关片单不存：按共同收录的作品实时算。
CREATE TABLE collections (
    id            bigserial PRIMARY KEY,
    owner_user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug          text NOT NULL UNIQUE,
    title         text NOT NULL,
    description   text NOT NULL DEFAULT '',
    featured      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE collection_items (
    collection_id bigint NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    media_id      bigint NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    position      integer NOT NULL DEFAULT 0,
    note          text NOT NULL DEFAULT '',
    PRIMARY KEY (collection_id, media_id)
);
CREATE INDEX collection_items_media_idx ON collection_items (media_id);

-- ---------- 播放进度 ----------
-- 只按作品和集数记录，不含任何源信息。电影固定用 season=1, episode=1。
-- 浏览器只上报，页面由服务器读库渲染，所以没有同步用的版本号和软删除。
CREATE TABLE playback_positions (
    id               bigserial PRIMARY KEY,
    user_id          bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    media_id         bigint NOT NULL REFERENCES media(id) ON DELETE CASCADE,
    season           integer NOT NULL DEFAULT 1,
    episode          integer NOT NULL DEFAULT 1,
    position_seconds double precision NOT NULL DEFAULT 0,
    duration_seconds double precision NOT NULL DEFAULT 0,
    completed        boolean NOT NULL DEFAULT false,
    activity_at      timestamptz NOT NULL,
    UNIQUE (user_id, media_id, season, episode)
);
CREATE INDEX playback_positions_recent_idx ON playback_positions (user_id, activity_at DESC);
-- 热门榜统计最近 7 天全站在看什么
CREATE INDEX playback_positions_activity_idx ON playback_positions (activity_at);

-- 哪些天看过东西：连续观影天数、每天的观影人数。进度表只留最后一次，算不出这些。
CREATE TABLE watch_days (
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day     date NOT NULL,
    PRIMARY KEY (user_id, day)
);
CREATE INDEX watch_days_day_idx ON watch_days (day);

-- 没有元数据的作品的观看历史：端到端加密，服务器解不开
CREATE SEQUENCE private_history_version_seq;
CREATE TABLE private_history (
    user_id        bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    rid            bytea NOT NULL CHECK (octet_length(rid) = 32),
    server_version bigint NOT NULL DEFAULT nextval('private_history_version_seq'),
    blob           bytea CHECK (blob IS NULL OR octet_length(blob) <= 1024),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, rid)
);
CREATE INDEX private_history_sync_idx ON private_history (user_id, server_version);

-- ---------- 弹幕 ----------
-- 有元数据的作品按 media_id + 季 + 集归档；没有的按"片名|S01|E003"归档
CREATE TABLE danmakus (
    id           bigserial PRIMARY KEY,
    media_id     bigint REFERENCES media(id) ON DELETE CASCADE,
    season       integer NOT NULL DEFAULT 1,
    episode      integer NOT NULL DEFAULT 1,
    title_key    text NOT NULL DEFAULT '',
    time_seconds double precision NOT NULL CHECK (time_seconds >= 0),
    body         text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 50),
    mode         smallint NOT NULL DEFAULT 0 CHECK (mode IN (0, 1, 2)),
    color        text NOT NULL DEFAULT '#FFFFFF' CHECK (color ~ '^#[0-9A-F]{6}$'),
    user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deleted      boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (media_id IS NOT NULL OR title_key <> '')
);
CREATE INDEX danmakus_media_idx ON danmakus (media_id, season, episode) WHERE media_id IS NOT NULL;
CREATE INDEX danmakus_title_idx ON danmakus (title_key) WHERE title_key <> '';
CREATE INDEX danmakus_created_idx ON danmakus (created_at);

-- ---------- 广告指纹 ----------
-- 只有指纹和票数。不存源、影片、集数，也不存谁投了什么票。
CREATE TABLE ad_fingerprints (
    fingerprint   bytea PRIMARY KEY CHECK (octet_length(fingerprint) = 32),
    confirm_count integer NOT NULL DEFAULT 0 CHECK (confirm_count >= 0),
    reject_count  integer NOT NULL DEFAULT 0 CHECK (reject_count >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ad_fingerprints_updated_idx ON ad_fingerprints (updated_at DESC);

-- ---------- 反馈和通知 ----------
CREATE TABLE feedbacks (
    id         bigserial PRIMARY KEY,
    user_id    bigint REFERENCES users(id) ON DELETE SET NULL,
    kind       text NOT NULL CHECK (kind IN ('bug', 'request', 'suggestion', 'dmca')),
    content    text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 5000),
    page_url   text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'rejected')),
    reply      text NOT NULL DEFAULT '',
    replied_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- 通知。点赞、回复引用原内容；管理员删了的内容已经不在了，所以把摘要和原因存在 body、reason 里。
CREATE TABLE notifications (
    id            bigserial PRIMARY KEY,
    user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind          text NOT NULL CHECK (kind IN ('like', 'reply', 'follow', 'feedback', 'removed', 'muted')),
    user_movie_id bigint REFERENCES user_movies(id) ON DELETE CASCADE,
    reply_id      bigint REFERENCES comment_replies(id) ON DELETE CASCADE,
    feedback_id   bigint REFERENCES feedbacks(id) ON DELETE CASCADE,
    media_id      bigint REFERENCES media(id) ON DELETE SET NULL,
    body          text NOT NULL DEFAULT '',
    reason        text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    read_at       timestamptz
);
CREATE INDEX notifications_user_idx ON notifications (user_id, id DESC);
CREATE UNIQUE INDEX notifications_like_key ON notifications (user_id, actor_id, user_movie_id) WHERE kind = 'like';
CREATE UNIQUE INDEX notifications_follow_key ON notifications (user_id, actor_id) WHERE kind = 'follow';

-- ---------- 运营 ----------
-- 屏蔽词：片名命中后，作品不在站内展示，也不上热搜
CREATE TABLE blocked_keywords (
    id         bigserial PRIMARY KEY,
    kind       text NOT NULL CHECK (kind IN ('copyright', 'nsfw')),
    keyword    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (kind, keyword)
);

-- 每天每个词被搜了几次，只用来生成热搜榜和后台的搜索趋势。不记录是谁搜的，也不保存每一次搜索。保留 31 天。
-- 只记作品库的搜索；浏览器直接查用户自己播放源的搜索，服务器看不到，也不记录。
CREATE TABLE search_counts (
    day        date NOT NULL,
    normalized text NOT NULL,
    keyword    text NOT NULL CHECK (char_length(keyword) BETWEEN 1 AND 40), -- 当天第一次出现的写法
    count      integer NOT NULL CHECK (count > 0),
    PRIMARY KEY (day, normalized)
);

-- 发信记录：发信频率限制和后台的发信趋势。recipient 是邮箱的 SHA-256，不存邮箱原文。保留 35 天。
CREATE TABLE mail_log (
    id         bigserial PRIMARY KEY,
    recipient  text NOT NULL,
    kind       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mail_log_recipient_idx ON mail_log (recipient, created_at DESC);
CREATE INDEX mail_log_created_idx ON mail_log (created_at);

-- 图片的 Telegram 备份：原图地址 → Telegram 的文件编号。图片代理优先从 Telegram 取。
CREATE TABLE image_mirrors (
    url_hash     bytea PRIMARY KEY CHECK (octet_length(url_hash) = 32), -- 原图地址的 SHA-256
    original_url text NOT NULL,
    tg_file_id   text NOT NULL,
    content_type text NOT NULL,
    bytes        integer NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------- 后台任务 ----------
CREATE TABLE jobs (
    id           bigserial PRIMARY KEY,
    kind         text NOT NULL,
    subject      text NOT NULL DEFAULT '',
    payload      jsonb NOT NULL DEFAULT '{}',
    priority     integer NOT NULL DEFAULT 0,
    status       text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    attempts     integer NOT NULL DEFAULT 0,
    run_at       timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    progress     jsonb NOT NULL DEFAULT '{}',
    last_error   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
-- 同一个对象的同一种任务，同时只能有一个在排队或运行
CREATE UNIQUE INDEX jobs_active_key ON jobs (kind, subject) WHERE status IN ('queued', 'running');
CREATE INDEX jobs_due_idx ON jobs (priority DESC, run_at) WHERE status = 'queued';
-- 查"这个对象有没有在排队、在运行、或者还在失败冷却期"
CREATE INDEX jobs_subject_idx ON jobs (kind, subject);
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE status IN ('done', 'failed');
