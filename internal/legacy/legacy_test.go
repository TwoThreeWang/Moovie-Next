package legacy_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"moovie/internal/db"
	"moovie/internal/db/testdb"
	"moovie/internal/legacy"
	"moovie/internal/media"
)

// oldSchema 是旧版数据库里迁移会读到的表和列（只保留用得到的部分）。
const oldSchema = `
CREATE TABLE media (id bigserial PRIMARY KEY, media_type text, title text, original_title text, year text, poster text, backdrops text,
  summary text, genres text, countries text, directors text, actors text, duration text, embedding vector(768), rating_douban double precision,
  rating_tmdb double precision, vote_count_tmdb integer, metadata_status text, last_metadata_sync_at timestamptz,
  created_at timestamptz DEFAULT now(), updated_at timestamptz DEFAULT now(), douban_id text DEFAULT '', series_status text,
  embedding_content text DEFAULT '');
CREATE TABLE media_external_ids (id bigserial PRIMARY KEY, media_id bigint, provider text, external_id text, is_primary boolean DEFAULT true, external_type text);
CREATE TABLE media_aliases (id bigserial PRIMARY KEY, media_id bigint, alias text, normalized_alias text, source text);
CREATE TABLE sites (id bigserial PRIMARY KEY, key text, base_url text, enabled boolean);
CREATE TABLE ad_fingerprints (fingerprint bytea PRIMARY KEY, confirm_count integer, reject_count integer);
CREATE TABLE media_units (id bigserial PRIMARY KEY, media_id bigint, unit_type text, season_number integer, episode_number integer,
  episode_key text, title text, air_date date, runtime_minutes integer);

INSERT INTO media (id, media_type, title, original_title, year, poster, backdrops, summary, genres, countries, directors, actors, duration,
  rating_douban, rating_tmdb, vote_count_tmdb, metadata_status, douban_id, series_status) VALUES
  (10, 'tv', '雾港', 'Fog Harbor', '2024', 'https://img1.doubanio.com/p.jpg',
   'https://image.tmdb.org/t/p/w1280/a.jpg,https://image.tmdb.org/t/p/w1280/b.jpg', '简介', '悬疑,犯罪', '中国大陆',
   '[{"id":"","name":"陈默"}]', '[{"id":"1","name":"周予"},{"id":"2","name":"张望"}]', '45分钟', 8.6, 8.1, 300, 'ready', '30000010', 'Ended'),
  (11, 'tv', '雾港 第二季', '', '2026', '', '', '', '悬疑', '', '', '', '', 8.9, 0, 0, 'partial', '30000011', 'Returning Series'),
  (12, 'movie', '回声', '', '2024年', '', '', '', '剧情/悬疑', '', '', '', '120分钟', 8.7, 0, 0, 'ready', '', ''),
  (13, 'cartoon', '星陨之地', '', '2026', '', '', '', '动画', '', '', '', '', 8.8, 0, 0, 'ready', '', ''),
  (14, 'movie', '资源站来的片子', '', '2020', '', '', '', '', '', '', '', '', 0, 0, 0, 'partial', '', ''),
  (15, 'movie', '禁片', '', '2020', '', '', '', '', '', '', '', '', 0, 0, 0, 'ready', '30000015', '');
UPDATE media SET embedding_content = '《雾港》(Fog Harbor) 港城悬疑，旧案与台风夜。该片适合喜欢罪案剧的观众。', last_metadata_sync_at = now() WHERE id = 10;
INSERT INTO media_external_ids (media_id, provider, external_id, external_type) VALUES
  (10, 'douban', '30000010', 'tv'), (10, 'tmdb', '4455', 'tv_season_1'), (10, 'imdb', 'tt1234567', ''),
  (11, 'tmdb', '4455', 'tv_season_2'), (11, 'imdb', 'tt1234567', ''),
  (12, 'tmdb', '603', 'movie'), (13, 'imdb', 'tt7654321', ''), (14, 'resource', 'lz-99', '');
INSERT INTO media_aliases (media_id, alias, source) VALUES (10, '雾港往事', 'douban'), (14, '资源站别名', 'resource');
INSERT INTO sites (key, base_url, enabled) VALUES
  ('豪华资源', 'https://hhzyapi.com/api.php/provide/vod/', true), ('豪华资源', 'https://hhzyapi.com/api.php/provide/vod', true),
  ('非凡影视', 'http://ffzy5.tv/api.php/provide/vod/', true), ('停用的', 'https://off.example/api.php/provide/vod/', false),
  ('同域名第二个', 'https://hhzyapi.com/at/xml/', true), ('', 'https://noname.example/api.php/provide/vod/', true),
  ('坏地址', 'ftp://bad.example/', true);
INSERT INTO ad_fingerprints VALUES (decode(repeat('ab', 32), 'hex'), 6, 1), ('\x01', 1, 0);
INSERT INTO media_units (id, media_id, unit_type, season_number, episode_number, episode_key, title, air_date) VALUES
  (100, 10, 'episode', 1, 1, 'S01E01', '雾起', '2024-10-01'), (101, 10, 'episode', 1, 2, 'S01E02', '旧码头', NULL),
  (102, 12, 'feature', 0, NULL, 'feature', '正片', NULL), (103, 11, 'episode', 2, 1, 'S02E01', '', NULL), (104, 10, 'trailer', 1, 0, 'x', '预告', NULL);

CREATE TABLE users (id bigserial PRIMARY KEY, email text, username text, password_hash text, role text DEFAULT 'user', douban_user_id text DEFAULT '',
  is_public boolean DEFAULT false, avatar text DEFAULT '🎬', ad_skip_enabled boolean DEFAULT false, created_at timestamptz DEFAULT now());
CREATE TABLE user_follows (follower_id bigint, followee_id bigint, created_at timestamptz DEFAULT now());
CREATE TABLE user_movies (id bigserial PRIMARY KEY, user_id bigint, movie_id text DEFAULT '', media_id bigint, status text, rating integer DEFAULT 0,
  comment text DEFAULT '', created_at timestamptz DEFAULT now(), updated_at timestamptz DEFAULT now());
CREATE TABLE comment_likes (id bigserial PRIMARY KEY, user_movie_id bigint, user_id bigint, created_at timestamptz DEFAULT now());
CREATE TABLE comment_replies (id bigserial PRIMARY KEY, user_movie_id bigint, user_id bigint, content text, created_at timestamptz DEFAULT now());
CREATE TABLE playback_positions (id bigserial PRIMARY KEY, user_id bigint, media_id bigint, media_unit_id bigint, position_seconds double precision,
  duration_seconds double precision, completed boolean DEFAULT false, last_source_key text DEFAULT '', last_vod_id text DEFAULT '',
  season_number integer DEFAULT 1, episode_key text DEFAULT '', activity_at timestamptz DEFAULT now(), deleted_at timestamptz);

INSERT INTO users (id, email, username, password_hash, role, douban_user_id, is_public, avatar, ad_skip_enabled, created_at) VALUES
  (1, 'Evan@Example.com', 'evan', '$2a$10$oldhasholdhasholdhashuOldhasholdhasholdhasholdhashol', 'admin', '12345', true, '🦊', true, '2024-01-02'),
  (2, 'lin@example.com', 'lin', 'hash-lin', 'user', '', false, '不是表情', false, '2024-02-03'),
  (3, 'LIN@example.com', 'lin2', 'hash-lin2', 'user', '', false, '🎬', false, now()),
  (4, 'zoe@example.com', '', 'hash-zoe', 'vip', '', false, '🐼', false, now()),
  (5, 'taken@example.com', 'taken', 'hash-taken', 'user', '', false, '🎬', false, now()),
  (6, 'second@example.com', 'newbie', 'hash-second', 'user', '', false, '🎬', false, now());
INSERT INTO user_follows (follower_id, followee_id) VALUES (1, 2), (2, 1), (1, 3);
INSERT INTO user_movies (id, user_id, media_id, status, rating, comment) VALUES
  (50, 1, 10, 'watched', 5, ' 好看 '), (51, 2, 12, 'watched', 9, repeat('长', 2100)), (52, 1, 14, 'wish', 0, ''), (53, 3, 10, 'wish', 0, ''),
  (54, 1, NULL, 'watched', 3, '');
INSERT INTO comment_likes (user_movie_id, user_id) VALUES (50, 2), (52, 2), (50, 3);
INSERT INTO comment_replies (user_movie_id, user_id, content) VALUES (50, 2, '同意'), (50, 2, '   '), (53, 1, '回复没迁移的短评');
INSERT INTO playback_positions (user_id, media_id, media_unit_id, position_seconds, duration_seconds, completed, last_source_key, last_vod_id,
  season_number, episode_key, activity_at, deleted_at) VALUES
  (1, 10, 101, 600, 2700, false, 'lzzy', '123', 1, 'S01E02', '2026-11-01 20:00+08', NULL),
  (1, 11, NULL, 300, 2700, false, 'lzzy', '124', 2, 'S02E01:上', '2026-11-02 20:00+08', NULL),
  (1, 11, NULL, 900, 2700, false, 'lzzy', '124', 2, 'S02E01:下', '2026-11-03 20:00+08', NULL),
  (1, 12, 102, 7000, 7200, true, 'lzzy', '125', 0, 'feature', '2026-11-03 21:00+08', NULL),
  (1, NULL, NULL, 10, 100, false, 'lzzy', '126', 1, 'S01E01', '2026-11-04 20:00+08', NULL),
  (2, 10, NULL, 10, 100, false, 'lzzy', '127', 1, '20241001', '2026-11-04 20:00+08', NULL),
  (1, 10, 100, 10, 100, false, 'lzzy', '128', 1, 'S01E01', '2026-11-04 20:00+08', '2026-11-05');
`

