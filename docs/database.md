# 数据库

PostgreSQL，需要 `vector` 和 `pg_trgm` 两个扩展。启动时自动执行 `internal/db/migrations/` 里还没执行过的迁移，按文件名顺序：

| 文件 | 内容 |
| --- | --- |
| `0001_init.sql` | 初始的 24 张表 |
| `0002_short_drama.sql` | 短剧的列和索引（见 [短剧](short-drama.md)） |
| `0003_collection_submissions.sql` | 片单的投稿状态、收藏、去重浏览，以及片单通知 |
| `0004_collection_revisions.sql` | 已发布片单的修改稿 |
| `0005_example_subscription_users.sql` | 临时：从旧版迁移过来、第一次登录时要填入示例订阅的账号（见 [从已有数据初始化](bootstrap.md#临时示例订阅)） |

新增表结构时加一个新的迁移文件，不修改已经上线的文件。

## 有哪些表

一共 28 张业务表（另有一张迁移记录表 `schema_migrations`）。留下来的标准是：**别处推不出来、又确实有人读**。能算出来的现算，只写不读的不存。

| 用途 | 表 | 为什么需要 |
| --- | --- | --- |
| 账号 | `users`、`user_follows`、`keyrings`、`example_subscription_users` | 账号本身；关注关系；登录即同步用的密文；临时：哪些老账号第一次登录时填入示例订阅 |
| 作品资料 | `media`、`media_aliases`、`media_units`、`chart_items` | 作品；别名用来匹配资源站片名和搜索；分集用来显示集名和"今日更新"；热门榜单 |
| 标记和社区 | `user_movies`、`comment_likes`、`comment_replies`、`danmakus`、`feedbacks`、`feedback_messages`、`notifications` | 都是用户产生的内容，没法从别处得到 |
| 片单 | `collections`、`collection_items`、`collection_favorites`、`collection_views` | 片单和其中的作品；收藏（取消后保留一行，再收藏时重新计时）；按天去重的浏览，只留最近 7 天，用于热门排序 |
| 播放 | `playback_positions`、`watch_days`、`private_history`、`ad_fingerprints` | 进度；哪些天看过（进度表只留最后一次，算不出连续天数）；加密历史；广告指纹的票数 |
| 运营 | `blocked_keywords`、`search_counts`、`mail_log`、`image_mirrors`、`jobs` | 屏蔽词；每天每个词的搜索次数；发信限流；图片在 Telegram 的文件编号；后台任务队列 |

`media` 上和采集有关的列：`metadata_synced_at`（豆瓣或红果同步时间）、`enriched_at`（Wikidata / TMDB 补充时间）、`semantic_text` 和 `semantic_hash`（AI 写的描述和所用资料的哈希）、`embedding`（向量）。失败次数、冷却期这些不另外存，看 `jobs` 表里的失败记录就够了。

`collections.revision` 是已发布片单待审核的修改稿（jsonb：标题、简介和作品列表）。公开页始终读 `collections` 和 `collection_items` 里的正式内容，审核通过时把修改稿合并进去再清空，不保存两份完整数据（见 [片单](collections.md)）。

反馈是一段对话：`feedbacks` 只记类型、相关页面和主动关闭的时间与结果，内容都在 `feedback_messages` 里。状态不存，按最后一条消息现算：用户发的是"待回复"，管理员发的是"等用户回复"，等用户回复超过 14 天算作关闭。

## schema

正式运行时不设置 `DB_SCHEMA`，表建在 `public` 里。演示数据用 `DB_SCHEMA=mv_demo` 单独放一个 schema，这样 `go run ./cmd/devseed -reset` 可以整个删掉重建而不碰别的数据；测试也是每个包一个临时 schema。`public` 里除了业务表只有 `vector` 和 `pg_trgm` 两个扩展。

`go run ./cmd/dbinfo` 可以查看数据库的大致情况。