func setup(t *testing.T) (*db.Pool, *db.Pool) {
	target := testdb.Pool(t)
	ctx := context.Background()
	schema := testdb.Schema() + "_old"
	if _, err := target.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	source, err := db.Open(ctx, testdb.URL(t), db.Options{MaxConns: 2, SearchPath: schema + ",public"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		source.Close()
		target.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	if _, err := source.Exec(ctx, oldSchema); err != nil {
		t.Fatal(err)
	}
	return source, target
}

func count(t *testing.T, pool *db.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestDryRunWritesNothing(t *testing.T) {
	source, target := setup(t)
	report, err := legacy.Run(context.Background(), source, target, legacy.Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Stats("media").Written != 5 {
		t.Errorf("试运行的报告应该和正式运行一样: %+v", report.Stats("media"))
	}
	if n := count(t, target, `SELECT count(*) FROM media`); n != 0 {
		t.Errorf("试运行不应该写入任何数据，实际有 %d 部作品", n)
	}
}

// 写入是成批发送的：豆瓣 ID 冲突要提前跳过，不能让整批失败；超过一批的数据要分几次发完；进度要输出。
func TestMigrateInBatches(t *testing.T) {
	source, target := setup(t)
	ctx := context.Background()
	if _, err := source.Exec(ctx, `
INSERT INTO media (id, media_type, title, douban_id, metadata_status) VALUES (16, 'movie', '同豆瓣 ID 的另一部', '30000010', 'ready');
INSERT INTO media_units (id, media_id, unit_type, season_number, episode_number, title)
  SELECT 1000 + n, 10, 'episode', 1, n, '第' || n || '集' FROM generate_series(3, 2502) AS n;`); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	report, err := legacy.Run(ctx, source, target, legacy.Options{Progress: &progress})
	if err != nil {
		t.Fatalf("豆瓣 ID 冲突应该跳过，不应该让迁移失败: %v", err)
	}
	if n := report.Stats("media").Skipped["豆瓣 ID 和已有作品重复"]; n != 1 {
		t.Errorf("应该跳过 1 部豆瓣 ID 重复的作品，实际 %d", n)
	}
	if n := count(t, target, `SELECT count(*) FROM media`); n != 5 {
		t.Errorf("应该迁移 5 部作品，实际 %d", n)
	}
	if n := count(t, target, `SELECT count(*) FROM media_units`); n != 2503 {
		t.Errorf("分几批写入后应该有 2503 个分集，实际 %d", n)
	}
	if stats := report.Stats("media_units"); stats.Written != 2503 {
		t.Errorf("分集的写入统计不对: %+v", stats)
	}
	for _, table := range []string{"media", "media_external_ids", "media_aliases", "media_units", "ad_fingerprints"} {
		if !strings.Contains(progress.String(), "开始迁移 "+table) {
			t.Errorf("进度里没有 %s:\n%s", table, progress.String())
		}
	}
}

func TestMigrate(t *testing.T) {
	source, target := setup(t)
	ctx := context.Background()
	report, err := legacy.Run(ctx, source, target, legacy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report.Print(&out)
	t.Log("\n" + out.String())

	// 作品：没有任何外部 ID 的（14）不迁移；其余字段转换成新格式。
	if n := count(t, target, `SELECT count(*) FROM media`); n != 5 {
		t.Errorf("应该迁移 5 部作品，实际 %d", n)
	}
	if n := count(t, target, `SELECT count(*) FROM media WHERE id = 14`); n != 0 {
		t.Error("没有外部 ID 的作品不应该迁移")
	}
	var kind, seriesKey, status, metadata string
	var year, season, episodes, duration int
	var genres, directors, actors, backdrops []string
	target.QueryRow(ctx, `SELECT media_type, series_key, series_status, metadata_status, year, season, episode_count, duration_minutes, genres, directors, actors, backdrops
FROM media WHERE id = 10`).Scan(&kind, &seriesKey, &status, &metadata, &year, &season, &episodes, &duration, &genres, &directors, &actors, &backdrops)
	if kind != "tv" || seriesKey != "雾港" || status != "ended" || metadata != "complete" || year != 2024 || season != 1 || episodes != 2 || duration != 45 {
		t.Errorf("作品 10 的字段不对: %s %s %s %s %d %d %d %d", kind, seriesKey, status, metadata, year, season, episodes, duration)
	}
	if strings.Join(genres, ",") != "悬疑,犯罪" || strings.Join(directors, ",") != "陈默" || strings.Join(actors, ",") != "周予,张望" ||
		len(backdrops) != 2 || backdrops[0] != "https://image.tmdb.org/t/p/original/a.jpg" {
		t.Errorf("作品 10 的列表字段不对: %v %v %v %v", genres, directors, actors, backdrops)
	}
	// 老系统 AI 写的语义描述原样带过来；哈希按新系统的算法算，所以新系统认得它，不会重新调用 AI。
	migrated, err := media.NewStore(target).Get(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(migrated.SemanticText, "《雾港》") || migrated.SemanticHash == "" || migrated.SemanticHash != migrated.InputHash() || migrated.EnrichedAt == nil {
		t.Errorf("语义描述没有正确迁移: %q 哈希 %q / %q 补充时间 %v", migrated.SemanticText, migrated.SemanticHash, migrated.InputHash(), migrated.EnrichedAt)
	}
	if want := "标题：雾港\n原名：Fog Harbor\n年份：2024\n地区：中国大陆\n类型：悬疑,犯罪\n导演：陈默\n主演：周予,张望\n剧情：简介"; migrated.SemanticInput() != want {
		t.Errorf("发给 AI 的资料格式要和老系统一致:\n%s", migrated.SemanticInput())
	}
	target.QueryRow(ctx, `SELECT season, series_status FROM media WHERE id = 11`).Scan(&season, &status)
	if season != 2 || status != "ongoing" {
		t.Errorf("第二季的季数和状态不对: %d %s", season, status)
	}
	target.QueryRow(ctx, `SELECT media_type, year, duration_minutes FROM media WHERE id = 12`).Scan(&kind, &year, &duration)
	if kind != "movie" || year != 2024 || duration != 120 {
		t.Errorf("作品 12 不对: %s %d %d", kind, year, duration)
	}
	if kind := ""; target.QueryRow(ctx, `SELECT media_type FROM media WHERE id = 13`).Scan(&kind) != nil || kind != "anime" {
		t.Errorf("旧的 cartoon 应该转成 anime，实际 %q", kind)
	}
	// 外部 ID：同一部剧的两季共用 TMDB 和 IMDb ID；TMDB 加上类型前缀；资源站的 ID 不迁移。
	if n := count(t, target, `SELECT count(*) FROM media WHERE tmdb_id = 'tv/4455'`); n != 2 {
		t.Errorf("两季都应该有 tv/4455，实际 %d", n)
	}
	if n := count(t, target, `SELECT count(*) FROM media WHERE tmdb_id = 'movie/603'`); n != 1 {
		t.Error("电影的 TMDB ID 应该是 movie/603")
	}
	if n := count(t, target, `SELECT count(*) FROM media WHERE tmdb_id LIKE '%lz%' OR imdb_id LIKE '%lz%' OR wikidata_id LIKE '%lz%'`); n != 0 {
		t.Error("资源站的 ID 不能迁移")
	}
	// 分集：只迁移正片之外的普通分集。
	if n := count(t, target, `SELECT count(*) FROM media_units`); n != 3 {
		t.Errorf("应该有 3 个分集，实际 %d", n)
	}

	// 广告指纹：票数原样带过来，长度不对的不要。
	if n := count(t, target, `SELECT count(*) FROM ad_fingerprints WHERE confirm_count = 6 AND reject_count = 1`); n != 1 || report.Stats("ad_fingerprints").Skipped["长度不对"] != 1 {
		t.Errorf("广告指纹没有正确迁移: %d 条，跳过 %v", n, report.Stats("ad_fingerprints").Skipped)
	}

	// 序列：迁移后新建的作品不能和旧 ID 冲突。
	var next int64
	target.QueryRow(ctx, `INSERT INTO media (title) VALUES ('新片') RETURNING id`).Scan(&next)
	if next <= 15 {
		t.Errorf("新作品的 ID 应该排在迁移过来的之后，实际 %d", next)
	}
	target.Exec(ctx, `DELETE FROM media WHERE id = $1`, next)

	// 再跑一次：结果不变，不会重复。
	if _, err := legacy.Run(ctx, source, target, legacy.Options{}); err != nil {
		t.Fatalf("重复运行失败: %v", err)
	}
	if count(t, target, `SELECT count(*) FROM media`) != 5 || count(t, target, `SELECT count(*) FROM media_units`) != 3 ||
		count(t, target, `SELECT count(*) FROM ad_fingerprints`) != 1 ||
		count(t, target, `SELECT count(*) FROM media_aliases WHERE media_id = 10`) < 3 {
		t.Error("重复运行后数量变了")
	}
}

// 临时功能：导出旧版的播放源。同一个地址只留一个，停用的和地址不对的跳过，HTTP 的留着（走本站转发），key 用域名。
func TestExportSources(t *testing.T) {
	source, _ := setup(t)
	subscription, skipped, err := legacy.ExportSources(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, item := range subscription.Sources {
		got = append(got, item.Key+"|"+item.Name)
	}
	want := "hhzyapi.com|豪华资源,ffzy5.tv|非凡影视,hhzyapi.com-2|同域名第二个,noname.example|noname.example"
	if strings.Join(got, ",") != want || subscription.Version != 1 || len(skipped) != 1 || !strings.Contains(skipped[0], "坏地址") {
		t.Errorf("导出的源不对:\n%v\n跳过 %v", got, skipped)
	}
}

// 账号和用户写下的东西：冲突的跳过并写进报告，长度超限的按新版规则截断；作品库外的进度和来源信息不要；
// 再跑一次不会覆盖新站上的改动。
func TestMigrateUsers(t *testing.T) {
	source, target := setup(t)
	ctx := context.Background()
	// 新站上线后已经有人注册：ID 5 被占用，用户名 newbie 被占用。
	if _, err := target.Exec(ctx, `INSERT INTO users (id, email, username, password_hash) VALUES (5, 'new@example.com', 'newbie', 'x')`); err != nil {
		t.Fatal(err)
	}
	report, err := legacy.Run(ctx, source, target, legacy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report.Print(&out)
	t.Log("\n" + out.String())

	// 账号：邮箱转小写，字段原样带过来；同一邮箱只留第一个；被占的 ID 跳过；被占的用户名加 _ID。
	var email, username, hash, role, avatar, doubanID string
	var public, adSkip bool
	target.QueryRow(ctx, `SELECT email, username, password_hash, role, avatar, douban_user_id, is_public, ad_skip_enabled FROM users WHERE id = 1`).
		Scan(&email, &username, &hash, &role, &avatar, &doubanID, &public, &adSkip)
	if email != "evan@example.com" || username != "evan" || !strings.HasPrefix(hash, "$2a$10$") || role != "admin" || avatar != "🦊" ||
		doubanID != "12345" || !public || !adSkip {
		t.Errorf("账号 1 没有原样迁移: %s %s %s %s %s %s %v %v", email, username, hash, role, avatar, doubanID, public, adSkip)
	}
	target.QueryRow(ctx, `SELECT avatar FROM users WHERE id = 2`).Scan(&avatar)
	if avatar != "🎬" {
		t.Errorf("不是 emoji 的头像应该换成默认的，实际 %q", avatar)
	}
	target.QueryRow(ctx, `SELECT username, role FROM users WHERE id = 4`).Scan(&username, &role)
	if username != "zoe" || role != "user" {
		t.Errorf("没有用户名的取邮箱前缀、不认识的角色当普通用户，实际 %q %q", username, role)
	}
	target.QueryRow(ctx, `SELECT username FROM users WHERE id = 6`).Scan(&username)
	if username != "newbie_6" {
		t.Errorf("被占的用户名应该加上 _ID，实际 %q", username)
	}
	if n := count(t, target, `SELECT count(*) FROM users WHERE id = 3 OR email = 'taken@example.com'`); n != 0 {
		t.Error("邮箱重复的、ID 被占用的账号不应该迁移")
	}
	if stats := report.Stats("users"); stats.Written != 4 || stats.Skipped["邮箱已被其他账号使用"] != 1 || stats.Skipped["ID 已被新库里的其他账号占用"] != 1 {
		t.Errorf("账号的统计不对: %+v", stats)
	}
	if n := count(t, target, `SELECT count(*) FROM example_subscription_users`); n != 4 {
		t.Errorf("迁移过来的账号都要记下来，第一次登录时填入示例订阅，实际 %d", n)
	}

	// 关注：两边账号都迁移了的才要。
	if n := count(t, target, `SELECT count(*) FROM user_follows`); n != 2 {
		t.Errorf("应该有 2 条关注，实际 %d", n)
	}

	// 标记：ID 不变；评分截到 0–5，短评截到 2000 字；账号或作品没迁移的不要。
	var rating, length int
	var comment string
	target.QueryRow(ctx, `SELECT rating, comment FROM user_movies WHERE id = 50 AND user_id = 1 AND media_id = 10 AND status = 'watched'`).Scan(&rating, &comment)
	if rating != 5 || comment != "好看" {
		t.Errorf("标记 50 不对: %d %q", rating, comment)
	}
	target.QueryRow(ctx, `SELECT rating, char_length(comment) FROM user_movies WHERE id = 51`).Scan(&rating, &length)
	if rating != 5 || length != 2000 {
		t.Errorf("评分应该截到 5、短评截到 2000 字，实际 %d %d", rating, length)
	}
	if n := count(t, target, `SELECT count(*) FROM user_movies`); n != 2 {
		t.Errorf("应该迁移 2 条标记，实际 %d", n)
	}

	// 点赞和回复：短评和人都迁移了才要；空回复不要。
	if count(t, target, `SELECT count(*) FROM comment_likes`) != 1 || count(t, target, `SELECT count(*) FROM comment_replies WHERE content = '同意'`) != 1 ||
		count(t, target, `SELECT count(*) FROM comment_replies`) != 1 {
		t.Error("点赞或回复迁移得不对")
	}

	// 进度：季取作品在新库里的季数，集号来自分集表或集键；上下集合成一条，留最新的；电影记成 1/1。
	type position struct {
		media, season, episode int
		seconds                float64
		completed              bool
	}
	rows, err := target.Query(ctx, `SELECT media_id, season, episode, position_seconds, completed FROM playback_positions ORDER BY media_id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []position
	for rows.Next() {
		var p position
		rows.Scan(&p.media, &p.season, &p.episode, &p.seconds, &p.completed)
		got = append(got, p)
	}
	rows.Close()
	want := []position{{10, 1, 2, 600, false}, {11, 2, 1, 900, false}, {12, 1, 1, 7000, true}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("进度不对:\n%v\n应该是\n%v", got, want)
	}
	if stats := report.Stats("playback_positions"); stats.Skipped["作品库里没有的片子（新版由浏览器加密保存）"] != 1 ||
		stats.Skipped["认不出是第几集（按日期排的期数等）"] != 1 || stats.Read != 6 {
		t.Errorf("进度的统计不对（已删除的不读）: %+v", stats)
	}
	if n := count(t, target, `SELECT count(*) FROM watch_days WHERE user_id = 1`); n < 2 {
		t.Errorf("进度的日期应该记成看过东西的日子，实际 %d 天", n)
	}

	// 序列：新注册的账号、新标记不能和迁移过来的 ID 冲突。
	var next int64
	target.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('later@example.com', 'later', 'x') RETURNING id`).Scan(&next)
	if next <= 6 {
		t.Errorf("新账号的 ID 应该排在迁移过来的之后，实际 %d", next)
	}

	// 上线后用户在新站上改了评分、看到了更后面；再跑一次不能被旧数据盖掉。
	target.Exec(ctx, `UPDATE user_movies SET rating = 3 WHERE id = 50`)
	target.Exec(ctx, `UPDATE playback_positions SET position_seconds = 1200, activity_at = now() WHERE media_id = 10`)
	if _, err := legacy.Run(ctx, source, target, legacy.Options{}); err != nil {
		t.Fatalf("重复运行失败: %v", err)
	}
	if n := count(t, target, `SELECT count(*) FROM user_movies WHERE id = 50 AND rating = 3`); n != 1 {
		t.Error("重复运行覆盖了新站上改过的评分")
	}
	if n := count(t, target, `SELECT count(*) FROM playback_positions WHERE media_id = 10 AND position_seconds = 1200`); n != 1 {
		t.Error("重复运行用旧进度盖掉了新进度")
	}
	if count(t, target, `SELECT count(*) FROM users`) != 6 || count(t, target, `SELECT count(*) FROM comment_replies`) != 1 ||
		count(t, target, `SELECT count(*) FROM comment_likes`) != 1 || count(t, target, `SELECT count(*) FROM user_follows`) != 2 {
		t.Error("重复运行后数量变了")
	}
}
